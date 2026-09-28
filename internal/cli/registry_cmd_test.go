package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The registry a CI job logs in to and scans/signs is the one the env's KCL
// declares — never a value the workflow names. These pin `forge registry
// login <env>` and `forge registry ref <env>`, the two commands that let a
// workflow work from the declaration alone.

// stubDockerLogin replaces the docker-login seam for one test and records
// the call.
type loginCall struct{ host, username, password string }

func stubDockerLogin(t *testing.T) *[]loginCall {
	t.Helper()
	var calls []loginCall
	prev := dockerLogin
	dockerLogin = func(_ context.Context, host, username string, password io.Reader) error {
		b, _ := io.ReadAll(password)
		calls = append(calls, loginCall{host, username, string(b)})
		return nil
	}
	t.Cleanup(func() { dockerLogin = prev })
	return &calls
}

func runRegistryCommand(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	var runErr error
	out := captureStdout(t, func() {
		cmd := newRegistryCmd()
		cmd.SetArgs(args)
		cmd.SetIn(strings.NewReader(stdin))
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		runErr = cmd.ExecuteContext(context.Background())
	})
	return out, runErr
}

// TestRegistryLogin_LogsInToTheDeclaredRegistryHost: the host comes from the
// env's KCL (registry.example/prod → registry.example); the credential from
// stdin and --username.
func TestRegistryLogin_LogsInToTheDeclaredRegistryHost(t *testing.T) {
	planProject(t, declaredRegistryFixture)
	calls := stubDockerLogin(t)

	if _, err := runRegistryCommand(t, "s3cret\n", "login", "prod", "--username", "ci-bot", "--password-stdin"); err != nil {
		t.Fatalf("forge registry login prod: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("docker login calls = %v, want exactly one", *calls)
	}
	got := (*calls)[0]
	if got.host != "registry.example" || got.username != "ci-bot" || strings.TrimSpace(got.password) != "s3cret" {
		t.Errorf("docker login = %+v, want host registry.example, user ci-bot, the stdin password", got)
	}
}

// TestRegistryLogin_HostedEnvUsesTheControlPlaneRegistry: a hosted env's
// registry is declared on forge.ControlPlane.
func TestRegistryLogin_HostedEnvUsesTheControlPlaneRegistry(t *testing.T) {
	planProject(t, hostedPushFixture("us-docker.pkg.dev/acme/org-7"))
	calls := stubDockerLogin(t)
	if _, err := runRegistryCommand(t, "tok", "login", "prod", "--username", "oauth2accesstoken", "--password-stdin"); err != nil {
		t.Fatalf("forge registry login prod (hosted): %v", err)
	}
	if len(*calls) != 1 || (*calls)[0].host != "us-docker.pkg.dev" {
		t.Errorf("docker login calls = %+v, want one to us-docker.pkg.dev", *calls)
	}
}

// TestRegistryLogin_TakesNoRegistry: the command has no flag or argument that
// could name a registry or host. The only positional is the env.
func TestRegistryLogin_TakesNoRegistry(t *testing.T) {
	planProject(t, declaredRegistryFixture)
	calls := stubDockerLogin(t)
	for name, args := range map[string][]string{
		"second positional": {"login", "prod", "ghcr.io", "--username", "u", "--password-stdin"},
		"registry flag":     {"login", "prod", "--registry", "ghcr.io", "--username", "u", "--password-stdin"},
		"host flag":         {"login", "prod", "--host", "ghcr.io", "--username", "u", "--password-stdin"},
		"registry as env":   {"login", "ghcr.io/acme", "--username", "u", "--password-stdin"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := runRegistryCommand(t, "x", args...); err == nil {
				t.Errorf("forge registry %s: want a usage error — the registry comes from the env's KCL only", strings.Join(args, " "))
			}
		})
	}
	if len(*calls) != 0 {
		t.Errorf("a refused invocation must not log in; calls = %+v", *calls)
	}
	cmd := newRegistryLoginCmd()
	for _, name := range []string{"registry", "host", "server"} {
		if cmd.Flags().Lookup(name) != nil {
			t.Errorf("forge registry login has a --%s flag; the registry is declared in KCL, never passed", name)
		}
	}
}

