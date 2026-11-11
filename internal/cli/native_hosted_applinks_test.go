package cli

import (
	"bytes"
	"slices"
	"testing"

	"github.com/reliant-labs/forge/pkg/release"
)

func TestHostedNativeIdentity_AppLinks(t *testing.T) {
	for _, tc := range []struct {
		name     string
		scheme   string
		declared []string
		want     []string
	}{
		{"derived HTTPS before scheme", "com.acme.mobile", nil, []string{"https://app.acme.example/oauthredirect", "com.acme.mobile:/oauthredirect"}},
		{"declared order retained", "com.acme.mobile", []string{"com.acme.mobile:/oauthredirect", "https://other.acme.example/oauthredirect"}, []string{"com.acme.mobile:/oauthredirect", "https://other.acme.example/oauthredirect"}},
		{"no scheme no derived callback", "", nil, nil},
		{"declared HTTPS without scheme", "", []string{"https://app.acme.example/oauthredirect"}, []string{"https://app.acme.example/oauthredirect"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, e, id := hostedNativeEntities(t, `{"expo":{"scheme":"`+tc.scheme+`"}}`, func(id *release.ShapeIdentity) {
				a := &id.Applications[len(id.Applications)-1]
				a.AssociatedDomains = []string{"app.acme.example", "other.acme.example"}
				a.RedirectURIs = tc.declared
			})
			resolved := hostedNativeIdentity(e, id, dir, "prod", &bytes.Buffer{})
			got := nativeApps(resolved)
			if tc.want == nil {
				if len(got) != 0 {
					t.Fatalf("unresolvable app sent: %+v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("apps = %+v", got)
			}
			if !slices.Equal(got[0].RedirectURIs, tc.want) {
				t.Errorf("redirects = %v, want %v", got[0].RedirectURIs, tc.want)
			}
			var logouts []string
			for _, r := range tc.want {
				logouts = append(logouts, release.NativeSignedOutBeside(r))
			}
			if !slices.Equal(got[0].LogoutURIs, logouts) {
				t.Errorf("logouts = %v, want %v", got[0].LogoutURIs, logouts)
			}
			if !slices.Equal(nativeApps(id)[0].RedirectURIs, tc.declared) {
				t.Error("input identity modified")
			}
			if err := validateHostedShape(resolved); err != nil {
				t.Fatal(err)
			}
		})
	}
}
