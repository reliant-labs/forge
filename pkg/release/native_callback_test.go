package release

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestValidateNativeCallbackURI(t *testing.T) {
	cases := []struct {
		uri       string
		allowHTTP bool
		want      string // "" = accepted, else a substring of the refusal
	}{
		{"com.acme.app:/oauthredirect", false, ""},
		{"com.acme.app:/signedout", false, ""},
		{"io.github.acme-labs.app:/oauthredirect", false, ""},
		{"https://acme.example/oauthredirect", false, ""},
		{"https://acme.example:8443/cb/oauthredirect", false, ""},
		{"https://acme.example", false, ""},
		{"http://localhost:8081/oauthredirect", true, ""},
		{"http://localhost:8081/oauthredirect", false, "http is accepted only"},
		{"http://acme.example/cb", false, "http is accepted only"},
		{"myapp:/oauthredirect", false, "reverse-DNS"},
		{"myapp://oauthredirect", false, "reverse-DNS"},
		{"com.acme.app://oauthredirect", false, "single slash"},
		{"com.acme.app://host/oauthredirect", false, "single slash"},
		{"com.acme.app:oauthredirect", false, "single slash"},
		{"com.acme.app:/", false, "single slash"},
		{"Com.Acme.App:/oauthredirect", false, "lowercase"},
		{"https://acme.example/*", false, "wildcard"},
		{"https://*.acme.example/cb", false, "wildcard"},
		{"https://acme.example/cb?x=1", false, "wildcard, query"},
		{"https://acme.example/cb#frag", false, "wildcard, query"},
		{"https://user:pw@acme.example/cb", false, "credentials"},
		{"https:///cb", false, "host"},
		{"javascript:alert(1)", false, "reverse-DNS"},
		{"/oauthredirect", false, "absolute"},
		{"", false, "empty"},
		{" com.acme.app:/oauthredirect", false, "whitespace"},
	}
	for _, tc := range cases {
		t.Run(tc.uri, func(t *testing.T) {
			err := ValidateNativeCallbackURI(tc.uri, tc.allowHTTP)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func nativeShape(rt string, mut func(a *ShapeIdentityApplication)) ShapeIdentity {
	a := ShapeIdentityApplication{Name: "prod:mobile", Kind: IdentityAppNative, Frontend: "mobile",
		RedirectURIs: []string{"com.acme.mobile:/oauthredirect"}, LogoutURIs: []string{"com.acme.mobile:/signedout"}}
	if mut != nil {
		mut(&a)
	}
	return ShapeIdentity{
		Name: "identity", Runtime: rt, Login: ShapeIdentityLogin{Mode: IdentityLoginNative},
		Applications: []ShapeIdentityApplication{{Name: "prod:api", Workload: "api", Kind: IdentityAppNativeBroker}, a},
	}
}

func TestShape_IdentityNativeApplicationValidate(t *testing.T) {
	const host, hosted = IdentityRuntimeHost, IdentityRuntimeHosted
	cases := []struct {
		name string
		rt   string
		mut  func(a *ShapeIdentityApplication)
		want string
	}{
		{"scheme only", host, nil, ""},
		{"scheme then https, in order", host, func(a *ShapeIdentityApplication) {
			a.RedirectURIs = append(a.RedirectURIs, "https://acme.example/oauthredirect")
		}, ""},
		{"https only", host, func(a *ShapeIdentityApplication) {
			a.RedirectURIs, a.LogoutURIs = []string{"https://acme.example/oauthredirect"}, []string{"https://acme.example/signedout"}
		}, ""},
		{"host runtime derives its callbacks", host, func(a *ShapeIdentityApplication) { a.RedirectURIs, a.LogoutURIs = nil, nil }, ""},
		{"host runtime accepts http", host, func(a *ShapeIdentityApplication) {
			a.RedirectURIs = []string{"http://localhost:8081/oauthredirect"}
		}, ""},
		{"hosted identity is refused, however complete the declaration", hosted, nil, "not yet supported on hosted identity"},
		{"hosted identity is refused with no callbacks too", hosted, func(a *ShapeIdentityApplication) { a.RedirectURIs, a.LogoutURIs = nil, nil }, "not yet supported on hosted identity"},
		{"the refusal names the skill", hosted, nil, "deploy/managed-identity/mobile"},
		{"needs a frontend", host, func(a *ShapeIdentityApplication) { a.Frontend = "" }, "needs the native frontend"},
		{"no workload", host, func(a *ShapeIdentityApplication) { a.Workload = "api" }, "no broker workload"},
		{"no login uri", host, func(a *ShapeIdentityApplication) { a.LoginURI = "https://acme.example/auth/sign-in" }, "no login_uri"},
		{"single-label scheme", host, func(a *ShapeIdentityApplication) { a.RedirectURIs = []string{"myapp:/oauthredirect"} }, "reverse-DNS"},
		{"bad logout", host, func(a *ShapeIdentityApplication) { a.LogoutURIs = []string{"com.acme.mobile://signedout"} }, "logout_uris"},
		{"broker app may not name a frontend", hosted, func(a *ShapeIdentityApplication) {
			a.Kind, a.Workload, a.RedirectURIs = IdentityAppNativeBroker, "api", nil
		}, "frontend applies only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := nativeShape(tc.rt, tc.mut)
			err := id.validate(nil)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "" && err == nil:
				t.Fatalf("accepted, want a refusal containing %q", tc.want)
			case tc.want != "" && (!errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want ErrInvalid containing %q", err, tc.want)
			}
		})
	}
}

func TestShape_IdentityNativeCanonicalKeepsRedirectOrder(t *testing.T) {
	id := nativeShape(IdentityRuntimeHosted, func(a *ShapeIdentityApplication) {
		a.RedirectURIs = []string{"https://acme.example/oauthredirect", "com.acme.mobile:/oauthredirect", "https://acme.example/oauthredirect", ""}
	})
	c := id.canonical()
	var native ShapeIdentityApplication
	for _, a := range c.Applications {
		if a.Kind == IdentityAppNative {
			native = a
		}
	}
	if strings.Join(native.RedirectURIs, " ") != "https://acme.example/oauthredirect com.acme.mobile:/oauthredirect" {
		t.Errorf("redirects = %v: the first is the app's preferred, so order is data", native.RedirectURIs)
	}
}

// Native applications reach no control plane until one registers them: the
// feature is defined and gates the field, but BundleDocFeatures does not
// advertise it, so a control plane that merely decodes the field is not sent a
// client it cannot register.
func TestLimitShape_NativeApplicationsAreWithheldFromEveryAdvertisedReader(t *testing.T) {
	id := nativeShape(IdentityRuntimeHost, nil)
	full := Shape{Kind: EnvLocal, Identity: &id}

	if slices.Contains(BundleDocFeatures(), BundleFeatureIdentityNativeApp) {
		t.Fatalf("%s is advertised: a control plane would claim to register native clients it cannot", BundleFeatureIdentityNativeApp)
	}
	for name, r := range map[string]*BundleReader{
		"names no feature":           {Features: []string{}},
		"names every advertised one": {Features: BundleDocFeatures()},
	} {
		got := r.LimitShape(full)
		if len(got.Identity.Applications) != 1 || got.Identity.Applications[0].Kind != IdentityAppNativeBroker {
			t.Errorf("%s: a native application was sent: %+v", name, got.Identity.Applications)
		}
		if names := r.WithheldNativeApplications(full); len(names) != 1 || names[0] != "prod:mobile" {
			t.Errorf("%s: withheld = %v, want [prod:mobile] so the caller can say so", name, names)
		}
	}
	if len(id.Applications) != 2 {
		t.Error("LimitShape modified its input")
	}

	// The day a control plane names the feature (it registers native clients),
	// it is sent them: the gate is the feature, not a hard-coded refusal.
	named := &BundleReader{Features: append(BundleDocFeatures(), BundleFeatureIdentityNativeApp)}
	if got := named.LimitShape(full); len(got.Identity.Applications) != 2 {
		t.Errorf("a reader naming the feature lost an application: %+v", got.Identity.Applications)
	}
	if names := named.WithheldNativeApplications(full); len(names) != 0 {
		t.Errorf("withheld = %v for a reader that registers them", names)
	}
	if got := (*BundleReader)(nil).LimitShape(full); len(got.Identity.Applications) != 2 {
		t.Error("the machine ledger (nil reader) decodes every feature and keeps them")
	}
	if !full.HasOptionalParts() {
		t.Error("a native application is an optional part: sending it needs the reader's feature list first")
	}
	if names := (&BundleReader{}).WithheldNativeApplications(Shape{Kind: EnvLocal}); names != nil {
		t.Errorf("no identity, nothing withheld, got %v", names)
	}
}

func TestIsNativePrivateScheme(t *testing.T) {
	for s, want := range map[string]bool{
		"com.acme.mobile": true, "com.my-app.mobile": true, "io.github.acme-labs.app": true,
		"mobile": false, "com.MyApp.mobile": false, "com.my_app.mobile": false, "": false, "com.": false, ".com.acme": false,
	} {
		if got := IsNativePrivateScheme(s); got != want {
			t.Errorf("IsNativePrivateScheme(%q) = %v, want %v", s, got, want)
		}
		// The predicate and the callback validator are one rule.
		if (ValidateNativeCallbackURI(s+":/oauthredirect", false) == nil) != want {
			t.Errorf("ValidateNativeCallbackURI disagrees on %q", s)
		}
	}
}
