package observe

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
)

// swapDefaultLogger points slog.Default at buf for the test. Not parallel:
// the default logger is process state.
func swapDefaultLogger(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(jsonLogger(buf, slog.LevelDebug))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

func failing(context.Context) error { return errors.New("not found") }

func panicking(context.Context) error { panic("boom") }

// TestComponentChainWithLogger_WritesToTheComponentLogger is the regression
// for a component built with its own logger (a test's discarded one) whose
// decorator still logged every failed call to slog.Default: the default chain,
// and a seam built with nil loggers, write to the logger WithLogger binds.
func TestComponentChainWithLogger_WritesToTheComponentLogger(t *testing.T) {
	var global bytes.Buffer
	swapDefaultLogger(t, &global)

	for name, chain := range map[string]*ComponentChain{
		"default chain": DefaultComponentChain("example.com/store", "store", slog.LevelDebug),
		"seam with nil loggers": NewComponentChain(
			RecoverMiddleware(nil),
			LogMiddleware(nil, slog.LevelDebug),
		),
	} {
		t.Run(name, func(t *testing.T) {
			global.Reset()
			var own bytes.Buffer
			bound := chain.WithMethodIDs(map[string]string{"store.Get": "m_1"}).WithLogger(jsonLogger(&own, slog.LevelDebug))

			_ = bound.Run(context.Background(), "store.Get", failing)
			_ = bound.Run(context.Background(), "store.Get", panicking)

			if global.Len() != 0 {
				t.Errorf("a chain bound to the component's logger wrote to slog.Default:\n%s", global.String())
			}
			if got := records(t, &own, "store.Get"); len(got) != 1 {
				t.Errorf("the failed call wrote %d records to the component logger, want 1:\n%s", len(got), own.String())
			}
			if records(t, &own, "store.Get: panic recovered") == nil {
				t.Errorf("the recovered panic is not on the component logger:\n%s", own.String())
			}

			// The unbound chain is untouched: WithLogger returns a new one.
			own.Reset()
			_ = chain.Run(context.Background(), "store.Get", failing)
			if own.Len() != 0 || global.Len() == 0 {
				t.Errorf("WithLogger changed the chain it was called on (own=%q global=%q)", own.String(), global.String())
			}
		})
	}
}

// TestComponentChainWithLogger_KeepsAnExplicitLogger: a seam that passes a
// logger of its own chose it; WithLogger does not override that choice.
func TestComponentChainWithLogger_KeepsAnExplicitLogger(t *testing.T) {
	var chosen, component bytes.Buffer
	chain := NewComponentChain(
		RecoverMiddleware(jsonLogger(&chosen, slog.LevelDebug)),
		LogMiddleware(jsonLogger(&chosen, slog.LevelDebug), slog.LevelDebug),
	).WithLogger(jsonLogger(&component, slog.LevelDebug))

	_ = chain.Run(context.Background(), "store.Get", failing)
	_ = chain.Run(context.Background(), "store.Get", panicking)

	if component.Len() != 0 {
		t.Errorf("WithLogger overrode a logger the seam chose:\n%s", component.String())
	}
	if records(t, &chosen, "store.Get") == nil || records(t, &chosen, "store.Get: panic recovered") == nil {
		t.Errorf("the seam's own logger is missing the failure or the panic:\n%s", chosen.String())
	}
}

// TestComponentChainWithLogger_NilCases: a nil logger leaves the defaults in
// place, and a nil chain stays a nil pass-through.
func TestComponentChainWithLogger_NilCases(t *testing.T) {
	var global bytes.Buffer
	swapDefaultLogger(t, &global)

	_ = NewComponentChain(LogMiddleware(nil, slog.LevelDebug)).WithLogger(nil).Run(context.Background(), "store.Get", failing)
	if records(t, &global, "store.Get") == nil {
		t.Errorf("WithLogger(nil) must keep slog.Default:\n%s", global.String())
	}

	var nilChain *ComponentChain
	if nilChain.WithLogger(slog.Default()) != nil {
		t.Error("a nil chain must stay nil")
	}
}
