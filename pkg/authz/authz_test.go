package authz

import (
	"context"
	"errors"
	"log/slog"
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
	// Default nil + FailMode=FailClosed → deny. FailClosed is the zero
	// value; it's spelled out here for readability. The zero-value path
	// is pinned by TestRolesDecider_ZeroValueFailsClosed.
	d := RolesDecider{
		MethodRoles: map[string][]string{
			"/svc/Known": {"admin"},
		},
		FailMode: FailClosed,
	}
	a := New(d)
	wireLookup(t, lookupClaims)
	ctx := putClaims(context.Background(), &auth.Claims{Role: "admin"})
	if err := a.CanAccess(ctx, "/svc/Unknown"); err == nil {
		t.Fatal("unknown method should deny when Default is nil and FailMode=FailClosed")
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
	// Explicit FailClosed (also the zero value).
	d := RolesDecider{
		MethodAuthRequired: map[string]bool{
			"/svc/Known": true,
		},
		MethodRoles: map[string][]string{},
		FailMode:    FailClosed,
	}
	a := New(d)
	wireLookup(t, lookupClaims)
	ctx := putClaims(context.Background(), &auth.Claims{Role: "admin"})
	if err := a.CanAccess(ctx, "/svc/Unknown"); err == nil {
		t.Fatal("unknown method should deny when MethodAuthRequired is set and FailMode=FailClosed")
	}
}

// The zero-value (FailClosed) and AllowUnknownMethods opt-in contracts
// are pinned in failmode_test.go.

// captureSlog swaps slog.Default() for a handler that records every
// emitted record, then restores it on test cleanup. Returned getRecords
// snapshots the slice so callers can compare without racing the
// background writer (we're synchronous here, but the pattern stays
// idiomatic).
func captureSlog(t *testing.T) (getRecords func() []slog.Record) {
	t.Helper()
	prev := slog.Default()
	var records []slog.Record
	h := &captureHandler{records: &records}
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []slog.Record {
		out := make([]slog.Record, len(records))
		copy(out, records)
		return out
	}
}

type captureHandler struct {
	records *[]slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	*h.records = append(*h.records, r)
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// TestRolesDecider_UnknownMethod_DefaultEmitsSlogWarn pins the
// loud-by-default contract. When an RPC reaches the authorizer with
// no entry in MethodAuthRequired the deny is correct (fail-closed)
// BUT silent — the only signal pre-this-change was the 403 in the
// response. Now the same deny also fires a slog.Warn so the foot-gun
// (stale proto codegen, hand-mounted endpoint outside the proto)
// shows up in server logs where on-call greps live.
func TestRolesDecider_UnknownMethod_DefaultEmitsSlogWarn(t *testing.T) {
	resetUnknownMethodWarnings()
	getRecords := captureSlog(t)

	// Explicit FailClosed — exercise the deny path so the slog assertion
	// is decoupled from the FailMode default (which now allows).
	d := RolesDecider{
		MethodAuthRequired: map[string]bool{"/svc/Known": true},
		MethodRoles:        map[string][]string{"/svc/Known": {}},
		FailMode:           FailClosed,
	}
	a := New(d)
	wireLookup(t, lookupClaims)
	ctx := putClaims(context.Background(), &auth.Claims{Role: "admin"})

	// Expected deny — that part is the existing contract for FailClosed.
	if err := a.CanAccess(ctx, "/svc/UnknownProc"); err == nil {
		t.Fatal("unknown method should deny under FailClosed")
	}

	records := getRecords()
	if len(records) != 1 {
		t.Fatalf("expected exactly one slog record, got %d", len(records))
	}
	r := records[0]
	if r.Level != slog.LevelWarn {
		t.Errorf("expected Warn level, got %v", r.Level)
	}
	// Must name the offending procedure so a grep against the access
	// log instantly points at the regen / proto-drift problem.
	var attrMethod string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "method" {
			attrMethod = a.Value.String()
		}
		return true
	})
	if attrMethod != "/svc/UnknownProc" {
		t.Errorf("expected method=/svc/UnknownProc, got %q", attrMethod)
	}
	if !strings.Contains(r.Message, "forge generate") {
		t.Errorf("warn message should point at the regen workflow, got: %s", r.Message)
	}
}

