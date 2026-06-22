package debug

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/factory"
)

func newBreakCmd(_ *factory.Factory) *cobra.Command {
	var (
		funcName   string
		condition  string
		jsonOutput bool
	)

	cmd := &cobra.Command{
		Use:   "break <file:line>",
		Short: "Set a breakpoint",
		Long: `Set a breakpoint at a file:line location or on a function name.

Examples:
  forge debug break handler.go:42
  forge debug break --func main.handleRequest
  forge debug break handler.go:42 --cond "id > 5"`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDebugBreak(args, funcName, condition, jsonOutput)
		},
	}

	cmd.Flags().StringVar(&funcName, "func", "", "Set breakpoint on a function by name")
	cmd.Flags().StringVar(&condition, "cond", "", "Conditional expression for the breakpoint")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")

	return cmd
}

func runDebugBreak(args []string, funcName, condition string, jsonOutput bool) error {
	dbg, err := connectToSession()
	if err != nil {
		return err
	}

	if funcName != "" {
		// If the user passed a short name (e.g. "Create" without module path),
		// try to resolve it to a fully-qualified function name for Docker sessions.
		if !strings.Contains(funcName, "/") && !strings.Contains(funcName, ".") {
			modPath := readModulePath(".")
			if modPath != "" {
				if resolved := resolveShortFuncName(modPath, funcName); resolved != "" {
					funcName = resolved
				}
			}
		}
		bp, err := dbg.SetFunctionBreakpoint(funcName, condition)
		if err != nil {
			return fmt.Errorf("setting function breakpoint: %w", err)
		}
		printBreakpoint(*bp, jsonOutput)
		return nil
	}

	if len(args) == 0 {
		return fmt.Errorf("provide a file:line argument or use --func")
	}
	file, line, err := parseFileLine(args[0])
	if err != nil {
		return err
	}

	bp, err := dbg.SetBreakpoint(file, line, condition)
	if err != nil {
		return fmt.Errorf("setting breakpoint: %w", err)
	}
	printBreakpoint(*bp, jsonOutput)
	return nil
}

func newBreakpointsCmd(_ *factory.Factory) *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "breakpoints",
		Short: "List all breakpoints",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDebugBreakpoints(jsonOutput)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func runDebugBreakpoints(jsonOutput bool) error {
	dbg, err := connectToSession()
	if err != nil {
		return err
	}

	bps, err := dbg.ListBreakpoints()
	if err != nil {
		return fmt.Errorf("listing breakpoints: %w", err)
	}

	if jsonOutput {
		_ = json.NewEncoder(os.Stdout).Encode(bps)
		return nil
	}

	if len(bps) == 0 {
		fmt.Println("No breakpoints set.")
		return nil
	}
	for _, bp := range bps {
		printBreakpoint(bp, false)
	}
	return nil
}

func newClearCmd(_ *factory.Factory) *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "clear <id>",
		Short: "Clear a breakpoint by ID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.Atoi(args[0])
			if err != nil {
				return fmt.Errorf("invalid breakpoint ID %q: %w", args[0], err)
			}

			dbg, err := connectToSession()
			if err != nil {
				return err
			}

			if err := dbg.ClearBreakpoint(id); err != nil {
				return fmt.Errorf("clearing breakpoint: %w", err)
			}

			if jsonOutput {
				_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"id": id, "cleared": true})
			} else {
				fmt.Printf("Breakpoint %d cleared.\n", id)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}
