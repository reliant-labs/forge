// derive.go — shape-derived defaults for forge.yaml.
//
// A freshly scaffolded forge.yaml is minimal: name, module_path,
// forge_version, services, frontends. Everything else — the features:
// block and the section blocks (database, ci, lint, contracts, auth,
// deploy, docker, k8s) — is DERIVED from the project shape at load time:
//
//   - feature flags derive from kind / database / frontends (see
//     DeriveFeatureDefaults for the per-flag rule);
//   - absent section blocks are filled with the canonical scaffold
//     defaults for the project kind (see sectionDefaults).
//
// Anything the user writes explicitly is taken literally; derivation
// never overrides a present value. The features: block and the section
// blocks therefore remain valid override surfaces — they are just no
// longer required boilerplate.
//
// The write side is symmetric: NormalizeForWrite drops values that are
// byte-identical to what derivation would produce, so a load → mutate →
// write round-trip keeps forge.yaml minimal instead of materializing
// every derived default back into the file.
package config

import (
	"go.yaml.in/yaml/v3"
)

// derivedFeatureDefaults carries the resolved shape-derived default for
// every stable feature. Computed once at load time by the config loader
// and attached to FeaturesConfig (unexported field); consulted by the
// *Enabled() accessors when the corresponding flag is absent.
type derivedFeatureDefaults struct {
	orm           bool
	codegen       bool
	migrations    bool
	ci            bool
	build         bool
	contracts     bool
	docs          bool
	frontend      bool
	observability bool
	hotReload     bool
	packs         bool
	starters      bool
	deploy        bool
}

// DeriveFeatureDefaults computes the default enabled/disabled state of
// every stable feature from the project shape. The rules:
//
//	orm           ⇔ kind == service AND a database driver is configured
//	codegen       ⇔ kind == service
//	migrations    ⇔ kind == service AND a database driver is configured
//	ci            ⇔ kind != library
//	build         ⇔ kind != library
//	contracts     ⇔ always on (contract.go works for every kind)
//	docs          ⇔ always on
//	frontend      ⇔ frontends list non-empty
//	observability ⇔ kind == service
//	hot_reload    ⇔ kind == service
//	packs         ⇔ kind == service
//	starters      ⇔ kind == service
//	deploy        ⇔ kind == service
//
// "database driver configured" means Database.Driver after section
// defaulting — i.e. postgres for a service project unless the user
// explicitly set `database: driver: none`. For the canonical service
// shape every rule resolves to enabled, matching the historical
// all-enabled default; for cli/library kinds the rules reproduce the
// per-kind matrix that `forge new --kind` used to write out explicitly.
//
// deploy derives from kind, NOT from a deploy/kcl/ directory probe.
// This is deliberate: derivation is intentionally pure project-shape —
// config load must not become order- or cwd-dependent by sniffing the
// filesystem. The scaffold ships deploy/kcl/ for every service project,
// so kind==service is the honest proxy; and the per-env deploy-config
// generate step is already a no-op when no deploy/kcl/<env>/ dirs exist
// on disk, so a user who deleted the deploy tree loses nothing (the
// steps simply find no envs to render). "deploy dir exists" was
// considered and rejected for those reasons.
//
// Experimental features (ingress, external_builds, operators,
// strict_wiring) and diagnostics are NOT derived — they stay default-off
// opt-ins regardless of shape.
func DeriveFeatureDefaults(c *ProjectConfig) map[FeatureName]bool {
	d := deriveFeatureDefaults(c)
	return map[FeatureName]bool{
		FeatureORM:           d.orm,
		FeatureCodegen:       d.codegen,
		FeatureMigrations:    d.migrations,
		FeatureCI:            d.ci,
		FeatureBuild:         d.build,
		FeatureContracts:     d.contracts,
		FeatureDocs:          d.docs,
		FeatureFrontend:      d.frontend,
		FeatureObservability: d.observability,
		FeatureHotReload:     d.hotReload,
		FeaturePacks:         d.packs,
		FeatureStarters:      d.starters,
		FeatureDeploy:        d.deploy,
	}
}

