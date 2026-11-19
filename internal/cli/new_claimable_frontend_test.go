package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.yaml.in/yaml/v3"
)

// Parse the actual CLI flags and write the real scaffold. The bootstrap and
// dependency installers are outside this test's question: whether an agent's
// default anonymous app contains a sign-in page before it authors any UI.
func TestNewClaimableFrontendDefaultsToPublicScaffold(t *testing.T) {
	if testing.Short() {
		t.Skip("writes the service/frontend scaffold and runs git and tool probes")
	}
	requirePOSIXFake(t, "node and go")
	forgeRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	previous := bootstrapGenerate
	bootstrapGenerate = func(string, bool) error { return nil }
	t.Cleanup(func() { bootstrapGenerate = previous })
	// An unsupported Node version stops installation before any network
	// request; tidy also fails locally rather than downloading dependencies.
	bin := t.TempDir()
	for tool, body := range map[string]string{
		"node": "#!/bin/sh\necho v1.0.0\n",
		"go":   "#!/bin/sh\nexit 1\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GOPROXY", "off")
	t.Setenv("GIT_AUTHOR_NAME", "forge-test")
	t.Setenv("GIT_AUTHOR_EMAIL", "forge-test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "forge-test")
	t.Setenv("GIT_COMMITTER_EMAIL", "forge-test@example.com")
	parent := t.TempDir()
	cmd := newNewCmd()
	cmd.SetArgs([]string{"counter", "--path", parent, "--mod", "example.com/counter",
		"--service", "counter", "--frontend", "web", "--claimable", "--skip-tools",
		"--link-forge", "--forge-root", forgeRoot})
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("project new --claimable: %v", err)
	}
	root := filepath.Join(parent, "counter")
	for _, rel := range []string{
		"deploy/kcl/identity.k", "frontends/web/src/app/auth",
		"frontends/web/src/components/auth-ui.tsx", "frontends/web/src/components/sign-in-methods.tsx",
	} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("default anonymous scaffold contains authentication UI or configuration: %s (stat: %v)", rel, err)
		}
	}
	providers, err := os.ReadFile(filepath.Join(root, "frontends/web/src/app/providers.tsx"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(providers), "<RouteGuard") {
		t.Error("default anonymous frontend mounts a sign-in guard")
	}
	raw, err := os.ReadFile(filepath.Join(root, "forge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		InstanceID string `yaml:"project_instance_id"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if id, err := uuid.Parse(cfg.InstanceID); err != nil || id == uuid.Nil {
		t.Fatalf("fresh anonymous project has no independent deployment identity: %q, %v", cfg.InstanceID, err)
	}
}
