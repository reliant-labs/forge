package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/airbuild"
)

// newDevAirBuildCmd is `forge dev air-build`, the build step of the scaffolded
// .air.toml (internal/airbuild says why it exists and how it works).
func newDevAirBuildCmd() *cobra.Command {
	var out string
	var precheckStatus int
	cmd := &cobra.Command{
		Use:   "air-build --out <binary> -- <build command> [args...]",
		Short: "Run air's build so a failed build leaves the running server alone (plumbing)",
		Long: `Runs <build command> exactly as given (no shell), for air's [build] cmd.
A non-zero --precheck-status skips the build and handles the failure the
same way as a non-zero build status.

air restarts the binary it runs whenever a build command returns, a failed
one included, so a compile error used to cost a full restart onto the old
binary. Under air, a failed build therefore never returns while air runs:
forge prints one line and the process waits (as a small /bin/sh loop on Linux
and macOS) until air exits. The running server keeps serving the last good
build, and the next successful build restarts it once, with the fresh code.

On success, <out>.tmp (where the build writes, ` + "`go build -o <out>.tmp`" + `) is moved onto
<out> atomically, and the command exits 0. A failed build exits with the
build's status at once when nothing is running to keep: on Windows (air stops
the binary before it builds there), for the first build under an air process,
or outside air. A held build exits with the build's status once air is gone,
its tmp_dir is removed, or on SIGINT/SIGTERM.`,
		Example: `  # .air.toml [build] cmd
  CGO_ENABLED=0 forge dev air-build --out ./tmp/app -- \
    go build $FORGE_GO_BUILD_FLAGS -buildvcs=false -o ./tmp/app.tmp ./cmd/app`,
		Args:         cobra.MinimumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(out) == "" {
				return errors.New("--out is required: the binary air runs (the config's entrypoint)")
			}
			res := airbuild.Run(cmd.Context(), airbuild.Options{
				Out:            out,
				Argv:           args,
				PrecheckStatus: precheckStatus,
				Stdout:         cmd.OutOrStdout(),
				Stderr:         cmd.ErrOrStderr(),
				// A held build waits as a small shell for the rest of
				// the air session, not as a forge.
				ExecWaiter: true,
			})
			if res.Err != nil {
				return exitCodeError{code: res.ExitCode, msg: "forge dev air-build: " + res.Err.Error()}
			}
			if res.ExitCode == 0 {
				return nil
			}
			what := args[0]
			if precheckStatus != 0 {
				what = "precheck"
			}
			return exitCodeError{
				code:    res.ExitCode,
				msg:     fmt.Sprintf("forge dev air-build: %s exited with status %d", what, res.ExitCode),
				verdict: true,
			}
		},
	}
	// Everything after the command name belongs to the build.
	cmd.Flags().SetInterspersed(false)
	cmd.Flags().StringVar(&out, "out", "", "The binary air runs; the build's <out>.tmp is installed here")
	cmd.Flags().IntVar(&precheckStatus, "precheck-status", 0, "Exit status of the pre-build storage check; non-zero skips the build")
	return cmd
}
