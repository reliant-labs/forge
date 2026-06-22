// Package authn provides the authentication interceptor mechanism that
// forge-generated services wire from their thin, user-owned
// pkg/middleware file.
//
// # Shape
//
// The library owns the MECHANISM:
//
//   - construction-time refusal: a production server with no auth
//     provider configured must not start (see [NewInterceptor]);
//   - mode resolution (validator installed / external provider /
//     AUTH_MODE=none / dev mode), decided once at construction, never
//     per-request;
//   - the exact-match unauthenticated-procedure allow-list gate;
//   - Bearer-token extraction and the CodeUnauthenticated error
//     envelope (a missing Authorization header is a 401, never a
//     silent pass-through);
//   - claims plumbing: validate → enrich → stash on the context.
//
// The project owns the POLICY, passed in as a [Policy] value from the
// scaffolded-once pkg/middleware/middleware.go:
//
//   - the token validator (and when it gets installed),
//   - the identity enricher hook (e.g. hydrate claims from the user
//     table after signature validation),
//   - the allow-list contents,
//   - dev-claims behaviour (the synthetic principal attached while
//     running with auth off), and
//   - the claims context key, via the ContextWithClaims callback —
//     generated handlers keep referencing the project's
//     middleware.Claims / middleware.ClaimsFromContext, so the public
//     surface of generated code does not churn when the mechanism
//     moves here.
//
// # Modes
//
// [NewInterceptor] resolves exactly one of three modes at construction
// time (decision order matters; first match wins):
//
//  1. Validate — Policy.ValidatorConfigured is true. Every procedure
//     not in Policy.Unauthenticated REQUIRES a valid Bearer token.
//  2. Passthrough — Policy.ExternalAuth is true (a pack or hand-rolled
//     interceptor later in the chain owns auth), OR the operator typed
//     AUTH_MODE=none into the environment (explicit opt-out, read once
//     at construction), OR Policy.DevMode is true (injected from the
//     project's typed config — this package never re-derives dev mode
//     from the environment).
//  3. Unconfigured — none of the above. NewInterceptor returns an
//     error and startup must abort: a production server with no auth
//     provider is always a bug, and refusing to start is safer than
//     silently accepting (or silently rejecting) every request.
//
// # Usage from the project's middleware package
//
//	// pkg/middleware/middleware.go (user-owned, scaffolded once)
//	func NewAuthInterceptor(devMode bool) (connect.Interceptor, error) {
//	    return authn.NewInterceptor(authn.Policy{
//	        DevMode:             devMode,
//	        ValidatorConfigured: validatorInstalled(),
//	        ExternalAuth:        externalAuthRegistered(),
//	        Validate:            ValidateToken,
//	        Unauthenticated:     unauthenticatedProcedures,
//	        Enrich:              enrichClaims,
//	        DevClaims:           devClaims,
//	        ContextWithClaims:   ContextWithClaims,
//	    })
//	}
//
// # Layering ADDITIONAL context (the Decorate seam)
//
// Some projects need to install MORE than the library's single claims
// stash once a request is authenticated — a second, parallel identity
// context (e.g. a ported internal/auth user-id context), the raw
// Authorization header for outbound propagation, tenant stamping, or
// any enrichment that writes context values rather than rewriting
// Claims. The library owns the hard mechanism (header extraction,
// validation, error mapping, the claims stash, the allow-list); the
// project layers its extra context through [Policy.Decorate], which runs
// at the SINGLE post-authentication chokepoint for BOTH the Validate
// path and the dev-claims path. Without it a project that needs a
// dual-context bridge had to fork the whole interceptor for one missing
// callback — Decorate is that callback, so the fork collapses to a
// Policy value.
package authn

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"connectrpc.com/connect"

	"github.com/reliant-labs/forge/pkg/auth"
)

