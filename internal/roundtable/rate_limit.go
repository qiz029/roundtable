package roundtable

import (
	"net"
	"net/http"
	"strings"
	"sync"
)

type RateLimitConfig struct {
	AuthPerMinute  int
	AgentPerSecond int
	VotePerMinute  int
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
	if config.AgentPerSecond == 0 {
		config.AgentPerSecond = 2
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
		if r.URL.Path == "/api/v1/agent/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1/agent/") {
			if token, ok := bearerToken(r.Header.Get("Authorization")); ok {
				key := "agent-api-key:" + hashSecret(token)
				if !a.limiter.allow(key, a.now().UTC().Unix(), a.limiter.config.AgentPerSecond) {
					writeError(w, errAgentRateLimited(a.limiter.config.AgentPerSecond))
					return
				}
			}
			next.ServeHTTP(w, r)
			return
		}

		limit, ok := a.limitFor(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		if !a.limiter.allow(a.limitKey(r), a.now().UTC().Unix()/60, limit) {
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

func (l *rateLimiter) allow(key string, window int64, limit int) bool {
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
