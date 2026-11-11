package release

import (
	"encoding/json"
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
		{"hosted identity accepts a complete declaration", hosted, nil, ""},
		{"hosted identity accepts https beside the scheme, in order", hosted, func(a *ShapeIdentityApplication) {
			a.RedirectURIs = []string{"https://acme.example/oauthredirect", "com.acme.mobile:/oauthredirect"}
			a.LogoutURIs = []string{"https://acme.example/signedout", "com.acme.mobile:/signedout"}
		}, ""},
		{"hosted identity needs redirects resolved", hosted, func(a *ShapeIdentityApplication) { a.RedirectURIs = nil }, "needs both redirect_uris and logout_uris resolved"},
		{"hosted identity needs logouts resolved", hosted, func(a *ShapeIdentityApplication) { a.LogoutURIs = nil }, "needs both redirect_uris and logout_uris resolved"},
		{"hosted identity needs both resolved, never defaulted", hosted, func(a *ShapeIdentityApplication) { a.RedirectURIs, a.LogoutURIs = nil, nil }, "needs both redirect_uris and logout_uris resolved"},
		{"hosted identity refuses http redirect", hosted, func(a *ShapeIdentityApplication) {
			a.RedirectURIs = []string{"http://localhost:8081/oauthredirect"}
		}, "http is accepted only"},
		{"hosted identity refuses http logout", hosted, func(a *ShapeIdentityApplication) {
			a.LogoutURIs = []string{"http://localhost:8081/signedout"}
		}, "http is accepted only"},
		{"hosted identity refuses a single-label scheme", hosted, func(a *ShapeIdentityApplication) { a.RedirectURIs = []string{"myapp:/oauthredirect"} }, "reverse-DNS"},
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

// The capability gate. This forge advertises the feature (a control plane
// built from it names it), but a reader that does not name it (an older control
// plane built from a forge without it, or one that withholds it) is NEVER sent
// a native application: LimitShape removes it, for a hosted identity and a
// host-run one alike, and WithheldNativeApplications names what it removed.
func TestLimitShape_NativeApplicationsReachOnlyAReaderThatNamesTheFeature(t *testing.T) {
	if !slices.Contains(BundleDocFeatures(), BundleFeatureIdentityNativeApp) {
		t.Fatalf("%s is not advertised by BundleDocFeatures", BundleFeatureIdentityNativeApp)
	}
	var withoutIt []string
	for _, f := range BundleDocFeatures() {
		if f != BundleFeatureIdentityNativeApp {
			withoutIt = append(withoutIt, f)
		}
	}
	for _, rt := range []string{IdentityRuntimeHosted, IdentityRuntimeHost} {
		id := nativeShape(rt, nil)
		full := Shape{Kind: EnvLocal, Identity: &id}
		for name, r := range map[string]*BundleReader{
			"names no feature (an old control plane)":                  {Features: []string{}},
			"names every feature this forge has except the native one": {Features: withoutIt},
			"a nil Features list":                                      {},
		} {
			got := r.LimitShape(full)
			if len(got.Identity.Applications) != 1 || got.Identity.Applications[0].Kind != IdentityAppNativeBroker {
				t.Errorf("%s/%s: a native application was sent: %+v", rt, name, got.Identity.Applications)
			}
			raw, err := json.Marshal(got.Canonical())
			if err != nil {
				t.Fatal(err)
			}
			for _, leak := range []string{"prod:mobile", `"frontend"`, "oauthredirect", "signedout"} {
				if strings.Contains(string(raw), leak) {
					t.Errorf("%s/%s: the encoded shape still carries %q: %s", rt, name, leak, raw)
				}
			}
			if names := r.WithheldNativeApplications(full); len(names) != 1 || names[0] != "prod:mobile" {
				t.Errorf("%s/%s: withheld = %v, want [prod:mobile] so the caller can say so", rt, name, names)
			}
		}
		if len(id.Applications) != 2 {
			t.Error("LimitShape modified its input")
		}

		// A control plane that names the feature is sent them: the gate is the
		// feature, not a hard-coded refusal.
		advertised := &BundleReader{Features: BundleDocFeatures()}
		if got := advertised.LimitShape(full); len(got.Identity.Applications) != 2 {
			t.Errorf("%s: a reader naming every advertised feature lost an application: %+v", rt, got.Identity.Applications)
		}
		if names := advertised.WithheldNativeApplications(full); len(names) != 0 {
			t.Errorf("%s: withheld = %v for a reader that registers them", rt, names)
		}
		if got := (*BundleReader)(nil).LimitShape(full); len(got.Identity.Applications) != 2 {
			t.Error("the machine ledger (nil reader) decodes every feature and keeps them")
		}
		if !full.HasOptionalParts() {
			t.Error("a native application is an optional part: sending it needs the reader's feature list first")
		}
	}
	if names := (&BundleReader{}).WithheldNativeApplications(Shape{Kind: EnvLocal}); names != nil {
		t.Errorf("no identity, nothing withheld, got %v", names)
	}
}

// A shape with only native applications and no broker still limits cleanly.
func TestLimitShape_OnlyNativeApplicationsLeavesNone(t *testing.T) {
	id := nativeShape(IdentityRuntimeHosted, nil)
	id.Applications = id.Applications[1:]
	got := (&BundleReader{Features: []string{}}).LimitShape(Shape{Kind: EnvLocal, Identity: &id})
	if got.Identity.Applications != nil {
		t.Errorf("applications = %+v, want none", got.Identity.Applications)
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

func TestNativeCallbackDerivation(t *testing.T) {
	if got := NativePrivateRedirectURI("com.acme.app"); got != "com.acme.app:/oauthredirect" {
		t.Errorf("NativePrivateRedirectURI = %q", got)
	}
	for in, want := range map[string]string{
		"com.acme.app:/oauthredirect":        "com.acme.app:/signedout",
		"https://acme.example/oauthredirect": "https://acme.example/signedout",
		"https://acme.example/app/cb":        "https://acme.example/app/signedout",
	} {
		if got := NativeSignedOutBeside(in); got != want {
			t.Errorf("NativeSignedOutBeside(%q) = %q, want %q", in, got, want)
		}
		if err := ValidateNativeCallbackURI(NativeSignedOutBeside(in), false); err != nil {
			t.Errorf("the derived post-logout URI of %q is refused: %v", in, err)
		}
	}
}
