package svcerr_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/reliant-labs/forge/pkg/svcerr"
)

// TestSentinel_MapsToConnectCode covers the full sentinel→code matrix
// for both the bare-sentinel form (`return svcerr.ErrNotFound`) and the
// constructor form (`return svcerr.NotFound("user")`). Both forms must
// land at the same connect.Code so service authors can pick whichever
// reads better at the call site without affecting the wire envelope.
func TestSentinel_MapsToConnectCode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		sentinel error
		ctor     func(string) error
		want     connect.Code
	}{
		{"Canceled", svcerr.ErrCanceled, svcerr.Canceled, connect.CodeCanceled},
		{"Unknown", svcerr.ErrUnknown, svcerr.Unknown, connect.CodeUnknown},
		{"InvalidArgument", svcerr.ErrInvalidArgument, svcerr.InvalidArgument, connect.CodeInvalidArgument},
		{"DeadlineExceeded", svcerr.ErrDeadlineExceeded, svcerr.DeadlineExceeded, connect.CodeDeadlineExceeded},
		{"NotFound", svcerr.ErrNotFound, svcerr.NotFound, connect.CodeNotFound},
		{"AlreadyExists", svcerr.ErrAlreadyExists, svcerr.AlreadyExists, connect.CodeAlreadyExists},
		{"PermissionDenied", svcerr.ErrPermissionDenied, svcerr.PermissionDenied, connect.CodePermissionDenied},
		{"ResourceExhausted", svcerr.ErrResourceExhausted, svcerr.ResourceExhausted, connect.CodeResourceExhausted},
		{"FailedPrecondition", svcerr.ErrFailedPrecondition, svcerr.FailedPrecondition, connect.CodeFailedPrecondition},
		{"Aborted", svcerr.ErrAborted, svcerr.Aborted, connect.CodeAborted},
		{"OutOfRange", svcerr.ErrOutOfRange, svcerr.OutOfRange, connect.CodeOutOfRange},
		{"Unimplemented", svcerr.ErrUnimplemented, svcerr.Unimplemented, connect.CodeUnimplemented},
		{"Internal", svcerr.ErrInternal, svcerr.Internal, connect.CodeInternal},
		{"Unavailable", svcerr.ErrUnavailable, svcerr.Unavailable, connect.CodeUnavailable},
		{"DataLoss", svcerr.ErrDataLoss, svcerr.DataLoss, connect.CodeDataLoss},
		{"Unauthenticated", svcerr.ErrUnauthenticated, svcerr.Unauthenticated, connect.CodeUnauthenticated},
	}

	for _, tc := range cases {
		t.Run("bare/"+tc.name, func(t *testing.T) {
			t.Parallel()
			ce := svcerr.ToConnect(tc.sentinel)
			if ce == nil {
				t.Fatalf("ToConnect(sentinel) returned nil")
			}
			if got := ce.Code(); got != tc.want {
				t.Fatalf("code = %v, want %v", got, tc.want)
			}
			if got := svcerr.Code(tc.sentinel); got != tc.want {
				t.Fatalf("Code(sentinel) = %v, want %v", got, tc.want)
			}
		})
		t.Run("ctor/"+tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.ctor("detail")
			ce := svcerr.ToConnect(err)
			if ce == nil {
				t.Fatalf("ToConnect(ctor) returned nil")
			}
			if got := ce.Code(); got != tc.want {
				t.Fatalf("code = %v, want %v", got, tc.want)
			}
			// Constructor-wrapped errors must remain Is-matchable
			// against their sentinel (the whole point of using %w).
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("errors.Is(ctor(...), sentinel) = false, want true")
			}
		})
	}
}

// TestToConnect_NilPassthrough checks the nil-input contract that
// callers rely on for `return nil, svcerr.Wrap(err)` to short-circuit
// cleanly when err is nil.
func TestToConnect_NilPassthrough(t *testing.T) {
	t.Parallel()
	if got := svcerr.ToConnect(nil); got != nil {
		t.Fatalf("ToConnect(nil) = %v, want nil", got)
	}
	if got := svcerr.Wrap(nil); got != nil {
		t.Fatalf("Wrap(nil) = %v, want nil", got)
	}
}

