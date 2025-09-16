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
		Short: "Debug a running service with Delve or CDP",
		Long: `Debug Go services with Delve or TypeScript/Next.js apps with Chrome DevTools Protocol.

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

// connectToSession loads a persisted debug session and connects to the debugger.
func connectToSession() (debug.Debugger, error) {
	session, err := debug.LoadSession(".")
	if err != nil {
		return nil, fmt.Errorf("loading debug session: %w", err)
	}
	if session == nil {
		return nil, fmt.Errorf("no active debug session (run 'forge debug start' first)")
	}

	switch session.Type {
	case "delve":
		d := debug.NewDelveDebugger()
		if err := d.Connect(session.Addr); err != nil {
			return nil, fmt.Errorf("connecting to Delve at %s: %w", session.Addr, err)
		}
		return d, nil
	case "cdp":
		d := debug.NewCDPDebugger()
		if err := d.ConnectTo(session.Addr); err != nil {
			return nil, fmt.Errorf("connecting to CDP at %s: %w", session.Addr, err)
		}
		return d, nil
	default:
		return nil, fmt.Errorf("unknown session type: %s", session.Type)
	}
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
		attachPID   int
		frontend    string
		port        int
		jsonOutput  bool
	)

	cmd := &cobra.Command{
		Use:   "start <service-or-path>",
		Short: "Start a debug session for a service",
		Long: `Start a debug session for a Go service or frontend.

For Go services, the binary is built with debug flags and launched under Delve.
For frontends, Next.js is started with --inspect via CDP.

Examples:
  forge debug start api-gateway
  forge debug start --attach 12345
  forge debug start --frontend web
  forge debug start ./cmd/server`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDebugStart(args, attachPID, frontend, port, jsonOutput)
		},
	}

	cmd.Flags().IntVar(&attachPID, "attach", 0, "Attach to an existing process by PID")
	cmd.Flags().StringVar(&frontend, "frontend", "", "Start debugging a frontend (Next.js) by name")
	cmd.Flags().IntVar(&port, "port", 0, "Debugger listen port (0 = auto)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")

	return cmd
}

func runDebugStart(args []string, attachPID int, frontend string, port int, jsonOutput bool) error {
	// --attach mode
	if attachPID > 0 {
		return runDebugStartAttach(attachPID, jsonOutput)
	}

	// --frontend mode
	if frontend != "" {
		return runDebugStartFrontend(frontend, jsonOutput)
	}

	// Service mode — need a service name or path
	if len(args) == 0 {
		return fmt.Errorf("provide a service name, path, or use --attach/--frontend")
	}
	target := args[0]

	return runDebugStartService(target, jsonOutput)
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

func runDebugStartFrontend(name string, jsonOutput bool) error {
	// Find frontend path from project config
	cfg, err := loadProjectConfig()
	if err != nil {
		return fmt.Errorf("loading project config: %w", err)
	}

	var frontendPath string
	for _, fe := range cfg.Frontends {
		if fe.Name == name {
			frontendPath = fe.Path
			break
		}
	}
	if frontendPath == "" {
		return fmt.Errorf("frontend %q not found in project config", name)
	}

	d := debug.NewCDPDebugger()
	if err := d.StartNextDev(frontendPath); err != nil {
		return fmt.Errorf("starting frontend debug session: %w", err)
	}

	session := &debug.SessionInfo{
		Type:    "cdp",
		Addr:    d.WsURL(),
		Binary:  name,
		Started: time.Now(),
	}
	if err := debug.SaveSession(".", session); err != nil {
		return fmt.Errorf("saving session: %w", err)
	}

	if jsonOutput {
		json.NewEncoder(os.Stdout).Encode(session)
	} else {
		fmt.Printf("Frontend %q debug session started\n", name)
		fmt.Printf("CDP connected at %s\n", d.WsURL())
	}
	return nil
}

func runDebugStartService(target string, jsonOutput bool) error {
	// Determine the build path.
	// If target looks like a path (contains / or .), use it directly.
	// Otherwise treat it as a service name and look it up in project config.
	buildPath := target
	serviceName := target

	// Sanitize service name for use as a file name
	if serviceName == "." || serviceName == "./" {
		serviceName = "app"
	}
	serviceName = filepath.Base(serviceName)

	if !strings.Contains(target, "/") && !strings.Contains(target, ".") {
		cfg, err := loadProjectConfig()
		if err != nil {
			return fmt.Errorf("loading project config: %w", err)
		}
		found := false
		for _, svc := range cfg.Services {
			if svc.Name == target {
				// The main package for a service is typically at cmd/server or
				// the service path itself. Use the service path with /cmd/server
				// as the convention, falling back to the path directly.
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

	// Build with debug flags
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

	// Make the path absolute for Delve
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
		fmt.Printf("Delve listening at %s (PID %d)\n", d.Addr(), d.PID())
		fmt.Printf("Binary: %s\n", absBinary)
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

	// Function breakpoint — only available on Delve
	if funcName != "" {
		dd, ok := dbg.(*debug.DelveDebugger)
		if !ok {
			return fmt.Errorf("function breakpoints are only supported for Delve (Go) sessions")
		}
		bp, err := dd.SetFunctionBreakpoint(funcName, condition)
		if err != nil {
			return fmt.Errorf("setting function breakpoint: %w", err)
		}
		printBreakpoint(*bp, jsonOutput)
		return nil
	}

	// File:line breakpoint
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
	// Delve requires absolute paths
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
	cmd := &cobra.Command{
		Use:   "clear <id>",
		Short: "Clear a breakpoint by ID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDebugClear(args[0])
		},
	}
	return cmd
}

func runDebugClear(idStr string) error {
	id, err := strconv.Atoi(idStr)
	if err != nil {
		return fmt.Errorf("invalid breakpoint ID %q: %w", idStr, err)
	}

	dbg, err := connectToSession()
	if err != nil {
		return err
	}

	if err := dbg.ClearBreakpoint(id); err != nil {
		return fmt.Errorf("clearing breakpoint: %w", err)
	}
	fmt.Printf("Breakpoint %d cleared.\n", id)
	return nil
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
			return runDebugContinue(jsonOutput)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func runDebugContinue(jsonOutput bool) error {
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
			return runDebugStep(jsonOutput)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func runDebugStep(jsonOutput bool) error {
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
}

// ---------------------------------------------------------------------------
// stepin
// ---------------------------------------------------------------------------

func newDebugStepInCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "stepin",
		Short: "Step into the current function call",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDebugStepIn(jsonOutput)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func runDebugStepIn(jsonOutput bool) error {
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
}

// ---------------------------------------------------------------------------
// stepout
// ---------------------------------------------------------------------------

func newDebugStepOutCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "stepout",
		Short: "Step out of the current function",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDebugStepOut(jsonOutput)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func runDebugStepOut(jsonOutput bool) error {
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
}

// ---------------------------------------------------------------------------
// eval
// ---------------------------------------------------------------------------

func newDebugEvalCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "eval <expression>",
		Short: "Evaluate an expression in the current scope",
		Long: `Evaluate a Go or JavaScript expression in the debugger's current scope.

