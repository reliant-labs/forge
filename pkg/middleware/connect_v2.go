package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"
	connectv2 "connectrpc.com/connect/v2"
)

// This file is the connect-go v2 surface of the package's Connect
// interceptors. Each v2 constructor shares its v1 twin's state and decisions
// — the same limiter, the same audit record, the same cache key — and
// returns a [connectv2.ServerInterceptor]. A v2 interceptor surrounds the
// whole server call for every stream type, so one function does what the
// v1 WrapUnary and WrapStreamingHandler did between them. The constructors
// that return nil when disabled (rate limit, idempotency) still do: a chain
// builder skips a nil interceptor.

// RateLimitInterceptorV2 is [RateLimitInterceptor] for connect-go v2: the
// same per-key token bucket, keyed by claim subject and falling back to the
// peer IP (CallInfo.PeerAddr, the address the v1 interceptor read from
// Peer().Addr), applied to unary and streaming calls alike. A throttled call
// fails with CodeResourceExhausted and the message "rate limit exceeded",
// exactly what the v1 interceptor sends. Returns nil when opts.Rps <= 0.
func RateLimitInterceptorV2(opts RateLimitOptions, claimsFrom ClaimsLookup) connectv2.ServerInterceptor {
	rl := newRateLimiter(opts, claimsFrom)
	if rl == nil {
		return nil
	}
	return func(next connectv2.ServerFunc) connectv2.ServerFunc {
		return func(ctx context.Context, spec connectv2.Spec, stream connectv2.ServerStream) error {
			if !rl.limiterFor(rl.rateLimitKey(ctx, peerAddrV2(ctx))).Allow() {
				return connectv2.NewError(connectv2.CodeResourceExhausted, "rate limit exceeded")
			}
			return next(ctx, spec, stream)
		}
	}
}

// AuditInterceptorV2 is [AuditInterceptor] for connect-go v2: the slog-only
// audit record, field for field.
func AuditInterceptorV2(logger *slog.Logger, claimsFrom ClaimsLookup) connectv2.ServerInterceptor {
	return AuditInterceptorWithSinkV2(logger, claimsFrom, nil)
}

// AuditInterceptorWithSinkV2 is [AuditInterceptorWithSink] for connect-go
// v2. Every RPC, unary or streaming, produces the same slog record and the
// same [AuditEvent] the v1 interceptor does: procedure from the Spec, peer
// from CallInfo.PeerAddr, and the error's code read from whichever major's
// *connect.Error it carries (a v2 one first). The duration covers the whole
// server call, which in v2 includes handing the response to the stream.
func AuditInterceptorWithSinkV2(logger *slog.Logger, claimsFrom ClaimsLookup, sink AuditSink) connectv2.ServerInterceptor {
	a := newAuditInterceptor(logger, claimsFrom, sink)
	a.v2 = true
	return func(next connectv2.ServerFunc) connectv2.ServerFunc {
		return func(ctx context.Context, spec connectv2.Spec, stream connectv2.ServerStream) error {
			start := time.Now()
			err := next(ctx, spec, stream)
			a.logAudit(ctx, spec.Procedure, peerAddrV2(ctx), start, err)
			return err
		}
	}
}

// IdempotencyInterceptorV2 is [IdempotencyInterceptor] for connect-go v2:
// a unary request carrying an Idempotency-Key header whose (caller,
// procedure, key) triple was answered within the TTL gets the cached answer
// without the handler running again. Streaming calls pass through, as in
// v1. Returns nil when opts.CacheSize <= 0 or claimsFrom is nil.
//
// Unlike the v1 interceptor, which keys its cache by procedure and client
// key alone, the v2 cache is scoped to the authenticated caller: claimsFrom
// names the principal (claims.UserID) and its tenant (claims.OrgID), and
// both are part of the key, so one caller can never be handed another's
// answer by reusing their Idempotency-Key. A call with no identity —
// claimsFrom reports none, or a claim with an empty UserID — is neither
// cached nor replayed: the handler runs as if no key were sent. Put the
// interceptor after authentication in the chain, so the claims are there.
//
// A v2 handler returns no response value — it sends its reply on the
// stream — so the interceptor records what the handler sends, and with it
// the response headers and trailers the handler set (only those: metadata
// that outer interceptors had already set before the call is the current
// request's, such as its request id, and is never replayed over). A replay
// re-applies that metadata and sends the recorded message, or returns the
// recorded error.
func IdempotencyInterceptorV2(opts IdempotencyOptions, claimsFrom ClaimsLookup) connectv2.ServerInterceptor {
	if opts.CacheSize <= 0 || claimsFrom == nil {
		return nil
	}
	if opts.TTL <= 0 {
		opts.TTL = time.Hour
	}
	i := &idempotencyInterceptorV2{cache: newLRUCache[*cachedRPCReply](opts.CacheSize), ttl: opts.TTL, claimsFrom: claimsFrom}
	return i.wrap
}

type idempotencyInterceptorV2 struct {
	mu         sync.Mutex
	cache      *lruCache[*cachedRPCReply]
	ttl        time.Duration
	claimsFrom ClaimsLookup
}

