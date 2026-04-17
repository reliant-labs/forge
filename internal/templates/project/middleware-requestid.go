//go:build ignore

package middleware

import (
	"context"
	"crypto/rand"
	"net/http"
	"time"

	"github.com/oklog/ulid/v2"
)

// RequestIDHeader is the canonical header carrying the per-request correlation
// ID on both the inbound request and the outbound response. Using a single
// constant here keeps middleware, handlers, and clients in agreement.
const RequestIDHeader = "X-Request-Id"

type requestIDContextKey struct{}

var requestIDKey = requestIDContextKey{}

// RequestIDFromContext returns the request ID attached to ctx, or "" if none
// is set. Useful for handlers and downstream logging sites that want to
// include the ID on custom log lines. Nil ctx returns "" (never panics) so
// callers in CLI/worker contexts can share the same helper.
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// ContextWithRequestID returns a copy of ctx with the given request ID
// attached. Exposed so tests and non-HTTP entrypoints can propagate the
// value into code paths that read RequestIDFromContext.
func ContextWithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDMiddleware is an HTTP middleware that ensures every request
// carries a correlation ID. It must be wired ahead of the logging middleware
// so log records inherit the generated ID.
//
// Behavior:
//
//   - If the inbound request has a non-empty RequestIDHeader, that value
//     is trusted and reused. This lets edge proxies and upstream services
//     stitch together a single trace across hops without coordination.
//
//   - Otherwise a Crockford-base32 ULID is generated (monotonic + sortable
//     + compact at 26 chars). ULIDs avoid UUID's opacity without requiring
//     a configured epoch or counter.
//
//   - The chosen ID is exposed to downstream middleware/handlers via the
//     request context (RequestIDFromContext) and echoed on the response
//     header so the client can log it for later correlation.
func RequestIDMiddleware() func(http.Handler) http.Handler {
	// A single entropy source shared across requests is safe: ulid.Make
	// serializes access via a mutex when the reader is non-nil. Using
	// crypto/rand keeps IDs unpredictable to third parties that might
	// otherwise enumerate them.
	entropy := rand.Reader

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(RequestIDHeader)
			if id == "" {
				id = newULID(entropy)
			}
			w.Header().Set(RequestIDHeader, id)
			next.ServeHTTP(w, r.WithContext(ContextWithRequestID(r.Context(), id)))
		})
	}
}

// newULID returns a fresh ULID string using the given entropy source. Factored
// out so tests can swap in a deterministic reader without reaching into the
// ULID internals.
func newULID(entropy interface {
	Read(p []byte) (int, error)
}) string {
	return ulid.MustNew(ulid.Timestamp(time.Now()), entropy).String()
}