// Policy carries the project-owned authentication decisions into the
// library mechanism. See the package doc for the policy/mechanism
// split. The zero value resolves to the Unconfigured mode (construction
// refused) outside dev mode — fail-closed by default.
type Policy struct {
	// ValidatorConfigured reports whether the project installed a real
	// token validator (e.g. via its SetTokenValidator helper). When
	// true the interceptor runs in Validate mode and Validate +
	// ContextWithClaims must be non-nil.
	ValidatorConfigured bool

	// ExternalAuth reports whether an auth provider (a pack, or
	// hand-rolled setup code) registered its own interceptor alongside
	// this one. The interceptor then becomes a pure passthrough so the
	// external interceptor is the sole source of truth.
	ExternalAuth bool

	// DevMode is INJECTED from the project's typed config
	// (cfg.Mode().IsDev(), computed once in config.Load from
	// ENVIRONMENT). This package never reads the environment to decide
	// dev mode — dev-mode has exactly one source of truth across
	// bootstrap, this interceptor, and any auth pack.
	DevMode bool

	// Validate validates a raw bearer token and returns the claims.
	// Called per-request in Validate mode, through whatever indirection
	// the project supplies — pass the project's ValidateToken wrapper
	// (not the validator function itself) so a validator installed or
	// swapped after interceptor construction still takes effect. Mode
	// resolution remains construction-time: if no validator is
	// installed by the time the interceptor is built, set
	// ValidatorConfigured accordingly and construction is refused.
	Validate func(token string) (*auth.Claims, error)

	// Unauthenticated is the explicit allow-list of procedures that
	// bypass authentication. Entries must be FULL procedure strings of
	// the form "/package.Service/Method" — matching is exact, never by
	// substring, so a user-defined "HealthReport" RPC can't ride along
	// with "/grpc.health.v1.Health/Check".
	Unauthenticated map[string]struct{}

	// Enrich, when non-nil, runs after token validation and before the
	// claims are stashed on the context. Projects use it to hydrate
	// identity (roles from the DB, org membership, feature flags) onto
	// the validated claims. Returning an error rejects the request: a
	// *connect.Error is passed through verbatim, anything else becomes
	// CodeUnauthenticated.
	Enrich func(ctx context.Context, claims *auth.Claims) (*auth.Claims, error)

	// DevClaims, when non-nil, supplies the synthetic principal
	// attached to every request while the interceptor runs in
	// passthrough mode (dev mode or AUTH_MODE=none). Lets handlers and
	// authorizers that read claims keep working without a validator.
	// nil (the default) keeps passthrough a pure identity — no claims.
	// Ignored in Validate mode and when ExternalAuth is set (the
	// external provider owns claims then).
	DevClaims func() *auth.Claims

	// ContextWithClaims stashes validated (or dev) claims on the
	// context. The project owns the context key — generated handlers
	// read claims back via the project's ClaimsFromContext, so the
	// library never defines a key of its own. Required in Validate
	// mode and whenever DevClaims is set.
	ContextWithClaims func(ctx context.Context, claims *auth.Claims) context.Context

	// Decorate, when non-nil, runs at the SINGLE post-authentication
	// chokepoint — AFTER the library has installed claims via
	// ContextWithClaims — in BOTH the Validate path (a real Bearer token
	// was validated and, if set, Enrich'd) and the dev-claims path
	// (DevClaims supplied the synthetic principal). It lets the project
	// layer ADDITIONAL context the library does not own:
	//
	//   - a second, parallel identity context (e.g. a ported
	//     internal/auth user-id context that other packages read);
	//   - the raw Authorization header, for forwarding the caller's
	//     identity on outbound calls — passed as authorization so the
	//     project never has to re-derive it (it is "" in the dev-claims
	//     path when no header was sent);
	//   - tenant stamping, feature flags, or any context-valued
	//     enrichment that writes onto ctx rather than rewriting Claims.
	//
	// Decorate only ADDS context; it cannot reject the request. Reject
	// at validation (Validate) or claims rewriting (Enrich), both of
	// which run before Decorate. nil (the default) leaves the context
	// exactly as the library produced it — behaviour is unchanged.
	//
	// Decorate does NOT run in the pure-passthrough modes (ExternalAuth,
	// or dev/AUTH_MODE=none with no DevClaims): there are no claims to
	// decorate around, and ExternalAuth means another interceptor owns
	// identity entirely.
	Decorate func(ctx context.Context, claims *auth.Claims, authorization string) context.Context

	// MapError, when non-nil, maps a token-validation failure into the
	// connect error returned to the caller. It receives the raw error
	// from Validate and the connect.Error the library would return by
	// default (always CodeUnauthenticated, wrapping the validation
	// error). Projects use it to distinguish, say, an expired token
	// (CodeUnauthenticated) from a revoked tenant (CodePermissionDenied)
	// without forking the interceptor. Returning nil falls back to the
	// library default. nil (the default) keeps the standard
	// CodeUnauthenticated envelope. Applies only to validator failures;
	// a missing or malformed Authorization header is always the
	// library's CodeUnauthenticated (those are protocol errors, not
	// policy decisions).
	MapError func(err error, fallback *connect.Error) *connect.Error
}

