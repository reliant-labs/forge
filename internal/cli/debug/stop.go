package debug

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/factory"
)

func newStopCmd(_ *factory.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the debug session and kill the debugged process",
		RunE: func(cmd *cobra.Command, args []string) error {
			session, _ := debugSvc().LoadSession(".")

			dbg, err := connectToSession()
			if err != nil {
				// Can't connect (timeout, refused, etc.).
				if session != nil && session.Docker {
					stopCmd := exec.CommandContext(cmd.Context(), "docker", "compose", "--profile", "debug", "stop", "app-debug")
					_ = stopCmd.Run()
				} else if session != nil && session.PID > 0 {
					if p, findErr := os.FindProcess(session.PID); findErr == nil {
						_ = p.Kill()
					}
				}
				if clearErr := debugSvc().ClearSession("."); clearErr != nil {
					return fmt.Errorf("clearing session: %w (original error: %v)", clearErr, err)
				}
				fmt.Println("Debug session stopped (killed by PID).")
				return nil
			}

			if session != nil && session.Docker {
				dbg.Disconnect()
				stopCmd := exec.CommandContext(cmd.Context(), "docker", "compose", "--profile", "debug", "stop", "app-debug")
				_ = stopCmd.Run()
			} else {
				if err := dbg.Stop(); err != nil {
					// Intentional soft warning: the debugger process may
					// already be dead (user Ctrl-C'd the IDE, OS reaped
					// the process, etc.). We still need to fall through
					// to ClearSession() below so the next `forge debug`
					// invocation doesn't trip the "session already
					// running" guard on stale state.
					fmt.Fprintf(os.Stderr, "Warning: error stopping debugger: %v\n", err)
				}
			}

			if err := debugSvc().ClearSession("."); err != nil {
				return fmt.Errorf("clearing session: %w", err)
			}

			fmt.Println("Debug session stopped.")
			return nil
		},
	}
	return cmd
}
