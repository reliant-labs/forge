package cli

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
)

// The runtime checks probe the port `forge env status` RESOLVED, not a
// default.
//
// `forge doctor`'s App Health looked up "app:8080" and, in a project serving
// on :3091, rendered a gray dash that was indistinguishable from "not
// applicable". The fix is not a better default: it is that the check's
// address comes from the rows the status table was built from — the rendered
// KCL plus the live-port overlay plus an actual probe. There is exactly one
// resolver, and this is the seam onto it.
func TestRuntimeTargetComesFromTheResolvedRows(t *testing.T) {
	rows := []upServiceRow{
		// Not listening: a down service must not become the probe target,
		// or every check downstream reports a connection error instead of
		// the honest "nothing is up".
		{Name: "admin-server", Kind: "host", Port: 8090, Listening: false},
		{Name: "reliant-api-server", Kind: "host", Port: 3091, Listening: true},
		{Name: "reliant-web", Kind: "frontend", Port: 3000, Listening: true},
	}
	entities := &KCLEntities{Services: []ServiceEntity{
		{Name: "reliant-api-server", Deploy: DeployConfigEntity{Type: "host", Host: &HostDeploy{
			EnvVars: []KCLEnvVar{{Name: "PPROF_ADDR", Value: ":6061"}},
		}}},
	}}

	got := runtimeTargetFor(entities, rows)
	if got.Service != "reliant-api-server" {
		t.Errorf("Service = %q, want the first LISTENING host service", got.Service)
	}
	if got.HTTP != "localhost:3091" {
		t.Errorf("HTTP = %q, want localhost:3091 (the resolved port), not a guessed default", got.HTTP)
	}
	// pprof is a SEPARATE serverkit listener; deriving it from the HTTP port
	// would be the same class of guess.
	if got.Pprof != "localhost:6061" {
		t.Errorf("Pprof = %q, want localhost:6061 from the service's declared PPROF_ADDR", got.Pprof)
	}
}

// Nothing listening means no target — and no target means the dependent
// check reports UNDETERMINED. Inventing one would produce a confident
// failure about a port nothing ever bound.
func TestRuntimeTargetIsEmptyWhenNothingIsListening(t *testing.T) {
	rows := []upServiceRow{
		{Name: "admin-server", Kind: "host", Port: 8090, Listening: false},
		{Name: "admin-web", Kind: "frontend", Port: 3000, Listening: true},
	}
	got := runtimeTargetFor(nil, rows)
	if got.HTTP != "" || got.Service != "" {
		t.Errorf("target = %+v, want empty: no host service is up, and a frontend is not the app", got)
	}
}

func TestPprofPortFromAddr(t *testing.T) {
	cases := map[string]string{
		":6060":            "6060",
		"0.0.0.0:6060":     "6060",
		"localhost:16060":  "16060",
		"":                 "",
		"not-an-addr":      "",
		"localhost:notnum": "",
		":0":               "",
	}
	for in, want := range cases {
		if got := pprofPortFromAddr(in); got != want {
			t.Errorf("pprofPortFromAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

// `forge env status <env> --json` must emit a document that PARSES.
//
// activateDevStack printed `[devstack] worktree="" branch="main"` to STDOUT
// before the render, so on any checkout with a branch — i.e. all of them —
// the JSON stream was prefixed with a log line and `json.Unmarshal` failed.
// This is the discovery call agents and scripts make (it carries the API
// port and the DATABASE_URL), so the contract being unparseable is the whole
// value of the flag.
func TestEnvStatusJSONIsParseable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	dir := t.TempDir()
	writeForgeYAML(t, dir, `name: demo
module_path: github.com/example/demo
features:
  codegen: true
  frontend: false
`)
	fixture := fmt.Sprintf(`{
      "services": [
        {"name": "admin-server", "deploy": {"type": "host", "runner": "go-run",
          "env_vars": [{"name": "ADMIN_SERVER_PORT", "value": "%d"}]}}
      ]
    }`, port)
	t.Setenv("FORGE_KCL_RENDER_FIXTURE", writeKCLFixture(t, fixture))
	t.Chdir(dir)

	out := captureStdout(t, func() {
		if err := runUpServices(t.Context(), "dev", true, "", false); err != nil {
			t.Fatalf("runUpServices: %v", err)
		}
	})

	var rep upServicesReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("`env status --json` did not emit parseable JSON (%v); stdout was:\n%s", err, out)
	}
	if rep.Env != "dev" {
		t.Errorf("env = %q, want dev", rep.Env)
	}
	if len(rep.Checks) == 0 {
		t.Error("the JSON envelope carries no runtime checks — they moved here from `forge doctor`")
	}
	// And no diagnostic text leaked into the document.
	if strings.Contains(out, "[devstack]") {
		t.Errorf("the devstack diagnostic is on stdout, inside the JSON stream:\n%s", out)
	}
}
