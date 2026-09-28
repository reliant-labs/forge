package cli

import (
	"context"
	"io"
	"strings"
	"testing"
)

// The rule these tests pin: an image registry is DECLARED in the env's KCL
// (deploy/kcl/<env>/main.k) and nowhere else. `--push` is a switch — "push
// this build" — never a carrier for a registry value, so there is exactly one
// place the destination can come from and `forge env deploy` pulls from the
// same one by construction.

// declaredRegistryFixture is a cluster env whose ClusterTarget declares the
// registry. The workload's runtime carries it too, exactly as render.k
// projects the target onto each workload.
const declaredRegistryFixture = `{
  "output": {
    "cluster_target": {
      "cluster": "c",
      "namespace": "n",
      "registry": "registry.example/prod"
    },
    "workloads": [
      {
        "name": "pt",
        "kind": "service",
        "image": "pt",
        "build": {
          "type": "go",
          "cmd": "./cmd/pt",
          "output_name": "pt"
        },
        "runtime": {
          "type": "cluster",
          "cluster": "c",
          "namespace": "n",
          "registry": "registry.example/prod"
        },
        "spec": {
          "kind": "service"
        }
      }
    ]
  }
}`

// hostedPushFixture is a hosted env (its Bundle declares forge.ControlPlane) with
// one workload bound to the control plane. registry is the ControlPlane's
// declared registry, "" for none.
func hostedPushFixture(registry string) string {
	reg := ""
	if registry != "" {
		reg = `,
      "registry": "` + registry + `"`
	}
	return `{
  "output": {
    "control_plane": {
      "type": "control_plane",
      "endpoint": "http://127.0.0.1:1",
      "token_env": "FORGE_CONTROL_PLANE_TOKEN"` + reg + `
    },
    "workloads": [
      {
        "name": "api",
        "kind": "service",
        "image": "pt",
        "build": {
          "type": "go",
          "cmd": "./cmd/pt",
          "output_name": "pt"
        },
        "runtime": {
          "type": "hosted"
        },
        "spec": {
          "kind": "service",
          "image": "pt"
        }
      }
    ]
  }
}`
}

// runBuildCommand drives the REAL cobra command — flag parsing included — and
// returns what it printed. --plan keeps it from building or pushing anything.
func runBuildCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var runErr error
	out := captureStdout(t, func() {
		cmd := newBuildCmd()
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		runErr = cmd.ExecuteContext(context.Background())
	})
	return out, runErr
}

// TestBuildCmd_PushIsABooleanSwitch: --push carries no value. A bool flag is
// what makes `--push <anything>` impossible to express, rather than a value
// the code has to remember to ignore.
func TestBuildCmd_PushIsABooleanSwitch(t *testing.T) {
	f := newBuildCmd().Flags().Lookup("push")
	if f == nil {
		t.Fatal("--push is not registered on forge build")
	}
	if f.Value.Type() != "bool" {
		t.Fatalf("--push is a %s flag; it must be a bool — the registry is declared in deploy/kcl/<env>/main.k, never passed", f.Value.Type())
	}
}

// TestBuildCmd_PushResolvesTheEnvDeclaredRegistry: `forge build prod --push`
// pushes to the registry prod's KCL declares, and says where it came from.
func TestBuildCmd_PushResolvesTheEnvDeclaredRegistry(t *testing.T) {
	planProject(t, declaredRegistryFixture)

	for name, args := range map[string][]string{
		"push last":  {"prod", "--plan", "--no-generate", "--tag", "t1", "--push"},
		"push first": {"--push", "prod", "--plan", "--no-generate", "--tag", "t1"},
		"push=true":  {"prod", "--push=true", "--plan", "--no-generate", "--tag", "t1"},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := runBuildCommand(t, args...)
			if err != nil {
				t.Fatalf("forge build %s: %v", strings.Join(args, " "), err)
			}
			if !strings.Contains(out, "push registry.example/prod/pt:t1") {
				t.Errorf("--push should push to the env-declared registry registry.example/prod; plan output:\n%s", out)
			}
			if !strings.Contains(out, "Push:     registry.example/prod (declared in deploy/kcl/prod/main.k)") {
				t.Errorf("the build header should say where the registry came from; plan output:\n%s", out)
			}
		})
	}
}

