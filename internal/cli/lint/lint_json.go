// File: internal/cli/lint_json.go
//
// `forge lint --json` — machine-readable lint output for sub-agents
// and CI, following the same conventions as `forge project audit --json` and
// `forge doctor --json` (flag name `--json`, indented json.Encoder to
// stdout, non-zero exit via a sentinel error after the report prints).
//
// Output contract (stable; extensions are additive, same policy as the
// audit-json skill documents for `forge project audit --json`):
//
//	{
//	  "findings": [
//	    {
//	      "file": "handlers/api/handlers.go",  // omitted when not file-scoped
//	      "line": 42,                          // 1-based; omitted when unknown
//	      "col": 7,                            // 1-based; omitted when unknown
//	      "severity": "error",                 // "error" | "warning" | "info"
//	      "rule": "forge-config-deps",         // rule id; "external" for raw sub-tool lines
//	      "message": "...",
//	      "fix_hint": "..."                    // omitted when the rule has none
//	    }
//	  ],
//	  "summary": { "errors": 1, "warnings": 3, "infos": 0, "total": 4 },
//	  "ok": false
//	}
//
// Exit-code semantics are IDENTICAL to text mode: `ok` is false exactly
// when text mode would have exited non-zero, and in that case the
// command returns a sentinel error after the JSON has been written so
// cobra exits 1. Linters that are warnings-only in text mode (db,
// tests, banners, check-workarounds, orm-sync,
// frontend-stores, config-deps) never flip `ok`.
//
// New rules arrive as NEW `rule` values on the existing finding shape —
// the additive-extension contract: no field is renamed or repurposed, so
// a consumer that filters on the rules it knows keeps working. The
// generated-file ownership gate contributes two:
// `forge-generated-file-edited` (error; flips `ok` to false) and
// `forge-generated-file-unverified` (warning; never flips `ok`).
//
// The frontend typecheck lane contributes two more:
// `forge-frontend-typecheck` (TypeScript diagnostics, file/line/col
// anchored at the project-relative source path; severity follows
// forge.yaml's lint.frontend.typecheck, error by default) and
// `forge-frontend-typecheck-unavailable` (warning — the check could NOT
// run because deps aren't installed or no compiler resolves; escalates
// to a gating error under --strict). A consumer that treats "unavailable"
// as clean is asserting something the report never claims: a typecheck
// that could not run is not a typecheck that passed.
//
// THE `-unavailable` FAMILY. That last sentence is a contract, not a note
// about one lane, and every lane that can fail to execute names the
// condition with its own rule id:
//
//	forge-frontend-typecheck-unavailable   — no compiler / deps not installed
//	forge-frontend-lint-unavailable        — frontend dir missing / deps not installed
//	typed-config-guardrail-unavailable     — golangci-lint never reported
//
// All three are warnings by default and errors that flip `ok` under
// --strict. The rule id is what makes them machine-legible: before it
// existed, the guardrail reported "could not run" under the SAME rule id
// and severity as its real findings, so `forge lint --json` answered
// "ok": true over a check that never executed and no consumer could tell.
// New lanes join the family rather than inventing a spelling.
//
// External sub-tools (golangci-lint, contractlint, buf, npm scripts)
// have their output captured and normalized: lines matching the
// conventional `file:line[:col]: message` shape become file-scoped
// findings; anything else is preserved verbatim as a finding with
// rule "external" — sub-tool output is never silently dropped.
// Skipped linters (tool not on PATH, feature disabled, directory
// missing) surface as severity "info" findings with rule "skipped" so
// an agent can tell "clean" apart from "didn't run".

package lint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/linter/finding"
	"github.com/reliant-labs/forge/internal/linter/forgeconv"
	"github.com/reliant-labs/forge/internal/linter/migrationlint"
	"github.com/reliant-labs/forge/internal/linter/scaffolds"
	"github.com/reliant-labs/forge/internal/projectstore"
)

// Severity values used in the JSON report. These now match the canonical
// internal/linter/finding spellings exactly, so no normalization is
// needed when mapping linter findings onto the JSON contract.
const (
	lintSevError   = string(finding.SeverityError)
	lintSevWarning = string(finding.SeverityWarning)
	lintSevInfo    = string(finding.SeverityInfo)
)

// lintJSONFinding is one normalized diagnostic. See the file header for
// the field contract.
type lintJSONFinding struct {
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Col      int    `json:"col,omitempty"`
	Severity string `json:"severity"`
	Rule     string `json:"rule"`
	Message  string `json:"message"`
	FixHint  string `json:"fix_hint,omitempty"`
}

// lintJSONSummary counts findings by severity. Total is the slice
// length, included so consumers don't have to add the buckets.
type lintJSONSummary struct {
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
	Infos    int `json:"infos"`
	Total    int `json:"total"`
}

// lintJSONReport is the top-level `forge lint --json` structure.
// Findings is always non-nil so consumers see `[]`, not `null`.
type lintJSONReport struct {
	Findings []lintJSONFinding `json:"findings"`
	Summary  lintJSONSummary   `json:"summary"`
	OK       bool              `json:"ok"`
}

// errLintJSONFailed is the sentinel returned after the JSON report has
// been written when text mode would have exited non-zero. Same pattern
// as errDoctorFailed: the report carries the detail; this line just
// makes cobra exit 1 with a one-line stderr reason.
var errLintJSONFailed = fmt.Errorf("lint reported errors; see JSON report above")

