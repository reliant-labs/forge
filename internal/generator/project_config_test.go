package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/config"
)

// TestWriteProjectConfig_StampsForgeVersion verifies that scaffolding a
// new project records the current forge binary version under
// `forge_version` in forge.yaml. This is the foundation of the upgrade
// story — `forge upgrade` consumes the field, `forge generate` warns on
// mismatch.
func TestWriteProjectConfig_StampsForgeVersion(t *testing.T) {
	tmp := t.TempDir()

	g := NewProjectGenerator("test-stamp", tmp, "example.com/test-stamp")
	g.ServiceName = "api"
	if err := g.writeProjectConfig(); err != nil {
		t.Fatalf("writeProjectConfig: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tmp, "forge.yaml"))
	if err != nil {
		t.Fatalf("read forge.yaml: %v", err)
	}

	var cfg config.ProjectConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	want := buildinfo.Version()
	if cfg.ForgeVersion != want {
		t.Errorf("ForgeVersion = %q, want %q (buildinfo.Version())", cfg.ForgeVersion, want)
	}

	// Sanity check: the field is actually present in the marshaled yaml,
	// not just defaulted by the unmarshaler.
	if want != "" && want != "dev" {
		// Only assert when buildinfo reports a real release; in dev/test
		// runs the value is "dev" which marshals via omitempty rules.
		got := string(data)
		if !strings.Contains(got, "forge_version") {
			t.Errorf("forge.yaml missing forge_version key:\n%s", got)
		}
	}
}

// TestWriteProjectConfig_NonServiceKindsStillStampForgeVersion verifies
// that CLI- and library-kind projects also get a forge_version pin
// (the scaffold-time stamp is shape-agnostic).
func TestWriteProjectConfig_NonServiceKindsStillStampForgeVersion(t *testing.T) {
	for _, kind := range []string{"cli", "library"} {
		t.Run(kind, func(t *testing.T) {
			tmp := t.TempDir()
			g := NewProjectGenerator("kind-"+kind, tmp, "example.com/kind-"+kind)
			g.Kind = kind
			if err := g.writeProjectConfig(); err != nil {
				t.Fatalf("writeProjectConfig: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(tmp, "forge.yaml"))
			if err != nil {
				t.Fatalf("read forge.yaml: %v", err)
			}
			var cfg config.ProjectConfig
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if cfg.ForgeVersion != buildinfo.Version() {
				t.Errorf("ForgeVersion = %q, want %q", cfg.ForgeVersion, buildinfo.Version())
			}
		})
	}
}
