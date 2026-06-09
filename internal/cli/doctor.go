package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/doctor"
)

func newDoctorCmd() *cobra.Command {
	var (
		jsonOutput bool
		verbose    bool
		timeout    time.Duration
		signal     string
	)

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the health of your local development stack",
		Long: `Run diagnostics on all services in the local development stack.

Checks Docker Compose services, app health, pprof, and all telemetry
signals (metrics, traces, logs, profiles). Reports a clear pass/fail
for each component.

Examples:
  forge doctor              # Check everything
  forge doctor --json       # Machine-readable output
  forge doctor --verbose    # Show evidence for passing checks
  forge doctor --signal traces  # Check only traces`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(jsonOutput, verbose, timeout, signal)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output results as JSON")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Show evidence for all checks (not just failures)")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Second, "Overall timeout for all checks")
	cmd.Flags().StringVar(&signal, "signal", "", "Check a specific signal only (metrics, traces, logs, profiles)")

	return cmd
}

func runDoctor(jsonOutput, verbose bool, timeout time.Duration, signal string) error {
	cfg, err := loadProjectConfig()
	if err != nil {
		return err
	}

	// The project directory is where forge.yaml (and docker-compose.yml) live.
	configPath, err := findProjectConfigFile()
	if err != nil {
		return err
	}
	projectDir := filepath.Dir(configPath)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	d := doctor.New(doctor.Deps{})

	if !jsonOutput {
		fmt.Printf("\n  Checking %s development stack...\n\n", cfg.Name)
	}

	report, err := d.RunFiltered(ctx, cfg.Name, projectDir, signal)
	if err != nil {
		return err
	}

	// Ingress checks live alongside the signal checks but are wired
	// at the cli layer because they need RenderKCL + ListEnvs +
	// kubectl, none of which the internal/doctor package has access
	// to. Skips when features.ingress is off or the signal filter
	// excludes them.
	appendIngressChecksToReport(&report, runIngressDoctorChecks(ctx, cfg, projectDir, signal))

	// Tool checks verify every host binary forge shells out to is
	// installed (+ meets a minimum version where pinned). Wired at
	// the cli layer for the same reason as ingress: the predicate
	// for mkcert needs RenderKCL/ListEnvs, and the install hints
	// are user-facing CLI guidance.
	appendIngressChecksToReport(&report, runToolDoctorChecks(ctx, cfg, projectDir, signal))

	// External-build checks surface per-service warnings for KCL
	// services that declare build_cmd — missing build_cwd, first
	// token not on PATH, plus the resolved (substituted) command
	// preview so the user sees what `forge build` will actually exec.
	appendExternalBuildChecksToReport(&report, runExternalBuildDoctorChecks(ctx, cfg, projectDir, signal))

	if jsonOutput {
		return d.PrintJSON(os.Stdout, report)
	}

	d.PrintReport(os.Stdout, report, verbose)

	if report.Overall == doctor.StatusFail {
		// Return a sentinel so cobra exits non-zero. The report has
		// already been printed; main.go's "Error: ..." line prints the
		// short message below so the user sees a clear failure reason.
		return errDoctorFailed
	}
	return nil
}

var errDoctorFailed = fmt.Errorf("doctor reported failing checks; see report above")