// buildLintJSONReport assembles the report envelope from collected
// findings plus the gating verdict computed by the caller (which
// mirrors text mode's hasFailed logic exactly — see file header).
func buildLintJSONReport(findings []lintJSONFinding, gated bool) *lintJSONReport {
	if findings == nil {
		findings = []lintJSONFinding{}
	}
	var sum lintJSONSummary
	for _, f := range findings {
		switch f.Severity {
		case lintSevError:
			sum.Errors++
		case lintSevInfo:
			sum.Infos++
		default:
			sum.Warnings++
		}
	}
	sum.Total = len(findings)
	return &lintJSONReport{Findings: findings, Summary: sum, OK: !gated}
}

// skippedFinding records a linter that did not run (tool missing,
// feature disabled, directory absent) so JSON consumers can tell
// "clean" from "didn't run". Never gates.
func skippedFinding(msg string) lintJSONFinding {
	return lintJSONFinding{Severity: lintSevInfo, Rule: "skipped", Message: msg}
}

// runLintJSON is the --json counterpart of runLint. It mirrors the
// same flag dispatch (targeted single-linter modes, else all linters)
// but collects findings instead of printing, then writes one JSON
// document to stdout.
//
// Stray human prints are a hazard here: several shared helpers
// (loadProjectConfig warnings and friends) write to
// os.Stdout. For the collection phase we point os.Stdout at stderr so
// stdout stays pure JSON and nothing a helper says is lost — it just
// lands on stderr where humans (and CI logs) still see it.
func runLintJSON(ctx context.Context, flags lintFlags, paths []string) error {
	// Suggestion / mutation modes emit YAML snippets or rewrite files —
	// neither has a sensible findings shape. Refuse loudly instead of
	// emitting JSON that silently ignored the flag.
	if flags.fix || flags.suggestExcludes || flags.suggestBufExcepts {
		return cliutil.UserErr("forge lint --json",
			"--json cannot be combined with --fix, --suggest-excludes, or --suggest-buf-excepts",
			"",
			"run those modes without --json; their output is suggestion- or mutation-shaped, not findings-shaped")
	}

	realStdout := os.Stdout
	os.Stdout = os.Stderr
	report, err := collectLintJSON(ctx, flags, paths)
	os.Stdout = realStdout
	if err != nil {
		// Hard failure (I/O, config parse, …) — same as text mode:
		// no report, non-zero exit with the underlying reason.
		return err
	}

	enc := json.NewEncoder(realStdout)
	enc.SetIndent("", "  ")
	if encErr := enc.Encode(report); encErr != nil {
		return encErr
	}
	if !report.OK {
		return errLintJSONFailed
	}
	return nil
}

// collectLintJSON dispatches on the targeted-linter flags exactly like
// runLint, falling through to the all-linters sweep when none is set.
func collectLintJSON(ctx context.Context, flags lintFlags, paths []string) (*lintJSONReport, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("getwd: %w", err)
	}

	// Project config: same tolerance as text mode — missing is fine,
	// parse errors are fatal.
	store, cfgErr := loadProjectStore()
	if cfgErr != nil && !errors.Is(cfgErr, ErrProjectConfigNotFound) {
		return nil, fmt.Errorf("failed to load project config: %w", cfgErr)
	}
	var cfg *config.ProjectConfig
	if store != nil {
		cfg = store.Config()
	}

	if report, handled, err := collectSingleLinterJSON(ctx, flags, paths, cwd, store, cfg); handled {
		return report, err
	}

	return collectAllLintersJSON(ctx, lintRunOptions{
		strict:        flags.strict,
		skipFrontends: flags.skipFrontends,
		paths:         paths,
		cfg:           cfg,
	}, cwd)
}

