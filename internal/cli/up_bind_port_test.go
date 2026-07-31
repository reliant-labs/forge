package cli

import (
	"strings"
	"testing"
)

// `forge run` allocates an ephemeral API port, PRINTS it, inlines it into the
// frontend bundle as NEXT_PUBLIC_API_URL, and hands it to the readiness gate
// and the pre-flight conflict guard. Only the process itself reads the port
// out of the environment — and host env layering is shell-wins, so an
// inherited PORT used to beat the allocated one. A measured run printed
// "ephemeral dev port 64157", wired the frontend to :64157, and bound :8099.
//
// The frontend launch path already forces its allocated PORT. These tests
// hold the same property for host services.

func envValue(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix), true
		}
	}
	return "", false
}

func TestForceHostBindPortsBeatsInheritedPort(t *testing.T) {
	// Not parallel: mutates the process environment, which forceHostBindPorts
	// reads to report what it is overriding.
	t.Setenv("PORT", "8099")

	// What LayerHostEnv produces today: the shell's PORT survives because
	// base wins, so the allocated 64157 never reaches the process.
	layered := []string{"PORT=8099", "DATABASE_URL=postgres://shell"}
	declared := map[string]string{"PORT": "64157"}

	got := forceHostBindPorts(layered, "peptides", declared)

	if v, _ := envValue(got, "PORT"); v != "64157" {
		t.Fatalf("PORT = %q, want the published 64157 — forge prints this port, "+
			"wires the frontend to it and probes it for readiness; the app must bind it", v)
	}
	if n := strings.Count(strings.Join(got, "\n"), "PORT="); n != 1 {
		t.Fatalf("PORT appears %d times in the child env; exactly one entry must survive", n)
	}
}

func TestForceHostBindPortsLeavesNonPortVarsToTheShell(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://shell")

	layered := []string{"DATABASE_URL=postgres://shell", "PORT=64157"}
	declared := map[string]string{"PORT": "64157", "DATABASE_URL": "postgres://kcl"}

	got := forceHostBindPorts(layered, "peptides", declared)

	// Overriding a dev's shell DATABASE_URL is NOT the fix. Shell-wins is the
	// deliberate policy for every host env var that is not a bind port.
	if v, _ := envValue(got, "DATABASE_URL"); v != "postgres://shell" {
		t.Fatalf("DATABASE_URL = %q, want the shell's value — only bind ports are forced", v)
	}
}

func TestForceHostBindPortsCoversServiceSpecificPortVars(t *testing.T) {
	t.Setenv("METRICS_PORT", "9000")

	layered := []string{"METRICS_PORT=9000"}
	declared := map[string]string{"METRICS_PORT": "51234"}

	got := forceHostBindPorts(layered, "peptides", declared)

	// hostEnvPorts treats every `<...>_PORT` var as a port this service binds,
	// and the pre-flight conflict guard reads that same set. A shell value
	// winning on any of them desynchronizes the guard from reality.
	if v, _ := envValue(got, "METRICS_PORT"); v != "51234" {
		t.Fatalf("METRICS_PORT = %q, want the declared 51234", v)
	}
}

func TestForceHostBindPortsIsANoOpWithoutDeclaredPorts(t *testing.T) {
	t.Parallel()

	layered := []string{"DATABASE_URL=postgres://shell"}
	got := forceHostBindPorts(layered, "peptides", map[string]string{"DATABASE_URL": "postgres://kcl"})

	if len(got) != 1 || got[0] != "DATABASE_URL=postgres://shell" {
		t.Fatalf("env changed when no bind port was declared: %v", got)
	}
}

// The three tests above prove the helper. This one proves it is WIRED —
// a decorated value constructed and then not injected is the failure this
// exercise has already found once, so the assertion belongs at the call
// site, not at the construction.
func TestBuildHostServiceCmdBindsThePublishedPort(t *testing.T) {
	t.Setenv("PORT", "8099")

	svc := ServiceEntity{
		Name: "peptides",
		Deploy: DeployConfigEntity{
			Type: "host",
			Host: &HostDeploy{
				Runner:      "go-run",
				ListenPorts: []int{64157},
				EnvVars:     []KCLEnvVar{{Name: "PORT", Value: "64157"}},
			},
		},
	}

	cmd, _, err := buildHostServiceCmd(t.Context(), nil, svc, nil, "dev")
	if err != nil {
		t.Fatalf("buildHostServiceCmd: %v", err)
	}

	got, ok := envValue(cmd.Env, "PORT")
	if !ok {
		t.Fatal("the launched process gets no PORT at all")
	}
	if got != "64157" {
		t.Fatalf("the launched process binds PORT=%s but forge published 64157 — "+
			"the printed URL, the frontend's inlined API URL and the readiness probe all use the published value", got)
	}
}
