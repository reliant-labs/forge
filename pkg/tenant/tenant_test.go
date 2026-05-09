package tenant

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/reliant-labs/forge/pkg/auth"
)

type ctxClaimsKey struct{}

func putClaims(ctx context.Context, c *auth.Claims) context.Context {
	return context.WithValue(ctx, ctxClaimsKey{}, c)
}
func lookupClaims(ctx context.Context) (*auth.Claims, bool) {
	c, ok := ctx.Value(ctxClaimsKey{}).(*auth.Claims)
	return c, ok
}

func TestWithTenantIDAndFromContext(t *testing.T) {
	ctx := WithTenantID(context.Background(), "tenant-42")
	if got := FromContext(ctx); got != "tenant-42" {
		t.Errorf("FromContext = %q, want tenant-42", got)
	}
}

func TestRequire(t *testing.T) {
	if _, err := Require(context.Background()); err == nil {
		t.Error("expected error when no tenant in context")
	}
	ctx := WithTenantID(context.Background(), "abc")
	id, err := Require(ctx)
	if err != nil || id != "abc" {
		t.Errorf("Require = %q, %v; want abc, nil", id, err)
	}
}

func TestExtractClaim(t *testing.T) {
	c := &auth.Claims{UserID: "u", Email: "e", OrgID: "o", Role: "r"}
	cases := map[string]string{
		"org_id":  "o",
		"sub":     "u",
		"subject": "u",
		"user_id": "u",
		"email":   "e",
		"role":    "r",
		"unknown": "o", // fallback
	}
	for field, want := range cases {
		if got := ExtractClaim(c, field); got != want {
			t.Errorf("ExtractClaim(%q) = %q, want %q", field, got, want)
		}
	}
	if got := ExtractClaim(nil, "org_id"); got != "" {
		t.Errorf("ExtractClaim(nil) = %q, want empty", got)
	}
}

func TestEffectiveDefaults(t *testing.T) {
	if (Config{}).EffectiveClaimField() != "org_id" {
		t.Error("default ClaimField should be org_id")
	}
	if (Config{}).EffectiveColumnName() != "org_id" {
		t.Error("default ColumnName should be org_id")
	}
	if (Config{ClaimField: "tenant_id"}).EffectiveClaimField() != "tenant_id" {
		t.Error("custom ClaimField not honoured")
	}
}

func TestApplyToContext_NoClaims_PassesThrough(t *testing.T) {
	ctx, err := ApplyToContext(context.Background(), Config{}, lookupClaims)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if FromContext(ctx) != "" {
		t.Error("expected empty tenant id when no claims")
	}
}

func TestApplyToContext_ExtractsTenantFromOrgID(t *testing.T) {
	ctx := putClaims(context.Background(), &auth.Claims{OrgID: "org-42"})
	out, err := ApplyToContext(ctx, Config{}, lookupClaims)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if FromContext(out) != "org-42" {
		t.Errorf("FromContext = %q, want org-42", FromContext(out))
	}
}

func TestApplyToContext_MissingTenantClaim_PermissionDenied(t *testing.T) {
	ctx := putClaims(context.Background(), &auth.Claims{})
	_, err := ApplyToContext(ctx, Config{}, lookupClaims)
	if err == nil || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
}

func TestApplyToContext_OptionalAllowsMissingTenant(t *testing.T) {
	ctx := putClaims(context.Background(), &auth.Claims{})
	out, err := ApplyToContext(ctx, Config{Optional: true}, lookupClaims)
	if err != nil {
		t.Errorf("optional mode should pass through, got: %v", err)
	}
	if FromContext(out) != "" {
		t.Errorf("expected no tenant id in optional missing-claim mode")
	}
}

func TestApplyToContext_CustomExtract(t *testing.T) {
	ctx := putClaims(context.Background(), &auth.Claims{UserID: "u9"})
	out, err := ApplyToContext(ctx, Config{
		ClaimField: "tenant_id",
		Extract: func(c *auth.Claims, field string) string {
			if field == "tenant_id" {
				return c.UserID + "-shadow"
			}
			return ""
		},
	}, lookupClaims)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if FromContext(out) != "u9-shadow" {
		t.Errorf("custom extract not used: got %q", FromContext(out))
	}
}

func TestNewInterceptor_Type(t *testing.T) {
	ic := NewInterceptor(Config{}, lookupClaims)
	if ic == nil {
		t.Fatal("nil interceptor")
	}
	wrapped := ic.WrapUnary(func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, nil
	})
	if wrapped == nil {
		t.Fatal("WrapUnary returned nil")
	}
}
