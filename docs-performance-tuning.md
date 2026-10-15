# 性能调优与排查（CPU / 内存）

适用于节点 CPU 或内存占用偏高的情况。默认安装路径：

- 配置：`/etc/XboardNode-Plus/config.yml`
- 二进制：`/usr/local/bin/xboard-node`
- 服务：`xboard-node.service`

## 1. 先看现状

```bash
xbctl status
systemctl status xboard-node            # Memory: 一行即当前 RSS
top -p "$(pidof xboard-node)"            # 实时 CPU / RES
ps -o pid,rss,vsz,%cpu,%mem,nlwp -p "$(pidof xboard-node)"
```

记录一个基线（空闲时、高峰时各一次），后面调参对比用。

## 2. 内存：限制 Go 运行时

在 `config.yml` 顶层（多实例时也放顶层，会被所有实例继承）加入：

```yaml
runtime:
  gomemlimit: "200MiB"   # 软上限，超过后 GC 更积极回收
  gogc: 50               # 默认 100；越小内存越低，CPU 略高
```

取值建议：

| 机器内存 | gomemlimit | gogc |
|---|---|---|
| 512MB | 150MiB | 50 |
| 1GB | 300MiB | 50~75 |
| 2GB 及以上 | 机器可用内存的 60%~80% | 100 |

- `gomemlimit` 是软限制，不会因为超出而崩溃，但设得过低会导致 GC 频繁、CPU 升高。
- 环境变量 `GOMEMLIMIT` / `GOGC` 优先级高于配置文件，可写到 `/etc/XboardNode-Plus/credentials.env`。

修改后重启：

```bash
xbctl restart
xbctl logs | grep "runtime:"     # 应看到 GOGC set / GOMEMLIMIT set
```

## 3. CPU：降低后台任务频率

```yaml
node:
  track_interval: 10            # 流量/在线采集间隔（秒），默认 10；用户多可调到 20~30
  device_report_interval: 30    # 设备上报间隔（秒），默认 30；可调到 60
ws:
  status_interval: 10           # WS 状态上报（秒），默认 10；可调到 30
```

注意：`track_interval` 调大会让面板上的在线人数和实时速度刷新变慢，流量统计总量不受影响。

## 4. 日志级别

`debug` 级别在连接多时会产生大量日志，明显增加 CPU 和磁盘 IO。生产环境用 `info` 或 `warn`：

```yaml
log:
  level: "warn"
```

内核日志同理（`kernel.log_level`）。

## 5. 深入排查：pprof

需要定位到具体函数时，用 Go 自带的 pprof。下面是临时排查方法，排查完请恢复原二进制。

1) 在 `cmd/xboard-node/main.go` 的 import 中加一行，并在 `main()` 开头启动监听（只监听本机）：

```go
import _ "net/http/pprof"

go func() { _ = http.ListenAndServe("127.0.0.1:6060", nil) }()
```

2) 编译并替换（机器内存小于 2GB 时先加 swap，否则链接阶段会被 OOM 杀掉）：

```bash
fallocate -l 2G /swapfile && chmod 600 /swapfile && mkswap /swapfile && swapon /swapfile
make build
systemctl stop xboard-node && cp xboard-node /usr/local/bin/xboard-node && systemctl start xboard-node
```

3) 采样：

```bash
# CPU：采 30 秒，看最耗时的函数
go tool pprof -top http://127.0.0.1:6060/debug/pprof/profile?seconds=30

# 内存：当前存活对象
go tool pprof -top http://127.0.0.1:6060/debug/pprof/heap

# 内存：累计分配（找 GC 压力来源）
go tool pprof -sample_index=alloc_space -top http://127.0.0.1:6060/debug/pprof/heap

# goroutine 数量是否持续增长（泄漏）
curl -s http://127.0.0.1:6060/debug/pprof/goroutine?debug=1 | head -5
```

4) 在本地看火焰图（先 `ssh -L 6060:127.0.0.1:6060 root@节点IP`）：

```bash
go tool pprof -http=:8080 http://127.0.0.1:6060/debug/pprof/profile?seconds=30
```

pprof 端口不要监听公网地址，它没有鉴权。

## 6. 常见现象对照

| 现象 | 可能原因 | 处理 |
|---|---|---|
| RSS 只涨不降 | 未设 `gomemlimit`，Go 默认等到堆翻倍才 GC | 第 2 节 |
| 空闲时 CPU 也有规律尖峰 | 采集/上报间隔过短，或 debug 日志 | 第 3、4 节 |
| 高峰时 CPU 高，pprof 显示加密/TLS 函数 | 正常的协议开销 | 换更高主频机器或分流节点 |
| goroutine 数持续增长 | 连接未正确关闭 | 抓 goroutine profile 提 issue |
| 编译时 `signal: killed` | 内存不足 | 加 swap 后再编译 |