// collectSingleLinterJSON dispatches the --<linter> flags that select ONE
// linter to run on its own. handled=false means no such flag was set and the
// caller should run the whole suite instead; it is distinct from a nil report,
// which a selected linter can legitimately return alongside an error.
func collectSingleLinterJSON(
	ctx context.Context,
	flags lintFlags,
	paths []string,
	cwd string,
	store *projectstore.Store,
	cfg *config.ProjectConfig,
) (*lintJSONReport, bool, error) {
	// Each arm returns through this closure so the switch stays a flat
	// one-line-per-linter dispatch table.
	done := func(r *lintJSONReport, err error) (*lintJSONReport, bool, error) {
		if err != nil {
			return nil, true, err
		}
		return r, true, nil
	}
	report := func(fs []lintJSONFinding, gated bool, err error) (*lintJSONReport, bool, error) {
		if err != nil {
			return nil, true, err
		}
		return done(buildLintJSONReport(fs, gated), nil)
	}
	// reportUngated is report for the linters that return findings only —
	// they cannot gate the build, so gated is always false.
	reportUngated := func(fs []lintJSONFinding, err error) (*lintJSONReport, bool, error) {
		return report(fs, false, err)
	}

	switch {
	case flags.contract, flags.exportedVars:
		if store != nil && !store.Features().ContractsEnabled() {
			return done(buildLintJSONReport([]lintJSONFinding{skippedFinding("contracts feature is disabled in forge.yaml")}, false), nil)
		}
		return report(collectContractLintJSON(ctx, paths, contractExcludesFromConfig(cfg)))
	case flags.migrationSafety:
		if store != nil && !store.Features().MigrationsEnabled() {
			return done(buildLintJSONReport([]lintJSONFinding{skippedFinding("migrations feature is disabled in forge.yaml")}, false), nil)
		}
		return report(collectMigrationSafetyJSON(cfg))
	case flags.conventions:
		return report(collectConventionsJSON(forgeconv.LintOptions{Strict: flags.strict}))
	case flags.generatedDrift:
		return report(collectGeneratedDriftJSON(cwd))
	case flags.frontendStores:
		return reportUngated(collectFrontendStoresJSON(cwd))
	case flags.scaffolds:
		return report(collectScaffoldsJSON(cwd))
	case flags.tests:
		return report(collectTestsJSON(cwd))
	case flags.banners:
		return reportUngated(collectBannersJSON(cwd))
	case flags.checkWorkarounds:
		return reportUngated(collectWorkaroundsJSON(cwd))
	case flags.optionalDepsGuard:
		return reportUngated(collectOptionalDepsGuardJSON(cwd))
	case flags.configDeps:
		return reportUngated(collectConfigDepsJSON(cwd))
	case flags.columnMarkers:
		return reportUngated(collectColumnMarkersJSON(cfg))
	case flags.crudFixtures:
		return reportUngated(collectCrudFixturesJSON(cwd, cfg))
	case flags.protoMarkers:
		return reportUngated(collectProtoMarkersJSON(protoDirDefault))
	case flags.createNullability:
		fs, err := collectCreateNullabilityJSON(protoDirDefault)
		return report(fs, len(fs) > 0, err)
	case flags.computedFields:
		return reportUngated(collectComputedFieldsJSON(cwd))
	case flags.protoOptions:
		return reportUngated(collectProtoOptionsJSON(protoDirDefault))
	case flags.vendoredProtos:
		return report(collectVendoredProtosJSON(cwd))
	case flags.configReach:
		return reportUngated(collectConfigReachJSON(cwd, cfg))
	}

	return nil, false, nil
}

// collectAllLintersJSON mirrors runAllLinters step-for-step. Each step
// contributes findings; `gated` flips on exactly the conditions that
// set hasFailed in text mode. Per-step collection errors (I/O etc.)
// degrade to an error-severity finding rather than aborting the sweep
// — matching text mode, which prints the failure and keeps walking
// only for the advisory linters but hard-fails the run for the gating
// ones via hasFailed.
func collectAllLintersJSON(ctx context.Context, opts lintRunOptions, cwd string) (*lintJSONReport, error) {
	rc := &lintRunCtx{
		ctx:           ctx,
		fix:           false,
		strict:        opts.strict,
		skipFrontends: opts.skipFrontends,
		paths:         opts.paths,
		cfg:           opts.cfg,
		cwd:           cwd,
	}

	var findings []lintJSONFinding
	gated := false

	for _, step := range lintPipeline() {
		run, skipMsg := step.shouldRun(rc)
		if !run {
			// A skip message surfaces as an info finding so JSON consumers
			// can tell "clean" from "didn't run"; a silent skip (directory
			// absent) contributes nothing — both mirror the text driver and
			// the pre-refactor JSON output exactly.
			if skipMsg != "" {
				findings = append(findings, skippedFinding(skipMsg))
			}
			continue
		}
		fs, g, err := step.collect(rc)
		if err != nil {
			// A hard collection failure degrades to a finding rather than
			// aborting the sweep — severity/gating governed by step.gates,
			// exactly as the old per-step collectErr did.
			sev := lintSevWarning
			if step.gates {
				sev = lintSevError
			}
			findings = append(findings, lintJSONFinding{
				Severity: sev,
				Rule:     "external",
				Message:  fmt.Sprintf("%s failed: %v", step.name, err),
			})
			gated = gated || step.gates
			continue
		}
		findings = append(findings, fs...)
		gated = gated || g
	}

	return buildLintJSONReport(findings, gated), nil
}

// ---------------------------------------------------------------------------
// Structured collectors — thin maps over the internal linter packages.
// ---------------------------------------------------------------------------

// findingsToJSON is the single canonical mapper from the shared
// finding.Finding (emitted by every internal linter — forgeconv,
// scaffolds, migrationlint) onto the lint --json contract. It replaces
// the near-identical per-package mappers that existed before the finding
// package was introduced.
//
// Field mapping rules, unified:
//   - Severity passes through directly: the canonical finding severities
//     ("error"/"warning"/"info") ARE the JSON contract values, so no
//     normalization shim is needed.
//   - File comes from f.File, falling back to f.Path for whole-file
//     (line-less) scaffold findings — exactly one of the two is ever set.
//   - Remediation becomes fix_hint (forgeconv's actionable hints).
func findingsToJSON(fs []finding.Finding) []lintJSONFinding {
	out := make([]lintJSONFinding, 0, len(fs))
	for _, f := range fs {
		file := f.File
		if file == "" {
			file = f.Path
		}
		out = append(out, lintJSONFinding{
			File:     file,
			Line:     f.Line,
			Severity: string(f.Severity),
			Rule:     f.Rule,
			Message:  f.Message,
			FixHint:  f.Remediation,
		})
	}
	return out
}

func collectConventionsJSON(opts forgeconv.LintOptions) ([]lintJSONFinding, bool, error) {
	combined, notes, _, err := collectConventionFindings(opts)
	if err != nil {
		return nil, false, err
	}
	out := make([]lintJSONFinding, 0, len(notes)+len(combined.Findings))
	for _, n := range notes {
		out = append(out, skippedFinding(n))
	}
	out = append(out, findingsToJSON(combined.Findings)...)
	return out, combined.HasErrors(), nil
}

