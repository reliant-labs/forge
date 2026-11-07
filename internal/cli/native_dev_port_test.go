package cli

import (
	"bytes"
	"context"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

func metroProbe(freePorts ...int) func(string, int) bool {
	set := map[int]bool{}
	for _, p := range freePorts {
		set[p] = true
	}
	return func(_ string, port int) bool { return set[port] }
}

func TestResolveEphemeralFrontendPorts_NativeMetroPort(t *testing.T) {
	native := func(name string, port int) FrontendEntity {
		return FrontendEntity{Name: name, Type: "react-native", Port: port}
	}
	web := func(name string, port int) FrontendEntity {
		return FrontendEntity{Name: name, Type: "vite-spa", Port: port}
	}

	cases := []struct {
		name    string
		fes     []FrontendEntity
		targets []string
		free    func(string, int) bool
		// want maps frontend name to an exact port, or -1 for "some ephemeral port".
		want map[string]int
	}{
		{"native prefers 8081 when free", []FrontendEntity{native("mobile", 0)}, nil, metroProbe(8081), map[string]int{"mobile": 8081}},
		{"native falls back to ephemeral when 8081 is busy", []FrontendEntity{native("mobile", 0)}, nil, metroProbe(), map[string]int{"mobile": -1}},
		{"declared port wins, even when 8081 is free", []FrontendEntity{native("mobile", 9000)}, nil, metroProbe(8081), map[string]int{"mobile": 9000}},
		{"declared 8081 is kept verbatim", []FrontendEntity{native("mobile", 8081)}, nil, metroProbe(), map[string]int{"mobile": 8081}},
		{"web frontend never takes 8081", []FrontendEntity{web("site", 0)}, nil, metroProbe(8081), map[string]int{"site": -1}},
		{"only the first native frontend gets 8081", []FrontendEntity{native("a", 0), native("b", 0)}, nil, metroProbe(8081), map[string]int{"a": 8081, "b": -1}},
		{"a declared 8081 elsewhere is not taken", []FrontendEntity{native("mobile", 0), web("site", 8081)}, nil, metroProbe(8081), map[string]int{"mobile": -1, "site": 8081}},
		{"an untargeted native frontend stays ephemeral", []FrontendEntity{native("mobile", 0), native("other", 0)}, []string{"other"}, metroProbe(8081), map[string]int{"mobile": -1, "other": 8081}},
		{"nil probe means ephemeral", []FrontendEntity{native("mobile", 0)}, nil, nil, map[string]int{"mobile": -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &KCLEntities{Frontends: append([]FrontendEntity(nil), tc.fes...)}
			cfg := &config.ProjectConfig{}
			for _, fe := range tc.fes {
				cfg.Frontends = append(cfg.Frontends, config.FrontendConfig{Name: fe.Name, Port: fe.Port})
			}
			resolveEphemeralFrontendPorts(cfg, e, tc.targets, tc.free)
			for i, fe := range e.Frontends {
				want := tc.want[fe.Name]
				switch {
				case want == -1 && fe.Port <= 0, want == -1 && fe.Port == 8081:
					t.Errorf("%s port = %d, want an ephemeral port", fe.Name, fe.Port)
				case want > 0 && fe.Port != want:
					t.Errorf("%s port = %d, want %d", fe.Name, fe.Port, want)
				}
				if cfg.Frontends[i].Port != fe.Port {
					t.Errorf("%s: cfg port %d out of step with entity port %d", fe.Name, cfg.Frontends[i].Port, fe.Port)
				}
			}
		})
	}
}

func TestResolveEphemeralFrontendPorts_HostWorkloadKeeps8081(t *testing.T) {
	e := &KCLEntities{
		Frontends: []FrontendEntity{{Name: "mobile", Type: "react-native"}},
		Workloads: []WorkloadEntity{hostWL("api", withListenPorts(8081))},
	}
	if got := claimedFrontendPorts(e); !got[8081] {
		t.Fatalf("host workload port 8081 not claimed: %v", got)
	}
	resolveEphemeralFrontendPorts(nil, e, nil, metroProbe(8081))
	if e.Frontends[0].Port == 8081 || e.Frontends[0].Port <= 0 {
		t.Fatalf("mobile port = %d, want an ephemeral port distinct from the API's 8081", e.Frontends[0].Port)
	}
}

func TestPickNativeMetroPort_PassesFrontendName(t *testing.T) {
	var gotName string
	var gotPort int
	pickNativeMetroPort("mobile", nil, func(name string, port int) bool { gotName, gotPort = name, port; return true })
	if gotName != "mobile" || gotPort != expoMetroDefaultPort {
		t.Fatalf("probe called with (%q, %d)", gotName, gotPort)
	}
}

func TestTCPPortFree_RealListener(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen on loopback: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if tcpPortFree(port) {
		t.Errorf("port %d reported free while a listener holds it", port)
	}
	_ = l.Close()
	if !tcpPortFree(port) {
		t.Errorf("port %d reported busy after the listener closed", port)
	}
}

func TestDescribeNativeMetroPort(t *testing.T) {
	if got := describeNativeMetroPort("mobile", 8081); got != nil {
		t.Errorf("default port must print nothing, got %v", got)
	}
	if got := describeNativeMetroPort("mobile", 0); got != nil {
		t.Errorf("no port must print nothing, got %v", got)
	}
	lines := describeNativeMetroPort("mobile", 54321)
	if len(lines) != 1 {
		t.Fatalf("want exactly one line, got %d: %v", len(lines), lines)
	}
	for _, want := range []string{
		"frontend mobile", ":54321", "-RCT_jsLocation localhost:54321", "RCT_METRO_PORT=54321",
		"adb reverse tcp:54321 tcp:54321", "localhost:54321", "dev client",
	} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("line missing %q:\n%s", want, lines[0])
		}
	}
	if strings.Contains(lines[0], "\n") {
		t.Errorf("line must be a single line:\n%s", lines[0])
	}
}

