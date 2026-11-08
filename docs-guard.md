# 防探测拉黑（guard）

参考 [ws-tunnel-pro](https://github.com/acc66/ws-tunnel-pro)：在非 TLS 的 WebSocket 节点前面加一层过滤，探测者访问错误路径即拉黑 IP，并返回 nginx 默认页（蜜罐）。

部署步骤见 [docs-guard-tutorial.md](docs-guard-tutorial.md)。

## 规则

| 请求 | 处理 |
| --- | --- |
| 已拉黑 IP | 建连即断开（设置 `real_ip_header` 时改为返回蜜罐页） |
| 路径不是节点 ws path（如 `/mm`） | 拉黑 IP + 蜜罐页 |
| 非 HTTP 数据（TLS 握手、原始协议探测） | 拉黑 IP |
| 路径正确但不是 WebSocket 升级 | 蜜罐页，不拉黑 |
| 连上不发数据 / 超时 / 连接重置 | 断开，不拉黑 |
| 路径正确的 WebSocket 升级 | 转发给内核 |

- 路径精确匹配，`?ed=2048` 等查询参数会忽略；`/mm/x`、`/mmx` 视为错误路径。
- 例外：sing-box 节点设置了 `max_early_data` 但没设 `early_data_header_name` 时，客户端会把 early data 拼在路径后面，此时改为前缀匹配。
- 转发时 `X-Forwarded-For` 会被覆盖为真实客户端 IP，内核的在线 IP、设备数限制照常生效，客户端无法伪造。

## 适用节点

- `vless` / `vmess` / `trojan` + `ws`，且未开启 TLS / Reality、未开启 proxy protocol
- `shadowsocks` + `v2ray-plugin` / `xray-plugin`（`mode=websocket`，未开启插件 `tls`）
  - 自动使用 xray 内核（sing-box 的 ss 入站不支持插件）
  - 客户端插件需关闭 mux（`mux=0`），xray 的 ss 入站不解析 mux.cool

其他节点不受影响，按原方式直接监听。开启 TLS 的 ws 节点会打日志 `guard: not guarding node` 并跳过。

## 配置

```yaml
guard:
  enabled: true
  blacklist: "/etc/xboard-node/blacklist.txt"  # 默认 <config_dir>/blacklist.txt，多节点共用
  allow: "1.2.3.4,5.6.7.8"                      # 永不拉黑
  real_ip_header: ""                            # 走 CDN 时填 CF-Connecting-IP
  no_ban: false                                 # true = 只返回蜜罐页，不拉黑（试运行）
```

拉黑文件每行一个 IP，`#` 后为注释（时间 + 节点 + 请求）。文件修改后 5 秒内自动重载：删行即解封，手动加行即拉黑，删除文件即全部解封。

## 走 CDN（Cloudflare）时

经 CDN 回源时 TCP 对端是 CDN 节点 IP。不设 `real_ip_header` 会把 CDN IP 拉黑，导致所有用户断连。

- 设置 `real_ip_header: CF-Connecting-IP`，按真实访客 IP 拉黑。
- 该头可被伪造，只有当端口仅允许 CDN 回源（防火墙只放行 Cloudflare IP 段）时才安全。
- 若混合直连和 CDN，请把直连流量放到另一个端口。

## 原理

guard 占用节点公网端口，内核改为监听 `127.0.0.1` 的随机端口。用户或配置热更新不会重建 guard 监听；修改端口、路径等会重建。