func collectMigrationSafetyJSON(cfg *config.ProjectConfig) ([]lintJSONFinding, bool, error) {
	migrationsDir := filepath.Join("db", "migrations")
	ruleConfig := migrationlint.DefaultConfig()
	if cfg != nil {
		if cfg.Database.MigrationsDir != "" {
			migrationsDir = cfg.Database.MigrationsDir
		}
		ruleConfig = migrationlint.ConfigFromProject(cfg.Database.MigrationSafety)
	}
	result, err := migrationlint.LintMigrationsDir(migrationsDir, ruleConfig)
	if err != nil {
		return nil, false, fmt.Errorf("migration safety lint failed: %w", err)
	}
	out := findingsToJSON(result.Findings)
	// Migration findings share one fixed remediation (they carry no
	// per-finding Remediation of their own).
	for i := range out {
		out[i].FixHint = migrationlint.DestructiveChangeRemediation
	}
	return out, result.HasErrors(), nil
}

func collectFrontendStoresJSON(cwd string) ([]lintJSONFinding, error) {
	res, err := forgeconv.LintFrontendStores(cwd)
	if err != nil {
		return nil, fmt.Errorf("frontend-stores lint failed: %w", err)
	}
	return findingsToJSON(res.Findings), nil
}

func collectScaffoldsJSON(cwd string) ([]lintJSONFinding, bool, error) {
	res, err := scaffolds.LintRoot(cwd)
	if err != nil {
		return nil, false, fmt.Errorf("scaffold lint failed: %w", err)
	}
	return findingsToJSON(res.Findings), res.HasErrors(), nil
}

// collectTestsJSON mirrors runTestsLint on the JSON path. The second
// return reports whether anything gates — only the shape rule can, so the
// two paths agree on which findings are fatal.
func collectTestsJSON(cwd string) ([]lintJSONFinding, bool, error) {
	handlerRes, err := forgeconv.LintHandlerTests(cwd)
	if err != nil {
		return nil, false, fmt.Errorf("handler-test lint failed: %w", err)
	}
	frontendRes, err := forgeconv.LintFrontendHookTests(cwd)
	if err != nil {
		return nil, false, fmt.Errorf("frontend-hook-test lint failed: %w", err)
	}
	shapeRes, err := forgeconv.LintFrontendHookTestShape(cwd)
	if err != nil {
		return nil, false, fmt.Errorf("frontend-hook-test-shape lint failed: %w", err)
	}
	out := findingsToJSON(handlerRes.Findings)
	out = append(out, findingsToJSON(frontendRes.Findings)...)
	out = append(out, findingsToJSON(shapeRes.Findings)...)
	return out, shapeRes.HasErrors(), nil
}

func collectBannersJSON(cwd string) ([]lintJSONFinding, error) {
	hasTemplates := dirExists(filepath.Join("internal", "templates")) ||
		dirExists(filepath.Join("internal", "packs"))
	if !hasTemplates {
		return nil, nil
	}
	res, err := scaffolds.BannerLintRoot(cwd)
	if err != nil {
		return nil, fmt.Errorf("banner lint failed: %w", err)
	}
	return findingsToJSON(res.Findings), nil
}

// collectOptionalDepsGuardJSON maps optional-deps-guard findings onto
// the JSON contract. Always warnings — the walker is deliberately not
// full dataflow, so findings never gate (see
// lint_optional_deps_guard.go's header for the conservatism contract).
func collectOptionalDepsGuardJSON(projectDir string) ([]lintJSONFinding, error) {
	findings, err := collectOptionalDepsGuardFindings(projectDir)
	if err != nil {
		return nil, fmt.Errorf("optional-deps-guard lint failed: %w", err)
	}
	out := make([]lintJSONFinding, 0, len(findings))
	for _, f := range findings {
		out = append(out, lintJSONFinding{
			File:     f.File,
			Line:     f.Line,
			Col:      f.Col,
			Severity: lintSevWarning,
			Rule:     "forge-optional-deps-guard",
			Message:  fmt.Sprintf("%s dereferences optional dep Deps.%s (marked `// forge:optional-dep` — may be nil) without a dominating nil-guard in %s", f.Expr, f.Field, f.Method),
			FixHint:  optionalDepsGuardFixHint(f),
		})
	}
	return out, nil
}

// collectConfigDepsJSON maps config-deps findings onto the JSON
// contract. Severity warning across the board — scalar Deps fields
// compile (and may be hand-wired today); the finding is the nudge
// toward the component config-block declaration.
func collectConfigDepsJSON(projectDir string) ([]lintJSONFinding, error) {
	findings, err := collectConfigDepsFindings(projectDir)
	if err != nil {
		return nil, fmt.Errorf("config-deps lint failed: %w", err)
	}
	out := make([]lintJSONFinding, 0, len(findings))
	for _, f := range findings {
		out = append(out, lintJSONFinding{
			File:     f.File,
			Line:     f.Line,
			Col:      f.Col,
			Severity: lintSevWarning,
			Rule:     "forge-config-deps",
			Message:  fmt.Sprintf("%s/%s Deps.%s is a naked scalar (%s) — scalar Deps fields are configuration, not collaborators", f.Role, f.Package, f.Field, f.Type),
			FixHint:  configDepsFixHint(f),
		})
	}
	return out, nil
}