func TestAnnounceNativeAPIOrigin_MetroPort(t *testing.T) {
	run := func(fe FrontendEntity) string {
		fl := frontendLaunch{env: "dev"}
		cmd := buildFrontendDevCmd(context.Background(), fl, fe, nil)
		var b bytes.Buffer
		announceNativeAPIOrigin(&b, fl, fe, cmd.Env)
		return b.String()
	}
	mobile := func(port int) FrontendEntity {
		return FrontendEntity{Name: "mobile", Type: "react-native", Path: "mobile", Port: port, EnvFile: "/does/not/exist"}
	}
	if out := run(mobile(8081)); strings.Contains(out, "RCT_jsLocation") {
		t.Errorf("Metro on 8081 must not print a redirect:\n%s", out)
	}
	out := run(mobile(53000))
	if !strings.Contains(out, "-RCT_jsLocation localhost:"+strconv.Itoa(53000)) {
		t.Errorf("ephemeral Metro port must print the redirect:\n%s", out)
	}
	web := FrontendEntity{Name: "web", Type: "vite-spa", Path: "web", Port: 53001, EnvFile: "/does/not/exist"}
	if out := run(web); out != "" {
		t.Errorf("web frontend must print nothing, got %q", out)
	}
}

func TestBuildFrontendDevCmd_DevlogOrigin(t *testing.T) {
	native := func(port int, vars ...KCLEnvVar) FrontendEntity {
		return FrontendEntity{Name: "mobile", Type: "react-native", Path: "mobile", Port: port, EnvFile: "/does/not/exist", EnvVars: vars}
	}
	web := FrontendEntity{Name: "site", Type: "vite-spa", Path: "site", Port: 5173, EnvFile: "/does/not/exist"}
	next := FrontendEntity{Name: "app", Type: "nextjs", Path: "app", Port: 3000, EnvFile: "/does/not/exist"}

	cases := []struct {
		name   string
		fe     FrontendEntity
		host   string
		parent []string
		want   string // "" means the variable must be absent
	}{
		{"native defaults to localhost", native(8081), "", nil, "http://localhost:8081"},
		{"native uses an ephemeral Metro port", native(53123), "", nil, "http://localhost:53123"},
		{"native uses the device host", native(8081), "192.168.1.20", nil, "http://192.168.1.20:8081"},
		{"android emulator alias", native(8081), "10.0.2.2", nil, "http://10.0.2.2:8081"},
		{"ipv6 device host is bracketed", native(8081), "fe80::1", nil, "http://[fe80::1]:8081"},
		{"shell value wins", native(8081), "192.168.1.20", []string{devlogOriginEnv + "=http://tunnel.test:9"}, "http://tunnel.test:9"},
		{"KCL env_vars value wins", native(8081, KCLEnvVar{Name: devlogOriginEnv, Value: "http://declared.test:1"}), "192.168.1.20", nil, "http://declared.test:1"},
		{"KCL beats shell", native(8081, KCLEnvVar{Name: devlogOriginEnv, Value: "http://declared.test:1"}), "", []string{devlogOriginEnv + "=http://tunnel.test:9"}, "http://declared.test:1"},
		{"empty shell value is not a declaration", native(8081), "", []string{devlogOriginEnv + "="}, "http://localhost:8081"},
		{"native without a port gets nothing", native(0), "", nil, ""},
		{"vite web frontend never", web, "192.168.1.20", nil, ""},
		{"nextjs web frontend never", next, "", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := buildFrontendDevCmd(context.Background(), frontendLaunch{env: "dev", deviceHost: tc.host}, tc.fe, tc.parent)
			got, present := envLookupLast(cmd.Env, devlogOriginEnv)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("%s = %q, want unset", devlogOriginEnv, got)
				}
				return
			}
			if !present || got != tc.want {
				t.Fatalf("%s = %q, want %q", devlogOriginEnv, got, tc.want)
			}
		})
	}
}

