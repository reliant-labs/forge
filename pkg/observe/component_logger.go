package observe

import "log/slog"

// componentLoggerBinder is a middleware that writes to a logger its
// constructor did not pin: WithLogger hands it the component's.
type componentLoggerBinder interface {
	withComponentLogger(logger *slog.Logger) ComponentMiddleware
}

// WithLogger returns an independent chain whose standard middlewares that were
// not given a logger of their own write to logger: RecoverMiddleware(nil),
// LogMiddleware(nil, …), and both of DefaultComponentChain's. A middleware
// constructed with an explicit logger keeps it, and every other middleware is
// shared unchanged, as are the chain's method IDs.
//
// The generated decorator calls it with the component's Deps.Logger, so a
// component built with its own logger — a test's discarded one, a replica's
// tagged one — logs its calls there rather than to the process default.
// A nil logger returns an equivalent chain; a nil chain stays nil.
func (c *ComponentChain) WithLogger(logger *slog.Logger) *ComponentChain {
	if c == nil {
		return nil
	}
	mw := make([]ComponentMiddleware, len(c.mw))
	for i, m := range c.mw {
		if b, ok := m.(componentLoggerBinder); ok && logger != nil {
			m = b.withComponentLogger(logger)
		}
		mw[i] = m
	}
	return &ComponentChain{mw: mw, methodIDs: c.methodIDs}
}

func (m recoverMiddleware) withComponentLogger(logger *slog.Logger) ComponentMiddleware {
	if !m.componentDefault {
		return m
	}
	m.logger = logger
	return m
}

// withComponentLogger keeps the policy: the rebound middleware is the same
// layer (its sampling windows included), writing elsewhere.
func (m *logMiddleware) withComponentLogger(logger *slog.Logger) ComponentMiddleware {
	if !m.componentDefault {
		return m
	}
	return &logMiddleware{logger: logger, policy: m.policy, componentDefault: true}
}
