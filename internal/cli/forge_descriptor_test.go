package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
)

// Regression: per-invocation fragments under gen/.descriptor.d/ must be
// merged into a single forge_descriptor.json with all sections preserved
// and a deterministic ordering. This is the load-bearing fix for the
// parallel-buf-plugin race where two plugin processes overwrote each
// other's read-modify-write through the prior in-process flock.
func TestMergeDescriptorFragments_CombinesAllFragments(t *testing.T) {
	dir := t.TempDir()
	stage := filepath.Join(dir, descriptorStageDir)
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatalf("mkdir stage: %v", err)
	}

	frags := []ForgeDescriptor{
		{
			// "services" fragment — what the admin_server proto plugin
			// invocation would emit.
			Services: []codegen.ServiceDef{{Name: "AdminServerService"}},
		},
		{
			// "entities" fragment — what the proto/db plugin invocation
			// would emit.
			Entities: []codegen.EntityDef{{Name: "Workspace", TableName: "workspaces"}},
		},
		{
			// "configs" fragment — what the proto/config plugin invocation
			// would emit.
			Configs: []codegen.ConfigMessage{{Name: "Config"}},
		},
	}
	for i, f := range frags {
		data, err := json.Marshal(f)
		if err != nil {
			t.Fatalf("marshal fragment %d: %v", i, err)
		}
		// Names chosen so sort.Strings produces a known order; the merge
		// step itself is order-independent for correctness, but
		// deterministic ordering matters for repeatable codegen output.
		name := []string{"a.json", "b.json", "c.json"}[i]
		if err := os.WriteFile(filepath.Join(stage, name), data, 0o644); err != nil {
			t.Fatalf("write fragment %d: %v", i, err)
		}
	}

	if err := MergeDescriptorFragments(dir); err != nil {
		t.Fatalf("MergeDescriptorFragments: %v", err)
	}

	out, err := os.ReadFile(filepath.Join(dir, "forge_descriptor.json"))
	if err != nil {
		t.Fatalf("read merged descriptor: %v", err)
	}
	var got ForgeDescriptor
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parse merged descriptor: %v", err)
	}

	if len(got.Services) != 1 || got.Services[0].Name != "AdminServerService" {
		t.Errorf("services not merged correctly: %+v", got.Services)
	}
	if len(got.Entities) != 1 || got.Entities[0].Name != "Workspace" {
		t.Errorf("entities not merged correctly: %+v", got.Entities)
	}
	if len(got.Configs) != 1 || got.Configs[0].Name != "Config" {
		t.Errorf("configs not merged correctly: %+v", got.Configs)
	}

	// Stage dir should be cleaned up after a successful merge.
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Errorf("stage dir should be removed, stat err = %v", err)
	}
}

// MergeDescriptorFragments must be a no-op when no fragments exist
// (clean projects with no services / entities / configs).
func TestMergeDescriptorFragments_NoStageDir_NoError(t *testing.T) {
	dir := t.TempDir()
	if err := MergeDescriptorFragments(dir); err != nil {
		t.Fatalf("MergeDescriptorFragments on empty dir: %v", err)
	}
}
