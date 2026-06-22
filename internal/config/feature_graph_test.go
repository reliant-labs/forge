package config

import (
	"strings"
	"testing"
)

// serviceComponent is a single server component, injected via the
// LoadStrict variadic to derive the project kind to "service" now that
// components (and kind) live outside forge.yaml.
func serviceComponent() []ComponentConfig {
	return []ComponentConfig{{Name: "api", Kind: "server", Path: "handlers/api"}}
}

// TestFeatureGraph_FrontendRequiresCodegen pins the canonical error
// shape from the spec: a feature enabled with a dependency off is a load
// error naming both sides and the fix.
func TestFeatureGraph_FrontendRequiresCodegen(t *testing.T) {
	in := `name: demo
module_path: github.com/example/demo
features:
  codegen: false
  frontend: true
`
	_, err := LoadStrict([]byte(in), "forge.yaml", serviceComponent()...)
	if err == nil {
		t.Fatal("expected load error for frontend-on/codegen-off, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"frontend", "codegen", "disabled", "Fix:"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q\ngot: %s", want, msg)
		}
	}
}

// TestFeatureGraph_ORMRequiresDriver: orm on with no database driver is a
// shape-precondition violation.
func TestFeatureGraph_ORMRequiresDriver(t *testing.T) {
	in := `name: demo
module_path: github.com/example/demo
database:
  driver: none
features:
  orm: true
  migrations: false
`
	_, err := LoadStrict([]byte(in), "forge.yaml", serviceComponent()...)
	if err == nil {
		t.Fatal("expected load error for orm-on/driver-none, got nil")
	}
	if !strings.Contains(err.Error(), "orm") || !strings.Contains(err.Error(), "database driver") {
		t.Errorf("error missing orm/driver wording\ngot: %s", err.Error())
	}
}

// TestFeatureGraph_DeployRequiresBuild: deploy on, build off → error.
func TestFeatureGraph_DeployRequiresBuild(t *testing.T) {
	in := `name: demo
module_path: github.com/example/demo
features:
  build: false
  deploy: true
`
	_, err := LoadStrict([]byte(in), "forge.yaml", serviceComponent()...)
	if err == nil {
		t.Fatal("expected load error for deploy-on/build-off, got nil")
	}
	if !strings.Contains(err.Error(), "deploy") || !strings.Contains(err.Error(), "build") {
		t.Errorf("error missing deploy/build wording\ngot: %s", err.Error())
	}
}

// TestFeatureGraph_IngressRequiresDeploy: experimental ingress on while
// deploy off → error.
func TestFeatureGraph_IngressRequiresDeploy(t *testing.T) {
	in := `name: demo
module_path: github.com/example/demo
features:
  deploy: false
  experimental:
    ingress: true
`
	_, err := LoadStrict([]byte(in), "forge.yaml", serviceComponent()...)
	if err == nil {
		t.Fatal("expected load error for ingress-on/deploy-off, got nil")
	}
	if !strings.Contains(err.Error(), "ingress") || !strings.Contains(err.Error(), "deploy") {
		t.Errorf("error missing ingress/deploy wording\ngot: %s", err.Error())
	}
}

// TestFeatureGraph_OperatorComponentRequiresOperatorsFeature: an
// operator-kind component without the experimental operators feature is
// a load error.
func TestFeatureGraph_OperatorComponentRequiresOperatorsFeature(t *testing.T) {
	// The operator component is injected via the variadic; an operator kind
	// derives the project to "service".
	operator := ComponentConfig{Name: "widget", Kind: "operator", Group: "example.com", Version: "v1"}
	base := `name: demo
module_path: github.com/example/demo
`
	_, err := LoadStrict([]byte(base), "forge.yaml", operator)
	if err == nil {
		t.Fatal("expected load error for operator component without operators feature, got nil")
	}
	if !strings.Contains(err.Error(), "operator") {
		t.Errorf("error missing operator wording\ngot: %s", err.Error())
	}

	// With the feature on, it loads clean.
	ok := base + `features:
  experimental:
    operators: true
`
	if _, err := LoadStrict([]byte(ok), "forge.yaml", operator); err != nil {
		t.Fatalf("operator component WITH operators feature should load: %v", err)
	}
}

