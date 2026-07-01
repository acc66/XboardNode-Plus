#!/bin/bash
#================================================================
#  Multi-Protocol XboardNode-Plus installer (hy8.sh)
#  支持同一台服务器运行1或2个节点实例
#  使用 acc66/XboardNode-Plus (Hysteria2性能优化版)
#  优化内容: Timer Pool / UDP锁修复 / connCount atomic / STW降频
#================================================================
set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

log_info()  { echo -e "${GREEN}[INFO]${NC} $1" >&2; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $1" >&2; }
log_error() { echo -e "${RED}[ERROR]${NC} $1" >&2; }
log_step()  { echo -e "${CYAN}[STEP]${NC} $1" >&2; }

DEFAULT_PANEL="https://api10.seox9.com"
DEFAULT_KEY="xiaomumuge666"
DEFAULT_CFKEY="你的CF_API_Token"
DEFAULT_CFUSER="xiaomumuge123@gmail.com"
DEFAULT_CFGLOBAL="你的CF_Global_API_Key"
INSTALL_DIR="/etc/XboardNode-Plus"
BIN_PATH="/usr/local/bin/xboard-node"

# ==================== 架构检测 ====================

get_arch() {
    case "$(uname -m)" in
        x86_64)  ARCH="amd64" ;;
        aarch64) ARCH="arm64" ;;
        armv7l)  ARCH="armv7" ;;
        *)       log_error "不支持的架构: $(uname -m)"; exit 1 ;;
    esac
}

# ==================== 检查是否已安装 ====================

check_installed() {
    if ! systemctl is-active xboard-node &>/dev/null; then
        return
    fi
    echo ""
    log_warn "xboard-node 已在运行"
    systemctl status xboard-node --no-pager -l 2>/dev/null | head -5
    echo ""
    echo "  1) 重新安装 (覆盖配置)"
    echo "  2) 追加节点"
    echo "  3) 取消"
    read -rp "请选择 [1/2/3]: " choice
    case "${choice:-1}" in
        2)
            append_node
            exit 0
            ;;
        3)
            log_info "已取消"; exit 0 ;;
    esac
}

append_node() {
    read -rp "追加节点 ID: " ADD_NODE_ID
    [ -z "$ADD_NODE_ID" ] && { log_error "节点ID不能为空"; exit 1; }

    read -rp "面板地址 [${DEFAULT_PANEL}]: " PANEL_URL
    PANEL_URL="${PANEL_URL:-$DEFAULT_PANEL}"
    PANEL_URL="${PANEL_URL%/}"

    read -rp "API Key [${DEFAULT_KEY}]: " API_KEY
    API_KEY="${API_KEY:-$DEFAULT_KEY}"

    read -rp "Cloudflare API Token [${DEFAULT_CFKEY}]: " CFKEY
    CFKEY="${CFKEY:-$DEFAULT_CFKEY}"

    read -rp "Cloudflare 账户邮箱 [${DEFAULT_CFUSER}]: " CFUSER
    CFUSER="${CFUSER:-$DEFAULT_CFUSER}"

    read -rp "Cloudflare Global API Key [${DEFAULT_CFGLOBAL}]: " CFGLOBAL
    CFGLOBAL="${CFGLOBAL:-$DEFAULT_CFGLOBAL}"

    local info domain node_type
    info=$(fetch_one_node "$ADD_NODE_ID")
    domain=$(echo "$info"    | cut -d'|' -f1)
    local port
    port=$(echo "$info"      | cut -d'|' -f2)
    node_type=$(echo "$info" | cut -d'|' -f3)
    local node_ports
    node_ports=$(echo "$info" | cut -d'|' -f4)
    local node_reality
    node_reality=$(echo "$info" | cut -d'|' -f5)
    local node_tls
    node_tls=$(echo "$info" | cut -d'|' -f6)

    setup_cf_dns "$domain" "$node_type"
    if [ "$node_type" = "vless" ] && [ "${node_reality:-0}" = "1" ]; then
        generate_reality_keypair "$domain"
    fi
    request_cert "$domain" "$node_type" "${node_reality:-0}" "${node_tls:-0}"

    {
        echo "    - panel:"
        echo "        url: \"${PANEL_URL}\""
        echo "        token: \"${API_KEY}\""
        echo "        node_id: ${ADD_NODE_ID}"
        echo "      kernel:"
        echo "        type: singbox"
        echo "        log_level: info"
        # 判断是否需要证书块: reality 或 无TLS 的节点都不需要
        add_need_cert=true
        if [ "$node_type" = "vless" ] && [ "${node_reality:-0}" = "1" ]; then
            add_need_cert=false
        fi
        if [[ "$node_type" =~ ^(v2ray|vmess|trojan|vless)$ ]]; then
            if [ "${node_tls:-1}" = "0" ] || [ "${node_tls:-1}" = "none" ] || [ "${node_tls:-1}" = "null" ] || [ "${node_tls:-1}" = "" ]; then
                add_need_cert=false
            fi
        fi
        if [ "$add_need_cert" = true ]; then
            make_cert_block "$domain"
        fi
        echo "      log:"
        echo "        level: info"
        echo "        output: stdout"
    } >> "${INSTALL_DIR}/config.yml"

    iptables -C INPUT -p tcp --dport "$port" -j ACCEPT 2>/dev/null || \
        iptables -A INPUT -p tcp --dport "$port" -j ACCEPT
    iptables -C INPUT -p udp --dport "$port" -j ACCEPT 2>/dev/null || \
        iptables -A INPUT -p udp --dport "$port" -j ACCEPT

    if [ -n "$node_ports" ]; then
        IFS=',' read -ra ranges <<< "$node_ports"
        for range in "${ranges[@]}"; do
            local ipt_range="${range/-/:}"
            iptables -C INPUT -p udp --dport "$ipt_range" -j ACCEPT 2>/dev/null || \
                iptables -A INPUT -p udp --dport "$ipt_range" -j ACCEPT
            log_info "已开放跳跃端口: ${range} (UDP)"
        done
    fi

    systemctl restart xboard-node
    sleep 3
    if systemctl is-active xboard-node &>/dev/null; then
        log_info "节点 ${ADD_NODE_ID}(${domain}) 追加成功"
    else
        log_error "服务重启失败"
        journalctl -u xboard-node --no-pager -n 20
        exit 1
    fi
}

# ==================== 初始化环境 ====================

init_environment() {
    log_step "初始化环境..."
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq -y 2>/dev/null || true

    local pkgs="wget curl screen git iperf3 python3 sudo nload tzdata jq openssl socat iptables conntrack"
    local need_install=""
    for pkg in $pkgs; do
        dpkg -s "$pkg" &>/dev/null || need_install="$need_install $pkg"
    done
    [ -n "$need_install" ] && apt-get install -y -qq $need_install 2>/dev/null

    timedatectl set-timezone Asia/Shanghai

    if ! command -v docker >/dev/null 2>&1; then
        log_info "安装 Docker..."
        curl -fsSL https://get.docker.com | bash
    fi
    systemctl enable --now docker

    systemctl restart cron 2>/dev/null || systemctl restart crond 2>/dev/null || true

    apt-get purge netfilter-persistent -y 2>/dev/null || true
    iptables -P INPUT ACCEPT
    iptables -P FORWARD ACCEPT
    iptables -P OUTPUT ACCEPT
    iptables -F
    log_info "环境初始化完成"
}

