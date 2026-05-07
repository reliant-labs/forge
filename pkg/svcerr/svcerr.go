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
// via the matching constructor that wraps a human-readable cause:
//
//	return svcerr.NotFound("user")            // → CodeNotFound, "user: not found"
//	return svcerr.PermissionDenied("admin only") // → CodePermissionDenied
//
// Both forms preserve the sentinel for downstream errors.Is checks and
// for Code() lookups.
package svcerr

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
)

// Sentinel errors mapped to Connect codes. Each sentinel corresponds to
// exactly one connect.Code (see codeFor). Service code returns either
// the sentinel directly (`return svcerr.ErrNotFound`) or one of the
// matching constructors (`return svcerr.NotFound("user")`) which wrap
// the sentinel via fmt.Errorf("...: %w", sentinel).
//
// Add new sentinels parsimoniously — only when an existing Connect code
// has no representative sentinel here AND the service layer needs to
// signal it from places the handler can't otherwise distinguish.
var (
	ErrCanceled           = errors.New("svcerr: canceled")
	ErrUnknown            = errors.New("svcerr: unknown")
	ErrInvalidArgument    = errors.New("svcerr: invalid argument")
	ErrDeadlineExceeded   = errors.New("svcerr: deadline exceeded")
	ErrNotFound           = errors.New("svcerr: not found")
	ErrAlreadyExists      = errors.New("svcerr: already exists")
	ErrPermissionDenied   = errors.New("svcerr: permission denied")
	ErrResourceExhausted  = errors.New("svcerr: resource exhausted")
	ErrFailedPrecondition = errors.New("svcerr: failed precondition")
	ErrAborted            = errors.New("svcerr: aborted")
	ErrOutOfRange         = errors.New("svcerr: out of range")
	ErrUnimplemented      = errors.New("svcerr: unimplemented")
	ErrInternal           = errors.New("svcerr: internal")
	ErrUnavailable        = errors.New("svcerr: unavailable")
	ErrDataLoss           = errors.New("svcerr: data loss")
	ErrUnauthenticated    = errors.New("svcerr: unauthenticated")
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

// NotFound wraps ErrNotFound. The supplied entity name appears in the
// error string ("user: not found") for log readability.
func NotFound(entity string) error { return wrapped(ErrNotFound, entity) }

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

// wrapped is the canonical "%s: %w" form used by every constructor so
// that the resulting error string is "<detail>: <sentinel>" and
// errors.Is(err, sentinel) returns true.
func wrapped(sentinel error, detail string) error {
	if detail == "" {
		return sentinel
	}
	return fmt.Errorf("%s: %w", detail, sentinel)
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
//   - Anything else maps to CodeInternal. The original error is
//     preserved as the cause; callers should ensure the message is
//     log-only and not client-visible (i.e., construct generic public
//     messages at the boundary if leakage matters).
func ToConnect(err error) *connect.Error {
	if err == nil {
		return nil
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce
	}
	code := codeFor(err)
	return connect.NewError(code, err)
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
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, ErrCanceled):
		return connect.CodeCanceled
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, ErrDeadlineExceeded):
		return connect.CodeDeadlineExceeded
	case errors.Is(err, ErrInvalidArgument):
		return connect.CodeInvalidArgument
	case errors.Is(err, ErrNotFound):
		return connect.CodeNotFound
	case errors.Is(err, ErrAlreadyExists):
		return connect.CodeAlreadyExists
	case errors.Is(err, ErrPermissionDenied):
		return connect.CodePermissionDenied
	case errors.Is(err, ErrResourceExhausted):
		return connect.CodeResourceExhausted
	case errors.Is(err, ErrFailedPrecondition):
		return connect.CodeFailedPrecondition
	case errors.Is(err, ErrAborted):
		return connect.CodeAborted
	case errors.Is(err, ErrOutOfRange):
		return connect.CodeOutOfRange
	case errors.Is(err, ErrUnimplemented):
		return connect.CodeUnimplemented
	case errors.Is(err, ErrInternal):
		return connect.CodeInternal
	case errors.Is(err, ErrUnavailable):
		return connect.CodeUnavailable
	case errors.Is(err, ErrDataLoss):
		return connect.CodeDataLoss
	case errors.Is(err, ErrUnauthenticated):
		return connect.CodeUnauthenticated
	case errors.Is(err, ErrUnknown):
		return connect.CodeUnknown
	default:
		return connect.CodeInternal
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
