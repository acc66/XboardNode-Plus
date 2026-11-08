package guard

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
)

// stubKernel binds the requested port like a real kernel would and answers
// ws upgrades with 101.
type stubKernel struct {
	kernel.Kernel
	ln     net.Listener
	starts []model.NodeSpec
}

func (s *stubKernel) Start(nc *model.NodeSpec, _ []model.UserSpec, _ kernel.TLSCert) error {
	s.Stop()
	s.starts = append(s.starts, *nc)
	ln, err := net.Listen("tcp", net.JoinHostPort(nc.ListenIP, strconv.Itoa(nc.ServerPort)))
	if err != nil {
		return err
	}
	s.ln = ln
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				if _, err := http.ReadRequest(bufio.NewReader(c)); err == nil {
					io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\n\r\n")
				}
			}()
		}
	}()
	return nil
}

func (s *stubKernel) Reload(nc *model.NodeSpec, u []model.UserSpec, tc kernel.TLSCert) error {
	return s.Start(nc, u, tc)
}

func (s *stubKernel) Stop() {
	if s.ln != nil {
		s.ln.Close()
		s.ln = nil
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	p, err := freeLoopbackPort()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// userNode mirrors the reported node:
// gcp1.help600.com;2086;0;ws;;path=/mm|server=gcp1.help600.com|host=xg2.apple9.org|outside_port=2086
func userNode(port int) *model.NodeSpec {
	return &model.NodeSpec{
		Protocol:        "vless",
		ListenIP:        "127.0.0.1",
		ServerPort:      port,
		Network:         "ws",
		NetworkSettings: map[string]any{"path": "/mm", "headers": map[string]any{"Host": "xg2.apple9.org"}},
	}
}

func TestPlan(t *testing.T) {
	cases := []struct {
		name   string
		nc     *model.NodeSpec
		path   string
		prefix bool
		skip   bool
	}{
		{"vless ws", userNode(2086), "/mm", false, false},
		{"query stripped", &model.NodeSpec{Protocol: "vmess", Network: "ws", NetworkSettings: map[string]any{"path": "/mm?ed=2048"}}, "/mm", false, false},
		{"no path", &model.NodeSpec{Protocol: "trojan", Network: "ws"}, "/", false, false},
		{"singbox early data in path", &model.NodeSpec{Protocol: "vless", Network: "ws", NetworkSettings: map[string]any{"path": "/mm", "max_early_data": 2048}}, "/mm", true, false},
		{"early data header", &model.NodeSpec{Protocol: "vless", Network: "ws", NetworkSettings: map[string]any{"path": "/mm", "max_early_data": 2048, "early_data_header_name": "Sec-WebSocket-Protocol"}}, "/mm", false, false},
		{"tls", &model.NodeSpec{Protocol: "vless", Network: "ws", TLS: 1}, "", false, true},
		{"tcp", &model.NodeSpec{Protocol: "vless", Network: "tcp"}, "", false, true},
		{"proxy protocol", &model.NodeSpec{Protocol: "vless", Network: "ws", AcceptProxyProtocol: true}, "", false, true},
		{"ss plain", &model.NodeSpec{Protocol: "shadowsocks"}, "", false, true},
		{"ss v2ray-plugin", &model.NodeSpec{Protocol: "shadowsocks", Plugin: "v2ray-plugin", PluginOpt: "mode=websocket;host=xg2.apple9.org;path=/mm"}, "/mm", false, false},
		{"ss xray-plugin default mode", &model.NodeSpec{Protocol: "shadowsocks", Plugin: "xray-plugin", PluginOpt: "path=/mm"}, "/mm", false, false},
		{"ss plugin tls", &model.NodeSpec{Protocol: "shadowsocks", Plugin: "v2ray-plugin", PluginOpt: "tls;path=/mm"}, "", false, true},
		{"ss quic", &model.NodeSpec{Protocol: "shadowsocks", Plugin: "v2ray-plugin", PluginOpt: "mode=quic"}, "", false, true},
	}
	for _, tc := range cases {
		path, prefix, skip := Plan(tc.nc)
		if (skip != "") != tc.skip || path != tc.path || prefix != tc.prefix {
			t.Errorf("%s: got path=%q prefix=%v skip=%q", tc.name, path, prefix, skip)
		}
	}
}

func TestWrapMovesKernelToLoopback(t *testing.T) {
	stub := &stubKernel{}
	cfg := config.GuardConfig{Enabled: true, Blacklist: filepath.Join(t.TempDir(), "bl.txt")}
	k := Wrap(stub, cfg).(*Kernel)
	port := freePort(t)

	if err := k.Start(userNode(port), nil, kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	defer k.Stop()
	inner := stub.starts[0]
	if inner.ListenIP != "127.0.0.1" || inner.ServerPort == port {
		t.Fatalf("kernel not moved: %+v", inner)
	}

	pub := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	probe := func(path string) int {
		c, err := net.Dial("tcp", pub)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		io.WriteString(c, wsRequest(path, ""))
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			return -1
		}
		return resp.StatusCode
	}
	if got := probe("/mm"); got != 101 {
		t.Fatalf("/mm → %d, want 101", got)
	}

	// Reload with the same config keeps the proxy and backend port.
	backend := k.opts.Backend
	if err := k.Reload(userNode(port), nil, kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	if k.opts.Backend != backend || stub.starts[1].ServerPort != inner.ServerPort {
		t.Fatal("reload changed backend port")
	}
	if got := probe("/mm"); got != 101 {
		t.Fatalf("after reload /mm → %d", got)
	}

	// Switching to TLS releases the public port to the kernel.
	tlsNode := userNode(port)
	tlsNode.TLS = 1
	if err := k.Reload(tlsNode, nil, kernel.TLSCert{}); err != nil {
		t.Fatalf("reload to unguarded: %v", err)
	}
	if k.proxy != nil || stub.starts[2].ServerPort != port {
		t.Fatal("unguarded node should bind the public port directly")
	}
}

func TestWrapDisabledReturnsKernel(t *testing.T) {
	stub := &stubKernel{}
	if Wrap(stub, config.GuardConfig{}) != kernel.Kernel(stub) {
		t.Fatal("disabled guard must not wrap")
	}
}
