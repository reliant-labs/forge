package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
)

// TestLoadProjectConfigEnvMap_SensitiveRoutesToSecret is the end-to-end mission
// check for the two-channel config projection: a scaffolded project's dev
// config renders each field through config_projection.appConfigEnvMap — the
// EXACT source `forge run` injects into the host env and a deploy projects into
// every workload's manifest — and each field lands on the RIGHT channel.
//
//   - DATABASE_URL is `sensitive`, so it projects a SECRET REFERENCE
//     (<project>-secrets / database_url), never an inline value. This is what
//     stops the rendered Deployment from carrying a database password as a
//     literal `value:` visible to anyone with repo or namespace read.
//   - ENVIRONMENT is ordinary config, so it still projects inline.
//
// The VALUE half lives in the gitignored `.env.dev` the same generate step
// scaffolds — asserted here too, because a Secret reference with nothing
// behind it is a dev loop that cannot boot.
func TestLoadProjectConfigEnvMap_SensitiveRoutesToSecret(t *testing.T) {
	if _, err := exec.LookPath("kcl"); err != nil {
		t.Skip("kcl not on PATH; skipping config projection render test")
	}
	tmp := t.TempDir()
	g := generator.NewProjectGenerator("cfgproj", tmp, "example.com/cfgproj")
	g.Kind = config.ProjectKindService
	g.ApplyKindFeatureDefaults(config.ProjectKindService)
	if err := g.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Emit the REAL KCL-native config trio from the scaffold defaults: the
	// shared schema + projection, the dev per-env values instance, and the
	// gitignored dev secrets dotenv.
	var fields []codegen.ConfigField
	for _, m := range codegen.DefaultConfigMessages() {
		fields = append(fields, m.Fields...)
	}
	kclDirAbs := filepath.Join(tmp, "deploy", "kcl")
	if err := codegen.GenerateConfigNativeShared(fields, "cfgproj", tmp, kclDirAbs, nil); err != nil {
		t.Fatalf("GenerateConfigNativeShared: %v", err)
	}
	if _, err := codegen.GenerateConfigKScaffold(fields, "cfgproj", kclDirAbs, "dev"); err != nil {
		t.Fatalf("GenerateConfigKScaffold: %v", err)
	}
	if _, err := codegen.GenerateEnvSecretsScaffold(fields, "cfgproj", tmp, "dev"); err != nil {
		t.Fatalf("GenerateEnvSecretsScaffold: %v", err)
	}

	srcs, err := loadProjectConfigEnvMap(tmp, "dev")
	if err != nil {
		t.Fatalf("loadProjectConfigEnvMap: %v", err)
	}

	db, ok := srcs["DATABASE_URL"]
	if !ok {
		t.Fatalf("projection dropped DATABASE_URL entirely; got %#v", srcs)
	}
	if db.Value != nil {
		t.Errorf("DATABASE_URL projected an INLINE value %q — a sensitive field must route to "+
			"the Secret channel in every environment", *db.Value)
	}
	if db.FromSecret["name"] != "cfgproj-secrets" || db.FromSecret["key"] != "database_url" {
		t.Errorf("DATABASE_URL from_secret = %#v, want {name: cfgproj-secrets, key: database_url}", db.FromSecret)
	}
	if env, ok := srcs["ENVIRONMENT"]; !ok || env.Value == nil || *env.Value != "development" {
		t.Errorf("ENVIRONMENT projection = %#v, want inline \"development\"", srcs["ENVIRONMENT"])
	}

	// The reference has to point at something: the dev secret provider's
	// dotenv carries the value, keyed by env-var NAME.
	dotenv, err := os.ReadFile(filepath.Join(tmp, ".env.dev"))
	if err != nil {
		t.Fatalf("read scaffolded .env.dev: %v", err)
	}
	want := "DATABASE_URL=postgres://postgres:postgres@localhost:5434/cfgproj?sslmode=disable"
	if !strings.Contains(string(dotenv), want) {
		t.Errorf(".env.dev missing %q — the Secret reference resolves to nothing:\n%s", want, dotenv)
	}
}