// collectColumnMarkersJSON maps column-markers findings onto the JSON
// contract. Severity warning across the board — an unrecognized forge:*
// marker might be a future forge version's, so the finding is a nudge to
// check the spelling, not a hard failure.
func collectColumnMarkersJSON(cfg *config.ProjectConfig) ([]lintJSONFinding, error) {
	findings, err := collectColumnMarkerFindings(migrationsDirFor(cfg))
	if err != nil {
		return nil, fmt.Errorf("column-markers lint failed: %w", err)
	}
	out := make([]lintJSONFinding, 0, len(findings))
	for _, f := range findings {
		out = append(out, lintJSONFinding{
			File:     f.File,
			Line:     f.Line,
			Severity: lintSevWarning,
			Rule:     "forge-column-markers",
			Message:  fmt.Sprintf("unrecognized forge:* marker on %s", f.Object),
			FixHint:  columnMarkerFixHint(f),
		})
	}
	return out, nil
}

// collectCrudFixturesJSON maps crud-fixtures findings onto the JSON
// contract. Severity warning across the board: the fixture is genuinely
// stale and its test genuinely fails, but the file is the user's — forge
// scaffolded it once and does not own it — so the finding reports and
// locates the problem rather than gating the build on an edit only the
// author can make.
func collectCrudFixturesJSON(cwd string, cfg *config.ProjectConfig) ([]lintJSONFinding, error) {
	findings, err := collectCrudFixtureFindings(cwd, migrationsDirFor(cfg))
	if err != nil {
		return nil, fmt.Errorf("crud-fixtures lint failed: %w", err)
	}
	out := make([]lintJSONFinding, 0, len(findings))
	for _, f := range findings {
		out = append(out, lintJSONFinding{
			File:     f.File,
			Line:     f.Line,
			Severity: lintSevWarning,
			Rule:     "forge-crud-fixtures",
			Message: fmt.Sprintf("seeded value %s in %s.%s references no seeded %s.%s row",
				f.Value, f.Table, f.Column, f.RefTable, f.RefColumn),
			FixHint: crudFixtureFixHint(f),
		})
	}
	return out, nil
}

// collectProtoMarkersJSON maps proto-markers findings onto the JSON
// contract. Severity warning across the board, for the same reason as its
// column-marker sibling — an unrecognized forge:* marker might be a future
// forge version's, or unrelated prose, so the finding is a nudge to check
// the spelling rather than a hard failure.
func collectProtoMarkersJSON(protoDir string) ([]lintJSONFinding, error) {
	findings, err := collectProtoMarkerFindings(protoDir)
	if err != nil {
		return nil, fmt.Errorf("proto-markers lint failed: %w", err)
	}
	out := make([]lintJSONFinding, 0, len(findings))
	for _, f := range findings {
		msg := fmt.Sprintf("unrecognized forge:* proto marker %q", f.Marker)
		if f.Renamed != "" {
			msg = fmt.Sprintf("removed forge:* proto marker %q (renamed to %q)", f.Marker, f.Renamed)
		}
		out = append(out, lintJSONFinding{
			File:     f.File,
			Line:     f.Line,
			Severity: lintSevWarning,
			Rule:     "forge-proto-markers",
			Message:  msg,
			FixHint:  protoMarkerFixHint(f),
		})
	}
	return out, nil
}

// collectCreateNullabilityJSON maps create-nullability findings onto the
// JSON contract. Severity ERROR, unlike its advisory proto siblings: an
// unknown marker or option might be a future forge's, but two declarations
// of the same field disagreeing about presence is unambiguous — it
// silently corrupts writes and has exactly one correct resolution.
func collectCreateNullabilityJSON(protoDir string) ([]lintJSONFinding, error) {
	findings, err := collectCreateNullabilityFindings(protoDir)
	if err != nil {
		return nil, fmt.Errorf("create-nullability lint failed: %w", err)
	}
	out := make([]lintJSONFinding, 0, len(findings))
	for _, f := range findings {
		side, other := "the entity", "Create"+f.Entity+"Request"
		if !f.EntityOptional {
			side, other = "Create"+f.Entity+"Request", "the entity"
		}
		out = append(out, lintJSONFinding{
			File:     f.File,
			Line:     f.Line,
			Severity: lintSevError,
			Rule:     "forgeconv-create-request-nullability",
			Message: fmt.Sprintf("%s.%s is `optional` on %s but not on %s",
				f.Entity, f.Field, side, other),
			FixHint: createNullabilityFixHint(f),
		})
	}
	return out, nil
}

// collectComputedFieldsJSON maps computed-fields findings onto the JSON
// contract. Severity warning: the finding is high-confidence, but the fix
// is app logic only the author can write, and a project mid-migration
// (marker added before the hook) should still be able to lint.
func collectComputedFieldsJSON(cwd string) ([]lintJSONFinding, error) {
	findings, err := collectComputedFieldFindings(cwd)
	if err != nil {
		return nil, fmt.Errorf("computed-fields lint failed: %w", err)
	}
	out := make([]lintJSONFinding, 0, len(findings))
	for _, f := range findings {
		out = append(out, lintJSONFinding{
			File:     f.File,
			Line:     f.Line,
			Severity: lintSevWarning,
			Rule:     "forgeconv-computed-field-unwritten",
			Message: fmt.Sprintf("%s.%s is marked %s but no non-generated Go file assigns %s",
				f.Entity, f.Field, codegen.ProtoMarkerComputed, f.GoField),
			FixHint: computedFieldFixHint(f),
		})
	}
	return out, nil
}