// callerCacheKey is the cache key of one caller's (procedure, key) pair, or
// "" when ctx carries no authenticated caller. Each part is length-prefixed
// before hashing, so no choice of ids, procedure or client key can make two
// different triples collide.
func (i *idempotencyInterceptorV2) callerCacheKey(ctx context.Context, procedure, clientKey string) string {
	claims, ok := i.claimsFrom(ctx)
	if !ok || claims == nil || claims.UserID == "" {
		return ""
	}
	h := sha256.New()
	for _, part := range []string{claims.OrgID, claims.UserID, procedure, clientKey} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// cachedRPCReply is one recorded v2 unary answer.
type cachedRPCReply struct {
	msg       any // the message the handler sent; nil when it sent none
	err       error
	header    map[string][]string // response headers the handler set
	trailer   map[string][]string // response trailers the handler set
	expiresAt time.Time
}

func (i *idempotencyInterceptorV2) wrap(next connectv2.ServerFunc) connectv2.ServerFunc {
	return func(ctx context.Context, spec connectv2.Spec, stream connectv2.ServerStream) error {
		if spec.StreamType != connectv2.StreamTypeUnary {
			return next(ctx, spec, stream) // idempotency is only meaningful for unary RPCs
		}
		info, ok := connectv2.CallInfoForServerContext(ctx)
		if !ok {
			return next(ctx, spec, stream)
		}
		key := info.RequestHeader().Get(IdempotencyKeyHeader)
		if key == "" {
			return next(ctx, spec, stream)
		}
		cacheKey := i.callerCacheKey(ctx, spec.Procedure, key)
		if cacheKey == "" {
			return next(ctx, spec, stream) // no caller to scope the answer to
		}

		i.mu.Lock()
		if cached, ok := i.cache.get(cacheKey); ok && time.Now().Before(cached.expiresAt) {
			i.mu.Unlock()
			return cached.replay(info, stream)
		}
		i.mu.Unlock()

		headerBefore := headerSnapshot(info.ResponseHeader())
		trailerBefore := headerSnapshot(info.ResponseTrailer())
		rec := &recordingStream{ServerStream: stream}
		err := next(ctx, spec, rec)

		i.mu.Lock()
		i.cache.add(cacheKey, &cachedRPCReply{
			msg:       rec.sent,
			err:       err,
			header:    headerChanges(headerBefore, info.ResponseHeader()),
			trailer:   headerChanges(trailerBefore, info.ResponseTrailer()),
			expiresAt: time.Now().Add(i.ttl),
		})
		i.mu.Unlock()

		return err
	}
}

func (c *cachedRPCReply) replay(info *connectv2.CallInfo, stream connectv2.ServerStream) error {
	for k, vs := range c.header {
		info.ResponseHeader().SetValues(k, slices.Clone(vs))
	}
	for k, vs := range c.trailer {
		info.ResponseTrailer().SetValues(k, slices.Clone(vs))
	}
	if c.err != nil {
		return c.err
	}
	if c.msg == nil {
		return nil
	}
	return stream.Send(c.msg)
}

// recordingStream remembers the response message a unary handler sends.
type recordingStream struct {
	connectv2.ServerStream
	sent any
}

func (s *recordingStream) Send(msg any) error {
	s.sent = msg
	return s.ServerStream.Send(msg)
}

// headerSnapshot copies h's entries, so later edits to h cannot reach it.
func headerSnapshot(h *connectv2.Header) map[string][]string {
	out := make(map[string][]string, h.Len())
	for k, vs := range h.All() {
		out[k] = slices.Clone(vs)
	}
	return out
}

// headerChanges returns the entries of h that are new or changed since
// before, copied.
func headerChanges(before map[string][]string, h *connectv2.Header) map[string][]string {
	var out map[string][]string
	for k, vs := range h.All() {
		if prev, ok := before[k]; ok && slices.Equal(prev, vs) {
			continue
		}
		if out == nil {
			out = map[string][]string{}
		}
		out[k] = slices.Clone(vs)
	}
	return out
}

// peerAddrV2 is the v2 call's remote address, or "" outside a server call.
func peerAddrV2(ctx context.Context) string {
	if info, ok := connectv2.CallInfoForServerContext(ctx); ok {
		return info.PeerAddr
	}
	return ""
}

// errorCode is the Connect code string of err for an audit record. The v1
// interceptor (preferV2 false) reads it exactly as connect.CodeOf did — a
// v1 *connect.Error anywhere in err — and otherwise from a v2 one. The v2
// interceptor reads the outermost *connect.Error of either major, so a v2
// error carrying a v1 one as its local cause reports its own code. An error
// with neither is "unknown", as before.
func errorCode(err error, preferV2 bool) string {
	if !preferV2 {
		if _, ok := errors.AsType[*connect.Error](err); ok {
			return connect.CodeOf(err).String()
		}
	}
	switch e1, e2 := outermostConnectError(err); {
	case e2 != nil:
		return e2.Code().String()
	case e1 != nil:
		return e1.Code().String()
	}
	return connect.CodeOf(err).String()
}

// outermostConnectError finds the first *connect.Error of either major in
// err's tree, in errors.As order, and returns it in its own major's slot.
func outermostConnectError(err error) (*connect.Error, *connectv2.Error) {
	switch e := err.(type) {
	case nil:
		return nil, nil
	case *connect.Error:
		return e, nil
	case *connectv2.Error:
		return nil, e
	case interface{ Unwrap() error }:
		return outermostConnectError(e.Unwrap())
	case interface{ Unwrap() []error }:
		for _, inner := range e.Unwrap() {
			if e1, e2 := outermostConnectError(inner); e1 != nil || e2 != nil {
				return e1, e2
			}
		}
	}
	return nil, nil
}
