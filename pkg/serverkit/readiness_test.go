package serverkit_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/serverkit"
)

// readiness_test.go — /readyz used to be a static "ok".
//
// Verified against a running scaffold: with the database unreachable, every
// RPC returned 500 and /readyz kept returning `ok [200]`. A rolling deploy
// carrying a bad DB secret therefore reports Ready on the first replica,
// shifts traffic onto it, and proceeds to 100% with green probes the whole
// way. That is the failure these tests exist to make impossible.

// getJSON performs a GET and returns the status and decoded body.
func getJSON(t *testing.T, url string) (int, map[string]any, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, decoded, string(raw)
}

// failingDependency is a dependency whose health a test flips at will —
// standing in for the database going away underneath a running process.
type failingDependency struct{ down atomic.Bool }

func (d *failingDependency) check(context.Context) error {
	if d.down.Load() {
		return errors.New("dial tcp 10.0.0.5:5432: connect: connection refused (dsn=postgres://app:s3cr3t@db.internal/prod)")
	}
	return nil
}

// TestReadyz_FailsWhenADependencyIsDown is the whole blocker in one test:
// the process is still listening and still "alive", but it cannot serve, and
// /readyz must say so.
func TestReadyz_FailsWhenADependencyIsDown(t *testing.T) {
	// Not parallel — sends SIGTERM.
	addr := freeAddr(t)
	dep := &failingDependency{}

	srv := serverkit.Server{Handler: emptyHandler()}
	srv.AddReadyCheck(serverkit.ReadyCheck{Name: "database", Check: dep.check})

	errCh, _ := runInBackground(t, baseConfig(addr), srv)
	waitReady(t, addr, 2*time.Second)

	// Healthy: 200 and a body that says so.
	status, body, raw := getJSON(t, "http://"+addr+"/readyz")
	if status != http.StatusOK {
		t.Fatalf("healthy readyz = %d, want 200 (body %s)", status, raw)
	}
	if body["status"] != "ready" {
		t.Errorf("healthy readyz status = %v, want \"ready\" (body %s)", body["status"], raw)
	}

	// The dependency goes away.
	dep.down.Store(true)

	status, body, raw = getJSON(t, "http://"+addr+"/readyz")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d with the database down, want 503 — a rolling deploy takes 200 as permission to continue (body %s)", status, raw)
	}
	if body["status"] != "not_ready" {
		t.Errorf("readyz status = %v, want \"not_ready\" (body %s)", body["status"], raw)
	}

	// It must NAME the failing dependency, or an operator has a red probe
	// and no idea which of five dependencies caused it.
	if !strings.Contains(raw, `"name":"database"`) || !strings.Contains(raw, `"ok":false`) {
		t.Errorf("readyz body does not name the failing dependency: %s", raw)
	}

	// ...and must not describe it. Probes bypass the edge (no auth) and
	// nothing guarantees the route is cluster-internal.
	if strings.Contains(raw, "s3cr3t") || strings.Contains(raw, "10.0.0.5") || strings.Contains(raw, "dsn=") {
		t.Errorf("readyz leaked connection detail to an anonymous caller: %s", raw)
	}

	// LIVENESS is a different question. A kubelet that restarts every
	// replica because a shared database blinked turns a dependency incident
	// into a cluster-wide crash loop.
	hStatus, _, hRaw := getJSON(t, "http://"+addr+"/healthz")
	if hStatus != http.StatusOK {
		t.Errorf("healthz = %d with the database down, want 200 — liveness must not follow readiness", hStatus)
	}
	if strings.TrimSpace(hRaw) != "ok" {
		t.Errorf("healthz body = %q, want \"ok\" (forge doctor asserts this exactly)", strings.TrimSpace(hRaw))
	}

	// Recovery is observed live, not latched.
	dep.down.Store(false)
	if status, _, raw = getJSON(t, "http://"+addr+"/readyz"); status != http.StatusOK {
		t.Errorf("readyz = %d after recovery, want 200 (body %s)", status, raw)
	}

	if err := shutdownAndWait(t, errCh, 10*time.Second); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