// collectProtoOptionsJSON maps proto-options findings onto the JSON
// contract. Severity warning: the scan is a source-level parse that does
// not resolve imports, so it cannot see a project that legitimately
// extended forge's option messages — the vendored-protos rule gates the
// root cause instead. See lint_proto_options.go for the full rationale.
func collectProtoOptionsJSON(protoDir string) ([]lintJSONFinding, error) {
	findings, err := collectProtoOptionFindings(protoDir)
	if err != nil {
		return nil, fmt.Errorf("proto-options lint failed: %w", err)
	}
	out := make([]lintJSONFinding, 0, len(findings))
	for _, f := range findings {
		msg := fmt.Sprintf("(%s) is not an annotation this forge binary defines", f.Extension)
		if f.Field != "" {
			msg = fmt.Sprintf("(%s).%s is not a field %s defines — the annotation is read by nothing",
				f.Extension, f.Field, f.Message)
		}
		out = append(out, lintJSONFinding{
			File:     f.File,
			Line:     f.Line,
			Severity: lintSevWarning,
			Rule:     "forge-proto-options",
			Message:  msg,
			FixHint:  protoOptionFixHint(f),
		})
	}
	return out, nil
}

// collectVendoredProtosJSON maps vendored-proto drift onto the JSON
// contract, returning the gating verdict alongside: drift in forge.proto
// is an ERROR that flips `ok` to false (a field-number collision with an
// upstream `reserved` is a correctness bug and the fix is mechanical),
// while validate.proto drift is a warning. See lint_vendored_protos.go.
func collectVendoredProtosJSON(projectDir string) ([]lintJSONFinding, bool, error) {
	findings, err := collectVendoredProtoFindings(projectDir)
	if err != nil {
		return nil, false, fmt.Errorf("vendored-protos lint failed: %w", err)
	}
	out := make([]lintJSONFinding, 0, len(findings))
	for _, f := range findings {
		severity := lintSevWarning
		if f.Severity == vendoredSeverityError {
			severity = lintSevError
		}
		out = append(out, lintJSONFinding{
			File:     f.File,
			Severity: severity,
			Rule:     "forge-vendored-proto-drift",
			Message: fmt.Sprintf("%s differs from the copy embedded in this forge binary:\n%s",
				f.File, strings.TrimRight(f.Diff, "\n")),
			FixHint: vendoredProtoFixHint(f),
		})
	}
	return out, vendoredProtosGate(findings), nil
}

// collectConfigReachJSON maps config-reach findings onto the JSON
// contract. Severity warning: an unreachable field is dead weight rather
// than a defect, and the remediation is a deletion the author has to
// judge. See lint_config_reach.go.
func collectConfigReachJSON(projectDir string, cfg *config.ProjectConfig) ([]lintJSONFinding, error) {
	findings, err := collectConfigReachFindingsForProject(projectDir, cfg)
	if err != nil {
		return nil, fmt.Errorf("config-reach lint failed: %w", err)
	}
	out := make([]lintJSONFinding, 0, len(findings))
	for _, f := range findings {
		out = append(out, lintJSONFinding{
			Severity: lintSevWarning,
			Rule:     "forge-config-unreachable",
			Message: fmt.Sprintf("config field %s.%s is loaded by no binary or frontend",
				f.Message, f.Field),
			FixHint: configReachFixHint(f),
		})
	}
	return out, nil
}

func collectWorkaroundsJSON(cwd string) ([]lintJSONFinding, error) {
	res, err := scaffolds.LintWorkaroundsRoot(cwd)
	if err != nil {
		return nil, fmt.Errorf("check-workarounds lint failed: %w", err)
	}
	return findingsToJSON(res.Findings), nil
}

// ---------------------------------------------------------------------------
// External-tool collectors — capture + normalize subprocess output.
// ---------------------------------------------------------------------------

// reFileLineCol matches the conventional `file:line[:col]: message`
// diagnostic shape emitted by golangci-lint, go vet–style analyzers
// (contractlint), and buf lint. Group 1 file, 2 line, 3 optional col,
// 4 message.
var reFileLineCol = regexp.MustCompile(`^([^\s:]+):(\d+)(?::(\d+))?:\s*(.+)$`)

// reTrailingLinter extracts golangci-lint's trailing `(lintername)`
// attribution so the finding can carry the concrete rule instead of a
// generic "golangci-lint".
var reTrailingLinter = regexp.MustCompile(`\(([A-Za-z0-9_-]+)\)$`)

// externalLinesToFindings normalizes captured sub-tool output. Lines in
// `file:line[:col]: message` shape become file-scoped findings with the
// given rule; everything else is kept verbatim with rule "external" so
// no output is silently dropped. severity applies to every produced
// finding (the caller knows whether the tool's exit gated the build).
func externalLinesToFindings(output, rule, severity string) []lintJSONFinding {
	var out []lintJSONFinding
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if m := reFileLineCol.FindStringSubmatch(line); m != nil {
			f := lintJSONFinding{
				File:     m[1],
				Line:     parseDigitsLint(m[2]),
				Col:      parseDigitsLint(m[3]),
				Severity: severity,
				Rule:     rule,
				Message:  m[4],
			}
			// golangci-lint suffixes the message with "(lintername)";
			// promote it to the rule field when present.
			if rule == "golangci-lint" {
				if lm := reTrailingLinter.FindStringSubmatch(m[4]); lm != nil {
					f.Rule = lm[1]
				}
			}
			out = append(out, f)
			continue
		}
		out = append(out, lintJSONFinding{
			Severity: severity,
			Rule:     "external",
			Message:  line,
		})
	}
	return out
}

