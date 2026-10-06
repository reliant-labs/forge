// derive.go — everything forge.yaml no longer says, derived from the repo.
//
// A scaffolded forge.yaml is minimal: name, module_path, forge_version and
// the few real settings. Everything else is DERIVED at load time from what
// exists in the tree:
//
//   - the project kind (service | cli | library) from the sources;
//   - every feature flag (DeriveFeatureDefaults) from the kind and from
//     marker files and directories — there is no `features:` block;
//   - the database driver and migrations directory (DeriveDatabase) from
//     whether db/migrations exists;
//   - the pnpm-workspaces layout (DeriveFrontendWorkspaces) from
//     pnpm-workspace.yaml;
//   - the frontend inventory from frontends/ on disk, overlaid by the KCL
//     `forge.Frontend` declarations (frontend_inventory.go);
//   - absent section blocks (ci, lint, deploy, k8s, database.migration_safety)
//     are filled with the canonical scaffold defaults for the kind.
//
// Anything the user writes in a section that still exists is taken
// literally; derivation never overrides a present value. The write side is
// symmetric: NormalizeForWrite drops values byte-identical to what
// derivation would produce, so a load → mutate → write round-trip keeps
// forge.yaml minimal.
package config

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// NoORMDirective is the package-level marker that opts a project out of the
// generated ORM. It goes in the doc comment of the hand-written DB package
// (internal/db), beside the code it affects:
//
//	// Package db is the hand-owned repository layer.
//	//
//	//forge:no-orm: the repository is hand-written; entities are not projected
//	package db
//
// It mirrors `//forge:exclude-contract: <why>`, the other per-package opt-out
// already in use: the exemption sits in the package it exempts, with a
// reason, where a reader of that package sees it. A forge.yaml key was the
// wrong home because it described a property of internal/db from a file that
// never mentions internal/db. The reason is not required by the loader.
const NoORMDirective = "//forge:no-orm"

// hasNoORMMarker reports whether internal/db declares `//forge:no-orm`.
// Only non-test, non-generated .go files directly in internal/db are read,
// and only line comments — the same scope `//forge:exclude-contract` is
// honoured in.
func hasNoORMMarker(projectDir string) bool {
	if projectDir == "" {
		return false
	}
	dir := filepath.Join(projectDir, "internal", "db")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") ||
			strings.HasSuffix(n, "_test.go") || strings.HasSuffix(n, "_gen.go") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		found := false
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, NoORMDirective) {
				rest := line[len(NoORMDirective):]
				if rest == "" || rest[0] == ':' || rest[0] == ' ' || rest[0] == '\t' {
					found = true
					break
				}
			}
		}
		_ = f.Close()
		if found {
			return true
		}
	}
	return false
}

// gatewayDeclRE matches a KCL `forge.Gateway { ... }` declaration.
var gatewayDeclRE = regexp.MustCompile(`\bforge\.Gateway\s*\{`)

// declaresGateway reports whether the project's KCL tree declares a Gateway —
// the object that makes ingress mean anything. It is a TEXT scan of
// deploy/kcl, deliberately not a render: a render costs seconds per env, and
// config load runs on every command. A `forge.Gateway {` literal is exactly
// the declaration the renderer would lower, so the two cannot disagree about
// whether one exists. The vendored copy (.forge-kcl) is not under deploy/kcl.
func declaresGateway(projectDir string) bool {
	if projectDir == "" {
		return false
	}
	root := filepath.Join(projectDir, "deploy", "kcl")
	found := false
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || found {
			return nil
		}
		if d.IsDir() {
			if d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".k") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr == nil && gatewayDeclRE.Match(body) {
			found = true
		}
		return nil
	})
	return found
}

// hasOperatorPackage reports whether internal/operators holds an operator —
// the same presence test the generate pipeline's component discovery uses.
func hasOperatorPackage(projectDir string) bool {
	if projectDir == "" {
		return false
	}
	entries, err := os.ReadDir(filepath.Join(projectDir, "internal", "operators"))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() && e.Name() != "testdata" {
			return true
		}
	}
	return false
}

// DeriveDatabase fills the database driver and migrations directory from the
// tree: a service project with db/migrations uses postgres and that
// directory; anything else has no database. There is no setting to write —
// the directory IS the declaration, the same way the schema is (the
// migrations are the source of truth for it).
func DeriveDatabase(c *ProjectConfig) {
	if c.IsServiceKind() && c.projectDir != "" && dirExists(filepath.Join(c.projectDir, DefaultMigrationsDir)) {
		c.Database.Driver = "postgres"
		c.Database.MigrationsDir = DefaultMigrationsDir
		return
	}
	c.Database.Driver = "none"
	c.Database.MigrationsDir = ""
}

// DeriveFrontendWorkspaces reads the pnpm-workspaces layout off the tree: a
// root pnpm-workspace.yaml means the project shares generated clients and
// hooks across frontends through packages/api and packages/hooks.
func DeriveFrontendWorkspaces(c *ProjectConfig) {
	if c.projectDir == "" {
		return
	}
	c.workspaces = fileExists(filepath.Join(c.projectDir, "pnpm-workspace.yaml"))
}

