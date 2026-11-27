package deploytarget

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/templates"
)

// Exercise Compose's interpretation of the real scaffold, not a hand-written
// network-inspect fixture: these are the labels Compose persists on creation.
func TestComposeScaffoldNetworkOwnershipLabels(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes docker compose config; runs in task test")
	}
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skipf("docker compose unavailable: %v", err)
	}
	dir := t.TempDir()
	content, err := templates.ProjectTemplates().Render("docker-compose.yml.tmpl", map[string]any{
		"ProjectName": "network-test", "HasFrontend": false, "NoIdentity": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(file, content, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, shared := range []bool{false, true} {
		overlay, err := (ComposeProvider{ProjectDir: dir}).composeNetworkEnv(&ComposeSpec{Shared: shared}, "dev")
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("docker", "compose", "-f", file, "config", "--format", "json")
		cmd.Env = os.Environ()
		for key, value := range overlay {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("compose config: %v\n%s", err, out)
		}
		var config struct {
			Networks map[string]struct{ Labels map[string]string }
		}
		if err := json.Unmarshal(out, &config); err != nil {
			t.Fatalf("decode compose config: %v\n%s", err, out)
		}
		labels := config.Networks["network-test-net"].Labels
		want := map[string]string{
			"io.reliant.forge.network.exclusive": overlay["FORGE_NETWORK_EXCLUSIVE"],
			"io.reliant.forge.network.checkout":  overlay["FORGE_NETWORK_CHECKOUT"],
			"io.reliant.forge.network.env":       overlay["FORGE_NETWORK_ENV"],
		}
		for key, value := range want {
			got, exists := labels[key]
			if !exists || got != value {
				t.Errorf("shared=%v: network label %s = %q (present=%v), want %q", shared, key, got, exists, value)
			}
		}
	}
}
