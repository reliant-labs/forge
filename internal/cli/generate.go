package cli

import (
	"bufio"
	"context"
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
		accept          bool
		acceptReason    string
		explain         bool
		explainDrift    bool
		skipValidate    bool
		skipPreChecks   bool
		skipConfigCheck bool
		resetTier2      bool
		assumeYes       bool
		checkOnly       bool
		forceCleanup    bool
		templatesOnly   bool
		strict          bool
		verbose         bool
		planOnly        bool
		steps           string
		deprecatedScope string // hidden alias for --steps, kept for one release
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
  forge generate                  # Generate all code
  forge generate --watch          # Watch mode for development
  forge generate --force          # Discard hand-edits to Tier-1 files and regenerate
  forge generate --accept --reason "<why>"  # DEPRECATED alias for 'forge disown' on the drifted set (--reason required)
  forge generate --explain        # Print per-file provenance log after generate
  forge generate --explain-drift  # On Tier-1 drift: diff on-disk vs fresh render per drifted file, then fail with the report
  forge generate --skip-validate    # Skip the final 'go build ./...' validate step
  forge generate --skip-pre-checks  # Bypass pre-codegen contract-shape check (parallel-lane workflows)
  forge generate --reset-tier2      # Explicitly opt-in to overwriting hand-edited Tier-2 scaffolds (prompts per file)
  forge generate --check            # Run generate into a tmpdir; exit 1 if it would change the tree
  forge generate --force-cleanup    # Actually delete stale generated files (default is report-only)
  forge generate --templates-only   # Re-render template-driven files only (skips cleanup/drift/validate; for propagating a template change into a WIP tree)
  forge generate --steps=mocks      # Fast path: regen only mock_gen.go after a contract.go change (skips Tier-1 drift guard)
  forge generate --plan             # Print the pipeline plan ([RUN]/[SKIP] per step + gate reason) and exit
  forge generate --strict           # Promote pipeline warnings to fatal errors (every "Warning: ..." aborts)
  forge generate --verbose          # Print one line per gate-off step ("⏩ skipped: <step name> (<reason>)")
  forge generate --skip-config-check # Bypass the forge.yaml ↔ filesystem cross-check (parallel-lane / mid-migration only)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if checkOnly {
				return runGenerateCheck()
			}
			if planOnly {
				return runGeneratePlan(".", pipelineFlags{
					Force:           force,
					Accept:          accept,
					SkipValidate:    skipValidate,
					SkipPreChecks:   skipPreChecks,
					SkipConfigCheck: skipConfigCheck,
					ResetTier2:      resetTier2,
					AssumeYes:       assumeYes,
					ForceCleanup:    forceCleanup,
					TemplatesOnly:   templatesOnly,
					Strict:          strict,
					Verbose:         verbose,
					Steps:           steps,
				})
			}
			// Capture pre-pipeline checksums so --explain can diff
			// against post-pipeline state to label rewritten vs idempotent.
			var preChecksums map[string]string
			if explain {
				if cs, err := generator.LoadChecksums("."); err == nil {
					preChecksums = make(map[string]string, len(cs.Files))
					for k, v := range cs.Files {
						preChecksums[k] = v.Hash
					}
				}
			}

			if force && accept {
				return cliutil.UserErr("forge generate",
					"--force and --accept are mutually exclusive: --force discards your edits, --accept disowns the drifted files (keeps them, permanently)",
					"",
					"pick one — --force to regenerate from templates, or `forge disown <path> --reason \"<why>\"` to take ownership")
			}

			// --accept is a DEPRECATED alias for disowning the drifted
			// set. Like `forge disown`, it refuses to run without a
			// recorded reason — disowns are design feedback, and the
			// reason is the payload.
			if accept {
				fmt.Fprintln(os.Stderr, "⚠️  DEPRECATED: `forge generate --accept` is now an alias for `forge disown` and will be removed in the next release.")
				fmt.Fprintln(os.Stderr, "   Use `forge disown <path>... --reason \"<why>\"` — it disowns exactly the files you name instead of the whole drifted set.")
				if strings.TrimSpace(acceptReason) == "" {
					return cliutil.UserErr("forge generate",
						"--accept requires --reason: disowning generated files is design feedback, and the reason is the payload",
						"",
						"re-run as `forge generate --accept --reason \"<why>\"`, or better: `forge disown <path>... --reason \"<why>\"`")
				}
			}

			// --reason only has meaning as the recorded WHY of an --accept
			// disown. Rejecting the stray spelling loudly (instead of
			// silently dropping the text) protects the design-feedback
			// signal: a reason typed but not recorded is worse than no
			// reason, because the user believes it was captured.
			if acceptReason != "" && !accept {
				return cliutil.UserErr("forge generate",
					"--reason requires --accept: the reason is recorded against the file(s) being disowned",
					"",
					"re-run as `forge generate --accept --reason \"<why>\"` (or drop --reason)")
			}

			// Backwards-compat: --scope was renamed to --steps in this
			// release. Cobra itself emits the deprecation warning (via
			// MarkDeprecated below). We just forward the value here, and
			// reject the ambiguous case where both flags are passed with
			// different values.
			if deprecatedScope != "" {
				if steps != "" && steps != deprecatedScope {
					return cliutil.UserErr("forge generate",
						fmt.Sprintf("--steps=%q conflicts with deprecated --scope=%q", steps, deprecatedScope),
						"",
						"pass only --steps; --scope is a deprecated alias and will be removed")
				}
				steps = deprecatedScope
			}

			generateMu.Lock()
			err := runGeneratePipelineFlags(".", pipelineFlags{
				Force:           force,
				Accept:          accept,
				AcceptReason:    acceptReason,
				ExplainDrift:    explainDrift,
				SkipValidate:    skipValidate,
				SkipPreChecks:   skipPreChecks,
				SkipConfigCheck: skipConfigCheck,
				ResetTier2:      resetTier2,
				AssumeYes:       assumeYes,
				ForceCleanup:    forceCleanup,
				TemplatesOnly:   templatesOnly,
				Strict:          strict,
				Verbose:         verbose,
				Steps:           steps,
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
	cmd.Flags().BoolVar(&accept, "accept", false, "DEPRECATED: alias for `forge disown` on every drifted Tier-1 file (one-way transfer to user ownership). Requires --reason. Prefer `forge disown <path>... --reason \"<why>\"`.")
	cmd.Flags().StringVar(&acceptReason, "reason", "", "WHY the disown was needed (used with --accept). Recorded per disowned path in .forge/friction.jsonl as design feedback; view with 'forge friction list --area disown'.")
	cmd.Flags().BoolVar(&explain, "explain", false, "Print a per-file provenance log after generate")
	cmd.Flags().BoolVar(&explainDrift, "explain-drift", false, "On Tier-1 drift, run the pipeline with drifted files redirected to .forge/render/ side renders, print a bounded diff of on-disk vs fresh render per file, then fail with the drift report (explains; never overwrites or approves)")
	cmd.Flags().BoolVar(&skipValidate, "skip-validate", false, "Skip the final 'go build ./...' validate step (useful during multi-lane migrations when the tree is in a partial-build state)")
	cmd.Flags().BoolVar(&skipPreChecks, "skip-pre-checks", false, "Bypass the pre-codegen contract-shape check (useful when a parallel lane's contract violation would otherwise block regen of this lane)")
	cmd.Flags().BoolVar(&resetTier2, "reset-tier2", false, "Explicitly opt-in to overwriting hand-edited Tier-2 scaffolds (service.go, handlers.go, …) — prompts per file unless --yes is also passed")
	cmd.Flags().BoolVar(&assumeYes, "yes", false, "Auto-confirm interactive prompts (currently consumed by --reset-tier2)")
	cmd.Flags().BoolVar(&checkOnly, "check", false, "Run generate into a tmpdir and diff against the current tree; exit 1 on drift (for CI guards)")
	cmd.Flags().BoolVar(&forceCleanup, "force-cleanup", false, "Actually delete stale generated files. Default is report-only: print which files WOULD be deleted and leave them in place.")
	cmd.Flags().BoolVar(&templatesOnly, "templates-only", false, "Re-render template-driven files only. Skips cleanup sweep, drift-guard, and validation. Use when a template change needs to propagate to a project that has uncommitted WIP and can't tolerate a full regen.")
	cmd.Flags().StringVar(&steps, "steps", "", "Narrow the pipeline to a named step preset. Valid values: \"bootstrap-only\" (used internally by 'forge add worker'), \"mocks\" (regen only mock_gen.go after a contract.go change; skips the Tier-1 drift guard since mocks cannot stomp Tier-1 files).")
	// Loud-by-default architecture flags. See the per-flag fields on
	// pipelineFlags for the rationale; runGeneratePlan + warnOrFail consume them.
	cmd.Flags().BoolVar(&strict, "strict", false, "Promote pipeline warnings to fatal errors. Every 'Warning: ... failed' site that today logs and continues will abort the pipeline instead.")
	// Note: cobra has a persistent --verbose/-v on root. We rebind the
	// long form here without a shorthand so generate-specific consumers
	// (the gate-off skip printer) can read it without conflicting with
	// the inherited persistent flag.
	cmd.Flags().BoolVar(&verbose, "verbose", false, "Print one line per gate-off skipped step ('⏩ skipped: <step name> (<reason>)'). Default is silent skip.")
	cmd.Flags().BoolVar(&planOnly, "plan", false, "Print the pipeline plan ([RUN]/[SKIP] annotation per step + gate reason) and exit 0 without running any step. Honors --steps and --templates-only.")
	cmd.Flags().BoolVar(&skipConfigCheck, "skip-config-check", false, "Bypass the forge.yaml ↔ filesystem cross-check (declared services/frontends/packages must have on-disk backing). Use for parallel-lane / mid-migration scenarios.")
	// Deprecated alias for --steps. The flag previously called --scope
	// was renamed in this release to free up the word "scope" for the
	// file-ownership concept (see internal/checksums/inspector.go).
	// Hidden so `--help` shows only the canonical --steps; still
	// functional for one release so existing scripts don't break
	// overnight.
	cmd.Flags().StringVar(&deprecatedScope, "scope", "", "(deprecated) renamed to --steps")
	// cobra's MarkDeprecated both hides the flag from --help and emits
	// a one-line deprecation message to stderr the first time the user
	// passes it. One-release alias — drop after the next minor bump.
	_ = cmd.Flags().MarkDeprecated("scope", "use --steps instead")

	// `forge generate unfork` survives ONE release as legacy-fork
	// migration tooling (convert `forked: true` entries to disowned, or
	// re-adopt with --readopt / reconcile with --merge). Registered here
	// for muscle memory; the top-level `forge unfork` is the canonical
	// spelling.
	cmd.AddCommand(newUnforkCmd())

	// `forge generate accept-fork <paths>` is a DEPRECATED alias for
	// `forge disown` — kept functioning one release.
	cmd.AddCommand(newAcceptForkCmd())

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
func runGeneratePipeline(projectDir string, force, accept bool) error {
	return runGeneratePipelineOpts(projectDir, force, accept, false)
}

// runGeneratePipelineOpts is the variant that lets the caller pass
// additional pipeline flags (currently --skip-validate). Wrapping the
// legacy 3-arg signature keeps test fixtures (and any out-of-tree
// callers) source-compatible.
func runGeneratePipelineOpts(projectDir string, force, accept, skipValidate bool) error {
	return runGeneratePipelineFlags(projectDir, pipelineFlags{
		Force:        force,
		Accept:       accept,
		SkipValidate: skipValidate,
	})
}

// pipelineFlags is the typed bag of opt-in toggles for the generate
// pipeline. Grew out of the per-flag positional-arg signatures
// (runGeneratePipeline, runGeneratePipelineOpts) once the flag count
// crossed three — adding a fourth (--skip-pre-checks) without a struct
// would have meant churning every caller of the positional form.
type pipelineFlags struct {
	Force  bool
	Accept bool
	// AcceptReason is the user-supplied WHY behind an --accept fork
	// (`--reason`). Recorded per accepted path into .forge/friction.jsonl
	// at the moment of forking — forks are design feedback, and the
	// reason is the payload. Empty means unstated: a placeholder entry
	// is still recorded and a one-line nudge prints (never an
	// interactive prompt; agents drive this flow). Meaningless without
	// Accept; the cobra layer rejects that combination up-front.
	AcceptReason string
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
	// ResetTier2 explicitly opts in to overwriting hand-edited Tier-2
	// scaffolds (service.go, handlers.go, …). The default for Tier-2 is
	// "preserve hand-edits even when --force is set" — the scaffold-once
	// contract is broken by the historic --force semantics. When this
	// flag is set, the user is prompted per file (with a diff preview)
	// unless AssumeYes is also true. See item 15 of FORGE_BACKLOG.md.
	ResetTier2 bool
	// AssumeYes auto-confirms y/N prompts. Currently only consumed by
	// the per-file Tier-2 overwrite prompt under --reset-tier2.
	AssumeYes bool
	// Steps names a step preset that narrows the set of pipeline steps
	// the runner executes. The empty string runs the full pipeline (the
	// historical default). The "bootstrap-only" value runs JUST the
	// load/parse/bootstrap/validate subset — used by `forge add worker`
	// so adding a single worker doesn't trigger a full project regen
	// that stomps unrelated Tier-1 files (.github/workflows/ci.yml,
	// cmd/server.go, frontend mocks, pkg/config/config.go). The step
	// preset allowlist lives in stepPresetAllowlist
	// (generate_pipeline.go).
	//
	// (Field previously named Scope; renamed because "scope" was
	// overloaded with the file-ownership concept in
	// internal/checksums/inspector.go. The CLI flag spelling moved from
	// --scope to --steps in the same release; --scope is preserved as a
	// hidden deprecated alias for one release.)
	//
	// FRICTION 2026-06-03: cp-forge port-workers ran `forge add worker`
	// 7× and watched regen rewrite 5 unrelated Tier-1 files per call.
	// Composes with the existing tier1OwnerRegistry scoping in
	// generate_tier1_scope.go — both narrow what `forge add worker`
	// touches, just at different layers (drift-guard vs step execution).
	Steps string

	// ForceCleanup opts in to the destructive stale-artifact sweep.
	// Default (false) makes stepCleanupStale report-only: it prints
	// which manifest-recorded files would be deleted but leaves them
	// on disk. See the matching pipelineContext.ForceCleanup field for
	// the cp-forge surprise-delete rationale.
	ForceCleanup bool

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
}

// runGeneratePipelineFlags is the canonical entrypoint. Both the legacy
// runGeneratePipeline (force/accept) and the slightly newer
// runGeneratePipelineOpts (+ skipValidate) call through here. New flags
// land on pipelineFlags.
func runGeneratePipelineFlags(projectDir string, flags pipelineFlags) error {
	// Cross-process file lock (complements the in-process generateMu).
	// Held for the lifetime of the pipeline so a parallel `forge add`
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

	// --reset-tier2 wires a per-file Tier-2 overwrite hook. The hook
	// drives WriteGeneratedFileTier2's "user-edited Tier-2 detected;
	// overwrite y/N?" decision. Without the hook the writer preserves
	// hand-edits — the historic safe default. With --reset-tier2 --yes,
	// the hook auto-approves; without --yes it prompts per file.
	checksums.ResetTier2State()
	// Per-run side-render redirect tracking (--explain-drift) starts
	// empty on every invocation.
	checksums.ResetPerRunState()
	if flags.ResetTier2 {
		fmt.Println("⚠️  --reset-tier2: hand-edited Tier-2 scaffolds will be overwritten (prompts per file unless --yes is set)")
		checksums.Tier2OverwriteFn = makeTier2OverwriteHook(ctx.AbsPath, ctx.Checksums, flags.AssumeYes)
	}

	// Save checksums on exit, even on partial failures: a step that
	// successfully wrote files should have those tracked so the user's
	// next `forge audit` doesn't false-flag user-edited drift.
	defer func() {
		if ctx.Checksums == nil {
			return
		}
		if saveErr := generator.SaveChecksums(ctx.AbsPath, ctx.Checksums); saveErr != nil {
			log.Printf("Warning: failed to save checksums: %v", saveErr)
		}
	}()

	// Tier-2 preservation summary fires only when --force is set: that's
	// the legacy user expectation we just changed. Users who run plain
	// `forge generate` already expect Tier-2 to be untouched and don't
	// need the nag line.
	defer func() {
		if flags.Force && checksums.Tier2PreservedCount > 0 {
			fmt.Fprintf(os.Stderr, "ℹ️  --force preserved %d hand-edited Tier-2 file(s); pass --reset-tier2 to overwrite explicitly.\n", checksums.Tier2PreservedCount)
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
			return fmt.Errorf("step %q: %w", step.Name, err)
		}
	}

	// --explain-drift: print the per-file diffs and fail with the drift
	// report — the flag explains the drift, it never approves it. No-op
	// nil when the guard found no drift (or the flag wasn't set).
	if err := finishExplainDrift(ctx); err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("✅ Code generation complete!")
	return nil
}

// makeTier2OverwriteHook returns the checksums.Tier2OverwriteFn the
// pipeline installs when the user passes `--reset-tier2`. The hook
// fires once per modified Tier-2 file as WriteGeneratedFileTier2
// encounters it; returning true clobbers the user's edits, false
// preserves them.
//
// When assumeYes is set the hook unconditionally approves the
// overwrite (`--reset-tier2 --yes`). Otherwise it prints a short
// preview ("modified Tier-2 file <path>, overwrite? y/N") on stderr
// and reads from stdin. Any answer other than `y` / `Y` / `yes` is
// treated as "preserve", matching standard y/N convention.
//
// The hook is intentionally simple — we don't print a full unified
// diff because users running --reset-tier2 already have git available
// for that ("git diff HEAD -- <path>" before re-running is the
// expected workflow). The prompt's job is the explicit per-file
// confirmation gate.
func makeTier2OverwriteHook(root string, cs *generator.FileChecksums, assumeYes bool) func(string) bool {
	reader := bufio.NewReader(os.Stdin)
	return func(relPath string) bool {
		if assumeYes {
			fmt.Fprintf(os.Stderr, "  ↻ --reset-tier2 --yes: overwriting %s\n", relPath)
			return true
		}
		recorded := ""
		current := ""
		if cs != nil {
			if entry, ok := cs.Files[relPath]; ok {
				recorded = short(entry.Hash)
			}
		}
		if data, err := os.ReadFile(filepath.Join(root, relPath)); err == nil {
			current = short(generator.HashContent(data))
		}
		fmt.Fprintf(os.Stderr, "\nTier-2 file modified: %s\n", relPath)
		fmt.Fprintf(os.Stderr, "  recorded hash: %s\n", recorded)
		fmt.Fprintf(os.Stderr, "  on-disk hash:  %s\n", current)
		fmt.Fprintf(os.Stderr, "Overwrite with newly rendered template? [y/N]: ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return false
		}
		ans := strings.ToLower(strings.TrimSpace(line))
		return ans == "y" || ans == "yes"
	}
}

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
		return cliutil.WrapUserErr("forge generate (validate generated code)",
			"go build failed", "", fix, err)
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
//  3. `undefined: GeneratedAuthorizer` / `authorizer_gen` not found —
//     authorizer_gen.go missing; re-run forge generate.
//
//  4. Default fall-through — generic "ensure imports / re-run generate".
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
	if strings.Contains(errOutput, "GeneratedAuthorizer") || strings.Contains(errOutput, "authorizer_gen") {
		return "authorizer_gen.go may be missing — re-run 'forge generate'"
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
// Only the contract-names rule runs here. The adapter-no-rpc and
// interactor-deps rules are warnings that don't gate codegen — they
// surface under `forge lint --conventions` instead. Keeping the
// pre-codegen check tight to "what would break the next `go build`"
// is the design discipline from the validation-vs-lint split.
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
	pipeErr := runGeneratePipelineOpts(".", false, false, true)
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
