#!/usr/bin/env bash
# vless-ban 一键安装 / 管理脚本
# 菜单：bash <(curl -fsSL https://raw.githubusercontent.com/acc66/XboardNode-Plus/main/vless-ban.sh)
# 一条命令安装（只填节点 ID）：
#   KEY=面板通讯密钥 bash <(curl -fsSL https://raw.githubusercontent.com/acc66/XboardNode-Plus/main/vless-ban.sh) install 节点ID
# 可选环境变量：PANEL=面板地址  ALLOW=白名单IP  CDN=y
set -euo pipefail

DEFAULT_PANEL="https://api10.seox9.com"
REPO="acc66/XboardNode-Plus"
RAW="https://raw.githubusercontent.com/${REPO}/main"
INSTALL_ROOT="/etc/XboardNode-Plus"
CONFIG_FILE="${INSTALL_ROOT}/config.yml"
BLACKLIST="${INSTALL_ROOT}/blacklist.txt"
SERVICE="xboard-node"

red()    { printf '\033[31m%s\033[0m\n' "$*"; }
green()  { printf '\033[32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }

need_root() {
    if [ "$(id -u)" -ne 0 ]; then
        red "请用 root 运行（sudo -i 后再执行）"
        exit 1
    fi
}

need_installed() {
    if [ ! -f "$CONFIG_FILE" ]; then
        red "未安装，请先选择 1 安装"
        exit 1
    fi
}

ask() { # ask 变量名 提示 [默认值]
    local __var="$1" __prompt="$2" __def="${3:-}" __ans
    if [ -n "$__def" ]; then
        read -r -p "${__prompt} [${__def}]: " __ans || true
        __ans="${__ans:-$__def}"
    else
        read -r -p "${__prompt}: " __ans || true
    fi
    printf -v "$__var" '%s' "$__ans"
}

my_ip() {
    curl -fsS4 --max-time 5 https://api.ipify.org 2>/dev/null || true
}

# 删除 config.yml 中已有的顶层 guard: 段
strip_guard() {
    awk '
        /^guard:/ { skip = 1; next }
        skip && /^[^[:space:]#]/ { skip = 0 }
        !skip { print }
    ' "$CONFIG_FILE" > "${CONFIG_FILE}.tmp" && mv "${CONFIG_FILE}.tmp" "$CONFIG_FILE"
    chmod 600 "$CONFIG_FILE"
}

current_guard_value() { # current_guard_value allow
    awk -v key="$1" '
        /^guard:/ { in_g = 1; next }
        in_g && /^[^[:space:]#]/ { in_g = 0 }
        in_g && $1 == key":" { sub(/^[^:]*:[[:space:]]*/, ""); gsub(/"/, ""); print; exit }
    ' "$CONFIG_FILE"
}

write_guard() { # write_guard allow real_ip_header
    local allow="$1" header="$2"
    strip_guard
    [ -n "$(tail -c1 "$CONFIG_FILE")" ] && echo >> "$CONFIG_FILE"
    {
        echo "guard:"
        echo "  enabled: true"
        echo "  blacklist: \"${BLACKLIST}\""
        echo "  allow: \"${allow}\""
        if [ -n "$header" ]; then
            echo "  real_ip_header: \"${header}\""
        fi
    } >> "$CONFIG_FILE"
}

configure_guard() {
    local ip allow cdn header=""
    ip=$(my_ip)
    if [ -n "${ALLOW+x}" ]; then
        allow="$ALLOW"
    else
        echo
        yellow "白名单 IP：这些 IP 永远不会被拉黑，建议填你自己电脑/手机的公网 IP，多个用逗号分隔，直接回车跳过"
        ask allow "白名单 IP" "$(current_guard_value allow)"
    fi
    if [ -n "${CDN+x}" ]; then
        cdn="$CDN"
    else
        echo
        yellow "节点域名是否开了 Cloudflare 小黄云（CDN）？直接回车 = 没开"
        yellow "开了却选 n，会把 CF 的 IP 拉黑导致所有用户断线"
        ask cdn "走 CDN 吗 (y/n)" "n"
    fi
    if [[ "$cdn" =~ ^[Yy] ]]; then
        header="CF-Connecting-IP"
    fi
    write_guard "$allow" "$header"
    green "已写入拉黑配置：${CONFIG_FILE}"
    if [ -n "$ip" ]; then
        echo "（本机公网 IP：${ip}）"
    fi
}

restart_and_check() {
    systemctl restart "$SERVICE"
    sleep 3
    if ! systemctl is-active --quiet "$SERVICE"; then
        red "服务启动失败，最近日志："
        journalctl -u "$SERVICE" -n 30 --no-pager
        exit 1
    fi
    green "服务运行中"
    show_guard_log
}

show_guard_log() {
    echo
    echo "---- 拉黑相关日志（最近 20 条）----"
    # 只看本次启动以来的日志；先存变量再匹配，避免 pipefail + grep -q 的 SIGPIPE 误判
    local inv logs
    inv=$(systemctl show -p InvocationID --value "$SERVICE" 2>/dev/null || true)
    if [ -n "$inv" ]; then
        logs=$(journalctl --no-pager "_SYSTEMD_INVOCATION_ID=${inv}" 2>/dev/null | grep "guard" || true)
    else
        logs=$(journalctl -u "$SERVICE" --no-pager -n 5000 2>/dev/null | grep "guard" || true)
    fi
    [ -n "$logs" ] && printf '%s\n' "$logs" | tail -20
    if [[ "$logs" != *"guard: enabled"* ]]; then
        yellow "还没看到 guard: enabled。可能节点还在同步配置，或节点不适用（开了 TLS / 不是 ws）"
        yellow "稍等片刻后选 4 再看一次"
    fi
}

do_install() { # do_install [节点ID]
    need_root
    local panel="${PANEL:-$DEFAULT_PANEL}" token="${KEY:-}" node_id="${1:-${NODE_ID:-}}"
    echo
    echo "面板地址：${panel}"
    if [ -z "$token" ]; then
        # 密钥不写进公开脚本，安装时输入（不回显）或用 KEY=xxx 传入
        read -r -s -p "面板通讯密钥: " token || true
        echo
    fi
    [ -z "$node_id" ] && ask node_id "节点 ID"
    if [ -z "$token" ] || ! [[ "$node_id" =~ ^[0-9]+$ ]]; then
        red "密钥不能为空，节点 ID 必须是数字"
        exit 1
    fi

    local tmp
    tmp=$(mktemp)
    curl -fsSL "${RAW}/install.sh" -o "$tmp"
    bash "$tmp" install --panel "$panel" --token "$token" --node-id "$node_id" --kernel auto --yes
    rm -f "$tmp"

    configure_guard
    restart_and_check
    echo
    green "安装完成"
    echo "测试：在另一台机器执行  curl -i http://本机IP:节点端口/   应返回 nginx 页面，再执行一次会被断开"
    echo "管理：再次运行本脚本"
}

do_upgrade() {
    need_root
    need_installed
    local tmp
    tmp=$(mktemp)
    curl -fsSL "${RAW}/install.sh" -o "$tmp"
    bash "$tmp" upgrade --yes
    rm -f "$tmp"
    if ! grep -q '^guard:' "$CONFIG_FILE"; then
        yellow "配置里还没有拉黑设置，现在添加"
        configure_guard
        restart_and_check
    fi
    green "升级完成，配置和黑名单保留不变"
}

do_list() {
    need_installed
    if [ ! -s "$BLACKLIST" ]; then
        echo "黑名单为空"
        return
    fi
    echo "共 $(grep -cv '^[[:space:]]*\(#\|$\)' "$BLACKLIST" || true) 个 IP："
    cat "$BLACKLIST"
}

do_unban() {
    need_root
    need_installed
    local ip
    ask ip "要解封的 IP"
    if [ -z "$ip" ] || [ ! -f "$BLACKLIST" ]; then
        return
    fi
    if ! awk -v ip="$ip" '$1 == ip { found = 1 } END { exit !found }' "$BLACKLIST"; then
        yellow "黑名单里没有 ${ip}"
        return
    fi
    awk -v ip="$ip" '$1 != ip' "$BLACKLIST" > "${BLACKLIST}.tmp" && mv "${BLACKLIST}.tmp" "$BLACKLIST"
    green "已解封 ${ip}（5 秒内生效，无需重启）"
}

do_clear() {
    need_root
    need_installed
    local yn
    ask yn "确定清空全部黑名单？(y/n)" "n"
    if [[ "$yn" =~ ^[Yy] ]]; then
        rm -f "$BLACKLIST"
        green "已清空（5 秒内生效）"
    fi
}

do_reconfigure() {
    need_root
    need_installed
    configure_guard
    restart_and_check
}

do_disable() {
    need_root
    need_installed
    strip_guard
    restart_and_check
    green "已关闭拉黑，内核恢复直接监听端口（黑名单文件保留）"
}

do_uninstall() {
    need_root
    local tmp
    tmp=$(mktemp)
    curl -fsSL "${RAW}/install.sh" -o "$tmp"
    bash "$tmp" uninstall
    rm -f "$tmp"
}

do_status() {
    need_installed
    systemctl status "$SERVICE" --no-pager -l | head -5 || true
    show_guard_log
}

menu() {
    echo
    echo "========== vless-ban 一键管理 =========="
    echo " 错误路径（如非 /mm）访问 → 拉黑 IP + nginx 蜜罐"
    echo "----------------------------------------"
    echo " 1. 安装"
    echo " 2. 升级（保留配置和黑名单）"
    echo " 3. 修改白名单 / CDN 设置"
    echo " 4. 查看状态和拉黑日志"
    echo " 5. 查看黑名单"
    echo " 6. 解封 IP"
    echo " 7. 清空黑名单"
    echo " 8. 关闭拉黑功能"
    echo " 9. 卸载"
    echo " 0. 退出"
    echo "========================================"
    local c
    ask c "请选择"
    case "$c" in
        1) do_install ;;
        2) do_upgrade ;;
        3) do_reconfigure ;;
        4) do_status ;;
        5) do_list ;;
        6) do_unban ;;
        7) do_clear ;;
        8) do_disable ;;
        9) do_uninstall ;;
        *) exit 0 ;;
    esac
}

case "${1:-}" in
    install)   do_install "${2:-}" ;;
    upgrade)   do_upgrade ;;
    status)    do_status ;;
    list)      do_list ;;
    unban)     do_unban ;;
    clear)     do_clear ;;
    uninstall) do_uninstall ;;
    *)         menu ;;
esac