// TestRegistryLogin_UndeclaredRegistryFails names the file and field.
func TestRegistryLogin_UndeclaredRegistryFails(t *testing.T) {
	planProject(t, hostedPushFixture(""))
	calls := stubDockerLogin(t)
	_, err := runRegistryCommand(t, "x", "login", "prod", "--username", "u", "--password-stdin")
	if err == nil || !strings.Contains(err.Error(), "deploy/kcl/prod/main.k") || !strings.Contains(err.Error(), "forge.ControlPlane") {
		t.Fatalf("login against an env that declares no registry: want the runbook, got %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("no registry declared: must not log in; calls = %+v", *calls)
	}
}

// TestRegistryLogin_LocalRegistryNeedsNoLogin: a k3d-local registry takes no
// credentials, so login is a no-op (and says so) rather than a failure a CI
// job would have to special-case.
func TestRegistryLogin_LocalRegistryNeedsNoLogin(t *testing.T) {
	planProject(t, strings.ReplaceAll(declaredRegistryFixture, "registry.example/prod", "localhost:5050"))
	calls := stubDockerLogin(t)
	out, err := runRegistryCommand(t, "", "login", "prod", "--username", "u", "--password-stdin")
	if err != nil {
		t.Fatalf("login against a local registry: %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("a local registry needs no login; calls = %+v", *calls)
	}
	if !strings.Contains(out, "localhost:5050") {
		t.Errorf("the no-op should name the registry it skipped; output:\n%s", out)
	}
}

// TestRegistryHost splits a registry reference to the host docker logs in to.
func TestRegistryHost(t *testing.T) {
	for in, want := range map[string]string{
		"ghcr.io/acme":                        "ghcr.io",
		"us-central1-docker.pkg.dev/p/r":      "us-central1-docker.pkg.dev",
		"localhost:5050":                      "localhost:5050",
		"123.dkr.ecr.us-east-1.amazonaws.com": "123.dkr.ecr.us-east-1.amazonaws.com",
		"acme":                                "docker.io",
	} {
		if got := registryHost(in); got != want {
			t.Errorf("registryHost(%q) = %q, want %q", in, got, want)
		}
	}
}

const refDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// TestRegistryRef_PrintsThePushedDigestRef: the ref a scan/sign step needs is
// what `forge build <env> --push` recorded — the registry the env declares,
// the image, the digest — never a YAML literal.
func TestRegistryRef_PrintsThePushedDigestRef(t *testing.T) {
	dir := planProject(t, declaredRegistryFixture)
	if err := WriteBuildState(dir, "prod", BuildState{Image: "pt", Tag: "t1", Registry: "registry.example/prod", Pushed: true, PushedAt: nowRFC3339(), Digest: refDigest}); err != nil {
		t.Fatal(err)
	}
	out, err := runRegistryCommand(t, "", "ref", "prod")
	if err != nil {
		t.Fatalf("forge registry ref prod: %v", err)
	}
	if strings.TrimSpace(out) != "registry.example/prod/pt@"+refDigest {
		t.Errorf("ref = %q, want registry.example/prod/pt@%s", strings.TrimSpace(out), refDigest)
	}
}

// TestRegistryRef_WritesGitHubOutput: --github-output appends ref/image/digest
// to the file GitHub Actions names in $GITHUB_OUTPUT (an output FILE, not a
// registry pointer), so later steps read them as step outputs.
func TestRegistryRef_WritesGitHubOutput(t *testing.T) {
	dir := planProject(t, declaredRegistryFixture)
	if err := WriteBuildState(dir, "prod", BuildState{Image: "pt", Tag: "t1", Registry: "registry.example/prod", Pushed: true, PushedAt: nowRFC3339(), Digest: refDigest}); err != nil {
		t.Fatal(err)
	}
	outFile := filepath.Join(t.TempDir(), "gh_output")
	t.Setenv("GITHUB_OUTPUT", outFile)
	if _, err := runRegistryCommand(t, "", "ref", "prod", "--github-output"); err != nil {
		t.Fatalf("forge registry ref prod --github-output: %v", err)
	}
	b, _ := os.ReadFile(outFile)
	for _, want := range []string{
		"ref=registry.example/prod/pt@" + refDigest,
		"image=registry.example/prod/pt",
		"digest=" + refDigest,
	} {
		if !strings.Contains(string(b), want+"\n") {
			t.Errorf("$GITHUB_OUTPUT lacks %q:\n%s", want, b)
		}
	}
}

// TestRegistryRef_UnpushedBuildFails: a ref needs a digest, which only a push
// records.
func TestRegistryRef_UnpushedBuildFails(t *testing.T) {
	dir := planProject(t, declaredRegistryFixture)
	if err := WriteBuildState(dir, "prod", BuildState{Image: "pt", Tag: "t1", PushedAt: nowRFC3339()}); err != nil {
		t.Fatal(err)
	}
	_, err := runRegistryCommand(t, "", "ref", "prod")
	if err == nil || !strings.Contains(err.Error(), "forge build prod --push") {
		t.Fatalf("ref of an unpushed build: want a runbook naming `forge build prod --push`, got %v", err)
	}
}

// TestRegistryRef_NamedImage reads a per-image state (a frontend, a DockerBuild).
func TestRegistryRef_NamedImage(t *testing.T) {
	planProject(t, declaredRegistryFixture)
	persistImageBuildStates(buildOptions{env: "prod", pushRegistry: "registry.example/prod"}, "t1",
		[]buildResult{{kind: "docker", image: "web", digest: refDigest}})
	out, err := runRegistryCommand(t, "", "ref", "prod", "--image", "web")
	if err != nil {
		t.Fatalf("forge registry ref prod --image web: %v", err)
	}
	if strings.TrimSpace(out) != "registry.example/prod/web@"+refDigest {
		t.Errorf("ref = %q, want registry.example/prod/web@%s", strings.TrimSpace(out), refDigest)
	}
}
