package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/reliant-labs/forge/pkg/auth"
)

// IdempotencyKeyHeader is the canonical header carrying the
// client-supplied idempotency key. Methods annotated with
// idempotency_key=true in the proto options SHOULD require this header;
// if absent the request proceeds without deduplication.
const IdempotencyKeyHeader = "Idempotency-Key"

// IdempotencyOptions configures the RPC idempotency interceptors.
//
//   - CacheSize: maximum number of cached responses (default 1000)
//   - TTL:       how long a cached response is valid (default 1h)
//   - Partition: replaces the caller-scoped interceptors' default
//     partition (see the field)
//
// CacheSize <= 0 disables the interceptor — the constructors return nil
// and callers should skip appending it to the chain.
type IdempotencyOptions struct {
	CacheSize int
	TTL       time.Duration

	// Partition, when set, names the cache partition a caller's answers
	// are stored in and replayed from, in place of the default — a digest
	// of every authority the caller's claims carry (the fixed fields, the
	// credential, and Raw minus iat/exp/nbf/jti). It is consulted by
	// [IdempotencyInterceptorWithCaller] and [IdempotencyInterceptorV2]
	// only, after they have found a principal with a non-empty UserID; the
	// procedure and Idempotency-Key are still part of the key.
	//
	// Two calls share cached answers exactly when Partition returns the
	// same string for both, so it must distinguish every authority the
	// handler checks: a replay skips the handler, and with it the handler's
	// permission check. Returning ok=false, or "", bypasses the cache for
	// that call: the handler runs, and nothing is stored or replayed.
	//
	// The plain [IdempotencyInterceptor] ignores it.
	Partition func(ctx context.Context, claims *auth.Claims) (partition string, ok bool)
}

type cachedRPCResponse struct {
	resp      connect.AnyResponse
	err       error
	expiresAt time.Time
}

type idempotencyInterceptor struct {
	mu    sync.Mutex
	cache *lruCache[*cachedRPCResponse]
	ttl   time.Duration
	scope *callerScope // nil: keyed by procedure and client key alone
}

// IdempotencyInterceptor returns a Connect interceptor that deduplicates
// requests carrying an Idempotency-Key header. If the same key is seen
// within the TTL window, the cached response (or error) is returned
// without calling the handler again.
//
// Keys are scoped per-procedure so the same key on different RPCs is
// treated as distinct. For the HTTP-layer equivalent (REST/webhook
// routes) see [IdempotencyMiddleware].
//
// The cache is NOT scoped to the caller: any caller presenting the same
// key on the same procedure within the TTL gets the first caller's answer,
// without the handler — or its permission checks — running. Where callers
// are authenticated, use [IdempotencyInterceptorWithCaller], which
// partitions the cache by the caller and every authority they presented.
// opts.Partition is ignored here.
//
// When opts.CacheSize <= 0 the interceptor is disabled and nil is returned.
//
// connect-go v2 projects use [IdempotencyInterceptorV2], which is always
// caller-scoped.
func IdempotencyInterceptor(opts IdempotencyOptions) connect.Interceptor {
	if opts.CacheSize <= 0 {
		return nil
	}
	if opts.TTL <= 0 {
		opts.TTL = time.Hour
	}
	return &idempotencyInterceptor{cache: newLRUCache[*cachedRPCResponse](opts.CacheSize), ttl: opts.TTL}
}

func (i *idempotencyInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		key := req.Header().Get(IdempotencyKeyHeader)
		if key == "" {
			return next(ctx, req)
		}
		var cacheKey string
		if i.scope == nil {
			cacheKey = idempotencyCacheKey(req.Spec().Procedure, key)
		} else if cacheKey = i.scope.cacheKey(ctx, req.Spec().Procedure, key); cacheKey == "" {
			return next(ctx, req) // no partition to scope the answer to
		}

		i.mu.Lock()
		if cached, ok := i.cache.get(cacheKey); ok && time.Now().Before(cached.expiresAt) {
			i.mu.Unlock()
			return cached.resp, cached.err
		}
		i.mu.Unlock()

		resp, err := next(ctx, req)

		i.mu.Lock()
		i.cache.add(cacheKey, &cachedRPCResponse{
			resp:      resp,
			err:       err,
			expiresAt: time.Now().Add(i.ttl),
		})
		i.mu.Unlock()

		return resp, err
	}
}

func (i *idempotencyInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *idempotencyInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next // idempotency is only meaningful for unary RPCs
}

// idempotencyCacheKey combines procedure and client key into a single cache
// key. Using a hash avoids unbounded key length from malicious clients.
func idempotencyCacheKey(procedure, clientKey string) string {
	h := sha256.Sum256([]byte(procedure + "\x00" + clientKey))
	return hex.EncodeToString(h[:])
}
