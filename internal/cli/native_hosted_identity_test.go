package cli

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/release"
)

func hostedNativeEntities(t *testing.T, appJSON string, mut func(id *release.ShapeIdentity)) (string, *KCLEntities, *release.ShapeIdentity) {
	t.Helper()
	dir, e := nativeProject(t, nativeIdentityOutput())
	if appJSON != "" {
		writeAppJSON(t, dir, "frontends/mobile", appJSON)
	}
	id := *e.Identity
	id.Runtime = release.IdentityRuntimeHosted
	id.Login = release.ShapeIdentityLogin{Mode: release.IdentityLoginNative}
	id.Applications = append([]release.ShapeIdentityApplication(nil), id.Applications...)
	if mut != nil {
		mut(&id)
	}
	e.Identity = &id
	return dir, e, &id
}

func nativeApps(id *release.ShapeIdentity) []release.ShapeIdentityApplication {
	var out []release.ShapeIdentityApplication
	for _, a := range id.Applications {
		if a.Kind == release.IdentityAppNative {
			out = append(out, a)
		}
	}
	return out
}

func TestHostedNativeIdentity_DerivesCallbacksFromAppJSON(t *testing.T) {
	dir, e, id := hostedNativeEntities(t, `{"expo":{"scheme":["mobile","com.acme.mobile"]}}`, nil)
	var errOut bytes.Buffer
	got := hostedNativeIdentity(e, id, dir, "prod", &errOut)

	apps := nativeApps(got)
	if len(apps) != 1 {
		t.Fatalf("native applications = %+v", got.Applications)
	}
	if want := []string{"com.acme.mobile:/oauthredirect"}; !slices.Equal(apps[0].RedirectURIs, want) {
		t.Errorf("redirects = %v, want %v", apps[0].RedirectURIs, want)
	}
	if want := []string{"com.acme.mobile:/signedout"}; !slices.Equal(apps[0].LogoutURIs, want) {
		t.Errorf("logouts = %v, want %v", apps[0].LogoutURIs, want)
	}
	if errOut.Len() != 0 {
		t.Errorf("unexpected output: %s", errOut.String())
	}
	if len(nativeApps(id)[0].RedirectURIs) != 0 {
		t.Error("the render's own identity was modified")
	}
	// The result is a shape a control plane accepts: hosted, resolved, no http.
	if err := validateHostedShape(got); err != nil {
		t.Fatalf("the resolved hosted identity does not validate: %v", err)
	}
	// Every URI passes the hosted validator (no http).
	for _, u := range append(append([]string{}, apps[0].RedirectURIs...), apps[0].LogoutURIs...) {
		if err := release.ValidateNativeCallbackURI(u, false); err != nil {
			t.Errorf("%v", err)
		}
	}
}

func TestHostedNativeIdentity_DeclaredCallbacksWin(t *testing.T) {
	dir, e, id := hostedNativeEntities(t, `{"expo":{"scheme":"com.other.scheme"}}`, func(id *release.ShapeIdentity) {
		last := len(id.Applications) - 1
		id.Applications[last].RedirectURIs = []string{"https://acme.example/oauthredirect", "com.acme.mobile:/oauthredirect"}
	})
	got := hostedNativeIdentity(e, id, dir, "prod", &bytes.Buffer{})
	apps := nativeApps(got)
	if want := []string{"https://acme.example/oauthredirect", "com.acme.mobile:/oauthredirect"}; !slices.Equal(apps[0].RedirectURIs, want) {
		t.Errorf("redirects = %v: a declared list must not be replaced or reordered", apps[0].RedirectURIs)
	}
	// Logouts the declaration omits are paired beside each declared redirect, in order.
	if want := []string{"https://acme.example/signedout", "com.acme.mobile:/signedout"}; !slices.Equal(apps[0].LogoutURIs, want) {
		t.Errorf("logouts = %v, want %v", apps[0].LogoutURIs, want)
	}
}

func TestHostedNativeIdentity_DeclaredLogoutsWin(t *testing.T) {
	dir, e, id := hostedNativeEntities(t, `{"expo":{"scheme":"com.acme.mobile"}}`, func(id *release.ShapeIdentity) {
		last := len(id.Applications) - 1
		id.Applications[last].LogoutURIs = []string{"com.acme.mobile:/bye"}
	})
	apps := nativeApps(hostedNativeIdentity(e, id, dir, "prod", &bytes.Buffer{}))
	if !slices.Equal(apps[0].RedirectURIs, []string{"com.acme.mobile:/oauthredirect"}) || !slices.Equal(apps[0].LogoutURIs, []string{"com.acme.mobile:/bye"}) {
		t.Errorf("app = %+v", apps[0])
	}
}

