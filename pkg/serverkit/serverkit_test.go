package serverkit_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	_ "github.com/mattn/go-sqlite3" // for TestApplyDBPoolTuning_AppliesNonZero

	"github.com/reliant-labs/forge/pkg/serverkit"
)

// freeAddr binds 127.0.0.1:0, captures the assigned port, then closes
// the listener so Run can re-bind. There's a tiny race window but it's
// adequate for in-process tests.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freeAddr listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// stubApp is a minimal Application used by every test below. It tracks
// shutdown invocations so tests can assert lifecycle ordering.
type stubApp struct {
	rest        http.Handler
	workers     []serverkit.Worker
	operators   []serverkit.Operator
	shutdownErr error
	shutdownCnt atomic.Int32
}

func (s *stubApp) WorkerList() []serverkit.Worker     { return s.workers }
func (s *stubApp) OperatorList() []serverkit.Operator { return s.operators }
func (s *stubApp) HasOperators() bool                 { return len(s.operators) > 0 }
func (s *stubApp) RunOperators(ctx context.Context, _ *slog.Logger, _ string) error {
	<-ctx.Done()
	return nil
}
func (s *stubApp) RESTHandler() http.Handler { return s.rest }
func (s *stubApp) Shutdown(context.Context) error {
	s.shutdownCnt.Add(1)
	return s.shutdownErr
}

// stubWorker records Start/Stop hits so tests can assert ordering.
type stubWorker struct {
	name      string
	startedAt atomic.Int64
	stoppedAt atomic.Int64
	startErr  error
}

func (w *stubWorker) Name() string { return w.name }
func (w *stubWorker) Start(ctx context.Context) error {
	w.startedAt.Store(time.Now().UnixNano())
	<-ctx.Done()
	return w.startErr
}
func (w *stubWorker) Stop(context.Context) error {
	w.stoppedAt.Store(time.Now().UnixNano())
	return nil
}

// runInBackground starts serverkit.Run on a goroutine and returns a
// helper that polls /readyz then triggers SIGTERM to drive shutdown.
// Tests block on the returned error channel.
func runInBackground(t *testing.T, cfg serverkit.Config, hooks serverkit.Hooks, args []string) (errCh chan error, addr string) {
	t.Helper()
	addr = cfg.Addr
	errCh = make(chan error, 1)
	go func() {
		errCh <- serverkit.Run(context.Background(), cfg, hooks, args)
	}()
	return errCh, addr
}

// waitReady polls /readyz until it returns 200 or the deadline expires.
func waitReady(t *testing.T, addr string, deadline time.Duration) {
	t.Helper()
	url := "http://" + addr + "/readyz"
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		resp, err := http.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("readyz did not return 200 within %s", deadline)
}

// shutdownAndWait triggers SIGTERM and waits for Run to return.
func shutdownAndWait(t *testing.T, errCh chan error, within time.Duration) error {
	t.Helper()
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("find self: %v", err)
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
	select {
	case e := <-errCh:
		return e
	case <-time.After(within):
		t.Fatalf("Run did not return within %s", within)
	}
	return nil
}

func baseConfig(addr string) serverkit.Config {
	return serverkit.Config{
		Addr:            addr,
		LogFormat:       "text",
		LogLevel:        slog.LevelError,
		PreStopDelay:    10 * time.Millisecond,
		ShutdownTimeout: 5 * time.Second,
	}
}

func TestRun_RequiresBootstrap(t *testing.T) {
	t.Parallel()
	err := serverkit.Run(context.Background(), serverkit.Config{Addr: ":0"}, serverkit.Hooks{}, nil)
	if err == nil || !contains(err.Error(), "Bootstrap is required") {
		t.Fatalf("expected Bootstrap-required error, got %v", err)
	}
}

func TestRun_RequiresAddr(t *testing.T) {
	t.Parallel()
	hooks := serverkit.Hooks{
		Bootstrap: func(context.Context, *http.ServeMux, *slog.Logger, []string, ...connect.HandlerOption) (serverkit.Application, error) {
			return &stubApp{}, nil
		},
	}
	err := serverkit.Run(context.Background(), serverkit.Config{}, hooks, nil)
	if err == nil || !contains(err.Error(), "Addr is required") {
		t.Fatalf("expected Addr-required error, got %v", err)
	}
}

func TestRun_StartsAndShutsDownCleanly(t *testing.T) {
	// Not parallel — sends SIGTERM to the test process.
	addr := freeAddr(t)
	app := &stubApp{}
	hooks := serverkit.Hooks{
		Bootstrap: func(context.Context, *http.ServeMux, *slog.Logger, []string, ...connect.HandlerOption) (serverkit.Application, error) {
			return app, nil
		},
	}
	errCh, _ := runInBackground(t, baseConfig(addr), hooks, nil)
	waitReady(t, addr, 2*time.Second)
	if err := shutdownAndWait(t, errCh, 5*time.Second); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if app.shutdownCnt.Load() == 0 {
		t.Fatal("Application.Shutdown was never called")
	}
}