// TestFeatureGraph_BatchesMultipleViolations: a config with several
// contradictions surfaces them all in one ValidationError.
func TestFeatureGraph_BatchesMultipleViolations(t *testing.T) {
	in := `name: demo
module_path: github.com/example/demo
features:
  codegen: false
  frontend: true
  build: false
  deploy: true
`
	_, err := LoadStrict([]byte(in), "forge.yaml", serviceComponent()...)
	if err == nil {
		t.Fatal("expected batched load error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "frontend") || !strings.Contains(msg, "deploy") {
		t.Errorf("expected both frontend and deploy violations batched\ngot: %s", msg)
	}
}

// TestFeatureGraph_ALaCarteLitmus is the spec's à la carte litmus: a
// kind:service project with ONLY orm + codegen + migrations on (frontend,
// deploy, observability, hot_reload, packs, starters explicitly off)
// loads with NO contradiction — "forge as pure postgres schema-truth ORM
// + codegen." The dependency graph must accept this clean.
func TestFeatureGraph_ALaCarteLitmus(t *testing.T) {
	in := `name: pure-orm
module_path: github.com/example/pure-orm
database:
  driver: postgres
features:
  codegen: true
  orm: true
  migrations: true
  frontend: false
  deploy: false
  build: false
  ci: false
  observability: false
  hot_reload: false
  packs: false
  contracts: false
  docs: false
`
	cfg, err := LoadStrict([]byte(in), "forge.yaml", serviceComponent()...)
	if err != nil {
		t.Fatalf("à la carte ORM+codegen+migrations config must load clean: %v", err)
	}
	// Confirm the resolved set is exactly what we asked for.
	eff := cfg.Features.EffectiveFeatures()
	on := map[string]bool{FeatureCodegen: true, FeatureORM: true, FeatureMigrations: true}
	for name, want := range map[string]bool{
		FeatureCodegen:       true,
		FeatureORM:           true,
		FeatureMigrations:    true,
		FeatureFrontend:      false,
		FeatureDeploy:        false,
		FeatureBuild:         false,
		FeatureObservability: false,
		FeatureHotReload:     false,
		FeaturePacks:         false,
	} {
		if eff[name] != want {
			t.Errorf("feature %q: got %v, want %v", name, eff[name], want)
		}
		_ = on
	}
}

// TestDeriveFeatureDefaults_Consistent asserts the DERIVED default set is
// always dependency-consistent: validateFeatureGraph must pass on a
// config that carries no explicit feature overrides, across every kind.
func TestDeriveFeatureDefaults_Consistent(t *testing.T) {
	for _, kind := range []string{ProjectKindService, ProjectKindCLI, ProjectKindLibrary} {
		t.Run(kind, func(t *testing.T) {
			c := &ProjectConfig{
				Name:       "demo",
				ModulePath: "github.com/example/demo",
				Kind:       kind,
			}
			ApplyDerivedDefaults(c)
			if issues := validateFeatureGraph(c); len(issues) > 0 {
				t.Errorf("derived defaults for kind=%s are not dependency-consistent: %+v", kind, issues)
			}
		})
	}
}

// TestDeriveFeatureDefaults_FrontendGatedOnCodegen: a non-service project
// that nonetheless declares a frontend must NOT derive frontend=on while
// codegen=off (that would trip the validator). The derived default gates
// frontend on codegen.
func TestDeriveFeatureDefaults_FrontendGatedOnCodegen(t *testing.T) {
	c := &ProjectConfig{
		Name:       "demo",
		ModulePath: "github.com/example/demo",
		Kind:       ProjectKindCLI,
		Frontends:  []FrontendConfig{{Name: "web"}},
	}
	ApplyDerivedDefaults(c)
	if c.Features.FrontendEnabled() {
		t.Error("frontend should not derive on for a non-service (codegen-off) kind")
	}
	if issues := validateFeatureGraph(c); len(issues) > 0 {
		t.Errorf("derived set should stay consistent: %+v", issues)
	}
}

// TestFeatureDependencies pins the public accessor used by `forge
// features`.
func TestFeatureDependencies(t *testing.T) {
	if got := FeatureDependencies(FeatureFrontend); len(got) != 1 || got[0] != FeatureCodegen {
		t.Errorf("frontend deps = %v, want [codegen]", got)
	}
	if got := FeatureDependencies(FeatureORM); len(got) != 2 {
		t.Errorf("orm deps = %v, want 2 (codegen + driver)", got)
	}
	if got := FeatureDependencies(FeatureCodegen); len(got) != 0 {
		t.Errorf("codegen deps = %v, want none", got)
	}
}
