package observe

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/reliant-labs/forge/pkg/svcerr"
)

// log_sampling_config_test.go — success sampling is OFF until configured.
//
// Sampling successful-call logs used to be the default, at both logging
// layers. The decision since: make it configurable, default to logging
// every success (particularly in dev), and let a deployment lower it. These
// tests pin that through the constructors apps actually call — none of
// which sets a window in code — so what they observe is what an app that
// configures nothing, or configures only its environment, gets:
//
//   - observe.DefaultMiddlewares — reliant's server;
//   - observe.Chain — the scaffolded cmd serve.go;
//   - observe.LogMiddleware — every package's scaffolded observe_chain.go.
//
// They read the REAL process environment (t.Setenv), so they cannot run in
// parallel; the per-layer seam tests in log_sampling_test.go use withGetenv.

type callOutcome int

const (
	outcomeSuccess callOutcome = iota
	outcomeFailure
	outcomeSlowSuccess // succeeds after DefaultSlowThreshold on the fake clock
)

// loggingLayer is one way an app builds a logging layer. build returns a
// function making one call with the given outcome; opts are appended to the
// layer's options exactly where an app would pass them.
type loggingLayer struct {
	name       string
	successMsg string // the msg of a success record
	failureMsg string // the msg of a failure record
	build      func(t *testing.T, buf *bytes.Buffer, clock *fakeClock, opts ...LogOption) func(callOutcome)
}

var loggingLayers = []loggingLayer{
	{
		name:       "DefaultMiddlewares",
		successMsg: "rpc completed",
		failureMsg: "rpc failed",
		build: func(t *testing.T, buf *bytes.Buffer, clock *fakeClock, opts ...LogOption) func(callOutcome) {
			return edgeCaller(t, clock, DefaultMiddlewares(DefaultMiddlewareDeps{
				Logger:     jsonLogger(buf, slog.LevelDebug),
				LogOptions: append([]LogOption{withClock(clock.Now)}, opts...),
			}))
		},
	},
	{
		name:       "Chain",
		successMsg: "rpc completed",
		failureMsg: "rpc failed",
		build: func(t *testing.T, buf *bytes.Buffer, clock *fakeClock, opts ...LogOption) func(callOutcome) {
			return edgeCaller(t, clock, Chain(Deps{
				Logger:     jsonLogger(buf, slog.LevelDebug),
				LogOptions: append([]LogOption{withClock(clock.Now)}, opts...),
			}))
		},
	},
	{
		name:       "LogMiddleware",
		successMsg: loopMethod,
		failureMsg: loopMethod,
		build: func(_ *testing.T, buf *bytes.Buffer, clock *fakeClock, opts ...LogOption) func(callOutcome) {
			// The scaffolded seam's shape: a Debug success level under a
			// Debug handler, as in every dev stack.
			mw := LogMiddleware(jsonLogger(buf, slog.LevelDebug), slog.LevelDebug,
				append([]LogOption{withClock(clock.Now)}, opts...)...)
			chain := NewComponentChain(mw)
			boom := errors.New("relation does not exist")
			return func(o callOutcome) {
				_ = chain.Run(context.Background(), loopMethod, func(context.Context) error {
					return outcomeErr(o, clock, boom)
				})
			}
		},
	},
}

// edgeCaller drives a Connect handler behind interceptors, one call per
// invocation, with the given outcome.
func edgeCaller(t *testing.T, clock *fakeClock, interceptors []connect.Interceptor) func(callOutcome) {
	t.Helper()
	var next callOutcome
	call := rpcEdge(t, interceptors, func(string) error {
		return outcomeErr(next, clock, svcerr.Wrap(svcerr.NotFound("item")))
	})
	return func(o callOutcome) {
		next = o
		call(pollProcedure)
	}
}

func outcomeErr(o callOutcome, clock *fakeClock, failure error) error {
	switch o {
	case outcomeFailure:
		return failure
	case outcomeSlowSuccess:
		clock.Advance(DefaultSlowThreshold)
	}
	return nil
}

// logged splits a layer's records into successes (no error) and failures.
func logged(t *testing.T, buf *bytes.Buffer, l loggingLayer) (successes, failures []map[string]any) {
	t.Helper()
	for _, rec := range records(t, buf, l.successMsg) {
		if rec["error"] == nil {
			successes = append(successes, rec)
		}
	}
	for _, rec := range records(t, buf, l.failureMsg) {
		if rec["error"] != nil {
			failures = append(failures, rec)
		}
	}
	return successes, failures
}

// TestSuccessLogging_DefaultLogsEverySuccess: an app that configures
// nothing gets one record per successful call, in the unsampled shape.
func TestSuccessLogging_DefaultLogsEverySuccess(t *testing.T) {
	// Hermetic: whatever the developer's shell exports, this is "unset".
	t.Setenv(SuccessSampleWindowEnv, "")
	for _, l := range loggingLayers {
		t.Run(l.name, func(t *testing.T) {
			var buf bytes.Buffer
			call := l.build(t, &buf, newFakeClock())

			for range 50 {
				call(outcomeSuccess)
			}

			successes, _ := logged(t, &buf, l)
			if len(successes) != 50 {
				t.Fatalf("50 successful calls with no sampling configured wrote %d records, want 50 — "+
					"the default is every success; sampling is opt-in", len(successes))
			}
			for _, rec := range successes {
				if _, sampled := rec["suppressed"]; sampled {
					t.Fatalf("an unsampled record must not carry a sampling count: %v", rec)
				}
			}
		})
	}
}