# ==================== 阿里云 DDNS ====================

setup_aliyun_ddns() {
    [ -z "${DDNS_DOMAIN:-}" ] && return
    log_step "设置 DDNS ($DDNS_DOMAIN)..."
    docker rm -f ms9 2>/dev/null || true
    docker run --restart=on-failure --name ms9 -d \
        -e "AKID=你的阿里云AKID" \
        -e "AKSCT=你的阿里云AKSCT" \
        -e "DOMAIN=${DDNS_DOMAIN}" \
        -e "REDO=30" -e "TTL=60" -e "TIMEZONE=8.0" -e "TYPE=A,AAAA" \
        sanjusss/aliyun-ddns
    log_info "DDNS 已启动: $DDNS_DOMAIN"
}

setup_aliyun_ddns_domain() {
    local domain="$1"
    log_step "设置阿里云 DDNS ($domain)..."

    log_info "使用 Docker 容器自动更新域名解析"
    docker rm -f aliyun-ddns-node 2>/dev/null || true
    docker run --restart=always --name aliyun-ddns-node -d \
        -e "AKID=你的阿里云AKID" \
        -e "AKSCT=你的阿里云AKSCT" \
        -e "DOMAIN=${domain}" \
        -e "REDO=30" -e "TTL=60" -e "TIMEZONE=8.0" -e "TYPE=A,AAAA" \
        sanjusss/aliyun-ddns

    log_info "阿里云 DDNS 已启动: $domain"
    log_info "容器名: aliyun-ddns-node (每30秒自动检测IP变化)"
    log_info "查看日志: docker logs -f aliyun-ddns-node"
}

# ==================== DDNS 自动更新 ====================

setup_ddns() {
    [ -z "${NODE_DOMAIN:-}" ] && return

    echo ""
    echo "  DDNS 自动更新服务:"
    echo "  1) Cloudflare (域名托管在Cloudflare)"
    echo "  2) 阿里云 (域名托管在阿里云)"
    echo "  3) 跳过 (不配置DDNS)"
    read -rp "请选择 [1/2/3, 默认3]: " ddns_choice

    case "${ddns_choice:-3}" in
        1) setup_cloudflare_ddns "$NODE_DOMAIN" ;;
        2) setup_aliyun_ddns_domain "$NODE_DOMAIN" ;;
        *) log_info "跳过 DDNS 配置" ;;
    esac
}

