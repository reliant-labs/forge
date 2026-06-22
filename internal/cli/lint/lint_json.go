// File: internal/cli/lint_json.go
//
// `forge lint --json` — machine-readable lint output for sub-agents
// and CI, following the same conventions as `forge audit --json` and
// `forge doctor --json` (flag name `--json`, indented json.Encoder to
// stdout, non-zero exit via a sentinel error after the report prints).
//
// Output contract (stable; extensions are additive, same policy as the
// audit-json skill documents for `forge audit --json`):
//
//	{
//	  "findings": [
//	    {
//	      "file": "handlers/api/handlers.go",  // omitted when not file-scoped
//	      "line": 42,                          // 1-based; omitted when unknown
//	      "col": 7,                            // 1-based; omitted when unknown
//	      "severity": "error",                 // "error" | "warning" | "info"
//	      "rule": "forge-wire-coverage",       // rule id; "external" for raw sub-tool lines
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
// tests, banners, check-workarounds, orm-sync, frontend-packs,
// frontend-stores, wire-coverage TODOs) never flip `ok`.
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
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/linter/finding"
	"github.com/reliant-labs/forge/internal/linter/forgeconv"
	"github.com/reliant-labs/forge/internal/linter/frontendpacklint"
	"github.com/reliant-labs/forge/internal/linter/migrationlint"
	"github.com/reliant-labs/forge/internal/linter/scaffolds"
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
// (resolveContractLintBinary, loadProjectConfig warnings) write to
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

	switch {
	case flags.contract, flags.exportedVars:
		if store != nil && !store.Features().ContractsEnabled() {
			return buildLintJSONReport([]lintJSONFinding{skippedFinding("contracts feature is disabled in forge.yaml")}, false), nil
		}
		fs, gated, err := collectContractLintJSON(ctx, paths, contractExcludesFromConfig(cfg))
		if err != nil {
			return nil, err
		}
		return buildLintJSONReport(fs, gated), nil
	case flags.migrationSafety:
		if store != nil && !store.Features().MigrationsEnabled() {
			return buildLintJSONReport([]lintJSONFinding{skippedFinding("migrations feature is disabled in forge.yaml")}, false), nil
		}
		fs, gated, err := collectMigrationSafetyJSON(cfg)
		if err != nil {
			return nil, err
		}
		return buildLintJSONReport(fs, gated), nil
	case flags.conventions:
		fs, gated, err := collectConventionsJSON(forgeconv.LintOptions{Strict: flags.strict})
		if err != nil {
			return nil, err
		}
		return buildLintJSONReport(fs, gated), nil
	case flags.frontendPacks:
		fs, err := collectFrontendPacksJSON()
		if err != nil {
			return nil, err
		}
		return buildLintJSONReport(fs, false), nil
	case flags.frontendStores:
		fs, err := collectFrontendStoresJSON(cwd)
		if err != nil {
			return nil, err
		}
		return buildLintJSONReport(fs, false), nil
	case flags.scaffolds:
		fs, gated, err := collectScaffoldsJSON(cwd)
		if err != nil {
			return nil, err
		}
		return buildLintJSONReport(fs, gated), nil
	case flags.tests:
		fs, err := collectTestsJSON(cwd)
		if err != nil {
			return nil, err
		}
		return buildLintJSONReport(fs, false), nil
	case flags.banners:
		fs, err := collectBannersJSON(cwd)
		if err != nil {
			return nil, err
		}
		return buildLintJSONReport(fs, false), nil
	case flags.wireCoverage:
		fs, gated, err := collectWireCoverageJSON(cwd)
		if err != nil {
			return nil, err
		}
		return buildLintJSONReport(fs, gated), nil
	case flags.bootstrapCoverage:
		fs, gated, err := collectBootstrapCoverageJSON(cwd)
		if err != nil {
			return nil, err
		}
		return buildLintJSONReport(fs, gated), nil
	case flags.checkWorkarounds:
		fs, err := collectWorkaroundsJSON(cwd)
		if err != nil {
			return nil, err
		}
		return buildLintJSONReport(fs, false), nil
	case flags.optionalDepsGuard:
		fs, err := collectOptionalDepsGuardJSON(cwd)
		if err != nil {
			return nil, err
		}
		return buildLintJSONReport(fs, false), nil
	case flags.configDeps:
		fs, err := collectConfigDepsJSON(cwd)
		if err != nil {
			return nil, err
		}
		return buildLintJSONReport(fs, false), nil
	}

	return collectAllLintersJSON(ctx, flags.strict, paths, cfg, cwd)
}

