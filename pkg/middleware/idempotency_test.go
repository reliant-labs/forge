package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestIdempotencyMiddleware_NoKey(t *testing.T) {
	var calls int32
	handler := IdempotencyMiddleware(IdempotencyConfig{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))

	// Request without Idempotency-Key passes through every time.
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("expected 3 handler invocations, got %d", got)
	}
}

func TestIdempotencyMiddleware_DuplicateKey(t *testing.T) {
	var calls int32
	handler := IdempotencyMiddleware(IdempotencyConfig{CacheSize: 10, TTL: time.Minute})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("X-Custom", "val")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	}))

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("Idempotency-Key", "abc-123")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusCreated {
			t.Fatalf("iteration %d: expected 201, got %d", i, rr.Code)
		}
		body, _ := io.ReadAll(rr.Body)
		if string(body) != "created" {
			t.Fatalf("iteration %d: expected body 'created', got %q", i, body)
		}
		if rr.Header().Get("X-Custom") != "val" {
			t.Fatalf("iteration %d: missing X-Custom header", i)
		}
	}

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 handler invocation (cached), got %d", got)
	}
}

func TestIdempotencyMiddleware_TTLExpiry(t *testing.T) {
	var calls int32
	handler := IdempotencyMiddleware(IdempotencyConfig{CacheSize: 10, TTL: 1 * time.Millisecond})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Idempotency-Key", "expire-me")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	time.Sleep(5 * time.Millisecond) // Wait for TTL expiry.

	req2 := httptest.NewRequest(http.MethodPost, "/", nil)
	req2.Header.Set("Idempotency-Key", "expire-me")
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req2)

	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected 2 handler invocations after TTL expiry, got %d", got)
	}
}

func TestIdempotencyMiddleware_LRUEviction(t *testing.T) {
	var calls int32
	handler := IdempotencyMiddleware(IdempotencyConfig{CacheSize: 2, TTL: time.Minute})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
	}))

	// Fill cache with 2 entries.
	for _, key := range []string{"a", "b"} {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("Idempotency-Key", key)
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}

	// Insert a 3rd — should evict "a".
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Idempotency-Key", "c")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	// "a" should now miss (handler invoked again).
	req2 := httptest.NewRequest(http.MethodPost, "/", nil)
	req2.Header.Set("Idempotency-Key", "a")
	handler.ServeHTTP(httptest.NewRecorder(), req2)

	// 3 initial + 1 re-invocation of "a" = 4
	if got := atomic.LoadInt32(&calls); got != 4 {
		t.Fatalf("expected 4 handler invocations, got %d", got)
	}
}
