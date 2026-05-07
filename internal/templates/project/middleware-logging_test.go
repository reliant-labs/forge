//go:build ignore

package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"connectrpc.com/connect"
)

// parseLogLines parses a JSON-lines buffer into a slice of maps, one per
// record. We use slog.NewJSONHandler so attributes are recoverable as-is;
// this keeps the test resilient to attribute reordering.
func parseLogLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("parse log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestLoggingInterceptor_Success(t *testing.T) {
	t.Parallel()

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ic := LoggingInterceptor(logger)

	req := connect.NewRequest(&struct{}{})
	req.Header().Set(RequestIDHeader, "req-abc")

	next := connect.UnaryFunc(func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, nil
	})

	_, err := ic.WrapUnary(next)(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	lines := parseLogLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("want 1 log line, got %d", len(lines))
	}
	got := lines[0]
	if got["msg"] != "rpc.ok" {
		t.Fatalf("want msg=rpc.ok, got %q", got["msg"])
	}
	if got["request_id"] != "req-abc" {
		t.Fatalf("want request_id=req-abc, got %v", got["request_id"])
	}
	// slog.Duration emits a numeric nanosecond count under the JSON
	// handler — not a formatted string. If a future refactor
	// accidentally switches to slog.String or slog.Any, the type will
	// flip to string/object and this assertion will catch it.
	switch got["duration"].(type) {
	case float64: // json.Unmarshal decodes numbers into float64
	default:
		t.Fatalf("duration must be numeric (slog.Duration), got %T: %v", got["duration"], got["duration"])
	}
}

func TestLoggingInterceptor_PropagatesRequestIDFromContext(t *testing.T) {
	t.Parallel()

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ic := LoggingInterceptor(logger)

	req := connect.NewRequest(&struct{}{})
	ctx := ContextWithRequestID(context.Background(), "ctx-id")

	next := connect.UnaryFunc(func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, nil
	})
	if _, err := ic.WrapUnary(next)(ctx, req); err != nil {
		t.Fatal(err)
	}

	lines := parseLogLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("want 1 log line, got %d", len(lines))
	}
	if lines[0]["request_id"] != "ctx-id" {
		t.Fatalf("want request_id=ctx-id (context), got %v", lines[0]["request_id"])
	}
}

func TestLoggingInterceptor_ErrorIsLoggedAtErrorLevel(t *testing.T) {
	t.Parallel()

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ic := LoggingInterceptor(logger)

	req := connect.NewRequest(&struct{}{})
	next := connect.UnaryFunc(func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, connect.NewError(connect.CodeInternal, nil)
	})
	if _, err := ic.WrapUnary(next)(context.Background(), req); err == nil {
		t.Fatal("want error propagated")
	}

	lines := parseLogLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("want 1 log line, got %d", len(lines))
	}
	got := lines[0]
	if got["msg"] != "rpc.error" {
		t.Fatalf("want msg=rpc.error, got %q", got["msg"])
	}
	if got["level"] != "ERROR" {
		t.Fatalf("want level=ERROR, got %v", got["level"])
	}
	if _, ok := got["error"]; !ok {
		t.Fatal("error attribute must be present on failed rpc")
	}
}