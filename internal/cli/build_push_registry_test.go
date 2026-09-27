package cli

import (
	"context"
	"io"
	"strings"
	"testing"
)

// The defect these tests pin: `forge build <env> --push` demanded the registry
// as the flag's argument even when the env's KCL already declares it
// (cluster_target.registry / forge.K8sCluster.registry) — the value `forge env
// up` and `forge env deploy` read. So every CI workflow restated the registry
// as `--push "$REGISTRY"`, a second source of truth that drifts from the KCL
// the deploy actually pulls from.

// declaredRegistryFixture is a cluster env whose ClusterTarget declares the
// registry. The service block carries it too, exactly as render.k projects the
// target onto each workload's K8sCluster.
const declaredRegistryFixture = `{
  "cluster_target":{"cluster":"c","namespace":"n","registry":"registry.example/prod"},
  "services":[
    {"name":"pt","image":"pt","deploy":{"type":"cluster","cluster":"c","namespace":"n","registry":"registry.example/prod"},
     "build":{"type":"go","cmd":"./cmd/pt","output_name":"pt"}}
  ]}`

// runBuildCommand drives the REAL cobra command — flag parsing included, which
// is where the defect lived — and returns what it printed. --plan keeps it
// from building or pushing anything.
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

// TestBuildCmd_BarePushResolvesTheEnvDeclaredRegistry is the regression:
// `forge build prod --push` with no value pushes to the registry prod's KCL
// declares. Before the fix, pflag refused the bare flag ("flag needs an
// argument: --push"), so the only way to push was to restate the registry.
func TestBuildCmd_BarePushResolvesTheEnvDeclaredRegistry(t *testing.T) {
	planProject(t, declaredRegistryFixture)

	out, err := runBuildCommand(t, "prod", "--plan", "--no-generate", "--tag", "t1", "--push")
	if err != nil {
		t.Fatalf("forge build prod --push (env declares its registry): want the declared registry, got error: %v", err)
	}
	if !strings.Contains(out, "push registry.example/prod/pt:t1") {
		t.Errorf("bare --push should push to the env-declared registry registry.example/prod; plan output:\n%s", out)
	}
	if !strings.Contains(out, "Push:     registry.example/prod (declared in deploy/kcl/prod/main.k)") {
		t.Errorf("the build header should say where the registry came from; plan output:\n%s", out)
	}
}

// TestBuildCmd_BarePushBeforeAnotherFlag: under the old StringVar, a --push
// followed by another flag SWALLOWED it — `--push --tag t1` pushed to a
// registry literally named "--tag". A bare --push must leave the next flag
// alone.
func TestBuildCmd_BarePushBeforeAnotherFlag(t *testing.T) {
	planProject(t, declaredRegistryFixture)

	out, err := runBuildCommand(t, "prod", "--push", "--plan", "--no-generate", "--tag", "t1")
	if err != nil {
		t.Fatalf("forge build prod --push --plan ...: %v", err)
	}
	if !strings.Contains(out, "push registry.example/prod/pt:t1") {
		t.Errorf("bare --push before another flag should still resolve the declared registry; plan output:\n%s", out)
	}
}

// TestBuildCmd_ExplicitPushOverridesTheDeclaredRegistry pins precedence step
// 1: an explicit registry wins over the env's declaration, in both spellings,
// and warns — `forge env deploy` pulls from the DECLARED registry, so an
// override that silently disagrees ships an image the deploy never reads.
func TestBuildCmd_ExplicitPushOverridesTheDeclaredRegistry(t *testing.T) {
	for name, args := range map[string][]string{
		"space":  {"prod", "--push", "override.example/team", "--plan", "--no-generate", "--tag", "t1"},
		"equals": {"prod", "--push=override.example/team", "--plan", "--no-generate", "--tag", "t1"},
		"first":  {"--push", "override.example/team", "prod", "--plan", "--no-generate", "--tag", "t1"},
	} {
		t.Run(name, func(t *testing.T) {
			planProject(t, declaredRegistryFixture)
			out, err := runBuildCommand(t, args...)
			if err != nil {
				t.Fatalf("forge build %s: %v", strings.Join(args, " "), err)
			}
			if !strings.Contains(out, "push override.example/team/pt:t1") {
				t.Errorf("explicit --push should win over the declared registry; plan output:\n%s", out)
			}
			if strings.Contains(out, "push registry.example/prod/") {
				t.Errorf("explicit --push must not ALSO push to the declared registry; plan output:\n%s", out)
			}
			if !strings.Contains(out, "Warning: --push override.example/team overrides the registry deploy/kcl/prod/main.k declares (registry.example/prod)") {
				t.Errorf("an override that differs from the declaration should warn; plan output:\n%s", out)
			}
		})
	}
}

