package serverkit

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

// boot.go — probes that answer BEFORE the server is composed.
//
// The generated serve pipeline connects to the database (AutoMigrate, then
// OpenInfra) before it can build the handler Run serves, and Run is what
// binds the listener. So with a database that is not up yet, nothing listens
// and the pod looks identical to a crashed one. StartBootProbes closes that
// window: it binds Config.Addr at the top of boot and answers
//
//	/healthz  200 "ok"        — liveness: the process is alive and booting
//	/readyz   503             — not serving yet; names every dependency
//	                            currently being waited on
//
// Run takes the address over from it (see BootProbes.release) just before
// binding its own listener, so the handoff is invisible to a kubelet probe
// that tolerates a refused connection, which every default probe does.

type bootKey struct{}

// BootProbes is the early probe listener. The zero value is not usable; use
// StartBootProbes. A nil *BootProbes is valid and inert, so callers need no
// guard.
type BootProbes struct {
	srv    *http.Server
	logger *slog.Logger
	addr   string // the Config.Addr bound, which a Listener on it takes over

	mu      sync.Mutex
	pending map[string]struct{}
	closed  bool
}

// StartBootProbes binds cfg.Addr and serves the boot probes. It returns ctx
// carrying the probes: pass THAT ctx to OpenDB / RetryConnect / AutoMigrate
// (which then report the dependency they wait on) and to Run (which takes the
// listener over). Close it on error paths; it is idempotent.
func StartBootProbes(ctx context.Context, cfg Config, logger *slog.Logger) (context.Context, *BootProbes, error) {
	cfg.Normalize()
	if cfg.Addr == "" {
		return ctx, nil, fmt.Errorf("serverkit.StartBootProbes: Config.Addr is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Addr)
	if err != nil {
		return ctx, nil, fmt.Errorf("listen %s: %w", cfg.Addr, err)
	}
	b := &BootProbes{logger: logger, addr: cfg.Addr, pending: map[string]struct{}{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		resp := readyzResponse{Status: "not_ready"}
		for _, name := range b.pendingNames() {
			resp.Checks = append(resp.Checks, readyzCheckStatus{Name: name, OK: false})
		}
		writeReadyz(w, http.StatusServiceUnavailable, resp)
	})
	b.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = b.srv.Serve(ln) }()
	logger.InfoContext(ctx, "boot probes listening — /readyz fails until the server is composed", "addr", cfg.Addr)
	return context.WithValue(ctx, bootKey{}, b), b, nil
}

// BootProbesFrom returns the probes StartBootProbes put on ctx, or nil.
func BootProbesFrom(ctx context.Context) *BootProbes {
	b, _ := ctx.Value(bootKey{}).(*BootProbes)
	return b
}

// Close stops the boot listener. Idempotent; safe on nil.
func (b *BootProbes) Close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	b.mu.Unlock()
	_ = b.srv.Close()
}

// release frees the address for Run's listener. Close, not Shutdown: probe
// connections are one-shot and waiting on them only widens the gap.
func (b *BootProbes) release() { b.Close() }

func (b *BootProbes) pend(name string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.pending[name] = struct{}{}
	b.mu.Unlock()
}

func (b *BootProbes) clear(name string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	delete(b.pending, name)
	b.mu.Unlock()
}

func (b *BootProbes) pendingNames() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	names := make([]string, 0, len(b.pending))
	for n := range b.pending {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
