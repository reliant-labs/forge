package serverkit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProbes_ChecksAddedAfterTheHandlerIsBuilt(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := &Probes{Drainer: &Drainer{Logger: logger}, Logger: logger, Metrics: http.NotFoundHandler()}
	front := p.Front()
	for _, route := range []string{"/healthz", "/readyz", "/metrics"} {
		if front[route] == nil {
			t.Fatalf("Front has no %s", route)
		}
	}
	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		front["/readyz"].ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return rec
	}
	if rec := get(); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz before MarkReady = %d", rec.Code)
	}
	p.Drainer.MarkReady()
	if rec := get(); rec.Code != http.StatusOK {
		t.Fatalf("/readyz with no checks = %d", rec.Code)
	}
	var failing error
	p.AddReadyCheck(
		ReadyCheck{Name: "db", Check: func(context.Context) error { return failing }},
		ReadyCheck{Name: "dropped"},
	)
	if rec := get(); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"db"`) || strings.Contains(rec.Body.String(), "dropped") {
		t.Fatalf("/readyz = %d %s", rec.Code, rec.Body)
	}
	failing = errors.New("down")
	if rec := get(); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz with a failing late check = %d", rec.Code)
	}
	if got := len(p.ReadyChecks()); got != 1 {
		t.Fatalf("%d checks, want 1", got)
	}
}

func TestEdge_FailsClosedOnAnUnwiredLayer(t *testing.T) {
	if _, err := (Edge{}).Wrap(Config{CORSOrigins: []string{"https://a"}}); err == nil {
		t.Fatal("CORS on with no factory was accepted")
	}
	if _, err := (Edge{}).Wrap(Config{SecurityHeaders: true}); err == nil {
		t.Fatal("security headers on with no factory was accepted")
	}
	var order []string
	layer := func(n string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { order = append(order, n); next.ServeHTTP(w, r) })
		}
	}
	wrap, err := Edge{
		CORS:            func([]string, bool) func(http.Handler) http.Handler { return layer("cors") },
		SecurityHeaders: func(bool) func(http.Handler) http.Handler { return layer("sec") },
		RequestID:       func() func(http.Handler) http.Handler { return layer("rid") },
		Outer:           layer("outer"),
	}.Wrap(Config{CORSOrigins: []string{"https://a"}, SecurityHeaders: true})
	if err != nil {
		t.Fatal(err)
	}
	wrap(http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if got := strings.Join(order, ","); got != "outer,rid,sec,cors" {
		t.Fatalf("edge order %s, want outer,rid,sec,cors", got)
	}
}
