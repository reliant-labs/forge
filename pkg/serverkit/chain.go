package serverkit

// chain.go — the interceptor chain as a component.
//
// Every request a Connect handler answers passes through the same ordered
// stack: a trace span, panic recovery, a request id, logging, metrics,
// authentication, audit, rate limiting, the stream drain and protovalidate.
// The order is a decision, and it used to be re-derived inline in every
// project's serve.go. NewChain is that stack as a library: every slot has a
// working default except Auth, which a caller must name, so a chain that
// authenticates nobody cannot be built by forgetting a line.
//
// A listener serving a different kind of caller — a daemon, an operator —
// builds a second Chain with its own Authenticator. Each Chain reports the
// audience its Authenticator admits, and a Listener refuses to start when it
// mounts a service declared for another audience (listener.go).

import (
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"

	"github.com/reliant-labs/forge/pkg/authn"
	"github.com/reliant-labs/forge/pkg/middleware"
	"github.com/reliant-labs/forge/pkg/observe"
	"github.com/reliant-labs/forge/pkg/validate"
)

// AudienceUser is the default audience: the product's own signed-in users.
// A proto service that declares no `(forge.v1.service).audience` serves it.
const AudienceUser = "user"

// Authenticator is the authentication slot of a Chain: the interceptor that
// proves who is calling, and the audience — the kind of caller — it admits.
// Audience is what a Listener compares with each mounted service's declared
// `(forge.v1.service).audience`.
type Authenticator interface {
	connect.Interceptor
	// Audience names the trust domain this authenticator admits: AudienceUser
	// ("user"), or a name the project chooses ("daemon", "operator").
	Audience() string
}

// AuthFor pairs an authentication interceptor with the audience it admits —
// the usual way to fill ChainDeps.Auth:
//
//	serverkit.AuthFor(serverkit.AudienceUser, authInterceptor)
//
// A nil interceptor or an empty audience is rejected by NewChain.
func AuthFor(audience string, interceptor connect.Interceptor) Authenticator {
	return &audienceAuth{Interceptor: interceptor, audience: audience}
}

type audienceAuth struct {
	connect.Interceptor
	audience string
}

func (a *audienceAuth) Audience() string { return a.audience }

// Omit drops one defaulted slot from a Chain. A nil slot means "forge's
// default", so leaving a layer out has to be said: ChainDeps{Audit:
// serverkit.NoAudit}. An Omit names its slot, and NewChain rejects one given
// for another slot (NoTrace in Audit).
//
// It satisfies connect.Interceptor only so it fits the slot's type; NewChain
// never installs it.
type Omit string

// The slots a Chain can drop. Auth has none: a chain always authenticates.
const (
	// NoTrace opens no server span for the chain's RPCs.
	NoTrace Omit = "trace"
	// NoAudit writes no audit event for the chain's RPCs.
	NoAudit Omit = "audit"
	// NoStreamDrain leaves server and bidi streams unbound to the listener's
	// drain: a rollout then waits for them for the whole shutdown budget and
	// cuts them without a status.
	NoStreamDrain Omit = "stream-drain"
	// NoValidate skips protovalidate: requests reach the handler with their
	// buf.validate rules unchecked.
	NoValidate Omit = "validate"
)

// WrapUnary is a passthrough; NewChain never installs an Omit.
func (Omit) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc { return next }

// WrapStreamingClient is a passthrough; NewChain never installs an Omit.
func (Omit) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler is a passthrough; NewChain never installs an Omit.
func (Omit) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// DefaultMaxBytes is the payload cap a Chain applies to each request and each
// response when ChainDeps leaves it zero.
const DefaultMaxBytes = 4 << 20