// TestBuildCmd_PushRefusesARegistryValue: every spelling that used to carry a
// registry is now a usage error, so a stale script fails loudly instead of
// silently pushing somewhere its env does not deploy from.
func TestBuildCmd_PushRefusesARegistryValue(t *testing.T) {
	planProject(t, declaredRegistryFixture)

	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"equals":         {[]string{"prod", "--push=ghcr.io/acme", "--plan", "--no-generate"}, `invalid argument "ghcr.io/acme" for "--push"`},
		"space after":    {[]string{"prod", "--push", "ghcr.io/acme", "--plan", "--no-generate"}, "accepts at most 1 arg"},
		"space, no env":  {[]string{"--push", "localhost:5051", "--plan", "--no-generate"}, "forge build takes no registry"},
		"space, first":   {[]string{"--push", "ghcr.io/acme", "prod", "--plan", "--no-generate"}, "accepts at most 1 arg"},
		"env-like value": {[]string{"prod", "--push", "acme", "--plan", "--no-generate"}, "accepts at most 1 arg"},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := runBuildCommand(t, tc.args...)
			if err == nil {
				t.Fatalf("forge build %s: want a usage error, got success:\n%s", strings.Join(tc.args, " "), out)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("forge build %s: error should contain %q, got: %v", strings.Join(tc.args, " "), tc.want, err)
			}
			if strings.Contains(out, "push ") {
				t.Errorf("a refused --push value must not reach the build plan; output:\n%s", out)
			}
		})
	}
}

// TestValidateBuildEnvArg: a positional that cannot be an env name is refused
// before anything renders, and one that looks like a registry says the
// registry is declared, not passed.
func TestValidateBuildEnvArg(t *testing.T) {
	for _, ok := range []string{"dev", "prod", "dev-k8s", "e2e", "eu-west"} {
		if err := validateBuildEnvArg(ok); err != nil {
			t.Errorf("validateBuildEnvArg(%q) = %v, want nil", ok, err)
		}
	}
	for _, reg := range []string{"ghcr.io/acme", "localhost:5051", "us-central1-docker.pkg.dev/p/r"} {
		err := validateBuildEnvArg(reg)
		if err == nil || !strings.Contains(err.Error(), "takes no registry") || !strings.Contains(err.Error(), "deploy/kcl/<env>/main.k") {
			t.Errorf("validateBuildEnvArg(%q) = %v, want the declared-registry runbook", reg, err)
		}
	}
	if err := validateBuildEnvArg("Prod_1"); err == nil || !strings.Contains(err.Error(), "invalid environment name") {
		t.Errorf("validateBuildEnvArg(%q) = %v, want an invalid-name error", "Prod_1", err)
	}
}

// TestBuildCmd_PushUndeclaredRegistryFails: a cluster env that declares no
// registry fails --push with a runbook naming the file and the field — and
// never offers a flag as the way out.
func TestBuildCmd_PushUndeclaredRegistryFails(t *testing.T) {
	planProject(t, `{
  "output": {
    "workloads": [
      {
        "name": "pt",
        "kind": "service",
        "image": "pt",
        "build": {
          "type": "go",
          "cmd": "./cmd/pt",
          "output_name": "pt"
        },
        "runtime": {
          "type": "cluster",
          "cluster": "c",
          "namespace": "n"
        },
        "spec": {
          "kind": "service"
        }
      }
    ]
  }
}`)

	_, err := runBuildCommand(t, "prod", "--push", "--plan", "--no-generate", "--tag", "t1")
	if err == nil {
		t.Fatal("--push against an env that declares no registry: want an error, got nil")
	}
	for _, want := range []string{"deploy/kcl/prod/main.k", "registry", "forge.ClusterTarget"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("runbook error should name %q; got: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "--push <") || strings.Contains(err.Error(), "--push=") {
		t.Errorf("the runbook must not offer a registry flag as the fix; got: %v", err)
	}
}

// TestBuildCmd_PushWithoutEnvFails: with no env there is no declaration to
// read, so --push asks for the env instead of building without pushing.
func TestBuildCmd_PushWithoutEnvFails(t *testing.T) {
	planProject(t, declaredRegistryFixture)
	_, err := runBuildCommand(t, "--push", "--plan", "--no-generate")
	if err == nil || !strings.Contains(err.Error(), "forge build <env> --push") {
		t.Fatalf("--push with no env: want a runbook naming `forge build <env> --push`, got %v", err)
	}
}

// TestBuildCmd_HostedEnvPushesToTheControlPlaneRegistry: a hosted env declares
// its registry on forge.ControlPlane, and --push resolves it from there.
func TestBuildCmd_HostedEnvPushesToTheControlPlaneRegistry(t *testing.T) {
	planProject(t, hostedPushFixture("registry.example/org-7"))

	out, err := runBuildCommand(t, "prod", "--push", "--plan", "--no-generate", "--tag", "t1")
	if err != nil {
		t.Fatalf("forge build prod --push (hosted env declaring ControlPlane.registry): %v", err)
	}
	if !strings.Contains(out, "Push:     registry.example/org-7 (declared in deploy/kcl/prod/main.k)") {
		t.Errorf("hosted --push should resolve forge.ControlPlane.registry; plan output:\n%s", out)
	}
}

// TestBuildCmd_HostedEnvWithoutRegistryFails: a hosted env that declares no
// registry fails with a runbook naming forge.ControlPlane's `registry` — the
// control plane's advertised push base is not consulted yet, and forge never
// invents a default.
func TestBuildCmd_HostedEnvWithoutRegistryFails(t *testing.T) {
	planProject(t, hostedPushFixture(""))

	_, err := runBuildCommand(t, "prod", "--push", "--plan", "--no-generate", "--tag", "t1")
	if err == nil {
		t.Fatal("--push against a hosted env that declares no registry: want an error, got nil")
	}
	for _, want := range []string{"hosted", "forge.ControlPlane", "registry", "deploy/kcl/prod/main.k"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("hosted runbook error should name %q; got: %v", want, err)
		}
	}
}