setup_cloudflare_ddns() {
    local domain="$1"
    log_step "设置 Cloudflare DDNS ($domain)..."

    # 从完整域名提取记录名和区域名
    CFRECORD_NAME=$(echo "$domain" | cut -d'.' -f1)
    CFZONE_NAME=$(echo "$domain" | cut -d'.' -f2-)

    log_info "DDNS 配置: 记录=${CFRECORD_NAME}, 区域=${CFZONE_NAME}"

    # 获取当前公网IP（增加重试和验证）
    log_info "正在获取公网IP..."
    WAN_IP=$(curl -s --connect-timeout 5 --max-time 10 http://ipv4.icanhazip.com)
    if [ -z "$WAN_IP" ] || ! echo "$WAN_IP" | grep -qE '^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$'; then
        log_warn "获取公网IP失败，DDNS 将通过 cron 继续尝试"
        WAN_IP=""
    else
        log_info "当前公网IP: $WAN_IP"
    fi

    WAN_IP_FILE=$HOME/.cf-wan_ip_${CFRECORD_NAME}.txt

    if [ -f "$WAN_IP_FILE" ]; then
        OLD_WAN_IP=$(cat "$WAN_IP_FILE")
    else
        OLD_WAN_IP=""
    fi

    # 获取 Zone ID 和 Record ID
    log_info "正在获取 Cloudflare Zone ID..."
    ID_FILE=$HOME/.cf-id_${CFRECORD_NAME}.txt
    if [ -f "$ID_FILE" ] && [ $(wc -l < "$ID_FILE") == 4 ] \
      && [ "$(sed -n '3p' "$ID_FILE")" == "$CFZONE_NAME" ] \
      && [ "$(sed -n '4p' "$ID_FILE")" == "${CFRECORD_NAME}.${CFZONE_NAME}" ]; then
        CFZONE_ID=$(sed -n '1p' "$ID_FILE")
        CFRECORD_ID=$(sed -n '2p' "$ID_FILE")
        log_info "使用缓存的 Zone ID: $CFZONE_ID"
    else
        log_info "查询 Cloudflare API..."
        CFZONE_ID=$(curl -s --connect-timeout 10 --max-time 15 -X GET "https://api.cloudflare.com/client/v4/zones?name=$CFZONE_NAME" \
          -H "X-Auth-Email: ${CFUSER}" -H "X-Auth-Key: ${CFGLOBAL}" -H "Content-Type: application/json" \
          | grep -Po '(?<="id":")[^"]*' | head -1)

        if [ -z "$CFZONE_ID" ]; then
            log_warn "========================================"
            log_warn "Cloudflare DDNS 配置跳过"
            log_warn "========================================"
            log_warn "原因: 域名 ${CFZONE_NAME} 未在 Cloudflare 托管"
            log_warn "或 Cloudflare API 配置错误"
            log_warn ""
            log_warn "如需使用 Cloudflare DDNS："
            log_warn "1. 将域名 NS 记录指向 Cloudflare"
            log_warn "2. 检查 Cloudflare API 邮箱和 Global Key"
            log_warn ""
            log_info "继续部署节点..."
            return
        fi
        log_info "Zone ID: $CFZONE_ID"

        log_info "正在获取 DNS Record ID..."
        CFRECORD_ID=$(curl -s --connect-timeout 10 --max-time 15 -X GET "https://api.cloudflare.com/client/v4/zones/$CFZONE_ID/dns_records?name=${CFRECORD_NAME}.${CFZONE_NAME}" \
          -H "X-Auth-Email: ${CFUSER}" -H "X-Auth-Key: ${CFGLOBAL}" -H "Content-Type: application/json" \
          | grep -Po '(?<="id":")[^"]*' | head -1)

        if [ -z "$CFRECORD_ID" ]; then
            log_warn "========================================"
            log_warn "Cloudflare DDNS 配置跳过"
            log_warn "========================================"
            log_warn "原因: DNS 记录 ${CFRECORD_NAME}.${CFZONE_NAME} 不存在"
            log_warn "请先在 Cloudflare 面板创建 A 记录"
            log_warn ""
            log_info "继续部署节点..."
            return
        fi
        log_info "Record ID: $CFRECORD_ID"

        echo "$CFZONE_ID" > "$ID_FILE"
        echo "$CFRECORD_ID" >> "$ID_FILE"
        echo "$CFZONE_NAME" >> "$ID_FILE"
        echo "${CFRECORD_NAME}.${CFZONE_NAME}" >> "$ID_FILE"
    fi

    # 如果获取IP失败，跳过本次更新但仍创建cron脚本
    if [ -z "$WAN_IP" ]; then
        log_info "跳过首次DNS更新，等待 cron 自动更新"
    else
        # 更新 DNS 记录
        log_info "正在更新 Cloudflare DNS 记录..."
        RESPONSE=$(curl -s --connect-timeout 10 --max-time 15 -X PUT "https://api.cloudflare.com/client/v4/zones/$CFZONE_ID/dns_records/$CFRECORD_ID" \
          -H "X-Auth-Email: ${CFUSER}" -H "X-Auth-Key: ${CFGLOBAL}" -H "Content-Type: application/json" \
          --data "{\"id\":\"$CFZONE_ID\",\"type\":\"A\",\"name\":\"${CFRECORD_NAME}.${CFZONE_NAME}\",\"content\":\"$WAN_IP\",\"ttl\":60}")

        if echo "$RESPONSE" | grep -q '"success":true'; then
            echo "$WAN_IP" > "$WAN_IP_FILE"
            log_info "Cloudflare DNS 更新成功: ${CFRECORD_NAME}.${CFZONE_NAME} -> $WAN_IP"
        else
            log_warn "Cloudflare DNS 更新失败，将在下次 cron 重试"
            log_warn "响应: $(echo "$RESPONSE" | grep -o '"message":"[^"]*"' | head -1)"
        fi
    fi

    # 创建轻量级 DDNS 更新脚本
    cat > /root/update-cf-ddns.sh << 'DDNSEOF'
#!/bin/bash
WAN_IP_FILE="__WAN_IP_FILE__"
CFZONE_ID="__CFZONE_ID__"
CFRECORD_ID="__CFRECORD_ID__"
CFUSER="__CFUSER__"
CFGLOBAL="__CFGLOBAL__"
CFRECORD_NAME="__CFRECORD_NAME__"
CFZONE_NAME="__CFZONE_NAME__"

# 获取新IP并验证格式
NEW_IP=$(curl -s --connect-timeout 5 --max-time 10 http://ipv4.icanhazip.com)
if [ -z "$NEW_IP" ] || ! echo "$NEW_IP" | grep -qE '^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$'; then
    exit 0
fi

# 对比旧IP
if [ -f "$WAN_IP_FILE" ]; then
    OLD_IP=$(cat "$WAN_IP_FILE")
    [ "$NEW_IP" = "$OLD_IP" ] && exit 0
fi

# 更新DNS
curl -s -X PUT "https://api.cloudflare.com/client/v4/zones/$CFZONE_ID/dns_records/$CFRECORD_ID" \
  -H "X-Auth-Email: $CFUSER" \
  -H "X-Auth-Key: $CFGLOBAL" \
  -H "Content-Type: application/json" \
  --data "{\"type\":\"A\",\"name\":\"${CFRECORD_NAME}.${CFZONE_NAME}\",\"content\":\"$NEW_IP\",\"ttl\":60}" \
  | grep -q '"success":true' && echo "$NEW_IP" > "$WAN_IP_FILE"
DDNSEOF

    # 替换占位符
    sed -i "s|__WAN_IP_FILE__|$WAN_IP_FILE|g" /root/update-cf-ddns.sh
    sed -i "s|__CFZONE_ID__|$CFZONE_ID|g" /root/update-cf-ddns.sh
    sed -i "s|__CFRECORD_ID__|$CFRECORD_ID|g" /root/update-cf-ddns.sh
    sed -i "s|__CFUSER__|$CFUSER|g" /root/update-cf-ddns.sh
    sed -i "s|__CFGLOBAL__|$CFGLOBAL|g" /root/update-cf-ddns.sh
    sed -i "s|__CFRECORD_NAME__|$CFRECORD_NAME|g" /root/update-cf-ddns.sh
    sed -i "s|__CFZONE_NAME__|$CFZONE_NAME|g" /root/update-cf-ddns.sh
    chmod +x /root/update-cf-ddns.sh

    # 添加到 crontab（每2分钟）
    (crontab -l 2>/dev/null | grep -v "update-cf-ddns.sh"; echo "*/2 * * * * /bin/bash /root/update-cf-ddns.sh >/dev/null 2>&1") | crontab - 2>/dev/null || true

    log_info "Cloudflare DDNS 配置完成，每2分钟自动检测IP变化"
    log_info "DDNS 脚本: /root/update-cf-ddns.sh"
}

# ==================== 系统优化 ====================

system_optimize() {
    log_step "系统优化 (BBR)..."
    echo "1000000" > /proc/sys/fs/file-max
    touch /etc/sysctl.conf
    sed -i '/fs.file-max/d' /etc/sysctl.conf
    echo "fs.file-max=1000000" >> /etc/sysctl.conf
    ulimit -SHn 1000000 && ulimit -c unlimited

    cat > /etc/security/limits.conf <<EOF
* soft nofile 1000000
* hard nofile 1000000
* hard memlock unlimited
* soft memlock unlimited
EOF

    sed -i '/DefaultLimitNOFILE/d' /etc/systemd/system.conf
    cat >> /etc/systemd/system.conf <<EOF
[Manager]
DefaultTimeoutStopSec=30s
DefaultLimitCORE=infinity
DefaultLimitNOFILE=65535
DefaultLimitNPROC=65535
EOF
    systemctl daemon-reload

    cat > /etc/sysctl.d/98-optimize.conf <<EOF
net.core.default_qdisc = fq
net.ipv4.tcp_congestion_control = bbr
net.ipv4.ip_forward = 1
net.core.somaxconn = 3276800
net.ipv4.tcp_fastopen = 3
EOF
    sysctl --system >/dev/null 2>&1
    log_info "系统优化完成"
}

# ==================== 获取节点配置 ====================

fetch_one_node() {
    local node_id="$1"
    local resp
    resp=$(curl -s --connect-timeout 10 \
        "${PANEL_URL}/api/v1/server/UniProxy/config?token=${API_KEY}&node_id=${node_id}")

    local domain port type ports reality tls
    domain=$(echo "$resp"   | jq -r '.server_addr // .server_name // empty')
    port=$(echo "$resp"     | jq -r '.server_port // 443')
    type=$(echo "$resp"     | jq -r '.server_type // .protocol // empty')
    ports=$(echo "$resp"    | jq -r '.ports // empty')
    reality=$(echo "$resp"  | jq -r '.reality // 0')
    tls=$(echo "$resp"      | jq -r '.tls // .network_settings.security // 0')

    if [ -z "$domain" ] || [ -z "$type" ]; then
        log_error "节点 ${node_id} 获取配置失败: $resp"
        exit 1
    fi
    log_info "节点 ${node_id}: 域名=${domain}  端口=${port}  协议=${type}${ports:+  跳跃端口=${ports}}"
    printf '%s|%s|%s|%s|%s|%s' "$domain" "$port" "$type" "$ports" "$reality" "$tls"
}

fetch_config() {
    log_step "获取节点配置..."
    local info1
    info1=$(fetch_one_node "$NODE_ID_1")
    NODE_DOMAIN=$(echo "$info1"   | cut -d'|' -f1)
    NODE_PORT=$(echo "$info1"     | cut -d'|' -f2)
    SERVER_TYPE=$(echo "$info1"   | cut -d'|' -f3)
    NODE_PORTS=$(echo "$info1"    | cut -d'|' -f4)
    NODE_REALITY=$(echo "$info1"  | cut -d'|' -f5)
    NODE_TLS=$(echo "$info1"      | cut -d'|' -f6)

    NODE_DOMAIN_2="" NODE_PORT_2="" SERVER_TYPE_2="" NODE_PORTS_2="" NODE_REALITY_2="" NODE_TLS_2=""
    if [ -n "${NODE_ID_2:-}" ]; then
        local info2
        info2=$(fetch_one_node "$NODE_ID_2")
        NODE_DOMAIN_2=$(echo "$info2"   | cut -d'|' -f1)
        NODE_PORT_2=$(echo "$info2"     | cut -d'|' -f2)
        SERVER_TYPE_2=$(echo "$info2"   | cut -d'|' -f3)
        NODE_PORTS_2=$(echo "$info2"    | cut -d'|' -f4)
        NODE_REALITY_2=$(echo "$info2"  | cut -d'|' -f5)
        NODE_TLS_2=$(echo "$info2"      | cut -d'|' -f6)
    fi
}

# ==================== 安装二进制 ====================

install_binary() {
    log_step "安装 XboardNode-Plus..."
    if [ ! -f "$BIN_PATH" ]; then
        local url="https://github.com/acc66/XboardNode-Plus/releases/latest/download/xboard-node-linux-${ARCH}"
        log_info "下载: $url"
        curl -fSL -o "$BIN_PATH" "$url"
        chmod +x "$BIN_PATH"
    else
        log_info "XboardNode-Plus 已存在，跳过下载"
    fi

    if [ ! -f /usr/local/bin/xbctl ]; then
        curl -fSL -o /usr/local/bin/xbctl \
            "https://github.com/acc66/XboardNode-Plus/releases/latest/download/xbctl-linux-${ARCH}" 2>/dev/null || true
        chmod +x /usr/local/bin/xbctl 2>/dev/null || true
    fi
}

# ==================== CF Origin CA 证书 / Let's Encrypt 证书 / ZeroSSL 证书 / 写配置文件 ====================

request_letsencrypt_cert() {
    local domain="$1"
    local cert_dir="${INSTALL_DIR}/certs/${domain}"
    mkdir -p "$cert_dir"

    log_step "申请 Let's Encrypt 证书: ${domain}..."

    # 安装 acme.sh
    if [ ! -f /root/.acme.sh/acme.sh ]; then
        curl -fsSL https://get.acme.sh | bash >/dev/null 2>&1
    fi

    # 注册 Let's Encrypt 账户并设为默认 CA（避免使用需要邮箱的 ZeroSSL）
    ~/.acme.sh/acme.sh --register-account -m "${CFUSER:-admin@${domain}}" --server letsencrypt >/dev/null 2>&1 || true
    ~/.acme.sh/acme.sh --set-default-ca --server letsencrypt >/dev/null 2>&1 || true

    # 选择验证方式
    echo ""
    echo "  DNS验证方式:"
    echo "  1) Cloudflare DNS (域名托管在Cloudflare)"
    echo "  2) Standalone HTTP-01 (需要80端口)"
    read -rp "请选择验证方式 [1/2, 默认2]: " dns_choice

    case "${dns_choice:-2}" in
        1)
            log_info "使用 Cloudflare DNS-01 验证"
            export CF_Token="${CFKEY}"
            export CF_Account_ID=""
            export CF_Zone_ID=""

            ~/.acme.sh/acme.sh --issue \
                --dns dns_cf \
                -d "${domain}" \
                --server letsencrypt \
                --cert-file "${cert_dir}/cert.pem" \
                --key-file "${cert_dir}/key.pem" \
                --fullchain-file "${cert_dir}/fullchain.pem" \
                --force
            ;;
        *)
            log_info "使用 Standalone HTTP-01 验证"
            log_warn "请确保："
            log_warn "1. 域名 ${domain} 已解析到本服务器IP"
            log_warn "2. 80端口对外开放"
            echo ""
            read -rp "确认继续? [y/N]: " confirm
            [[ ! "$confirm" =~ ^[Yy]$ ]] && { log_error "用户取消"; exit 1; }

            ~/.acme.sh/acme.sh --issue \
                --standalone \
                -d "${domain}" \
                --server letsencrypt \
                --cert-file "${cert_dir}/cert.pem" \
                --key-file "${cert_dir}/key.pem" \
                --fullchain-file "${cert_dir}/fullchain.pem" \
                --httpport 80 \
                --force
            ;;
    esac

    if [ ! -f "${cert_dir}/cert.pem" ]; then
        log_error "Let's Encrypt 证书申请失败: ${domain}"
        exit 1
    fi

    log_info "Let's Encrypt 证书申请成功: ${domain}"
}

request_cf_origin_cert() {
    local domain="$1"
    local cert_dir="${INSTALL_DIR}/certs/${domain}"
    mkdir -p "$cert_dir"

    if [ -f "${cert_dir}/cert.pem" ] && [ -f "${cert_dir}/key.pem" ]; then
        log_info "证书已存在，跳过申请: ${domain}"
        return
    fi

    log_step "申请 CF Origin CA 证书: ${domain}..."

    openssl genrsa -out "${cert_dir}/key.pem" 2048 2>/dev/null
    openssl req -new -key "${cert_dir}/key.pem" \
        -subj "/CN=${domain}" \
        -out "${cert_dir}/csr.pem" 2>/dev/null

    local csr
    csr=$(cat "${cert_dir}/csr.pem")

    local resp
    resp=$(curl -s -X POST "https://api.cloudflare.com/client/v4/certificates" \
        -H "X-Auth-Email: ${CFUSER}" \
        -H "X-Auth-Key: ${CFGLOBAL}" \
        -H "Content-Type: application/json" \
        --data "{
            \"hostnames\": [\"${domain}\"],
            \"requested_validity\": 5475,
            \"request_type\": \"origin-rsa\",
            \"csr\": $(echo "$csr" | jq -Rs .)
        }")

    local success
    success=$(echo "$resp" | jq -r '.success')
    if [ "$success" != "true" ]; then
        log_error "CF Origin CA 申请失败: $(echo "$resp" | jq -r '.errors[0].message // .errors')"
        exit 1
    fi

    echo "$resp" | jq -r '.result.certificate' > "${cert_dir}/cert.pem"
    rm -f "${cert_dir}/csr.pem"
    log_info "CF Origin CA 证书申请成功: ${domain}"
}

request_zerossl_cert() {
    local domain="$1"
    local cert_dir="${INSTALL_DIR}/certs/${domain}"
    mkdir -p "$cert_dir"

    log_step "申请 ZeroSSL 证书: ${domain}..."

    # 安装 acme.sh
    if [ ! -f /root/.acme.sh/acme.sh ]; then
        curl -fsSL https://get.acme.sh | bash -s email="${CFUSER}" >/dev/null 2>&1
    fi

    # 让用户选择验证方式
    echo ""
    echo "  DNS验证方式:"
    echo "  1) Cloudflare DNS (域名托管在Cloudflare)"
    echo "  2) Standalone HTTP-01 (阿里云/腾讯云/其他DNS，需要80端口)"
    read -rp "请选择验证方式 [1/2, 默认2]: " dns_choice

    case "${dns_choice:-2}" in
        1)
            log_info "使用 Cloudflare DNS-01 验证"

            # 清理旧变量
            unset CF_Key
            unset CF_Email
            if [ -f /root/.acme.sh/account.conf ]; then
                sed -i '/SAVED_CF_/d' /root/.acme.sh/account.conf
            fi
            export CF_Token="${CFKEY}"

            /root/.acme.sh/acme.sh --issue \
                --dns dns_cf \
                -d "$domain" \
                --server zerossl \
                --keylength 2048 \
                --force
            ;;
        *)
            log_info "使用 Standalone HTTP-01 验证"
            log_warn "请确保："
            log_warn "1. 域名 $domain 已解析到本服务器IP"
            log_warn "2. 80端口对外开放"
            echo ""
            read -rp "确认继续? [y/N]: " confirm
            [[ ! "$confirm" =~ ^[Yy]$ ]] && { log_error "已取消证书申请"; exit 1; }

            # 临时停止占用 80 端口的服务
            systemctl stop xboard-node 2>/dev/null || true

            /root/.acme.sh/acme.sh --issue \
                -d "$domain" \
                --standalone \
                --server zerossl \
                --keylength 2048 \
                --httpport 80 \
                --force

            # 恢复服务
            systemctl start xboard-node 2>/dev/null || true
            ;;
    esac

    # 安装证书
    /root/.acme.sh/acme.sh --install-cert -d "$domain" \
        --cert-file  "${cert_dir}/cert.pem" \
        --key-file   "${cert_dir}/key.pem" \
        --fullchain-file "${cert_dir}/cert.pem" \
        --reloadcmd "systemctl restart xboard-node" 2>/dev/null || true

    if [ ! -f "${cert_dir}/cert.pem" ]; then
        log_error "ZeroSSL 证书申请失败: ${domain}"
        echo ""
        log_warn "常见问题排查："
        log_warn "1. Cloudflare DNS: 检查 API Token 是否正确"
        log_warn "2. HTTP-01: 检查域名是否解析到本服务器"
        log_warn "3. HTTP-01: 检查80端口是否开放 (iptables -L INPUT -n | grep 80)"
        log_warn "4. 查看详细日志: cat /root/.acme.sh/acme.sh.log"
        exit 1
    fi
    log_info "ZeroSSL 证书申请成功: ${domain}"
}