func TestMetroPreference_Settle(t *testing.T) {
	newEntities := func() (*KCLEntities, *config.ProjectConfig) {
		e := &KCLEntities{Frontends: []FrontendEntity{{Name: "mobile", Type: "react-native", Port: 8081}}}
		cfg := &config.ProjectConfig{Frontends: []config.FrontendConfig{{Name: "mobile", Port: 8081}}}
		return e, cfg
	}
	alloc := func(port int) func() (int, error) { return func() (int, error) { return port, nil } }
	always := func(ok bool) func(string, int) bool { return func(string, int) bool { return ok } }

	t.Run("free preferred port is kept", func(t *testing.T) {
		e, cfg := newEntities()
		m := &metroPreference{cfg: cfg, names: map[string]bool{"mobile": true}, alloc: alloc(54000)}
		var out bytes.Buffer
		if got := m.settle(e, "mobile", always(true), &out); got != 8081 || e.Frontends[0].Port != 8081 || out.Len() != 0 {
			t.Fatalf("port=%d entity=%d out=%q", got, e.Frontends[0].Port, out.String())
		}
	})

	t.Run("foreign holder moves the frontend to an ephemeral port", func(t *testing.T) {
		e, cfg := newEntities()
		m := &metroPreference{cfg: cfg, names: map[string]bool{"mobile": true}, alloc: alloc(54000)}
		var out bytes.Buffer
		if got := m.settle(e, "mobile", always(false), &out); got != 54000 {
			t.Fatalf("settle = %d, want 54000", got)
		}
		if e.Frontends[0].Port != 54000 || cfg.Frontends[0].Port != 54000 {
			t.Fatalf("entity=%d cfg=%d, want both 54000", e.Frontends[0].Port, cfg.Frontends[0].Port)
		}
		if !strings.Contains(out.String(), "8081") || !strings.Contains(out.String(), "54000") || !strings.Contains(out.String(), "left running") {
			t.Fatalf("announcement = %q", out.String())
		}
		// The move is final: a later re-check (launch) must not touch it again.
		if got := m.settle(e, "mobile", always(false), &out); got != 0 || e.Frontends[0].Port != 54000 {
			t.Fatalf("second settle = %d port=%d", got, e.Frontends[0].Port)
		}
	})

	t.Run("a declared port is never in the preference set", func(t *testing.T) {
		e, cfg := newEntities()
		e.Frontends[0].Port, cfg.Frontends[0].Port = 9000, 9000
		prefs := resolveEphemeralFrontendPorts(cfg, e, nil, metroProbe(8081))
		m := &metroPreference{cfg: cfg, names: prefs, alloc: alloc(54000)}
		m.settleAll(e, always(false), &bytes.Buffer{})
		if e.Frontends[0].Port != 9000 {
			t.Fatalf("declared port changed to %d", e.Frontends[0].Port)
		}
	})

	t.Run("a resolved 8081 preference is recorded, an ephemeral one is not", func(t *testing.T) {
		e := &KCLEntities{Frontends: []FrontendEntity{{Name: "mobile", Type: "react-native"}, {Name: "other", Type: "react-native"}}}
		prefs := resolveEphemeralFrontendPorts(nil, e, nil, metroProbe(8081))
		if !prefs["mobile"] || prefs["other"] || len(prefs) != 1 {
			t.Fatalf("prefs = %v", prefs)
		}
	})

	t.Run("allocation failure leaves the port and does not panic", func(t *testing.T) {
		e, cfg := newEntities()
		m := &metroPreference{cfg: cfg, names: map[string]bool{"mobile": true}, alloc: func() (int, error) { return 0, net.ErrClosed }}
		var out bytes.Buffer
		if got := m.settle(e, "mobile", always(false), &out); got != 8081 || e.Frontends[0].Port != 8081 {
			t.Fatalf("settle = %d port=%d", got, e.Frontends[0].Port)
		}
	})

	t.Run("nil receiver is a no-op", func(t *testing.T) {
		e, _ := newEntities()
		var m *metroPreference
		if m.settle(e, "mobile", always(false), &bytes.Buffer{}) != 0 {
			t.Fatal("nil settle should return 0")
		}
		m.settleAll(e, always(false), &bytes.Buffer{})
	})
}

