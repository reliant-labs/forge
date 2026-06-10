package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
	forgev1 "github.com/reliant-labs/forge/internal/gen/forge/v1"
)

// TestDescriptorParsesErrors verifies that a (forge.v1.method).errors list
// is plumbed onto codegen.Method.Errors. This is the proto-side half of
// the typed-error-contract feature: the LLM/handler-author reads the
// generated methodErrors map; the data has to actually come through the
// descriptor extraction or that map is always empty.
func TestDescriptorParsesErrors(t *testing.T) {
	method := codegen.Method{Name: "GetWidget"}
	mo := &forgev1.MethodOptions{
		Errors: []string{"NotFound", "PermissionDenied"},
	}

	applyMethodOptions(&method, mo)

	want := []string{"NotFound", "PermissionDenied"}
	if !reflect.DeepEqual(method.Errors, want) {
		t.Errorf("Errors = %v, want %v", method.Errors, want)
	}
}

// Regression: methods with no declared errors must yield a nil/empty
// slice (not panic, not propagate a stale value). The template omits
// methods with no errors from the methodErrors map; that hinges on
// len(method.Errors) == 0 here.
func TestDescriptorParsesErrors_Unset(t *testing.T) {
	method := codegen.Method{Name: "GetWidget"}
	mo := &forgev1.MethodOptions{} // no errors field set

	applyMethodOptions(&method, mo)

	if len(method.Errors) != 0 {
		t.Errorf("Errors = %v, want empty", method.Errors)
	}
}

// Regression: applyMethodOptions must coexist with the existing
// AuthRequired plumbing — adding the new errors field should not break
// the optional-bool auth_required round-trip.
func TestDescriptorParsesErrors_PreservesAuthRequired(t *testing.T) {
	method := codegen.Method{Name: "GetWidget", AuthRequired: true}
	authOff := false
	mo := &forgev1.MethodOptions{
		AuthRequired: &authOff,
		Errors:       []string{"NotFound"},
	}

	applyMethodOptions(&method, mo)

	if method.AuthRequired {
		t.Errorf("AuthRequired = true, want false (explicit opt-out)")
	}
	if len(method.Errors) != 1 || method.Errors[0] != "NotFound" {
		t.Errorf("Errors = %v, want [NotFound]", method.Errors)
	}
}

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
