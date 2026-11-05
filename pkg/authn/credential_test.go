package authn

import (
	"context"
	"testing"

	"github.com/reliant-labs/forge/pkg/auth"
)

// Enrich sees the credential the validator proved, and cannot change it:
// an enricher that builds fresh claims for a key keeps the key's
// credential, and one that copies a key credential onto a token's claims
// is overruled.
func TestAuthenticate_EnrichCannotChangeTheCredential(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		validated *auth.Claims
		enrich    func(*auth.Claims) *auth.Claims
		wantKey   bool
	}{
		"fresh claims for a key stay a key": {
			validated: &auth.Claims{UserID: "apikey:abcd1234", Credential: auth.APIKeyCredential("abcd1234")},
			enrich:    func(*auth.Claims) *auth.Claims { return &auth.Claims{UserID: "apikey:abcd1234", OrgID: "t1"} },
			wantKey:   true,
		},
		"a token cannot be enriched into a key": {
			validated: &auth.Claims{UserID: "u1", Credential: auth.TokenCredential()},
			enrich: func(c *auth.Claims) *auth.Claims {
				out := *c
				out.Credential = auth.APIKeyCredential("forged12")
				return &out
			},
			wantKey: false,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var sawKey bool
			p := validatePolicy(func(string) (*auth.Claims, error) { return tc.validated, nil })
			p.ContextWithClaims = ContextWithClaims
			p.Enrich = func(_ context.Context, c *auth.Claims) (*auth.Claims, error) {
				sawKey = c.IsAPIKey()
				return tc.enrich(c), nil
			}
			a := &interceptor{policy: p}

			ctx, err := a.authenticate(context.Background(), "Bearer x")
			if err != nil {
				t.Fatalf("authenticate: %v", err)
			}
			if sawKey != tc.validated.IsAPIKey() {
				t.Fatalf("Enrich saw IsAPIKey=%v, validated claims say %v", sawKey, tc.validated.IsAPIKey())
			}
			got, _ := ClaimsFromContext(ctx)
			if got.Credential != tc.validated.Credential {
				t.Fatalf("stashed credential %v, want the validated %v", got.Credential.Kind(), tc.validated.Credential.Kind())
			}
			if IsAPIKey(ctx) != tc.wantKey {
				t.Fatalf("IsAPIKey(ctx) = %v, want %v", IsAPIKey(ctx), tc.wantKey)
			}
			if tc.wantKey && got.OrgID != "t1" {
				t.Fatalf("Enrich's other fields were lost: %+v", got)
			}
		})
	}
}

func TestIsAPIKey_NoClaims(t *testing.T) {
	t.Parallel()
	if IsAPIKey(context.Background()) {
		t.Fatal("a context with no claims is not an API key")
	}
	ctx := ContextWithClaims(context.Background(), &auth.Claims{UserID: "u", Credential: auth.APIKeyCredential("abcd1234")})
	if !IsAPIKey(ctx) {
		t.Fatal("API-key claims on the context: IsAPIKey must be true")
	}
}
