package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// replyServer answers every procedure with status and body.
func replyServer(t *testing.T, status int, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	ep, err := ResolveEndpoint("prod", &Declaration{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return NewClient(ep, Credential{Token: "tok", Source: SourceEnv, From: "ACME_TOKEN"})
}

// jsonOfLen is a JSON object exactly n bytes long.
func jsonOfLen(n int) string {
	const head, tail = `{"pad":"`, `"}`
	return head + strings.Repeat("x", n-len(head)-len(tail)) + tail
}

const procBig = "controlplane.v1.ObservabilityService/GetTrace"

// An answer past the read limit used to be cut at the limit and fail to
// decode as "unexpected end of JSON input", which callers reported as an
// unreachable control plane. It is now its own error, naming the procedure
// and the limit.
func TestCall_AnswerOverTheLimitIsResponseTooLarge(t *testing.T) {
	c := replyServer(t, http.StatusOK, jsonOfLen(DefaultMaxResponseBytes+1))
	var out map[string]any
	err := c.Call(context.Background(), procBig, map[string]any{}, &out)
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("err = %v, want ErrResponseTooLarge", err)
	}
	var se *json.SyntaxError
	if errors.As(err, &se) {
		t.Fatalf("err = %v: a cut-off answer must not be decoded", err)
	}
	var tl *ResponseTooLargeError
	if !errors.As(err, &tl) || tl.Limit != DefaultMaxResponseBytes || tl.Procedure != procBig {
		t.Fatalf("err = %#v, want the procedure and the default limit", err)
	}
	if !strings.Contains(err.Error(), "GetTrace") || !strings.Contains(err.Error(), "8 MiB") {
		t.Errorf("message %q should name the procedure and the limit", err)
	}
}

func TestCall_AnswerExactlyAtTheLimitDecodes(t *testing.T) {
	c := replyServer(t, http.StatusOK, jsonOfLen(64))
	c.DeclareResponseLimit(64, procBig)
	var out struct{ Pad string }
	if err := c.Call(context.Background(), procBig, map[string]any{}, &out); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(out.Pad) == 0 {
		t.Fatal("answer not decoded")
	}
}

// A declared limit applies to its procedure only; the others keep the
// default, and a limit <= 0 restores it.
func TestDeclareResponseLimit(t *testing.T) {
	c := replyServer(t, http.StatusOK, jsonOfLen(DefaultMaxResponseBytes+1))
	c.DeclareResponseLimit(DefaultMaxResponseBytes+1, "/"+procBig)
	var out map[string]any
	if err := c.Call(context.Background(), procBig, map[string]any{}, &out); err != nil {
		t.Fatalf("declared procedure: %v", err)
	}
	if err := c.Call(context.Background(), "controlplane.v1.ObservabilityService/SearchLogs", map[string]any{}, &out); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("undeclared procedure: err = %v, want ErrResponseTooLarge", err)
	}
	c.DeclareResponseLimit(0, procBig)
	if err := c.Call(context.Background(), procBig, map[string]any{}, &out); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("after reset: err = %v, want ErrResponseTooLarge", err)
	}
}

// An oversized error page is still read up to the limit and reported as the
// failed call it is, not as an oversized answer.
func TestCall_OversizedErrorPageIsTheStatusError(t *testing.T) {
	c := replyServer(t, http.StatusBadGateway, "<html>"+strings.Repeat("x", 128))
	c.DeclareResponseLimit(64, procBig)
	err := c.Call(context.Background(), procBig, map[string]any{}, nil)
	if err == nil || errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("err = %v, want the 502's error", err)
	}
}