// parseDigitsLint parses a digits-only string (pre-validated by regex);
// returns 0 for empty.
func parseDigitsLint(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

// collectGolangciLintJSON runs golangci-lint with captured output.
// Non-zero exit gates (same as text mode); the captured diagnostics
// become findings at error severity. A clean exit contributes nothing.
func collectGolangciLintJSON(ctx context.Context, paths []string) ([]lintJSONFinding, bool) {
	args := append([]string{"run"}, paths...)
	cmd := exec.CommandContext(ctx, "golangci-lint", args...)
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		fs := externalLinesToFindings(buf.String(), "golangci-lint", lintSevError)
		if len(fs) == 0 {
			fs = []lintJSONFinding{{Severity: lintSevError, Rule: "external", Message: fmt.Sprintf("golangci-lint failed: %v", err)}}
		}
		return fs, true
	}
	return nil, false
}

// Rule ids contributed by the typed-config guardrail lane.
//
// They are SEPARATE ids for the same reason the frontend typecheck lane's
// are: a consumer must be able to tell "the guardrail ran and had nothing to
// say" from "the guardrail never ran", and one shared id forces it to parse
// English out of a free-text message to do that. Both conditions used to
// arrive as rule "typed-config-guardrail" at warning severity, so a machine
// reading `ok` — or filtering by rule — could not distinguish them at all.
const (
	// ruleTypedAccessGuard tags a forbidigo finding the advisory pass
	// actually reported.
	ruleTypedAccessGuard = "typed-config-guardrail"
	// ruleTypedAccessGuardUnavailable tags the pass producing NO verdict:
	// golangci-lint never got far enough to report. Warning by default,
	// error (and gating) under --strict.
	ruleTypedAccessGuardUnavailable = "typed-config-guardrail-unavailable"
)

// collectTypedAccessGuardJSON mirrors runTypedAccessGuardAdvisory with
// captured output. It is the `warn` arm of config.enforce_typed_access:
// forbidigo FINDINGS are surfaced as warnings that never gate.
//
// Run with --issues-exit-code=0, so a non-zero exit cannot mean "found
// something" — it can only mean the invocation never reported. That is not a
// clean lane: it is emitted under ruleTypedAccessGuardUnavailable, and under
// --strict it is an error that flips `ok` to false. The old code returned the
// same rule id at warning severity with gated=false, so `forge lint --json`
// answered "ok": true over a check that never executed.
func collectTypedAccessGuardJSON(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
	args := append([]string{"run", "--enable-only=forbidigo", "--issues-exit-code=0"}, rc.paths...)
	cmd := exec.CommandContext(rc.ctx, "golangci-lint", args...)
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		out := []lintJSONFinding{{
			Severity: unavailableSeverity(rc.strict),
			Rule:     ruleTypedAccessGuardUnavailable,
			Message:  fmt.Sprintf("typed-config guardrail did NOT run: golangci-lint exited %v", err),
			FixHint:  typedAccessGuardUnavailableHint,
		}}
		// Whatever golangci-lint managed to emit is the EVIDENCE for why it
		// could not run ("parallel golangci-lint is running"). This
		// collector used to capture it into buf and then drop it on the
		// floor, leaving the consumer an exit status and no cause.
		out = append(out, externalLinesToFindings(buf.String(), ruleTypedAccessGuardUnavailable, lintSevInfo)...)
		return out, rc.strict, nil
	}
	fs := externalLinesToFindings(buf.String(), ruleTypedAccessGuard, lintSevWarning)
	return fs, false, nil
}

// collectContractLintJSON mirrors runContractLinter: the same
// IN-PROCESS analysis (contract_inprocess.go), with diagnostics mapped
// to findings instead of printed. Diagnostics gate; an analysis failure
// (broken packages, analyzer error) gates with the error preserved as a
// finding.
func collectContractLintJSON(ctx context.Context, paths []string, excludes []string) ([]lintJSONFinding, bool, error) {
	diags, err := runContractAnalysisInProcess(ctx, paths, excludes)
	if err != nil {
		return []lintJSONFinding{{
			Severity: lintSevError,
			Rule:     "contract",
			Message:  fmt.Sprintf("contract linter failed: %v", err),
		}}, true, nil
	}
	if len(diags) == 0 {
		return nil, false, nil
	}
	fs := make([]lintJSONFinding, 0, len(diags))
	for _, d := range diags {
		fs = append(fs, lintJSONFinding{
			File:     d.Pos.Filename,
			Line:     d.Pos.Line,
			Col:      d.Pos.Column,
			Severity: lintSevError,
			Rule:     "contract",
			Message:  fmt.Sprintf("%s (%s)", d.Message, d.Analyzer),
			FixHint:  "either declare the exported method in the contract interface, or unexport it (lowercase) if it's helper-only",
		})
	}
	return fs, true, nil
}

