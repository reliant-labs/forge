package deploytarget

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/statefile"
)

func TestComposeDownExactCheckoutEnvAndRecordedContainers(t *testing.T) {
	dir := t.TempDir()
	record := composeOwned{Directory: dir, Checkout: dir, Env: "dev", Project: "project-dev-feature", Service: "clickstack", IDs: []string{"ours", "reused"}}
	for _, env := range []string{"dev", "other"} {
		if err := statefile.Write(composeOwnedPath(dir, env, "clickstack"), "compose ownership", record); err != nil {
			t.Fatal(err)
		}
	}
	label := func(directory, project string) string {
		b, _ := json.Marshal([]map[string]any{{"Config": map[string]any{"Labels": map[string]string{"com.docker.compose.project": project, "com.docker.compose.project.working_dir": directory, "com.docker.compose.service": "clickstack"}}}})
		return string(b)
	}
	r := &fakeRunner{outputs: map[string]string{"docker inspect ours": label(dir, record.Project), "docker inspect reused": label(filepath.Join(dir, "another"), record.Project)}}
	p := ComposeProvider{ProjectDir: dir, Runner: r}
	if err := p.StopOwned(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(r.calls, "\n")
	if !strings.Contains(joined, "docker rm -f ours") {
		t.Fatalf("owned container forgotten: %s", joined)
	}
	if strings.Contains(joined, "rm -f reused") || strings.Contains(joined, "-v") || strings.Contains(joined, "down") {
		t.Fatalf("overbroad teardown: %s", joined)
	}
	other, err := statefile.Read[composeOwned](composeOwnedPath(dir, "other", "clickstack"), "compose ownership")
	if err != nil || other == nil {
		t.Fatalf("other env record lost: %v", err)
	}
}