// mode is the construction-time resolution of a Policy.
type mode int

const (
	modeUnconfigured mode = iota
	modeValidate
	modePassthrough
)

// resolve applies the documented decision order. AUTH_MODE is read here
// — once, at construction — never per-request.
func (p Policy) resolve() mode {
	switch {
	case p.ValidatorConfigured:
		return modeValidate
	case p.ExternalAuth:
		return modePassthrough
	case strings.EqualFold(os.Getenv("AUTH_MODE"), "none"):
		return modePassthrough
	case p.DevMode:
		return modePassthrough
	}
	return modeUnconfigured
}

// NewInterceptor resolves the policy into a Connect interceptor, or
// refuses construction when no auth provider is configured and no
// explicit opt-out was given. Callers must treat the error as fatal and
// abort startup before binding the listener.
func NewInterceptor(p Policy) (connect.Interceptor, error) {
	switch p.resolve() {
	case modeUnconfigured:
		return nil, errors.New("authn.NewInterceptor: no auth provider configured — " +
			"install an auth pack, register a real validator (middleware.SetTokenValidator), " +
			"or set AUTH_MODE=none (or ENVIRONMENT=development) to explicitly run without " +
			"authentication; see pkg/middleware/middleware.go for the policy hooks")
	case modeValidate:
		if p.Validate == nil {
			return nil, errors.New("authn.NewInterceptor: ValidatorConfigured is true but Policy.Validate is nil")
		}
		if p.ContextWithClaims == nil {
			return nil, errors.New("authn.NewInterceptor: Validate mode requires Policy.ContextWithClaims (the project-owned claims stash)")
		}
		return &interceptor{policy: p}, nil
	default: // modePassthrough
		// External auth owns claims; dev/none passthrough may attach a
		// synthetic dev principal when the project supplies one.
		if !p.ExternalAuth && p.DevClaims != nil {
			if p.ContextWithClaims == nil {
				return nil, errors.New("authn.NewInterceptor: Policy.DevClaims requires Policy.ContextWithClaims")
			}
			return &devClaimsInterceptor{policy: p}, nil
		}
		return passthrough{}, nil
	}
}

// passthrough is the no-op identity interceptor for the passthrough
// mode without dev claims. WrapUnary/WrapStreamingHandler return next
// untouched — the interceptor never inspects the Authorization header.
type passthrough struct{}

func (passthrough) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc { return next }
func (passthrough) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}
func (passthrough) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// devClaimsInterceptor is passthrough plus the project's synthetic dev
// principal: no header inspection, no rejection, but every request
// carries claims so claim-reading handlers work with auth off.
type devClaimsInterceptor struct {
	policy Policy
}

