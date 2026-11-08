package guard

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/cedar2025/xboard-node/internal/nlog"
)

// Kernel wraps a kernel and, for eligible ws nodes, moves the kernel listener
// to loopback and puts a Proxy on the public port. All other methods pass
// through unchanged.
type Kernel struct {
	kernel.Kernel
	cfg config.GuardConfig
	bl  *Blacklist

	mu    sync.Mutex
	proxy *Proxy
	opts  Options
}

// Wrap returns k unchanged when the guard is disabled.
func Wrap(k kernel.Kernel, cfg config.GuardConfig) kernel.Kernel {
	if !cfg.Enabled || k == nil {
		return k
	}
	return &Kernel{Kernel: k, cfg: cfg, bl: SharedBlacklist(cfg.Blacklist, cfg.Allow)}
}

// Plan decides whether nc can be guarded. It returns the ws path and whether
// a path prefix match is needed, or a reason when the node is skipped.
func Plan(nc *model.NodeSpec) (path string, prefix bool, skip string) {
	if nc == nil {
		return "", false, "no config"
	}
	if ssPath, _, tls, ok := model.SSWebSocketPlugin(nc); ok {
		if tls {
			return "", false, "plugin tls enabled"
		}
		return cleanPath(ssPath), false, ""
	}
	switch nc.Protocol {
	case "vless", "vmess", "trojan":
	default:
		return "", false, "protocol " + nc.Protocol + " has no ws path"
	}
	if nc.Network != "ws" {
		return "", false, "network " + nc.Network + " is not ws"
	}
	if nc.TLS != 0 {
		return "", false, "tls/reality enabled (path not visible)"
	}
	if nc.GetProxyProtocol() {
		return "", false, "proxy protocol enabled"
	}
	path = "/"
	if v, ok := nc.NetworkSettings["path"].(string); ok && strings.TrimSpace(v) != "" {
		path = v
	}
	// sing-box puts early data in the path unless a header name is set.
	if _, ok := nc.NetworkSettings["max_early_data"]; ok {
		if h, _ := nc.NetworkSettings["early_data_header_name"].(string); h == "" {
			prefix = true
		}
	}
	return cleanPath(path), prefix, ""
}

func cleanPath(p string) string {
	p = strings.TrimSpace(p)
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i] // "/mm?ed=2048" → "/mm"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

func (g *Kernel) Start(nc *model.NodeSpec, users []model.UserSpec, tc kernel.TLSCert) error {
	return g.apply(nc, func(inner *model.NodeSpec) error { return g.Kernel.Start(inner, users, tc) })
}

func (g *Kernel) Reload(nc *model.NodeSpec, users []model.UserSpec, tc kernel.TLSCert) error {
	return g.apply(nc, func(inner *model.NodeSpec) error { return g.Kernel.Reload(inner, users, tc) })
}

func (g *Kernel) Stop() {
	g.Kernel.Stop()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closeProxyLocked()
}

func (g *Kernel) apply(nc *model.NodeSpec, run func(*model.NodeSpec) error) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	tag := fmt.Sprintf("%s:%d", nodeProto(nc), nodePort(nc))
	path, prefix, skip := Plan(nc)
	if skip != "" {
		if nc != nil && model.EffectiveNetwork(nc) == "ws" {
			nlog.Core().Warn("guard: not guarding node", "tag", tag, "reason", skip)
		}
		// Free the public port before the kernel binds it directly.
		g.closeProxyLocked()
		return run(nc)
	}

	listen := net.JoinHostPort(nc.ListenIP, strconv.Itoa(nc.ServerPort))
	want := Options{
		Listen:       listen,
		Path:         path,
		PathPrefix:   prefix,
		RealIPHeader: strings.TrimSpace(g.cfg.RealIPHeader),
		NoBan:        g.cfg.NoBan,
		Tag:          tag,
	}

	// Keep the existing proxy (and its backend port) when nothing changed,
	// so a kernel reload does not drop the public listener.
	backend := ""
	if g.proxy != nil {
		cur := g.opts
		cur.Backend = ""
		if cur == want {
			backend = g.opts.Backend
		} else {
			g.closeProxyLocked()
		}
	}
	if backend == "" {
		port, err := freeLoopbackPort()
		if err != nil {
			return fmt.Errorf("guard: pick backend port: %w", err)
		}
		backend = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	}
	want.Backend = backend

	inner := *nc
	inner.ListenIP = "127.0.0.1"
	inner.ServerPort, _ = strconv.Atoi(backend[strings.LastIndexByte(backend, ':')+1:])
	if err := run(&inner); err != nil {
		return err
	}

	if g.proxy == nil {
		p, err := Listen(want, g.bl)
		if err != nil {
			g.Kernel.Stop()
			return fmt.Errorf("guard: listen %s: %w", listen, err)
		}
		g.proxy, g.opts = p, want
		nlog.Core().Info("guard: enabled", "tag", tag, "listen", listen, "path", path,
			"prefix", prefix, "backend", backend, "banned", g.bl.Len())
	}
	return nil
}

func (g *Kernel) closeProxyLocked() {
	if g.proxy != nil {
		g.proxy.Close()
		g.proxy = nil
		g.opts = Options{}
	}
}

func freeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func nodeProto(nc *model.NodeSpec) string {
	if nc == nil {
		return ""
	}
	return nc.Protocol
}

func nodePort(nc *model.NodeSpec) int {
	if nc == nil {
		return 0
	}
	return nc.ServerPort
}
