// Package svcerr provides the canonical service-error → Connect-error
// mapping used by every forge handler package.
//
// # Why this lives here
//
// Earlier forge versions left handler packages to write their own
// per-package mapServiceError / toConnectError helper. The dogfood pass
// of control-plane-next ended up with four byte-identical copies of the
// same switch statement (handlers/{billing,daemon,llm_gateway,org}/
// handlers.go). Worse, the api/handlers skill prescribed the
// hand-rolled pattern, so the duplication was earned: the LLM was
// faithfully following the documented convention.
//
// svcerr ships the mapping once, as a library. Sentinels in this
// package are the canonical service-layer error categories; Wrap
// converts any error wrapping one of them into the right
// *connect.Error. Already-Connect errors pass through unchanged so
// handlers can compose freely.
//
// # Usage
//
//	// In internal/<svc>/contract.go (the service layer):
//	import "github.com/reliant-labs/forge/pkg/svcerr"
//
//	func (s *svc) GetThing(ctx context.Context, id string) (*Thing, error) {
//	    row, err := s.db.GetThing(ctx, id)
//	    if errors.Is(err, sql.ErrNoRows) {
//	        return nil, svcerr.NotFound("thing")
//	    }
//	    if err != nil {
//	        return nil, fmt.Errorf("get thing: %w", err)
//	    }
//	    return row, nil
//	}
//
//	// In handlers/<svc>/handlers.go (the wire layer):
//	import "github.com/reliant-labs/forge/pkg/svcerr"
//
//	func (s *Service) GetThing(ctx context.Context, req *connect.Request[pb.GetThingRequest]) (*connect.Response[pb.GetThingResponse], error) {
//	    thing, err := s.deps.Things.GetThing(ctx, req.Msg.GetId())
//	    if err != nil {
//	        return nil, svcerr.Wrap(err)
//	    }
//	    return connect.NewResponse(thingToProto(thing)), nil
//	}
//
// # Sentinel set
//
// One sentinel per Connect code we map. Service code constructs them
// either by `errors.Is`-able comparison (return svcerr.ErrNotFound) or
// via the matching constructor that carries a human-readable cause:
//
//	return svcerr.NotFound("user")               // → CodeNotFound, "user not found"
//	return svcerr.PermissionDenied("admin only") // → CodePermissionDenied, "admin only"
//
// Both forms preserve the sentinel for downstream errors.Is checks and
// for Code() lookups.
//
// # What the client reads
//
// The message on the wire is the DETAIL the constructor was given, and
// nothing else — see detailError and clientMessage. A sentinel is an
// identity, not display copy, and the category it names is already on the
// wire twice (the Connect code, and ReasonHeader for the machine-readable
// reason).
//
// Wrapping does not widen that. `fmt.Errorf("get thing %s from %s: %w",
// id, shard, err)` is context for the OPERATOR: it survives as a cause
// (see [Cause]) and reaches the log, and it never reaches the client. That
// is not a courtesy — a recognised sentinel used to make the whole
// accumulated string client-visible, which published request ids, internal
// hostnames and, measured, a DSN with a password to an unauthenticated
// caller. Prose meant for a client goes in the constructor's argument.
package svcerr

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
)

// ReasonHeader is the Connect error-metadata key under which a stable,
// machine-readable domain reason code (see WithReason) is delivered.
// Clients route on this code instead of matching server MESSAGE TEXT
// with brittle regexes. The value is an app-defined snake_case code
// (e.g. "no_active_subscription"); forge only carries it, it never
// invents codes. HTTP header keys are case-insensitive, so the frontend
// error mapper may read it as "x-forge-error-reason" regardless of
// canonicalization.
const ReasonHeader = "x-forge-error-reason"