// TestSuccessLogging_EnvWindowApplies: LOG_SUCCESS_SAMPLE_WINDOW turns
// sampling on for every layer that sets nothing in code, with ITS window —
// here 5m, so a call one minute in is still suppressed.
func TestSuccessLogging_EnvWindowApplies(t *testing.T) {
	t.Setenv(SuccessSampleWindowEnv, "5m")
	for _, l := range loggingLayers {
		t.Run(l.name, func(t *testing.T) {
			var buf bytes.Buffer
			clock := newFakeClock()
			call := l.build(t, &buf, clock)

			for range 10 {
				call(outcomeSuccess)
			}
			if successes, _ := logged(t, &buf, l); len(successes) != 1 {
				t.Fatalf("10 successes inside one 5m window wrote %d records, want 1", len(successes))
			}

			clock.Advance(time.Minute)
			call(outcomeSuccess)
			if successes, _ := logged(t, &buf, l); len(successes) != 1 {
				t.Fatalf("a success 1m into a 5m window wrote a record (%d total) — the configured "+
					"window was not applied", len(successes))
			}

			clock.Advance(4 * time.Minute)
			call(outcomeSuccess)
			successes, _ := logged(t, &buf, l)
			if len(successes) != 2 {
				t.Fatalf("the first success of the next window must be written: %d records, want 2", len(successes))
			}
			if n, _ := successes[1]["suppressed"].(float64); n != 10 {
				t.Errorf("suppressed = %v, want 10 — the successes the window swallowed", successes[1]["suppressed"])
			}
		})
	}
}

// TestSuccessLogging_FailuresAndSlowCallsAlwaysLogged: whatever the setting
// — default, environment, code, or a value that does not parse — every
// failure and every slow success is written. Only plain successes follow
// the setting.
func TestSuccessLogging_FailuresAndSlowCallsAlwaysLogged(t *testing.T) {
	settings := []struct {
		name    string
		env     string
		opts    []LogOption
		sampled bool // whether plain successes are sampled under this setting
	}{
		{name: "not configured", env: ""},
		{name: "env 0", env: "0"},
		{name: "env 1h", env: "1h", sampled: true},
		{name: "env unparseable", env: "every minute"},
		{name: "code 1h", opts: []LogOption{WithSuccessSampling(time.Hour)}, sampled: true},
		{name: "code 0 over env 1h", env: "1h", opts: []LogOption{WithSuccessSampling(0)}},
	}
	for _, s := range settings {
		t.Run(s.name, func(t *testing.T) {
			t.Setenv(SuccessSampleWindowEnv, s.env)
			for _, l := range loggingLayers {
				t.Run(l.name, func(t *testing.T) {
					var buf bytes.Buffer
					call := l.build(t, &buf, newFakeClock(), s.opts...)

					const rounds = 5
					for range rounds {
						call(outcomeSuccess)
						call(outcomeFailure)
						call(outcomeSlowSuccess)
					}

					successes, failures := logged(t, &buf, l)
					if len(failures) != rounds {
						t.Fatalf("%d failed calls wrote %d failure records, want every one", rounds, len(failures))
					}
					var slow, plain int
					for _, rec := range successes {
						if rec["slow"] == true {
							slow++
						} else {
							plain++
						}
					}
					if slow != rounds {
						t.Fatalf("%d slow successes wrote %d slow=true records, want every one", rounds, slow)
					}
					wantPlain := rounds
					if s.sampled {
						wantPlain = 1 // the first; the rest fall inside the window
					}
					if plain != wantPlain {
						t.Fatalf("plain successes: %d records, want %d", plain, wantPlain)
					}
				})
			}
		})
	}
}

// TestSuccessLogging_CodeWinsOverEnv: a window set in code is a decision
// about that layer, and the deployment's variable does not overturn it — in
// either direction.
func TestSuccessLogging_CodeWinsOverEnv(t *testing.T) {
	cases := []struct {
		name string
		env  string
		opts []LogOption
		want int // records for 5 successes
	}{
		{name: "env samples, code logs every success", env: "1h", opts: []LogOption{WithSuccessSampling(0)}, want: 5},
		{name: "env logs every success, code samples", env: "0", opts: []LogOption{WithSuccessSampling(time.Hour)}, want: 1},
		{name: "env samples, code silent", env: "1h", want: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(SuccessSampleWindowEnv, c.env)
			for _, l := range loggingLayers {
				t.Run(l.name, func(t *testing.T) {
					var buf bytes.Buffer
					call := l.build(t, &buf, newFakeClock(), c.opts...)
					for range 5 {
						call(outcomeSuccess)
					}
					if successes, _ := logged(t, &buf, l); len(successes) != c.want {
						t.Fatalf("got %d success records, want %d", len(successes), c.want)
					}
				})
			}
		})
	}
}
