package guard

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cedar2025/xboard-node/internal/nlog"
)

const (
	headerTimeout  = 10 * time.Second
	dialTimeout    = 5 * time.Second
	maxHeaderBytes = 16 << 10
)

// honeypotBody mimics a default nginx install (same page as ws-tunnel-pro).
const honeypotBody = `<!DOCTYPE html><html><head><title>Welcome to nginx!</title><style>body{width:35em;margin:0 auto;font-family:Tahoma,Verdana,Arial,sans-serif}</style></head><body><h1>Welcome to nginx!</h1><p>If you see this page, the nginx web server is successfully installed and working. Further configuration is required.</p><p>For online documentation and support please refer to <a href="http://nginx.org/">nginx.org</a>.</p><p><em>Thank you for using nginx.</em></p></body></html>`

// Options configures one guarded public port.
type Options struct {
	Listen  string // public address, e.g. ":2086"
	Backend string // kernel loopback address, e.g. "127.0.0.1:40123"
	Path    string // exact ws path, e.g. "/mm"
	// PathPrefix also accepts Path followed by extra characters (sing-box
	// clients put early data in the path when no header name is set).
	PathPrefix   bool
	RealIPHeader string
	NoBan        bool
	Tag          string // log prefix, e.g. "vless:2086"
}

// Proxy owns the public port, filters requests and splices valid ones to the
// kernel. The X-Forwarded-For header is overwritten with the verified client
// IP so the kernel's per-user IP tracking and device limits keep working.
type Proxy struct {
	opts Options
	bl   *Blacklist
	ln   net.Listener

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// Listen binds the public port and starts serving.
func Listen(opts Options, bl *Blacklist) (*Proxy, error) {
	ln, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return nil, err
	}
	p := &Proxy{opts: opts, bl: bl, ln: ln, conns: map[net.Conn]struct{}{}}
	p.wg.Add(1)
	go p.serve()
	return p, nil
}

// Addr returns the bound public address.
func (p *Proxy) Addr() net.Addr { return p.ln.Addr() }

// Close stops listening and drops all active connections.
func (p *Proxy) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.ln.Close()
	for c := range p.conns {
		c.Close()
	}
	p.mu.Unlock()
	p.wg.Wait()
}

func (p *Proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *Proxy) untrack(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
}

func (p *Proxy) serve() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			nlog.Core().Warn("guard: accept failed", "tag", p.opts.Tag, "error", err)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		// Without a real-IP header the TCP peer is the client: drop banned
		// IPs before reading anything.
		if p.opts.RealIPHeader == "" && p.bl.Has(remoteIP(c.RemoteAddr())) {
			c.Close()
			continue
		}
		if !p.track(c) {
			c.Close()
			return
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer p.untrack(c)
			defer c.Close()
			p.handle(c)
		}()
	}
}

func (p *Proxy) handle(c net.Conn) {
	peer := remoteIP(c.RemoteAddr())
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(30 * time.Second)
	}

	c.SetReadDeadline(time.Now().Add(headerTimeout))
	lr := &io.LimitedReader{R: c, N: maxHeaderBytes}
	br := bufio.NewReaderSize(lr, 4096)
	req, err := http.ReadRequest(br)
	if err != nil {
		// Silent connects / timeouts are not banned; garbage (TLS hello,
		// raw protocol probes) on a ws port is.
		if !isQuietErr(err) {
			p.ban(peer, "non-HTTP data")
		}
		return
	}
	c.SetReadDeadline(time.Time{})

	clientIP := peer
	if h := p.opts.RealIPHeader; h != "" {
		if v := firstIP(req.Header.Get(h)); v != "" {
			clientIP = v
		}
		if p.bl.Has(clientIP) {
			writeHoneypot(c, req)
			return
		}
	}

	path := req.URL.Path
	if !p.pathOK(path) {
		nlog.Core().Info("guard: probe", "tag", p.opts.Tag, "ip", clientIP, "method", req.Method, "path", truncate(req.URL.RequestURI(), 128))
		p.ban(clientIP, req.Method+" "+truncate(req.URL.RequestURI(), 128))
		writeHoneypot(c, req)
		return
	}
	// Correct path but not a ws upgrade: honeypot, no ban (same as ws-tunnel-pro).
	if !isWebSocketUpgrade(req) {
		writeHoneypot(c, req)
		return
	}

	backend, err := net.DialTimeout("tcp", p.opts.Backend, dialTimeout)
	if err != nil {
		nlog.Core().Warn("guard: dial kernel failed", "tag", p.opts.Tag, "backend", p.opts.Backend, "error", err)
		return
	}
	if !p.track(backend) {
		backend.Close()
		return
	}
	defer p.untrack(backend)
	defer backend.Close()

	// The kernel takes the first X-Forwarded-For entry as the source IP;
	// overwrite it so clients cannot spoof it.
	req.Header.Del("X-Forwarded-For")
	req.Header.Set("X-Forwarded-For", clientIP)
	req.Header.Del(p.opts.RealIPHeader)
	if err := req.Write(backend); err != nil {
		return
	}

	// Lift the header size limit for the tunnelled stream; bytes already
	// buffered in br (early ws frames) are forwarded first.
	lr.N = 1<<63 - 1
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(backend, br)
		if tc, ok := backend.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		io.Copy(c, backend)
		if tc, ok := c.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	// Give the other direction a moment to flush, then tear down.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

func (p *Proxy) pathOK(path string) bool {
	if path == p.opts.Path {
		return true
	}
	return p.opts.PathPrefix && strings.HasPrefix(path, p.opts.Path)
}

func (p *Proxy) ban(ip, reason string) {
	if p.opts.NoBan || ip == "" {
		return
	}
	if p.bl.Add(ip, p.opts.Tag+" "+reason) {
		nlog.Core().Warn("guard: banned", "tag", p.opts.Tag, "ip", ip, "reason", reason)
	}
}

func isWebSocketUpgrade(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		headerHasToken(r.Header, "Connection", "upgrade") &&
		headerHasToken(r.Header, "Upgrade", "websocket")
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

func writeHoneypot(c net.Conn, req *http.Request) {
	c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{},
		ContentLength: int64(len(honeypotBody)),
		Close:         true,
		Request:       req,
	}
	resp.Header.Set("Server", "nginx/1.18.0")
	resp.Header.Set("Content-Type", "text/html; charset=utf-8")
	resp.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	if req.Method != http.MethodHead {
		resp.Body = io.NopCloser(strings.NewReader(honeypotBody))
	}
	resp.Write(c)
}

func isQuietErr(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, net.ErrClosed) {
		return true
	}
	// Socket-level errors (reset etc.) are not proof of probing.
	var oe *net.OpError
	return errors.As(err, &oe)
}

func firstIP(v string) string {
	first, _, _ := strings.Cut(v, ",")
	ip := net.ParseIP(strings.TrimSpace(first))
	if ip == nil {
		return ""
	}
	return normalizeIP(ip.String())
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