// TestReadyz_NoChecksStillReportsBoundListener guards the default: a project
// that registers nothing keeps the old listener-bound semantics rather than
// failing closed and becoming unroutable.
func TestReadyz_NoChecksStillReportsBoundListener(t *testing.T) {
	// Not parallel — sends SIGTERM.
	addr := freeAddr(t)
	errCh, _ := runInBackground(t, baseConfig(addr), serverkit.Server{Handler: emptyHandler()})
	waitReady(t, addr, 2*time.Second)

	status, body, raw := getJSON(t, "http://"+addr+"/readyz")
	if status != http.StatusOK || body["status"] != "ready" {
		t.Fatalf("readyz = %d body %s, want 200/ready", status, raw)
	}

	if err := shutdownAndWait(t, errCh, 10*time.Second); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

// TestReadyz_TimeoutBoundsTheWholeRequest pins the probe-latency contract: a
// wedged dependency must not hold /readyz open past ReadinessTimeout, and
// several wedged dependencies must not multiply it — a kubelet probe has one
// timeoutSeconds, not one per check.
func TestReadyz_TimeoutBoundsTheWholeRequest(t *testing.T) {
	// Not parallel — sends SIGTERM.
	addr := freeAddr(t)

	wedged := func(ctx context.Context) error {
		<-ctx.Done() // never returns on its own
		return ctx.Err()
	}
	srv := serverkit.Server{Handler: emptyHandler()}
	srv.AddReadyCheck(serverkit.ReadyCheck{Name: "queue", Check: wedged})
	srv.AddReadyCheck(serverkit.ReadyCheck{Name: "cache", Check: wedged})

	cfg := baseConfig(addr)
	cfg.ReadinessTimeout = 150 * time.Millisecond
	errCh, _ := runInBackground(t, cfg, srv)

	// waitReady would spin forever here (readyz never returns 200), so poll
	// the liveness probe to know the listener is up.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get("http://" + addr + "/healthz"); err == nil { //nolint:noctx // short-lived probe poll
			_ = resp.Body.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	start := time.Now()
	status, _, raw := getJSON(t, "http://"+addr+"/readyz")
	elapsed := time.Since(start)

	if status != http.StatusServiceUnavailable {
		t.Errorf("readyz = %d with two wedged dependencies, want 503 (body %s)", status, raw)
	}
	// One shared deadline: two wedged checks must cost ~one timeout, not two.
	if elapsed > 2*cfg.ReadinessTimeout {
		t.Errorf("readyz took %s with ReadinessTimeout=%s — the checks are not sharing one deadline",
			elapsed, cfg.ReadinessTimeout)
	}

	if err := shutdownAndWait(t, errCh, 10*time.Second); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestAddReadyCheck_IgnoresIncomplete(t *testing.T) {
	t.Parallel()
	var s serverkit.Server
	s.AddReadyCheck(serverkit.ReadyCheck{Name: "", Check: func(context.Context) error { return nil }})
	s.AddReadyCheck(serverkit.ReadyCheck{Name: "no-func", Check: nil})
	// The composition-root ergonomic: a project with no DATABASE_URL passes
	// a nil pool and must not end up permanently unready over a dependency
	// it does not have.
	s.AddReadyCheck(serverkit.DBReadyCheck("database", nil))
	s.AddReadyCheck(serverkit.ReadyCheck{Name: "good", Check: func(context.Context) error { return nil }})

	if len(s.ReadyChecks) != 1 || s.ReadyChecks[0].Name != "good" {
		t.Errorf("expected only the valid check registered, got %+v", s.ReadyChecks)
	}
}

func TestConfigNormalize_FillsReadinessTimeout(t *testing.T) {
	t.Parallel()
	var cfg serverkit.Config
	cfg.Normalize()
	if cfg.ReadinessTimeout != 2*time.Second {
		t.Errorf("ReadinessTimeout = %s, want 2s", cfg.ReadinessTimeout)
	}
}
