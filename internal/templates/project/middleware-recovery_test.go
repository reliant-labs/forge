//go:build ignore

package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect"
)

// errSentinel is an error value that TestRecovery_PreservesErrorValue uses
// to verify the recovered error chain is preserved (so callers can use
// errors.Is / errors.As to unwrap).
var errSentinel = errors.New("sentinel boom")

func TestRecoveryInterceptor_RecoversErrorValue(t *testing.T) {
	t.Parallel()

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, nil))
	ic := RecoveryInterceptor(logger)

	req := connect.NewRequest(&struct{}{})
	next := connect.UnaryFunc(func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		panic(errSentinel)
	})

	resp, err := ic.WrapUnary(next)(context.Background(), req)
	if err == nil {
		t.Fatal("want error from recovered panic")
	}
	if resp != nil {
		t.Fatalf("want nil response on panic, got %T", resp)
	}
	// The recovered error must be wrapped with %w so callers can unwrap
	// to the original cause. This guarantees errors.Is keeps working
	// across the recovery boundary.
	if !errors.Is(err, errSentinel) {
		t.Fatalf("recovered error must wrap the original: %v", err)
	}
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("want *connect.Error, got %T", err)
	}
	if cerr.Code() != connect.CodeInternal {
		t.Fatalf("want CodeInternal, got %s", cerr.Code())
	}

	// The log record must carry procedure, panic, and stack attributes.
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &record); err != nil {
		t.Fatalf("parse log record: %v", err)
	}
	for _, key := range []string{"procedure", "panic", "stack"} {
		if _, ok := record[key]; !ok {
			t.Fatalf("log record missing attr %q: %v", key, record)
		}
	}
	if stack, ok := record["stack"].(string); !ok || !strings.Contains(stack, "middleware-recovery") && !strings.Contains(stack, "recovery.go") {
		// We don't care which filename format goroutine stacks use —
		// just that a non-empty stack is present.
		if stack == "" {
			t.Fatal("stack must not be empty")
		}
	}
}

func TestRecoveryInterceptor_RecoversNonErrorValue(t *testing.T) {
	t.Parallel()

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, nil))
	ic := RecoveryInterceptor(logger)

	req := connect.NewRequest(&struct{}{})
	next := connect.UnaryFunc(func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		// Panicking with a non-error value (string, int, struct) is a
		// common mistake; the middleware must still produce a
		// reasonable connect.Error.
		panic("something went wrong: 42")
	})

	resp, err := ic.WrapUnary(next)(context.Background(), req)
	if err == nil {
		t.Fatal("want error from recovered panic")
	}
	if resp != nil {
		t.Fatalf("want nil response on panic, got %T", resp)
	}
	if !strings.Contains(err.Error(), "something went wrong: 42") {
		t.Fatalf("recovered error should embed panic value: %v", err)
	}

	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &record); err != nil {
		t.Fatalf("parse log record: %v", err)
	}
	if record["msg"] != "panic recovered" {
		t.Fatalf("want msg=panic recovered, got %v", record["msg"])
	}
}

func TestRecoveryInterceptor_NoPanicIsPassthrough(t *testing.T) {
	t.Parallel()

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, nil))
	ic := RecoveryInterceptor(logger)

	req := connect.NewRequest(&struct{}{})
	next := connect.UnaryFunc(func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, nil
	})

	resp, err := ic.WrapUnary(next)(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != nil {
		t.Fatalf("want nil response, got %T", resp)
	}
	if buf.Len() != 0 {
		t.Fatalf("recovery middleware must not log on success path, got %q", buf.String())
	}
}