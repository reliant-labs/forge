// File: log_policy.go — which SUCCESSFUL calls get a log record.
//
// By default, every one. The logging layers — LoggingInterceptor at the RPC
// edge, LogMiddleware at the in-process component boundary — write a record
// for every failure AND every success, so an app that configures nothing
// sees each call it served. That is what a developer reading a dev stack
// expects, and what makes "is this process doing its job?" answerable from
// a production log.
//
// SAMPLING IS OPT-IN, for the deployment where an access log's volume — the
// traffic's volume — outgrows its value: a frontend polling three RPCs every
// couple of seconds produced 47k "rpc completed" lines in twelve hours of one
// dev stack, 92% of the API server's INFO output. Turned on, successes are
// SAMPLED IN TIME, per call name: the first success of a name is written,
// then at most one per window, and each sampled record carries `suppressed`
// — how many successes of that name since the previous record were not
// written. Volume is then bounded by the number of distinct names rather
// than by traffic, every active name keeps a heartbeat, and the count keeps
// the rate recoverable from the log alone. A sampling RATE (log 1 in N) was
// the alternative and is the weaker knob: its volume still grows with
// traffic, so the setting that tamed a poll today floods again at ten times
// the users, and a probabilistic rate can hide a rarely-called name
// entirely.
//
// # Where the window comes from — precedence
//
//  1. CODE: WithSuccessSampling(window), passed to LoggingInterceptor /
//     LogMiddleware directly or through Deps.LogOptions /
//     DefaultMiddlewareDeps.LogOptions. It wins, for the layer it is passed
//     to: a window set in code is a deliberate decision about one layer (a
//     test, an audit-relevant seam that must keep every record), and a
//     process-wide deploy setting must not silently overturn it. The same
//     rule OpenTelemetry applies to its OTEL_* variables.
//  2. CONFIG: the SuccessSampleWindowEnv environment variable, a Go duration
//     ("1m"). It is the process-wide default for every layer that sets no
//     window in code. A forge app declares it in proto/config, sets it per
//     environment in deploy/kcl/<env>/config.k, and the generated
//     config_gen.k projects it onto the workload's env.
//  3. NOTHING SET: no sampling — every success is written.
//
// A window of zero or less, at any level, means "write every success".
//
// # Why this one setting is read from the environment
//
// forge/pkg otherwise never reads the ambient environment: a library changes
// behaviour through its arguments, and the app owns where they come from.
// This is the deliberate exception, because the call sites that build these
// layers are OWNED code that forge never regenerates — the cmd's serve.go
// (Chain / DefaultMiddlewares), every package's observe_chain.go seam
// (LogMiddleware), and servers that are not forge-scaffolded at all. An
// argument would reach none of them without editing each one; the
// environment reaches all of them, so a deploy can turn sampling on or off
// for a whole process without a code change. It is read once, when a layer
// is built.
//
// # Never sampled
//
//   - failures — every one is written with full fields (the policy is not
//     consulted for them at all);
//   - slow successes — at or above the slow threshold, written with
//     slow=true at the default success level, or at the name's own level
//     when that is higher (so turning a poll down to DEBUG never hides the
//     call that hung).

package observe

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// SuccessSampleWindowEnv names the environment variable that turns on
	// success sampling for every logging layer in the process that sets no
	// window in code (see WithSuccessSampling). Its value is a Go duration —
	// at most one success record per call name per window, e.g. "1m".
	// Unset, empty, zero or negative writes every success. An unparseable
	// value is reported once, as a WARN, and also writes every success: a
	// typo costs log volume, never the records that show a process working.
	SuccessSampleWindowEnv = "LOG_SUCCESS_SAMPLE_WINDOW"

	// DefaultSlowThreshold is the duration at which a successful call is
	// written regardless of sampling. Interactive calls finish well under
	// it; one that does not is exactly the outlier a sample would hide.
	DefaultSlowThreshold = time.Second
)

// LogOption tunes how a logging layer — LoggingInterceptor at the RPC edge,
// LogMiddleware at the in-process component boundary — logs SUCCESSFUL
// calls. Failures are never affected: each one is written, with full fields.
type LogOption func(*logPolicy)

// WithSuccessSampling sets the sampling window for one layer: at most one
// success record per call name per window, carrying `suppressed`. A window
// of zero or less writes every success.
//
// It overrides SuccessSampleWindowEnv for the layer it is passed to — set
// it only where that layer must not follow the deployment's setting.
// Without it, the layer takes its window from the environment, and with no
// environment value it writes every success.
func WithSuccessSampling(window time.Duration) LogOption {
	return func(p *logPolicy) { p.window, p.windowSet = window, true }
}

// WithSlowThreshold sets the duration at or above which a successful call is
// always written, with slow=true (default DefaultSlowThreshold). Zero or less
// disables the rule. It applies to unary calls only: a stream's duration is
// its session length, not its latency.
func WithSlowThreshold(threshold time.Duration) LogOption {
	return func(p *logPolicy) { p.slow = threshold }
}

