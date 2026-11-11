package devidentity

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/identity"
	"github.com/reliant-labs/forge/pkg/release"
)

func nativeInput(app release.ShapeIdentityApplication, ids map[string]string) Input {
	in := baseInput()
	in.Identity.Applications = append(in.Identity.Applications, app)
	in.NativeAppSchemes = ids
	return in
}

func specApp(t *testing.T, in Input, name string) (identity.Application, []string) {
	t.Helper()
	spec, warnings, err := Spec(in)
	if err != nil {
		t.Fatalf("Spec: %v", err)
	}
	for _, a := range spec.Applications {
		if a.Name == name {
			return a, warnings
		}
	}
	return identity.Application{}, warnings
}

func TestSpec_NativeApplicationProjection(t *testing.T) {
	const derived = "dev:mobile"
	cases := []struct {
		name      string
		app       release.ShapeIdentityApplication
		ids       map[string]string
		redirects []string
		logouts   []string
		devMode   bool
		missing   string // a warning substring: the app is left out
	}{
		{
			name:      "derived from the app id",
			app:       release.ShapeIdentityApplication{Name: derived, Kind: release.IdentityAppNative, Frontend: "mobile"},
			ids:       map[string]string{"mobile": "com.acme.mobile"},
			redirects: []string{"com.acme.mobile:/oauthredirect"},
			logouts:   []string{"com.acme.mobile:/signedout"},
		},
		{
			name:      "declared literals win, order kept, signedout paired per redirect",
			app:       release.ShapeIdentityApplication{Name: derived, Kind: release.IdentityAppNative, Frontend: "mobile", RedirectURIs: []string{"https://acme.example/oauthredirect", "com.acme.mobile:/oauthredirect"}},
			ids:       map[string]string{"mobile": "com.other.id"},
			redirects: []string{"https://acme.example/oauthredirect", "com.acme.mobile:/oauthredirect"},
			logouts:   []string{"https://acme.example/signedout", "com.acme.mobile:/signedout"},
		},
		{
			name:      "declared logout kept",
			app:       release.ShapeIdentityApplication{Name: derived, Kind: release.IdentityAppNative, Frontend: "mobile", RedirectURIs: []string{"com.acme.mobile:/oauthredirect"}, LogoutURIs: []string{"com.acme.mobile:/bye"}},
			redirects: []string{"com.acme.mobile:/oauthredirect"},
			logouts:   []string{"com.acme.mobile:/bye"},
		},
		{
			name:      "an http loopback callback is dev only and turns devMode on",
			app:       release.ShapeIdentityApplication{Name: derived, Kind: release.IdentityAppNative, Frontend: "mobile", RedirectURIs: []string{"http://localhost:8081/oauthredirect"}},
			redirects: []string{"http://localhost:8081/oauthredirect"},
			logouts:   []string{"http://localhost:8081/signedout"},
			devMode:   true,
		},
		{
			name:    "no app id to derive from: left out with a warning, not fatal",
			app:     release.ShapeIdentityApplication{Name: derived, Kind: release.IdentityAppNative, Frontend: "mobile"},
			missing: "no callback scheme",
		},
		{
			name:    "an invalid callback is left out with a warning",
			app:     release.ShapeIdentityApplication{Name: derived, Kind: release.IdentityAppNative, Frontend: "mobile", RedirectURIs: []string{"myapp://cb"}},
			missing: "reverse-DNS",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, warnings := specApp(t, nativeInput(tc.app, tc.ids), derived)
			if tc.missing != "" {
				if got.Name != "" {
					t.Fatalf("application registered: %+v", got)
				}
				if !strings.Contains(strings.Join(warnings, "\n"), tc.missing) {
					t.Fatalf("warnings = %v, want one containing %q", warnings, tc.missing)
				}
				if web, _ := specApp(t, nativeInput(tc.app, tc.ids), "dev:api"); web.Name == "" {
					t.Fatal("a native app problem must not take the web app's sign-in down with it")
				}
				return
			}
			if got.Kind != identity.AppNative {
				t.Fatalf("kind = %q, want native", got.Kind)
			}
			if got.LoginURI != "" {
				t.Errorf("login URI = %q: the native app uses the IdP's hosted login, not the web sign-in", got.LoginURI)
			}
			if got.Broker != nil {
				t.Error("a native app has no broker account")
			}
			if strings.Join(got.RedirectURIs, " ") != strings.Join(tc.redirects, " ") {
				t.Errorf("redirects = %v, want %v", got.RedirectURIs, tc.redirects)
			}
			if strings.Join(got.PostLogoutURIs, " ") != strings.Join(tc.logouts, " ") {
				t.Errorf("post-logout = %v, want %v", got.PostLogoutURIs, tc.logouts)
			}
			if got.DevMode != tc.devMode {
				t.Errorf("devMode = %v, want %v (a private-use scheme is compared literally)", got.DevMode, tc.devMode)
			}
		})
	}
}

func TestSpec_NativeApplicationIsAPublicPKCEClient(t *testing.T) {
	in := nativeInput(release.ShapeIdentityApplication{Name: "dev:mobile", Kind: release.IdentityAppNative, Frontend: "mobile"}, map[string]string{"mobile": "com.acme.mobile"})
	spec, _, err := Spec(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Applications) != 2 {
		t.Fatalf("applications = %d, want the broker and the native app", len(spec.Applications))
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("the library refuses the projection: %v", err)
	}
}

func TestSpec_NativeAppWarningsReachThePass(t *testing.T) {
	in := nativeInput(release.ShapeIdentityApplication{Name: "dev:mobile", Kind: release.IdentityAppNative, Frontend: "mobile"}, map[string]string{"mobile": "com.acme.mobile"})
	in.NativeAppWarnings = []string{"frontend mobile: scheme not registered"}
	_, warnings := specApp(t, in, "dev:mobile")
	if !strings.Contains(strings.Join(warnings, "\n"), "scheme not registered") {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestSignedOutBeside(t *testing.T) {
	for in, want := range map[string]string{
		"com.acme.app:/oauthredirect":        "com.acme.app:/signedout",
		"https://acme.example/oauthredirect": "https://acme.example/signedout",
		"https://acme.example/app/cb":        "https://acme.example/app/signedout",
	} {
		if got := signedOutBeside(in); got != want {
			t.Errorf("signedOutBeside(%q) = %q, want %q", in, got, want)
		}
	}
}

// The IdP itself may refuse the native client (a locally valid callback it
// still dislikes); only an Optional application survives that without taking
// the web app's output down with it.
func TestSpec_NativeApplicationIsOptionalAndTheWebAppIsNot(t *testing.T) {
	in := nativeInput(release.ShapeIdentityApplication{Name: "dev:mobile", Kind: release.IdentityAppNative, Frontend: "mobile"}, map[string]string{"mobile": "com.acme.mobile"})
	spec, _, err := Spec(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range spec.Applications {
		if want := a.Kind == identity.AppNative; a.Optional != want {
			t.Errorf("application %q (kind %s): Optional = %v, want %v", a.Name, a.Kind, a.Optional, want)
		}
	}
}
