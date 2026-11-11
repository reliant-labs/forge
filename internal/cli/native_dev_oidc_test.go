package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	deployv1alpha1 "github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
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

	// The issuer is whatever the identity converge published. Under a
	// --device-host run that is the device origin (the IdP serves it and the
	// API validates it), and the launch injects it as published.
	t.Run("--device-host: the published device issuer is injected and announced", func(t *testing.T) {
		const deviceIssuer = "http://192.168.1.20:8098"
		out := nativeIdentityOutput()
		out.Issuer = deviceIssuer
		dir, e := nativeProject(t, out)
		mobile := e.Frontends[1]
		fl := frontendLaunch{entities: e, env: "dev", projectDir: dir, apiBaseURL: "http://localhost:4000", deviceHost: "192.168.1.20"}
		cmd := buildFrontendDevCmd(context.Background(), fl, mobile, nil)
		env := nativeOriginEnvMap(cmd.Env)
		if env[oidcIssuerEnv] != deviceIssuer {
			t.Errorf("issuer = %q, want the device origin", env[oidcIssuerEnv])
		}
		if env["EXPO_PUBLIC_API_URL"] != "http://192.168.1.20:4000" {
			t.Errorf("the API origin must follow the device host too, got %q", env["EXPO_PUBLIC_API_URL"])
		}
		var b bytes.Buffer
		announceNativeOIDC(&b, "mobile", readNativeOIDC(dir, e, mobile), cmd.Env, deviceHostState{Requested: "192.168.1.20", Accepted: "192.168.1.20"})
		for _, want := range []string{"sign-in: issuer " + deviceIssuer, "client mobile-client", "serves it for this run"} {
			if !strings.Contains(b.String(), want) {
				t.Errorf("announcement missing %q:\n%s", want, b.String())
			}
		}
		if strings.Contains(b.String(), "adb reverse") || strings.Contains(b.String(), "simulator only") {
			t.Errorf("a device issuer must not carry the localhost advice:\n%s", b.String())
		}
	})
	t.Run("localhost issuer with --device-host says it did not move and why", func(t *testing.T) {
		fl := fl
		fl.deviceHost = "192.168.1.20"
		cmd := buildFrontendDevCmd(context.Background(), fl, mobile, nil)
		var b bytes.Buffer
		announceNativeOIDC(&b, "mobile", readNativeOIDC(dir, e, mobile), cmd.Env, deviceHostState{Requested: "192.168.1.20", Accepted: "192.168.1.20"})
		for _, want := range []string{"sign-in: issuer " + testIssuerBase, "adb reverse tcp:8098 tcp:8098", "did not move it", "cannot sign in"} {
			if !strings.Contains(b.String(), want) {
				t.Errorf("announcement missing %q:\n%s", want, b.String())
			}
		}
	})
	// A refused device host is explained by the real reason, the one the run's
	// top notice gives, not by "set explicitly / no native application".
	t.Run("a refused device host: the sign-in line names the real reason", func(t *testing.T) {
		local := lanAddrs("192.168.1.20", "fd00::20")
		for _, host := range []string{"my-mac.local", "fd00::20", "10.0.2.2"} {
			e := e
			st := deviceHostStateFor(e, host, local)
			if !st.Refused || st.Accepted != "" {
				t.Fatalf("%s: state = %+v, want refused", host, st)
			}
			cmd := buildFrontendDevCmd(context.Background(), frontendLaunch{entities: e, env: "dev", projectDir: dir, deviceHost: host}, mobile, nil)
			var b bytes.Buffer
			announceNativeOIDC(&b, "mobile", readNativeOIDC(dir, e, mobile), cmd.Env, st)
			for _, want := range []string{"sign-in: issuer " + testIssuerBase, "--device-host " + host + " is not this machine's LAN IPv4 address", "pass e.g. --device-host 192.168.1.20"} {
				if !strings.Contains(b.String(), want) {
					t.Errorf("%s: line missing %q:\n%s", host, want, b.String())
				}
			}
			if strings.Contains(b.String(), "set explicitly") || strings.Contains(b.String(), "no native application") {
				t.Errorf("%s: the line gives a reason the run did not have:\n%s", host, b.String())
			}
		}
	})
	t.Run("an accepted device host is not reported as refused", func(t *testing.T) {
		local := lanAddrs("192.168.1.20")
		_, e2 := nativeProject(t, nativeIdentityOutput())
		applyDeviceHostToIDP(e2, nil, "192.168.1.20", local)
		st := deviceHostStateFor(e2, "192.168.1.20", local)
		if st.Refused || st.Accepted != "192.168.1.20" {
			t.Errorf("state = %+v, want accepted", st)
		}
		// A valid host that no native application needed is neither accepted nor refused.
		_, e3 := nativeProject(t, nativeIdentityOutput())
		applyDeviceHostToIDP(e3, []string{"web"}, "192.168.1.20", local)
		if st := deviceHostStateFor(e3, "192.168.1.20", local); st.Refused || st.Accepted != "" {
			t.Errorf("no native frontend targeted: state = %+v", st)
		}
	})
	t.Run("localhost issuer without --device-host points at the flag", func(t *testing.T) {
		cmd := buildFrontendDevCmd(context.Background(), fl, mobile, nil)
		var b bytes.Buffer
		announceNativeOIDC(&b, "mobile", readNativeOIDC(dir, e, mobile), cmd.Env, deviceHostState{})
		if !strings.Contains(b.String(), "--device-host <this machine's LAN IPv4 address>") {
			t.Errorf("announcement does not name the way to a device:\n%s", b.String())
		}
	})
}