func deriveFeatureDefaults(c *ProjectConfig) *derivedFeatureDefaults {
	isService := c.IsServiceKind()
	isLibrary := c.IsLibraryKind()
	hasDB := isService && c.Database.Driver != "" && c.Database.Driver != "none"
	codegen := isService
	// Derivation MUST be dependency-consistent (see feature_graph.go): the
	// default set it produces can never trip the load-time graph validator.
	// codegen is the foundational dependency — orm, migrations, and frontend
	// all require it. Gate every derived codegen-dependent default on the
	// EFFECTIVE codegen value (an explicit `features.codegen: false` wins
	// over the shape-derived default), so disabling codegen cascades its
	// dependents off instead of leaving them on to trip the validator. A
	// user who wants one of them without codegen still has to opt in
	// explicitly, and then the validator makes them turn codegen on too.
	codegenEffective := codegen
	if c.Features.Codegen != nil {
		codegenEffective = *c.Features.Codegen
	}
	frontend := len(c.Frontends) > 0 && codegenEffective
	return &derivedFeatureDefaults{
		orm:           hasDB && codegenEffective,
		codegen:       codegen,
		migrations:    hasDB && codegenEffective,
		ci:            !isLibrary,
		build:         !isLibrary,
		contracts:     true,
		docs:          true,
		frontend:      frontend,
		observability: isService,
		hotReload:     isService,
		packs:         isService,
		starters:      isService,
		deploy:        isService,
	}
}

// EffectiveHotReload resolves the top-level hot_reload toggle: explicit
// value wins; absent derives to "on for service kind" (the value the
// scaffold used to write).
func (c *ProjectConfig) EffectiveHotReload() bool {
	if c.HotReload != nil {
		return *c.HotReload
	}
	return c.IsServiceKind()
}

// sectionDefaults returns the canonical scaffold defaults for every
// derivable section block, for the given project shape. This is the
// single source of truth shared by the load-time fill
// (ApplyDerivedDefaults) and the write-time normalizer
// (NormalizeForWrite) — and it is exactly what `forge new` used to
// write into every forge.yaml.
type sectionDefaultsSet struct {
	Database  DatabaseConfig
	CI        CIConfig
	Deploy    DeployConfig
	Docker    DockerConfig
	K8s       K8sConfig
	Lint      LintConfig
	Contracts ContractsConfig
	Auth      AuthConfig
}

func sectionDefaults(c *ProjectConfig) sectionDefaultsSet {
	isService := c.IsServiceKind()
	hasFrontend := len(c.Frontends) > 0
	t := true
	d := sectionDefaultsSet{
		CI: CIConfig{
			Provider: "github",
			Lint: CILintConfig{
				Golangci:        true,
				Buf:             isService,
				BufBreaking:     isService,
				Frontend:        hasFrontend,
				MigrationSafety: isService,
			},
			Test: CITestConfig{Race: true, Coverage: false},
			VulnScan: CIVulnConfig{
				Go:     true,
				Docker: isService,
				NPM:    hasFrontend,
			},
		},
		Lint: LintConfig{
			Contract: true,
			Frontend: FrontendLintConfig{
				CSSHealth:      hasFrontend,
				NoImportant:    "warn",
				NoInlineStyles: "warn",
			},
		},
		// Contracts: strict-by-default — every internal/<pkg>/ that
		// exposes behavior must declare an interface in contract.go.
		Contracts: ContractsConfig{
			Strict:             true,
			AllowExportedVars:  false,
			AllowExportedFuncs: false,
		},
		Auth: AuthConfig{Provider: "none"},
	}
	if isService {
		// Server-shaped sections only exist for service projects: a CLI
		// or library has no DB layer, nothing to deploy, no image.
		d.Database = DatabaseConfig{
			Driver:        "postgres",
			MigrationsDir: "db/migrations",
			MigrationSafety: MigrationSafetyConfig{
				Enabled:           &t,
				UnsafeAddColumn:   "error",
				DestructiveChange: "error",
				VolatileDefault:   "warn",
			},
		}
		d.Deploy = DeployConfig{Provider: "github"}
		d.Docker = DockerConfig{Registry: "ghcr.io"}
		d.K8s = K8sConfig{KCLDir: "deploy/kcl"}
	}
	return d
}

// ApplyDerivedDefaults resolves the shape-derived state of a freshly
// unmarshalled ProjectConfig:
//
//  1. every section block that is entirely absent (zero value) is filled
//     with the canonical scaffold default for the project kind;
//  2. the features block gets its derivation context attached so the
//     *Enabled() accessors can resolve absent flags from shape.
//
// Called by the loader (LoadStrict) — code that hand-constructs a
// ProjectConfig in tests without calling this keeps the historical
// zero-value semantics.
func ApplyDerivedDefaults(c *ProjectConfig) {
	d := sectionDefaults(c)
	if sectionIsZero(c.Database) {
		c.Database = d.Database
	}
	if sectionIsZero(c.CI) {
		c.CI = d.CI
	}
	if sectionIsZero(c.Deploy) {
		c.Deploy = d.Deploy
	}
	if sectionIsZero(c.Docker) {
		c.Docker = d.Docker
	}
	if sectionIsZero(c.K8s) {
		c.K8s = d.K8s
	}
	if sectionIsZero(c.Lint) {
		c.Lint = d.Lint
	}
	if sectionIsZero(c.Contracts) {
		c.Contracts = d.Contracts
	}
	if sectionIsZero(c.Auth) {
		c.Auth = d.Auth
	}
	// Features derivation runs AFTER the database fill — the orm /
	// migrations rules read the effective driver.
	c.Features.derived = deriveFeatureDefaults(c)
}