// Sentinel errors mapped to Connect codes. Each sentinel corresponds to
// exactly one connect.Code (see codeFor). Service code returns either
// the sentinel directly (`return svcerr.ErrNotFound`) or one of the
// matching constructors (`return svcerr.NotFound("user")`), which carry
// the sentinel for identity while the client reads only the detail (see
// detailError).
//
// Their identity is the VALUE, not the text — errors.New returns a
// distinct pointer per call — so the strings here are free to be plain
// readable prose, and they must be: a service that returns a bare
// sentinel puts exactly this text in front of a user.
//
// Add new sentinels parsimoniously — only when an existing Connect code
// has no representative sentinel here AND the service layer needs to
// signal it from places the handler can't otherwise distinguish.
var (
	ErrCanceled           = errors.New("canceled")
	ErrUnknown            = errors.New("unknown")
	ErrInvalidArgument    = errors.New("invalid argument")
	ErrDeadlineExceeded   = errors.New("deadline exceeded")
	ErrNotFound           = errors.New("not found")
	ErrAlreadyExists      = errors.New("already exists")
	ErrPermissionDenied   = errors.New("permission denied")
	ErrResourceExhausted  = errors.New("resource exhausted")
	ErrFailedPrecondition = errors.New("failed precondition")
	ErrAborted            = errors.New("aborted")
	ErrOutOfRange         = errors.New("out of range")
	ErrUnimplemented      = errors.New("unimplemented")
	ErrInternal           = errors.New("internal")
	ErrUnavailable        = errors.New("unavailable")
	ErrDataLoss           = errors.New("data loss")
	ErrUnauthenticated    = errors.New("unauthenticated")

	// Domain-distinct sentinels with codes shared by the canonical set
	// above. Added because real services routinely need to distinguish
	// these failure modes at errors.Is time even though they map to the
	// same wire-level Connect code. Without them, billing/quota/expiry
	// callers either lose fidelity (collapse into the canonical sentinel)
	// or carry private sentinels, breaking svcerr.Wrap composition.

	// ErrPlanLimit indicates the operation was rejected because the
	// caller's plan/tier doesn't allow it (seat cap, feature gate, model
	// allowlist, etc.). Distinct from a generic rate-limit hit so
	// upstream code can errors.Is(err, ErrPlanLimit) for upsell-prompt /
	// per-cause metric purposes. Wire code: ResourceExhausted.
	ErrPlanLimit = errors.New("plan limit reached")

	// ErrInsufficientBalance indicates a billing wallet / credit
	// balance is insufficient for the operation. Distinct from generic
	// "system not in required state" cases so callers can distinguish
	// "your account is out of money" from "this resource is currently
	// suspended/locked." Wire code: FailedPrecondition.
	ErrInsufficientBalance = errors.New("insufficient balance")

	// ErrExpired indicates a time-limited resource (invitation, session,
	// password-reset token, license key) is no longer valid. Distinct
	// from NotFound (the resource never existed) and from generic
	// FailedPrecondition. Wire code: FailedPrecondition.
	ErrExpired = errors.New("expired")
)

// Constructors wrap each sentinel with a human-readable cause. Use
// these when the service layer wants to communicate WHY the failure
// occurred ("user not found", "AI access is billed via wallet, not
// subscription"). The wrapped error still satisfies errors.Is against
// the sentinel and against the constructor's category, so handlers and
// tests can match on either.

// Canceled wraps ErrCanceled with the supplied detail.
func Canceled(detail string) error { return wrapped(ErrCanceled, detail) }

// Unknown wraps ErrUnknown with the supplied detail. Prefer a more
// specific category when one applies.
func Unknown(detail string) error { return wrapped(ErrUnknown, detail) }

// InvalidArgument wraps ErrInvalidArgument with the supplied detail.
// Use for domain invariants violated post-validation; pure wire-format
// validation should fail earlier in the handler's validator.
func InvalidArgument(detail string) error { return wrapped(ErrInvalidArgument, detail) }

// DeadlineExceeded wraps ErrDeadlineExceeded with the supplied detail.
func DeadlineExceeded(detail string) error { return wrapped(ErrDeadlineExceeded, detail) }

// NotFound wraps ErrNotFound. Its parameter is the ENTITY name rather
// than a detail — the one constructor of which that is true — so it
// composes the sentence itself: NotFound("user") reads "user not found".
func NotFound(entity string) error {
	if entity == "" {
		return ErrNotFound
	}
	return wrapped(ErrNotFound, entity+" not found")
}

// AlreadyExists wraps ErrAlreadyExists with the supplied detail.
func AlreadyExists(detail string) error { return wrapped(ErrAlreadyExists, detail) }

// PermissionDenied wraps ErrPermissionDenied with the supplied reason.
func PermissionDenied(reason string) error { return wrapped(ErrPermissionDenied, reason) }

