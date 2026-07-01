bash << 'EOF'
echo "=========================================="
echo "  XboardNode 完整清理脚本"
echo "=========================================="
echo ""

# 停止服务
echo "[1/7] 停止服务..."
systemctl stop xboard-node 2>/dev/null || true

# 禁用服务
echo "[2/7] 禁用服务..."
systemctl disable xboard-node 2>/dev/null || true

# 删除 systemd 服务文件
echo "[3/7] 删除服务文件..."
rm -f /etc/systemd/system/xboard-node.service
systemctl daemon-reload

# 删除二进制文件
echo "[4/7] 删除二进制文件..."
rm -f /usr/local/bin/xboard-node
rm -f /usr/local/bin/xbctl

# 删除配置目录
echo "[5/7] 删除配置目录..."
rm -rf /etc/xboard-node
rm -rf /etc/XboardNode-Plus

# 清理 crontab 中的证书检测任务
echo "[6/7] 清理 crontab..."
crontab -l 2>/dev/null | grep -v "check_cert\|xboard-node" | crontab - 2>/dev/null || true

echo "[7/7] 验证清理结果..."
echo ""
echo "=========================================="
echo "  清理结果验证"
echo "=========================================="

# 检查服务
echo ""
echo ">>> 服务状态:"
if systemctl status xboard-node 2>&1 | grep -qE "could not be found|inactive"; then
    echo "✅ 服务已删除"
else
    echo "❌ 服务仍存在"
fi

# 检查二进制
echo ""
echo ">>> 二进制文件:"
if [ ! -f /usr/local/bin/xboard-node ]; then
    echo "✅ xboard-node 已删除"
else
    echo "❌ xboard-node 仍存在"
fi

if [ ! -f /usr/local/bin/xbctl ]; then
    echo "✅ xbctl 已删除"
else
    echo "❌ xbctl 仍存在"
fi

# 检查配置目录
echo ""
echo ">>> 配置目录:"
if [ ! -d /etc/xboard-node ]; then
    echo "✅ /etc/xboard-node 已删除"
else
    echo "❌ /etc/xboard-node 仍存在"
fi

if [ ! -d /etc/XboardNode-Plus ]; then
    echo "✅ /etc/XboardNode-Plus 已删除"
else
    echo "❌ /etc/XboardNode-Plus 仍存在"
fi

# 检查进程
echo ""
echo ">>> 进程检查:"
if ps aux | grep -i xboard | grep -v grep > /dev/null; then
    echo "⚠️  仍有残留进程:"
    ps aux | grep -i xboard | grep -v grep
else
    echo "✅ 无残留进程"
fi

# 保留的证书
echo ""
echo ">>> 证书文件 (保留):"
if [ -d /root/.acme.sh ]; then
    cert_count=$(find /root/.acme.sh -name "*.help600.com" -type d 2>/dev/null | wc -l)
    if [ "$cert_count" -gt 0 ]; then
        echo "📁 保留 $cert_count 个证书目录 (可复用)"
        find /root/.acme.sh -name "*.help600.com" -type d 2>/dev/null | head -3
    else
        echo "📭 无证书文件"
    fi
else
    echo "📭 无 acme.sh 证书"
fi

echo ""
echo "=========================================="
echo "✅ 清理完成！可以重新安装"
echo "=========================================="
echo ""

EOF