generate_reality_keypair() {
    local domain="$1"
    local cert_dir="${INSTALL_DIR}/certs/${domain}"
    mkdir -p "$cert_dir"

    local keypair_file="${cert_dir}/reality_keypair.txt"
    if [ -f "$keypair_file" ]; then
        log_info "REALITY 密钥已存在: ${keypair_file}"
        cat "$keypair_file"
        return
    fi

    log_step "生成 VLESS REALITY 密钥对..."
    openssl genpkey -algorithm x25519 -out "${cert_dir}/reality_private.pem" 2>/dev/null
    local pub priv short_id
    pub=$(openssl pkey -in "${cert_dir}/reality_private.pem" -pubout -outform DER 2>/dev/null | tail -c 32 | base64 | tr '+/' '-_' | tr -d '=')
    priv=$(openssl pkey -in "${cert_dir}/reality_private.pem" -outform DER 2>/dev/null | tail -c 32 | base64 | tr '+/' '-_' | tr -d '=')
    short_id=$(openssl rand -hex 4)

    cat > "$keypair_file" <<EOF
publicKey=${pub}
privateKey=${priv}
shortId=${short_id}
EOF

    echo ""
    echo "=========================================="
    echo "  VLESS REALITY 密钥对已生成"
    echo "  请将以下内容填入面板节点 server 字段:"
    echo ""
    echo "  ${domain};443;0;tcp;tls;publicKey=${pub}|privateKey=${priv}|shortId=${short_id}|flow=xtls-rprx-vision|host=www.microsoft.com|fingerprint=chrome"
    echo ""
    echo "  密钥已保存至: ${keypair_file}"
    echo "=========================================="
    echo ""
    read -rp "已填入面板并保存节点? 按 Enter 继续..." _
}