// collectAllLintersJSON mirrors runAllLinters step-for-step. Each step
// contributes findings; `gated` flips on exactly the conditions that
// set hasFailed in text mode. Per-step collection errors (I/O etc.)
// degrade to an error-severity finding rather than aborting the sweep
// — matching text mode, which prints the failure and keeps walking
// only for the advisory linters but hard-fails the run for the gating
// ones via hasFailed.
func collectAllLintersJSON(ctx context.Context, strict bool, paths []string, cfg *config.ProjectConfig, cwd string) (*lintJSONReport, error) {
	rc := &lintRunCtx{ctx: ctx, fix: false, strict: strict, paths: paths, cfg: cfg, cwd: cwd}

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
// scaffolds, migrationlint, frontendpacklint) onto the lint --json
// contract. It replaces the four near-identical per-package mappers that
// existed before the finding package was introduced.
//
// Field mapping rules, unified:
//   - Severity passes through directly: the canonical finding severities
//     ("error"/"warning"/"info") ARE the JSON contract values, so no
//     normalization shim is needed.
//   - File comes from f.File, falling back to f.Path for whole-file
//     (line-less) scaffold findings — exactly one of the two is ever set.
//   - Remediation becomes fix_hint (forgeconv's actionable hints).
//
// Pack/Import are linter-internal context that the JSON contract folds
// into Message at emit time, so they are not projected as separate
// fields here (preserving the historical frontendpacklint JSON shape,
// which never exposed them either).
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
		out[i].FixHint = "either rewrite the destructive migration as a non-destructive sequence, or allowlist the file under migration_safety.allowed_destructive in forge.yaml"
	}
	return out, result.HasErrors(), nil
}