// ChainDeps configures a Chain. Auth is required; every other slot left nil is
// forge's default, and a defaulted slot is dropped only by its Omit constant.
type ChainDeps struct {
	// Auth authenticates every request and names the audience the chain
	// admits. Required: a nil Auth is a NewChain error, so a server with no
	// authenticator fails at boot instead of serving.
	Auth Authenticator

	// Logger feeds recovery, logging and the default audit interceptor. nil
	// is slog.Default().
	Logger *slog.Logger

	// Trace opens the server span; it runs OUTERMOST, so a request auth
	// rejects still has a span and every log line carries its trace_id. nil
	// is otelconnect with WithTrustRemote — an inbound traceparent (the
	// browser's) becomes the span's parent. A chain facing callers that
	// must not choose its trace ids passes its own otelconnect interceptor
	// without that option. NoTrace drops it.
	Trace connect.Interceptor

	// Audit records who called what, after Auth. nil is
	// middleware.AuditInterceptor over the claims Auth stashed. NoAudit
	// drops it.
	Audit connect.Interceptor

	// RateLimit runs after Audit. It has no default — a rate limit is a
	// budget only configuration can name — so nil is none.
	// middleware.RateLimitInterceptor returns nil for a zero rate, so its
	// result can be passed through unconditionally.
	RateLimit connect.Interceptor

	// LogOptions tune how the logging interceptor records successful RPCs
	// (observe.WithSuccessSampling). nil logs every one.
	LogOptions []observe.LogOption

	// Extras are the project's own interceptors, run after RateLimit and
	// before the stream drain and protovalidate.
	Extras []connect.Interceptor

	// StreamDrain binds every server-streaming and bidi RPC to the drain of
	// the listener serving it, so a rollout ends it with a status its client
	// reconnects on (stream_drain.go). nil is StreamDrainInterceptor().
	// NoStreamDrain drops it.
	StreamDrain connect.Interceptor

	// Validate rejects a request whose buf.validate rules fail, with
	// InvalidArgument, right before the handler: it is the innermost layer.
	// nil is protovalidate (pkg/validate). NoValidate drops it.
	Validate connect.Interceptor

	// ReadMaxBytes and SendMaxBytes cap one request's and one response's
	// payload. connect-go reads and decodes a whole body before any
	// interceptor runs, so an uncapped handler turns one anonymous request
	// into an OOMKill. Zero is DefaultMaxBytes (4 MiB); negative is no cap.
	ReadMaxBytes int
	SendMaxBytes int
}

// Chain is a built interceptor chain: the ordered interceptors and payload
// caps every Connect handler mounted with it receives, and the audience its
// Authenticator admits. Build it with NewChain.
type Chain struct {
	audience     string
	interceptors []connect.Interceptor
	options      []connect.HandlerOption
}