// ResourceExhausted wraps ErrResourceExhausted with the supplied detail.
// Use for rate limits, quota overruns, plan-limit-reached cases.
func ResourceExhausted(detail string) error { return wrapped(ErrResourceExhausted, detail) }

// FailedPrecondition wraps ErrFailedPrecondition with the supplied
// detail. Use when the operation was rejected because the system is
// not in the required state (e.g., cannot rotate a revoked key, cannot
// remove the last owner).
func FailedPrecondition(detail string) error { return wrapped(ErrFailedPrecondition, detail) }

// Aborted wraps ErrAborted with the supplied detail. Use for
// optimistic-concurrency / transactional-conflict cases.
func Aborted(detail string) error { return wrapped(ErrAborted, detail) }

// OutOfRange wraps ErrOutOfRange with the supplied detail.
func OutOfRange(detail string) error { return wrapped(ErrOutOfRange, detail) }

// Unimplemented wraps ErrUnimplemented with the supplied detail. Use
// for stubbed RPCs and feature-flagged paths.
func Unimplemented(detail string) error { return wrapped(ErrUnimplemented, detail) }

// Internal wraps ErrInternal with the supplied detail. Prefer letting
// generic errors fall through to ToConnect's CodeInternal default
// rather than constructing this explicitly — opaque internal errors
// shouldn't leak detail to clients.
func Internal(detail string) error { return wrapped(ErrInternal, detail) }

// Unavailable wraps ErrUnavailable with the supplied detail.
func Unavailable(detail string) error { return wrapped(ErrUnavailable, detail) }

// DataLoss wraps ErrDataLoss with the supplied detail.
func DataLoss(detail string) error { return wrapped(ErrDataLoss, detail) }

// Unauthenticated wraps ErrUnauthenticated with the supplied reason.
func Unauthenticated(reason string) error { return wrapped(ErrUnauthenticated, reason) }

// PlanLimit wraps ErrPlanLimit with the supplied detail. Use when the
// caller's plan/tier disallows the operation (seat cap, feature gate,
// per-model allowlist). Distinct from ResourceExhausted so upstream
// code can react specifically to plan-related rejections.
func PlanLimit(detail string) error { return wrapped(ErrPlanLimit, detail) }

// InsufficientBalance wraps ErrInsufficientBalance with the supplied
// detail. Use when a billing wallet / credit account doesn't have
// funds for the operation.
func InsufficientBalance(detail string) error { return wrapped(ErrInsufficientBalance, detail) }

// Expired wraps ErrExpired with the supplied detail. Use for
// time-limited resources (invitation, session, password-reset token)
// that have aged out.
func Expired(detail string) error { return wrapped(ErrExpired, detail) }

// detailError is what every constructor returns: the application's own
// words, carrying a sentinel for identity.
//
// Error() is the DETAIL alone. connect.Error.Message() is literally
// err.Error(), so anything Error() appends is published to every caller —
// and the sentinel is an errors.Is identity, not a message. Formatting
// "<detail>: <sentinel>" put that identity on the wire:
//
//	order demo-order-ok cannot become ORDER_STATUS_DELIVERED: svcerr: failed precondition
//
// Nothing on the far side could use it. The Connect code already carries
// the category and ReasonHeader carries the machine-readable one, so the
// tag was pure noise — noise forge's own frontend runtime then had to
// delete with a regex before showing the message to a user.
//
// The identity is not lost: Unwrap returns the sentinel, so errors.Is,
// Code and codeForRecognized are unchanged. It simply stops being TEXT.
type detailError struct {
	detail   string
	sentinel error
}

func (e *detailError) Error() string { return e.detail }
func (e *detailError) Unwrap() error { return e.sentinel }

// wrapped is the canonical form used by every constructor: the client
// reads the detail, errors.Is(err, sentinel) still reports true. An empty
// detail has nothing to say, so the sentinel is returned as-is (its own
// text is readable prose for exactly this reason).
func wrapped(sentinel error, detail string) error {
	if detail == "" {
		return sentinel
	}
	return &detailError{detail: detail, sentinel: sentinel}
}

