package serverkit

import (
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Probes is a process's probe surface as listener Front routes: /healthz,
// /readyz over the process's Drainer and its ready checks, and /metrics. It
// is the root's policy, not a component: one per process, built before the
// components, and served IN FRONT of a listener's edge so a kubelet or a
// scraper reaches it without CORS or credentials.
//
// Ready checks may be added after the /readyz handler is built — a
// composition graph knows which of its nodes report Ready only once the
// root's closure is constructed, after the listener that serves the probe.
type Probes struct {
	// Drainer is the readiness gate and the stop budget: /readyz answers 503
	// before Drainer.MarkReady and once its drain has begun. Required.
	Drainer *Drainer
	// Metrics is the /metrics handler (observe.Setup's); nil serves none.
	Metrics http.Handler
	// Timeout bounds all of one /readyz request's checks together; zero is
	// the serverkit default.
	Timeout time.Duration
	// Logger receives check transitions; nil is slog.Default().
	Logger *slog.Logger

	mu     sync.RWMutex
	checks []ReadyCheck
	front  http.HandlerFunc
	once   sync.Once
}

// AddReadyCheck adds dependency checks to /readyz. Checks with no Name or no
// Check are dropped. Safe while /readyz is serving.
func (p *Probes) AddReadyCheck(checks ...ReadyCheck) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range checks {
		if c.Name == "" || c.Check == nil {
			continue
		}
		p.checks = append(p.checks, c)
	}
}

// ReadyChecks returns the checks /readyz runs.
func (p *Probes) ReadyChecks() []ReadyCheck {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]ReadyCheck(nil), p.checks...)
}

// Front returns the probe routes, for ListenerDeps.Front: /healthz,
// /readyz and, when Metrics is set, /metrics.
func (p *Probes) Front() map[string]http.Handler {
	front := map[string]http.Handler{
		"/healthz": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok\n")
		}),
		"/readyz": p.readyz(),
	}
	if p.Metrics != nil {
		front["/metrics"] = p.Metrics
	}
	return front
}

// readyz is the /readyz handler: one per Probes, so the per-check
// transition logging sees every request.
func (p *Probes) readyz() http.HandlerFunc {
	p.once.Do(func() {
		timeout := p.Timeout
		if timeout <= 0 {
			timeout = defaultReadinessTimeout
		}
		logger := p.Logger
		if logger == nil {
			logger = slog.Default()
		}
		drainer := p.Drainer
		if drainer == nil {
			drainer = NewDrainer(Config{})
		}
		p.front = readyzHandlerFunc(drainer, p.ReadyChecks, timeout, logger)
	})
	return p.front
}
