package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"

	"github.com/reliant-labs/forge/pkg/seedplan"
)

// The seed-apply dev gate is FAIL-CLOSED and classifies by the runtime MODE in
// the per-env KCL config (deploy/kcl/<env>/config.k) — only "development"/"dev"
// is allowed; everything else (production, an unset mode, a missing file) is
// refused.
func TestSeedEnvClassification(t *testing.T) {
	dir := t.TempDir()
	writeConfigK := func(env, body string) {
		envDir := filepath.Join(dir, "deploy", "kcl", env)
		if err := os.MkdirAll(envDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(envDir, "config.k"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	const kHeader = "import config_gen\n\napp_config: config_gen.AppConfig = {\n"

	// dev: config.k marks development (the scaffolded shape) -> dev.
	writeConfigK("dev", kHeader+"    environment = \"development\"\n}\n")
	// prod: config.k marks production -> not dev.
	writeConfigK("prod", kHeader+"    environment = \"production\"\n}\n")
	// staging: config.k present but sets no environment (inherits the schema
	// default, production) -> not dev, fail-closed.
	writeConfigK("staging", kHeader+"}\n")

	cases := []struct {
		env  string
		want bool
	}{
		{"dev", true},
		{"prod", false},
		{"staging", false}, // mode unset -> not dev
		{"nope", false},    // missing config.k -> fail closed
	}
	for _, tc := range cases {
		if got := seedEnvIsDevIn(dir, tc.env); got != tc.want {
			t.Errorf("seedEnvIsDevIn(%q) = %v, want %v", tc.env, got, tc.want)
		}
	}
}

// The database.seed block flows onto seedplan.Config.
func TestSeedConfigFlows(t *testing.T) {
	load := func(t *testing.T, forgeYAML string) seedplan.Config {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "forge.yaml")
		if err := os.WriteFile(path, []byte(forgeYAML), 0o644); err != nil {
			t.Fatal(err)
		}
		store, err := loadProjectStoreFrom(path)
		if err != nil {
			t.Fatalf("loadProjectStoreFrom: %v", err)
		}
		return seedConfigFromStore(store)
	}

	const base = "name: app\nmodule_path: example.com/app\n"
	got := load(t, base+"database:\n  seed:\n    rows: 42\n    salt: 7\n")
	if got.Rows != 42 {
		t.Errorf("database.seed.rows: Rows = %d, want 42", got.Rows)
	}
	if got.Salt != 7 {
		t.Errorf("database.seed.salt: Salt = %d, want 7", got.Salt)
	}
}

// There is deliberately NO override flag on apply/reset — an override is
// exactly the conventional hole the structural gate exists to close.
func TestSeedApplyHasNoOverrideFlag(t *testing.T) {
	cmd := newDBSeedApplyCommand()
	banned := []string{"allow-nondev", "allow-non-dev", "force", "prod", "unsafe", "yes"}
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		for _, b := range banned {
			if f.Name == b {
				t.Errorf("apply must not expose an override flag, found --%s", f.Name)
			}
		}
	})
}
