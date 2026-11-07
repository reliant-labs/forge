package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/identity"
	"github.com/reliant-labs/forge/pkg/release"
)

const (
	testAudScope   = "urn:zitadel:iam:org:project:id:123:aud"
	testIssuerBase = "http://localhost:8098"
)

func nativeIdentityOutput() identity.Output {
	return identity.Output{
		Issuer: testIssuerBase, Audience: "123",
		Scopes: []string{"openid", "profile", "email", identity.RolesScope, testAudScope},
		Apps: map[string]identity.AppOutput{
			"dev:api": {ClientID: "api-client", RedirectURIs: []string{"http://localhost:3000/auth/callback"}},
			"dev:mobile": {
				ClientID:       "mobile-client",
				RedirectURIs:   []string{"com.acme.mobile:/oauthredirect", "https://acme.example/oauthredirect"},
				PostLogoutURIs: []string{"com.acme.mobile:/signedout", "https://acme.example/signedout"},
			},
		},
	}
}

func TestNativeOIDCFromOutput(t *testing.T) {
	got := nativeOIDCFromOutput(nativeIdentityOutput(), "dev:mobile")
	if got == nil {
		t.Fatal("nil")
	}
	want := nativeOIDC{
		Issuer:      testIssuerBase,
		ClientID:    "mobile-client",
		Scopes:      "openid profile email offline_access " + identity.RolesScope + " " + testAudScope,
		RedirectURI: "com.acme.mobile:/oauthredirect",
		// The first (preferred) redirect, so the client never branches on the scheme.
		PostLogoutRedirectURI: "com.acme.mobile:/signedout",
	}
	if *got != want {
		t.Fatalf("got  %+v\nwant %+v", *got, want)
	}
	if !strings.Contains(got.Scopes, testAudScope) {
		t.Error("without the project audience scope every RPC 401s")
	}
	if strings.Count(got.Scopes, "openid") != 1 {
		t.Errorf("scopes repeat openid: %q", got.Scopes)
	}
}

func TestNativeOIDCFromOutput_NotRegisteredYet(t *testing.T) {
	out := nativeIdentityOutput()
	if nativeOIDCFromOutput(out, "dev:ghost") != nil {
		t.Error("an unregistered app injects nothing")
	}
	noID := nativeIdentityOutput()
	noID.Apps["dev:mobile"] = identity.AppOutput{RedirectURIs: []string{"com.acme.mobile:/oauthredirect"}}
	if nativeOIDCFromOutput(noID, "dev:mobile") != nil {
		t.Error("an app without a client id injects nothing")
	}
	noIssuer := nativeIdentityOutput()
	noIssuer.Issuer = ""
	if nativeOIDCFromOutput(noIssuer, "dev:mobile") != nil {
		t.Error("no issuer, no injection")
	}
}

