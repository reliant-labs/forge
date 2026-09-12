// File: internal/config/partial_section_test.go
//
// Regression coverage for the defect these tests were written against: a
// PARTIALLY specified `database:` block silently turned the migrations
// feature off, taking `forge lint --migration-safety` with it.
//
// ApplyDerivedDefaults filled section blocks all-or-nothing — `if
// sectionIsZero(c.Database) { c.Database = d.Database }`. Writing the escape
// hatch that forge's own migration-safety error recommends:
//
//	database:
//	    migration_safety:
//	        allowed_destructive: [db/migrations/0009_harden.up.sql]
//
// makes the section non-zero, so the fill is skipped entirely and Driver stays
// "". DeriveFeatureDefaults reads hasDB off that empty Driver, resolves
// FeatureMigrations to false, and `forge lint --migration-safety` prints
// "migrations feature is disabled in forge.yaml" and exits 0. A CI job goes
// green with the safety check switched off — strictly worse than the finding
// it was working around, and the user was following forge's own instructions.
//
// The fix is field-level defaulting: an absent FIELD within a present section
// gets its derived default, so declaring one knob cannot silently unset the
// rest. Every section shares the bug, so every section is covered here.
package config

import "testing"

// TestPartialDatabaseSectionKeepsMigrationsEnabled is the exact reproduction:
// the config text from the dogfood run, and the feature it must not disable.
func TestPartialDatabaseSectionKeepsMigrationsEnabled(t *testing.T) {
	src := minimalForgeYAML + `
database:
    migration_safety:
        allowed_destructive:
            - db/migrations/00009_harden_schema.up.sql
`
	cfg, err := LoadProject([]byte(src), serviceProjectPath(t, src))
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}

	if got := cfg.Database.Driver; got != "postgres" {
		t.Errorf("Driver = %q, want postgres — declaring migration_safety must not unset the driver", got)
	}
	if got := cfg.Database.MigrationsDir; got != "db/migrations" {
		t.Errorf("MigrationsDir = %q, want db/migrations", got)
	}
	if !cfg.Features.MigrationsEnabled() {
		t.Error("migrations resolved to DISABLED after declaring database.migration_safety.allowed_destructive — " +
			"following forge's own escape-hatch advice must not turn the check off")
	}
	if !cfg.Features.ORMEnabled() {
		t.Error("orm resolved to DISABLED — it derives from the same driver the partial section dropped")
	}
	if got := cfg.Database.MigrationSafety.AllowedDestructive; len(got) != 1 {
		t.Errorf("AllowedDestructive = %#v, want the one entry the user wrote", got)
	}
	if !cfg.Database.MigrationSafety.IsEnabled() {
		t.Error("migration_safety.enabled should still resolve on")
	}
}

// TestPartialDatabaseSectionFillsMissingSeverities covers the nested case: a
// section inside a section. Setting one severity dial must not blank the other
// two into their fallbacks by accident — here the fallbacks happen to match,
// so the assertion is on the stored values, which is what NormalizeForWrite
// and `forge project audit` read.
func TestPartialDatabaseSectionFillsMissingSeverities(t *testing.T) {
	src := minimalForgeYAML + `
database:
    migration_safety:
        volatile_default: error
`
	cfg, err := LoadProject([]byte(src), serviceProjectPath(t, src))
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}

	ms := cfg.Database.MigrationSafety
	if ms.VolatileDefault != "error" {
		t.Errorf("VolatileDefault = %q, want the explicit error — the user's value must win", ms.VolatileDefault)
	}
	if ms.UnsafeAddColumn != "error" {
		t.Errorf("UnsafeAddColumn = %q, want error from the derived default", ms.UnsafeAddColumn)
	}
	if ms.DestructiveChange != "error" {
		t.Errorf("DestructiveChange = %q, want error from the derived default", ms.DestructiveChange)
	}
	if !cfg.Features.MigrationsEnabled() {
		t.Error("migrations resolved to DISABLED from a partial migration_safety block")
	}
}

// TestPartialDatabaseSectionExplicitDriverWins guards the direction that
// matters more than the fill: an explicit value must never be overwritten by
// the default. `driver: none` is how a project legitimately says "no database",
// and a fill that clobbered it would be a far worse bug than the one being
// fixed.
func TestPartialDatabaseSectionExplicitDriverWins(t *testing.T) {
	src := minimalForgeYAML + `
database:
    driver: none
`
	cfg, err := LoadProject([]byte(src), serviceProjectPath(t, src))
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}

	if got := cfg.Database.Driver; got != "none" {
		t.Fatalf("Driver = %q, want none — an explicit value must survive the fill", got)
	}
	if cfg.Features.MigrationsEnabled() {
		t.Error("migrations should derive OFF for driver: none")
	}
}

