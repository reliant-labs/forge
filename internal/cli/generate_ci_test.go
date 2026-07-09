package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

// Proto-first projects declare services only in proto/ (no forge.yaml
// components). CI workflow data must derive HasServices from proto truth —
// the same shape the generate pipeline sees — or the scaffolds silently
// drop buf lint steps and skip proto-breaking.yml.
func TestBuildCIWorkflowData_HasServicesProtoFirst(t *testing.T) {
	root := t.TempDir()
	protoDir := filepath.Join(root, "proto", "services", "widget", "v1")
	if err := os.MkdirAll(protoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	proto := "syntax = \"proto3\";\npackage services.widget.v1;\nservice WidgetService {}\n"
	if err := os.WriteFile(filepath.Join(protoDir, "widget.proto"), []byte(proto), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.ProjectConfig{Name: "proto-first"} // no components declared
	data := buildCIWorkflowData(cfg, root)
	if !data.HasServices {
		t.Fatal("proto-first project (proto service declaration, no forge.yaml components) must set HasServices")
	}
}

func TestBuildCIWorkflowData_NoServicesAnywhere(t *testing.T) {
	cfg := &config.ProjectConfig{Name: "empty"}
	data := buildCIWorkflowData(cfg, t.TempDir())
	if data.HasServices {
		t.Fatal("project with no components and no proto services must leave HasServices false")
	}
}
