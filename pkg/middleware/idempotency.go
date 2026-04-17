// Package middleware provides reusable HTTP middleware for forge-generated services.
package middleware

import (
	"bytes"
	"net/http"
	"sync"
	"time"
)

// IdempotencyConfig controls the behavior of the idempotency middleware.
type IdempotencyConfig struct {
	// CacheSize is the maximum number of idempotency keys to track.
	// Default: 1000.
	CacheSize int

	// TTL is how long a cached response is kept. After this duration the
	// key is evicted and a duplicate request is treated as new.
	// Default: 1 hour.
	TTL time.Duration
}

func (c *IdempotencyConfig) cacheSize() int {
	if c.CacheSize > 0 {
		return c.CacheSize
	}
	return 1000
}

func (c *IdempotencyConfig) ttl() time.Duration {
	if c.TTL > 0 {
		return c.TTL
	}
	return time.Hour
}

type cachedResponse struct {
	statusCode int
	header     http.Header
	body       []byte
	expiresAt  time.Time
}

// IdempotencyMiddleware returns an http.Handler that deduplicates requests
// carrying an Idempotency-Key header. If a request with the same key has
// already been served (and is still in the cache), the cached response is
// replayed without invoking the downstream handler.
//
// Usage:
//
//	mux := http.NewServeMux()
//	cfg := middleware.IdempotencyConfig{CacheSize: 500, TTL: 30 * time.Minute}
//	handler := middleware.IdempotencyMiddleware(cfg)(mux)
func IdempotencyMiddleware(cfg IdempotencyConfig) func(http.Handler) http.Handler {
	maxSize := cfg.cacheSize()
	ttl := cfg.ttl()

	var mu sync.Mutex
	cache := make(map[string]*cachedResponse)
	// order tracks insertion order for LRU eviction.
	order := make([]string, 0, maxSize)

	evictExpired := func() {
		now := time.Now()
		for k, v := range cache {
			if now.After(v.expiresAt) {
				delete(cache, k)
			}
		}
		// Rebuild order slice.
		filtered := order[:0]
		for _, k := range order {
			if _, ok := cache[k]; ok {
				filtered = append(filtered, k)
			}
		}
		order = filtered
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("Idempotency-Key")
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}

			mu.Lock()
			// Check cache hit.
			if entry, ok := cache[key]; ok && time.Now().Before(entry.expiresAt) {
				mu.Unlock()
				for k, vs := range entry.header {
					for _, v := range vs {
						w.Header().Add(k, v)
					}
				}
				w.WriteHeader(entry.statusCode)
				_, _ = w.Write(entry.body)
				return
			}
			mu.Unlock()

			// Cache miss — execute handler and capture response.
			rec := &responseRecorder{
				header: make(http.Header),
				body:   &bytes.Buffer{},
				code:   http.StatusOK,
			}
			next.ServeHTTP(rec, r)

			// Write the captured response to the real writer.
			for k, vs := range rec.header {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(rec.code)
			_, _ = w.Write(rec.body.Bytes())

			// Store in cache.
			entry := &cachedResponse{
				statusCode: rec.code,
				header:     rec.header.Clone(),
				body:       rec.body.Bytes(),
				expiresAt:  time.Now().Add(ttl),
			}

			mu.Lock()
			defer mu.Unlock()
			evictExpired()
			if len(cache) >= maxSize {
				// Evict oldest.
				if len(order) > 0 {
					delete(cache, order[0])
					order = order[1:]
				}
			}
			cache[key] = entry
			order = append(order, key)
		})
	}
}

// responseRecorder captures an HTTP response for caching.
type responseRecorder struct {
	header http.Header
	body   *bytes.Buffer
	code   int
}

func (r *responseRecorder) Header() http.Header { return r.header }

func (r *responseRecorder) Write(b []byte) (int, error) { return r.body.Write(b) }

func (r *responseRecorder) WriteHeader(code int) { r.code = code }
