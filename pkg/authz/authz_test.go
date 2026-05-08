package authz

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/reliant-labs/forge/pkg/auth"
)

// helperLookup is a per-test claims-lookup wired into the package-level
// claimsLookup via SetClaimsLookup. Tests run sequentially so cross-test
// interference is bounded; t.Cleanup restores the previous value.
func wireLookup(t *testing.T, fn func(context.Context) (*auth.Claims, bool)) {
	t.Helper()
	prev := claimsLookup
	SetClaimsLookup(fn)
	t.Cleanup(func() { claimsLookup = prev })
}

type ctxKey struct{}

func putClaims(ctx context.Context, c *auth.Claims) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}
func lookupClaims(ctx context.Context) (*auth.Claims, bool) {
	c, ok := ctx.Value(ctxKey{}).(*auth.Claims)
	return c, ok
}

func TestNew_NilDeciderPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("New(nil) should panic")
		}
	}()
	_ = New(nil)
}

func TestDenyAll_DeniesByDefault(t *testing.T) {
	a := New(DenyAll{})
	wireLookup(t, lookupClaims)

	if err := a.CanAccess(context.Background(), "/svc/Foo"); err == nil {
		t.Fatal("DenyAll.CanAccess: expected error, got nil")
	}
	if err := a.Can(context.Background(), &auth.Claims{Role: "admin"}, "create", "user"); err == nil {
		t.Fatal("DenyAll.Can(admin): expected error, got nil")
	}
}

func TestAllowAll_AllowsEverything(t *testing.T) {
	a := New(AllowAll{})
	wireLookup(t, lookupClaims)

	if err := a.CanAccess(context.Background(), "/svc/Foo"); err != nil {
		t.Fatalf("AllowAll.CanAccess: %v", err)
	}
	if err := a.Can(context.Background(), nil, "delete", "world"); err != nil {
		t.Fatalf("AllowAll.Can: %v", err)
	}
}