request_cert() {
    local domain="$1"
    local type="$2"
    local reality="${3:-0}"
    local tls="${4:-1}"

    if [ "$type" = "vless" ] && [ "$reality" = "1" ]; then
        log_info "VLESS REALITY 无需外部证书，跳过"
        return
    fi

    # 检查 TLS 状态（V2Ray/VMess/Trojan/VLESS）
    if [[ "$type" =~ ^(v2ray|vmess|trojan|vless)$ ]]; then
        if [ "$tls" = "0" ] || [ "$tls" = "none" ] || [ "$tls" = "null" ] || [ "$tls" = "" ]; then
            log_info "节点未启用 TLS (TLS=${tls})，跳过证书申请"
            return
        fi
    fi

    if [ "$type" = "hysteria2" ] || [ "$type" = "hysteria" ]; then
        echo ""
        echo "  证书类型:"
        echo "  1) Let's Encrypt (推荐, 公网可信, 无需注册)"
        echo "  2) CF Origin CA (15年有效期, 需CF代理)"
        echo "  3) ZeroSSL (需要邮箱注册)"
        read -rp "请选择 [1/2/3, 默认1]: " cert_choice
        case "${cert_choice:-1}" in
            2) request_cf_origin_cert "$domain" ;;
            3) request_zerossl_cert "$domain" ;;
            *) request_letsencrypt_cert "$domain" ;;
        esac
    else
        echo ""
        echo "  证书类型:"
        echo "  1) Let's Encrypt (推荐, 无需注册)"
        echo "  2) CF Origin CA (推荐, 配合CF代理)"
        echo "  3) ZeroSSL (需要邮箱注册)"
        read -rp "请选择 [1/2/3, 默认2]: " cert_choice
        case "${cert_choice:-2}" in
            1) request_letsencrypt_cert "$domain" ;;
            3) request_zerossl_cert "$domain" ;;
            *) request_cf_origin_cert "$domain" ;;
        esac
    fi
}

