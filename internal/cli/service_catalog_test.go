package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/pkg/servicecatalog"
)

func TestBundleCollectsGeneratedServiceCatalog(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "gen"), 0755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(servicecatalog.Catalog{Schema: servicecatalog.Schema, Module: "example.com/shop"})
	file := filepath.Join(dir, servicecatalog.GeneratedPath)
	if err := os.WriteFile(file, raw, 0644); err != nil {
		t.Fatal(err)
	}
	out, err := bundleObservabilityFiles(dir, bundleBuildInputs{StrictObservability: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range out {
		if f.Path == servicecatalog.BundlePath {
			found = true
			if string(f.Data) != string(raw) {
				t.Fatal("catalog was altered")
			}
		}
	}
	if !found {
		t.Fatal("catalog not attached to existing bundle")
	}
	if err := os.WriteFile(file, []byte(`{"schema":"bad"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := bundleObservabilityFiles(dir, bundleBuildInputs{StrictObservability: true}, nil); err == nil {
		t.Fatal("invalid schema accepted")
	}
}