// TestToConnect_PreservesCause asserts that errors.Is on the *result*
// of ToConnect still finds the original sentinel. This is what allows
// upstream layers (logging interceptors, audit hooks) to inspect the
// real cause after the handler has wrapped it.
func TestToConnect_PreservesCause(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("getting user: %w", svcerr.NotFound("user"))
	ce := svcerr.ToConnect(err)
	if ce == nil {
		t.Fatal("ToConnect returned nil")
	}
	if !errors.Is(ce, svcerr.ErrNotFound) {
		t.Fatal("errors.Is(ce, ErrNotFound) = false, want true")
	}
	if ce.Code() != connect.CodeNotFound {
		t.Fatalf("code = %v, want CodeNotFound", ce.Code())
	}
}

// TestToConnect_AlreadyConnect_Passthrough verifies that an existing
// *connect.Error returned from a service is reused verbatim — code and
// detail are preserved without re-wrapping.
func TestToConnect_AlreadyConnect_Passthrough(t *testing.T) {
	t.Parallel()
	original := connect.NewError(connect.CodeUnavailable, errors.New("upstream down"))
	d, err := connect.NewErrorDetail(structpb.NewStringValue("retry-after"))
	if err != nil {
		t.Fatalf("NewErrorDetail: %v", err)
	}
	original.AddDetail(d)

	ce := svcerr.ToConnect(original)
	if ce == nil {
		t.Fatal("ToConnect returned nil")
	}
	if ce.Code() != connect.CodeUnavailable {
		t.Fatalf("code = %v, want CodeUnavailable", ce.Code())
	}
	// Pointer-identity passthrough: the Connect error should not be
	// re-allocated when it's already shaped correctly.
	if ce != original {
		t.Fatalf("expected identical pointer; got new instance")
	}
	if got := len(ce.Details()); got != 1 {
		t.Fatalf("details preserved = %d, want 1", got)
	}
}

// TestToConnect_AlreadyConnect_Wrapped covers the case where a Connect
// error is wrapped via fmt.Errorf("%w") deeper in the stack. The
// errors.As traversal must find it and return that inner Connect error
// unchanged so we don't double-wrap as CodeInternal.
func TestToConnect_AlreadyConnect_Wrapped(t *testing.T) {
	t.Parallel()
	inner := connect.NewError(connect.CodeAborted, errors.New("conflict"))
	wrapped := fmt.Errorf("commit: %w", inner)
	ce := svcerr.ToConnect(wrapped)
	if ce == nil {
		t.Fatal("ToConnect returned nil")
	}
	if ce.Code() != connect.CodeAborted {
		t.Fatalf("code = %v, want CodeAborted (errors.As must find the inner Connect error)", ce.Code())
	}
}

// TestToConnect_ContextErrors maps stdlib context cancellations.
func TestToConnect_ContextErrors(t *testing.T) {
	t.Parallel()
	if got := svcerr.Code(context.Canceled); got != connect.CodeCanceled {
		t.Fatalf("Code(context.Canceled) = %v, want CodeCanceled", got)
	}
	if got := svcerr.Code(context.DeadlineExceeded); got != connect.CodeDeadlineExceeded {
		t.Fatalf("Code(context.DeadlineExceeded) = %v, want CodeDeadlineExceeded", got)
	}
}

// TestToConnect_UnknownErrorMapsToInternal: any error that doesn't
// match a sentinel or context cancellation falls through to CodeInternal.
// This is the "unknown DB error" case.
func TestToConnect_UnknownErrorMapsToInternal(t *testing.T) {
	t.Parallel()
	err := errors.New("connection reset by peer")
	if got := svcerr.Code(err); got != connect.CodeInternal {
		t.Fatalf("Code(unknown) = %v, want CodeInternal", got)
	}
	ce := svcerr.ToConnect(err)
	if ce == nil || ce.Code() != connect.CodeInternal {
		t.Fatalf("ToConnect(unknown) returned %v, want CodeInternal", ce)
	}
}

// TestCode_NilReturnsUnknown matches connect.CodeOf's nil behaviour.
func TestCode_NilReturnsUnknown(t *testing.T) {
	t.Parallel()
	if got := svcerr.Code(nil); got != connect.CodeUnknown {
		t.Fatalf("Code(nil) = %v, want CodeUnknown", got)
	}
}

// TestWrap_ReturnsErrorTypedNil verifies the canonical handler-layer
// pattern: `return nil, svcerr.Wrap(err)` works when err is nil
// (returns plain nil, no typed-nil interface foot-gun).
func TestWrap_ReturnsErrorTypedNil(t *testing.T) {
	t.Parallel()
	var ret error = svcerr.Wrap(nil)
	if ret != nil {
		t.Fatalf("Wrap(nil) typed-nil interface check failed: got %v", ret)
	}
}

