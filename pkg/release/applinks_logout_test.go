package release_test

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/release"
)

func TestPublicShapeNativeLogoutAssociatedHost(t *testing.T) {
	for _, tc := range []struct {
		name    string
		logouts []string
		want    string
	}{
		{"foreign https", []string{"https://other.example/signedout"}, "logout_uris"},
		{"later foreign https", []string{"https://app.example/signedout", "https://other.example/signedout"}, "not in associated_domains"},
		{"member https", []string{"https://app.example/signedout"}, ""},
		{"private use only", []string{"com.acme.mobile:/signedout"}, ""},
		{"derived logout", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := release.Shape{Kind: release.EnvLocal, Identity: &release.ShapeIdentity{Name: "identity", Runtime: release.IdentityRuntimeHost, Login: release.ShapeIdentityLogin{Mode: release.IdentityLoginNative}, Applications: []release.ShapeIdentityApplication{{Name: "dev:mobile", Kind: release.IdentityAppNative, Frontend: "mobile", AssociatedDomains: []string{"app.example"}, RedirectURIs: []string{"https://app.example/oauthredirect", "com.acme.mobile:/oauthredirect"}, LogoutURIs: tc.logouts}}}}
			err := s.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want %s", err, tc.want)
			}
		})
	}
}