func lanAddrs(ips ...string) []net.Addr {
	var out []net.Addr
	for _, s := range ips {
		out = append(out, &net.IPNet{IP: net.ParseIP(s), Mask: net.CIDRMask(24, 32)})
	}
	return out
}

func TestDeviceHostIsThisMachineIPv4(t *testing.T) {
	local := lanAddrs("127.0.0.1", "192.168.1.20", "fd00::20")
	for host, want := range map[string]bool{
		"192.168.1.20": true, "192.168.1.99": false, "10.0.2.2": false,
		// A name, or an IPv6 literal, even when it is this machine's: the
		// userinfo lookup and the broker accept plain HTTP only to an IPv4
		// literal they can confirm at connect time.
		"my-mac.local": false, "fd00::20": false, "[fd00::20]": false, "nowhere.test": false,
	} {
		if got := deviceHostIsThisMachineIPv4(host, local); got != want {
			t.Errorf("%s: got %v, want %v", host, got, want)
		}
	}
}

// The opt-in knob is forge's default where nothing was said, and is never
// forced over a decision the workload declared (HostEnv layers LaunchEnv over
// declared values, so setting it would otherwise override them). A declared
// literal false is the config default that config_gen.workloadEnvMap projects
// onto every workload of a scaffolded project, so it is not a decision: the
// Android emulator acceptance found the knob off (and Claims.Email empty) in
// every real project while a fixture without that projection passed.
func TestEnableLocalUserInfo_DeclaredValues(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declared *deployv1alpha1.EnvVar
		want     string // effective host env value
		forced   bool   // forge set LaunchEnv
	}{
		{name: "nothing declared", want: "true", forced: true},
		{name: "the projected config default false", declared: &deployv1alpha1.EnvVar{Name: userInfoLocalHTTPEnv, Value: "false"}, want: "true", forced: true},
		{name: "a declared true", declared: &deployv1alpha1.EnvVar{Name: userInfoLocalHTTPEnv, Value: "true"}, want: "true"},
		{name: "an unparseable literal", declared: &deployv1alpha1.EnvVar{Name: userInfoLocalHTTPEnv, Value: "maybe"}, want: "maybe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, e := nativeProject(t, nativeIdentityOutput())
			for i := range e.Workloads {
				if e.Workloads[i].Name == "api" && tc.declared != nil {
					e.Workloads[i].Spec.Env = append(e.Workloads[i].Spec.Env, *tc.declared)
				}
			}
			enableLocalUserInfo(e)
			seen := false
			for _, w := range e.Workloads {
				if w.Name != "api" || w.Runtime.Host == nil {
					continue
				}
				seen = true
				if _, set := w.Runtime.Host.LaunchEnv[userInfoLocalHTTPEnv]; set != tc.forced {
					t.Errorf("forge set LaunchEnv = %v, want %v", set, tc.forced)
				}
				if got := w.HostEnv()[userInfoLocalHTTPEnv]; got != tc.want {
					t.Errorf("effective host env = %q, want %q", got, tc.want)
				}
			}
			if !seen {
				t.Fatal("fixture has no host api workload")
			}
		})
	}
	t.Run("a secret reference", func(t *testing.T) {
		_, e := nativeProject(t, nativeIdentityOutput())
		for i := range e.Workloads {
			if e.Workloads[i].Name == "api" {
				e.Workloads[i].Spec.Env = append(e.Workloads[i].Spec.Env, deployv1alpha1.EnvVar{
					Name: userInfoLocalHTTPEnv, SecretRef: &deployv1alpha1.SecretKeyRef{Name: "s", Key: "k"},
				})
			}
		}
		enableLocalUserInfo(e)
		for _, w := range e.Workloads {
			if w.Name == "api" && w.Runtime.Host != nil {
				if v, set := w.Runtime.Host.LaunchEnv[userInfoLocalHTTPEnv]; set {
					t.Errorf("forge set %s=%q over a declared reference", userInfoLocalHTTPEnv, v)
				}
			}
		}
	})
}