// TestWrap_ProducesConnectError ensures Wrap returns the same code path
// as ToConnect (just re-typed as error for handler-return ergonomics).
func TestWrap_ProducesConnectError(t *testing.T) {
	t.Parallel()
	err := svcerr.Wrap(svcerr.AlreadyExists("user with that email"))
	if err == nil {
		t.Fatal("Wrap returned nil for non-nil input")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		t.Fatalf("errors.As(*connect.Error) failed; type = %T", err)
	}
	if ce.Code() != connect.CodeAlreadyExists {
		t.Fatalf("code = %v, want CodeAlreadyExists", ce.Code())
	}
}

// TestIsX_Helpers spot-checks the IsX predicates against both the bare
// sentinel and a constructor-wrapped form.
func TestIsX_Helpers(t *testing.T) {
	t.Parallel()
	if !svcerr.IsNotFound(svcerr.ErrNotFound) {
		t.Fatal("IsNotFound(ErrNotFound) = false")
	}
	if !svcerr.IsNotFound(svcerr.NotFound("user")) {
		t.Fatal("IsNotFound(NotFound(...)) = false")
	}
	if svcerr.IsNotFound(svcerr.AlreadyExists("...")) {
		t.Fatal("IsNotFound(AlreadyExists) = true; want false")
	}
	if !svcerr.IsAlreadyExists(svcerr.ErrAlreadyExists) {
		t.Fatal("IsAlreadyExists(ErrAlreadyExists) = false")
	}
	if !svcerr.IsPermissionDenied(svcerr.PermissionDenied("admin only")) {
		t.Fatal("IsPermissionDenied(...) = false")
	}
	if !svcerr.IsCanceled(context.Canceled) {
		t.Fatal("IsCanceled(context.Canceled) = false")
	}
	if !svcerr.IsDeadlineExceeded(context.DeadlineExceeded) {
		t.Fatal("IsDeadlineExceeded(context.DeadlineExceeded) = false")
	}
	if !svcerr.IsUnauthenticated(svcerr.Unauthenticated("no token")) {
		t.Fatal("IsUnauthenticated(Unauthenticated(...)) = false")
	}
	if svcerr.IsUnauthenticated(nil) {
		t.Fatal("IsUnauthenticated(nil) = true; want false")
	}
}

// TestWithDetail_RoundTrip verifies a structured proto detail is
// attached to the resulting *connect.Error and can be retrieved.
func TestWithDetail_RoundTrip(t *testing.T) {
	t.Parallel()
	original := svcerr.InvalidArgument("name must be <= 256 chars")
	detail := structpb.NewStringValue("name")

	err := svcerr.WithDetail(original, detail)
	var ce *connect.Error
	if !errors.As(err, &ce) {
		t.Fatalf("errors.As(*connect.Error) failed; type = %T", err)
	}
	if ce.Code() != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want CodeInvalidArgument", ce.Code())
	}
	details := ce.Details()
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	got, err := details[0].Value()
	if err != nil {
		t.Fatalf("decoding detail: %v", err)
	}
	if !proto.Equal(got, detail) {
		t.Fatalf("detail round-trip mismatch:\n got: %v\nwant: %v", got, detail)
	}
}

// TestWithDetail_NilInputs covers the contract corners.
func TestWithDetail_NilInputs(t *testing.T) {
	t.Parallel()
	if got := svcerr.WithDetail(nil, structpb.NewStringValue("x")); got != nil {
		t.Fatalf("WithDetail(nil, _) = %v, want nil", got)
	}
	err := svcerr.WithDetail(svcerr.NotFound("x"), nil)
	var ce *connect.Error
	if !errors.As(err, &ce) {
		t.Fatalf("WithDetail(err, nil) did not produce *connect.Error")
	}
	if got := len(ce.Details()); got != 0 {
		t.Fatalf("WithDetail(err, nil) attached %d details, want 0", got)
	}
	if ce.Code() != connect.CodeNotFound {
		t.Fatalf("WithDetail(err, nil) code = %v, want CodeNotFound", ce.Code())
	}
}

// TestWrapped_EmptyDetail returns the sentinel itself so callers can
// `return svcerr.NotFound("")`-shaped paths without producing a
// confusing "<empty>: not found" string.
func TestWrapped_EmptyDetail(t *testing.T) {
	t.Parallel()
	err := svcerr.NotFound("")
	if err != svcerr.ErrNotFound {
		t.Fatalf("NotFound(\"\") = %v, want ErrNotFound sentinel", err)
	}
}
