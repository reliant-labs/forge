//go:build cgo

package kclrender_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/kclrender"
)

// TestRunVendorsAbsentForgeModule is the F4 regression through the seam
// every forge render takes. A minimal project whose kcl.mod points at
// ../../.forge-kcl — with no `forge generate` ever run — failed with kpm's
// raw CannotFindModule and a `kcl mod add forge` suggestion that is wrong
// for forge. The render must now vendor the module on demand and succeed.
func TestRunVendorsAbsentForgeModule(t *testing.T) {
	dir := t.TempDir()
	envDir := filepath.Join(dir, "deploy", "kcl", "dev")
	files := map[string]string{
		"deploy/kcl/kcl.mod":    "[package]\nname = \"vendor_probe\"\n\n[dependencies]\nforge = { path = \"../../.forge-kcl\" }\n",
		"deploy/kcl/dev/main.k": "import forge\n\nok = forge.Bundle is not None\n",
	}
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	out, err := kclrender.Run(dir, envDir, []string{"env=dev"})
	if err != nil {
		t.Fatalf("render of a never-generated project must vendor the forge module on demand; got: %v", err)
	}
	if !strings.Contains(string(out), `"ok": true`) && !strings.Contains(string(out), `"ok":true`) {
		t.Errorf("render output does not show the forge module resolved:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".forge-kcl", "kcl.mod")); err != nil {
		t.Errorf(".forge-kcl/ was not materialized: %v", err)
	}
}
