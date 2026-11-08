package guard

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeKernel accepts one connection at a time, records the HTTP request the
// guard forwarded, answers 101 and then echoes the tunnelled bytes.
type fakeKernel struct {
	ln   net.Listener
	reqs chan *http.Request
}

func newFakeKernel(t *testing.T) *fakeKernel {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	k := &fakeKernel{ln: ln, reqs: make(chan *http.Request, 8)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				k.reqs <- req
				io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
				io.Copy(c, br)
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return k
}

func startProxy(t *testing.T, opts Options) (*Proxy, *Blacklist, *fakeKernel) {
	t.Helper()
	k := newFakeKernel(t)
	bl := newBlacklist(filepath.Join(t.TempDir(), "bl.txt"), "")
	opts.Listen = "127.0.0.1:0"
	opts.Backend = k.ln.Addr().String()
	if opts.Tag == "" {
		opts.Tag = "vless:test"
	}
	p, err := Listen(opts, bl)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p, bl, k
}

func dial(t *testing.T, p *Proxy) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", p.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	t.Cleanup(func() { c.Close() })
	return c
}

func wsRequest(path, extra string) string {
	return "GET " + path + " HTTP/1.1\r\nHost: xg2.apple9.org\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n" + extra + "\r\n"
}

func readResponse(t *testing.T, c net.Conn) (*http.Response, string) {
	t.Helper()
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func TestWrongPathBansAndServesHoneypot(t *testing.T) {
	p, bl, _ := startProxy(t, Options{Path: "/mm"})
	c := dial(t, p)
	io.WriteString(c, "GET /admin HTTP/1.1\r\nHost: x\r\n\r\n")
	resp, body := readResponse(t, c)
	if resp.StatusCode != 200 || resp.Header.Get("Server") != "nginx/1.18.0" || !strings.Contains(body, "Welcome to nginx!") {
		t.Fatalf("expected honeypot, got %d %q", resp.StatusCode, body)
	}
	if !bl.Has("127.0.0.1") {
		t.Fatal("IP not banned")
	}
	data, _ := os.ReadFile(bl.path)
	if !strings.HasPrefix(string(data), "127.0.0.1 # ") || !strings.Contains(string(data), "GET /admin") {
		t.Fatalf("blacklist file = %q", data)
	}

	// Banned peer is dropped on connect, even with the right path.
	c2 := dial(t, p)
	io.WriteString(c2, wsRequest("/mm", ""))
	if n, err := c2.Read(make([]byte, 1)); err == nil {
		t.Fatalf("banned IP got %d bytes", n)
	}
}

func TestCorrectPathTunnelsAndRewritesXFF(t *testing.T) {
	p, bl, k := startProxy(t, Options{Path: "/mm"})
	c := dial(t, p)
	// Query string (xray early data "?ed=2048") must still match.
	io.WriteString(c, wsRequest("/mm?ed=2048", "X-Forwarded-For: 9.9.9.9\r\n"))
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	req := <-k.reqs
	if got := req.Header.Values("X-Forwarded-For"); len(got) != 1 || got[0] != "127.0.0.1" {
		t.Fatalf("X-Forwarded-For = %v, want [127.0.0.1]", got)
	}
	if req.Host != "xg2.apple9.org" || req.URL.RequestURI() != "/mm?ed=2048" {
		t.Fatalf("forwarded host=%q uri=%q", req.Host, req.URL.RequestURI())
	}

	io.WriteString(c, "ping-payload")
	buf := make([]byte, len("ping-payload"))
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "ping-payload" {
		t.Fatalf("echo = %q err=%v", buf, err)
	}
	if bl.Has("127.0.0.1") {
		t.Fatal("valid client banned")
	}
}

func TestCorrectPathWithoutUpgradeNotBanned(t *testing.T) {
	p, bl, _ := startProxy(t, Options{Path: "/mm"})
	c := dial(t, p)
	io.WriteString(c, "GET /mm HTTP/1.1\r\nHost: x\r\n\r\n")
	resp, _ := readResponse(t, c)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if bl.Has("127.0.0.1") {
		t.Fatal("banned on correct path")
	}
}

func TestPathPrefixAndSubpath(t *testing.T) {
	p, bl, _ := startProxy(t, Options{Path: "/mm"})
	c := dial(t, p)
	io.WriteString(c, wsRequest("/mm/x", ""))
	readResponse(t, c)
	if !bl.Has("127.0.0.1") {
		t.Fatal("/mm/x should be banned in exact mode")
	}

	p2, bl2, _ := startProxy(t, Options{Path: "/mm", PathPrefix: true})
	c2 := dial(t, p2)
	io.WriteString(c2, wsRequest("/mmAAAAearlydata", ""))
	resp, err := http.ReadResponse(bufio.NewReader(c2), nil)
	if err != nil || resp.StatusCode != 101 || bl2.Has("127.0.0.1") {
		t.Fatalf("prefix mode: resp=%v err=%v", resp, err)
	}
}

func TestGarbageBannedSilentNotBanned(t *testing.T) {
	p, bl, _ := startProxy(t, Options{Path: "/mm"})

	c := dial(t, p)
	c.Close() // connect and leave
	time.Sleep(100 * time.Millisecond)
	if bl.Has("127.0.0.1") {
		t.Fatal("silent connect banned")
	}

	c2 := dial(t, p)
	c2.Write([]byte{0x16, 0x03, 0x01, 0x00, 0xa5, 0x01, 0x00, 0x00, 0xa1, '\r', '\n', '\r', '\n'}) // TLS ClientHello-ish
	c2.Read(make([]byte, 1))
	deadline := time.Now().Add(2 * time.Second)
	for !bl.Has("127.0.0.1") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !bl.Has("127.0.0.1") {
		t.Fatal("garbage sender not banned")
	}
}

func TestRealIPHeader(t *testing.T) {
	p, bl, k := startProxy(t, Options{Path: "/mm", RealIPHeader: "CF-Connecting-IP"})

	c := dial(t, p)
	io.WriteString(c, "GET /wp-login.php HTTP/1.1\r\nHost: x\r\nCF-Connecting-IP: 203.0.113.7\r\n\r\n")
	readResponse(t, c)
	if !bl.Has("203.0.113.7") || bl.Has("127.0.0.1") {
		t.Fatal("should ban header IP, not the CDN peer")
	}

	// Banned header IP → honeypot even on the right path; CDN peer still accepted.
	c2 := dial(t, p)
	io.WriteString(c2, wsRequest("/mm", "CF-Connecting-IP: 203.0.113.7\r\n"))
	if resp, _ := readResponse(t, c2); resp.StatusCode != 200 {
		t.Fatalf("banned header IP got %d", resp.StatusCode)
	}

	c3 := dial(t, p)
	io.WriteString(c3, wsRequest("/mm", "CF-Connecting-IP: 198.51.100.1\r\n"))
	if resp, err := http.ReadResponse(bufio.NewReader(c3), nil); err != nil || resp.StatusCode != 101 {
		t.Fatalf("good header IP: %v %v", resp, err)
	}
	req := <-k.reqs
	if req.Header.Get("X-Forwarded-For") != "198.51.100.1" || req.Header.Get("CF-Connecting-IP") != "" {
		t.Fatalf("headers = %v", req.Header)
	}
}

func TestAllowListAndNoBan(t *testing.T) {
	p, bl, _ := startProxy(t, Options{Path: "/mm"})
	bl.addAllow("127.0.0.1")
	c := dial(t, p)
	io.WriteString(c, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	readResponse(t, c)
	if bl.Has("127.0.0.1") || bl.Len() != 0 {
		t.Fatal("allow-listed IP banned")
	}

	p2, bl2, _ := startProxy(t, Options{Path: "/mm", NoBan: true})
	c2 := dial(t, p2)
	io.WriteString(c2, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	if resp, _ := readResponse(t, c2); resp.StatusCode != 200 || bl2.Len() != 0 {
		t.Fatal("no_ban should only serve honeypot")
	}
}

func TestBlacklistReloadUnban(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bl.txt")
	os.WriteFile(path, []byte("# comment\n1.2.3.4 # old\n::ffff:5.6.7.8\n"), 0o644)
	bl := newBlacklist(path, "")
	bl.reload(true)
	if !bl.Has("1.2.3.4") || !bl.Has("5.6.7.8") {
		t.Fatal("load failed")
	}
	bl.Add("9.9.9.9", "test")
	bl.reload(false) // own write must not drop entries
	if !bl.Has("9.9.9.9") {
		t.Fatal("own write lost")
	}

	time.Sleep(20 * time.Millisecond)
	os.WriteFile(path, []byte("5.6.7.8\n"), 0o644) // operator deletes lines
	bl.reload(false)
	if bl.Has("1.2.3.4") || bl.Has("9.9.9.9") || !bl.Has("5.6.7.8") {
		t.Fatal("manual unban not applied")
	}
	os.Remove(path)
	bl.reload(false)
	if bl.Len() != 0 {
		t.Fatal("removed file should clear list")
	}
}

func TestReasonCannotInjectLines(t *testing.T) {
	bl := newBlacklist(filepath.Join(t.TempDir(), "bl.txt"), "")
	bl.Add("1.1.1.1", "GET /x\n2.2.2.2")
	bl.reload(true)
	if bl.Has("2.2.2.2") {
		t.Fatal("newline in reason injected an entry")
	}
}
