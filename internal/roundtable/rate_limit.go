package roundtable

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type RateLimitConfig struct {
	AuthPerMinute      int
	AgentReadPerMinute int
	VotePerMinute      int
}

type rateLimiter struct {
	mu      sync.Mutex
	config  RateLimitConfig
	buckets map[string]rateBucket
}

type rateBucket struct {
	Window int64
	Count  int
}

func newRateLimiter(config RateLimitConfig) *rateLimiter {
	if config.AuthPerMinute == 0 {
		config.AuthPerMinute = 20
	}
	if config.AgentReadPerMinute == 0 {
		config.AgentReadPerMinute = 120
	}
	if config.VotePerMinute == 0 {
		config.VotePerMinute = 120
	}
	return &rateLimiter{
		config:  config,
		buckets: map[string]rateBucket{},
	}
}

func (a *App) limitRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit, ok := a.limitFor(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		if !a.limiter.allow(a.limitKey(r), a.now().UTC(), limit) {
			writeError(w, errRateLimited())
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) limitFor(r *http.Request) (int, bool) {
	if r.Method == http.MethodPost && (r.URL.Path == "/api/v1/auth/register" || r.URL.Path == "/api/v1/auth/login") {
		return a.limiter.config.AuthPerMinute, true
	}
	if r.Method == http.MethodGet && r.URL.Path == "/api/v1/agent/invitations" {
		return a.limiter.config.AgentReadPerMinute, true
	}
	if strings.HasSuffix(r.URL.Path, "/like") && (r.Method == http.MethodPost || r.Method == http.MethodDelete) {
		return a.limiter.config.VotePerMinute, true
	}
	return 0, false
}

func (a *App) limitKey(r *http.Request) string {
	identity := clientIP(r)
	if token, ok := bearerToken(r.Header.Get("Authorization")); ok {
		identity = "bearer:" + hashSecret(token)
	} else if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" {
		identity = "cookie:" + hashSecret(cookie.Value)
	}
	return r.Method + ":" + r.URL.Path + ":" + identity
}

func (l *rateLimiter) allow(key string, now time.Time, limit int) bool {
	window := now.Unix() / 60

	l.mu.Lock()
	defer l.mu.Unlock()

	bucket := l.buckets[key]
	if bucket.Window != window {
		bucket = rateBucket{Window: window}
	}
	if bucket.Count >= limit {
		l.buckets[key] = bucket
		return false
	}
	bucket.Count++
	l.buckets[key] = bucket
	return true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}