func collectFrontendPacksJSON() ([]lintJSONFinding, error) {
	packsRoot := filepath.Join("internal", "packs")
	if _, err := os.Stat(packsRoot); os.IsNotExist(err) {
		return []lintJSONFinding{skippedFinding("No internal/packs/ directory found — skipping frontend pack lint")}, nil
	}
	res, err := frontendpacklint.LintPacksRoot(packsRoot)
	if err != nil {
		return nil, fmt.Errorf("frontend pack lint failed: %w", err)
	}
	return findingsToJSON(res.Findings), nil
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

func collectTestsJSON(cwd string) ([]lintJSONFinding, error) {
	handlerRes, err := forgeconv.LintHandlerTests(cwd)
	if err != nil {
		return nil, fmt.Errorf("handler-test lint failed: %w", err)
	}
	frontendRes, err := forgeconv.LintFrontendHookTests(cwd)
	if err != nil {
		return nil, fmt.Errorf("frontend-hook-test lint failed: %w", err)
	}
	out := findingsToJSON(handlerRes.Findings)
	out = append(out, findingsToJSON(frontendRes.Findings)...)
	return out, nil
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

// collectWireCoverageJSON mirrors runWireCoverageLint: TODO markers are
// warnings, unresolved forge:placeholder annotations are errors and
// gate the build.
func collectWireCoverageJSON(projectDir string) ([]lintJSONFinding, bool, error) {
	var out []lintJSONFinding

	path := filepath.Join(projectDir, "pkg", "app", "wire_gen.go")
	if f, err := os.Open(path); err == nil {
		got, scanErr := scanWireGen(f, path, projectDir)
		_ = f.Close()
		if scanErr != nil {
			return nil, false, fmt.Errorf("scan %s: %w", path, scanErr)
		}
		for _, w := range got {
			msg := fmt.Sprintf("%s is unresolved — wire_gen emitted a typed-zero placeholder", w.Field)
			if w.Function != "" {
				msg = fmt.Sprintf("%s in %s is unresolved — wire_gen emitted a typed-zero placeholder", w.Field, w.Function)
			}
			out = append(out, lintJSONFinding{
				File:     w.File,
				Line:     w.Line,
				Severity: lintSevWarning,
				Rule:     "forge-wire-coverage",
				Message:  msg,
				FixHint:  fmt.Sprintf("add `%s <Type>` to AppExtras in pkg/app/app_extras.go and assign in setup.go, OR mark the field `// forge:optional-dep` if it's intentionally optional", w.Field),
			})
		}
	} else if !os.IsNotExist(err) {
		return nil, false, fmt.Errorf("open %s: %w", path, err)
	}

	placeholders, err := scanUnresolvedPlaceholders(projectDir)
	if err != nil {
		return nil, false, fmt.Errorf("scan placeholders: %w", err)
	}
	for _, p := range placeholders {
		out = append(out, lintJSONFinding{
			File:     filepath.Join("pkg", "app", "app_extras.go"),
			Severity: lintSevError,
			Rule:     "forge-wire-coverage",
			Message:  fmt.Sprintf("%s carries `forge:placeholder: %s` but is still typed `%s`", p.FieldName, p.TargetType, p.CurrentType),
			FixHint:  fmt.Sprintf("tighten the declaration in app_extras.go from `%s %s` to `%s %s`, then re-run `forge generate`", p.FieldName, p.CurrentType, p.FieldName, p.TargetType),
		})
	}
	return out, len(placeholders) > 0, nil
}

func collectBootstrapCoverageJSON(projectDir string) ([]lintJSONFinding, bool, error) {
	gaps, skipReason, err := collectBootstrapCoverageFindings(projectDir)
	if err != nil {
		return nil, false, err
	}
	if skipReason != "" {
		return []lintJSONFinding{skippedFinding("bootstrap-deps-coverage: " + skipReason)}, false, nil
	}
	out := make([]lintJSONFinding, 0, len(gaps))
	for _, g := range gaps {
		out = append(out, lintJSONFinding{
			File:     filepath.Join("internal", g.Package, "contract.go"),
			Severity: lintSevError,
			Rule:     "forge-bootstrap-deps-coverage",
			Message:  fmt.Sprintf("%s matches AppExtras.%s by name but the types diverge (Deps.%s = %s, AppExtras.%s = %s) — bootstrap silently drops the wire and the feature no-ops at runtime", g.Field, g.Field, g.Field, g.DepsType, g.Field, g.AppType),
			FixHint:  fmt.Sprintf("align AppExtras.%s to %s, OR re-construct %s.New(%s.Deps{%s: ...}) in pkg/app/setup.go", g.Field, g.DepsType, g.Package, g.Package, g.Field),
		})
	}
	return out, len(gaps) > 0, nil
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

// collectTypedAccessGuardJSON mirrors runTypedAccessGuardAdvisory with
// captured output. It is the `warn` arm of config.enforce_typed_access:
// forbidigo findings are surfaced as WARNINGS that never gate (the bool
// return is always false). Run with --issues-exit-code=0 so a non-zero exit
// only signals a genuine tool error, which degrades to a single warning
// finding rather than gating.
func collectTypedAccessGuardJSON(ctx context.Context, paths []string) ([]lintJSONFinding, bool, error) {
	args := append([]string{"run", "--enable-only=forbidigo", "--issues-exit-code=0"}, paths...)
	cmd := exec.CommandContext(ctx, "golangci-lint", args...)
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		// Tool error (not findings — those are neutralized by
		// --issues-exit-code=0). Degrade to a single advisory finding.
		return []lintJSONFinding{{
			Severity: lintSevWarning,
			Rule:     "typed-config-guardrail",
			Message:  fmt.Sprintf("typed-config guardrail check could not run: %v", err),
		}}, false, nil
	}
	fs := externalLinesToFindings(buf.String(), "typed-config-guardrail", lintSevWarning)
	return fs, false, nil
}

// collectContractLintJSON mirrors runContractLinter with captured
// output. Exit code 3 is the analyzer's "violations found" signal —
// the diagnostics are parsed into findings and the run gates. Any
// other failure gates with the raw output preserved.
func collectContractLintJSON(ctx context.Context, paths []string, excludes []string) ([]lintJSONFinding, bool, error) {
	binPath, err := resolveContractLintBinary(ctx)
	if err != nil {
		return nil, false, err
	}

	var lintArgs []string
	if len(excludes) > 0 {
		lintArgs = append(lintArgs, "-exclude="+strings.Join(excludes, ","))
	}
	lintArgs = append(lintArgs, paths...)

	var lintExec *exec.Cmd
	if strings.HasSuffix(binPath, "main.go") {
		goArgs := append([]string{"run", binPath}, lintArgs...)
		lintExec = exec.CommandContext(ctx, "go", goArgs...)
	} else {
		lintExec = exec.CommandContext(ctx, binPath, lintArgs...)
	}

	// Same env discipline as the text path — see runContractLinter for
	// the GOWORK / GOFLAGS rationale.
	lintExec.Env = os.Environ()
	if !hasWorkspaceGoMod() {
		lintExec.Env = appendEnvIfUnset(lintExec.Env, "GOWORK", "off")
		lintExec.Env = appendEnvIfUnset(lintExec.Env, "GOFLAGS", "-mod=mod")
	}
	lintExec.Env = ensureEnvDefault(lintExec.Env, "GOPROXY", "https://proxy.golang.org,direct")

	var buf strings.Builder
	lintExec.Stdout = &buf
	lintExec.Stderr = &buf

	if err := lintExec.Run(); err != nil {
		fs := externalLinesToFindings(buf.String(), "contract", lintSevError)
		if len(fs) == 0 {
			fs = []lintJSONFinding{{Severity: lintSevError, Rule: "contract", Message: fmt.Sprintf("contract linter failed: %v", err)}}
		}
		for i := range fs {
			if fs[i].FixHint == "" {
				fs[i].FixHint = "either declare the exported method in the contract interface, or unexport it (lowercase) if it's helper-only"
			}
		}
		return fs, true, nil
	}
	return nil, false, nil
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

// collectFrontendLintJSON mirrors runFrontendLinters / lintFrontendDir
// but captures npm output instead of streaming it. Failed scripts gate
// (matching text mode); their output is preserved line-by-line via
// externalLinesToFindings. Skips (missing dir / node_modules / script)
// surface as info findings.
func collectFrontendLintJSON(ctx context.Context, cfg *config.ProjectConfig) ([]lintJSONFinding, bool) {
	type fe struct{ name, dir, feType string }
	var frontends []fe
	cssHealth := false
	if cfg != nil && len(cfg.Frontends) > 0 {
		cssHealth = cfg.Lint.Frontend.CSSHealth
		for _, f := range cfg.Frontends {
			dir := f.Path
			if dir == "" {
				dir = filepath.Join("frontends", f.Name)
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
	for _, f := range frontends {
		if !dirExists(f.dir) {
			out = append(out, skippedFinding(fmt.Sprintf("%s: directory %s not found, skipping", f.name, f.dir)))
			continue
		}
		pkgJSON := filepath.Join(f.dir, "package.json")
		if _, err := os.Stat(pkgJSON); err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(f.dir, "node_modules")); os.IsNotExist(err) {
			out = append(out, skippedFinding(fmt.Sprintf("%s: node_modules not found — run 'npm install' in %s", f.name, f.dir)))
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
		if hasPackageScript(scripts, "typecheck") {
			runScript("typecheck")
		} else if _, err := os.Stat(filepath.Join(f.dir, "tsconfig.json")); err == nil {
			out = append(out, skippedFinding(fmt.Sprintf("%s: no npm typecheck script found; add `typecheck`: `tsc --noEmit`", f.name)))
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
