package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/atomicfile"
	"go.yaml.in/yaml/v3"
)

func TestForcedInPlaceScaffoldPreservesProjectInstance(t *testing.T) {
	t.Parallel()
	for _, prior := range []string{"", "ee7d1ea9-c6a9-4bfe-8846-cf976cc6ae40"} {
		t.Run(prior, func(t *testing.T) {
			staging, target := t.TempDir(), t.TempDir()
			if err := atomicfile.WriteFile(filepath.Join(staging, "forge.yaml"), []byte("name: new-scaffold\nproject_instance_id: 8eb52828-0e0a-420b-9cce-33624584c64e\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			doc := "name: original\n"
			if prior != "" {
				doc += "project_instance_id: " + prior + "\n"
			}
			if err := atomicfile.WriteFile(filepath.Join(target, "forge.yaml"), []byte(doc), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := mergeScaffoldInto(staging, target, true); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(target, "forge.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]string
			if err := yaml.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if got["project_instance_id"] != prior || got["name"] != "new-scaffold" {
				t.Fatalf("force must update scaffold but keep deployment identity: %v", got)
			}
		})
	}
}