// NewChain builds the chain, outermost first: trace → recovery → request-id →
// logging → tracing → metrics → auth → audit → rate-limit → extras →
// stream-drain → protovalidate. Every slot but Auth is defaulted; see
// ChainDeps for each default and how to drop it. It fails when Auth is
// missing or names no audience, when an Omit is given for the wrong slot, and
// when a default cannot be built — a chain missing a layer it was meant to
// have is a boot failure, not a log line.
func NewChain(deps ChainDeps) (*Chain, error) {
	if err := validateChainDeps(deps); err != nil {
		return nil, err
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	trace, err := chainSlot(deps.Trace, NoTrace, func() (connect.Interceptor, error) {
		return otelconnect.NewInterceptor(otelconnect.WithTrustRemote())
	})
	if err != nil {
		return nil, err
	}
	audit, err := chainSlot(deps.Audit, NoAudit, func() (connect.Interceptor, error) {
		return middleware.AuditInterceptor(logger, authn.ClaimsFromContext), nil
	})
	if err != nil {
		return nil, err
	}
	streamDrain, err := chainSlot(deps.StreamDrain, NoStreamDrain, func() (connect.Interceptor, error) {
		return StreamDrainInterceptor(), nil
	})
	if err != nil {
		return nil, err
	}
	validator, err := chainSlot(deps.Validate, NoValidate, validate.Interceptor)
	if err != nil {
		return nil, err
	}

	extras := make([]connect.Interceptor, 0, len(deps.Extras)+2)
	extras = append(extras, deps.Extras...)
	if streamDrain != nil {
		extras = append(extras, streamDrain)
	}
	if validator != nil {
		extras = append(extras, validator)
	}
	interceptors := observe.Chain(observe.Deps{
		Logger:     logger,
		Trace:      trace,
		Auth:       deps.Auth,
		Audit:      audit,
		RateLimit:  deps.RateLimit,
		LogOptions: deps.LogOptions,
		Extras:     extras,
	})
	return &Chain{
		audience:     deps.Auth.Audience(),
		interceptors: interceptors,
		options: []connect.HandlerOption{
			connect.WithInterceptors(interceptors...),
			connect.WithReadMaxBytes(payloadCap(deps.ReadMaxBytes)),
			connect.WithSendMaxBytes(payloadCap(deps.SendMaxBytes)),
		},
	}, nil
}

func validateChainDeps(deps ChainDeps) error {
	var errs []error
	switch {
	case deps.Auth == nil:
		errs = append(errs, errors.New("ChainDeps.Auth is required: a chain authenticates every request (serverkit.AuthFor(serverkit.AudienceUser, interceptor))"))
	case deps.Auth.Audience() == "":
		errs = append(errs, errors.New("ChainDeps.Auth names no audience: serverkit.AuthFor(serverkit.AudienceUser, interceptor) for the default"))
	default:
		if a, ok := deps.Auth.(*audienceAuth); ok && a.Interceptor == nil {
			errs = append(errs, fmt.Errorf("ChainDeps.Auth (audience %q) wraps a nil interceptor", a.audience))
		}
	}
	for _, s := range []struct {
		field string
		value connect.Interceptor
		omit  Omit
	}{
		{"Trace", deps.Trace, NoTrace},
		{"Audit", deps.Audit, NoAudit},
		{"StreamDrain", deps.StreamDrain, NoStreamDrain},
		{"Validate", deps.Validate, NoValidate},
		{"RateLimit", deps.RateLimit, ""},
	} {
		if o, ok := s.value.(Omit); ok && o != s.omit {
			errs = append(errs, fmt.Errorf("ChainDeps.%s is serverkit.Omit(%q), which drops another slot", s.field, string(o)))
		}
	}
	for i, x := range deps.Extras {
		if x == nil {
			errs = append(errs, fmt.Errorf("ChainDeps.Extras[%d] is nil", i))
		} else if _, ok := x.(Omit); ok {
			errs = append(errs, fmt.Errorf("ChainDeps.Extras[%d] is an Omit, which only drops a defaulted slot", i))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("serverkit.NewChain: %w", errors.Join(errs...))
	}
	return nil
}

// chainSlot resolves one defaulted slot: the caller's interceptor, nil for its
// Omit, or the default.
func chainSlot(v connect.Interceptor, omit Omit, def func() (connect.Interceptor, error)) (connect.Interceptor, error) {
	if o, ok := v.(Omit); ok && o == omit {
		return nil, nil
	}
	if v != nil {
		return v, nil
	}
	ic, err := def()
	if err != nil {
		return nil, fmt.Errorf("serverkit.NewChain: build the default %s interceptor: %w (pass serverkit.%s to run without it)", omit, err, omitName(omit))
	}
	return ic, nil
}

func omitName(o Omit) string {
	switch o {
	case NoTrace:
		return "NoTrace"
	case NoAudit:
		return "NoAudit"
	case NoStreamDrain:
		return "NoStreamDrain"
	case NoValidate:
		return "NoValidate"
	}
	return string(o)
}

// payloadCap maps a ChainDeps cap onto connect's, where zero is unlimited.
func payloadCap(n int) int {
	switch {
	case n == 0:
		return DefaultMaxBytes
	case n < 0:
		return 0
	}
	return n
}

// Audience is the audience the chain's Authenticator admits.
func (c *Chain) Audience() string { return c.audience }

// Interceptors returns the chain's interceptors, outermost first.
func (c *Chain) Interceptors() []connect.Interceptor {
	return append([]connect.Interceptor(nil), c.interceptors...)
}

// HandlerOptions returns the options every Connect handler mounted with this
// chain is built with: the interceptors and the payload caps.
func (c *Chain) HandlerOptions() []connect.HandlerOption {
	return append([]connect.HandlerOption(nil), c.options...)
}