// TestDeclaredPushRegistry_TargetNarrowingKeepsTheRegistry: the registry is an
// env-wide fact. A contract with no cluster_target states it only on its
// workloads, and `--target <frontend>` narrows those away — the resolver reads
// the FULL render, so the registry survives.
func TestDeclaredPushRegistry_TargetNarrowingKeepsTheRegistry(t *testing.T) {
	full := &KCLEntities{Workloads: []WorkloadEntity{clusterWL("api", "c", "n", func(w *WorkloadEntity) {
		w.Runtime.Cluster.Registry = "registry.example/prod"
	})}}
	if narrowed := filterEntitiesByTarget(full, []string{"web"}); declaredRegistry(narrowed) != "" {
		t.Fatal("fixture precondition: narrowing to a frontend should drop the only workload stating the registry")
	}
	got, err := resolvePushRegistry(buildOptions{env: "prod", push: true, targets: []string{"web"}}, full)
	if err != nil || got.registry != "registry.example/prod" {
		t.Fatalf("resolvePushRegistry = (%+v, %v), want registry.example/prod", got, err)
	}
}

// TestResolvePushRegistry_NoPushIsANoOp: a build without --push resolves no
// registry and never errors, even against an env that declares none.
func TestResolvePushRegistry_NoPushIsANoOp(t *testing.T) {
	got, err := resolvePushRegistry(buildOptions{env: "prod"}, &KCLEntities{})
	if err != nil || got != (pushRegistryChoice{}) {
		t.Fatalf("resolvePushRegistry(no --push) = (%+v, %v), want zero, nil", got, err)
	}
}

// TestDeclaredRegistry_Precedence: ClusterTarget.registry first (it is the env
// target), then a cluster workload's, then forge.ControlPlane.registry. One
// resolution, shared by --push, `forge registry login` and `forge env up`.
func TestDeclaredRegistry_Precedence(t *testing.T) {
	cp := &ControlPlaneEntity{Type: "control_plane", Endpoint: "https://cp.example", Registry: "cp.example/org"}
	cases := map[string]struct {
		ents *KCLEntities
		want string
	}{
		"nil":                {nil, ""},
		"none":               {&KCLEntities{}, ""},
		"cluster target":     {&KCLEntities{ClusterTarget: &ClusterTargetEntity{Registry: "ct.example/x"}, ControlPlane: cp}, "ct.example/x"},
		"control plane only": {&KCLEntities{ControlPlane: cp}, "cp.example/org"},
	}
	for name, tc := range cases {
		if got := declaredRegistry(tc.ents); got != tc.want {
			t.Errorf("%s: declaredRegistry = %q, want %q", name, got, tc.want)
		}
	}
}

// TestImageTagSet_LocalTagIsTheDeclaredRegistryOrBare: a build's LOCAL tag is
// the image under the registry the env declares — so a `forge env deploy`
// against a k3d-local registry finds what was built — and, with no declared
// registry, the bare image. Never a forge.yaml registry, never the project
// name standing in for one.
func TestImageTagSet_LocalTagIsTheDeclaredRegistryOrBare(t *testing.T) {
	if got := imageTagSet("", "web", "", "t1", false).local; strings.Join(got, ",") != "web:latest,web:t1" {
		t.Errorf("no declared registry: local tags = %v, want the bare image", got)
	}
	if got := imageTagSet("ghcr.io/acme", "web", "", "t1", false).local; strings.Join(got, ",") != "ghcr.io/acme/web:latest,ghcr.io/acme/web:t1" {
		t.Errorf("declared registry: local tags = %v, want it under ghcr.io/acme", got)
	}
}

// TestBuildCmd_LocalBuildTagsUnderTheDeclaredRegistry: `forge build prod
// --docker` (no push) tags the image under prod's declared registry — read
// from the env's KCL, not forge.yaml — and pushes nothing.
func TestBuildCmd_LocalBuildTagsUnderTheDeclaredRegistry(t *testing.T) {
	planProject(t, declaredRegistryFixture)
	out, err := runBuildCommand(t, "prod", "--docker", "--plan", "--no-generate", "--tag", "t1")
	if err != nil {
		t.Fatalf("forge build prod --docker --plan: %v", err)
	}
	if strings.Contains(out, "push ") {
		t.Errorf("a build without --push must push nothing; plan output:\n%s", out)
	}
	if !strings.Contains(out, "Registry: registry.example/prod (declared in deploy/kcl/prod/main.k; tagged locally, not pushed)") {
		t.Errorf("the build header should name the env-declared registry the image is tagged under; plan output:\n%s", out)
	}
}