// ToConnect converts a domain error into a *connect.Error with the
// right code. Behavior:
//
//   - nil input returns nil.
//   - An already-Connect error (anything that errors.As-matches
//     *connect.Error) is returned as-is so handlers can construct
//     specific errors and still funnel them through the same helper.
//   - A wrapped sentinel from this package maps to the matching
//     connect.Code; the original error is preserved as the cause so
//     errors.Is keeps working downstream.
//   - context.Canceled / context.DeadlineExceeded map to
//     CodeCanceled / CodeDeadlineExceeded so handler-side cancellation
//     is surfaced cleanly.
//   - Anything else — a raw driver / SDK error this package does not
//     recognise — maps to CodeInternal with the FIXED message
//     [InternalMessage]. Its text is never client-visible, because that
//     text is written by postgres or a vendor SDK and routinely contains
//     SQL, schema names and connection strings. The original is kept as
//     an unexported cause, reachable server-side via [Cause] and by
//     errors.Is / errors.As.
func ToConnect(err error) *connect.Error {
	if err == nil {
		return nil
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		ce = connect.NewError(codeFor(err), clientSafe(err))
	}
	// A reason code annotated via WithReason rides through to the client
	// as Connect error metadata under ReasonHeader, so the frontend routes
	// on a stable code instead of matching the human-readable message text.
	// Applied to both freshly-built and passed-through Connect errors.
	// Absent a WithReason annotation the metadata is left untouched, so
	// the no-reason path is byte-identical to before.
	if reason, ok := reasonOf(err); ok {
		ce.Meta().Set(ReasonHeader, reason)
	}
	return ce
}

// Wrap is the canonical handler-layer construct:
//
//	if err != nil {
//	    return nil, svcerr.Wrap(err)
//	}
//
// It is identical to ToConnect but returns error so it composes with
// `return ..., err`-style handler signatures. Wrap(nil) returns nil.
func Wrap(err error) error {
	ce := ToConnect(err)
	if ce == nil {
		return nil
	}
	return ce
}

// InternalMessage is the ONLY text an unrecognised server-side failure
// puts on the wire. It matches what the plain-HTTP stack
// (middleware.HTTPStack) has always returned for a 500, so a client sees
// the same thing whichever transport it used.
//
// The distinction that matters: a message the APPLICATION chose —
// svcerr.NotFound("user"), svcerr.Internal("create order failed") — is
// intentional and passes through untouched. A message the application
// never wrote — `ERROR: relation "accounts" does not exist (SQLSTATE
// 42P01) dsn=postgres://app:s3cr3t@db.internal:5432/prod` — is not a
// message at all, it is a diagnostic, and diagnostics are for the
// operator. connect.Error.Message() is literally err.Error(), so
// handing a driver error to connect.NewError publishes it verbatim to
// an unauthenticated caller.
const InternalMessage = "internal server error"

// redactedError separates what the CLIENT may read from what the SERVER
// needs to keep. Error — and therefore connect.Error.Message — returns
// only msg; cause holds the detail, reachable via [Cause]; chain is what
// errors.Is / errors.As traverse, so sentinel checks and driver-error
// type assertions keep working all the way up the stack.
type redactedError struct {
	msg   string
	cause error
	chain error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.chain }

// WithCause returns an error that shows err's message to the client
// while carrying cause for the server's eyes only.
//
// Use it wherever a safe, deliberate summary and an unsafe diagnostic
// both exist and both matter — the classic case being a repository
// failure, where the client should learn "get item failed" and the
// operator needs the *pgconn.PgError behind it:
//
//	return svcerr.Wrap(svcerr.WithCause(
//	    svcerr.Internal(fmt.Sprintf("%s %s failed", op, entity)), dbErr))
//
// The result keeps err's sentinel (so the Connect code is unchanged) and
// keeps cause errors.As-able (so `var pgErr *pgconn.PgError` still
// matches), but cause NEVER contributes to Error(), which is what stops
// it reaching the wire. Retrieve it with [Cause].
func WithCause(err, cause error) error {
	if err == nil {
		return nil
	}
	if cause == nil {
		return err
	}
	return &redactedError{msg: err.Error(), cause: cause, chain: errors.Join(err, cause)}
}

// Cause returns the server-only detail withheld from the client, or nil
// when err carries none. It is what a logging interceptor calls to log
// the truth about a failure the client was only told "internal server
// error" about.
//
// It sees through *connect.Error, so calling it on the value a handler
// returned works without unwrapping first.
func Cause(err error) error {
	var re *redactedError
	if errors.As(err, &re) {
		return re.cause
	}
	return nil
}