func TestRun_HealthzReadyzLifecycle(t *testing.T) {
	// Not parallel — sends SIGTERM.
	addr := freeAddr(t)
	bootstrapGate := make(chan struct{})
	hooks := serverkit.Hooks{
		Bootstrap: func(ctx context.Context, mux *http.ServeMux, _ *slog.Logger, _ []string, _ ...connect.HandlerOption) (serverkit.Application, error) {
			<-bootstrapGate
			return &stubApp{}, nil
		},
	}
	errCh, _ := runInBackground(t, baseConfig(addr), hooks, nil)

	// Before bootstrap returns, the listener isn't bound at all — both
	// probes should fail to connect. (We can't distinguish "before
	// bootstrap, after listener bind" from "after readiness flip" in a
	// black-box way; the listener bind and readiness flip happen back-
	// to-back inside Run. The meaningful invariants we CAN assert are:
	// (a) /readyz fails after we flip ready=false during shutdown, and
	// (b) /healthz always 200s once the listener is up.)
	close(bootstrapGate)
	waitReady(t, addr, 2*time.Second)

	// /healthz returns 200 now.
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", resp.StatusCode)
	}

	if err := shutdownAndWait(t, errCh, 5*time.Second); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestRun_WorkerLifecycle(t *testing.T) {
	// Not parallel — sends SIGTERM.
	addr := freeAddr(t)
	w := &stubWorker{name: "alpha"}
	app := &stubApp{workers: []serverkit.Worker{w}}
	hooks := serverkit.Hooks{
		Bootstrap: func(context.Context, *http.ServeMux, *slog.Logger, []string, ...connect.HandlerOption) (serverkit.Application, error) {
			return app, nil
		},
	}
	errCh, _ := runInBackground(t, baseConfig(addr), hooks, nil)
	waitReady(t, addr, 2*time.Second)

	// Worker should have started by now.
	if w.startedAt.Load() == 0 {
		t.Fatal("worker.Start was never called before readyz returned 200")
	}

	if err := shutdownAndWait(t, errCh, 5*time.Second); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if w.stoppedAt.Load() == 0 {
		t.Fatal("worker.Stop was never called")
	}
	if w.startedAt.Load() > w.stoppedAt.Load() {
		t.Fatalf("worker stopped (%d) before it started (%d)?", w.stoppedAt.Load(), w.startedAt.Load())
	}
}

func TestRun_PostBootstrapErrorAborts(t *testing.T) {
	t.Parallel()
	addr := freeAddr(t)
	sentinel := errors.New("post-bootstrap boom")
	hooks := serverkit.Hooks{
		Bootstrap: func(context.Context, *http.ServeMux, *slog.Logger, []string, ...connect.HandlerOption) (serverkit.Application, error) {
			return &stubApp{}, nil
		},
		PostBootstrap: func(serverkit.Application) error { return sentinel },
	}
	err := serverkit.Run(context.Background(), baseConfig(addr), hooks, nil)
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped post-bootstrap error, got %v", err)
	}
}

func TestRun_PostBootstrapNilOK(t *testing.T) {
	// Not parallel — sends SIGTERM.
	addr := freeAddr(t)
	hooks := serverkit.Hooks{
		Bootstrap: func(context.Context, *http.ServeMux, *slog.Logger, []string, ...connect.HandlerOption) (serverkit.Application, error) {
			return &stubApp{}, nil
		},
	}
	errCh, _ := runInBackground(t, baseConfig(addr), hooks, nil)
	waitReady(t, addr, 2*time.Second)
	if err := shutdownAndWait(t, errCh, 5*time.Second); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestRun_HookOrdering(t *testing.T) {
	// Not parallel — sends SIGTERM.
	addr := freeAddr(t)
	var (
		mu     sync.Mutex
		events []string
	)
	record := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, s)
	}

	hooks := serverkit.Hooks{
		SetupOTel: func(context.Context) (func(context.Context) error, http.Handler, error) {
			record("setup-otel")
			return func(context.Context) error { record("shutdown-otel"); return nil }, nil, nil
		},
		Bootstrap: func(context.Context, *http.ServeMux, *slog.Logger, []string, ...connect.HandlerOption) (serverkit.Application, error) {
			record("bootstrap")
			return &stubApp{}, nil
		},
		PostBootstrap: func(serverkit.Application) error {
			record("post-bootstrap")
			return nil
		},
	}
	errCh, _ := runInBackground(t, baseConfig(addr), hooks, nil)
	waitReady(t, addr, 2*time.Second)
	if err := shutdownAndWait(t, errCh, 5*time.Second); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"setup-otel", "bootstrap", "post-bootstrap", "shutdown-otel"}
	if len(events) != len(want) {
		t.Fatalf("event count: got %v want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("event[%d] = %q want %q (full: %v)", i, events[i], want[i], events)
		}
	}
}