func TestApplyDeviceHostToIDP(t *testing.T) {
	local := lanAddrs("192.168.1.20", "fd00::20")
	zitadelOf := func(e *KCLEntities) *HostInfraEntity {
		z, err := hostZitadel(e, e.Identity)
		if err != nil {
			t.Fatal(err)
		}
		return z
	}
	launchEnv := func(e *KCLEntities) map[string]string {
		out := map[string]string{}
		for _, w := range e.Workloads {
			if w.Runtime.Host != nil {
				out[w.Name] = w.Runtime.Host.LaunchEnv[userInfoLocalHTTPEnv]
			}
		}
		return out
	}

	t.Run("a native app on a host identity and a local IPv4 device host: the IdP serves it", func(t *testing.T) {
		_, e := nativeProject(t, nativeIdentityOutput())
		notice := applyDeviceHostToIDP(e, nil, "192.168.1.20", local)
		if zitadelOf(e).DeviceHost != "192.168.1.20" {
			t.Fatal("the zitadel entry carries no device host")
		}
		for _, want := range []string{"also serves http://192.168.1.20:", "every network interface", "plain HTTP", "not shared Wi-Fi", "stop the stack", "before changing networks"} {
			if !strings.Contains(notice, want) {
				t.Errorf("notice missing %q:\n%s", want, notice)
			}
		}
		if strings.Contains(notice, "social") {
			t.Errorf("no social IdPs declared, no social warning:\n%s", notice)
		}
		// The dev-only userinfo opt-in reaches the identity's host consumer, and only it.
		got := launchEnv(e)
		if got["api"] != "true" {
			t.Errorf("the API's launch env = %v: the userinfo opt-in must be set for the identity's consumer in a device-host run", got)
		}
	})
	t.Run("the opt-in is off without a device host and for a hostname or IPv6 host", func(t *testing.T) {
		for _, host := range []string{"", "my-mac.local", "fd00::20", "10.0.2.2"} {
			_, e := nativeProject(t, nativeIdentityOutput())
			applyDeviceHostToIDP(e, nil, host, local)
			if zitadelOf(e).DeviceHost != "" {
				t.Errorf("%q: the device host was applied", host)
			}
			for name, v := range launchEnv(e) {
				if v != "" {
					t.Errorf("%q: %s got %s=%s; the opt-in is only for a served LAN IPv4 issuer", host, name, userInfoLocalHTTPEnv, v)
				}
			}
		}
	})
	t.Run("a hostname or IPv6 device host keeps the localhost issuer and says to pass the LAN IPv4", func(t *testing.T) {
		for _, host := range []string{"my-mac.local", "fd00::20"} {
			_, e := nativeProject(t, nativeIdentityOutput())
			n := applyDeviceHostToIDP(e, nil, host, local)
			for _, want := range []string{"not an IPv4 address of this machine", "keeps its localhost issuer", "LAN IPv4 address", "my-mac.local", "adb reverse"} {
				if !strings.Contains(n, want) && want != "my-mac.local" {
					t.Errorf("%s: notice missing %q:\n%s", host, want, n)
				}
			}
		}
	})
	t.Run("an address of another machine keeps the localhost issuer and says why", func(t *testing.T) {
		_, e := nativeProject(t, nativeIdentityOutput())
		n := applyDeviceHostToIDP(e, nil, "10.0.2.2", local)
		if zitadelOf(e).DeviceHost != "" {
			t.Error("a non-local address must not become the issuer: nothing on this machine could dial it")
		}
		for _, want := range []string{"not an IPv4 address of this machine", "adb reverse"} {
			if !strings.Contains(n, want) {
				t.Errorf("notice missing %q:\n%s", want, n)
			}
		}
	})
	t.Run("no device host: untouched, silent", func(t *testing.T) {
		_, e := nativeProject(t, nativeIdentityOutput())
		if n := applyDeviceHostToIDP(e, nil, "", local); n != "" || zitadelOf(e).DeviceHost != "" {
			t.Errorf("notice %q, device host %q", n, zitadelOf(e).DeviceHost)
		}
	})
	t.Run("no native frontend targeted: the IdP is not put on the network", func(t *testing.T) {
		_, e := nativeProject(t, nativeIdentityOutput())
		if n := applyDeviceHostToIDP(e, []string{"web"}, "192.168.1.20", local); n != "" || zitadelOf(e).DeviceHost != "" {
			t.Errorf("notice %q, device host %q", n, zitadelOf(e).DeviceHost)
		}
	})
	t.Run("a native frontend that opted out of sign-in has no application: nothing to serve", func(t *testing.T) {
		_, e := nativeProject(t, nativeIdentityOutput())
		e.Identity.Applications = e.Identity.Applications[:len(e.Identity.Applications)-1]
		if n := applyDeviceHostToIDP(e, nil, "192.168.1.20", local); n != "" || zitadelOf(e).DeviceHost != "" {
			t.Errorf("notice %q, device host %q", n, zitadelOf(e).DeviceHost)
		}
	})
	// M4: the social callback moves to the LAN origin for the run.
	t.Run("social IdPs declared: a warning that the callback moved", func(t *testing.T) {
		_, e := nativeProject(t, nativeIdentityOutput())
		e.Identity.Social = []release.ShapeIdentitySocial{{Type: "google"}}
		n := applyDeviceHostToIDP(e, nil, "192.168.1.20", local)
		for _, want := range []string{"warning", "social sign-in", "http://192.168.1.20:", "/idps/callback", "Google refuses"} {
			if !strings.Contains(n, want) {
				t.Errorf("social warning missing %q:\n%s", want, n)
			}
		}
	})
}