// TestBuildCmd_ExplicitPushMatchingTheDeclarationDoesNotWarn: restating the
// declared registry (the pre-fix CI spelling) is not an override.
func TestBuildCmd_ExplicitPushMatchingTheDeclarationDoesNotWarn(t *testing.T) {
	planProject(t, declaredRegistryFixture)
	out, err := runBuildCommand(t, "prod", "--push", "registry.example/prod/", "--plan", "--no-generate", "--tag", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Warning: --push") {
		t.Errorf("--push equal to the declared registry must not warn; plan output:\n%s", out)
	}
}

// TestBuildCmd_BarePushUndeclaredRegistryFails: a cluster env that declares
// no registry fails a bare --push with a runbook naming the file and the
// field to set — never a silent local-only build.
func TestBuildCmd_BarePushUndeclaredRegistryFails(t *testing.T) {
	planProject(t, `{"services":[
	  {"name":"pt","image":"pt","deploy":{"type":"cluster","cluster":"c","namespace":"n"},
	   "build":{"type":"go","cmd":"./cmd/pt","output_name":"pt"}}
	]}`)

	_, err := runBuildCommand(t, "prod", "--push", "--plan", "--no-generate", "--tag", "t1")
	if err == nil {
		t.Fatal("bare --push against an env that declares no registry: want an error, got nil")
	}
	for _, want := range []string{"deploy/kcl/prod/main.k", "registry", "forge.ClusterTarget", "forge build prod --push <registry>"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("runbook error should name %q; got: %v", want, err)
		}
	}
}

// TestBuildCmd_BarePushHostedEnvFails: a HOSTED env (Bundle declares
// forge.ControlPlane) declares no registry in its KCL, and until the platform
// default lands (platformDefaultRegistry) forge must not invent one. A bare
// --push refuses and asks for the image push base explicitly.
func TestBuildCmd_BarePushHostedEnvFails(t *testing.T) {
	planProject(t, `{
	  "control_plane":{"type":"control_plane","endpoint":"http://127.0.0.1:1","token_env":"FORGE_CONTROL_PLANE_TOKEN"},
	  "services":[{"name":"api","image":"pt","deploy":{"type":"simple-backend","spec":{"image":"pt"}},
	    "build":{"type":"go","cmd":"./cmd/pt","output_name":"pt"}}]}`)

	_, err := runBuildCommand(t, "prod", "--push", "--plan", "--no-generate", "--tag", "t1")
	if err == nil {
		t.Fatal("bare --push against a hosted env: want an error, got nil")
	}
	for _, want := range []string{"hosted", "forge.ControlPlane", "forge build prod --push <image push base>"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("hosted runbook error should name %q; got: %v", want, err)
		}
	}
}

// TestBuildCmd_BarePushWithoutEnvFails: with no env there is no declaration
// to read, so a bare --push says so instead of building without pushing.
func TestBuildCmd_BarePushWithoutEnvFails(t *testing.T) {
	planProject(t, declaredRegistryFixture)
	_, err := runBuildCommand(t, "--push", "--plan", "--no-generate")
	if err == nil || !strings.Contains(err.Error(), "forge build <env> --push") {
		t.Fatalf("bare --push with no env: want a runbook naming `forge build <env> --push`, got %v", err)
	}
}

// TestResolvePushRegistry_TargetNarrowingKeepsTheDeclaredRegistry: the
// registry is an env-wide fact. A contract with no cluster_target states it
// only on its services, and `--target <frontend>` narrows those away — the
// resolver reads the FULL render, so the registry survives.
func TestResolvePushRegistry_TargetNarrowingKeepsTheDeclaredRegistry(t *testing.T) {
	full := &KCLEntities{Services: []ServiceEntity{{
		Name:   "api",
		Deploy: DeployConfigEntity{Type: "cluster", Cluster: &K8sCluster{Cluster: "c", Namespace: "n", Registry: "registry.example/prod"}},
	}}}
	if narrowed := filterEntitiesByTarget(full, []string{"web"}); k8sClusterFieldFromEntities(narrowed, "registry") != "" {
		t.Fatal("fixture precondition: narrowing to a frontend should drop the only service stating the registry")
	}
	got, err := resolvePushRegistry(context.Background(), buildOptions{env: "prod", pushDeclared: true, targets: []string{"web"}}, full)
	if err != nil || got.registry != "registry.example/prod" {
		t.Fatalf("resolvePushRegistry = (%+v, %v), want registry.example/prod", got, err)
	}
}