// TestPartialDatabaseSectionExplicitDisableWins is the same guard one level
// down: `enabled: false` is the documented way to switch migration safety off,
// and the fill must not resurrect it.
func TestPartialDatabaseSectionExplicitDisableWins(t *testing.T) {
	src := minimalForgeYAML + `
database:
    migration_safety:
        enabled: false
`
	cfg, err := LoadProject([]byte(src), serviceProjectPath(t, src))
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}

	if cfg.Database.MigrationSafety.IsEnabled() {
		t.Error("explicit migration_safety.enabled: false was overwritten by the derived default")
	}
	if !cfg.Features.MigrationsEnabled() {
		t.Error("migration_safety.enabled: false turns the RULES off; it must not also unset the driver " +
			"and disable the whole migrations feature")
	}
}

// TestPartialSectionsFillPerField sweeps the remaining sections. Database is
// the one with a measured safety consequence, but the all-or-nothing fill was
// shared by every section, so each one had its own version of "declare one
// knob, silently lose the rest".
func TestPartialSectionsFillPerField(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		check func(t *testing.T, cfg *ProjectConfig)
	}{
		{
			name: "docker_registry_survives_partial_block",
			src: `
docker:
    build_contexts:
        forgepkg: ../forge/pkg
`,
			check: func(t *testing.T, cfg *ProjectConfig) {
				if got := cfg.Docker.Registry; got != "ghcr.io" {
					t.Errorf("Docker.Registry = %q, want ghcr.io — declaring a build context must not unset the registry", got)
				}
				if got := cfg.Docker.BuildContexts["forgepkg"]; got != "../forge/pkg" {
					t.Errorf("BuildContexts[forgepkg] = %q, want the user's value", got)
				}
			},
		},
		{
			name: "lint_frontend_defaults_survive_sibling_key",
			src: `
lint:
    handler_file_max_loc: 400
`,
			check: func(t *testing.T, cfg *ProjectConfig) {
				if got := cfg.Lint.HandlerFileMaxLOC; got != 400 {
					t.Errorf("HandlerFileMaxLOC = %d, want the explicit 400", got)
				}
				if got := cfg.Lint.Frontend.NoImportant; got != "warn" {
					t.Errorf("Lint.Frontend.NoImportant = %q, want warn from the derived default", got)
				}
				if !cfg.Lint.Frontend.CSSHealth {
					t.Error("Lint.Frontend.CSSHealth = false — a sibling key must not disable the css-health lane")
				}
			},
		},
		{
			name: "lint_frontend_partial_nested_block",
			src: `
lint:
    frontend:
        no_important: error
`,
			check: func(t *testing.T, cfg *ProjectConfig) {
				if got := cfg.Lint.Frontend.NoImportant; got != "error" {
					t.Errorf("NoImportant = %q, want the explicit error", got)
				}
				if got := cfg.Lint.Frontend.NoInlineStyles; got != "warn" {
					t.Errorf("NoInlineStyles = %q, want warn from the derived default — nested blocks default per field too", got)
				}
				if !cfg.Lint.Frontend.CSSHealth {
					t.Error("Lint.Frontend.CSSHealth = false after declaring a sibling severity dial")
				}
			},
		},
		{
			name: "ci_provider_survives_partial_block",
			src: `
ci:
    test:
        coverage: true
`,
			check: func(t *testing.T, cfg *ProjectConfig) {
				if got := cfg.CI.Provider; got != "github" {
					t.Errorf("CI.Provider = %q, want github", got)
				}
				if !cfg.CI.Lint.Golangci {
					t.Error("CI.Lint.Golangci = false — declaring ci.test must not disable the lint lanes")
				}
				if !cfg.CI.Lint.MigrationSafety {
					t.Error("CI.Lint.MigrationSafety = false — the same silent-disable as the database case, in CI")
				}
				if !cfg.CI.Test.Coverage {
					t.Error("explicit ci.test.coverage: true was lost")
				}
				if !cfg.CI.Test.Race {
					t.Error("CI.Test.Race = false — declaring coverage must not turn the race detector off")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := minimalForgeYAML + tc.src
			cfg, err := LoadProject([]byte(src), serviceProjectPath(t, src))
			if err != nil {
				t.Fatalf("LoadProject: %v", err)
			}
			tc.check(t, cfg)
		})
	}
}

// TestPartialSectionNormalizeRoundTrip closes the loop with the writer. A
// partial block that now loads to the full default set must still normalize
// back to just the user's own override — otherwise the fill leaks boilerplate
// into every forge.yaml that `forge project` rewrites.
func TestPartialSectionNormalizeRoundTrip(t *testing.T) {
	src := minimalForgeYAML + `
database:
    migration_safety:
        allowed_destructive:
            - db/migrations/00009_harden_schema.up.sql
`
	cfg, err := LoadProject([]byte(src), serviceProjectPath(t, src))
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}

	out := NormalizeForWrite(cfg)
	if got := out.Database.Driver; got != "" {
		t.Errorf("normalized Driver = %q, want it dropped as a derived default", got)
	}
	if got := out.Database.MigrationSafety.AllowedDestructive; len(got) != 1 {
		t.Errorf("normalized AllowedDestructive = %#v, want the user's entry preserved", got)
	}
}
