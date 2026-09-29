package cli

import (
	"context"
	"io"
	"strings"
	"testing"
)

// The rule these tests pin: an image registry is DECLARED on a WORKLOAD, as
// part of its image, and nowhere else. No env declares one. `--push` is a
// switch — "push these builds" — never a carrier for a registry value, so each
// destination has exactly one source and `forge env deploy` pulls from the same
// one by construction.

// declaredRegistryFixture is a cluster env whose WORKLOAD declares the full
// image reference. Nothing on the env carries a registry.
const declaredRegistryFixture = `{
  "output": {
    "cluster_target": {
      "cluster": "c",
      "namespace": "n"
    },
    "workloads": [
      {
        "name": "pt",
        "kind": "service",
        "image": "registry.example/prod/pt",
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
}`

// hostedPushFixture is a hosted env (its Bundle declares forge.ControlPlane) with
// one workload bound to the control plane. registry is the ControlPlane's
// declared registry, "" for none.
func hostedPushFixture(registry string) string {
	image := "pt"
	if registry != "" {
		image = registry + "/pt"
	}
	return `{
  "output": {
    "control_plane": {
      "type": "control_plane",
      "endpoint": "http://127.0.0.1:1",
      "token_env": "FORGE_CONTROL_PLANE_TOKEN"
    },
    "workloads": [
      {
        "name": "api",
        "kind": "service",
        "image": "` + image + `",
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
          "image": "` + image + `"
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
		t.Fatalf("--push is a %s flag; it must be a bool — each registry is declared on its workload's image, never passed", f.Value.Type())
	}
}

// TestBuildCmd_PushResolvesTheWorkloadDeclaredReference: `forge build prod
// --push` pushes to the reference the workload declares, and says which
// workload it came from.
func TestBuildCmd_PushResolvesTheWorkloadDeclaredReference(t *testing.T) {
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
				t.Errorf("--push should push to the workload's declared reference; plan output:\n%s", out)
			}
			if !strings.Contains(out, `Image:    registry.example/prod/pt (declared by workload "pt"; pushed)`) {
				t.Errorf("the build header should name the workload that declared the reference; plan output:\n%s", out)
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
		if err == nil || !strings.Contains(err.Error(), "takes no registry") || !strings.Contains(err.Error(), "deploy/kcl/workloads.k") {
			t.Errorf("validateBuildEnvArg(%q) = %v, want the declared-registry runbook", reg, err)
		}
	}
	if err := validateBuildEnvArg("Prod_1"); err == nil || !strings.Contains(err.Error(), "invalid environment name") {
		t.Errorf("validateBuildEnvArg(%q) = %v, want an invalid-name error", "Prod_1", err)
	}
}

