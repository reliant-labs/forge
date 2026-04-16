package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/debug"
)

// newDebugCmd creates the top-level debug command and registers all subcommands.
func newDebugCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "debug",
		Short: "Debug a running service with Delve",
		Long: `Debug Go services with Delve.

Session state is persisted to .forge/debug-session.json so subsequent
commands (break, continue, eval, ...) reconnect to the same debugger.

Examples:
  forge debug start api-gateway
  forge debug break handler.go:42
  forge debug continue
  forge debug eval "req.UserID"
  forge debug stop`,
	}

	cmd.AddCommand(newDebugStartCmd())
	cmd.AddCommand(newDebugBreakCmd())
	cmd.AddCommand(newDebugBreakpointsCmd())
	cmd.AddCommand(newDebugClearCmd())
	cmd.AddCommand(newDebugContinueCmd())
	cmd.AddCommand(newDebugStepCmd())
	cmd.AddCommand(newDebugStepInCmd())
	cmd.AddCommand(newDebugStepOutCmd())
	cmd.AddCommand(newDebugEvalCmd())
	cmd.AddCommand(newDebugLocalsCmd())
	cmd.AddCommand(newDebugArgsCmd())
	cmd.AddCommand(newDebugStackCmd())
	cmd.AddCommand(newDebugGoroutinesCmd())
	cmd.AddCommand(newDebugStopCmd())

	return cmd
}

// ---------------------------------------------------------------------------
// Session reconnection
// ---------------------------------------------------------------------------

