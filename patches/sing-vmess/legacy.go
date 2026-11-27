package vmess

import (
	"crypto/hmac"
	"crypto/md5"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"
)

// legacyAuthWindow is the accepted clock offset (seconds) for legacy MD5 AuthIDs.
// Legacy clients randomize their timestamp by up to ±30 s; the rest covers drift.
const legacyAuthWindow = 60

type legacyUser[U comparable] struct {
	user   U
	cmdKey [16]byte
	ids    [][16]byte // primary UUID followed by derived AlterIDs
}

type legacyBucket[U comparable] struct {
	ts      int64
	entries map[[16]byte]*legacyUser[U]
}

type legacyTable[U comparable] struct {
	users   []*legacyUser[U]
	buckets []*legacyBucket[U]
}

// legacyAuth keeps a sliding per-second table of legacy MD5 AuthIDs for every user,
// refreshed once per second so lookups are O(window) map reads with no hashing.
type legacyAuth[U comparable] struct {
	table   atomic.Pointer[legacyTable[U]]
	mu      sync.Mutex
	users   []*legacyUser[U]
	started bool
	stop    chan struct{}
}

func buildLegacyBucket[U comparable](users []*legacyUser[U], ts int64) *legacyBucket[U] {
	b := &legacyBucket[U]{ts: ts, entries: make(map[[16]byte]*legacyUser[U], len(users))}
	var stamp [8]byte
	binary.BigEndian.PutUint64(stamp[:], uint64(ts))
	var tag [16]byte
	for _, u := range users {
		for _, id := range u.ids {
			mac := hmac.New(md5.New, id[:])
			mac.Write(stamp[:])
			mac.Sum(tag[:0])
			b.entries[tag] = u
		}
	}
	return b
}

func (l *legacyAuth[U]) setUsers(users []*legacyUser[U], now int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.users = users
	l.refreshLocked(now, true)
}

func (l *legacyAuth[U]) refresh(now int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refreshLocked(now, false)
}

func (l *legacyAuth[U]) refreshLocked(now int64, rebuild bool) {
	reuse := map[int64]*legacyBucket[U]{}
	if old := l.table.Load(); old != nil && !rebuild {
		for _, b := range old.buckets {
			reuse[b.ts] = b
		}
	}
	buckets := make([]*legacyBucket[U], 0, 2*legacyAuthWindow+1)
	for ts := now - legacyAuthWindow; ts <= now+legacyAuthWindow; ts++ {
		if b, ok := reuse[ts]; ok {
			buckets = append(buckets, b)
		} else {
			buckets = append(buckets, buildLegacyBucket(l.users, ts))
		}
	}
	l.table.Store(&legacyTable[U]{users: l.users, buckets: buckets})
}

func (l *legacyAuth[U]) lookup(authID [16]byte) (*legacyUser[U], int64, bool) {
	t := l.table.Load()
	if t == nil {
		return nil, 0, false
	}
	for _, b := range t.buckets {
		if u, ok := b.entries[authID]; ok {
			return u, b.ts, true
		}
	}
	return nil, 0, false
}

func (l *legacyAuth[U]) start(now func() time.Time) {
	l.mu.Lock()
	if l.started {
		l.mu.Unlock()
		return
	}
	l.started = true
	l.stop = make(chan struct{})
	stop := l.stop
	l.mu.Unlock()
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				l.refresh(now().Unix())
			}
		}
	}()
}

func (l *legacyAuth[U]) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.started {
		close(l.stop)
		l.started = false
	}
}