// nativeProject writes the project the injection reads: the published
// identity outputs.
func nativeProject(t *testing.T, out identity.Output) (string, *KCLEntities) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".forge", "identity"), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if err := os.WriteFile(filepath.Join(dir, ".forge", "identity", "identity.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	e := identityEntities()
	e.Frontends = []FrontendEntity{
		{Name: "web", Type: "vite-spa", Port: 5173},
		{Name: "mobile", Type: "rn", Path: "frontends/mobile", Port: 8081, EnvFile: "/does/not/exist"},
	}
	e.Identity.Applications = append(e.Identity.Applications,
		release.ShapeIdentityApplication{Name: "dev:mobile", Kind: release.IdentityAppNative, Frontend: "mobile"})
	return dir, e
}

func TestBuildFrontendDevCmd_InjectsNativeOIDC(t *testing.T) {
	dir, e := nativeProject(t, nativeIdentityOutput())
	fl := frontendLaunch{entities: e, env: "dev", projectDir: dir, apiBaseURL: "http://localhost:4000"}
	mobile := e.Frontends[1]

	cmd := buildFrontendDevCmd(context.Background(), fl, mobile, nil)
	env := nativeOriginEnvMap(cmd.Env)
	for k, want := range map[string]string{
		"EXPO_PUBLIC_FORGE_OIDC_ISSUER":                   testIssuerBase,
		"EXPO_PUBLIC_FORGE_OIDC_CLIENT_ID":                "mobile-client",
		"EXPO_PUBLIC_FORGE_OIDC_SCOPES":                   "openid profile email offline_access " + identity.RolesScope + " " + testAudScope,
		"EXPO_PUBLIC_FORGE_OIDC_REDIRECT_URI":             "com.acme.mobile:/oauthredirect",
		"EXPO_PUBLIC_FORGE_OIDC_POST_LOGOUT_REDIRECT_URI": "com.acme.mobile:/signedout",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}

	t.Run("a web frontend gets nothing", func(t *testing.T) {
		cmd := buildFrontendDevCmd(context.Background(), fl, e.Frontends[0], nil)
		for k := range nativeOriginEnvMap(cmd.Env) {
			if strings.Contains(k, "FORGE_OIDC") {
				t.Errorf("web frontend got %s", k)
			}
		}
	})

	t.Run("--device-host does not move the issuer", func(t *testing.T) {
		fl := fl
		fl.deviceHost = "192.168.1.20"
		cmd := buildFrontendDevCmd(context.Background(), fl, mobile, nil)
		env := nativeOriginEnvMap(cmd.Env)
		if env[oidcIssuerEnv] != testIssuerBase {
			t.Errorf("issuer = %q: a LAN issuer is a different Zitadel instance lookup and a different iss", env[oidcIssuerEnv])
		}
		if env["EXPO_PUBLIC_API_URL"] != "http://192.168.1.20:4000" {
			t.Errorf("the API origin must still follow the device host, got %q", env["EXPO_PUBLIC_API_URL"])
		}
		var b bytes.Buffer
		announceNativeOIDC(&b, "mobile", readNativeOIDC(dir, e, mobile), cmd.Env, "192.168.1.20")
		for _, want := range []string{"sign-in: issuer " + testIssuerBase, "client mobile-client", "adb reverse tcp:8098 tcp:8098", "--device-host does not apply to the IdP"} {
			if !strings.Contains(b.String(), want) {
				t.Errorf("announcement missing %q:\n%s", want, b.String())
			}
		}
	})
}

func TestBuildFrontendDevCmd_NativeOIDCPrecedence(t *testing.T) {
	dir, e := nativeProject(t, nativeIdentityOutput())
	mobile := e.Frontends[1]
	fl := frontendLaunch{entities: e, env: "dev", projectDir: dir}

	t.Run("shell beats injected, per variable", func(t *testing.T) {
		cmd := buildFrontendDevCmd(context.Background(), fl, mobile, []string{oidcIssuerEnv + "=http://tunnel.test:8098"})
		env := nativeOriginEnvMap(cmd.Env)
		if env[oidcIssuerEnv] != "http://tunnel.test:8098" {
			t.Errorf("issuer = %q", env[oidcIssuerEnv])
		}
		if env[oidcClientIDEnv] != "mobile-client" {
			t.Errorf("an unset variable is still injected, got %q", env[oidcClientIDEnv])
		}
	})
	t.Run("KCL env_vars beat injected", func(t *testing.T) {
		fe := mobile
		fe.EnvVars = []KCLEnvVar{{Name: oidcClientIDEnv, Value: "declared-client"}}
		cmd := buildFrontendDevCmd(context.Background(), fl, fe, nil)
		if got := nativeOriginEnvMap(cmd.Env)[oidcClientIDEnv]; got != "declared-client" {
			t.Errorf("client = %q", got)
		}
	})
	t.Run(".env.<env> beats injected", func(t *testing.T) {
		envFile := filepath.Join(t.TempDir(), ".env.dev")
		if err := os.WriteFile(envFile, []byte(oidcScopesEnv+"=openid\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		fe := mobile
		fe.EnvFile = envFile
		cmd := buildFrontendDevCmd(context.Background(), fl, fe, nil)
		if got := nativeOriginEnvMap(cmd.Env)[oidcScopesEnv]; got != "openid" {
			t.Errorf("scopes = %q", got)
		}
	})
}

func TestBuildFrontendDevCmd_NoNativeOIDCWithoutManagedIdentity(t *testing.T) {
	dir, e := nativeProject(t, nativeIdentityOutput())
	mobile := e.Frontends[1]
	assertNone := func(t *testing.T, e *KCLEntities, projectDir string) {
		t.Helper()
		fl := frontendLaunch{entities: e, env: "dev", projectDir: projectDir}
		cmd := buildFrontendDevCmd(context.Background(), fl, mobile, nil)
		for k := range nativeOriginEnvMap(cmd.Env) {
			if strings.Contains(k, "FORGE_OIDC") {
				t.Errorf("injected %s", k)
			}
		}
		if len(describeNativeOIDC("mobile", readNativeOIDC(projectDir, e, mobile), cmd.Env, "")) != 0 {
			t.Error("nothing to announce")
		}
	}
	t.Run("no identity", func(t *testing.T) {
		e := *e
		e.Identity = nil
		assertNone(t, &e, dir)
	})
	t.Run("hosted identity is the control plane's", func(t *testing.T) {
		id := *e.Identity
		id.Runtime = release.IdentityRuntimeHosted
		e := *e
		e.Identity = &id
		assertNone(t, &e, dir)
	})
	t.Run("identity declares no native application for the frontend", func(t *testing.T) {
		id := *e.Identity
		id.Applications = id.Applications[:1]
		e := *e
		e.Identity = &id
		assertNone(t, &e, dir)
	})
	t.Run("identity not converged yet (no outputs file)", func(t *testing.T) {
		assertNone(t, e, t.TempDir())
	})
	t.Run("outputs have no such app", func(t *testing.T) {
		out := nativeIdentityOutput()
		delete(out.Apps, "dev:mobile")
		dir, e := nativeProject(t, out)
		assertNone(t, e, dir)
	})
}

func writeAppJSON(t *testing.T, projectDir, rel, body string) {
	t.Helper()
	dir := filepath.Join(projectDir, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNativeAppSchemes(t *testing.T) {
	cases := []struct {
		name       string
		appJSON    string
		wantScheme string
		warnings   []string
	}{
		{"scaffolded app.json", `{"expo":{"scheme":["mobile","com.acme.mobile"],"ios":{"bundleIdentifier":"com.acme.mobile"},"android":{"package":"com.acme.mobile"}}}`,
			"com.acme.mobile", nil},
		{"a project named MyApp: the bundle id has uppercase, the scheme is lowercased", `{"expo":{"scheme":["mobile","com.myapp.mobile"],"ios":{"bundleIdentifier":"com.MyApp.mobile"},"android":{"package":"com.MyApp.mobile"}}}`,
			"com.myapp.mobile", nil},
		{"a project named my_app: the Android package keeps `_`, the scheme is sanitised", `{"expo":{"scheme":["mobile","com.my-app.mobile"],"ios":{"bundleIdentifier":"com.my-app.mobile"},"android":{"package":"com.my_app.mobile"}}}`,
			"com.my-app.mobile", nil},
		{"scheme as a bare string", `{"expo":{"scheme":"com.acme.mobile"}}`, "com.acme.mobile", nil},
		{"a single sanitised scheme beside a different bundle id is authoritative", `{"expo":{"scheme":"com.myapp.mobile","ios":{"bundleIdentifier":"com.MyApp.mobile"},"android":{"package":"com.my_app.mobile"}}}`, "com.myapp.mobile", nil},
		{"an acme-style single scheme", `{"expo":{"scheme":"com.acme.mobile","ios":{"bundleIdentifier":"com.acme.mobile"}}}`, "com.acme.mobile", nil},
		{"the link scheme is not the callback", `{"expo":{"scheme":["mobile","com.acme.mobile"]}}`, "com.acme.mobile", nil},
		{"bundle id only, no usable scheme", `{"expo":{"scheme":"mobile","ios":{"bundleIdentifier":"com.MyApp.mobile"},"android":{"package":"com.my_app.mobile"}}}`,
			"", []string{"NO native sign-in client was registered", `"scheme": ["mobile", "com.myapp.mobile"]`, "may stay as it is"}},
		{"an invalid scheme is not accepted", `{"expo":{"scheme":["mobile","com.my_app.mobile","Com.Acme.App"]}}`,
			"", []string{"has expo.scheme", "none of it is a valid private-use scheme", `"com.my-app.mobile"`}},
		{"no scheme and no ids", `{"expo":{}}`, "", []string{"has no expo.scheme", "com.example.mobile"}},
		{"not JSON", `{`, "", []string{"not valid JSON"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, e := nativeProject(t, nativeIdentityOutput())
			writeAppJSON(t, dir, "frontends/mobile", tc.appJSON)
			schemes, warnings := nativeAppSchemes(e, e.Identity, dir)
			if schemes["mobile"] != tc.wantScheme {
				t.Errorf("scheme = %q, want %q", schemes["mobile"], tc.wantScheme)
			}
			joined := strings.Join(warnings, "\n")
			for _, w := range tc.warnings {
				if !strings.Contains(joined, w) {
					t.Errorf("warnings %q missing %q", joined, w)
				}
			}
			if len(tc.warnings) == 0 && len(warnings) != 0 {
				t.Errorf("unexpected warnings: %v", warnings)
			}
		})
	}

	t.Run("missing app.json warns", func(t *testing.T) {
		dir, e := nativeProject(t, nativeIdentityOutput())
		schemes, warnings := nativeAppSchemes(e, e.Identity, dir)
		if len(schemes) != 0 || len(warnings) != 1 || !strings.Contains(warnings[0], "cannot read its app.json") {
			t.Errorf("schemes %v warnings %v", schemes, warnings)
		}
	})
	t.Run("an app.config.js project is named, with the declared-callback fix", func(t *testing.T) {
		dir, e := nativeProject(t, nativeIdentityOutput())
		if err := os.MkdirAll(filepath.Join(dir, "frontends/mobile"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "frontends/mobile/app.config.js"), []byte("module.exports = {}"), 0o644); err != nil {
			t.Fatal(err)
		}
		schemes, warnings := nativeAppSchemes(e, e.Identity, dir)
		joined := strings.Join(warnings, "\n")
		if len(schemes) != 0 || !strings.Contains(joined, "app.config.js") || !strings.Contains(joined, "redirect_uris") {
			t.Errorf("schemes %v warnings %v", schemes, warnings)
		}
	})
	t.Run("a declared callback needs no app.json", func(t *testing.T) {
		dir, e := nativeProject(t, nativeIdentityOutput())
		e.Identity.Applications[len(e.Identity.Applications)-1].RedirectURIs = []string{"com.acme.mobile:/oauthredirect"}
		schemes, warnings := nativeAppSchemes(e, e.Identity, dir)
		if len(schemes) != 0 || len(warnings) != 0 {
			t.Errorf("schemes %v warnings %v", schemes, warnings)
		}
	})
}

func TestSuggestedScheme(t *testing.T) {
	for _, tc := range []struct {
		ids  []string
		want string
	}{
		{[]string{"com.MyApp.mobile", ""}, "com.myapp.mobile"},
		{[]string{"", "com.my_app.mobile"}, "com.my-app.mobile"},
		{[]string{"", "", "Mobile App"}, "com.example.mobile-app"},
		{[]string{"", "", "mobile", "Com.Acme.App", "x"}, "com.acme.app"},
		{[]string{"", "", "2fa"}, "com.example.app2fa"},
	} {
		got := suggestedScheme(tc.ids...)
		if got != tc.want || !release.IsNativePrivateScheme(got) {
			t.Errorf("suggestedScheme(%q) = %q, want %q (and a valid scheme)", tc.ids, got, tc.want)
		}
	}
}

func TestDevIdentityInput_NativeFrontendIsNotTheWebOrigin(t *testing.T) {
	dir, e := nativeProject(t, nativeIdentityOutput())
	// Metro first in the list: its port must not become the web sign-in origin.
	e.Frontends = []FrontendEntity{e.Frontends[1], e.Frontends[0]}
	writeAppJSON(t, dir, "frontends/mobile", `{"expo":{"scheme":["com.acme.mobile"],"ios":{"bundleIdentifier":"com.acme.mobile"}}}`)
	in, err := devIdentityInput(e, e.Identity, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if in.FrontendOrigin != "http://localhost:5173" {
		t.Errorf("frontend origin = %q: Metro serves no /auth/callback", in.FrontendOrigin)
	}
	if in.NativeAppSchemes["mobile"] != "com.acme.mobile" {
		t.Errorf("native app schemes = %v", in.NativeAppSchemes)
	}
}

// The IdP refused the native client, so the published output has the web
// application and not the native one: nothing is injected and the app keeps
// its mock provider, while the web application's entry is untouched.
func TestNativeOIDC_NothingInjectedWhenTheIdPRefusedTheNativeApp(t *testing.T) {
	out := nativeIdentityOutput()
	delete(out.Apps, "dev:mobile")
	dir, e := nativeProject(t, out)
	mobile := e.Frontends[1]
	if out.Apps["dev:api"].ClientID == "" {
		t.Fatal("the web-side application must remain in the output")
	}
	cmd := buildFrontendDevCmd(context.Background(), frontendLaunch{entities: e, env: "dev", projectDir: dir}, mobile, nil)
	for k := range nativeOriginEnvMap(cmd.Env) {
		if strings.Contains(k, "FORGE_OIDC") {
			t.Errorf("injected %s for a native app the IdP never registered", k)
		}
	}
}

// The whole chain for one OIDC variable, highest first: KCL env_vars, the
// frontend's .env.<env>, the developer's shell (which a declared value beats
// unless the key is in FORGE_ENV_OVERRIDE), then what forge injects. Expo's own
// .env / .env.local never reach this: they load into the dev server's process
// and never replace a variable already in its environment, so every layer here
// beats them.
func TestBuildFrontendDevCmd_NativeOIDCFullPrecedence(t *testing.T) {
	dir, e := nativeProject(t, nativeIdentityOutput())
	mobile := e.Frontends[1]
	fl := frontendLaunch{entities: e, env: "dev", projectDir: dir}
	envFile := filepath.Join(t.TempDir(), ".env.dev")
	if err := os.WriteFile(envFile, []byte(oidcScopesEnv+"=from-env-file\n"+oidcIssuerEnv+"=http://env-file.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fe := mobile
	fe.EnvFile = envFile
	fe.EnvVars = []KCLEnvVar{{Name: oidcIssuerEnv, Value: "http://kcl.test"}}
	shell := []string{oidcIssuerEnv + "=http://shell.test", oidcScopesEnv + "=from-shell", oidcClientIDEnv + "=shell-client"}

	got := nativeOriginEnvMap(buildFrontendDevCmd(context.Background(), fl, fe, shell).Env)
	for k, want := range map[string]string{
		oidcIssuerEnv:      "http://kcl.test",                // KCL > .env.<env> > shell
		oidcScopesEnv:      "from-env-file",                  // .env.<env> > shell
		oidcClientIDEnv:    "shell-client",                   // shell > injected
		oidcRedirectURIEnv: "com.acme.mobile:/oauthredirect", // nothing above it: injected
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}

	override := append(slices.Clone(shell), "FORGE_ENV_OVERRIDE="+oidcIssuerEnv)
	if got := nativeOriginEnvMap(buildFrontendDevCmd(context.Background(), fl, fe, override).Env); got[oidcIssuerEnv] != "http://shell.test" {
		t.Errorf("FORGE_ENV_OVERRIDE: issuer = %q, want the shell's", got[oidcIssuerEnv])
	}
}
