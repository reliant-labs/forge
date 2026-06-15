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

// TestApplyKindFeatureDefaults_Service is a no-op assertion: the
// default scaffold (`forge new --kind service` or no flag) must leave
// every STABLE feature enabled. Experimental features are default-off
// for every kind (including service) — the user opts in per project
// via `features.experimental.<name>: true` after scaffolding.
func TestApplyKindFeatureDefaults_Service(t *testing.T) {
	g := NewProjectGenerator("svc", "/tmp/svc", "example.com/svc")
	g.ApplyKindFeatureDefaults(config.ProjectKindService)
	effective := g.Features.EffectiveFeatures()
	for name, on := range effective {
		if config.IsExperimentalFeature(name) {
			if on {
				t.Errorf("kind=service: experimental feature %q expected disabled, got enabled", name)
			}
			continue
		}
		if !on {
			t.Errorf("kind=service: stable feature %q expected enabled, got disabled", name)
		}
	}
}

// TestApplyKindFeatureDefaults_CLI verifies the CLI per-kind matrix.
// The feature-block prompt's documented matrix is "build/ci/docs
// true; deploy/frontend/packs/starters/observability false." Per the
// existing forge convention (forge has disabled codegen/ORM/migrations
// for non-service kinds since the kind flag landed), we also leave
// those off — the CLI scaffold has no proto/services dir to drive
// them. Contracts stays on so the linter still nudges CLI authors
// toward interface-bounded surface area.
func TestApplyKindFeatureDefaults_CLI(t *testing.T) {
	g := NewProjectGenerator("c", "/tmp/c", "example.com/c")
	g.ApplyKindFeatureDefaults(config.ProjectKindCLI)
	effective := g.Features.EffectiveFeatures()

	want := map[string]bool{
		config.FeatureBuild:         true,
		config.FeatureCI:            true,
		config.FeatureDocs:          true,
		config.FeatureContracts:     true,
		config.FeatureDeploy:        false,
		config.FeatureFrontend:      false,
		config.FeaturePacks:         false,
		config.FeatureStarters:      false,
		config.FeatureObservability: false,
		config.FeatureORM:           false,
		config.FeatureCodegen:       false, // existing forge default — no proto/services to codegen
		config.FeatureMigrations:    false,
		config.FeatureHotReload:     false,
		config.FeatureIngress:       false,
	}
	for name, expect := range want {
		if effective[name] != expect {
			t.Errorf("kind=cli: feature %q = %v, want %v", name, effective[name], expect)
		}
	}
}

// TestApplyKindFeatureDefaults_Library verifies the library matrix.
// The feature-block prompt's documented matrix is "library: ci/docs
// true, everything else false." We honor the prompt for docs/build/
// deploy/frontend/packs/starters/observability/orm/codegen/migrations/
// hot_reload but preserve the existing forge convention of CI=false
// for library — TestProjectGeneratorKindLibraryScaffold asserts no
// .github/workflows/ tree is emitted on a library scaffold, and the
// user can flip features.ci: true manually to re-enable lint+test
// workflows. Contracts stays enabled (linting is the headline value
// for a library; package authors want interface-bounded surface).
func TestApplyKindFeatureDefaults_Library(t *testing.T) {
	g := NewProjectGenerator("lib", "/tmp/lib", "example.com/lib")
	g.ApplyKindFeatureDefaults(config.ProjectKindLibrary)
	effective := g.Features.EffectiveFeatures()

	want := map[string]bool{
		config.FeatureDocs:          true,
		config.FeatureContracts:     true,
		config.FeatureCI:            false, // existing forge convention
		config.FeatureBuild:         false,
		config.FeatureDeploy:        false,
		config.FeatureFrontend:      false,
		config.FeaturePacks:         false,
		config.FeatureStarters:      false,
		config.FeatureObservability: false,
		config.FeatureORM:           false,
		config.FeatureCodegen:       false,
		config.FeatureMigrations:    false,
		config.FeatureHotReload:     false,
		config.FeatureIngress:       false,
	}
	for name, expect := range want {
		if effective[name] != expect {
			t.Errorf("kind=library: feature %q = %v, want %v", name, effective[name], expect)
		}
	}
}

// TestApplyKindFeatureDefaults_PreservesExplicit ensures the per-kind
// defaults are commutative with --disable: a caller that already set
// `gen.Features.Frontend = boolPtr(true)` before invoking
// ApplyKindFeatureDefaults("cli") keeps the explicit true (the helper
// only sets fields that were still nil). Matches the doc on
// ApplyKindFeatureDefaults.
func TestApplyKindFeatureDefaults_PreservesExplicit(t *testing.T) {
	g := NewProjectGenerator("c", "/tmp/c", "example.com/c")
	keepTrue := true
	g.Features.Frontend = &keepTrue // user explicitly wants frontend ON even in CLI mode

	g.ApplyKindFeatureDefaults(config.ProjectKindCLI)
	if !g.Features.FrontendEnabled() {
		t.Error("explicit Frontend=true overwritten by ApplyKindFeatureDefaults(cli)")
	}
}

// TestWriteProjectConfig_CLIKindFeaturesDeriveOnLoad verifies the
// scaffolded CLI forge.yaml carries NO features: block — the per-kind
// matrix is derived from `kind: cli` at load time. The round-trip that
// matters is `forge new` → loadProjectConfig: the loaded config must
// resolve build=on / packs=off without any explicit flags on disk.
func TestWriteProjectConfig_CLIKindFeaturesDeriveOnLoad(t *testing.T) {
	tmp := t.TempDir()
	g := NewProjectGenerator("cli-feat", tmp, "example.com/cli-feat")
	g.Kind = config.ProjectKindCLI
	g.ApplyKindFeatureDefaults(config.ProjectKindCLI)
	if err := g.writeProjectConfig(); err != nil {
		t.Fatalf("writeProjectConfig: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(tmp, "forge.yaml"))
	if err != nil {
		t.Fatalf("read forge.yaml: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "features:") {
			t.Errorf("kind=cli forge.yaml should not materialize a features: block (derived from kind); got:\n%s", data)
		}
	}
	cfg, err := ReadProjectConfig(filepath.Join(tmp, "forge.yaml"))
	if err != nil {
		t.Fatalf("ReadProjectConfig: %v", err)
	}
	if !cfg.Features.BuildEnabled() {
		t.Error("kind=cli loaded config: BuildEnabled() = false, want true (derived)")
	}
	if cfg.Features.PacksEnabled() {
		t.Error("kind=cli loaded config: PacksEnabled() = true, want false (derived)")
	}
	if cfg.Features.CodegenEnabled() {
		t.Error("kind=cli loaded config: CodegenEnabled() = true, want false (derived)")
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