func TestCanAccess_EmptyProcedureDenied(t *testing.T) {
	a := New(AllowAll{})
	err := a.CanAccess(context.Background(), "")
	if err == nil {
		t.Fatal("CanAccess(\"\"): expected deny, got nil")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodePermissionDenied {
		t.Fatalf("CanAccess(\"\"): want CodePermissionDenied, got %v", err)
	}
}

func TestCanAccess_ClaimsFlowFromContext(t *testing.T) {
	wireLookup(t, lookupClaims)
	var seen *auth.Claims
	a := New(DeciderFunc(func(ctx context.Context, method string, claims *auth.Claims) error {
		seen = claims
		return nil
	}))
	want := &auth.Claims{UserID: "u1", Role: "admin"}
	ctx := putClaims(context.Background(), want)
	if err := a.CanAccess(ctx, "/svc/Foo"); err != nil {
		t.Fatalf("CanAccess: %v", err)
	}
	if seen != want {
		t.Fatalf("decider received claims %+v, want %+v", seen, want)
	}
}

func TestCan_NilClaimsRewrappedAsUnauthenticated(t *testing.T) {
	a := New(DeciderFunc(func(ctx context.Context, method string, claims *auth.Claims) error {
		return Deny("nope")
	}))
	err := a.Can(context.Background(), nil, "create", "user")
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeUnauthenticated {
		t.Fatalf("want CodeUnauthenticated, got %v", err)
	}
}

func TestCan_DeciderConnectErrorPreserved(t *testing.T) {
	want := connect.NewError(connect.CodeInvalidArgument, errors.New("bad shape"))
	a := New(DeciderFunc(func(ctx context.Context, method string, claims *auth.Claims) error {
		return want
	}))
	// nil claims would normally re-wrap as Unauthenticated; explicit
	// connect.Error is preserved across that path.
	err := a.Can(context.Background(), nil, "create", "user")
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeInvalidArgument {
		t.Fatalf("connect.Error code lost: got %v", err)
	}
}

func TestCan_MethodIdentifierFormat(t *testing.T) {
	var got string
	a := New(DeciderFunc(func(ctx context.Context, method string, claims *auth.Claims) error {
		got = method
		return nil
	}))
	if err := a.Can(context.Background(), &auth.Claims{}, "create", "patient"); err != nil {
		t.Fatalf("Can: %v", err)
	}
	if got != "create:patient" {
		t.Fatalf("method = %q, want create:patient", got)
	}
}

func TestPanicRecovery_DeciderPanicBecomesPermissionDenied(t *testing.T) {
	a := New(DeciderFunc(func(ctx context.Context, method string, claims *auth.Claims) error {
		panic("kaboom")
	}))
	err := a.Can(context.Background(), &auth.Claims{}, "create", "x")
	if err == nil {
		t.Fatal("panic should produce an error")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodePermissionDenied {
		t.Fatalf("want CodePermissionDenied from panic, got %v", err)
	}
	if !strings.Contains(err.Error(), "kaboom") {
		t.Fatalf("want panic message threaded through, got %q", err.Error())
	}
}

func TestRolesDecider_AllowMatch(t *testing.T) {
	d := RolesDecider{
		MethodRoles: map[string][]string{
			"/svc/Admin": {"admin"},
		},
	}
	a := New(d)
	wireLookup(t, lookupClaims)
	ctx := putClaims(context.Background(), &auth.Claims{Role: "admin"})
	if err := a.CanAccess(ctx, "/svc/Admin"); err != nil {
		t.Fatalf("admin should be allowed: %v", err)
	}
}

func TestRolesDecider_AllowFromRolesSlice(t *testing.T) {
	d := RolesDecider{
		MethodRoles: map[string][]string{
			"/svc/Admin": {"admin"},
		},
	}
	a := New(d)
	wireLookup(t, lookupClaims)
	ctx := putClaims(context.Background(), &auth.Claims{Roles: []string{"reader", "admin"}})
	if err := a.CanAccess(ctx, "/svc/Admin"); err != nil {
		t.Fatalf("user with role in Roles slice should be allowed: %v", err)
	}
}

func TestRolesDecider_DenyOnRoleMismatch(t *testing.T) {
	d := RolesDecider{
		MethodRoles: map[string][]string{
			"/svc/Admin": {"admin"},
		},
	}
	a := New(d)
	wireLookup(t, lookupClaims)
	ctx := putClaims(context.Background(), &auth.Claims{Role: "viewer"})
	err := a.CanAccess(ctx, "/svc/Admin")
	if err == nil {
		t.Fatal("viewer should be denied")
	}
}

func TestRolesDecider_UnknownMethodFallsThroughToDefault(t *testing.T) {
	// Default nil → deny (legacy fail-closed behaviour).
	d := RolesDecider{
		MethodRoles: map[string][]string{
			"/svc/Known": {"admin"},
		},
	}
	a := New(d)
	wireLookup(t, lookupClaims)
	ctx := putClaims(context.Background(), &auth.Claims{Role: "admin"})
	if err := a.CanAccess(ctx, "/svc/Unknown"); err == nil {
		t.Fatal("unknown method should deny when Default is nil")
	}
}

func TestRolesDecider_DefaultEmptySliceAllowsAuthenticated(t *testing.T) {
	d := RolesDecider{
		MethodRoles: map[string][]string{},
		Default:     []string{}, // any authenticated user
	}
	a := New(d)
	wireLookup(t, lookupClaims)
	ctx := putClaims(context.Background(), &auth.Claims{UserID: "u1"})
	if err := a.CanAccess(ctx, "/svc/Whatever"); err != nil {
		t.Fatalf("authenticated user should be allowed via empty default: %v", err)
	}
}

func TestRolesDecider_NilClaimsIsUnauthenticated(t *testing.T) {
	d := RolesDecider{
		MethodRoles: map[string][]string{"/svc/X": {"admin"}},
	}
	a := New(d)
	wireLookup(t, lookupClaims)
	// No claims in context.
	err := a.CanAccess(context.Background(), "/svc/X")
	if err == nil {
		t.Fatal("expected error for unauthenticated request")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeUnauthenticated {
		t.Fatalf("want CodeUnauthenticated for nil claims, got %v", err)
	}
}

func TestSetClaimsLookup_NilDisablesLookup(t *testing.T) {
	prev := claimsLookup
	t.Cleanup(func() { claimsLookup = prev })
	SetClaimsLookup(nil)
	if c, ok := ClaimsFromContext(context.Background()); c != nil || ok {
		t.Fatalf("after SetClaimsLookup(nil), got (%v, %v); want (nil, false)", c, ok)
	}
}

func TestRolesDecider_MethodAuthRequired_OptOut(t *testing.T) {
	d := RolesDecider{
		MethodRoles: map[string][]string{
			"/svc/Open":      {},
			"/svc/Protected": {"admin"},
		},
		MethodAuthRequired: map[string]bool{
			"/svc/Open":      false,
			"/svc/Protected": true,
		},
	}
	a := New(d)
	wireLookup(t, lookupClaims)
	// Open method allows even unauthenticated requests.
	if err := a.CanAccess(context.Background(), "/svc/Open"); err != nil {
		t.Fatalf("auth-not-required method should allow unauth: %v", err)
	}
	// Protected method denies without claims.
	if err := a.CanAccess(context.Background(), "/svc/Protected"); err == nil {
		t.Fatal("auth-required method should deny without claims")
	}
}

func TestRolesDecider_MethodAuthRequired_UnknownDenies(t *testing.T) {
	d := RolesDecider{
		MethodAuthRequired: map[string]bool{
			"/svc/Known": true,
		},
		MethodRoles: map[string][]string{},
	}
	a := New(d)
	wireLookup(t, lookupClaims)
	ctx := putClaims(context.Background(), &auth.Claims{Role: "admin"})
	if err := a.CanAccess(ctx, "/svc/Unknown"); err == nil {
		t.Fatal("unknown method should deny when MethodAuthRequired is set")
	}
}

func TestDecidedErrorWrappedAsPermissionDenied(t *testing.T) {
	a := New(DeciderFunc(func(ctx context.Context, method string, claims *auth.Claims) error {
		return errors.New("custom denial")
	}))
	err := a.CanAccess(context.Background(), "/svc/Foo")
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodePermissionDenied {
		t.Fatalf("plain error not wrapped to CodePermissionDenied: got %v", err)
	}
}