func connectToSession() (debug.Debugger, error) {
	session, err := debug.LoadSession(".")
	if err != nil {
		return nil, fmt.Errorf("loading debug session: %w", err)
	}
	if session == nil {
		return nil, fmt.Errorf("no active debug session (run 'forge debug start' first)")
	}
	d := debug.NewDelveDebugger()
	if err := d.Connect(session.Addr); err != nil {
		return nil, fmt.Errorf("connecting to debugger at %s: %w", session.Addr, err)
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// Output helpers
// ---------------------------------------------------------------------------

func printStopState(state *debug.StopState, jsonOutput bool) {
	if jsonOutput {
		json.NewEncoder(os.Stdout).Encode(state)
		return
	}
	fmt.Printf("Stopped at %s:%d (%s)\n", state.File, state.Line, state.Function)
	fmt.Printf("Reason: %s", state.Reason)
	if state.GoroutineID > 0 {
		fmt.Printf(" | Goroutine %d", state.GoroutineID)
	}
	fmt.Println()

	if len(state.Args) > 0 {
		fmt.Println("\nArguments:")
		for _, v := range state.Args {
			printVariable(v, "  ")
		}
	}
	if len(state.Locals) > 0 {
		fmt.Println("\nLocals:")
		for _, v := range state.Locals {
			printVariable(v, "  ")
		}
	}
}

func printVariable(v debug.Variable, indent string) {
	fmt.Printf("%s%-20s %-30s %s\n", indent, v.Name, v.Type, v.Value)
	for _, child := range v.Children {
		printVariable(child, indent+"  ")
	}
}

func printBreakpoint(bp debug.BreakpointInfo, jsonOutput bool) {
	if jsonOutput {
		json.NewEncoder(os.Stdout).Encode(bp)
		return
	}
	loc := fmt.Sprintf("%s:%d", bp.File, bp.Line)
	if bp.FunctionName != "" {
		loc = fmt.Sprintf("%s (%s)", loc, bp.FunctionName)
	}
	extra := ""
	if bp.Condition != "" {
		extra = fmt.Sprintf(" [cond: %s]", bp.Condition)
	}
	fmt.Printf("Breakpoint %d: %s  hits=%d%s\n", bp.ID, loc, bp.HitCount, extra)
}

func printVariables(vars []debug.Variable, jsonOutput bool) {
	if jsonOutput {
		json.NewEncoder(os.Stdout).Encode(vars)
		return
	}
	if len(vars) == 0 {
		fmt.Println("(none)")
		return
	}
	for _, v := range vars {
		printVariable(v, "")
	}
}

func printStacktrace(frames []debug.StackFrame, jsonOutput bool) {
	if jsonOutput {
		json.NewEncoder(os.Stdout).Encode(frames)
		return
	}
	for i, f := range frames {
		fmt.Printf("#%-3d %s\n     %s:%d\n", i, f.Function, f.File, f.Line)
	}
}

func printGoroutines(goroutines []debug.GoroutineInfo, jsonOutput bool) {
	if jsonOutput {
		json.NewEncoder(os.Stdout).Encode(goroutines)
		return
	}
	fmt.Printf("%-8s %-12s %-50s %s\n", "ID", "STATUS", "FUNCTION", "LOCATION")
	for _, g := range goroutines {
		loc := fmt.Sprintf("%s:%d", g.CurrentFile, g.CurrentLine)
		fmt.Printf("%-8d %-12s %-50s %s\n", g.ID, g.Status, g.Function, loc)
	}
}

// ---------------------------------------------------------------------------
// start
// ---------------------------------------------------------------------------

func newDebugStartCmd() *cobra.Command {
	var (
		attachPID  int
		port       int
		jsonOutput bool
	)

	cmd := &cobra.Command{
		Use:   "start <service>",
		Short: "Start a debug session for a service",
		Long: `Start a debug session for a Go service.

The binary is built with debug flags (-gcflags=all=-N -l) and launched under Delve.

If the argument contains "/" or ".", it is treated as a direct path.
Otherwise it is looked up by name in forge.project.yaml.

Examples:
  forge debug start api-gateway
  forge debug start --attach 12345
  forge debug start --port 2345 api-gateway
  forge debug start ./cmd/server`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if attachPID > 0 {
				return runDebugStartAttach(attachPID, jsonOutput)
			}
			if len(args) == 0 {
				return fmt.Errorf("provide a service name or path, or use --attach <pid>")
			}
			return runDebugStartService(args[0], port, jsonOutput)
		},
	}

	cmd.Flags().IntVar(&attachPID, "attach", 0, "Attach to an existing process by PID")
	cmd.Flags().IntVar(&port, "port", 0, "Debugger listen port (0 = auto)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")

	return cmd
}

func runDebugStartAttach(pid int, jsonOutput bool) error {
	d := debug.NewDelveDebugger()
	if err := d.StartAttach(pid); err != nil {
		return fmt.Errorf("attaching to PID %d: %w", pid, err)
	}

	session := &debug.SessionInfo{
		Type:    "delve",
		Addr:    d.Addr(),
		PID:     d.PID(),
		Binary:  fmt.Sprintf("pid:%d", pid),
		Started: time.Now(),
	}
	if err := debug.SaveSession(".", session); err != nil {
		return fmt.Errorf("saving session: %w", err)
	}

	if jsonOutput {
		json.NewEncoder(os.Stdout).Encode(session)
	} else {
		fmt.Printf("Attached to PID %d\n", pid)
		fmt.Printf("Delve listening at %s\n", d.Addr())
	}
	return nil
}

func runDebugStartService(target string, port int, jsonOutput bool) error {
	buildPath := target
	serviceName := target

	if serviceName == "." || serviceName == "./" {
		serviceName = "app"
	}
	serviceName = filepath.Base(serviceName)

	// If the target doesn't look like a path, resolve it from project config.
	if !strings.Contains(target, "/") && !strings.Contains(target, ".") {
		cfg, err := loadProjectConfig()
		if err != nil {
			return fmt.Errorf("loading project config: %w", err)
		}
		found := false
		for _, svc := range cfg.Services {
			if svc.Name == target {
				candidate := filepath.Join(svc.Path, "cmd", "server")
				if _, err := os.Stat(candidate); err == nil {
					buildPath = "./" + candidate
				} else {
					buildPath = "./" + svc.Path
				}
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("service %q not found in project config; provide a path instead", target)
		}
	}

	// Build with debug flags.
	outputBinary := filepath.Join(".forge", "debug", serviceName)
	if err := os.MkdirAll(filepath.Dir(outputBinary), 0o755); err != nil {
		return fmt.Errorf("creating debug output dir: %w", err)
	}

	fmt.Printf("Building %s with debug flags...\n", buildPath)
	buildCmd := exec.Command("go", "build", "-gcflags=all=-N -l", "-o", outputBinary, buildPath)
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		return fmt.Errorf("building debug binary: %w", err)
	}

	absBinary, err := filepath.Abs(outputBinary)
	if err != nil {
		return fmt.Errorf("resolving binary path: %w", err)
	}

	d := debug.NewDelveDebugger()
	if err := d.Start(absBinary, nil); err != nil {
		return fmt.Errorf("starting Delve: %w", err)
	}

	session := &debug.SessionInfo{
		Type:    "delve",
		Addr:    d.Addr(),
		PID:     d.PID(),
		Binary:  absBinary,
		Started: time.Now(),
	}
	if err := debug.SaveSession(".", session); err != nil {
		return fmt.Errorf("saving session: %w", err)
	}

	if jsonOutput {
		json.NewEncoder(os.Stdout).Encode(session)
	} else {
		fmt.Printf("Debug session started for %s\n", serviceName)
		fmt.Printf("Delve listening at %s\n", d.Addr())
	}
	return nil
}

// ---------------------------------------------------------------------------
// break
// ---------------------------------------------------------------------------

func newDebugBreakCmd() *cobra.Command {
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

// parseFileLine splits "file.go:42" into file and line.
func parseFileLine(s string) (string, int, error) {
	idx := strings.LastIndex(s, ":")
	if idx < 0 {
		return "", 0, fmt.Errorf("expected file:line format (e.g. handler.go:42), got %q", s)
	}
	file := s[:idx]
	line, err := strconv.Atoi(s[idx+1:])
	if err != nil {
		return "", 0, fmt.Errorf("invalid line number in %q: %w", s, err)
	}
	file, err = filepath.Abs(file)
	if err != nil {
		return "", 0, fmt.Errorf("resolving absolute path for %q: %w", s[:idx], err)
	}
	return file, line, nil
}

// ---------------------------------------------------------------------------
// breakpoints
// ---------------------------------------------------------------------------

func newDebugBreakpointsCmd() *cobra.Command {
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
		json.NewEncoder(os.Stdout).Encode(bps)
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

// ---------------------------------------------------------------------------
// clear
// ---------------------------------------------------------------------------

func newDebugClearCmd() *cobra.Command {
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
				json.NewEncoder(os.Stdout).Encode(map[string]any{"id": id, "cleared": true})
			} else {
				fmt.Printf("Breakpoint %d cleared.\n", id)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

// ---------------------------------------------------------------------------
// continue
// ---------------------------------------------------------------------------

func newDebugContinueCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "continue",
		Short: "Resume execution until the next breakpoint",
		RunE: func(cmd *cobra.Command, args []string) error {
			dbg, err := connectToSession()
			if err != nil {
				return err
			}
			state, err := dbg.Continue()
			if err != nil {
				return fmt.Errorf("continuing: %w", err)
			}
			printStopState(state, jsonOutput)
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

// ---------------------------------------------------------------------------
// step (step over)
// ---------------------------------------------------------------------------

func newDebugStepCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "step",
		Short: "Step over the current line",
		RunE: func(cmd *cobra.Command, args []string) error {
			dbg, err := connectToSession()
			if err != nil {
				return err
			}
			state, err := dbg.StepOver()
			if err != nil {
				return fmt.Errorf("stepping over: %w", err)
			}
			printStopState(state, jsonOutput)
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

// ---------------------------------------------------------------------------
// step-in
// ---------------------------------------------------------------------------

func newDebugStepInCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "step-in",
		Short: "Step into the current function call",
		RunE: func(cmd *cobra.Command, args []string) error {
			dbg, err := connectToSession()
			if err != nil {
				return err
			}
			state, err := dbg.StepInto()
			if err != nil {
				return fmt.Errorf("stepping in: %w", err)
			}
			printStopState(state, jsonOutput)
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

// ---------------------------------------------------------------------------
// step-out
// ---------------------------------------------------------------------------

func newDebugStepOutCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "step-out",
		Short: "Step out of the current function",
		RunE: func(cmd *cobra.Command, args []string) error {
			dbg, err := connectToSession()
			if err != nil {
				return err
			}
			state, err := dbg.StepOut()
			if err != nil {
				return fmt.Errorf("stepping out: %w", err)
			}
			printStopState(state, jsonOutput)
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

// ---------------------------------------------------------------------------
// eval
// ---------------------------------------------------------------------------

func newDebugEvalCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "eval <expression>",
		Short: "Evaluate an expression in the current scope",
		Long: `Evaluate a Go expression in the debugger's current scope.

Examples:
  forge debug eval "req.UserID"
  forge debug eval "len(items)"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dbg, err := connectToSession()
			if err != nil {
				return err
			}
			v, err := dbg.Eval(args[0])
			if err != nil {
				return fmt.Errorf("evaluating expression: %w", err)
			}
			if jsonOutput {
				json.NewEncoder(os.Stdout).Encode(v)
			} else {
				printVariable(*v, "")
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

// ---------------------------------------------------------------------------
// locals
// ---------------------------------------------------------------------------

func newDebugLocalsCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "locals",
		Short: "Show local variables in the current scope",
		RunE: func(cmd *cobra.Command, args []string) error {
			dbg, err := connectToSession()
			if err != nil {
				return err
			}
			vars, err := dbg.Locals()
			if err != nil {
				return fmt.Errorf("listing locals: %w", err)
			}
			printVariables(vars, jsonOutput)
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

// ---------------------------------------------------------------------------
// args
// ---------------------------------------------------------------------------

func newDebugArgsCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "args",
		Short: "Show function arguments in the current scope",
		RunE: func(cmd *cobra.Command, args []string) error {
			dbg, err := connectToSession()
			if err != nil {
				return err
			}
			vars, err := dbg.Args()
			if err != nil {
				return fmt.Errorf("listing args: %w", err)
			}
			printVariables(vars, jsonOutput)
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

// ---------------------------------------------------------------------------
// stack
// ---------------------------------------------------------------------------

func newDebugStackCmd() *cobra.Command {
	var (
		depth      int
		jsonOutput bool
	)

	cmd := &cobra.Command{
		Use:   "stack",
		Short: "Show the current call stack",
		RunE: func(cmd *cobra.Command, args []string) error {
			dbg, err := connectToSession()
			if err != nil {
				return err
			}
			frames, err := dbg.Stacktrace(depth)
			if err != nil {
				return fmt.Errorf("getting stacktrace: %w", err)
			}
			printStacktrace(frames, jsonOutput)
			return nil
		},
	}

	cmd.Flags().IntVar(&depth, "depth", 50, "Maximum stack depth")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

// ---------------------------------------------------------------------------
// goroutines
// ---------------------------------------------------------------------------

func newDebugGoroutinesCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "goroutines",
		Short: "List goroutines",
		RunE: func(cmd *cobra.Command, args []string) error {
			dbg, err := connectToSession()
			if err != nil {
				return err
			}
			goroutines, err := dbg.Goroutines()
			if err != nil {
				return fmt.Errorf("listing goroutines: %w", err)
			}
			printGoroutines(goroutines, jsonOutput)
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

// ---------------------------------------------------------------------------
// stop
// ---------------------------------------------------------------------------

func newDebugStopCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the debug session and kill the debugged process",
		RunE: func(cmd *cobra.Command, args []string) error {
			dbg, err := connectToSession()
			if err != nil {
				// If we can't connect, still try to clear the session file.
				if clearErr := debug.ClearSession("."); clearErr != nil {
					return fmt.Errorf("clearing session: %w (original error: %v)", clearErr, err)
				}
				fmt.Println("Session file cleared (debugger was not reachable).")
				return nil
			}

			if err := dbg.Stop(); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: error stopping debugger: %v\n", err)
			}

			if err := debug.ClearSession("."); err != nil {
				return fmt.Errorf("clearing session: %w", err)
			}

			fmt.Println("Debug session stopped.")
			return nil
		},
	}
	return cmd
}