// The relocated port must reach everything downstream of the resolve step: the
// preflight conflict scan, the PORT env, the port hint, and the devlog origin.
func TestMetroPreference_FlowsDownstream(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen on loopback: %v", err)
	}
	defer func() { _ = l.Close() }()
	held := l.Addr().(*net.TCPAddr).Port

	e := &KCLEntities{Frontends: []FrontendEntity{{Name: "mobile", Type: "react-native", Path: "mobile", Port: held, EnvFile: "/does/not/exist"}}}
	m := &metroPreference{names: map[string]bool{"mobile": true}, alloc: func() (int, error) { return 54001, nil }}

	if got := conflictingPorts(e, nil, true, portInUse); len(got) != 1 {
		t.Fatalf("before settle: conflicts = %v, want the held port", got)
	}
	m.settleAll(e, forgeReclaimablePortFree("p", "e"), &bytes.Buffer{})
	if got := conflictingPorts(e, nil, true, portInUse); len(got) != 0 {
		t.Fatalf("after settle: conflicts = %v, want none", got)
	}

	fl := frontendLaunch{env: "dev", deviceHost: "192.168.1.20", entities: e}
	fe := e.Frontends[0]
	cmd := buildFrontendDevCmd(context.Background(), fl, fe, nil)
	if got := envLookupLastOr(cmd.Env, "PORT"); got != "54001" {
		t.Errorf("PORT = %q, want 54001", got)
	}
	if got := envLookupLastOr(cmd.Env, devlogOriginEnv); got != "http://192.168.1.20:54001" {
		t.Errorf("devlog origin = %q", got)
	}
	var out bytes.Buffer
	announceNativeAPIOrigin(&out, fl, fe, cmd.Env)
	if !strings.Contains(out.String(), "RCT_jsLocation localhost:54001") {
		t.Errorf("port hint missing the new port:\n%s", out.String())
	}
}