// clientMessage returns the text this package is willing to publish for
// err, and whether it recognised the error at all.
//
// It reads the NEAREST detail — never the accumulated Error() string. That
// distinction is the whole guard. A recognised sentinel says "this failure
// is a NotFound"; it says nothing about the text wrapped around it on the
// way out, and the service-layer skill tells authors to wrap:
//
//	return Thing{}, fmt.Errorf("get thing: %w", err)
//
// Because connect.Error.Message() is literally err.Error(), publishing the
// outer string published whatever an author put in it — request ids,
// internal hostnames, and in the measured case a DSN with a password, to
// an unauthenticated caller. Wrapping is for the OPERATOR: it survives as a
// cause (see [Cause]) and reaches the log. The client reads the detail the
// constructor was given, and a bare sentinel's own category text when
// there is no detail.
//
// The corollary for callers: prose meant for a client goes in the
// constructor's argument. Nothing outside it crosses.
func clientMessage(err error) (string, bool) {
	var de *detailError
	if errors.As(err, &de) {
		return de.detail, true
	}
	if _, sentinel, recognised := codeForRecognized(err); recognised {
		return sentinel.Error(), true
	}
	return "", false
}

// clientSafe reduces err to what the client may read. An unrecognised
// error carries text nobody on this side of the wire chose, so only
// [InternalMessage] goes out; either way the original is kept as a cause
// and stays errors.Is/As-able.
func clientSafe(err error) error {
	msg, ok := clientMessage(err)
	if !ok {
		return &redactedError{msg: InternalMessage, cause: err, chain: err}
	}
	if msg == err.Error() {
		return err // nothing was wrapped around it: the error IS the message
	}
	return &redactedError{msg: msg, cause: err, chain: err}
}

// Code returns the connect.Code that ToConnect would assign to err.
// Returns CodeUnknown if err is nil OR if err carries no recognised
// sentinel — this matches Connect's own CodeOf semantics so callers can
// substitute svcerr.Code for connect.CodeOf when they want sentinel
// awareness in addition to *connect.Error inspection.
func Code(err error) connect.Code {
	if err == nil {
		return connect.CodeUnknown
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Code()
	}
	return codeFor(err)
}

// codeFor walks the sentinel→code mapping. Order does not matter
// because each sentinel is distinct; the function returns CodeInternal
// when no sentinel matches and the error is not a context cancellation.
func codeFor(err error) connect.Code {
	code, _, _ := codeForRecognized(err)
	return code
}

// codeForRecognized is codeFor plus the fact that decides whether the
// error's MESSAGE may be shown to a client: whether a sentinel from this
// package (or a context cancellation) actually matched.
//
// recognised=true means the application built this error deliberately —
// svcerr.NotFound("user"), svcerr.Internal("create order failed") — so
// its text was written by someone on this side of the wire.
// recognised=false is the fallback branch: the error came from a driver,
// an SDK, or the standard library, its text was written by neither the
// application nor forge, and CodeInternal is a guess rather than a
// decision. Only that branch gets redacted; see clientSafe.
func codeForRecognized(err error) (connect.Code, error, bool) {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, ErrCanceled):
		return connect.CodeCanceled, ErrCanceled, true
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, ErrDeadlineExceeded):
		return connect.CodeDeadlineExceeded, ErrDeadlineExceeded, true
	case errors.Is(err, ErrInvalidArgument):
		return connect.CodeInvalidArgument, ErrInvalidArgument, true
	case errors.Is(err, ErrNotFound):
		return connect.CodeNotFound, ErrNotFound, true
	case errors.Is(err, ErrAlreadyExists):
		return connect.CodeAlreadyExists, ErrAlreadyExists, true
	case errors.Is(err, ErrPermissionDenied):
		return connect.CodePermissionDenied, ErrPermissionDenied, true
	// ErrPlanLimit MUST be checked before ErrResourceExhausted because
	// constructors share the same Connect code; matching the broader
	// sentinel first would shadow the more specific one.
	case errors.Is(err, ErrPlanLimit):
		return connect.CodeResourceExhausted, ErrPlanLimit, true
	case errors.Is(err, ErrResourceExhausted):
		return connect.CodeResourceExhausted, ErrResourceExhausted, true
	// Same shadowing rule: ErrInsufficientBalance and ErrExpired share
	// FailedPrecondition with the canonical sentinel; check them first.
	case errors.Is(err, ErrInsufficientBalance):
		return connect.CodeFailedPrecondition, ErrInsufficientBalance, true
	case errors.Is(err, ErrExpired):
		return connect.CodeFailedPrecondition, ErrExpired, true
	case errors.Is(err, ErrFailedPrecondition):
		return connect.CodeFailedPrecondition, ErrFailedPrecondition, true
	case errors.Is(err, ErrAborted):
		return connect.CodeAborted, ErrAborted, true
	case errors.Is(err, ErrOutOfRange):
		return connect.CodeOutOfRange, ErrOutOfRange, true
	case errors.Is(err, ErrUnimplemented):
		return connect.CodeUnimplemented, ErrUnimplemented, true
	case errors.Is(err, ErrInternal):
		return connect.CodeInternal, ErrInternal, true
	case errors.Is(err, ErrUnavailable):
		return connect.CodeUnavailable, ErrUnavailable, true
	case errors.Is(err, ErrDataLoss):
		return connect.CodeDataLoss, ErrDataLoss, true
	case errors.Is(err, ErrUnauthenticated):
		return connect.CodeUnauthenticated, ErrUnauthenticated, true
	case errors.Is(err, ErrUnknown):
		return connect.CodeUnknown, ErrUnknown, true
	default:
		return connect.CodeInternal, nil, false
	}
}