// WithSuccessLevel sets the level of ONE call name's success records,
// overriding the layer's default (INFO at the RPC edge; the seam's level for
// LogMiddleware). For LoggingInterceptor the name is the full procedure,
// "/pkg.v1.Service/Method" — the generated
// <pkg>connect.<Service><Method>Procedure constant; for LogMiddleware it is
// the "<pkg>.<Method>" operation name. Sampling, when on, still applies.
//
// slog.LevelDebug is how to silence a known-noisy procedure (a poll, a health
// check) under an INFO handler; a higher level makes an important one stand
// out. Failures and slow calls of that name are logged as before.
func WithSuccessLevel(name string, level slog.Level) LogOption {
	return func(p *logPolicy) {
		if p.levels == nil {
			p.levels = map[string]slog.Level{}
		}
		p.levels[name] = level
	}
}

// withClock replaces the time source for timing and sampling. Test seam.
func withClock(now func() time.Time) LogOption {
	return func(p *logPolicy) { p.now = now }
}

// withGetenv replaces the environment lookup the window is resolved through.
// Test seam: it lets a parallel test set the "environment" of one layer.
func withGetenv(getenv func(string) string) LogOption {
	return func(p *logPolicy) { p.getenv = getenv }
}

// logPolicy decides which successful calls are written, at what level, and
// with which explanatory attribute. Configured once at construction, then
// read concurrently; only the per-name sample slots mutate, atomically.
type logPolicy struct {
	level     slog.Level            // success level when no per-name override exists
	window    time.Duration         // sampling window; <= 0 writes every success
	windowSet bool                  // window came from code, so the environment is not consulted
	slow      time.Duration         // slow threshold; <= 0 disables the rule
	levels    map[string]slog.Level // per-name success levels; read-only after construction
	now       func() time.Time
	getenv    func(string) string

	slots sync.Map // name -> *sampleSlot
}

// newLogPolicy builds a layer's policy: the code options first, then — only
// when they set no window — the window from SuccessSampleWindowEnv. logger
// receives the warning for an unparseable value; nil means slog.Default.
func newLogPolicy(level slog.Level, logger *slog.Logger, opts []LogOption) *logPolicy {
	p := &logPolicy{
		level:  level,
		slow:   DefaultSlowThreshold,
		now:    time.Now,
		getenv: os.Getenv,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(p)
		}
	}
	if !p.windowSet {
		p.window = sampleWindowFromEnv(p.getenv, logger)
	}
	return p
}

// warnedSampleWindows holds each unparseable SuccessSampleWindowEnv value
// already reported, so a process that builds a dozen logging layers warns
// once rather than a dozen times.
var warnedSampleWindows sync.Map

// sampleWindowFromEnv resolves SuccessSampleWindowEnv to a window. Unset or
// empty is no sampling; so is an unparseable value, which is reported.
func sampleWindowFromEnv(getenv func(string) string, logger *slog.Logger) time.Duration {
	raw := strings.TrimSpace(getenv(SuccessSampleWindowEnv))
	if raw == "" {
		return 0
	}
	window, err := time.ParseDuration(raw)
	if err != nil {
		if _, reported := warnedSampleWindows.LoadOrStore(raw, struct{}{}); !reported {
			if logger == nil {
				logger = slog.Default()
			}
			logger.Warn("observe: ignoring invalid "+SuccessSampleWindowEnv+"; every successful call will be logged",
				slog.String("value", raw), slog.String("want", "a Go duration such as 1m, or 0 to log every success"),
				slog.Any("error", err))
		}
		return 0
	}
	return window
}

// success decides how a successful call named name, which took elapsed, is
// logged. ok=false means write nothing. why, when its Key is non-empty, is
// the attribute explaining the record: slow=true, or suppressed=<n> on a
// sampled record. slowApplies is false for streams.
func (p *logPolicy) success(ctx context.Context, logger *slog.Logger, name string, elapsed time.Duration, slowApplies bool) (level slog.Level, why slog.Attr, ok bool) {
	level = p.level
	if l, found := p.levels[name]; found {
		level = l
	}
	if slowApplies && p.slow > 0 && elapsed >= p.slow {
		level = max(level, p.level)
		return level, slog.Bool("slow", true), logger.Enabled(ctx, level)
	}
	// Ask the handler before sampling: a record it would drop must not
	// consume the name's slot, and the common production case — DEBUG
	// successes under an INFO handler — then costs no synchronisation.
	if !logger.Enabled(ctx, level) {
		return level, slog.Attr{}, false
	}
	if p.window <= 0 {
		return level, slog.Attr{}, true
	}
	suppressed, admitted := p.admit(name)
	if !admitted {
		return level, slog.Attr{}, false
	}
	return level, slog.Int64("suppressed", suppressed), true
}

// sampleSlot is one name's sampling state.
type sampleSlot struct {
	last       atomic.Int64 // UnixNano of the last record written; 0 = none yet
	suppressed atomic.Int64 // successes not written since then
}

// admit reports whether a success of name opens a new window, and if so how
// many successes the previous window suppressed. Every success is either
// admitted or counted exactly once: a racing caller that loses the CAS adds
// itself to the count, which the winner's Swap or the next window reports.
func (p *logPolicy) admit(name string) (suppressed int64, admitted bool) {
	v, found := p.slots.Load(name)
	if !found {
		v, _ = p.slots.LoadOrStore(name, new(sampleSlot))
	}
	slot := v.(*sampleSlot)
	now := p.now().UnixNano()
	last := slot.last.Load()
	if (last == 0 || now-last >= int64(p.window)) && slot.last.CompareAndSwap(last, now) {
		return slot.suppressed.Swap(0), true
	}
	slot.suppressed.Add(1)
	return 0, false
}