// attach installs the synthetic dev principal and then runs the
// project's Decorate hook (if any) at the same chokepoint the Validate
// path uses. The raw Authorization header is passed through to Decorate
// — the dev-claims path accepts any header without validating it (the
// admin-web stub provider sends a placeholder token), so a project that
// forwards the caller's Authorization on outbound calls still sees it
// here.
func (d *devClaimsInterceptor) attach(ctx context.Context, authorization string) context.Context {
	claims := d.policy.DevClaims()
	if claims == nil {
		return ctx
	}
	ctx = d.policy.ContextWithClaims(ctx, claims)
	if d.policy.Decorate != nil {
		ctx = d.policy.Decorate(ctx, claims, authorization)
	}
	return ctx
}

func (d *devClaimsInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		var authorization string
		if req != nil {
			authorization = req.Header().Get("Authorization")
		}
		return next(d.attach(ctx, authorization), req)
	}
}

func (d *devClaimsInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (d *devClaimsInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		return next(d.attach(ctx, conn.RequestHeader().Get("Authorization")), conn)
	}
}

// interceptor is the Validate-mode implementation.
type interceptor struct {
	policy Policy
}

func (a *interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if a.allowUnauthenticated(req.Spec().Procedure) {
			return next(ctx, req)
		}
		ctx, err := a.authenticate(ctx, req.Header().Get("Authorization"))
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (a *interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next // client-side: no server auth to enforce
}

func (a *interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if a.allowUnauthenticated(conn.Spec().Procedure) {
			return next(ctx, conn)
		}
		ctx, err := a.authenticate(ctx, conn.RequestHeader().Get("Authorization"))
		if err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

// allowUnauthenticated reports whether the procedure is on the explicit
// allow-list. Exact match only — see Policy.Unauthenticated.
func (a *interceptor) allowUnauthenticated(procedure string) bool {
	if procedure == "" {
		return false
	}
	_, ok := a.policy.Unauthenticated[procedure]
	return ok
}

// authenticate extracts and validates a Bearer token from the
// Authorization header, runs the enricher hook, and attaches the
// resulting claims to the context.
//
// A missing Authorization header is CodeUnauthenticated. The ONLY
// unauthenticated path through this interceptor is the explicit
// allow-list, which the callers check BEFORE invoking this function —
// an anonymous pass-through here would silently downgrade every handler
// that forgets to check claims.
func (a *interceptor) authenticate(ctx context.Context, authorization string) (context.Context, error) {
	if authorization == "" {
		return ctx, connect.NewError(connect.CodeUnauthenticated,
			errors.New("missing Authorization header (procedure is not on the unauthenticated allow-list)"))
	}

	token := strings.TrimPrefix(authorization, "Bearer ")
	if token == authorization {
		return ctx, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid authorization format"))
	}

	claims, err := a.policy.Validate(token)
	if err != nil {
		fallback := connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("invalid token: %w", err))
		if a.policy.MapError != nil {
			if mapped := a.policy.MapError(err, fallback); mapped != nil {
				return ctx, mapped
			}
		}
		return ctx, fallback
	}

	if a.policy.Enrich != nil {
		claims, err = a.policy.Enrich(ctx, claims)
		if err != nil {
			var ce *connect.Error
			if errors.As(err, &ce) {
				return ctx, err
			}
			return ctx, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("identity enrichment failed: %w", err))
		}
	}

	ctx = a.policy.ContextWithClaims(ctx, claims)
	if a.policy.Decorate != nil {
		// The single post-authentication chokepoint: layer any
		// project-owned context (dual identity bridge, raw Authorization
		// for outbound propagation, tenant stamping) around the
		// library's claims stash. Decorate only adds context — it cannot
		// reject — so the raw authorization header is handed through too.
		ctx = a.policy.Decorate(ctx, claims, authorization)
	}
	return ctx, nil
}
