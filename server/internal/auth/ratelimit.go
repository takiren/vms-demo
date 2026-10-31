package auth

import (
	"sync"
	"time"
)

// Limiter counts failures per key (a username or a client IP) in a fixed
// window that starts at the first failure. When a key reaches max failures
// it is blocked until the window ends.
//
// State is in memory: the design is a single server, and a restart resets
// the counts. The number of keys is capped; when the cap is reached even
// after dropping expired keys, keys that are not tracked yet are blocked
// (fail closed) rather than letting an attacker evict real counters.
type Limiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	maxKeys int
	now     func() time.Time
	entries map[string]*limitEntry
}

type limitEntry struct {
	failures int
	start    time.Time
}

// NewLimiter returns a Limiter. now may be nil (time.Now).
func NewLimiter(maxFailures int, window time.Duration, maxKeys int, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{max: maxFailures, window: window, maxKeys: maxKeys, now: now, entries: make(map[string]*limitEntry)}
}

// RetryAfter returns how long key is blocked, or 0 if it is allowed.
func (l *Limiter) RetryAfter(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e, ok := l.entries[key]
	if ok && !now.Before(e.start.Add(l.window)) {
		delete(l.entries, key)
		ok = false
	}
	if !ok {
		if len(l.entries) >= l.maxKeys {
			l.sweep(now)
			if len(l.entries) >= l.maxKeys {
				return l.window
			}
		}
		return 0
	}
	if e.failures >= l.max {
		return e.start.Add(l.window).Sub(now)
	}
	return 0
}

// Fail records a failure for key.
func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e, ok := l.entries[key]
	if ok && !now.Before(e.start.Add(l.window)) {
		ok = false
	}
	if !ok {
		if _, exists := l.entries[key]; !exists && len(l.entries) >= l.maxKeys {
			l.sweep(now)
			if len(l.entries) >= l.maxKeys {
				return
			}
		}
		e = &limitEntry{start: now}
		l.entries[key] = e
	}
	e.failures++
}

// Reset forgets key, for example after a successful login.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// sweep drops expired entries. Callers hold l.mu.
func (l *Limiter) sweep(now time.Time) {
	for k, e := range l.entries {
		if !now.Before(e.start.Add(l.window)) {
			delete(l.entries, k)
		}
	}
}