// TestResolvePushRegistry_NoPushIsANoOp: a build without --push resolves no
// registry and never errors, even against an env that declares none.
func TestResolvePushRegistry_NoPushIsANoOp(t *testing.T) {
	got, err := resolvePushRegistry(context.Background(), buildOptions{env: "prod"}, &KCLEntities{})
	if err != nil || got != (pushRegistryChoice{}) {
		t.Fatalf("resolvePushRegistry(no --push) = (%+v, %v), want zero, nil", got, err)
	}
}

// TestPlatformDefaultRegistry_ProvidesNothingYet pins the seam's contract
// until the Reliant-hosted registry fills it: no default, no error, for a
// hosted env and a cluster env alike. When that lands, this test changes
// with it — deliberately.
func TestPlatformDefaultRegistry_ProvidesNothingYet(t *testing.T) {
	for name, ents := range map[string]*KCLEntities{
		"hosted":  {ControlPlane: &ControlPlaneEntity{Type: "control_plane", Endpoint: "https://cp.example"}},
		"cluster": {},
		"none":    nil,
	} {
		if reg, ok, err := platformDefaultRegistry(context.Background(), "prod", ents); reg != "" || ok || err != nil {
			t.Errorf("%s: platformDefaultRegistry = (%q, %v, %v), want (\"\", false, nil)", name, reg, ok, err)
		}
	}
}

// TestSplitBuildArgs covers the positional disambiguation a bare --push
// needs: a registry written with a space arrives as a positional, and env
// names ([a-z][a-z0-9-]*) never look like one.
func TestSplitBuildArgs(t *testing.T) {
	cases := []struct {
		name              string
		args              []string
		barePush          bool
		wantEnv, wantReg  string
		wantErrSubstrings []string
	}{
		{name: "env only", args: []string{"prod"}, wantEnv: "prod"},
		{name: "nothing", args: nil},
		{name: "two positionals without bare push", args: []string{"prod", "staging"}, wantErrSubstrings: []string{"at most 1"}},
		{name: "bare push, env only", args: []string{"prod"}, barePush: true, wantEnv: "prod"},
		{name: "spaced registry after env", args: []string{"prod", "ghcr.io/acme"}, barePush: true, wantEnv: "prod", wantReg: "ghcr.io/acme"},
		{name: "spaced registry before env", args: []string{"ghcr.io/acme", "prod"}, barePush: true, wantEnv: "prod", wantReg: "ghcr.io/acme"},
		{name: "spaced registry, no env", args: []string{"localhost:5051"}, barePush: true, wantReg: "localhost:5051"},
		{name: "bare localhost", args: []string{"localhost"}, barePush: true, wantReg: "localhost"},
		{name: "two registries", args: []string{"a.io", "b.io"}, barePush: true, wantErrSubstrings: []string{"one registry"}},
		{name: "ambiguous bare names", args: []string{"prod", "acme"}, barePush: true, wantErrSubstrings: []string{"--push=<registry>"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, reg, err := splitBuildArgs(tc.args, tc.barePush)
			if len(tc.wantErrSubstrings) > 0 {
				if err == nil {
					t.Fatalf("splitBuildArgs(%v) = (%q, %q), want an error", tc.args, env, reg)
				}
				for _, s := range tc.wantErrSubstrings {
					if !strings.Contains(err.Error(), s) {
						t.Errorf("error should contain %q, got: %v", s, err)
					}
				}
				return
			}
			if err != nil || env != tc.wantEnv || reg != tc.wantReg {
				t.Fatalf("splitBuildArgs(%v) = (%q, %q, %v), want (%q, %q, nil)", tc.args, env, reg, err, tc.wantEnv, tc.wantReg)
			}
		})
	}
}
