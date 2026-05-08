package cli

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/linter/forgeconv"
)

// generateMu protects the generation pipeline from concurrent runs.
// It is legitimately package-level shared state used by generate, add, and new commands.
var generateMu sync.Mutex

func newGenerateCmd() *cobra.Command {
	var (
		watch   bool
		force   bool
		accept  bool
		explain bool
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
  forge generate              # Generate all code
  forge generate --watch      # Watch mode for development
  forge generate --force      # Discard hand-edits to Tier-1 files and regenerate
  forge generate --accept     # Keep hand-edits to Tier-1 files; refresh recorded checksums
  forge generate --explain    # Print per-file provenance log after generate`,
		RunE: func(cmd *cobra.Command, args []string) error {
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
				return fmt.Errorf("--force and --accept are mutually exclusive: --force discards your edits, --accept keeps them; pick one")
			}

			generateMu.Lock()
			err := runGeneratePipeline(".", force, accept)
			generateMu.Unlock()

			// Print the explain log even when the pipeline failed — partial
			// provenance is still useful for diagnosing what got generated
			// before the build break. The original error is returned below.
			if explain {
				if explainErr := printExplainLog(".", preChecksums); explainErr != nil {
					fmt.Fprintf(os.Stderr, "Warning: explain log failed: %v\n", explainErr)
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
	cmd.Flags().BoolVar(&accept, "accept", false, "Keep hand-edits to Tier-1 files; refresh recorded checksums to match (rare; documents an intentional fork)")
	cmd.Flags().BoolVar(&explain, "explain", false, "Print a per-file provenance log after generate")

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
	// Cross-process file lock (complements the in-process generateMu).
	// Held for the lifetime of the pipeline so a parallel `forge add`
	// can't race a long `forge generate`.
	release, err := acquireGenerateLock(projectDir)
	if err != nil {
		return err
	}
	defer release()

	ctx, err := newPipelineContext(projectDir, force, accept)
	if err != nil {
		return err
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

	for _, step := range generateSteps() {
		if !step.Gate(ctx) {
			continue
		}
		if err := step.Run(ctx); err != nil {
			return fmt.Errorf("step %q: %w", step.Name, err)
		}
	}

	fmt.Println()
	fmt.Println("✅ Code generation complete!")
	return nil
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
		if errOutput != "" {
			fmt.Fprintf(os.Stderr, "\n💡 Build failed. Common fixes:\n")
			if strings.Contains(errOutput, "pkg/config") {
				fmt.Fprintf(os.Stderr, "  • Ensure proto/config/ has annotated config fields and re-run 'forge generate'\n")
			}
			if strings.Contains(errOutput, "GeneratedAuthorizer") || strings.Contains(errOutput, "authorizer_gen") {
				fmt.Fprintf(os.Stderr, "  • authorizer_gen.go may be missing — re-run 'forge generate'\n")
			}
		}
		return fmt.Errorf("generated code failed to compile: %w", err)
	}
	return nil
}

// preCodegenContractCheck runs the forgeconv internal-package contract
// shape analyzer BEFORE any code generators write files. The bootstrap
// codegen template (internal/templates/project/bootstrap.go.tmpl)
// hardcodes references to <pkg>.Service / <pkg>.Deps / <pkg>.New(...) for
// every internal package; a contract.go that uses different names produces
// a bootstrap.go that doesn't compile. Catching this at validation time
// (rather than at the final `go build` step) gives the user a clear,
// actionable error pointing at their contract.go rather than a build
// error pointing at generated code.
//
// Honors `contracts.exclude` from forge.yaml so analyzer sub-packages and
// other non-bootstrap-managed internal packages can opt out.
func preCodegenContractCheck(projectDir string, cfg *config.ProjectConfig) error {
	internalDir := filepath.Join(projectDir, "internal")
	if _, err := os.Stat(internalDir); os.IsNotExist(err) {
		return nil
	}
	excludes := contractExcludesFromConfig(cfg)
	res, err := forgeconv.LintInternalContracts(projectDir, excludes)
	if err != nil {
		// Best-effort: a walk error shouldn't block generate.
		fmt.Fprintf(os.Stderr, "Warning: pre-codegen contract check failed: %v\n", err)
		return nil
	}
	if !res.HasErrors() {
		return nil
	}

	// Surface each finding with the same actionable message the lint
	// command would emit, then abort the pipeline.
	fmt.Fprintln(os.Stderr, "\n❌ Internal-package contract convention violations:")
	fmt.Fprintln(os.Stderr)
	fmt.Fprint(os.Stderr, res.FormatText())
	fmt.Fprintln(os.Stderr, "Aborting before bootstrap codegen — fix the contract.go names above and retry.")
	return fmt.Errorf("forge convention: internal-package contracts must declare 'type Service interface', 'type Deps struct', and 'func New(Deps) Service' (run 'forge lint --conventions' for details)")
}
