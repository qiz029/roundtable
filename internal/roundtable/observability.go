package roundtable

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const requestIDHeader = "X-Request-Id"

type requestStateContextKey struct{}

type requestState struct {
	RequestID string
	UserID    string
	AgentID   string
}

func (a *App) observeRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := requestIDFromHeader(r.Header.Get(requestIDHeader))
		if requestID == "" {
			requestID = newRequestID()
		}

		state := &requestState{RequestID: requestID}
		ctx := context.WithValue(r.Context(), requestStateContextKey{}, state)
		r = r.WithContext(ctx)

		w.Header().Set(requestIDHeader, requestID)
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)

		if a.logger == nil {
			return
		}
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}

		level := slog.LevelInfo
		if status >= 500 {
			level = slog.LevelError
		} else if status >= 400 {
			level = slog.LevelWarn
		}

		attrs := []slog.Attr{
			slog.String("request_id", requestID),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", status),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			slog.Int("bytes", recorder.bytes),
			slog.String("remote_addr", clientIP(r)),
		}
		if userAgent := r.UserAgent(); userAgent != "" {
			attrs = append(attrs, slog.String("user_agent", userAgent))
		}
		if cfRay := strings.TrimSpace(r.Header.Get("CF-Ray")); cfRay != "" {
			attrs = append(attrs, slog.String("cf_ray", cfRay))
		}
		if state.UserID != "" {
			attrs = append(attrs, slog.String("user_id", state.UserID))
		}
		if state.AgentID != "" {
			attrs = append(attrs, slog.String("agent_id", state.AgentID))
		}
		a.logger.LogAttrs(r.Context(), level, "http_request", attrs...)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status != 0 {
		return
	}
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(body []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(body)
	r.bytes += n
	return n, err
}

func requestIDFromHeader(value string) string {
	value = strings.TrimSpace(value)
	if validRequestID(value) {
		return value
	}
	return ""
}

func validRequestID(value string) bool {
	if len(value) < 8 || len(value) > 128 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '_' || c == '-' || c == '.' || c == ':' {
			continue
		}
		return false
	}
	return true
}

func newRequestID() string {
	id, err := newID("rt_req")
	if err == nil {
		return id
	}
	return fmt.Sprintf("rt_req_%d", time.Now().UnixNano())
}

func markRequestUser(ctx context.Context, userID string) {
	if state := requestStateFromContext(ctx); state != nil {
		state.UserID = userID
	}
}

func markRequestAgent(ctx context.Context, agent currentAgent) {
	if state := requestStateFromContext(ctx); state != nil {
		state.AgentID = agent.ID
		if state.UserID == "" {
			state.UserID = agent.OwnerID
		}
	}
}

func requestStateFromContext(ctx context.Context) *requestState {
	state, _ := ctx.Value(requestStateContextKey{}).(*requestState)
	return state
}
