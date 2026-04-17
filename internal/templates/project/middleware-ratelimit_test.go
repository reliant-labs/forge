//go:build ignore

package middleware

import (
	"context"
	"testing"
)

func TestRateLimitInterceptor_Disabled(t *testing.T) {
	if got := RateLimitInterceptor(RateLimitOptions{Rps: 0, Burst: 0}); got != nil {
		t.Fatalf("expected nil interceptor when Rps<=0, got %T", got)
	}
	if got := RateLimitInterceptor(RateLimitOptions{Rps: -1, Burst: 10}); got != nil {
		t.Fatalf("expected nil interceptor when Rps<0, got %T", got)
	}
}

func TestRateLimitInterceptor_EnforcesBurst(t *testing.T) {
	// Very low steady rate + small burst so we can observe rejection
	// without any time dependency. Burst=2 allows two Allow() calls
	// before the token bucket drains.
	ic := RateLimitInterceptor(RateLimitOptions{Rps: 1, Burst: 2})
	if ic == nil {
		t.Fatal("expected non-nil interceptor when Rps>0")
	}
	rl, ok := ic.(*rateLimitInterceptor)
	if !ok {
		t.Fatalf("unexpected interceptor type %T", ic)
	}

	key := "ip:203.0.113.1"
	l := rl.limiterFor(key)

	if !l.Allow() {
		t.Fatal("first request should be allowed")
	}
	if !l.Allow() {
		t.Fatal("second request should be allowed (within burst)")
	}
	if l.Allow() {
		t.Fatal("third request should be throttled (burst exhausted)")
	}
}

func TestRateLimitInterceptor_PerKeyIsolation(t *testing.T) {
	// Two distinct keys should have independent limiters: exhausting one
	// must not affect the other.
	ic := RateLimitInterceptor(RateLimitOptions{Rps: 1, Burst: 1})
	rl := ic.(*rateLimitInterceptor)

	a := rl.limiterFor("ip:198.51.100.1")
	b := rl.limiterFor("ip:198.51.100.2")

	if !a.Allow() {
		t.Fatal("key a first call should be allowed")
	}
	if a.Allow() {
		t.Fatal("key a second call should be throttled")
	}
	if !b.Allow() {
		t.Fatal("key b first call should be allowed even after a is exhausted")
	}
}

func TestRateLimitKey_PrefersClaimSubject(t *testing.T) {
	ctx := ContextWithClaims(context.Background(), &Claims{UserID: "user-123"})
	if got := rateLimitKey(ctx, "203.0.113.9:443"); got != "sub:user-123" {
		t.Fatalf("expected sub key for authenticated request, got %q", got)
	}
}

func TestRateLimitKey_FallsBackToPeerIP(t *testing.T) {
	ctx := context.Background()
	if got := rateLimitKey(ctx, "203.0.113.9:443"); got != "ip:203.0.113.9" {
		t.Fatalf("expected ip key stripped of port, got %q", got)
	}
	// Unparseable peer addresses must not panic and must be used as-is.
	if got := rateLimitKey(ctx, "bogus"); got != "ip:bogus" {
		t.Fatalf("expected ip:bogus for unparseable addr, got %q", got)
	}
	if got := rateLimitKey(ctx, ""); got != "ip:" {
		t.Fatalf("expected ip: for empty addr, got %q", got)
	}
}