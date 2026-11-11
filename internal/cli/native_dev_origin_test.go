package cli

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeDeviceHost(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr string
	}{
		{in: "192.168.1.20", want: "192.168.1.20"},
		{in: " 10.0.2.2 ", want: "10.0.2.2"},
		{in: "my-mac.local", want: "my-mac.local"},
		{in: "My-Mac.Local", want: "my-mac.local"},
		{in: "fe80::1", want: "[fe80::1]"},
		{in: "[fe80::1]", want: "[fe80::1]"},
		{in: "http://192.168.1.20", wantErr: "is a URL"},
		{in: "192.168.1.20:8080", wantErr: "port"},
		{in: "192.168.1.20/path", wantErr: "not a bare host"},
		{in: "0.0.0.0", wantErr: "bind address"},
		{in: "::", wantErr: "bind address"},
		{in: "-bad.local", wantErr: "not a valid hostname"},
		{in: "bad host", wantErr: "not a bare host"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := normalizeDeviceHost(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got (%q, %v), want %q", got, err, tc.want)
			}
		})
	}
}

func TestResolveDeviceHost_FlagBeatsEnv(t *testing.T) {
	env := func(k string) (string, bool) {
		if k == deviceHostEnv {
			return "10.9.9.9", true
		}
		return "", false
	}
	cases := []struct {
		name, flag, want string
		env              func(string) (string, bool)
		wantErr          bool
	}{
		{name: "none", env: func(string) (string, bool) { return "", false }},
		{name: "nil lookup"},
		{name: "env only", env: env, want: "10.9.9.9"},
		{name: "flag wins", flag: "192.168.1.5", env: env, want: "192.168.1.5"},
		{name: "empty env value is no opt-in", env: func(string) (string, bool) { return "  ", true }},
		{name: "invalid flag", flag: "http://x", env: env, wantErr: true},
		{name: "invalid env", env: func(string) (string, bool) { return "a:1", true }, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveDeviceHost(tc.flag, tc.env)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("got (%q, %v), want (%q, err=%v)", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestResolveDeviceHost_ErrorNamesSource(t *testing.T) {
	_, err := resolveDeviceHost("http://x", nil)
	if err == nil || !strings.Contains(err.Error(), "--device-host") {
		t.Fatalf("flag error should name the flag: %v", err)
	}
	_, err = resolveDeviceHost("", func(string) (string, bool) { return "a:1", true })
	if err == nil || !strings.Contains(err.Error(), deviceHostEnv) {
		t.Fatalf("env error should name the variable: %v", err)
	}
}

func TestAdvertisedAPIOrigin(t *testing.T) {
	cases := []struct{ origin, host, want string }{
		{"http://localhost:43210", "192.168.1.20", "http://192.168.1.20:43210"},
		{"http://localhost:43210", "10.0.2.2", "http://10.0.2.2:43210"},
		{"http://localhost:43210", "[fe80::1]", "http://[fe80::1]:43210"},
		{"http://localhost:43210", "mac.local", "http://mac.local:43210"},
		{"http://localhost:43210", "", "http://localhost:43210"},
		{"", "192.168.1.20", ""},
		{"http://localhost", "192.168.1.20", "http://192.168.1.20"},
	}
	for _, tc := range cases {
		got, err := advertisedAPIOrigin(tc.origin, tc.host)
		if err != nil || got != tc.want {
			t.Errorf("advertisedAPIOrigin(%q,%q) = (%q,%v), want %q", tc.origin, tc.host, got, err, tc.want)
		}
	}
	if _, err := advertisedAPIOrigin("::notaurl", "1.2.3.4"); err == nil {
		t.Error("expected an error for an unparseable origin")
	}
}

func TestNativeAPIOrigin_OnlyNativeFrontends(t *testing.T) {
	const origin = "http://localhost:4000"
	cases := []struct {
		feType, host, want string
	}{
		{"react-native", "", origin},
		{"react-native", "192.168.1.20", "http://192.168.1.20:4000"},
		{"rn", "192.168.1.20", "http://192.168.1.20:4000"},
		{"vite-spa", "192.168.1.20", origin},
		{"nextjs", "192.168.1.20", origin},
		{"", "192.168.1.20", origin},
	}
	for _, tc := range cases {
		if got := nativeAPIOrigin(FrontendEntity{Type: tc.feType}, origin, tc.host); got != tc.want {
			t.Errorf("type %q host %q: got %q want %q", tc.feType, tc.host, got, tc.want)
		}
	}
}

func nativeOriginEnvMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

// Precedence through the real launch-env builder for a native frontend:
// KCL env_vars > parent shell > opt-in device host > localhost.
func TestBuildFrontendDevCmd_NativeAPIOriginPrecedence(t *testing.T) {
	const listener = "http://localhost:4000"
	native := FrontendEntity{Name: "mobile", Type: "react-native", Path: "mobile", Port: 8081, EnvFile: "/does/not/exist"}
	withKCL := native
	withKCL.EnvVars = []KCLEnvVar{{Name: "EXPO_PUBLIC_API_URL", Value: "https://api.example.test"}}
	web := FrontendEntity{Name: "web", Type: "vite-spa", Path: "web", Port: 5173, EnvFile: "/does/not/exist"}

	cases := []struct {
		name   string
		fe     FrontendEntity
		varKey string
		parent []string
		host   string
		want   string
	}{
		{"default is localhost", native, "EXPO_PUBLIC_API_URL", nil, "", listener},
		{"opt-in host", native, "EXPO_PUBLIC_API_URL", nil, "192.168.1.20", "http://192.168.1.20:4000"},
		{"KCL env_vars beats opt-in", withKCL, "EXPO_PUBLIC_API_URL", nil, "192.168.1.20", "https://api.example.test"},
		{"parent shell beats opt-in", native, "EXPO_PUBLIC_API_URL", []string{"EXPO_PUBLIC_API_URL=http://tunnel.test"}, "192.168.1.20", "http://tunnel.test"},
		{"parent shell beats default localhost", native, "EXPO_PUBLIC_API_URL", []string{"EXPO_PUBLIC_API_URL=http://tunnel.test"}, "", "http://tunnel.test"},
		{"KCL env_vars beats parent shell (declared wins)", withKCL, "EXPO_PUBLIC_API_URL", []string{"EXPO_PUBLIC_API_URL=http://tunnel.test"}, "192.168.1.20", "https://api.example.test"},
		{"web frontend: injected origin still beats shell", web, "VITE_API_URL", []string{"VITE_API_URL=http://shell.test"}, "", listener},
		{"web frontend ignores opt-in", web, "VITE_API_URL", nil, "192.168.1.20", listener},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fl := frontendLaunch{env: "dev", apiBaseURL: listener, deviceHost: tc.host}
			cmd := buildFrontendDevCmd(context.Background(), fl, tc.fe, tc.parent)
			if got := nativeOriginEnvMap(cmd.Env)[tc.varKey]; got != tc.want {
				t.Fatalf("%s = %q, want %q", tc.varKey, got, tc.want)
			}
		})
	}
}

// A native frontend's API origin follows KCL > .env.<env> > shell > injected:
// a value in the frontend's own .env.<env> beats the shell and forge's
// injection, and KCL beats the file.
func TestBuildFrontendDevCmd_NativeAPIURLPrecedence(t *testing.T) {
	const listener = "http://localhost:4000"
	envFile := filepath.Join(t.TempDir(), ".env.dev")
	if err := os.WriteFile(envFile, []byte("EXPO_PUBLIC_API_URL=http://env-file.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	native := FrontendEntity{Name: "mobile", Type: "react-native", Path: "mobile", Port: 8081, EnvFile: envFile}
	web := FrontendEntity{Name: "web", Type: "vite-spa", Path: "web", Port: 5173, EnvFile: envFile}
	withKCL := native
	withKCL.EnvVars = []KCLEnvVar{{Name: "EXPO_PUBLIC_API_URL", Value: "http://kcl.test"}}
	get := func(fe FrontendEntity, key, host string, parent []string) string {
		fl := frontendLaunch{env: "dev", apiBaseURL: listener, deviceHost: host}
		return nativeOriginEnvMap(buildFrontendDevCmd(context.Background(), fl, fe, parent).Env)[key]
	}
	if got := get(native, "EXPO_PUBLIC_API_URL", "", nil); got != "http://env-file.test" {
		t.Errorf(".env.<env> must beat forge's injection, got %q", got)
	}
	if got := get(native, "EXPO_PUBLIC_API_URL", "192.168.1.20", nil); got != "http://env-file.test" {
		t.Errorf(".env.<env> must beat the device-host origin too, got %q", got)
	}
	if got := get(native, "EXPO_PUBLIC_API_URL", "", []string{"EXPO_PUBLIC_API_URL=http://shell.test"}); got != "http://env-file.test" {
		t.Errorf("a value in .env.<env> still beats the shell (declared wins), got %q", got)
	}
	if got := get(withKCL, "EXPO_PUBLIC_API_URL", "", nil); got != "http://kcl.test" {
		t.Errorf("KCL must beat .env.<env>, got %q", got)
	}
	if got := get(web, "VITE_API_URL", "", nil); got != listener {
		t.Errorf("a web frontend is unchanged: forge's origin wins, got %q", got)
	}
}

func TestAnnounceNativeAPIOrigin(t *testing.T) {
	const listener = "http://localhost:4000"
	fe := FrontendEntity{Name: "mobile", Type: "react-native", Path: "mobile", Port: 8081, EnvFile: "/does/not/exist"}

	run := func(fl frontendLaunch, fe FrontendEntity, parent []string) string {
		cmd := buildFrontendDevCmd(context.Background(), fl, fe, parent)
		var b bytes.Buffer
		announceNativeAPIOrigin(&b, fl, fe, cmd.Env)
		return b.String()
	}

	t.Run("default prints origin and the opt-in note", func(t *testing.T) {
		out := run(frontendLaunch{env: "dev", apiBaseURL: listener}, fe, nil)
		for _, want := range []string{"API origin http://localhost:4000", "iOS simulator", "10.0.2.2", "--device-host", deviceHostEnv} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
	})
	t.Run("opt-in prints origin without the localhost note", func(t *testing.T) {
		out := run(frontendLaunch{env: "dev", apiBaseURL: listener, deviceHost: "192.168.1.20"}, fe, nil)
		if !strings.Contains(out, "API origin http://192.168.1.20:4000") || strings.Contains(out, "simulator") {
			t.Errorf("unexpected output:\n%s", out)
		}
		// A default Release build bakes the localhost API origin: the one-line hint.
		if !strings.Contains(out, "Release build bakes EXPO_PUBLIC_API_URL") || !strings.Contains(out, "EXPO_PUBLIC_API_URL=http://192.168.1.20:4000") {
			t.Errorf("missing the Release build hint:\n%s", out)
		}
		// Android Release refuses cleartext: the hint must not promise it works there.
		if !strings.Contains(out, "an Android Release build refuses plain http://") {
			t.Errorf("the Release hint does not warn that Android Release blocks cleartext:\n%s", out)
		}
	})
	t.Run("https device origin: no cleartext caveat", func(t *testing.T) {
		out := run(frontendLaunch{env: "dev", apiBaseURL: listener, deviceHost: "192.168.1.20"}, fe,
			[]string{"EXPO_PUBLIC_API_URL=https://tunnel.test"})
		if strings.Contains(out, "refuses plain http://") {
			t.Errorf("cleartext caveat printed for an https origin:\n%s", out)
		}
	})
	t.Run("no device host: no Release hint", func(t *testing.T) {
		if out := run(frontendLaunch{env: "dev", apiBaseURL: listener}, fe, nil); strings.Contains(out, "Release build") {
			t.Errorf("unexpected Release hint without a device host:\n%s", out)
		}
	})
	t.Run("explicit override is reported as the effective origin", func(t *testing.T) {
		out := run(frontendLaunch{env: "dev", apiBaseURL: listener, deviceHost: "192.168.1.20"}, fe,
			[]string{"EXPO_PUBLIC_API_URL=http://tunnel.test"})
		if !strings.Contains(out, "API origin http://tunnel.test") || !strings.Contains(out, "set explicitly") || strings.Contains(out, "192.168.1.20:4000") {
			t.Errorf("unexpected output:\n%s", out)
		}
	})
	t.Run("explicit loopback override still gets the note", func(t *testing.T) {
		out := run(frontendLaunch{env: "dev", apiBaseURL: listener}, fe, []string{"EXPO_PUBLIC_API_URL=http://127.0.0.1:9"})
		if !strings.Contains(out, "http://127.0.0.1:9") || !strings.Contains(out, "simulator") {
			t.Errorf("unexpected output:\n%s", out)
		}
	})
	t.Run("web frontend prints nothing", func(t *testing.T) {
		web := FrontendEntity{Name: "web", Type: "vite-spa", Path: "web", Port: 5173, EnvFile: "/does/not/exist"}
		if out := run(frontendLaunch{env: "dev", apiBaseURL: listener, deviceHost: "192.168.1.20"}, web, nil); out != "" {
			t.Errorf("web frontend output = %q", out)
		}
	})
	t.Run("no host API origin is silent by default and explains an opt-in", func(t *testing.T) {
		if out := run(frontendLaunch{env: "dev"}, fe, nil); out != "" {
			t.Errorf("output = %q, want none", out)
		}
		if out := run(frontendLaunch{env: "dev", deviceHost: "192.168.1.20"}, fe, nil); !strings.Contains(out, "no effect") {
			t.Errorf("output = %q, want a no-effect note", out)
		}
	})
}

func TestDeviceHostBindWarning(t *testing.T) {
	lan := net.ParseIP("192.168.1.20")
	local := []net.Addr{&net.IPNet{IP: lan, Mask: net.CIDRMask(24, 32)}, &net.IPAddr{IP: net.ParseIP("127.0.0.1")}}
	refuse := func(string, time.Duration) error { return errors.New("connection refused") }
	var dialed string
	accept := func(addr string, _ time.Duration) error { dialed = addr; return nil }
	const origin = "http://localhost:4000"

	t.Run("loopback-only API on a local LAN address warns", func(t *testing.T) {
		w := deviceHostBindWarning("192.168.1.20", origin, local, refuse)
		for _, want := range []string{"loopback only", "does not widen a bind", "4000"} {
			if !strings.Contains(w, want) {
				t.Errorf("warning missing %q: %s", want, w)
			}
		}
	})
	t.Run("reachable API is silent and the LAN address is what was dialed", func(t *testing.T) {
		if w := deviceHostBindWarning("192.168.1.20", origin, local, accept); w != "" || dialed != "192.168.1.20:4000" {
			t.Errorf("warning %q, dialed %q", w, dialed)
		}
	})
	t.Run("addresses that are not this machine are not judged", func(t *testing.T) {
		for _, host := range []string{"10.0.2.2", "mac.local", "10.9.9.9"} {
			if w := deviceHostBindWarning(host, origin, local, refuse); w != "" {
				t.Errorf("host %s: unexpected warning %q", host, w)
			}
		}
	})
	t.Run("no opt-in or no origin is silent", func(t *testing.T) {
		if deviceHostBindWarning("", origin, local, refuse) != "" || deviceHostBindWarning("192.168.1.20", "", local, refuse) != "" {
			t.Error("expected no warning")
		}
	})
}

func TestWarnDeviceHostBinding_OnlyWithNativeFrontend(t *testing.T) {
	// A loopback-only listener on 127.0.0.1; the opt-in host is an address of
	// this machine that the listener does not serve.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen on loopback: %v", err)
	}
	defer ln.Close()
	addrs, _ := net.InterfaceAddrs()
	lan := ""
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
			lan = ipn.IP.String()
			break
		}
	}
	if lan == "" {
		t.Skip("no non-loopback IPv4 address on this host")
	}
	origin := "http://localhost:" + strings.Split(ln.Addr().String(), ":")[1]

	native := &KCLEntities{Frontends: []FrontendEntity{{Name: "mobile", Type: "react-native"}}}
	web := &KCLEntities{Frontends: []FrontendEntity{{Name: "web", Type: "vite-spa"}}}

	var b bytes.Buffer
	warnDeviceHostBinding(&b, native, nil, lan, origin)
	if !strings.Contains(b.String(), "loopback only") {
		t.Errorf("native + loopback-bound API should warn, got %q", b.String())
	}
	b.Reset()
	warnDeviceHostBinding(&b, web, nil, lan, origin)
	warnDeviceHostBinding(&b, native, []string{"other"}, lan, origin)
	warnDeviceHostBinding(&b, native, nil, "", origin)
	warnDeviceHostBinding(&b, nil, nil, lan, origin)
	if b.Len() != 0 {
		t.Errorf("expected no output, got %q", b.String())
	}
}

// A stale or invalid FORGE_DEV_DEVICE_HOST exported globally must not fail a
// run that launches no native frontend: up passes a nil env lookup then, and
// resolveDeviceHost must treat that as "no opt-in" rather than erroring.
func TestTargetsNativeFrontendGatesDeviceHostEnv(t *testing.T) {
	native := &KCLEntities{Frontends: []FrontendEntity{{Name: "mobile", Type: "rn"}, {Name: "web", Type: "vite-spa"}}}
	web := &KCLEntities{Frontends: []FrontendEntity{{Name: "web", Type: "nextjs"}}}
	for _, tc := range []struct {
		name    string
		e       *KCLEntities
		targets []string
		want    bool
	}{
		{"native untargeted list", native, nil, true},
		{"native targeted", native, []string{"mobile"}, true},
		{"only web targeted", native, []string{"web"}, false},
		{"web-only project", web, nil, false},
		{"nil entities", nil, nil, false},
	} {
		if got := targetsNativeFrontend(tc.e, tc.targets); got != tc.want {
			t.Errorf("%s: targetsNativeFrontend = %v, want %v", tc.name, got, tc.want)
		}
	}
	if host, err := resolveDeviceHost("", nil); err != nil || host != "" {
		t.Fatalf("nil lookup must mean no opt-in, got %q, %v", host, err)
	}
	if _, err := resolveDeviceHost("not a host!", nil); err == nil {
		t.Fatal("an explicit invalid flag must still be rejected")
	}
}

func TestEnvUpDeviceHostFlagRegistered(t *testing.T) {
	f := newEnvUpCmd().Flags().Lookup(deviceHostFlag)
	if f == nil {
		t.Fatal("--device-host flag not registered")
	}
	if f.DefValue != "" {
		t.Errorf("default = %q, want empty (no opt-in)", f.DefValue)
	}
}
