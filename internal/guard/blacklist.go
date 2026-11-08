package guard

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cedar2025/xboard-node/internal/nlog"
)

// Blacklist is a file-backed IP set. One IP per line, "#" starts a comment.
// The file is re-read when modified externally, so deleting a line unbans.
type Blacklist struct {
	path string

	mu    sync.RWMutex
	ips   map[string]struct{}
	allow map[string]struct{}

	fmu     sync.Mutex // serialises file access
	modTime time.Time
	size    int64
}

var (
	registryMu sync.Mutex
	registry   = map[string]*Blacklist{}
)

// SharedBlacklist returns the process-wide blacklist for path so that all
// nodes (multi-node / machine mode) share one file and one watcher.
// allow is merged into the existing allow set.
func SharedBlacklist(path, allow string) *Blacklist {
	registryMu.Lock()
	defer registryMu.Unlock()
	if b, ok := registry[path]; ok {
		b.addAllow(allow)
		return b
	}
	b := newBlacklist(path, allow)
	b.reload(true)
	go b.watch()
	registry[path] = b
	return b
}

func newBlacklist(path, allow string) *Blacklist {
	b := &Blacklist{
		path:  path,
		ips:   map[string]struct{}{},
		allow: map[string]struct{}{},
	}
	b.addAllow(allow)
	return b
}

func (b *Blacklist) addAllow(allow string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ip := range strings.Split(allow, ",") {
		if ip = strings.TrimSpace(ip); ip != "" {
			b.allow[normalizeIP(ip)] = struct{}{}
		}
	}
}

// Has reports whether ip is banned (allow-listed IPs never are).
func (b *Blacklist) Has(ip string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if _, ok := b.allow[ip]; ok {
		return false
	}
	_, ok := b.ips[ip]
	return ok
}

// Add bans ip and appends it to the file. Returns true if it was newly banned.
func (b *Blacklist) Add(ip, reason string) bool {
	if ip == "" {
		return false
	}
	b.mu.Lock()
	if _, ok := b.allow[ip]; ok {
		b.mu.Unlock()
		return false
	}
	if _, ok := b.ips[ip]; ok {
		b.mu.Unlock()
		return false
	}
	b.ips[ip] = struct{}{}
	b.mu.Unlock()

	b.fmu.Lock()
	defer b.fmu.Unlock()
	if dir := filepath.Dir(b.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	f, err := os.OpenFile(b.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		nlog.Core().Warn("guard: write blacklist failed", "error", err)
		return true
	}
	// Strip newlines so a crafted path cannot inject extra entries.
	reason = strings.NewReplacer("\r", " ", "\n", " ").Replace(reason)
	fmt.Fprintf(f, "%s # %s %s\n", ip, time.Now().Format("2006-01-02 15:04:05"), reason)
	f.Close()
	if st, err := os.Stat(b.path); err == nil {
		b.modTime, b.size = st.ModTime(), st.Size() // own writes don't trigger reload
	}
	return true
}

// Len returns the number of banned IPs.
func (b *Blacklist) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.ips)
}

func (b *Blacklist) reload(force bool) {
	b.fmu.Lock()
	defer b.fmu.Unlock()
	st, err := os.Stat(b.path)
	if err != nil {
		if os.IsNotExist(err) {
			b.mu.Lock()
			cleared := len(b.ips) > 0
			b.ips = map[string]struct{}{}
			b.mu.Unlock()
			b.modTime, b.size = time.Time{}, 0
			if cleared {
				nlog.Core().Info("guard: blacklist file removed, cleared", "file", b.path)
			}
		}
		return
	}
	if !force && st.ModTime().Equal(b.modTime) && st.Size() == b.size {
		return
	}
	f, err := os.Open(b.path)
	if err != nil {
		nlog.Core().Warn("guard: read blacklist failed", "error", err)
		return
	}
	defer f.Close()
	next := map[string]struct{}{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "#")
		if fields := strings.Fields(line); len(fields) > 0 {
			next[normalizeIP(fields[0])] = struct{}{}
		}
	}
	b.mu.Lock()
	b.ips = next
	b.mu.Unlock()
	b.modTime, b.size = st.ModTime(), st.Size()
	nlog.Core().Info("guard: blacklist loaded", "file", b.path, "ips", len(next))
}

func (b *Blacklist) watch() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for range t.C {
		b.reload(false)
	}
}

func normalizeIP(s string) string {
	s = strings.TrimSpace(s)
	ip := net.ParseIP(s)
	if ip == nil {
		return s
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String()
}

func remoteIP(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		host = addr.String()
	}
	return normalizeIP(host)
}