// IsCanceled reports whether err carries (or wraps) ErrCanceled or
// context.Canceled.
func IsCanceled(err error) bool {
	return errors.Is(err, ErrCanceled) || errors.Is(err, context.Canceled)
}

// IsDeadlineExceeded reports whether err carries (or wraps)
// ErrDeadlineExceeded or context.DeadlineExceeded.
func IsDeadlineExceeded(err error) bool {
	return errors.Is(err, ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded)
}

// IsInvalidArgument reports whether err carries (or wraps) ErrInvalidArgument.
func IsInvalidArgument(err error) bool { return errors.Is(err, ErrInvalidArgument) }

// IsNotFound reports whether err carries (or wraps) ErrNotFound.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// IsAlreadyExists reports whether err carries (or wraps) ErrAlreadyExists.
func IsAlreadyExists(err error) bool { return errors.Is(err, ErrAlreadyExists) }

// IsPermissionDenied reports whether err carries (or wraps) ErrPermissionDenied.
func IsPermissionDenied(err error) bool { return errors.Is(err, ErrPermissionDenied) }

// IsResourceExhausted reports whether err carries (or wraps) ErrResourceExhausted.
func IsResourceExhausted(err error) bool { return errors.Is(err, ErrResourceExhausted) }

// IsFailedPrecondition reports whether err carries (or wraps) ErrFailedPrecondition.
func IsFailedPrecondition(err error) bool { return errors.Is(err, ErrFailedPrecondition) }

// IsAborted reports whether err carries (or wraps) ErrAborted.
func IsAborted(err error) bool { return errors.Is(err, ErrAborted) }

// IsOutOfRange reports whether err carries (or wraps) ErrOutOfRange.
func IsOutOfRange(err error) bool { return errors.Is(err, ErrOutOfRange) }

// IsUnimplemented reports whether err carries (or wraps) ErrUnimplemented.
func IsUnimplemented(err error) bool { return errors.Is(err, ErrUnimplemented) }

// IsInternal reports whether err carries (or wraps) ErrInternal.
func IsInternal(err error) bool { return errors.Is(err, ErrInternal) }

// IsUnavailable reports whether err carries (or wraps) ErrUnavailable.
func IsUnavailable(err error) bool { return errors.Is(err, ErrUnavailable) }

// IsDataLoss reports whether err carries (or wraps) ErrDataLoss.
func IsDataLoss(err error) bool { return errors.Is(err, ErrDataLoss) }

// IsUnauthenticated reports whether err carries (or wraps) ErrUnauthenticated.
func IsUnauthenticated(err error) bool { return errors.Is(err, ErrUnauthenticated) }