Examples:
  forge debug eval "req.UserID"
  forge debug eval "len(items)"
  forge debug eval "config.Port"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDebugEval(args[0], jsonOutput)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func runDebugEval(expr string, jsonOutput bool) error {
	dbg, err := connectToSession()
	if err != nil {
		return err
	}

	v, err := dbg.Eval(expr)
	if err != nil {
		return fmt.Errorf("evaluating expression: %w", err)
	}

	if jsonOutput {
		json.NewEncoder(os.Stdout).Encode(v)
	} else {
		printVariable(*v, "")
	}
	return nil
}

// ---------------------------------------------------------------------------
// locals
// ---------------------------------------------------------------------------

func newDebugLocalsCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "locals",
		Short: "List local variables in the current scope",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDebugLocals(jsonOutput)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func runDebugLocals(jsonOutput bool) error {
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
}

// ---------------------------------------------------------------------------
// args
// ---------------------------------------------------------------------------

func newDebugArgsCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "args",
		Short: "List function arguments in the current scope",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDebugArgs(jsonOutput)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func runDebugArgs(jsonOutput bool) error {
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
}

// ---------------------------------------------------------------------------
// stack
// ---------------------------------------------------------------------------

func newDebugStackCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "stack [depth]",
		Short: "Print the current call stack",
		Long: `Print the current call stack up to the given depth (default 10).

Examples:
  forge debug stack
  forge debug stack 20`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			depth := 10
			if len(args) > 0 {
				d, err := strconv.Atoi(args[0])
				if err != nil {
					return fmt.Errorf("invalid depth %q: %w", args[0], err)
				}
				depth = d
			}
			return runDebugStack(depth, jsonOutput)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func runDebugStack(depth int, jsonOutput bool) error {
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
}

// ---------------------------------------------------------------------------
// goroutines
// ---------------------------------------------------------------------------

func newDebugGoroutinesCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "goroutines",
		Short: "List goroutines (Delve sessions only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDebugGoroutines(jsonOutput)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func runDebugGoroutines(jsonOutput bool) error {
	dbg, err := connectToSession()
	if err != nil {
		return err
	}

	dd, ok := dbg.(*debug.DelveDebugger)
	if !ok {
		return fmt.Errorf("goroutines command is only available for Delve (Go) sessions")
	}

	goroutines, err := dd.Goroutines()
	if err != nil {
		return fmt.Errorf("listing goroutines: %w", err)
	}
	printGoroutines(goroutines, jsonOutput)
	return nil
}

// ---------------------------------------------------------------------------
// stop
// ---------------------------------------------------------------------------

func newDebugStopCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the debug session and kill the debugged process",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDebugStop()
		},
	}
	return cmd
}

func runDebugStop() error {
	dbg, err := connectToSession()
	if err != nil {
		// If we can't connect, still try to clear the session file
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
}