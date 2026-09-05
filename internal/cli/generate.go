package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/contractcheck"
	"github.com/reliant-labs/forge/internal/generator"
)

// generateMu protects the generation pipeline from concurrent runs.
// It is legitimately package-level shared state used by generate, add, and new commands.
var generateMu sync.Mutex

func newGenerateCmd() *cobra.Command {
	var (
		watch           bool
		force           bool
		explain         bool
		explainDrift    bool
		skipValidate    bool
		skipPreChecks   bool
		skipConfigCheck bool
		checkOnly       bool
		forceCleanup    bool
		allowKCLDown    bool
		templatesOnly   bool
		strict          bool
		verbose         bool
		planOnly        bool
		heal            bool
		noRevert        bool
		steps           string
	)

	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Generate code from proto files",
		Long: `Generate code from proto files based on project configuration or directory conventions.

When forge.yaml exists, generation is driven by the config:
  - buf generate for Go stubs (protoc-gen-go + protoc-gen-connect-go)
  - protoc-gen-forge for entity protos in proto/db/
  - buf generate for TypeScript stubs for Next.js frontends
  - Service stubs and mocks for new services
  - pkg/app/bootstrap.go with explicit service bootstrapping
  - sqlc generate if sqlc.yaml exists
  - go mod tidy in gen/

Without forge.yaml, falls back to directory convention scanning:
  proto/           - Root proto directory (for buf generate)
  proto/services/  - Service definitions (stubs + mocks)
  proto/api/       - API messages
  proto/db/        - Database models (protoc-gen-forge)

Examples:
  forge generate            # Generate all code
  forge generate --watch    # Watch mode for development
  forge generate --force    # Discard hand-edits to Tier-1 files and regenerate
  forge generate --check    # Run generate into a tmpdir; exit 1 if it would change the tree (CI guard)
  forge generate --explain  # Print per-file provenance log after generate
  forge generate --verbose  # Print one line per gate-off skipped step

Additional maintainer/debug flags exist (pipeline narrowing, drift
forensics, parallel-lane and migration escape hatches); run
'forge generate --help-dev' to list them.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if checkOnly {
				return runGenerateCheck()
			}
			if planOnly {
				return runGeneratePlan(".", pipelineFlags{
					Force:             force,
					SkipValidate:      skipValidate,
					SkipPreChecks:     skipPreChecks,
					SkipConfigCheck:   skipConfigCheck,
					ForceCleanup:      forceCleanup,
					AllowKCLDowngrade: allowKCLDown,
					TemplatesOnly:     templatesOnly,
					Strict:            strict,
					Verbose:           verbose,
					Steps:             steps,
				})
			}
			// Capture pre-pipeline body hashes (from the embedded
			// markers) so --explain can diff against post-pipeline
			// state to label rewritten vs idempotent.
			var preChecksums map[string]string
			if explain {
				pre := checksums.ScanMarkers(".")
				preChecksums = make(map[string]string, len(pre))
				for k, v := range pre {
					preChecksums[k] = v.Body
				}
			}

			generateMu.Lock()
			err := runGeneratePipelineFlags(".", pipelineFlags{
				Force:             force,
				ExplainDrift:      explainDrift,
				SkipValidate:      skipValidate,
				SkipPreChecks:     skipPreChecks,
				SkipConfigCheck:   skipConfigCheck,
				ForceCleanup:      forceCleanup,
				AllowKCLDowngrade: allowKCLDown,
				TemplatesOnly:     templatesOnly,
				Strict:            strict,
				Verbose:           verbose,
				Heal:              heal,
				NoRevert:          noRevert,
				Steps:             steps,
			})
			generateMu.Unlock()

			// Print the explain log even when the pipeline failed — partial
			// provenance is still useful for diagnosing what got generated
			// before the build break. The original error is returned below.
			//
			// Honor --strict: a failed explain render under strict promotes
			// to a fatal error (consistent with the rest of the pipeline's
			// loud-by-default thesis). Outside strict it stays a soft warn
			// so an explain-log bug doesn't mask a successful generate.
			if explain {
				if explainErr := printExplainLog(".", preChecksums); explainErr != nil {
					if strict {
						return fmt.Errorf("explain log failed: %w (--strict)", explainErr)
					}
					fmt.Fprintf(os.Stderr, "⚠️  Warning: explain log failed: %v — pass --strict to fail on this\n", explainErr)
				}
			}

			if err != nil {
				return err
			}

			if watch {
				fmt.Println("\n👀 Watching for changes... (Press Ctrl+C to stop)")
				return watchForChanges()
			}

			return nil
		},
	}

	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "Watch for changes and regenerate")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Discard hand-edits to Tier-1 files and regenerate from current templates")
	cmd.Flags().BoolVar(&explain, "explain", false, "Print a per-file provenance log after generate")
	cmd.Flags().BoolVar(&explainDrift, "explain-drift", false, "On Tier-1 drift, run the pipeline with drifted files redirected to .forge/render/ side renders, print a bounded diff of on-disk vs fresh render per file, then fail with the drift report (explains; never overwrites or approves)")
	cmd.Flags().BoolVar(&skipValidate, "skip-validate", false, "Skip the final 'go build ./...' validate step (useful during multi-lane migrations when the tree is in a partial-build state)")
	cmd.Flags().BoolVar(&skipPreChecks, "skip-pre-checks", false, "Bypass the pre-codegen contract-shape check (useful when a parallel lane's contract violation would otherwise block regen of this lane)")
	cmd.Flags().BoolVar(&checkOnly, "check", false, "Run generate into a tmpdir and diff against the current tree; exit 1 on drift (for CI guards)")
	cmd.Flags().BoolVar(&forceCleanup, "force-cleanup", false, "Actually delete stale generated files. Default is report-only: print which files WOULD be deleted and leave them in place.")
	cmd.Flags().BoolVar(&allowKCLDown, "allow-kcl-downgrade", false, "Permit this forge to overwrite .forge-kcl/ with its embedded KCL module even when a NEWER forge vendored the copy on disk. Off by default: a version-blind overwrite once replaced control-plane's vendored schema with a stale one and broke prod's 'env render'. Pass this only when rolling forge back deliberately.")
	cmd.Flags().BoolVar(&templatesOnly, "templates-only", false, "Re-render template-driven files only. Skips cleanup sweep, drift-guard, and validation. Use when a template change needs to propagate to a project that has uncommitted WIP and can't tolerate a full regen.")
	cmd.Flags().StringVar(&steps, "steps", "", "Narrow the pipeline to a named step preset. Valid values: \"bootstrap-only\" (used internally by 'forge scaffold worker'), \"mocks\" (regen both kinds of mock_gen.go — contract-derived and service-derived — after a contract.go or proto change; skips the Tier-1 drift guard since mocks cannot stomp Tier-1 files).")
	// Loud-by-default architecture flags. See the per-flag fields on
	// pipelineFlags for the rationale; runGeneratePlan + warnOrFail consume them.
	cmd.Flags().BoolVar(&strict, "strict", false, "Promote pipeline warnings to fatal errors. Every 'Warning: ... failed' site that today logs and continues will abort the pipeline instead.")
	// -v works here. It used to be swallowed by a root PersistentFlag bound
	// to a variable nobody read, so `forge generate -v` parsed cleanly and
	// changed nothing — the long form was declared without a shorthand
	// precisely to dodge that collision. The root flag is gone; the
	// shorthand belongs to the command that actually consumes it.
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Print one line per gate-off skipped step ('⏩ skipped: <step name> (<reason>)'). Default is silent skip.")
	cmd.Flags().BoolVar(&planOnly, "plan", false, "Print the pipeline plan ([RUN]/[SKIP] annotation per step + gate reason) and exit 0 without running any step. Honors --steps and --templates-only.")
	// --dry-run is the spelling every other forge verb uses for "show me,
	// don't do it" (`forge scaffold`, `disown`, `secrets`, `project upgrade`).
	// Without it, the one command people most want to preview answers
	// `unknown flag` and sends them to --help to learn that this particular
	// verb spells it differently.
	//
	// The description deliberately does NOT name the flag it shares a
	// variable with: that one is hidden from --help (see help_dev.go), and
	// naming it here would leak it back into the visible surface.
	cmd.Flags().BoolVar(&planOnly, "dry-run", false, "Print the pipeline plan ([RUN]/[SKIP] per step + gate reason) and exit without running any step.")
	cmd.Flags().BoolVar(&skipConfigCheck, "skip-config-check", false, "Bypass the forge.yaml ↔ filesystem cross-check (declared services/frontends/packages must have on-disk backing). Use for parallel-lane / mid-migration scenarios.")
	cmd.Flags().BoolVar(&heal, "heal", false, "Overwrite on-disk content that matches a PRIOR forge render (an older vintage) with the current template. OFF by default: such content is byte-indistinguishable from a deliberate edit, so forge leaves it untouched and tells you how to proceed rather than silently reverting your work. Pass --heal to advance every such file to the current templates.")
	cmd.Flags().BoolVar(&noRevert, "no-revert", false, "Diagnostic mode: on a post-write step failure (most often the final 'go build' validate), leave the generated files ON DISK instead of rewinding the tree, so you can inspect the codegen output that failed to build. Default (off) reverts to the clean pre-run tree.")

	// User-vs-maintainer surface split: the flags below are fully
	// functional but hidden from --help (visible via --help-dev). The
	// visible set is pinned by TestGenerateHelpSurface — a new flag must
	// consciously pick a side. See help_dev.go for the rule of thumb.
	hideDevFlags(cmd,
		"explain-drift",     // drift forensics (debugging the drift guard)
		"skip-validate",     // multi-lane migration escape hatch
		"skip-pre-checks",   // parallel-lane escape hatch
		"force-cleanup",     // destructive cleanup of stale generated files
		"templates-only",    // forge-template-development fast path
		"steps",             // pipeline narrowing (internal/agent fast paths)
		"strict",            // pipeline-hardening mode for forge CI/dev
		"plan",              // pipeline introspection (debugging gates)
		"skip-config-check", // parallel-lane / mid-migration escape hatch
		"no-revert",         // codegen forensics (inspect the output that failed to build)
	)

	return cmd
}

// runGeneratePipeline executes the unified generation pipeline.
//
// Pre-2026-05-06, this was a 584-line procedural function with 25
// numbered ordered steps. As of 2026-05-07 it is a flat loop over the
// typed []GenStep plan defined in generate_pipeline.go — every legacy
// step is now its own GenStep entry with a dedicated stepXxx body.
//
// projectDir is the root of the project (contains go.mod, proto/, etc.).
// The caller must hold generateMu.
func runGeneratePipeline(projectDir string, force bool) error {
	return runGeneratePipelineOpts(projectDir, force, false)
}

// runGeneratePipelineOpts is the variant that lets the caller pass
// additional pipeline flags (currently --skip-validate). Wrapping the
// shorter signature keeps test fixtures (and any out-of-tree callers)
// source-compatible.
func runGeneratePipelineOpts(projectDir string, force, skipValidate bool) error {
	return runGeneratePipelineFlags(projectDir, pipelineFlags{
		Force:        force,
		SkipValidate: skipValidate,
	})
}

// pipelineFlags is the typed bag of opt-in toggles for the generate
// pipeline. Grew out of the per-flag positional-arg signatures
// (runGeneratePipeline, runGeneratePipelineOpts) once the flag count
// crossed three — adding a fourth (--skip-pre-checks) without a struct
// would have meant churning every caller of the positional form.
type pipelineFlags struct {
	Force        bool
	SkipValidate bool

	SkipPreChecks bool
	// SkipConfigCheck opts out of the forge.yaml ↔ filesystem cross-
	// check that stepLoadConfig runs after a successful load. The default
	// is loud-by-default: declared services / frontends / packages with
	// no on-disk backing (or vice versa) abort the pipeline at load time
	// with a batched report pointing at both sides of the asymmetry.
	// Opt-out exists for parallel-lane / mid-migration scenarios where a
	// transient mismatch is expected.
	SkipConfigCheck bool

	// ExplainDrift turns a Tier-1 drift abort into a diagnostic run:
	// drifted paths render to .forge/render/ side files, the run prints
	// a bounded on-disk-vs-fresh-render diff per file, and then still
	// fails with the drift report. See generate_explain_drift.go.
	ExplainDrift bool
	// Steps names a step preset that narrows the set of pipeline steps
	// the runner executes. The empty string runs the full pipeline (the
	// historical default). The "bootstrap-only" value runs JUST the
	// load/parse/bootstrap/validate subset — used by `forge scaffold worker`
	// so adding a single worker doesn't trigger a full project regen
	// that stomps unrelated Tier-1 files (.github/workflows/ci.yml,
	// cmd/server.go, frontend mocks, pkg/config/config.go). The step
	// preset allowlist lives in stepPresetAllowlist
	// (generate_pipeline.go).
	//
	// (Field previously named Scope; renamed because "scope" was
	// overloaded with the file-ownership concept in
	// internal/checksums/inspector.go. The CLI flag is --steps.)
	//
	// FRICTION 2026-06-03: cp-forge port-workers ran `forge scaffold worker`
	// 7× and watched regen rewrite 5 unrelated Tier-1 files per call.
	// Composes with the existing tier1OwnerRegistry scoping in
	// generate_tier1_scope.go — both narrow what `forge scaffold worker`
	// touches, just at different layers (drift-guard vs step execution).
	Steps string

	// ForceCleanup opts in to the destructive stale-artifact sweep.
	// Default (false) makes stepCleanupStale report-only: it prints
	// which manifest-recorded files would be deleted but leaves them
	// on disk. See the matching pipelineContext.ForceCleanup field for
	// the cp-forge surprise-delete rationale.
	ForceCleanup bool

	// AllowKCLDowngrade lets `forge generate` overwrite `.forge-kcl/`
	// with an OLDER forge's embedded KCL module. Off by default, and the
	// default is the whole point: the silent version-blind overwrite is
	// what put an outdated Gateway listener rule into control-plane's
	// vendored schema and broke prod's `env render`, with nothing in the
	// generate output to suggest a downgrade had happened. On for the
	// case where the rollback is the intent — pinning an older forge
	// deliberately and wanting its KCL to match.
	AllowKCLDowngrade bool

	// TemplatesOnly restricts the pipeline to template-driven render
	// steps only. Skips the Tier-1 drift guard, the validation tail
	// (pre-codegen contract check, post-gen warnings, `go build`), the
	// stale-artifact cleanup sweep, and every external generator
	// (buf/protoc/sqlc/goimports/go mod tidy/KCL).
	//
	// Use case: a forge template change (e.g. `bootstrap.go.tmpl` gets a
	// louder warning) needs to land in a downstream project that has
	// uncommitted WIP, so a full `forge generate` would either trip the
	// drift guard or shell out to tooling the partial tree can't build.
	// `--templates-only` re-renders just the files the changed template
	// emits, leaving the cleanup/drift/validate machinery for the next
	// full regen once the WIP settles.
	//
	// Composes with Steps: when both are set, only steps that pass BOTH
	// allowlists run (intersection). When Steps is empty and
	// TemplatesOnly is set, every template-driven step runs. The
	// allowlist of template-driven step names lives in
	// templatesOnlyStepAllow (generate_pipeline.go).
	TemplatesOnly bool

	// Strict promotes the historically-silent "Warning: ... failed"
	// sites into hard pipeline-abort errors. Used by the warnOrFail
	// helper on pipelineContext — steps that opt into the helper get
	// strict semantics for free without per-site code changes once they
	// adopt it. The default (false) preserves the historical lenient
	// behavior so existing CI / local-iteration scripts don't suddenly
	// fail on a goimports glitch or a missing protoc-gen-connect-openapi.
	Strict bool

	// Verbose toggles per-step skip messages. The generate pipeline runs
	// dozens of steps, most of which are gated off by project-shape
	// predicates (no frontends → skip all frontend steps). The default
	// (false) is silent skip — the user only sees output from steps that
	// actually ran. When true, every gated-off step prints one line:
	// "⏩ skipped: <step name> (gate: <gate name>)". Diagnostic surface
	// for "why didn't generate touch X?" questions without requiring
	// --plan.
	Verbose bool

	// NoRevert (--no-revert) is a codegen-forensics escape hatch. By default
	// a post-write step failure — most importantly the final `go build`
	// validate — rewinds the tree to its clean pre-run state (the rollback
	// journal armed by BeginRollbackJournal), so the user is never left with a
	// mid-regen tree to `git checkout`. That same rewind, though, DELETES the
	// generated output that failed to build — exactly what a forge/codegen
	// developer needs to read to diagnose the bug. With NoRevert set, the
	// journal is dropped (CommitRollback) instead of restored on failure, so
	// the generated artifacts stay on disk for inspection; the run still exits
	// non-zero with the underlying error. Off by default (the safe,
	// clean-tree behavior).
	NoRevert bool

	// Heal (--heal) opts IN to overwriting on-disk content that matches a
	// PRIOR forge render (an older vintage, but not the latest) with the
	// current template. It is OFF by default, and that default is the
	// correctness fix for FRICTION cp-forge fr-2c1c2328c7: such content is
	// byte-indistinguishable from a deliberate user revert/edit (a
	// hand-edit to pkg/app/bootstrap.go hash-equaled a prior render and was
	// silently reverted). With Heal off, writers SKIP these files and
	// NoHealSkipFn names them plus the remedies; forge never silently
	// destroys the bytes. With Heal on, the whole tree advances to the
	// current templates (loudly — checksums.HealNoticeFn still fires per
	// file). `forge generate --force` is the per-file equivalent.
	Heal bool
}

// runGeneratePipelineFlags is the canonical entrypoint. Both the
// shorter runGeneratePipeline (force) and runGeneratePipelineOpts
// (+ skipValidate) call through here. New flags land on pipelineFlags.
func runGeneratePipelineFlags(projectDir string, flags pipelineFlags) error {
	// Cross-process file lock (complements the in-process generateMu).
	// Held for the lifetime of the pipeline so a parallel `forge scaffold`
	// can't race a long `forge generate`.
	release, err := acquireGenerateLock(projectDir)
	if err != nil {
		return err
	}
	defer release()

	ctx, err := newPipelineContextWithFlags(projectDir, flags)
	if err != nil {
		return err
	}

	if flags.SkipValidate {
		fmt.Println("⏩ --skip-validate: final 'go build ./...' step will be skipped")
	}
	if flags.SkipPreChecks {
		fmt.Println("⚠️  pre-codegen contract check skipped via --skip-pre-checks")
	}

	// Per-run side-render redirect tracking (--explain-drift), the
	// heal-notice dedupe set, and the heal opt-in start empty/off on
	// every invocation (AutoHeal defaults OFF — the non-destructive
	// behavior).
	checksums.ResetPerRunState()
	if flags.Heal {
		fmt.Println("♻️  --heal: on-disk content matching a PRIOR forge render will be overwritten with the current template")
		checksums.AutoHeal = true
	}

	// Arm the stage-then-validate rollback journal (fr-40f7ec9bd9). From
	// here on, every forge write captures its target's pre-run bytes so a
	// post-write failure — most importantly the final `go build` validate
	// step — can rewind the tree to its clean pre-regen state instead of
	// leaving it mid-regen for the user to `git checkout`. rolledBack is
	// set by the deferred outcome handler below; it gates SaveChecksums so
	// we never persist manifest state describing writes we just undid.
	checksums.BeginRollbackJournal(ctx.AbsPath)
	rolledBack := false

	// Save checksums on exit, even on partial failures: a step that
	// successfully wrote files should have those tracked so the user's
	// next `forge project audit` doesn't false-flag user-edited drift. EXCEPTION:
	// a rolled-back run restored the tree to its pre-run state, so saving
	// the post-write manifest would describe files that no longer exist on
	// disk — skip it and let the pre-run state files stand.
	defer func() {
		if ctx.Checksums == nil {
			return
		}
		if rolledBack {
			return
		}
		if saveErr := generator.SaveChecksums(ctx.AbsPath, ctx.Checksums); saveErr != nil {
			log.Printf("Warning: failed to save checksums: %v", saveErr)
		}
	}()

	// Step-preset filter — when flags.Steps is non-empty, drop steps not
	// in the allowlist BEFORE the Gate check. The gate is a project-shape
	// predicate ("does this project have services?"); the step preset is
	// a caller-intent predicate ("am I doing a bootstrap-only regen?").
	// They compose: a step that's allowlisted by the preset still has to
	// pass its Gate, and a step gated off would skip regardless of the
	// preset.
	steps := generateSteps()
	totalSteps := len(steps)
	if flags.Steps != "" {
		allow, ok := stepPresetAllowlist[flags.Steps]
		if !ok {
			return fmt.Errorf("unknown pipeline step preset %q (valid: %s)", flags.Steps, knownStepPresetNames())
		}
		filtered := steps[:0:0]
		for _, step := range steps {
			if allow[step.Name] {
				filtered = append(filtered, step)
			}
		}
		fmt.Printf("⏩ steps=%s: running %d of %d pipeline steps\n", flags.Steps, len(filtered), len(steps))
		steps = filtered
	}

	// --templates-only filter — intersects with --steps (if both are
	// set, a step must pass BOTH allowlists). Applied AFTER --steps so
	// the user-visible "running N of M" count reflects whichever filter
	// is narrower. See templatesOnlyStepAllow for the included set and
	// the WIP-tree rationale.
	if flags.TemplatesOnly {
		before := len(steps)
		filtered := steps[:0:0]
		for _, step := range steps {
			if templatesOnlyStepAllow[step.Name] {
				filtered = append(filtered, step)
			}
		}
		fmt.Printf("⏩ --templates-only: running %d of %d pipeline steps (skipping cleanup, drift-guard, validation, external generators)\n", len(filtered), before)
		steps = filtered
	}

	for _, step := range steps {
		if !step.Gate(ctx) {
			// Verbose mode prints one line per gate-off skip so the user
			// can answer "why didn't generate touch X?" without --plan.
			// Default (silent) preserves the historical low-noise output.
			if ctx.Verbose {
				fmt.Fprintf(os.Stderr, "⏩ skipped: %s (%s)\n", step.Name, gateSkipReason(step))
			}
			continue
		}
		if err := step.Run(ctx); err != nil {
			// --explain-drift cleanup still runs on a mid-pipeline
			// failure: whatever renders were parked are diffed, and the
			// snapshot restore keeps the deferred SaveChecksums honest.
			// The step error wins over the drift error.
			if expErr := finishExplainDrift(ctx); expErr != nil {
				fmt.Fprintf(os.Stderr, "%v\n", expErr)
			}
			// Same for the legacy-migration quarantine: stamp the
			// unverified sentinels now so the next run's guard still
			// names the unresolved files. Done BEFORE the rollback so the
			// sentinel stamps survive — they describe the user's pre-run
			// drifted files, not this run's reverted output.
			if migErr := finishLegacyMigration(ctx); migErr != nil {
				fmt.Fprintf(os.Stderr, "%v\n", migErr)
			}
			// Stage-then-validate rollback: a step failed AFTER writes
			// landed (the classic case: the final `go build` validate),
			// so the tree is mid-regen. Rewind every forge-written file to
			// its pre-run state and tell the user the tree is clean rather
			// than leaving them a `git checkout` to find (fr-40f7ec9bd9).
			//
			// --no-revert opts OUT: a forge/codegen developer debugging a
			// generator bug needs to READ the output that failed to build,
			// which the rewind would delete. Keep the artifacts on disk (drop
			// the journal instead of restoring it) and say so; the run still
			// fails with the underlying error.
			if flags.NoRevert {
				checksums.CommitRollback()
				fmt.Fprintln(os.Stderr, "\n🔎 --no-revert: leaving this run's generated files on disk for inspection (the tree was NOT rewound). Re-run without --no-revert to restore the clean pre-run tree.")
				// The compiler output scrolls away behind the pipeline's
				// own tail output — repeat it here so the last thing on
				// screen is the thing to fix (same rationale as the
				// rollback branch).
				reprintCompilerOutput(err, "")
			} else {
				rolledBack = rollbackGeneratedTree(ctx.AbsPath, err)
			}
			return fmt.Errorf("step %q: %w", step.Name, err)
		}
	}

	// --explain-drift: print the per-file diffs and fail with the drift
	// report — the flag explains the drift, it never approves it. No-op
	// nil when the guard found no drift (or the flag wasn't set).
	if err := finishExplainDrift(ctx); err != nil {
		// Deliberate end-state, not a mid-regen tree: --explain-drift
		// redirected its writes to side renders and never touched the
		// drifted files, so the journal's real-file captures stand.
		checksums.CommitRollback()
		return err
	}

	// Legacy-manifest quarantine adjudication: rescue fresh-render
	// matches, stamp unverified sentinels on the rest, and fail with the
	// standard drift report when any file stays unresolved. The emit pass
	// SUCCEEDED here; the failure is an adjudication the next run needs,
	// so the writes (and the sentinel stamps) stand — do not roll back.
	if err := finishLegacyMigration(ctx); err != nil {
		checksums.CommitRollback()
		return err
	}

	// Success: the writes stand. Read the write ledger BEFORE dropping the
	// journal — it is the only record of what this run actually did, and
	// the completion line is required to prove its claim from it.
	summary := checksums.SummarizeWrites(ctx.AbsPath)
	checksums.CommitRollback()
	if err := os.RemoveAll(filepath.Join(ctx.AbsPath, failedGenerateDir)); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Warning: could not clean up %s: %v\n", failedGenerateDir, err)
	}
	fmt.Println()
	printGenerateOutcome(os.Stdout, summary, flags, len(steps), totalSteps)
	return nil
}

// printGenerateOutcome renders the completion line from the run's write
// ledger instead of from "the last step returned nil".
//
// FRICTION: `forge generate --steps mocks` printed `✅ Code generation
// complete!` while regenerating NOTHING — the preset's allowlist omitted
// the emitter for the very workflow it is named after. The allowlist bug
// was fixable; the message that hid it is the deeper defect, because a
// success line that reads identically after 93 writes and after zero
// writes cannot be believed in either case. So the line states counts,
// and the zero-write case is LOUD rather than green.
//
// The three outcomes are genuinely different facts and must not share
// wording:
//
//	Touched == 0  no emitter fired. The pipeline produced nothing at all.
//	              This is the silent-no-op signature; it is a warning with
//	              the two things that cause it and the command that tells
//	              them apart.
//	Changed == 0  every emitter fired and the bytes already matched. This
//	              is the normal, healthy re-run and stays a quiet one-liner.
//	Changed > 0   real work landed. Name the counts; the file names are
//	              already on screen from the per-step output above.
func printGenerateOutcome(w io.Writer, sum checksums.WriteSummary, flags pipelineFlags, ranSteps, totalSteps int) {
	if sum.Touched == 0 {
		fmt.Fprintf(w, "⚠️  Code generation ran %d of %d pipeline steps and WROTE NO FILES.\n", ranSteps, totalSteps)
		switch {
		case flags.Steps != "" || flags.TemplatesOnly:
			// The narrowed pipeline is the case that actually shipped
			// broken, so it gets the sharpest next command.
			fmt.Fprintf(w, "    Either the tree already matched every template, or the narrowed step set excludes the emitter you need\n")
			fmt.Fprintf(w, "    (the \"mocks\" preset once omitted its own mock emitter and reported success from exactly here).\n")
			fmt.Fprintf(w, "    Fix: run `forge generate --plan%s` to see which steps the filter selects, or drop the filter to run the full pipeline.\n", planFlagEcho(flags))
		default:
			fmt.Fprintf(w, "    A full pipeline that emits nothing means every codegen step was gated off by project shape.\n")
			fmt.Fprintf(w, "    Fix: run `forge generate --plan` to see each step's [RUN]/[SKIP] verdict and the gate reason behind it.\n")
		}
		return
	}
	if sum.Changed() == 0 {
		fmt.Fprintf(w, "✅ Code generation complete — %d file(s) up to date, 0 changed.\n", sum.Touched)
		return
	}
	fmt.Fprintf(w, "✅ Code generation complete — %d file(s) written, %d changed (%d new, %d updated).\n",
		sum.Touched, sum.Changed(), len(sum.Created), len(sum.Updated))
}

// planFlagEcho reproduces the pipeline-narrowing flags so the suggested
// `--plan` command reproduces the run the user just made, rather than
// planning a different (full) pipeline that would not show the gap.
func planFlagEcho(flags pipelineFlags) string {
	var b strings.Builder
	if flags.Steps != "" {
		fmt.Fprintf(&b, " --steps=%s", flags.Steps)
	}
	if flags.TemplatesOnly {
		b.WriteString(" --templates-only")
	}
	return b.String()
}

// failedGenerateDir is where a failed run's generated output is
// preserved (project-relative, mirroring each file's project path).
// The revert is the right default — the user's tree must come back
// clean — but pre-preservation it also destroyed the only sources the
// validation error CITES: `go build` fails with file:line coordinates
// in generated code, the revert deletes that code, and the user is left
// debugging a compiler error against files that no longer exist. The
// next successful generate removes the directory.
const failedGenerateDir = ".forge/failed-generate"

// failedGenerateErrorFile holds the failing step's full error text
// (including the complete compiler output for a `go build` validation
// failure) inside failedGenerateDir.
const failedGenerateErrorFile = "error.txt"

// rollbackGeneratedTree rewinds every file forge wrote this run to its
// captured pre-run state and reports what it recovered. Returns true when
// a rollback actually ran (journaling was armed) so the caller can skip
// the post-write checksums save — a restored tree must not be described
// by a manifest of the writes it just undid. The user-facing message is
// the antidote to fr-40f7ec9bd9: instead of an opaque mid-regen failure
// the user learns the tree is back to its pre-run state and that the
// codegen problem itself is what needs fixing.
//
// Before rewinding, the failed run's output is preserved under
// .forge/failed-generate/ (SnapshotJournalTargets) so the error's
// file:line coordinates stay inspectable, and stepErr's compiler output
// (when it carries any — see validateBuildError) is REPEATED after the
// reverted-file list: the original print scrolls away behind that list,
// and a `tail` of the run must show the error, not just bookkeeping.
func rollbackGeneratedTree(absPath string, stepErr error) bool {
	if !checksums.RollbackEnabled() {
		return false
	}
	preserved := checksums.SnapshotJournalTargets(absPath, filepath.Join(absPath, failedGenerateDir))
	if len(preserved) > 0 {
		writeFailedGenerateErrorFile(absPath, stepErr)
	}
	restored := checksums.RestoreRollback(absPath)
	if len(restored) == 0 {
		fmt.Fprintln(os.Stderr, "↩️  generate failed after validation; no forge-written files needed reverting (tree is unchanged).")
		reprintCompilerOutput(stepErr, "")
		return true
	}
	fmt.Fprintf(os.Stderr, "\n↩️  generate failed its own validation — reverted %d file(s) forge wrote this run; your tree is back to its pre-run state (no `git checkout` needed):\n", len(restored))
	for _, p := range restored {
		fmt.Fprintf(os.Stderr, "   - %s\n", p)
	}
	preservedAt := ""
	if len(preserved) > 0 {
		preservedAt = failedGenerateDir
		fmt.Fprintf(os.Stderr, "\n🗂  The failing generated sources were preserved under %s/ (same relative paths, plus %s with the full error) so you can inspect the code the error points at. The next successful `forge generate` cleans that directory up.\n", failedGenerateDir, failedGenerateErrorFile)
	}
	reprintCompilerOutput(stepErr, preservedAt)
	fmt.Fprintln(os.Stderr, "\n   Fix the codegen error above (the generated code did not build), then re-run `forge generate`.")
	return true
}

// reprintCompilerOutput repeats a validation failure's compiler output
// at the END of the failure report. The original output prints before
// the reverted-file list and the final error line, so on a long run it
// scrolls off screen and `tail -n` shows only bookkeeping — a real
// agent burned a full extra pipeline run piped through grep just to
// re-capture the error. No-op when stepErr carries no build output
// (non-validate step failures). preservedAt names the directory where
// the cited sources now live ("" when nothing was preserved).
func reprintCompilerOutput(stepErr error, preservedAt string) {
	var ve *validateBuildError
	if !errors.As(stepErr, &ve) || strings.TrimSpace(ve.Output) == "" {
		return
	}
	where := ""
	if preservedAt != "" {
		where = fmt.Sprintf(" (cited files preserved under %s/)", preservedAt)
	}
	fmt.Fprintf(os.Stderr, "\n❌ Compiler output, repeated so it is the last thing on screen%s:\n", where)
	lines := strings.Split(strings.TrimRight(ve.Output, "\n"), "\n")
	shown := lines
	if len(shown) > compilerOutputTailLines {
		shown = shown[:compilerOutputTailLines]
	}
	for _, line := range shown {
		fmt.Fprintf(os.Stderr, "   %s\n", line)
	}
	if len(lines) > len(shown) {
		suffix := ""
		if preservedAt != "" {
			suffix = fmt.Sprintf("; full output in %s/%s", preservedAt, failedGenerateErrorFile)
		}
		fmt.Fprintf(os.Stderr, "   … (%d more line(s)%s)\n", len(lines)-len(shown), suffix)
	}
}

// compilerOutputTailLines bounds the repeated compiler output. The
// first errors are the actionable ones (the Go compiler reports in
// file order, and later errors are usually knock-ons), so a bounded
// head keeps the tail of the run useful without re-dumping megabytes
// on a catastrophically broken tree.
const compilerOutputTailLines = 60

// writeFailedGenerateErrorFile records the failing step's full error —
// including the complete compiler output when present — inside the
// failed-generate preserve directory, so the error text survives any
// terminal scrollback limit alongside the sources it cites.
func writeFailedGenerateErrorFile(absPath string, stepErr error) {
	if stepErr == nil {
		return
	}
	var b strings.Builder
	b.WriteString(stepErr.Error())
	b.WriteString("\n")
	var ve *validateBuildError
	if errors.As(stepErr, &ve) && strings.TrimSpace(ve.Output) != "" {
		b.WriteString("\n--- full compiler output ---\n")
		b.WriteString(ve.Output)
		if !strings.HasSuffix(ve.Output, "\n") {
			b.WriteString("\n")
		}
	}
	dest := filepath.Join(absPath, failedGenerateDir, failedGenerateErrorFile)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(dest, []byte(b.String()), 0o644)
}

// validateBuildError is the error returned by runGoBuildValidate. It
// carries the captured `go build ./...` stderr so the pipeline's
// failure path can repeat the compiler output at the END of the run
// (reprintCompilerOutput) and preserve it next to the failing sources
// (writeFailedGenerateErrorFile). Unwraps to the user-facing wrapped
// error so errors.Is/As and the printed message are unchanged.
type validateBuildError struct {
	Output string // full captured go build stderr
	err    error  // the cliutil-wrapped user-facing error
}

func (e *validateBuildError) Error() string { return e.err.Error() }
func (e *validateBuildError) Unwrap() error { return e.err }

// runGoBuildValidate is the body of stepGoBuildValidate (was Step 9 in
// the pre-refactor pipeline). Kept as a non-step helper so unit tests
// can invoke it directly without spinning up the full GenStep loop.
func runGoBuildValidate(projectDir string) error {
	fmt.Println("\n🔨 Validating generated code...")
	validateCmd := exec.Command("go", "build", "./...")
	validateCmd.Dir = projectDir
	var buildStderr strings.Builder
	validateCmd.Stdout = os.Stdout
	validateCmd.Stderr = io.MultiWriter(os.Stderr, &buildStderr)
	if err := validateCmd.Run(); err != nil {
		errOutput := buildStderr.String()
		fix := goBuildValidateFixHint(errOutput)
		return &validateBuildError{
			Output: errOutput,
			err: cliutil.WrapUserErr("forge generate (validate generated code)",
				"go build failed", "", fix, err),
		}
	}
	return nil
}

// goBuildValidateFixHint inspects the `go build ./...` stderr captured
// by runGoBuildValidate and returns the most-actionable single-line
// remediation tip for the failure pattern seen.
//
// Pattern hierarchy (first match wins):
//
//  1. `undefined: orm.Type*` — protoc-gen-forge emitted a reference to
//     an orm.* constant that the project's pinned forge/pkg version
//     does NOT export. This is the "codegen plugin newer than runtime
//     pin" skew kalshi-trader's migration round hit four separate
//     times (TypeDoublePrecision/TypeReal landed in forge/pkg after
//     the project's go.mod pin). The fix is mechanical: bump the
//     forge/pkg pin in both root and gen/ and re-tidy.
//
//  2. `undefined:` against the project's own `pkg/config` package —
//     proto/config/ likely has no annotated config fields yet.
//
//  3. Default fall-through — generic "ensure imports / re-run generate".
//
// Extracted from runGoBuildValidate so unit tests can pin the hint
// selection without spinning up a tmpdir project + a real go build.
func goBuildValidateFixHint(errOutput string) string {
	if errOutput == "" {
		return "ensure all referenced types are imported and re-run 'forge generate'"
	}
	// Pattern 1: forge/pkg runtime skew. The protoc-gen-forge in PATH
	// is newer than the project's pinned forge/pkg version, so codegen
	// emits constants the runtime doesn't export.
	//
	// We match on `undefined: orm.Type` (covers TypeReal,
	// TypeDoublePrecision, and any future orm.Type<X> constant the
	// plugin emits — the pattern is forward-compatible without a
	// growing per-constant allowlist).
	if strings.Contains(errOutput, "undefined: orm.Type") {
		return "forge/pkg pin is older than the codegen plugin (orm.Type* not exported). Run `go get github.com/reliant-labs/forge/pkg@latest && go mod tidy` in BOTH the project root and gen/ to bump the pin, then re-run 'forge generate'."
	}
	if strings.Contains(errOutput, "pkg/config") {
		return "ensure proto/config/ has annotated config fields and re-run 'forge generate'"
	}
	return "ensure all referenced types are imported and re-run 'forge generate'"
}

// preCodegenContractCheck runs the internal-package contract shape rule
// BEFORE any code generators write files. The bootstrap codegen template
// (internal/templates/project/bootstrap.go.tmpl) hardcodes references to
// <pkg>.Service / <pkg>.Deps / <pkg>.New(...) for every internal package;
// a contract.go that uses different names produces a bootstrap.go that
// doesn't compile. Catching this at validation time (rather than at the
// final `go build` step) gives the user a clear, actionable error
// pointing at their contract.go rather than a build error pointing at
// generated code.
//
// Honors `contracts.exclude` from forge.yaml so analyzer sub-packages and
// other non-bootstrap-managed internal packages can opt out.
//
// Only the contract-names rule runs here, and severity is not the reason:
// deps-are-interfaces is an error too. The question this check answers is
// narrower — would the code forge is about to WRITE fail to compile? A
// non-canonical contract breaks the bootstrap template, so it must stop
// generation. A concrete `Deps` field compiles fine; it just cannot be
// unit-tested, so it fails `forge lint` instead. Keeping the pre-codegen
// check tight to "what would break the next `go build`" is the design
// discipline from the validation-vs-lint split.
// contractExcludesFromConfig returns the contracts.exclude list from the
// project config, or nil when no config is loaded. (A local copy of the
// helper that moved to internal/cli/lint with `forge lint`; generate.go is
// the only remaining internal/cli caller.)
func contractExcludesFromConfig(cfg *config.ProjectConfig) []string {
	if cfg == nil {
		return nil
	}
	return cfg.Contracts.Exclude
}

func preCodegenContractCheck(projectDir string, cfg *config.ProjectConfig) error {
	internalDir := filepath.Join(projectDir, "internal")
	if _, err := os.Stat(internalDir); os.IsNotExist(err) {
		return nil
	}
	excludes := contractExcludesFromConfig(cfg)
	fs, err := contractcheck.Inspect(context.Background(), projectDir, contractcheck.Options{
		Rules:    []contractcheck.Rule{contractcheck.RuleInternalPackageContractNames},
		Excludes: excludes,
	})
	if err != nil {
		// PROMOTED 2026-06-07 from silent warn to hard error: a walk
		// error here (permission denied, transient I/O glitch) means we
		// can't confirm contract shape — proceeding silently would let
		// the pipeline emit bootstrap.go against an unvalidated tree,
		// and the user would diagnose a confusing build failure later.
		// The opt-out (--skip-pre-checks) exists for the parallel-lane
		// scenario where the walk error is expected.
		return cliutil.WrapUserErr("forge generate (pre-codegen contract check)",
			"unable to validate contracts (could not read internal/)",
			"",
			"check filesystem permissions on internal/, or run with --skip-pre-checks if this is a parallel-lane scenario",
			err)
	}
	if !contractcheck.HasErrors(fs) {
		return nil
	}

	// Surface each finding with the same actionable message the lint
	// command would emit, then abort the pipeline.
	fmt.Fprintln(os.Stderr, "\n❌ Internal-package contract convention violations:")
	fmt.Fprintln(os.Stderr)
	fmt.Fprint(os.Stderr, contractcheck.AsResult(fs).FormatText())
	fmt.Fprintln(os.Stderr, "Aborting before bootstrap codegen — fix the contract.go names above and retry.")
	return cliutil.UserErr("forge generate (pre-codegen contract check)",
		"internal-package contracts must declare 'type Service interface', 'type Deps struct', and 'func New(Deps) Service'",
		"",
		"fix the offending contract.go files (see findings above), or run 'forge lint --conventions' for the per-file detail")
}

// runGenerateCheck implements `forge generate --check` — the CI guard
// that verifies the committed tree matches what the generator would
// produce from current proto + forge.yaml + templates. Drift means
// someone forgot to run `forge generate` after editing a proto file or
// upgrading forge; CI should fail loudly so the gap doesn't ship.
//
// Approach:
//  1. Snapshot the current tree's committed state via `git stash --keep-index --include-untracked`
//     equivalent — we use `git diff --quiet` after running generate to
//     detect any change.
//  2. Run the pipeline against `.` (the normal path).
//  3. Compare the post-generate tree against HEAD via `git status --porcelain`.
//  4. If anything tracked changed (or new files appeared at tracked paths),
//     emit the diff and exit 1.
//
// We don't actually copy the tree to a tmpdir — for forge projects the
// pipeline is idempotent in the steady state, so the cheapest and most
// honest check is "run it and see if git notices". The pipeline is
// already designed to be re-runnable.
func runGenerateCheck() error {
	if _, err := exec.LookPath("git"); err != nil {
		return cliutil.UserErr("forge generate --check",
			"git not found on PATH",
			"",
			"--check requires git to diff the post-generate tree against HEAD")
	}
	// Refuse to --check on a dirty working tree — we'd otherwise blame
	// the user's uncommitted edits on the generator.
	dirty, err := workingTreeDirty()
	if err != nil {
		return fmt.Errorf("git status check: %w", err)
	}
	if dirty {
		return cliutil.UserErr("forge generate --check",
			"working tree has uncommitted changes — --check would misattribute them to the generator",
			"",
			"commit or stash your changes, then re-run forge generate --check")
	}

	fmt.Println("[generate --check] running generate against current tree...")
	generateMu.Lock()
	pipeErr := runGeneratePipelineOpts(".", false, true)
	generateMu.Unlock()
	if pipeErr != nil {
		return fmt.Errorf("generate pipeline: %w", pipeErr)
	}

	// Did anything change?
	statusCmd := exec.Command("git", "status", "--porcelain")
	out, err := statusCmd.Output()
	if err != nil {
		return fmt.Errorf("git status --porcelain: %w", err)
	}
	if strings.TrimSpace(string(out)) == "" {
		fmt.Println("[generate --check] no drift — tree matches generator output.")
		return nil
	}

	fmt.Fprintln(os.Stderr, "[generate --check] drift detected — committed tree does not match generator output:")
	fmt.Fprintln(os.Stderr)
	fmt.Fprint(os.Stderr, string(out))
	fmt.Fprintln(os.Stderr)
	// Show a short unified diff so reviewers can see what's stale.
	diffCmd := exec.Command("git", "--no-pager", "diff", "--stat")
	diffCmd.Stdout = os.Stderr
	diffCmd.Stderr = os.Stderr
	_ = diffCmd.Run()
	return cliutil.UserErr("forge generate --check",
		"generated artifacts are out of date in the committed tree",
		"",
		"run 'forge generate' locally, commit the result, and push")
}

// workingTreeDirty returns true when `git status --porcelain` reports
// any tracked-or-untracked change.
func workingTreeDirty() (bool, error) {
	out, err := exec.Command("git", "status", "--porcelain").Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}