// collectBufLintJSON runs `buf lint` with captured output. Missing
// buf.yaml is a silent no-op, same as text mode.
func collectBufLintJSON(ctx context.Context) ([]lintJSONFinding, bool) {
	if _, err := os.Stat("buf.yaml"); os.IsNotExist(err) {
		return nil, false
	}
	cmd := exec.CommandContext(ctx, "buf", "lint")
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		fs := externalLinesToFindings(buf.String(), "buf", lintSevError)
		if len(fs) == 0 {
			fs = []lintJSONFinding{{Severity: lintSevError, Rule: "buf", Message: fmt.Sprintf("buf lint failed: %v", err)}}
		}
		return fs, true
	}
	return nil, false
}

// ruleFrontendLintUnavailable tags a frontend whose eslint lane could NOT
// run (declared directory missing, or deps not installed). It used to arrive
// as rule "skipped" at info severity — indistinguishable from a lane that
// genuinely did not apply, over a GATING step. Warning by default, error
// (and gating) under --strict, matching ruleFrontendTypecheckUnavailable.
const ruleFrontendLintUnavailable = "forge-frontend-lint-unavailable"

// collectFrontendLintJSON mirrors runFrontendLinters / lintFrontendDir
// but captures npm output instead of streaming it. Failed scripts gate
// (matching text mode); their output is preserved line-by-line via
// externalLinesToFindings. A frontend the lane could not check surfaces
// under ruleFrontendLintUnavailable; a frontend with no lint script (the
// project's own choice) stays an info "skipped".
//
// TypeScript typechecking is collected by collectFrontendTypecheckJSON
// (its own pipeline step), not here.
func collectFrontendLintJSON(rc *lintRunCtx) ([]lintJSONFinding, bool) {
	ctx, cfg := rc.ctx, rc.cfg
	type fe struct{ name, dir, feType string }
	var frontends []fe
	// css_health is a CONFIG setting, so it is read from config — never from
	// which branch discovered the frontends. Reading it inside the
	// `len(cfg.Frontends) > 0` arm silently disabled CSS-health linting for
	// every project whose inventory is DERIVED rather than declared (forge.yaml
	// with no `frontends:` block — control-plane today): the directory scan
	// found the frontend and linted it, with css_health stuck at false and no
	// "skipped" finding to say so. Every other reader of this field
	// (lint.go:1315, generate_ci.go:181) reads it unconditionally.
	cssHealth := false
	if cfg != nil {
		cssHealth = cfg.Lint.Frontend.CSSHealth
	}
	if cfg != nil && len(cfg.Frontends) > 0 {
		for _, f := range cfg.Frontends {
			dir, ok := f.Dir(".")
			if !ok {
				continue
			}
			frontends = append(frontends, fe{name: f.Name, dir: dir, feType: f.Type})
		}
	} else if dirExists("frontends") {
		entries, err := os.ReadDir("frontends")
		if err == nil {
			for _, e := range entries {
				if e.IsDir() {
					frontends = append(frontends, fe{name: e.Name(), dir: filepath.Join("frontends", e.Name())})
				}
			}
		}
	}

	var out []lintJSONFinding
	gated := false
	unavailable := func(msg, hint string) {
		out = append(out, lintJSONFinding{
			Severity: unavailableSeverity(rc.strict),
			Rule:     ruleFrontendLintUnavailable,
			Message:  msg,
			FixHint:  hint,
		})
		if rc.strict {
			gated = true
		}
	}
	for _, f := range frontends {
		if !dirExists(f.dir) {
			unavailable(fmt.Sprintf("%s: directory %s not found — eslint did NOT run", f.name, f.dir),
				fmt.Sprintf("fix the `path:` for frontend %q in forge.yaml, or remove the entry", f.name))
			continue
		}
		pkgJSON := filepath.Join(f.dir, "package.json")
		if _, err := os.Stat(pkgJSON); err != nil {
			// Not a Node project — does not apply, so it contributes nothing.
			continue
		}
		if _, err := os.Stat(filepath.Join(f.dir, "node_modules")); os.IsNotExist(err) {
			unavailable(fmt.Sprintf("%s: node_modules not found in %s — eslint did NOT run", f.name, f.dir),
				fmt.Sprintf("run `npm install` in %s, then re-run `forge lint`", f.dir))
			continue
		}
		scripts, err := readPackageScripts(pkgJSON)
		if err != nil {
			out = append(out, lintJSONFinding{Severity: lintSevError, Rule: "external", Message: fmt.Sprintf("%s: read package.json scripts: %v", f.name, err)})
			gated = true
			continue
		}

		runScript := func(script string) {
			cmd := exec.CommandContext(ctx, "npm", "run", script)
			cmd.Dir = f.dir
			var buf strings.Builder
			cmd.Stdout = &buf
			cmd.Stderr = &buf
			if err := cmd.Run(); err != nil {
				gated = true
				out = append(out, lintJSONFinding{
					Severity: lintSevError,
					Rule:     "external",
					Message:  fmt.Sprintf("%s: npm run %s failed: %v", f.name, script, err),
				})
				out = append(out, externalLinesToFindings(buf.String(), "external", lintSevError)...)
			}
		}

		if hasPackageScript(scripts, "lint") {
			runScript("lint")
		} else {
			out = append(out, skippedFinding(fmt.Sprintf("%s: no npm lint script found, skipping lint", f.name)))
		}
		if cssHealth {
			if hasPackageScript(scripts, "lint:styles") {
				runScript("lint:styles")
			} else {
				out = append(out, skippedFinding(fmt.Sprintf("%s: css_health enabled but no npm lint:styles script found", f.name)))
			}
		}
	}
	return out, gated
}
