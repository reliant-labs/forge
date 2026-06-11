package authz

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	"github.com/reliant-labs/forge/pkg/auth"
)

// TestRolesDecider_ZeroValueFailsClosed pins the security-critical zero
// value: a RolesDecider with NO FailMode set must DENY a method that is
// absent from both tables. Permissiveness is an explicit opt-in
// (AllowUnknownMethods), never the default — dev ergonomics are the
// DevAuthorizer's job, not the policy table's.
func TestRolesDecider_ZeroValueFailsClosed(t *testing.T) {
	resetUnknownMethodWarnings()
	d := RolesDecider{
		MethodRoles:        map[string][]string{},
		MethodAuthRequired: map[string]bool{},
	}
	a := New(d)
	wireLookup(t, lookupClaims)
	ctx := putClaims(context.Background(), &auth.Claims{Role: "admin"})

	err := a.CanAccess(ctx, "/svc/ZeroValueUnknown")
	if err == nil {
		t.Fatal("zero-value FailMode must DENY unknown methods (fail closed); got nil")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodePermissionDenied {
		t.Fatalf("want CodePermissionDenied for unknown method, got %v", err)
	}
}

// TestRolesDecider_ZeroValueFailsClosed_NoAuthRequiredMap covers the
// second unknown branch (MethodRoles miss with nil MethodAuthRequired):
// the zero value must fail closed there too.
func TestRolesDecider_ZeroValueFailsClosed_NoAuthRequiredMap(t *testing.T) {
	resetUnknownMethodWarnings()
	d := RolesDecider{
		MethodRoles: map[string][]string{},
	}
	a := New(d)
	wireLookup(t, lookupClaims)
	ctx := putClaims(context.Background(), &auth.Claims{Role: "admin"})

	if err := a.CanAccess(ctx, "/svc/ZeroValueUnknownSecondBranch"); err == nil {
		t.Fatal("zero-value FailMode must DENY unknown methods (second branch); got nil")
	}
}

// TestWarnUnknownMethod_OncePerMethod pins the log-volume contract: the
// unknown-method signal fires ONCE per distinct method per process, not
// once per request. The pre-fix behavior warned on every request, which
// in steady state turned a single missing annotation into a WARN flood.
func TestWarnUnknownMethod_OncePerMethod(t *testing.T) {
	resetUnknownMethodWarnings()
	getRecords := captureSlog(t)

	d := RolesDecider{
		MethodRoles:        map[string][]string{},
		MethodAuthRequired: map[string]bool{},
	}
	a := New(d)
	wireLookup(t, lookupClaims)
	ctx := putClaims(context.Background(), &auth.Claims{Role: "admin"})

	// Same unknown method, three requests → one warn.
	for i := 0; i < 3; i++ {
		_ = a.CanAccess(ctx, "/svc/OncePerMethod")
	}
	if records := getRecords(); len(records) != 1 {
		t.Fatalf("unknown-method warn must fire once per method, got %d records", len(records))
	}

	// A DIFFERENT unknown method still gets its own warn.
	_ = a.CanAccess(ctx, "/svc/OncePerMethodOther")
	if records := getRecords(); len(records) != 2 {
		t.Fatalf("distinct unknown method must still warn, got %d records", len(records))
	}
}

// TestWarnUnknownMethod_OncePerMethod_CustomCallback: the dedup applies
// to the OnUnknownMethod override too — the contract is "once per
// unknown-method", regardless of sink.
func TestWarnUnknownMethod_OncePerMethod_CustomCallback(t *testing.T) {
	resetUnknownMethodWarnings()
	var seen []string
	d := RolesDecider{
		MethodRoles:        map[string][]string{},
		MethodAuthRequired: map[string]bool{},
		OnUnknownMethod:    func(m string) { seen = append(seen, m) },
	}
	a := New(d)
	wireLookup(t, lookupClaims)
	ctx := putClaims(context.Background(), &auth.Claims{Role: "admin"})

	for i := 0; i < 3; i++ {
		_ = a.CanAccess(ctx, "/svc/OncePerMethodCallback")
	}
	if len(seen) != 1 {
		t.Fatalf("OnUnknownMethod must fire once per method, got %d calls", len(seen))
	}
}

// TestRolesDecider_AllowUnknownMethods_OptIn: the permissive mode still
// exists but must be opted into by its self-indicting name. Unknown
// methods are allowed and the warn still fires.
func TestRolesDecider_AllowUnknownMethods_OptIn(t *testing.T) {
	resetUnknownMethodWarnings()
	getRecords := captureSlog(t)

	d := RolesDecider{
		MethodRoles:        map[string][]string{},
		MethodAuthRequired: map[string]bool{},
		FailMode:           AllowUnknownMethods,
	}
	a := New(d)
	wireLookup(t, lookupClaims)
	ctx := putClaims(context.Background(), &auth.Claims{Role: "admin"})

	if err := a.CanAccess(ctx, "/svc/OptInAllowUnknown"); err != nil {
		t.Fatalf("AllowUnknownMethods should allow unknown methods, got %v", err)
	}
	if records := getRecords(); len(records) != 1 {
		t.Fatalf("AllowUnknownMethods must still warn, got %d records", len(records))
	}
}