// IsPlanLimit reports whether err carries (or wraps) ErrPlanLimit.
// Distinct from IsResourceExhausted (which would also match rate-limit
// errors); use this when you specifically need "plan-tier rejected."
func IsPlanLimit(err error) bool { return errors.Is(err, ErrPlanLimit) }

// IsInsufficientBalance reports whether err carries (or wraps)
// ErrInsufficientBalance.
func IsInsufficientBalance(err error) bool { return errors.Is(err, ErrInsufficientBalance) }

// IsExpired reports whether err carries (or wraps) ErrExpired.
func IsExpired(err error) bool { return errors.Is(err, ErrExpired) }

// WithDetail attaches a structured proto.Message detail to the connect
// error that ToConnect would build for err. Used when the service
// layer wants to surface client-readable structured context (e.g., a
// validation-failure message describing which field failed).
//
// Behavior:
//   - WithDetail(nil, _) returns nil.
//   - WithDetail(err, nil) returns ToConnect(err) unchanged.
//   - WithDetail(err, msg) returns ToConnect(err) with the supplied
//     proto attached as a *connect.ErrorDetail. If detail-encoding
//     fails (which would only happen for a non-marshalable proto, in
//     practice never), the error is returned without the detail —
//     attaching detail is best-effort and never masks the underlying
//     mapping.
//
// The returned error is always a *connect.Error so handlers can
// `return nil, svcerr.WithDetail(err, &validationFailure{...})`
// directly.
func WithDetail(err error, detail proto.Message) error {
	if err == nil {
		return nil
	}
	ce := ToConnect(err)
	if ce == nil {
		return nil
	}
	if detail == nil {
		return ce
	}
	d, dErr := connect.NewErrorDetail(detail)
	if dErr != nil {
		// Best-effort: return the mapped error without the detail
		// rather than masking the original failure with a detail-
		// encoding error.
		return ce
	}
	ce.AddDetail(d)
	return ce
}

// reasonError annotates an error with a stable machine-readable reason
// code without disturbing the underlying error chain. It Unwraps to the
// wrapped error, so errors.Is/errors.As, codeFor, and ToConnect's
// already-Connect passthrough all keep working through it; the reason is
// only materialized as wire metadata at the ToConnect boundary.
type reasonError struct {
	code string
	err  error
}

func (r *reasonError) Error() string { return r.err.Error() }
func (r *reasonError) Unwrap() error { return r.err }

// WithReason annotates err with a stable, machine-readable reason code
// that the svcerr→Connect mapping surfaces as error metadata under
// ReasonHeader ("x-forge-error-reason"). The reason code is a thin,
// explicit convention: the app author supplies a domain-specific
// snake_case code and forge carries it to the wire, where the frontend
// error mapper routes on the code instead of matching message text.
//
//	// service layer:
//	return svcerr.WithReason(
//	    svcerr.FailedPrecondition("no active subscription"),
//	    "no_active_subscription",
//	)
//	// handler layer (unchanged): return nil, svcerr.Wrap(err)
//
// Behavior:
//   - WithReason(nil, _) returns nil.
//   - WithReason(err, "") returns err unchanged (no annotation, so the
//     mapped Connect error carries no ReasonHeader metadata).
//   - Otherwise the returned error wraps err, preserves errors.Is/As and
//     the mapped Connect code, and carries the code through Wrap/ToConnect
//     to ReasonHeader metadata.
//
// svcerr itself never invents reason codes; it only plumbs the ones its
// caller supplies. forge's own generated-CRUD layer IS such a caller: it
// defines a fixed vocabulary (the crud.Reason* constants) and stamps one
// on every error it produces, so a scaffolded app's frontend never reads
// a null reason on a CRUD path. Hand-written handlers choose their own
// codes; reuse a crud.Reason* name where the meaning matches.
func WithReason(err error, code string) error {
	if err == nil {
		return nil
	}
	if code == "" {
		return err
	}
	return &reasonError{code: code, err: err}
}

// reasonOf returns the nearest reason code annotated on err's chain, if
// any. Additional wrapping (fmt.Errorf("...: %w", err)) around a
// WithReason error is transparent because errors.As walks Unwrap.
func reasonOf(err error) (string, bool) {
	var re *reasonError
	if errors.As(err, &re) {
		return re.code, true
	}
	return "", false
}
