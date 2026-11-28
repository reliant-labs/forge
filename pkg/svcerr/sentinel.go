package svcerr

// sentinel.go — a domain package's own sentinel errors, with client-visible
// text.
//
// A store or domain package wants named errors it can return and callers can
// match: ErrJobDeleted, ErrLeaseLost. The obvious spelling is a type of its
// own that unwraps to an svcerr kind:
//
//	type sentinel struct{ msg string; kind error }
//	func (e *sentinel) Error() string { return e.msg }
//	func (e *sentinel) Unwrap() error { return e.kind }
//	var ErrJobDeleted = &sentinel{"job was deleted", svcerr.ErrNotFound}
//
// It maps to the right code, and the client reads `not found`: its own text
// is dropped. That is deliberate (see clientMessage). Only text the
// application handed to an svcerr constructor crosses the wire, because
// svcerr cannot tell an Error() someone wrote for a user from one a driver
// wrote for an operator, and the second has carried DSNs. A type of your own
// is the second kind as far as svcerr can see.
//
// Sentinel is the supported spelling of the first.

import "fmt"

// Sentinel returns a sentinel error of kind with msg as its exact
// client-visible message, for a package-level var:
//
//	var ErrJobDeleted = svcerr.Sentinel(svcerr.ErrNotFound, "job was deleted")
//
// The value is the identity: errors.Is(err, ErrJobDeleted) holds through
// fmt.Errorf("…: %w") wrapping, and so does errors.Is(err, kind), which is
// what Code, ToConnect and the Is* predicates read. Returned through
// [Wrap] (or the chain's [Interceptor]) it reaches the client as kind's code
// with msg as the message, exactly as written: unlike [NotFound], nothing is
// appended. A package that returns it needs neither connect-go major.
//
// kind is any error svcerr classifies — one of the Err* sentinels, or
// another Sentinel (the new one then also matches it under errors.Is). msg
// is copy for the caller, never a diagnostic's Error() text; attach
// diagnostics per call with [WithCause]:
//
//	return svcerr.WithCause(ErrJobDeleted, err)
//
// Sentinel panics on a kind svcerr does not classify or an empty msg. Both
// are mistakes in a package-level declaration, so they surface when the
// package is first loaded — in its own tests — rather than as an internal
// error on some later request.
//
// A server-fault kind (ErrInternal, ErrUnavailable, ErrDataLoss, ErrUnknown)
// is allowed. Its stack is captured where [Wrap] first sees it, not at the
// declaration, which would point every report at package initialisation.
func Sentinel(kind error, msg string) error {
	if msg == "" {
		panic("svcerr.Sentinel: empty message; a sentinel's message is what the client reads")
	}
	if kind == nil {
		panic(fmt.Sprintf("svcerr.Sentinel(nil, %q): kind must be an svcerr kind such as svcerr.ErrNotFound", msg))
	}
	if _, _, ok := codeForRecognized(kind); !ok {
		panic(fmt.Sprintf("svcerr.Sentinel(%v, %q): kind is not an error svcerr classifies; use one of the svcerr.Err* sentinels (svcerr.ErrNotFound, svcerr.ErrFailedPrecondition, …)", kind, msg))
	}
	return &detailError{detail: msg, sentinel: kind}
}
