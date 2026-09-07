package meetingsecurity

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	ErrHostNotAllowed   = errors.New("request host is not allowed")
	ErrOriginNotAllowed = errors.New("request origin is not allowed")
	ErrTLSRequired      = errors.New("TLS is required")
)

type RequestPolicy struct {
	AllowedHosts   []string
	AllowedOrigins []string
	RequireTLS     bool
}

func (p RequestPolicy) Validate(request *http.Request) error {
	if request == nil {
		return ErrHostNotAllowed
	}
	if p.RequireTLS && request.TLS == nil {
		return ErrTLSRequired
	}
	if !containsFold(p.AllowedHosts, hostname(request.Host)) {
		return ErrHostNotAllowed
	}
	origin := strings.TrimSpace(request.Header.Get("Origin"))
	if origin == "" {
		return nil
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || !containsFold(p.AllowedOrigins, parsed.Scheme+"://"+parsed.Host) {
		return ErrOriginNotAllowed
	}
	return nil
}

func hostname(value string) string {
	if host, _, err := net.SplitHostPort(value); err == nil {
		return host
	}
	return strings.Trim(value, "[]")
}

func containsFold(values []string, value string) bool {
	for _, candidate := range values {
		if strings.EqualFold(strings.TrimSpace(candidate), value) {
			return true
		}
	}
	return false
}

type window struct {
	start int64
	count int
}

type FixedWindowLimiter struct {
	mu            sync.Mutex
	burst         int
	windowSeconds int64
	now           func() int64
	entries       map[string]window
}

func NewFixedWindowLimiter(burst int, windowSeconds int64, now func() int64) *FixedWindowLimiter {
	if burst < 1 {
		burst = 1
	}
	if windowSeconds < 1 {
		windowSeconds = 1
	}
	if now == nil {
		now = func() int64 { return time.Now().Unix() }
	}
	return &FixedWindowLimiter{burst: burst, windowSeconds: windowSeconds, now: now, entries: make(map[string]window)}
}

func (l *FixedWindowLimiter) Allow(key string) bool {
	if l == nil {
		return true
	}
	now := l.now()
	start := now - now%l.windowSeconds
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.entries[key]
	if entry.start != start {
		entry = window{start: start}
	}
	if entry.count >= l.burst {
		l.entries[key] = entry
		return false
	}
	entry.count++
	l.entries[key] = entry
	return true
}
