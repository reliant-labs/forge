// File: log_policy.go — which SUCCESSFUL calls get a log record.
//
// The RPC logging interceptor wrote one record per successful call. That is
// an access log, and an access log's volume is the traffic's volume: a
// frontend polling three RPCs every couple of seconds produced 47k
// "rpc completed" lines in twelve hours of one dev stack — 92% of the API
// server's INFO output — and buried every line that said something.
//
// Demoting successes to DEBUG is the obvious fix and the wrong one, twice
// over. Dev stacks run at DEBUG, so it does not quieten the stacks where the
// flood was measured; and production runs at INFO precisely so that "is this
// process doing its job?" is answerable from its logs — a process whose
// successes are invisible looks exactly like a dead one.
//
// So successes are SAMPLED IN TIME, per call name: the first success of a
// name is written, then at most one per window, and each sampled record
// carries `suppressed` — how many successes of that name since the previous
// record were not written. Volume is bounded by the number of distinct names
// rather than by traffic, every active name keeps a heartbeat, and the count
// keeps the rate recoverable from the log alone.
//
// Never sampled:
//   - failures — every one is written with full fields (the policy is not
//     consulted for them at all);
//   - slow successes — at or above the slow threshold, written with
//     slow=true at the default success level, or at the name's own level
//     when that is higher (so turning a poll down to DEBUG never hides the
//     call that hung);
//   - anything, once sampling is switched off with WithSuccessSampling(0),
//     which restores one record per success in the original shape.

package observe

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultSuccessSampleWindow is how often, at most, a successful call
	// of one name is written when nothing else marks it for logging. A
	// minute keeps a per-name heartbeat at the granularity logs are usually
	// queried at, while bounding a 2s poll to 1 record in 30.
	DefaultSuccessSampleWindow = time.Minute

	// DefaultSlowThreshold is the duration at which a successful call is
	// written regardless of sampling. Interactive calls finish well under
	// it; one that does not is exactly the outlier a sample would hide.
	DefaultSlowThreshold = time.Second
)

// LogOption tunes how a logging layer — LoggingInterceptor at the RPC edge,
// LogMiddleware at the in-process component boundary — logs SUCCESSFUL
// calls. Failures are never affected: each one is written, with full fields.
type LogOption func(*logPolicy)

// WithSuccessSampling sets the sampling window: at most one success record
// per call name per window (default DefaultSuccessSampleWindow). A window of
// zero or less disables sampling — every success is written, in the original
// record shape.
func WithSuccessSampling(window time.Duration) LogOption {
	return func(p *logPolicy) { p.window = window }
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
// the "<pkg>.<Method>" operation name. Sampling still applies.
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

// logPolicy decides which successful calls are written, at what level, and
// with which explanatory attribute. Configured once at construction, then
// read concurrently; only the per-name sample slots mutate, atomically.
type logPolicy struct {
	level  slog.Level            // success level when no per-name override exists
	window time.Duration         // sampling window; <= 0 writes every success
	slow   time.Duration         // slow threshold; <= 0 disables the rule
	levels map[string]slog.Level // per-name success levels; read-only after construction
	now    func() time.Time

	slots sync.Map // name -> *sampleSlot
}

func newLogPolicy(level slog.Level, opts []LogOption) *logPolicy {
	p := &logPolicy{
		level:  level,
		window: DefaultSuccessSampleWindow,
		slow:   DefaultSlowThreshold,
		now:    time.Now,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(p)
		}
	}
	return p
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