// NormalizeForWrite returns a copy of c with every derivable value that
// matches its shape-derived default removed, so marshalling produces the
// minimal forge.yaml. Explicit values that DIFFER from derivation are
// preserved — overrides survive round-trips; boilerplate does not.
//
// Dropping a value that equals its derived default is behavior-preserving
// by construction: the loader re-derives the identical value on the next
// read. The original c is not mutated.
func NormalizeForWrite(c *ProjectConfig) *ProjectConfig {
	out := *c
	d := sectionDefaults(c)
	if sectionsEquivalent(out.Database, d.Database) {
		out.Database = DatabaseConfig{}
	}
	if sectionsEquivalent(out.CI, d.CI) {
		out.CI = CIConfig{}
	}
	if sectionsEquivalent(out.Deploy, d.Deploy) {
		out.Deploy = DeployConfig{}
	}
	if sectionsEquivalent(out.Docker, d.Docker) {
		out.Docker = DockerConfig{}
	}
	if sectionsEquivalent(out.K8s, d.K8s) {
		out.K8s = K8sConfig{}
	}
	if sectionsEquivalent(out.Lint, d.Lint) {
		out.Lint = LintConfig{}
	}
	if sectionsEquivalent(out.Contracts, d.Contracts) {
		out.Contracts = ContractsConfig{}
	}
	if sectionsEquivalent(out.Auth, d.Auth) {
		out.Auth = AuthConfig{}
	}

	// hot_reload: drop when it matches the kind-derived default.
	if out.HotReload != nil && *out.HotReload == out.IsServiceKind() {
		out.HotReload = nil
	}

	// Feature flags: drop every explicit value that matches derivation.
	// Recompute fresh against the EFFECTIVE shape (absent sections filled
	// — e.g. a minimal service config derives orm/migrations from the
	// filled postgres driver, not from the empty on-disk block) and fresh
	// because the caller may have mutated shape (e.g. appended a
	// frontend) since the config was loaded.
	eff := *c
	ApplyDerivedDefaults(&eff)
	out.Features = normalizeFeatures(out.Features, eff.Features.derived)
	return &out
}

func normalizeFeatures(f FeaturesConfig, d *derivedFeatureDefaults) FeaturesConfig {
	drop := func(b *bool, derived bool) *bool {
		if b != nil && *b == derived {
			return nil
		}
		return b
	}
	f.ORM = drop(f.ORM, d.orm)
	f.Codegen = drop(f.Codegen, d.codegen)
	f.Migrations = drop(f.Migrations, d.migrations)
	f.CI = drop(f.CI, d.ci)
	f.Build = drop(f.Build, d.build)
	f.Contracts = drop(f.Contracts, d.contracts)
	f.Docs = drop(f.Docs, d.docs)
	f.Frontend = drop(f.Frontend, d.frontend)
	f.Observability = drop(f.Observability, d.observability)
	f.HotReload = drop(f.HotReload, d.hotReload)
	f.Packs = drop(f.Packs, d.packs)
	f.Starters = drop(f.Starters, d.starters)
	f.Deploy = drop(f.Deploy, d.deploy)
	// Diagnostics derives to off; drop an explicit false.
	f.Diagnostics = drop(f.Diagnostics, false)
	f.derived = d
	return f
}

// sectionIsZero reports whether a section block is entirely absent from
// the file (zero value). Marshal-based so semantically-empty shapes
// (nil vs empty slice) compare equal without per-section reflection.
func sectionIsZero[T any](section T) bool {
	var zero T
	return sectionsEquivalent(section, zero)
}

// sectionsEquivalent compares two section values by their canonical YAML
// rendering — the representation that actually round-trips through
// forge.yaml. This sidesteps nil-vs-empty slice and pointer-identity
// noise that reflect.DeepEqual would surface.
func sectionsEquivalent[T any](a, b T) bool {
	ab, errA := yaml.Marshal(a)
	bb, errB := yaml.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ab) == string(bb)
}
