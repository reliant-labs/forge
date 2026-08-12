package cli

import "testing"

// TestCollapseClusterServicesToHostSkipsNonGoBuilds pins the boundary of the
// host-only collapse.
//
// Observed for real on control-plane: `forge env up dev --host-only` rewrote
// daemon-gateway (a ShellBuild that builds a binary living in a SIBLING repo)
// into `go run ./cmd/daemon-gateway server`. That package does not exist in
// control-plane, so the process died with
// "stat .../cmd/daemon-gateway: directory not found" and forge reported it as
// "nothing is listening — the service failed to bind its port", exited
// non-zero, and left a healthy stack behind. A reader chases a port conflict
// that never existed.
//
// goRunCmdForService already documents that a non-Go build "has no meaningful
// go-run target"; this test holds the caller to it.
func TestCollapseClusterServicesToHostSkipsNonGoBuilds(t *testing.T) {
	e := &KCLEntities{Services: []ServiceEntity{
		{
			Name:   "api",
			Deploy: DeployConfigEntity{Type: "cluster"},
			Build:  BuildConfigEntity{Type: "go", Go: &GoBuild{Cmd: "./cmd/app"}},
		},
		{
			// Built out of band from a sibling repo — the external_builds hatch.
			Name:   "daemon-gateway",
			Deploy: DeployConfigEntity{Type: "cluster"},
			Build:  BuildConfigEntity{Type: "shell", Shell: &ShellBuild{Cmd: "docker build -f ../reliant/Dockerfile ."}},
		},
		{
			Name:   "sidecar",
			Deploy: DeployConfigEntity{Type: "cluster"},
			Build:  BuildConfigEntity{Type: "docker", Docker: &DockerBuild{}},
		},
	}}

	collapseClusterServicesToHost(e)

	if len(e.Services) != 1 {
		names := make([]string, 0, len(e.Services))
		for _, s := range e.Services {
			names = append(names, s.Name)
		}
		t.Fatalf("expected only the Go-built service to collapse to host, got %v", names)
	}
	got := e.Services[0]
	if got.Name != "api" {
		t.Fatalf("wrong service kept: %q", got.Name)
	}
	want := []string{"go", "run", "./cmd/app", "server"}
	if len(got.Command) != len(want) {
		t.Fatalf("command = %v, want %v", got.Command, want)
	}
	for i := range want {
		if got.Command[i] != want[i] {
			t.Fatalf("command = %v, want %v", got.Command, want)
		}
	}
}

// TestCollapseClusterServicesToHostKeepsDeclaredHostServices guards the other
// half: a service with an explicit host runner is preserved verbatim, whatever
// its build shape, because the author said how to run it.
func TestCollapseClusterServicesToHostKeepsDeclaredHostServices(t *testing.T) {
	e := &KCLEntities{Services: []ServiceEntity{{
		Name:    "admin-server",
		Deploy:  DeployConfigEntity{Type: "host", Host: &HostDeploy{Runner: "air"}},
		Build:   BuildConfigEntity{Type: "shell", Shell: &ShellBuild{Cmd: "make admin"}},
		Command: []string{"air", "-c", ".air.toml"},
	}}}

	collapseClusterServicesToHost(e)

	if len(e.Services) != 1 || e.Services[0].Deploy.Host == nil ||
		e.Services[0].Deploy.Host.Runner != "air" {
		t.Fatalf("declared host service was not preserved: %+v", e.Services)
	}
}

// TestCollapseClusterServicesToHostKeepsComposeInfra guards the declaration
// the host-only dev loop depends on.
//
// A compose service is INFRASTRUCTURE — the postgres/IdP/telemetry containers
// the host processes are about to dial — and `forge run` brings it up by
// dispatching the entities this function returns. Dropping it here (as "not a
// cluster service") deletes the declaration before anything can act on it,
// and the failure is silent and awful: the stack reports success, and the app
// connects to whatever else happens to be listening on the DSN's port —
// another project's database, with the right port and the wrong data.
func TestCollapseClusterServicesToHostKeepsComposeInfra(t *testing.T) {
	e := &KCLEntities{Services: []ServiceEntity{
		{
			Name:   "dev-infra",
			Deploy: DeployConfigEntity{Type: "compose", Compose: &ComposeDeploy{Service: "dev-infra"}},
		},
		{
			Name:   "api",
			Deploy: DeployConfigEntity{Type: "cluster"},
			Build:  BuildConfigEntity{Type: "go", Go: &GoBuild{Cmd: "./cmd/api"}},
		},
	}}

	collapseClusterServicesToHost(e)

	var infra, host int
	for _, svc := range e.Services {
		switch svc.Deploy.Type {
		case "compose":
			infra++
		case "host":
			host++
		}
	}
	if infra != 1 {
		t.Errorf("compose infra was dropped (%d survived, want 1): host services would start against nothing", infra)
	}
	if host != 1 {
		t.Errorf("cluster service did not collapse to host (%d, want 1)", host)
	}
}