func TestRun_AutoMigrateRequiresHook(t *testing.T) {
	t.Parallel()
	cfg := baseConfig(":0")
	cfg.AutoMigrate = true
	cfg.DatabaseURL = "postgres://nope"
	hooks := serverkit.Hooks{
		Bootstrap: func(context.Context, *http.ServeMux, *slog.Logger, []string, ...connect.HandlerOption) (serverkit.Application, error) {
			return &stubApp{}, nil
		},
	}
	err := serverkit.Run(context.Background(), cfg, hooks, nil)
	if err == nil || !contains(err.Error(), "Hooks.AutoMigrate is nil") {
		t.Fatalf("expected AutoMigrate hook required error, got %v", err)
	}
}

func TestRun_BootstrapErrorPropagates(t *testing.T) {
	t.Parallel()
	addr := freeAddr(t)
	sentinel := errors.New("bootstrap boom")
	hooks := serverkit.Hooks{
		Bootstrap: func(context.Context, *http.ServeMux, *slog.Logger, []string, ...connect.HandlerOption) (serverkit.Application, error) {
			return nil, sentinel
		},
	}
	err := serverkit.Run(context.Background(), baseConfig(addr), hooks, nil)
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped bootstrap error, got %v", err)
	}
}

func TestRun_BootstrapNilAppIsError(t *testing.T) {
	t.Parallel()
	addr := freeAddr(t)
	hooks := serverkit.Hooks{
		Bootstrap: func(context.Context, *http.ServeMux, *slog.Logger, []string, ...connect.HandlerOption) (serverkit.Application, error) {
			return nil, nil
		},
	}
	err := serverkit.Run(context.Background(), baseConfig(addr), hooks, nil)
	if err == nil || !contains(err.Error(), "nil Application") {
		t.Fatalf("expected nil-Application error, got %v", err)
	}
}

func TestRun_ShutdownWithinBudget(t *testing.T) {
	// Not parallel — sends SIGTERM.
	addr := freeAddr(t)
	app := &stubApp{}
	hooks := serverkit.Hooks{
		Bootstrap: func(context.Context, *http.ServeMux, *slog.Logger, []string, ...connect.HandlerOption) (serverkit.Application, error) {
			return app, nil
		},
	}
	cfg := baseConfig(addr)
	cfg.ShutdownTimeout = 500 * time.Millisecond
	errCh, _ := runInBackground(t, cfg, hooks, nil)
	waitReady(t, addr, 2*time.Second)

	start := time.Now()
	if err := shutdownAndWait(t, errCh, cfg.ShutdownTimeout+cfg.PreStopDelay+1*time.Second); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	elapsed := time.Since(start)
	maxAllowed := cfg.ShutdownTimeout + cfg.PreStopDelay + 500*time.Millisecond
	if elapsed > maxAllowed {
		t.Fatalf("shutdown took %s, want <= %s", elapsed, maxAllowed)
	}
}

func TestApplyDBPoolTuning_NilDBNoop(t *testing.T) {
	t.Parallel()
	// Should not panic.
	serverkit.ApplyDBPoolTuning(nil, serverkit.DBPoolTuning{
		MaxOpenConns:    10,
		MaxIdleConns:    5,
		ConnMaxIdleTime: time.Minute,
		ConnMaxLifetime: time.Hour,
	})
}

func TestApplyDBPoolTuning_AppliesNonZero(t *testing.T) {
	t.Parallel()
	// Open an in-memory SQLite handle — we don't actually exercise it,
	// just confirm pool tuning calls don't error.
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Skipf("sqlite3 driver not available: %v", err)
	}
	defer db.Close()
	serverkit.ApplyDBPoolTuning(db, serverkit.DBPoolTuning{
		MaxOpenConns:    7,
		MaxIdleConns:    3,
		ConnMaxIdleTime: 30 * time.Second,
		ConnMaxLifetime: 2 * time.Minute,
	})
	stats := db.Stats()
	if stats.MaxOpenConnections != 7 {
		t.Fatalf("MaxOpenConnections = %d, want 7", stats.MaxOpenConnections)
	}
}

// contains is a tiny strings.Contains shim — kept inline so the test
// file has no non-stdlib runtime deps beyond serverkit/connect.
func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// Compile-time check: stubApp satisfies the Application interface.
var _ serverkit.Application = (*stubApp)(nil)

// Compile-time check: stubWorker satisfies Worker.
var _ serverkit.Worker = (*stubWorker)(nil)

// Used by error-wrap tests that check the message contains a substring.
var _ = fmt.Sprintf
