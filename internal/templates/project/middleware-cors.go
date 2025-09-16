//go:build ignore

package middleware

import (
	"net/http"
	"strings"
)

// CORSMiddleware adds CORS headers for frontend development.
func CORSMiddleware(allowOrigins []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Vary: Origin is required for correct caching when responses differ by origin.
			w.Header().Set("Vary", "Origin")

			origin := r.Header.Get("Origin")
			matched := false
			for _, allowed := range allowOrigins {
				if !strings.EqualFold(origin, allowed) && allowed != "*" {
					continue
				}
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Access-Control-Expose-Headers", "Connect-Protocol-Version")
				matched = true
				break
			}
			if matched && r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Connect-Protocol-Version, Connect-Timeout-Ms, Authorization")
				w.Header().Set("Access-Control-Max-Age", "86400")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