func TestHostedNativeIdentity_NoUsableSchemeIsNotSent(t *testing.T) {
	for name, appJSON := range map[string]string{
		"a scheme that is not reverse-DNS": `{"expo":{"scheme":"mobile"}}`,
		"no scheme":                        `{"expo":{}}`,
		"app.json is not JSON":             `{`,
		"no app.json":                      "",
	} {
		t.Run(name, func(t *testing.T) {
			dir, e, id := hostedNativeEntities(t, appJSON, nil)
			var errOut bytes.Buffer
			got := hostedNativeIdentity(e, id, dir, "prod", &errOut)
			if n := nativeApps(got); len(n) != 0 {
				t.Fatalf("a native application with no callbacks was sent: %+v", n)
			}
			if len(got.Applications) != 1 || got.Applications[0].Kind != release.IdentityAppNativeBroker {
				t.Errorf("the broker application was dropped with it: %+v", got.Applications)
			}
			for _, want := range []string{"[shape] Warning", "env prod", "dev:mobile", "NOT sent to the control plane"} {
				if !strings.Contains(errOut.String(), want) {
					t.Errorf("notice missing %q:\n%s", want, errOut.String())
				}
			}
			if err := validateHostedShape(got); err != nil {
				t.Errorf("the remaining identity does not validate: %v", err)
			}
		})
	}
}

func TestHostedNativeIdentity_LeavesOtherIdentitiesAlone(t *testing.T) {
	dir, e, id := hostedNativeEntities(t, `{"expo":{"scheme":"com.acme.mobile"}}`, nil)
	if got := hostedNativeIdentity(e, nil, dir, "prod", &bytes.Buffer{}); got != nil {
		t.Errorf("nil identity = %+v", got)
	}
	host := *id
	host.Runtime = release.IdentityRuntimeHost
	if got := hostedNativeIdentity(e, &host, dir, "dev", &bytes.Buffer{}); got != &host {
		t.Error("a host-run identity is resolved by `forge env up`, not here")
	}
	brokerOnly := *id
	brokerOnly.Applications = id.Applications[:1]
	if got := hostedNativeIdentity(e, &brokerOnly, dir, "prod", &bytes.Buffer{}); got != &brokerOnly {
		t.Error("an identity with no native application is returned unchanged")
	}
}

func TestHostedNativeRedirects_SeamForUniversalLinks(t *testing.T) {
	if got := hostedNativeRedirects(""); got != nil {
		t.Errorf("no scheme = %v, want none: empty callbacks are never sent", got)
	}
	if got := hostedNativeRedirects("com.acme.mobile"); !slices.Equal(got, []string{"com.acme.mobile:/oauthredirect"}) {
		t.Errorf("redirects = %v", got)
	}
}

// validateHostedShape runs the shape validation a hosted bundle goes through
// (bundle.ProjectShape calls the same Validate) over the identity alone.
func validateHostedShape(id *release.ShapeIdentity) error {
	return release.Shape{Kind: release.EnvLocal, Identity: id}.Validate()
}

func TestNativeOIDCFromStatus(t *testing.T) {
	var doc identityStatusDocument
	reply := `{"hasInstance":true,"state":"IDENTITY_STATE_READY","issuer":"https://acme.reliantidentity.com",
		"scopes":["openid","profile","email","urn:zitadel:iam:org:project:id:123:aud"],
		"apps":[{"name":"prod:api","clientId":"api-client"},
		        {"name":"prod:mobile","clientId":"mobile-client","kind":"oidc_native",
		         "redirectUris":["com.acme.mobile:/oauthredirect"],"postLogoutUris":["com.acme.mobile:/signedout"]}]}`
	if err := json.Unmarshal([]byte(reply), &doc); err != nil {
		t.Fatal(err)
	}
	got := nativeOIDCFromStatus(doc, doc.Apps[1])
	want := nativeOIDC{
		Issuer:      "https://acme.reliantidentity.com",
		ClientID:    "mobile-client",
		Scopes:      "openid profile email offline_access urn:zitadel:iam:org:project:id:123:aud",
		RedirectURI: "com.acme.mobile:/oauthredirect", PostLogoutRedirectURI: "com.acme.mobile:/signedout",
	}
	if got == nil || *got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if nativeOIDCFromStatus(doc, doc.Apps[0]) != nil {
		t.Error("a broker app is not a native client")
	}

	// A control plane that predates the native fields reports none of them:
	// nothing is guessed.
	var old identityStatusDocument
	if err := json.Unmarshal([]byte(`{"hasInstance":true,"issuer":"https://acme.reliantidentity.com","apps":[{"name":"prod:mobile","clientId":"mobile-client"}]}`), &old); err != nil {
		t.Fatal(err)
	}
	if nativeOIDCFromStatus(old, old.Apps[0]) != nil {
		t.Error("an old control plane's reply produced a native client")
	}
	noRedirect := doc
	noRedirect.Apps = []identityAppWire{{Name: "prod:mobile", ClientID: "c", Kind: "oidc_native"}}
	if nativeOIDCFromStatus(noRedirect, noRedirect.Apps[0]) != nil {
		t.Error("a native app with no redirect produced a client")
	}
}
