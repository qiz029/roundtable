package roundtable

import (
	"net/http"
)

const (
	corsAllowMethods = "GET, POST, PATCH, DELETE, OPTIONS"
	corsAllowHeaders = "Authorization, Content-Type, X-Request-Id"
)

func allowCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}

		header := w.Header()
		header.Set("Access-Control-Allow-Origin", origin)
		header.Set("Access-Control-Allow-Credentials", "true")
		header.Set("Access-Control-Allow-Methods", corsAllowMethods)
		header.Set("Access-Control-Allow-Headers", requestedHeaders(r))
		header.Set("Access-Control-Expose-Headers", requestIDHeader)
		header.Add("Vary", "Origin")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func requestedHeaders(r *http.Request) string {
	headers := r.Header.Get("Access-Control-Request-Headers")
	if headers == "" {
		return corsAllowHeaders
	}
	return headers
}