setup_cert_renew_cron() {
    # acme.sh 自带 cron，这里额外加一个检测脚本用于 CF Origin CA 证书到期提醒
    local check_script="${INSTALL_DIR}/check_cert.sh"
    cat > "$check_script" <<'SCRIPT'
#!/bin/bash
INSTALL_DIR="/etc/XboardNode-Plus"
for cert in "$INSTALL_DIR"/certs/*/cert.pem; do
    [ -f "$cert" ] || continue
    domain=$(basename "$(dirname "$cert")")
    expire=$(openssl x509 -enddate -noout -in "$cert" 2>/dev/null | cut -d= -f2)
    expire_ts=$(date -d "$expire" +%s 2>/dev/null)
    now_ts=$(date +%s)
    days=$(( (expire_ts - now_ts) / 86400 ))
    if [ "$days" -lt 30 ]; then
        echo "[WARN] 证书即将到期: ${domain} 剩余 ${days} 天"
    fi
done
SCRIPT
    chmod +x "$check_script"
    # 每天 3 点检测证书
    (crontab -l 2>/dev/null | grep -v check_cert; echo "0 3 * * * $check_script >> /var/log/cert_check.log 2>&1") | crontab -
    log_info "证书到期检测 cron 已设置 (每天 03:00)"
}

make_cert_block() {
    local domain="$1"
    cat <<EOF
      cert:
        cert_mode: file
        cert_file: ${INSTALL_DIR}/certs/${domain}/cert.pem
        key_file: ${INSTALL_DIR}/certs/${domain}/key.pem
EOF
}

write_config() {
    log_step "写配置文件..."
    mkdir -p "${INSTALL_DIR}"

    {
        echo "instances:"
        echo "    - panel:"
        echo "        url: \"${PANEL_URL}\""
        echo "        token: \"${API_KEY}\""
        echo "        node_id: ${NODE_ID_1}"
        echo "      kernel:"
        echo "        type: singbox"
        echo "        log_level: info"
        # 检查是否需要证书块
        local need_cert=true
        if [ "$SERVER_TYPE" = "vless" ] && [ "${NODE_REALITY:-0}" = "1" ]; then
            need_cert=false
        fi
        if [[ "$SERVER_TYPE" =~ ^(v2ray|vmess|trojan|vless)$ ]]; then
            if [ "${NODE_TLS:-1}" = "0" ] || [ "${NODE_TLS:-1}" = "none" ] || [ "${NODE_TLS:-1}" = "null" ] || [ "${NODE_TLS:-1}" = "" ]; then
                need_cert=false
            fi
        fi
        if [ "$need_cert" = true ]; then
            make_cert_block "$NODE_DOMAIN"
        fi
        echo "      dns:"
        echo "        servers:"
        echo "          - address: 1.1.1.1"
        echo "            detour: direct"
        echo "          - address: 8.8.8.8"
        echo "            detour: direct"
        echo "        strategy: prefer_ipv4"
        echo "      log:"
        echo "        level: info"
        echo "        output: stdout"
    } > "${INSTALL_DIR}/config.yml"

    if [ -n "${NODE_ID_2:-}" ]; then
        {
            echo "    - panel:"
            echo "        url: \"${PANEL_URL}\""
            echo "        token: \"${API_KEY}\""
            echo "        node_id: ${NODE_ID_2}"
            echo "      kernel:"
            echo "        type: singbox"
            echo "        log_level: info"
            # 检查是否需要证书块
            local need_cert2=true
            if [ "$SERVER_TYPE_2" = "vless" ] && [ "${NODE_REALITY_2:-0}" = "1" ]; then
                need_cert2=false
            fi
            if [[ "$SERVER_TYPE_2" =~ ^(v2ray|vmess|trojan|vless)$ ]]; then
                if [ "${NODE_TLS_2:-1}" = "0" ] || [ "${NODE_TLS_2:-1}" = "none" ] || [ "${NODE_TLS_2:-1}" = "null" ] || [ "${NODE_TLS_2:-1}" = "" ]; then
                    need_cert2=false
                fi
            fi
            if [ "$need_cert2" = true ]; then
                make_cert_block "$NODE_DOMAIN_2"
            fi
            echo "      dns:"
            echo "        servers:"
            echo "          - address: 1.1.1.1"
            echo "            detour: direct"
            echo "          - address: 8.8.8.8"
            echo "            detour: direct"
            echo "        strategy: prefer_ipv4"
            echo "      log:"
            echo "        level: info"
            echo "        output: stdout"
        } >> "${INSTALL_DIR}/config.yml"
        log_info "双节点配置: 节点${NODE_ID_1}(${NODE_DOMAIN}) + 节点${NODE_ID_2}(${NODE_DOMAIN_2})"
    else
        log_info "单节点配置: 节点${NODE_ID_1}(${NODE_DOMAIN})"
    fi
}

# ==================== systemd 服务 ====================

setup_service() {
    log_step "设置 systemd 服务..."
    cat > /etc/systemd/system/xboard-node.service <<EOF
[Unit]
Description=XboardNode-Plus Backend
After=network.target

[Service]
Type=simple
ExecStart=${BIN_PATH} -c ${INSTALL_DIR}/config.yml
Restart=on-failure
RestartSec=5
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable xboard-node
}

# ==================== CF DNS 自动解析 ====================

setup_cf_dns() {
    local domain="$1"
    local type="${2:-}"
    log_step "设置 CF DNS 解析: ${domain}..."

    local vps_ip
    vps_ip=$(curl -s --connect-timeout 5 https://api.ipify.org || curl -s --connect-timeout 5 https://ifconfig.me)
    if [ -z "$vps_ip" ]; then
        log_warn "获取 VPS IP 失败，跳过 DNS 解析"
        return
    fi
    log_info "VPS IP: ${vps_ip}"

    local proxied=false
    if [[ "$type" =~ ^(vless|vmess|v2ray|trojan)$ ]]; then
        read -rp "是否为 ${domain} 开启 CF 代理（小黄云）? [y/N]: " cf_proxy
        [[ "$cf_proxy" =~ ^[Yy]$ ]] && proxied=true
    fi

    # 获取 zone_id
    local zone_name
    zone_name=$(echo "$domain" | awk -F. '{print $(NF-1)"."$NF}')
    local zone_resp
    zone_resp=$(curl -s "https://api.cloudflare.com/client/v4/zones?name=${zone_name}" \
        -H "X-Auth-Email: ${CFUSER}" \
        -H "X-Auth-Key: ${CFGLOBAL}")
    local zone_id
    zone_id=$(echo "$zone_resp" | jq -r '.result[0].id // empty')
    if [ -z "$zone_id" ]; then
        log_warn "获取 Zone ID 失败，跳过 DNS 解析"
        return
    fi

    # 查找已有记录
    local rec_resp
    rec_resp=$(curl -s "https://api.cloudflare.com/client/v4/zones/${zone_id}/dns_records?type=A&name=${domain}" \
        -H "X-Auth-Email: ${CFUSER}" \
        -H "X-Auth-Key: ${CFGLOBAL}")
    local rec_id
    rec_id=$(echo "$rec_resp" | jq -r '.result[0].id // empty')

    local data="{\"type\":\"A\",\"name\":\"${domain}\",\"content\":\"${vps_ip}\",\"ttl\":60,\"proxied\":${proxied}}"
    if [ -n "$rec_id" ]; then
        curl -s -X PUT "https://api.cloudflare.com/client/v4/zones/${zone_id}/dns_records/${rec_id}" \
            -H "X-Auth-Email: ${CFUSER}" \
            -H "X-Auth-Key: ${CFGLOBAL}" \
            -H "Content-Type: application/json" \
            --data "$data" >/dev/null
        log_info "DNS 记录已更新: ${domain} → ${vps_ip} (代理: ${proxied})"
    else
        curl -s -X POST "https://api.cloudflare.com/client/v4/zones/${zone_id}/dns_records" \
            -H "X-Auth-Email: ${CFUSER}" \
            -H "X-Auth-Key: ${CFGLOBAL}" \
            -H "Content-Type: application/json" \
            --data "$data" >/dev/null
        log_info "DNS 记录已创建: ${domain} → ${vps_ip} (代理: ${proxied})"
    fi
}

# ==================== 防火墙 ====================

setup_firewall() {
    log_step "配置防火墙规则..."

    # 不清空所有规则，采用增量添加方式
    log_info "添加防火墙规则（保留现有规则）..."

    # 开放基础端口
    local all_ports=("$NODE_PORT" ${NODE_PORT_2:-})
    local all_hops=("${NODE_PORTS:-}" "${NODE_PORTS_2:-}")

    for port in "${all_ports[@]}"; do
        [ -z "$port" ] && continue
        iptables -C INPUT -p tcp --dport "$port" -j ACCEPT 2>/dev/null || \
            iptables -A INPUT -p tcp --dport "$port" -j ACCEPT
        iptables -C INPUT -p udp --dport "$port" -j ACCEPT 2>/dev/null || \
            iptables -A INPUT -p udp --dport "$port" -j ACCEPT
        log_info "已开放端口: ${port} (TCP+UDP)"
    done

    for hops in "${all_hops[@]}"; do
        [ -z "$hops" ] && continue
        IFS=',' read -ra ranges <<< "$hops"
        for range in "${ranges[@]}"; do
            local ipt_range="${range/-/:}"
            iptables -C INPUT -p udp --dport "$ipt_range" -j ACCEPT 2>/dev/null || \
                iptables -A INPUT -p udp --dport "$ipt_range" -j ACCEPT
            log_info "已开放跳跃端口: ${range} (UDP)"
        done
    done

    # 端口跳跃 NAT 转发：把跳跃范围 UDP 重定向到对应主端口（参考 hy7.sh）
    local iface
    iface=$(ip route get 1.1.1.1 2>/dev/null | awk '/dev/ {for(i=1;i<=NF;i++) if($i=="dev"){print $(i+1); exit}}')
    if [ -n "$iface" ]; then
        # 节点1
        if [ -n "${NODE_PORTS:-}" ] && [ -n "${NODE_PORT:-}" ]; then
            IFS=',' read -ra ranges1 <<< "$NODE_PORTS"
            for range in "${ranges1[@]}"; do
                local r="${range/-/:}"
                while iptables -t nat -C PREROUTING -i "$iface" -p udp --dport "$r" -j REDIRECT --to-ports "$NODE_PORT" 2>/dev/null; do
                    iptables -t nat -D PREROUTING -i "$iface" -p udp --dport "$r" -j REDIRECT --to-ports "$NODE_PORT"
                done
                iptables -t nat -A PREROUTING -i "$iface" -p udp --dport "$r" -j REDIRECT --to-ports "$NODE_PORT"
                log_info "端口跳跃转发: UDP ${range} -> ${NODE_PORT}"
            done
        fi
        # 节点2
        if [ -n "${NODE_PORTS_2:-}" ] && [ -n "${NODE_PORT_2:-}" ]; then
            IFS=',' read -ra ranges2 <<< "$NODE_PORTS_2"
            for range in "${ranges2[@]}"; do
                local r="${range/-/:}"
                while iptables -t nat -C PREROUTING -i "$iface" -p udp --dport "$r" -j REDIRECT --to-ports "$NODE_PORT_2" 2>/dev/null; do
                    iptables -t nat -D PREROUTING -i "$iface" -p udp --dport "$r" -j REDIRECT --to-ports "$NODE_PORT_2"
                done
                iptables -t nat -A PREROUTING -i "$iface" -p udp --dport "$r" -j REDIRECT --to-ports "$NODE_PORT_2"
                log_info "端口跳跃转发: UDP ${range} -> ${NODE_PORT_2}"
            done
        fi
    fi

    # 持久化 NAT/防火墙规则，确保重启后端口跳跃转发仍生效
    mkdir -p /etc/iptables
    iptables-save > /etc/iptables/rules.v4 2>/dev/null || true
    ip6tables-save > /etc/iptables/rules.v6 2>/dev/null || true

    # 创建开机自动恢复规则的 systemd 服务（在节点服务前加载）
    cat > /etc/systemd/system/xboard-iptables.service <<'IPTEOF'
[Unit]
Description=Restore iptables rules for Hysteria2 port hopping
Before=xboard-node.service
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/bin/sh -c 'iptables-restore < /etc/iptables/rules.v4'
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
IPTEOF
    systemctl daemon-reload
    systemctl enable xboard-iptables.service >/dev/null 2>&1
    log_info "防火墙规则已持久化 (重启后自动恢复端口跳跃)"
}

check_firewall_status() {
    echo ""
    echo "=========================================="
    echo "  防火墙端口转发检查"
    echo "=========================================="

    echo ""
    echo ">>> Hysteria2 监听端口:"
    ss -ulnp 2>/dev/null | grep -E "xboard|:${NODE_PORT:-443}" || echo "  未检测到监听 (服务可能未启动)"

    echo ""
    echo ">>> NAT 端口跳跃转发规则:"
    local nat_rules
    nat_rules=$(iptables -t nat -L PREROUTING -n -v 2>/dev/null | grep -i "redir")
    if [ -n "$nat_rules" ]; then
        echo "$nat_rules"
        echo "  ✅ 端口跳跃转发已生效"
    else
        echo "  ❌ 未找到 NAT 转发规则"
    fi

    echo ""
    echo ">>> 开机恢复服务状态:"
    if systemctl is-enabled xboard-iptables.service &>/dev/null; then
        echo "  ✅ xboard-iptables.service 已启用 (重启不掉)"
    else
        echo "  ⚠️  xboard-iptables.service 未启用"
    fi

    echo ""
    echo ">>> 实时连接的目标端口分布 (按 Ctrl+C 退出):"
    echo "  (客户端正在使用时，端口应落在跳跃范围内并不断变化)"
    if ! command -v conntrack >/dev/null 2>&1; then
        log_info "conntrack 未安装，正在自动安装..."
        apt-get install -y -qq conntrack 2>/dev/null || true
    fi
    if command -v conntrack >/dev/null 2>&1; then
        conntrack -L -p udp 2>/dev/null | grep -oP 'dport=\K[0-9]+' | sort -n | uniq -c | tail -20
    else
        echo "  conntrack 安装失败，请手动执行: apt install -y conntrack"
    fi
    echo "=========================================="
}

uninstall() {
    log_warn "即将卸载 xboard-node，此操作不可逆！"
    read -rp "确认卸载? [y/N]: " confirm
    [[ ! "$confirm" =~ ^[Yy]$ ]] && { log_info "已取消"; return; }
    systemctl stop xboard-node 2>/dev/null || true
    systemctl disable xboard-node 2>/dev/null || true
    rm -f /etc/systemd/system/xboard-node.service
    rm -f "$BIN_PATH" /usr/local/bin/xbctl
    rm -rf "$INSTALL_DIR"
    # 清理端口跳跃 iptables 恢复服务
    systemctl stop xboard-iptables.service 2>/dev/null || true
    systemctl disable xboard-iptables.service 2>/dev/null || true
    rm -f /etc/systemd/system/xboard-iptables.service
    systemctl daemon-reload
    # 清理 acme.sh 证书
    if [ -f /root/.acme.sh/acme.sh ]; then
        /root/.acme.sh/acme.sh --uninstall-cronjob 2>/dev/null || true
    fi
    # 清理 cron
    crontab -l 2>/dev/null | grep -v check_cert | crontab - 2>/dev/null || true
    log_info "卸载完成"
}

# ==================== 启动验证 ====================

start_and_verify() {
    log_step "启动服务..."
    systemctl restart xboard-node
    sleep 5

    echo ""
    echo "=========================================="
    if systemctl is-active xboard-node &>/dev/null; then
        echo -e "  服务状态:  ${GREEN}运行中${NC}"
    else
        echo -e "  服务状态:  ${RED}启动失败${NC}"
        journalctl -u xboard-node --no-pager -n 20
        exit 1
    fi
    echo -e "  节点1:     ${CYAN}${NODE_DOMAIN}:${NODE_PORT} (${SERVER_TYPE})${NC}"
    [ -n "${NODE_ID_2:-}" ] && \
        echo -e "  节点2:     ${CYAN}${NODE_DOMAIN_2}:${NODE_PORT_2} (${SERVER_TYPE_2})${NC}"
    echo "=========================================="
    echo ""
    echo "  管理命令:"
    echo "  systemctl status xboard-node"
    echo "  journalctl -u xboard-node -f"
    echo "  systemctl restart xboard-node"
    echo "  cat ${INSTALL_DIR}/config.yml"
    echo ""
}

# ==================== 主函数 ====================

main() {
    echo "=========================================="
    echo "  Multi-Protocol XboardNode-Plus 安装脚本"
    echo "  基于 XboardNode-Plus 1.14+"
    echo "=========================================="
    echo ""

    [ "$EUID" -ne 0 ] && { log_error "请使用 root 权限运行"; exit 1; }

    get_arch
    check_installed

    read -rp "节点1 ID: " NODE_ID_1
    [ -z "$NODE_ID_1" ] && { log_error "节点ID不能为空"; exit 1; }

    read -rp "节点2 ID (留空则只安装一个节点): " NODE_ID_2

    read -rp "面板地址 [${DEFAULT_PANEL}]: " PANEL_URL
    PANEL_URL="${PANEL_URL:-$DEFAULT_PANEL}"
    PANEL_URL="${PANEL_URL%/}"

    read -rp "API Key [${DEFAULT_KEY}]: " API_KEY
    API_KEY="${API_KEY:-$DEFAULT_KEY}"

    read -rp "Cloudflare API Token [${DEFAULT_CFKEY}]: " CFKEY
    CFKEY="${CFKEY:-$DEFAULT_CFKEY}"

    read -rp "Cloudflare 账户邮箱 [${DEFAULT_CFUSER}]: " CFUSER
    CFUSER="${CFUSER:-$DEFAULT_CFUSER}"

    read -rp "Cloudflare Global API Key [${DEFAULT_CFGLOBAL}]: " CFGLOBAL
    CFGLOBAL="${CFGLOBAL:-$DEFAULT_CFGLOBAL}"

    read -rp "DDNS 域名 (留空跳过): " DDNS_DOMAIN

    check_installed
    init_environment
    setup_aliyun_ddns
    system_optimize
    fetch_config
    setup_ddns  # DDNS 自动更新IP（支持Cloudflare/阿里云）
    if [ "$SERVER_TYPE" = "vless" ]; then
        if [ "${NODE_REALITY:-0}" = "1" ]; then
            generate_reality_keypair "$NODE_DOMAIN" && fetch_config
        fi
    fi
    if [ -n "${NODE_ID_2:-}" ] && [ "$SERVER_TYPE_2" = "vless" ]; then
        if [ "${NODE_REALITY_2:-0}" = "1" ]; then
            generate_reality_keypair "$NODE_DOMAIN_2" && fetch_config
        fi
    fi
    setup_cf_dns "$NODE_DOMAIN" "$SERVER_TYPE"
    [ -n "${NODE_ID_2:-}" ] && setup_cf_dns "$NODE_DOMAIN_2" "$SERVER_TYPE_2"
    request_cert "$NODE_DOMAIN" "$SERVER_TYPE" "${NODE_REALITY:-0}" "${NODE_TLS:-1}"
    [ -n "${NODE_ID_2:-}" ] && request_cert "$NODE_DOMAIN_2" "$SERVER_TYPE_2" "${NODE_REALITY_2:-0}" "${NODE_TLS_2:-1}"
    install_binary
    write_config
    setup_service
    setup_firewall
    setup_cert_renew_cron
    start_and_verify

    log_info "安装完成"

    while true; do
        echo ""
        echo "  1) 查看实时日志"
        echo "  2) 重启服务"
        echo "  3) 卸载"
        echo "  4) 重启系统并退出"
        echo "  5) 查看防火墙端口转发"
        read -rp "请选择 [1/2/3/4/5]: " action
        case "${action:-4}" in
            1) journalctl -u xboard-node -f ;;
            2) systemctl restart xboard-node && log_info "服务已重启" ;;
            3) uninstall ;;
            4) break ;;
            5) check_firewall_status ;;
        esac
    done

    echo -e "${YELLOW}系统将在 5 秒后重启...${NC}"
    for i in {5..1}; do echo -e "${YELLOW}倒计时: $i${NC}"; sleep 1; done
    reboot
}

main "$@"