// DeriveFeatureDefaults computes the enabled/disabled state of every feature
// from what exists in the repo. The rules:
//
//	codegen       ⇔ kind == service
//	orm           ⇔ a database AND codegen AND no //forge:no-orm marker in internal/db
//	migrations    ⇔ a database AND codegen
//	ci            ⇔ kind != library
//	build         ⇔ kind != library
//	contracts     ⇔ always on (contract.go works for every kind)
//	frontend      ⇔ a frontend exists (frontends/ or KCL) AND codegen
//	observability ⇔ kind == service
//	hot_reload    ⇔ kind == service
//	deploy        ⇔ kind == service
//	ingress       ⇔ deploy AND deploy/kcl declares a forge.Gateway
//	operators     ⇔ kind == service AND internal/operators holds an operator
//
// "a database" means db/migrations exists (DeriveDatabase). deploy derives
// from kind, NOT from a deploy/kcl probe: the scaffold ships deploy/kcl for
// every service, so kind==service is the honest proxy, and the per-env
// deploy steps are already a no-op when no env directories exist.
//
// The set is dependency-consistent by construction (see feature_graph.go):
// every dependent is gated on the EFFECTIVE value of what it requires, so a
// derived set can never trip the graph validator.
func DeriveFeatureDefaults(c *ProjectConfig) map[FeatureName]bool {
	isService := c.IsServiceKind()
	isLibrary := c.IsLibraryKind()
	hasDB := isService && c.Database.Driver != "" && c.Database.Driver != "none"
	codegen := isService
	deploy := isService
	return map[FeatureName]bool{
		FeatureORM:           hasDB && codegen && !hasNoORMMarker(c.projectDir),
		FeatureCodegen:       codegen,
		FeatureMigrations:    hasDB && codegen,
		FeatureCI:            !isLibrary,
		FeatureBuild:         !isLibrary,
		FeatureContracts:     true,
		FeatureFrontend:      len(c.Frontends) > 0 && codegen,
		FeatureObservability: isService,
		FeatureHotReload:     isService,
		FeatureDeploy:        deploy,
		FeatureIngress:       deploy && declaresGateway(c.projectDir),
		FeatureOperators:     isService && hasOperatorPackage(c.projectDir),
	}
}

// sectionDefaults returns the canonical scaffold defaults for every
// derivable section block, for the given project shape. This is the
// single source of truth shared by the load-time fill
// (ApplyDerivedDefaults) and the write-time normalizer
// (NormalizeForWrite) — and it is exactly what `forge project new` used to
// write into every forge.yaml.
type sectionDefaultsSet struct {
	Database DatabaseConfig
	CI       CIConfig
	Deploy   DeployConfig
	K8s      K8sConfig
	Lint     LintConfig
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
			Frontend: FrontendLintConfig{
				CSSHealth:      hasFrontend,
				NoImportant:    "warn",
				NoInlineStyles: "warn",
			},
		},
	}
	if isService {
		// Server-shaped sections only exist for service projects: a CLI
		// or library has no DB layer, nothing to deploy, no image.
		d.Database = DatabaseConfig{
			MigrationSafety: MigrationSafetyConfig{
				Enabled:           &t,
				UnsafeAddColumn:   "error",
				DestructiveChange: "error",
				VolatileDefault:   "warn",
			},
		}
		// Deploy carries only pipeline-control knobs now; its zero value is
		// the canonical default (the dead `provider` field was removed — the
		// CI provider derives to "github" via cfg.CI). No section carries a
		// registry: it is declared in the env's KCL.
		d.Deploy = DeployConfig{}
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
// Called by the loader (LoadProject) — code that hand-constructs a
// ProjectConfig in tests without calling this keeps the historical
// zero-value semantics.
//
// Prefer ApplyDerivedDefaultsFromNode when the yaml.Node is in hand: a
// partially-written section needs FIELD-level defaulting, and only the node
// knows which keys the user actually wrote. See derive_fill.go for why a
// zero-value test cannot substitute.
func ApplyDerivedDefaults(c *ProjectConfig) {
	ApplyDerivedDefaultsFromNode(c, nil)
}

// ApplyDerivedDefaultsFromNode is ApplyDerivedDefaults with the parsed
// forge.yaml mapping in hand, so an absent FIELD inside a present section
// still gets its derived default.
//
// A nil root reproduces the historical whole-section behaviour, which is what
// the no-node callers want: they are filling a config nobody hand-wrote, so
// there are no user-written keys to respect.
func ApplyDerivedDefaultsFromNode(c *ProjectConfig, root *yaml.Node) {
	d := sectionDefaults(c)
	present := map[string]bool{}
	if root != nil {
		present = presentKeys(root)
	}
	fillSectionDefaults(&c.Database, d.Database, "database", present)
	fillSectionDefaults(&c.CI, d.CI, "ci", present)
	fillSectionDefaults(&c.Deploy, d.Deploy, "deploy", present)
	fillSectionDefaults(&c.K8s, d.K8s, "k8s", present)
	fillSectionDefaults(&c.Lint, d.Lint, "lint", present)
	// Tree-derived state runs AFTER the section fill, and in dependency
	// order: the database (feature derivation reads it), the workspaces
	// layout, then the features that read both.
	DeriveDatabase(c)
	DeriveFrontendWorkspaces(c)
	c.Features.derived = DeriveFeatureDefaults(c)
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
	// Field-level, mirroring the field-level fill: a section that matches
	// its default in every field collapses to the zero value and vanishes
	// from forge.yaml, while a section with one real override keeps that
	// override alone instead of dragging the whole default block along.
	stripSectionDefaults(&out.Database, d.Database)
	stripSectionDefaults(&out.CI, d.CI)
	stripSectionDefaults(&out.Deploy, d.Deploy)
	stripSectionDefaults(&out.K8s, d.K8s)
	stripSectionDefaults(&out.Lint, d.Lint)

	return &out
}
