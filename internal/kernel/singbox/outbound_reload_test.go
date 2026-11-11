package singbox

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/service"
	"golang.org/x/net/proxy"
)

func freeTestPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// A real CONNECT proxy lets the test prove which dynamically loaded outbound
// handled each request, without depending on the Internet or panel credentials.
func testHTTPProxy(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	count := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", 400)
			return
		}
		upstream, err := net.DialTimeout("tcp", r.Host, time.Second)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		count.Add(1)
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		go func() { defer conn.Close(); defer upstream.Close(); _, _ = io.Copy(upstream, rw) }()
		go func() { defer conn.Close(); defer upstream.Close(); _, _ = io.Copy(conn, upstream) }()
	}))
	t.Cleanup(server.Close)
	return server, count
}

func TestReloadOutboundsWithoutRestart(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	t.Cleanup(target.Close)
	p1, count1 := testHTTPProxy(t)
	p2, count2 := testHTTPProxy(t)
	users := []model.UserSpec{{ID: 1, UUID: "aaaaaaaa-1111-2222-3333-444444444444"}}
	nc := &model.NodeSpec{Protocol: "socks", ListenIP: "127.0.0.1", ServerPort: freeTestPort(t),
		CustomRouteRules: []model.CustomRouteRule{{Name: "allow-loopback-for-test", Match: model.RouteMatch{IPCIDRs: []string{"127.0.0.1/32"}}, Action: model.RouteAction{Type: "route", Target: "direct"}}},
	}
	s := New(config.KernelConfig{Type: "singbox", LogLevel: "error"})
	if err := s.Start(nc, users, kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	originalBox, originalTracker := s.box, s.connTracker
	im := service.FromContext[adapter.InboundManager](s.ctx)
	inbound, _ := im.Get("socks-in")
	om := service.FromContext[adapter.OutboundManager](s.ctx)
	direct, _ := om.Outbound("direct")
	d, err := proxy.SOCKS5("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(nc.ServerPort)), &proxy.Auth{User: users[0].UUID, Password: users[0].UUID}, proxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) { return d.Dial(network, addr) }}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	request := func() {
		t.Helper()
		r, err := client.Get(target.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != "ok" {
			t.Fatalf("response %q: %v", body, err)
		}
	}
	request()
	// An existing TCP session must survive outbound-only updates.
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = echo.Close() })
	go func() {
		c, err := echo.Accept()
		if err == nil {
			defer c.Close()
			_, _ = io.Copy(c, c)
		}
	}()
	persistent, err := d.Dial("tcp", echo.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer persistent.Close()
	alive := func() {
		t.Helper()
		_ = persistent.SetDeadline(time.Now().Add(time.Second))
		if _, err := persistent.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 4)
		if _, err := io.ReadFull(persistent, b); err != nil || string(b) != "ping" {
			t.Fatalf("existing connection interrupted: %q %v", b, err)
		}
	}
	alive()
	withProxy := func(server *httptest.Server) *model.NodeSpec {
		next := *nc
		addr := server.Listener.Addr().(*net.TCPAddr)
		next.CustomOutbounds = []model.OutboundConfig{{Tag: "warp", Protocol: "http", Settings: map[string]any{"server": "127.0.0.1", "server_port": addr.Port}}}
		next.CustomRouteRules = []model.CustomRouteRule{{Name: "test-warp", Match: model.RouteMatch{IPCIDRs: []string{"127.0.0.1/32"}}, Action: model.RouteAction{Type: "route", Target: "warp"}}}
		return &next
	}
	reload := func(next *model.NodeSpec) {
		t.Helper()
		if err := s.Reload(next, users, kernel.TLSCert{}); err != nil {
			t.Fatal(err)
		}
		for _, outbound := range next.CustomOutbounds {
			if _, exists := om.Outbound(outbound.Tag); !exists {
				t.Fatalf("new outbound %s not registered", outbound.Tag)
			}
		}
		gotInbound, _ := im.Get("socks-in")
		gotDirect, _ := om.Outbound("direct")
		if s.box != originalBox || s.connTracker != originalTracker || gotInbound != inbound || gotDirect != direct {
			t.Fatal("outbound update replaced unrelated runtime state")
		}
		alive()
	}
	reload(withProxy(p1))
	request()
	if count1.Load() != 1 {
		t.Fatalf("new outbound not used: count=%d", count1.Load())
	}
	first, _ := om.Outbound("warp")
	reload(withProxy(p1))
	unchanged, _ := om.Outbound("warp")
	if unchanged != first {
		t.Fatal("identical outbound was recreated")
	}
	reload(withProxy(p2))
	request()
	if count2.Load() != 1 || count1.Load() != 1 {
		t.Fatalf("changed outbound not used: %d %d", count1.Load(), count2.Load())
	}
	reload(nc)
	request()
	if _, exists := om.Outbound("warp"); exists {
		t.Fatal("deleted outbound still registered")
	}
	if count2.Load() != 1 {
		t.Fatal("removed route still uses proxy")
	}

	// A dependent detour caches its dialer, so it must also be refreshed
	// when its parent changes. List it first to exercise dependency ordering.
	withChain := func(parent *httptest.Server) *model.NodeSpec {
		next := withProxy(parent)
		child := model.OutboundConfig{Tag: "chain", Protocol: "http", ProxyTag: "warp", Settings: map[string]any{"server": "127.0.0.1", "server_port": p2.Listener.Addr().(*net.TCPAddr).Port}}
		next.CustomOutbounds = append([]model.OutboundConfig{child}, next.CustomOutbounds...)
		next.CustomRouteRules[0].Action.Target = "chain"
		return next
	}
	reload(withChain(p1))
	request()
	before1, before2 := count1.Load(), count2.Load()
	reload(withChain(p2))
	request()
	if count1.Load() != before1 || count2.Load() != before2+2 {
		t.Fatal("dependent outbound kept its old dialer")
	}
	reload(nc)
	request()
	if _, exists := om.Outbound("chain"); exists {
		t.Fatal("dependent outbound not removed")
	}
}

func TestOrderOutboundsDependencies(t *testing.T) {
	chain := option.Outbound{Type: "socks", Tag: "chain", Options: &option.SOCKSOutboundOptions{DialerOptions: option.DialerOptions{Detour: "warp"}}}
	warp := option.Outbound{Type: "direct", Tag: "warp", Options: &option.DirectOutboundOptions{}}
	group := option.Outbound{Type: "selector", Tag: "group", Options: &option.SelectorOutboundOptions{Outbounds: []string{"chain"}}}
	ordered, err := orderOutbounds([]option.Outbound{group, chain, warp})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"warp", "chain", "group"} {
		if ordered[i].Tag != want {
			t.Fatalf("order[%d]=%s", i, ordered[i].Tag)
		}
	}
	cycle := option.Outbound{Type: "direct", Tag: "warp", Options: &option.DirectOutboundOptions{DialerOptions: option.DialerOptions{Detour: "chain"}}}
	if _, err := orderOutbounds([]option.Outbound{chain, cycle}); err == nil {
		t.Fatal("cycle accepted")
	}
}
