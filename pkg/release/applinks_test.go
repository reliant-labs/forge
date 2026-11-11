package release

import (
	"slices"
	"strings"
	"testing"
)

func TestValidateAssociatedDomain(t *testing.T) {
	for _, ok := range []string{"app.acme.example", "acme.example", "a-b.c1.example.co", "x.io"} {
		if err := ValidateAssociatedDomain(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", "localhost", "https://app.acme.example", "app.acme.example:8443", "app.acme.example/path",
		"*.acme.example", "App.Acme.Example", "10.0.0.1", "acme.example.", "-a.acme.example", "a_b.acme.example",
	} {
		if err := ValidateAssociatedDomain(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestValidateAssociatedDomains(t *testing.T) {
	https := "https://app.acme.example/oauthredirect"
	scheme := "com.acme.mobile:/oauthredirect"
	cases := []struct {
		name      string
		domains   []string
		redirects []string
		want      string
	}{
		{"no domains", nil, []string{scheme}, ""},
		{"derived redirects", []string{"app.acme.example"}, nil, ""},
		{"https first host in domains", []string{"app.acme.example"}, []string{https, scheme}, ""},
		{"second https host also in domains", []string{"app.acme.example", "acme.example"}, []string{https, "https://acme.example/oauthredirect", scheme}, ""},
		{"https host not in domains", []string{"app.acme.example"}, []string{"https://other.example/oauthredirect", scheme}, "not in associated_domains"},
		{"second https host not in domains", []string{"app.acme.example"}, []string{https, "https://other.example/x"}, "not in associated_domains"},
		{"domains but no https redirect", []string{"app.acme.example"}, []string{scheme}, "no redirect_uris entry is an https URL"},
		{"duplicate", []string{"app.acme.example", "app.acme.example"}, nil, "listed twice"},
		{"bad domain", []string{"https://app.acme.example"}, nil, "bare lowercase hostname"},
		{"host case is folded", []string{"app.acme.example"}, []string{"https://APP.acme.example/oauthredirect", scheme}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAssociatedDomains(tc.domains, tc.redirects)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestPrependAppLinkRedirect(t *testing.T) {
	scheme := "com.acme.mobile:/oauthredirect"
	link := "https://app.acme.example/oauthredirect"
	if got := PrependAppLinkRedirect(nil, []string{scheme}); !slices.Equal(got, []string{scheme}) {
		t.Errorf("no domains must change nothing, got %v", got)
	}
	if got := PrependAppLinkRedirect([]string{"app.acme.example", "acme.example"}, []string{scheme}); !slices.Equal(got, []string{link, scheme}) {
		t.Errorf("got %v, want the first domain's https redirect ahead of the private-use one", got)
	}
	if got := PrependAppLinkRedirect([]string{"app.acme.example"}, nil); !slices.Equal(got, []string{link}) {
		t.Errorf("got %v", got)
	}
	// Idempotent, and a declared order is never rearranged.
	declared := []string{scheme, link}
	if got := PrependAppLinkRedirect([]string{"app.acme.example"}, declared); !slices.Equal(got, declared) {
		t.Errorf("a list that already carries the link was changed: %v", got)
	}
	in := []string{scheme}
	_ = PrependAppLinkRedirect([]string{"app.acme.example"}, in)
	if len(in) != 1 {
		t.Error("the input slice was modified")
	}
}

func TestShape_IdentityNativeAssociatedDomainsValidate(t *testing.T) {
	const host = IdentityRuntimeHost
	cases := []struct {
		name string
		mut  func(a *ShapeIdentityApplication)
		want string
	}{
		{"https then scheme", func(a *ShapeIdentityApplication) {
			a.AssociatedDomains = []string{"app.acme.example"}
			a.RedirectURIs = []string{"https://app.acme.example/oauthredirect", "com.acme.mobile:/oauthredirect"}
		}, ""},
		{"https host outside the domains", func(a *ShapeIdentityApplication) {
			a.AssociatedDomains = []string{"app.acme.example"}
			a.RedirectURIs = []string{"https://other.example/oauthredirect"}
		}, "not in associated_domains"},
		{"wildcard domain", func(a *ShapeIdentityApplication) { a.AssociatedDomains = []string{"*.acme.example"} }, "bare lowercase hostname"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := nativeShape(host, tc.mut).validate(nil)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
	// associated_domains is a native application's: a broker may not carry them.
	id := nativeShape(host, nil)
	id.Applications[0].AssociatedDomains = []string{"app.acme.example"}
	if err := id.validate(nil); err == nil || !strings.Contains(err.Error(), "applies only to kind native") {
		t.Errorf("broker with associated_domains: err = %v", err)
	}
}