// TestRolesDecider_UnknownMethod_OnUnknownOverridesDefault: projects
// that want a different log shape (or rate-limited / silenced under
// random-procedure attack traffic) wire a custom OnUnknownMethod.
// When set, the default slog warn is suppressed — otherwise we'd
// double-log, defeating the override's purpose.
func TestRolesDecider_UnknownMethod_OnUnknownOverridesDefault(t *testing.T) {
	resetUnknownMethodWarnings()
	getRecords := captureSlog(t)

	var seen []string
	d := RolesDecider{
		MethodAuthRequired: map[string]bool{"/svc/Known": true},
		MethodRoles:        map[string][]string{"/svc/Known": {}},
		OnUnknownMethod:    func(m string) { seen = append(seen, m) },
		FailMode:           FailClosed,
	}
	a := New(d)
	wireLookup(t, lookupClaims)
	ctx := putClaims(context.Background(), &auth.Claims{Role: "admin"})

	if err := a.CanAccess(ctx, "/svc/UnknownProc"); err == nil {
		t.Fatal("unknown method should deny under FailClosed")
	}

	if len(seen) != 1 || seen[0] != "/svc/UnknownProc" {
		t.Errorf("custom OnUnknownMethod should fire with method, got %v", seen)
	}
	if records := getRecords(); len(records) != 0 {
		t.Errorf("default slog warn should be suppressed when OnUnknownMethod is set, got %d records", len(records))
	}
}

// TestRolesDecider_UnknownMethod_BothBranchesWarn: there are two
// distinct unknown-method branches in Decide — the MethodAuthRequired
// miss AND the MethodRoles miss with nil Default. Both have the same
// operator-visible symptom ("regen or check the proto"), so both must
// emit the warn or the signal would be inconsistent depending on
// which map happened to be populated.
func TestRolesDecider_UnknownMethod_BothBranchesWarn(t *testing.T) {
	t.Run("MethodAuthRequired-miss", func(t *testing.T) {
		resetUnknownMethodWarnings()
		var seen []string
		d := RolesDecider{
			MethodAuthRequired: map[string]bool{"/svc/Known": true},
			OnUnknownMethod:    func(m string) { seen = append(seen, m) },
		}
		a := New(d)
		wireLookup(t, lookupClaims)
		ctx := putClaims(context.Background(), &auth.Claims{Role: "admin"})
		_ = a.CanAccess(ctx, "/svc/Unknown")
		if len(seen) != 1 {
			t.Errorf("MethodAuthRequired-miss branch must warn, got %v", seen)
		}
	})
	t.Run("MethodRoles-miss-nil-default", func(t *testing.T) {
		resetUnknownMethodWarnings()
		// No MethodAuthRequired → skip the first branch. MethodRoles is
		// empty + Default nil → second branch fires the deny.
		var seen []string
		d := RolesDecider{
			MethodRoles:     map[string][]string{},
			Default:         nil,
			OnUnknownMethod: func(m string) { seen = append(seen, m) },
		}
		a := New(d)
		wireLookup(t, lookupClaims)
		ctx := putClaims(context.Background(), &auth.Claims{Role: "admin"})
		_ = a.CanAccess(ctx, "/svc/Unknown")
		if len(seen) != 1 {
			t.Errorf("MethodRoles-miss-with-nil-Default branch must warn, got %v", seen)
		}
	})
}

// TestRolesDecider_KnownMethod_NoWarn confirms the happy path stays
// quiet. Emitting a warn on every allowed call would drown out the
// signal we actually care about.
func TestRolesDecider_KnownMethod_NoWarn(t *testing.T) {
	getRecords := captureSlog(t)

	d := RolesDecider{
		MethodAuthRequired: map[string]bool{"/svc/Known": true},
		MethodRoles:        map[string][]string{"/svc/Known": {"admin"}},
	}
	a := New(d)
	wireLookup(t, lookupClaims)
	ctx := putClaims(context.Background(), &auth.Claims{Role: "admin"})
	if err := a.CanAccess(ctx, "/svc/Known"); err != nil {
		t.Fatalf("known method should allow, got: %v", err)
	}
	if records := getRecords(); len(records) != 0 {
		t.Errorf("known-method allow path must not warn, got %d records", len(records))
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