// TestBuildCmd_PushBareImageFails: a cluster workload whose image names no
// registry host fails --push with a runbook naming the file and the field — and
// never offers a flag as the way out.
func TestBuildCmd_PushBareImageFails(t *testing.T) {
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
		t.Fatal("--push with no pushable image: want an error, got nil")
	}
	for _, want := range []string{"deploy/kcl/workloads.k", "image", "ghcr.io/<owner>"} {
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

// TestBuildCmd_HostedEnvPushesToTheWorkloadReference: a hosted workload
// declares its own reference, exactly as a cluster one does — the runtime does
// not change where a registry is declared.
func TestBuildCmd_HostedEnvPushesToTheWorkloadReference(t *testing.T) {
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

// TestResolvePushPlan_TargetNarrowingKeepsEveryDestination: a --target
// narrowing drops workloads from the set the build ACTS on, but the
// destinations are resolved from the FULL render — so the header still names
// every image and a narrowed build does not lose a push target.
func TestResolvePushPlan_TargetNarrowingKeepsEveryDestination(t *testing.T) {
	full := &KCLEntities{Workloads: []WorkloadEntity{
		clusterWL("api", "c", "n", func(w *WorkloadEntity) {
			w.Image = "ghcr.io/acme/api"
			w.Build.Type = "go"
		}),
	}}
	if narrowed := filterEntitiesByTarget(full, []string{"web"}); len(declaredImageDestinations(narrowed)) != 0 {
		t.Fatal("fixture precondition: narrowing to a frontend should drop the only workload declaring an image")
	}
	got, err := resolvePushPlan(buildOptions{env: "prod", push: true, targets: []string{"web"}}, full)
	if err != nil {
		t.Fatalf("resolvePushPlan: %v", err)
	}
	if len(got.destinations) != 1 || got.destinations[0].repository != "ghcr.io/acme/api" {
		t.Fatalf("destinations = %+v, want ghcr.io/acme/api from the full render", got.destinations)
	}
}

// TestResolvePushPlan_NoPushIsANoOp: a build without --push resolves no push
// and never errors, even against an env with nothing pushable.
func TestResolvePushPlan_NoPushIsANoOp(t *testing.T) {
	got, err := resolvePushPlan(buildOptions{env: "prod"}, &KCLEntities{})
	if err != nil {
		t.Fatalf("resolvePushPlan(no --push) = %v, want nil", err)
	}
	if got.push || len(got.destinations) != 0 {
		t.Fatalf("resolvePushPlan(no --push) = %+v, want no push and no destinations", got)
	}
}

// TestDeclaredImageDestinations_TwoWorkloadsTwoRegistries is the case an
// env-wide registry could not express at all: two workloads in ONE env, each
// pushing to its own registry. Both are destinations, each labelled with the
// workload that declared it, and `forge registry login` derives both hosts.
func TestDeclaredImageDestinations_TwoWorkloadsTwoRegistries(t *testing.T) {
	ents := &KCLEntities{Workloads: []WorkloadEntity{
		clusterWL("admin", "c", "n", func(w *WorkloadEntity) {
			w.Image = "ghcr.io/reliant-labs/app"
			w.Build.Type = "go"
		}),
		clusterWL("workspace-proxy", "c2", "n2", func(w *WorkloadEntity) {
			w.Image = "us-central1-docker.pkg.dev/proj/repo/daemon"
			w.Build.Type = "docker"
		}),
		// A third-party image is PULLED, never pushed: including it would make
		// `forge registry login` demand docker.io credentials nothing needs.
		clusterWL("nats", "c", "n", func(w *WorkloadEntity) {
			w.Image = "docker.io/library/nats:2.10"
			w.Build = BuildConfigEntity{} // forge builds nothing for it
		}),
	}}

	dests := declaredImageDestinations(ents)
	if len(dests) != 2 {
		t.Fatalf("destinations = %+v, want exactly the two BUILT images", dests)
	}
	byRepo := map[string]string{}
	for _, d := range dests {
		byRepo[d.repository] = d.workload
	}
	for repo, workload := range map[string]string{
		"ghcr.io/reliant-labs/app":                    "admin",
		"us-central1-docker.pkg.dev/proj/repo/daemon": "workspace-proxy",
	} {
		if byRepo[repo] != workload {
			t.Errorf("destination %q declared by %q, want %q", repo, byRepo[repo], workload)
		}
	}

	// login derives BOTH hosts, sorted, with no host argument anywhere.
	hosts := pushPlan{destinations: dests}.hosts()
	if strings.Join(hosts, ",") != "ghcr.io,us-central1-docker.pkg.dev" {
		t.Errorf("hosts = %v, want both registries the images name", hosts)
	}
}

// TestImageTagSet_TagsTheDeclaredRepositoryVerbatim: a build's tags are the
// workload's DECLARED repository plus tags. The reference is never assembled
// from a registry and a name, so there is nothing for forge to prefix, default
// or get wrong — including a k3d-local repository, which `forge env deploy`
// then finds because both sides read the one declaration.
func TestImageTagSet_TagsTheDeclaredRepositoryVerbatim(t *testing.T) {
	if got := imageTagSet("ghcr.io/acme/web", "t1", false, false).local; strings.Join(got, ",") != "ghcr.io/acme/web:latest,ghcr.io/acme/web:t1" {
		t.Errorf("local tags = %v, want the declared repository with :latest and :t1", got)
	}
	// A bare name has no host, so nothing is invented for it: it is tagged
	// locally exactly as declared, and there is no push destination.
	if got := imageTagSet("web", "t1", false, false).local; strings.Join(got, ",") != "web:latest,web:t1" {
		t.Errorf("bare image: local tags = %v, want the bare name", got)
	}
	// A localhost repository also gets its registry.localhost mirror tag —
	// tagged, never pushed (the host cannot resolve that name).
	got := imageTagSet("localhost:5050/web", "t1", true, false)
	if !containsStr(got.local, "registry.localhost:5050/web:t1") {
		t.Errorf("k3d mirror tag missing from local tags: %v", got.local)
	}
	for _, ref := range got.push {
		if strings.HasPrefix(ref, "registry.localhost:") {
			t.Errorf("pushed %q — the host cannot resolve registry.localhost", ref)
		}
	}
}

// TestBuildCmd_LocalBuildTagsTheDeclaredReference: `forge build prod --docker`
// (no push) tags the image under the reference its workload declares — so a
// later --push of the same build is a push, not a rebuild — and pushes nothing.
func TestBuildCmd_LocalBuildTagsTheDeclaredReference(t *testing.T) {
	planProject(t, declaredRegistryFixture)
	out, err := runBuildCommand(t, "prod", "--docker", "--plan", "--no-generate", "--tag", "t1")
	if err != nil {
		t.Fatalf("forge build prod --docker --plan: %v", err)
	}
	if strings.Contains(out, "push ") {
		t.Errorf("a build without --push must push nothing; plan output:\n%s", out)
	}
	if !strings.Contains(out, `Image:    registry.example/prod/pt (declared by workload "pt"; tagged locally, not pushed)`) {
		t.Errorf("the build header should name the declared reference the image is tagged under; plan output:\n%s", out)
	}
}
