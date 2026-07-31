package middleware

import (
	"net/http"
	"strings"

	"github.com/reliant-labs/forge/pkg/svcerr"
)

// CORSMiddleware returns an HTTP middleware that applies the CORS policy
// described by allowOrigins and allowCredentials.
//
// Semantics (per the Fetch / CORS specs):
//
//   - When the request has no Origin header, no CORS headers are written
//     and the request is passed through. Emitting ACAO with an empty
//     value (or "*") on a same-origin request is a well-known footgun:
//     the browser never needed CORS on it in the first place, and some
//     caches key on the ACAO value.
//
//   - When allowOrigins contains "*" AND allowCredentials is true, this
//     middleware panics. The combination is spec-invalid (browsers
//     reject it) and must be caught at config validation time (the
//     scaffolded config.Validate does). The panic is a belt-and-
//     suspenders guard if validation is bypassed.
//
//   - When allowOrigins contains "*" and credentials are disabled, the
//     request Origin is echoed back rather than a literal "*". Echoing
//     keeps the response compatible with future credentialed callers
//     (no silent breakage) and still satisfies "*" semantically.
//
//   - When specific origins are listed, an exact (case-insensitive)
//     match on the Origin header is required. No match → no CORS
//     headers (the browser will block the response).
//
// Vary: Origin is always set so intermediate caches key on the Origin.
func CORSMiddleware(allowOrigins []string, allowCredentials bool) func(http.Handler) http.Handler {
	// Belt-and-suspenders: the scaffolded config.Validate rejects this
	// combination at startup, but if a caller constructs the middleware
	// directly (e.g. in a test) we must still refuse to produce a
	// spec-invalid policy.
	if allowCredentials && containsWildcard(allowOrigins) {
		panic("middleware: CORS wildcard origin ('*') is incompatible with allowCredentials=true")
	}
	wildcard := containsWildcard(allowOrigins)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				// Same-origin or non-browser client: no CORS headers.
				next.ServeHTTP(w, r)
				return
			}

			// Vary: Origin is required whenever responses differ by origin.
			w.Header().Add("Vary", "Origin")

			allowedOrigin, matched := resolveAllowedOrigin(origin, allowOrigins, wildcard)
			if !matched {
				// Origin not allowed — don't emit CORS headers; the
				// browser will block the response.
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
			if allowCredentials {
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
			// Expose Connect-Protocol-Version AND the forge error-reason
			// header cross-origin. The generated frontend runtime keys
			// `error.reason` off svcerr.ReasonHeader ("x-forge-error-reason");
			// without it in Expose-Headers the browser hides the header and
			// the typed-error runtime reads null on every cross-origin
			// response.
			w.Header().Set("Access-Control-Expose-Headers", "Connect-Protocol-Version, "+svcerr.ReasonHeader)

			if r.Method == http.MethodOptions {
				writePreflightHeaders(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// DevCORSMiddleware returns an HTTP middleware that reflects ANY request
// Origin back to the caller — an allow-all CORS policy intended for LOCAL
// DEVELOPMENT ONLY. It exists so a freshly-forged product's browser frontend
// works cross-origin out of the box, without the author first hand-curating a
// CORS_ORIGINS allow-list.
//
// Why this instead of passing "*" to CORSMiddleware:
//
//   - Credential-safe. It always echoes the CONCRETE request Origin into
//     Access-Control-Allow-Origin (never the literal "*"), so pairing it with
//     Access-Control-Allow-Credentials: true stays spec-valid. The "*" path in
//     CORSMiddleware, by contrast, panics when credentials are enabled (the
//     wildcard+credentials combination is spec-invalid and rejected in prod).
//
//   - Unambiguously dev-scoped. The generated server selects it ONLY when
//     ENVIRONMENT=development, so the permissive behavior can never leak into a
//     production build the way a stray "*" in a prod allow-list could.
//
// It MUST NOT be wired outside development. Reflecting every origin (especially
// with credentials) is exactly the footgun CORSMiddleware guards against for
// real deployments.
func DevCORSMiddleware(allowCredentials bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				// Same-origin or non-browser client: no CORS headers.
				next.ServeHTTP(w, r)
				return
			}

			// Responses differ by origin (we echo it), so cache on Origin.
			w.Header().Add("Vary", "Origin")

			// Allow-all: reflect whatever origin asked. Echoing the concrete
			// value (rather than "*") keeps credentialed requests spec-valid.
			w.Header().Set("Access-Control-Allow-Origin", origin)
			if allowCredentials {
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
			// Same expose-header contract as CORSMiddleware so the generated
			// frontend's typed-error runtime can read x-forge-error-reason
			// cross-origin.
			w.Header().Set("Access-Control-Expose-Headers", "Connect-Protocol-Version, "+svcerr.ReasonHeader)

			if r.Method == http.MethodOptions {
				writePreflightHeaders(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// containsWildcard reports whether origins contains the literal "*".
func containsWildcard(origins []string) bool {
	for _, o := range origins {
		if o == "*" {
			return true
		}
	}
	return false
}

// resolveAllowedOrigin picks the Access-Control-Allow-Origin value for the
// given request origin. Returns the value to echo plus whether a match was
// found. On wildcard policies the caller's origin is echoed rather than a
// literal "*" so the response stays usable if credentials are added later.
func resolveAllowedOrigin(origin string, allowOrigins []string, wildcard bool) (string, bool) {
	if wildcard {
		return origin, true
	}
	for _, allowed := range allowOrigins {
		if strings.EqualFold(origin, allowed) {
			return allowed, true
		}
	}
	return "", false
}

// writePreflightHeaders sets the response headers required for a CORS
// preflight (OPTIONS) reply and writes the 204 status. Kept separate from
// the main handler so the simple-request path stays easy to audit.
func writePreflightHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Connect-Protocol-Version, Connect-Timeout-Ms, Authorization, Traceparent, Tracestate, X-Request-Id")
	w.Header().Set("Access-Control-Max-Age", "86400")
	w.WriteHeader(http.StatusNoContent)
}