func TestBuildFrontendDevCmd_NativeOIDCPrecedence(t *testing.T) {
	dir, e := nativeProject(t, nativeIdentityOutput())
	mobile := e.Frontends[1]
	fl := frontendLaunch{entities: e, env: "dev", projectDir: dir}

	t.Run("shell beats injected, per variable (scopes alone)", func(t *testing.T) {
		cmd := buildFrontendDevCmd(context.Background(), fl, mobile, []string{oidcScopesEnv + "=openid"})
		env := nativeOriginEnvMap(cmd.Env)
		if env[oidcScopesEnv] != "openid" {
			t.Errorf("scopes = %q", env[oidcScopesEnv])
		}
		if env[oidcIssuerEnv] != testIssuerBase || env[oidcClientIDEnv] != "mobile-client" {
			t.Errorf("the other variables are still injected, got issuer %q client %q", env[oidcIssuerEnv], env[oidcClientIDEnv])
		}
	})
	t.Run("KCL env_vars beat injected", func(t *testing.T) {
		fe := mobile
		fe.EnvVars = []KCLEnvVar{{Name: oidcRedirectURIEnv, Value: "custom:/cb"}}
		cmd := buildFrontendDevCmd(context.Background(), fl, fe, nil)
		if got := nativeOriginEnvMap(cmd.Env)[oidcRedirectURIEnv]; got != "custom:/cb" {
			t.Errorf("redirect = %q", got)
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
		if len(describeNativeOIDC("mobile", readNativeOIDC(projectDir, e, mobile), cmd.Env, deviceHostState{})) != 0 {
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
			"", []string{"NO native sign-in client was registered", `"scheme": "com.myapp.mobile"`, "can stay as they are", "native_sign_in = False", "migrations/v1.4.0/native-auth"}},
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
	if err := os.WriteFile(envFile, []byte(oidcScopesEnv+"=from-env-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fe := mobile
	fe.EnvFile = envFile
	fe.EnvVars = []KCLEnvVar{{Name: oidcRedirectURIEnv, Value: "kcl:/cb"}}
	shell := []string{oidcRedirectURIEnv + "=shell:/cb", oidcScopesEnv + "=from-shell", oidcPostLogoutURIEnv + "=shell:/out"}

	got := nativeOriginEnvMap(buildFrontendDevCmd(context.Background(), fl, fe, shell).Env)
	for k, want := range map[string]string{
		oidcRedirectURIEnv:   "kcl:/cb",       // KCL > shell
		oidcScopesEnv:        "from-env-file", // .env.<env> > shell
		oidcPostLogoutURIEnv: "shell:/out",    // shell > injected
		oidcClientIDEnv:      "mobile-client", // nothing above it: injected
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}

	override := append(slices.Clone(shell), "FORGE_ENV_OVERRIDE="+oidcRedirectURIEnv)
	if got := nativeOriginEnvMap(buildFrontendDevCmd(context.Background(), fl, fe, override).Env); got[oidcRedirectURIEnv] != "shell:/cb" {
		t.Errorf("FORGE_ENV_OVERRIDE: redirect = %q, want the shell's", got[oidcRedirectURIEnv])
	}
}

// The five variables are one unit: an issuer or a client id set by ANY layer
// (shell, KCL env_vars, .env.<env>) means the app is configured for another
// identity provider, so forge injects none of its dev client's scopes or
// redirects and prints one line saying why.
func TestBuildFrontendDevCmd_NativeOIDCIsInjectedAsOneUnit(t *testing.T) {
	dir, e := nativeProject(t, nativeIdentityOutput())
	mobile := e.Frontends[1]
	fl := frontendLaunch{entities: e, env: "dev", projectDir: dir}
	envFile := func(body string) string {
		p := filepath.Join(t.TempDir(), ".env.dev")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name  string
		fe    func(FrontendEntity) FrontendEntity
		shell []string
		set   string
	}{
		{"shell issuer", func(f FrontendEntity) FrontendEntity { return f }, []string{oidcIssuerEnv + "=https://other.idp"}, oidcIssuerEnv},
		{"shell client id", func(f FrontendEntity) FrontendEntity { return f }, []string{oidcClientIDEnv + "=other"}, oidcClientIDEnv},
		{"KCL issuer", func(f FrontendEntity) FrontendEntity {
			f.EnvVars = []KCLEnvVar{{Name: oidcIssuerEnv, Value: "https://other.idp"}}
			return f
		}, nil, oidcIssuerEnv},
		{".env.<env> client id", func(f FrontendEntity) FrontendEntity {
			f.EnvFile = envFile(oidcClientIDEnv + "=other\n")
			return f
		}, nil, oidcClientIDEnv},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fe := tc.fe(mobile)
			cmd := buildFrontendDevCmd(context.Background(), fl, fe, tc.shell)
			env := nativeOriginEnvMap(cmd.Env)
			for _, n := range []string{oidcScopesEnv, oidcRedirectURIEnv, oidcPostLogoutURIEnv} {
				if env[n] != "" {
					t.Errorf("%s = %q: forge's dev scopes and redirects must not mix with another provider's %s", n, env[n], tc.set)
				}
			}
			if tc.set == oidcIssuerEnv && env[oidcClientIDEnv] != "" || tc.set == oidcClientIDEnv && env[oidcIssuerEnv] != "" {
				t.Errorf("forge filled the sibling of %s: %v", tc.set, env)
			}
			_, note := withNativeOIDCEnvNote(cmd.Env, readNativeOIDC(dir, e, fe))
			if !strings.Contains(note, tc.set) || !strings.Contains(note, "injects none") {
				t.Errorf("note = %q, want one line naming %s and that forge injects none", note, tc.set)
			}
		})
	}
	t.Run("nothing set outside forge: all five injected, no note", func(t *testing.T) {
		cmd := buildFrontendDevCmd(context.Background(), fl, mobile, nil)
		env := nativeOriginEnvMap(cmd.Env)
		for _, n := range oidcEnvNames() {
			if env[n] == "" {
				t.Errorf("%s not injected", n)
			}
		}
		if _, note := withNativeOIDCEnvNote(nil, readNativeOIDC(dir, e, mobile)); note != "" {
			t.Errorf("unexpected note %q", note)
		}
	})
}

// An https first redirect is injected as the unit's redirect, and the private-use
// entry that follows it as the fallback: the app picks by platform from the pair
// the IdP registered, with no other config.
func TestNativeOIDCFromOutput_HTTPSFirstInjectsTheFallback(t *testing.T) {
	out := nativeIdentityOutput()
	app := out.Apps["dev:mobile"]
	app.RedirectURIs = []string{"https://app.acme.example/oauthredirect", "com.acme.mobile:/oauthredirect"}
	app.PostLogoutURIs = []string{"https://app.acme.example/signedout", "com.acme.mobile:/signedout"}
	out.Apps["dev:mobile"] = app
	got := nativeOIDCFromOutput(out, "dev:mobile")
	if got.RedirectURI != "https://app.acme.example/oauthredirect" || got.PostLogoutRedirectURI != "https://app.acme.example/signedout" {
		t.Fatalf("first redirects = %q / %q", got.RedirectURI, got.PostLogoutRedirectURI)
	}
	if got.FallbackRedirectURI != "com.acme.mobile:/oauthredirect" || got.FallbackPostLogoutRedirectURI != "com.acme.mobile:/signedout" {
		t.Errorf("fallbacks = %q / %q", got.FallbackRedirectURI, got.FallbackPostLogoutRedirectURI)
	}

	// Private-use first: nothing to fall back from, so the env is the old five.
	plain := nativeOIDCFromOutput(nativeIdentityOutput(), "dev:mobile")
	if plain.FallbackRedirectURI != "" || plain.FallbackPostLogoutRedirectURI != "" {
		t.Errorf("a private-use first redirect grew a fallback: %+v", plain)
	}

	// https only, with no private-use entry: no fallback, no invented one.
	app.RedirectURIs = []string{"https://app.acme.example/oauthredirect"}
	app.PostLogoutURIs = []string{"https://app.acme.example/signedout"}
	out.Apps["dev:mobile"] = app
	if only := nativeOIDCFromOutput(out, "dev:mobile"); only.FallbackRedirectURI != "" {
		t.Errorf("fallback = %q with no private-use entry", only.FallbackRedirectURI)
	}
}

// The fallback is part of the one OIDC unit: when the developer points the app
// at another provider, forge injects none of it; otherwise a variable already
// set is kept and the rest are filled.
func TestWithNativeOIDCEnv_FallbackBelongsToTheUnit(t *testing.T) {
	n := &nativeOIDC{
		Issuer: "http://localhost:8098", ClientID: "c", Scopes: "openid",
		RedirectURI: "https://app.acme.example/oauthredirect", PostLogoutRedirectURI: "https://app.acme.example/signedout",
		FallbackRedirectURI: "com.acme.mobile:/oauthredirect", FallbackPostLogoutRedirectURI: "com.acme.mobile:/signedout",
	}
	env := nativeOriginEnvMap(withNativeOIDCEnv(nil, n))
	for _, name := range append(oidcEnvNames(), oidcFallbackEnvNames()...) {
		if env[name] == "" {
			t.Errorf("%s not injected", name)
		}
	}
	other := nativeOriginEnvMap(withNativeOIDCEnv([]string{oidcIssuerEnv + "=https://other.idp"}, n))
	for _, name := range oidcFallbackEnvNames() {
		if other[name] != "" {
			t.Errorf("%s injected beside another provider's issuer", name)
		}
	}
	// A developer-set redirect is kept; the fallback is still filled.
	kept := nativeOriginEnvMap(withNativeOIDCEnv([]string{oidcRedirectURIEnv + "=custom:/cb"}, n))
	if kept[oidcRedirectURIEnv] != "custom:/cb" || kept[oidcFallbackRedirectURIEnv] == "" {
		t.Errorf("per-variable precedence broken: %v", kept)
	}
	// A private-use-only client sets neither fallback variable.
	plain := nativeOriginEnvMap(withNativeOIDCEnv(nil, &nativeOIDC{Issuer: "http://localhost:8098", ClientID: "c", RedirectURI: "com.acme.mobile:/oauthredirect"}))
	for _, name := range oidcFallbackEnvNames() {
		if _, set := plain[name]; set {
			t.Errorf("%s set for a private-use-only client", name)
		}
	}
}
