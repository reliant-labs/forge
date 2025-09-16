package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/debug"
)

// connectDebugger loads the persisted session and connects to the running debugger.
func connectDebugger() (debug.Debugger, error) {
	session, err := debug.LoadSession(".")
	if err != nil {
		return nil, fmt.Errorf("reading debug session: %w", err)
	}
	if session == nil {
		return nil, fmt.Errorf("no active debug session — use debug_start first")
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

// jsonResult marshals v to indented JSON and returns it as a string.
func jsonResult(v interface{}) (string, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshaling result: %w", err)
	}
	return string(data), nil
}

// ---------------------------------------------------------------------------
// Tool definitions
// ---------------------------------------------------------------------------

// GetDebugStartTool returns the debug_start tool definition.
func GetDebugStartTool() Tool {
	return Tool{
		Name: "debug_start",
		Description: `Start a debug session for a Go service (using Delve) or TypeScript frontend (using Chrome DevTools Protocol).

For Go services the binary is built with debug symbols and launched under Delve in headless mode. For TypeScript the Node.js inspector is started via --inspect-brk so execution pauses on the first line.

After starting, use debug_set_breakpoint to set breakpoints and debug_continue to run.`,
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"service": map[string]interface{}{
					"type":        "string",
					"description": "Name of the service to debug. For Go services this is the import path or binary name. For TypeScript this is the entry script path.",
				},
				"type": map[string]interface{}{
					"type":        "string",
					"enum":        []string{"go", "typescript"},
					"description": "Type of debugger to start: 'go' uses Delve, 'typescript' uses Chrome DevTools Protocol.",
				},
				"frontend_dir": map[string]interface{}{
					"type":        "string",
					"description": "Directory of the frontend project (TypeScript only). When set, starts 'npx next dev --inspect' from this directory instead of running a script directly.",
				},
				"attach_pid": map[string]interface{}{
					"type":        "integer",
					"description": "PID of an already-running Go process to attach to (Go only). When set, Delve attaches to this process instead of launching a new one.",
				},
			},
			"required": []string{"service", "type"},
		},
	}
}

// validateServiceName ensures the service name contains only safe characters.
func validateServiceName(name string) error {
	if name == "" {
		return fmt.Errorf("service name cannot be empty")
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return fmt.Errorf("invalid character %q in service name", string(r))
		}
	}
	return nil
}

func executeDebugStart(arguments json.RawMessage) (string, error) {
	var args struct {
		Service     string `json:"service"`
		Type        string `json:"type"`
		FrontendDir string `json:"frontend_dir"`
		AttachPID   int    `json:"attach_pid"`
	}
	if err := json.Unmarshal(arguments, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	// Validate service name to prevent path traversal.
	if err := validateServiceName(args.Service); err != nil {
		return "", fmt.Errorf("invalid service: %w", err)
	}

	var addr string
	var pid int
	var tmpDir string

	switch args.Type {
	case "go":
		d := debug.NewDelveDebugger()
		if args.AttachPID > 0 {
			if err := d.StartAttach(args.AttachPID); err != nil {
				return "", fmt.Errorf("attaching to PID %d: %w", args.AttachPID, err)
			}
		} else {
			// Use a unique temp directory to avoid predictable paths.
			var err error
			tmpDir, err = os.MkdirTemp("", "forge-debug-*")
			if err != nil {
				return "", fmt.Errorf("creating temp dir: %w", err)
			}
			binaryPath := filepath.Join(tmpDir, "debug-"+args.Service)

			// Build the binary with debug symbols.
			buildCmd := exec.Command("go", "build", "-gcflags=all=-N -l", "-o", binaryPath, "./"+args.Service)
			if out, err := buildCmd.CombinedOutput(); err != nil {
				return "", fmt.Errorf("building %s: %s\n%s", args.Service, err, string(out))
			}
			if err := d.Start(binaryPath, nil); err != nil {
				return "", fmt.Errorf("starting Delve for %s: %w", args.Service, err)
			}
		}
		addr = d.Addr()
		pid = d.PID()

	case "typescript":
		d := debug.NewCDPDebugger()
		if args.FrontendDir != "" {
			if strings.Contains(args.FrontendDir, "..") {
				return "", fmt.Errorf("frontend_dir must not contain '..': %s", args.FrontendDir)
			}
			if err := d.StartNextDev(args.FrontendDir); err != nil {
				return "", fmt.Errorf("starting Next.js dev debugger in %s: %w", args.FrontendDir, err)
			}
		} else {
			if err := d.Start(args.Service, nil); err != nil {
				return "", fmt.Errorf("starting CDP debugger for %s: %w", args.Service, err)
			}
		}
		addr = d.WsURL()

	default:
		return "", fmt.Errorf("unsupported debug type: %s (use 'go' or 'typescript')", args.Type)
	}

	session := &debug.SessionInfo{
		Type:    typeToSessionType(args.Type),
		Addr:    addr,
		PID:     pid,
		Binary:  args.Service,
		TmpDir:  tmpDir,
		Started: time.Now(),
	}
	if err := debug.SaveSession(".", session); err != nil {
		return "", fmt.Errorf("saving debug session: %w", err)
	}

	return jsonResult(map[string]interface{}{
		"type":    session.Type,
		"address": addr,
		"pid":     pid,
	})
}

func typeToSessionType(t string) string {
	switch t {
	case "go":
		return "delve"
	case "typescript":
		return "cdp"
	default:
		return t
	}
}

// GetDebugSetBreakpointTool returns the debug_set_breakpoint tool definition.
func GetDebugSetBreakpointTool() Tool {
	return Tool{
		Name: "debug_set_breakpoint",
		Description: `Set a breakpoint in the debugged program.

Provide either file+line or function to specify the location. An optional condition expression causes the breakpoint to trigger only when the condition evaluates to true.`,
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"file": map[string]interface{}{
					"type":        "string",
					"description": "Source file path for the breakpoint.",
				},
				"line": map[string]interface{}{
					"type":        "integer",
					"description": "Line number in the source file.",
				},
				"condition": map[string]interface{}{
					"type":        "string",
					"description": "Optional condition expression. The breakpoint only fires when this evaluates to true.",
				},
				"function": map[string]interface{}{
					"type":        "string",
					"description": "Fully-qualified function name to break on (alternative to file+line). For Go, e.g. 'main.HandleRequest'. For TypeScript, the function name.",
				},
			},
		},
	}
}

func executeDebugSetBreakpoint(arguments json.RawMessage) (string, error) {
	var args struct {
		File      string `json:"file"`
		Line      int    `json:"line"`
		Condition string `json:"condition"`
		Function  string `json:"function"`
	}
	if err := json.Unmarshal(arguments, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	// Function breakpoints are only supported on Delve.
	if args.Function != "" {
		session, err := debug.LoadSession(".")
		if err != nil {
			return "", fmt.Errorf("reading debug session: %w", err)
		}
		if session == nil {
			return "", fmt.Errorf("no active debug session — use debug_start first")
		}
		if session.Type != "delve" {
			return "", fmt.Errorf("function breakpoints are only supported for Go/Delve sessions")
		}
		d := debug.NewDelveDebugger()
		if err := d.Connect(session.Addr); err != nil {
			return "", fmt.Errorf("connecting to Delve at %s: %w", session.Addr, err)
		}
		defer d.Disconnect()
		bp, err := d.SetFunctionBreakpoint(args.Function, args.Condition)
		if err != nil {
			return "", err
		}
		return jsonResult(bp)
	}

	if args.File == "" || args.Line == 0 {
		return "", fmt.Errorf("either file+line or function is required")
	}

	dbg, err := connectDebugger()
	if err != nil {
		return "", err
	}
	defer dbg.Disconnect()

	bp, err := dbg.SetBreakpoint(args.File, args.Line, args.Condition)
	if err != nil {
		return "", err
	}
	return jsonResult(bp)
}

// GetDebugListBreakpointsTool returns the debug_list_breakpoints tool definition.
func GetDebugListBreakpointsTool() Tool {
	return Tool{
		Name: "debug_list_breakpoints",
		Description: `List all breakpoints in the current debug session.

Returns an array of breakpoints with their IDs, locations, conditions, and hit counts.`,
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}
}

func executeDebugListBreakpoints(arguments json.RawMessage) (string, error) {
	dbg, err := connectDebugger()
	if err != nil {
		return "", err
	}
	defer dbg.Disconnect()

	bps, err := dbg.ListBreakpoints()
	if err != nil {
		return "", err
	}
	return jsonResult(bps)
}

// GetDebugClearBreakpointTool returns the debug_clear_breakpoint tool definition.
func GetDebugClearBreakpointTool() Tool {
	return Tool{
		Name: "debug_clear_breakpoint",
		Description: `Remove a breakpoint by its ID.

Use debug_list_breakpoints to find the ID of the breakpoint you want to remove.`,
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]interface{}{
					"type":        "integer",
					"description": "Breakpoint ID to remove.",
				},
			},
			"required": []string{"id"},
		},
	}
}

func executeDebugClearBreakpoint(arguments json.RawMessage) (string, error) {
	var args struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(arguments, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	dbg, err := connectDebugger()
	if err != nil {
		return "", err
	}
	defer dbg.Disconnect()

	if err := dbg.ClearBreakpoint(args.ID); err != nil {
		return "", err
	}
	return jsonResult(map[string]string{"status": "cleared"})
}

// GetDebugContinueTool returns the debug_continue tool definition.
func GetDebugContinueTool() Tool {
	return Tool{
		Name: "debug_continue",
		Description: `Resume program execution until the next breakpoint, exception, or exit.

Returns the full stop state including file, line, function name, reason, local variables, function arguments, and a stack trace.`,
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}
}

func executeDebugContinue(arguments json.RawMessage) (string, error) {
	dbg, err := connectDebugger()
	if err != nil {
		return "", err
	}
	defer dbg.Disconnect()

	state, err := dbg.Continue()
	if err != nil {
		return "", err
	}
	return jsonResult(state)
}

// GetDebugStepOverTool returns the debug_step_over tool definition.
func GetDebugStepOverTool() Tool {
	return Tool{
		Name: "debug_step_over",
		Description: `Step over the current line of code, executing it without entering called functions.

Returns the stop state at the next line.`,
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}
}

func executeDebugStepOver(arguments json.RawMessage) (string, error) {
	dbg, err := connectDebugger()
	if err != nil {
		return "", err
	}
	defer dbg.Disconnect()

	state, err := dbg.StepOver()
	if err != nil {
		return "", err
	}
	return jsonResult(state)
}

// GetDebugStepIntoTool returns the debug_step_into tool definition.
func GetDebugStepIntoTool() Tool {
	return Tool{
		Name: "debug_step_into",
		Description: `Step into the function call on the current line.

If the current line contains a function call, execution moves to the first line of that function. Returns the stop state at the new location.`,
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}
}

func executeDebugStepInto(arguments json.RawMessage) (string, error) {
	dbg, err := connectDebugger()
	if err != nil {
		return "", err
	}
	defer dbg.Disconnect()

	state, err := dbg.StepInto()
	if err != nil {
		return "", err
	}
	return jsonResult(state)
}

// GetDebugStepOutTool returns the debug_step_out tool definition.
func GetDebugStepOutTool() Tool {
	return Tool{
		Name: "debug_step_out",
		Description: `Step out of the current function, continuing until it returns to the caller.

Returns the stop state at the caller's next line after the function returns.`,
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}
}

func executeDebugStepOut(arguments json.RawMessage) (string, error) {
	dbg, err := connectDebugger()
	if err != nil {
		return "", err
	}
	defer dbg.Disconnect()

	state, err := dbg.StepOut()
	if err != nil {
		return "", err
	}
	return jsonResult(state)
}

// GetDebugEvalTool returns the debug_eval tool definition.
func GetDebugEvalTool() Tool {
	return Tool{
		Name: "debug_eval",
		Description: `Evaluate an expression in the current debug scope.

For Go: supports any valid Go expression including struct fields, method calls, type assertions, etc.
For TypeScript: supports JavaScript expressions evaluated in the paused scope.

Returns the variable tree with name, type, value, and children for composite types.`,
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"expression": map[string]interface{}{
					"type":        "string",
					"description": "Expression to evaluate in the current scope.",
				},
			},
			"required": []string{"expression"},
		},
	}
}

func executeDebugEval(arguments json.RawMessage) (string, error) {
	var args struct {
		Expression string `json:"expression"`
	}
	if err := json.Unmarshal(arguments, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	dbg, err := connectDebugger()
	if err != nil {
		return "", err
	}
	defer dbg.Disconnect()

	v, err := dbg.Eval(args.Expression)
	if err != nil {
		return "", err
	}
	return jsonResult(v)
}

// GetDebugInspectTool returns the debug_inspect tool definition.
func GetDebugInspectTool() Tool {
	return Tool{
		Name: "debug_inspect",
		Description: `Get the full debugging state at once: local variables, function arguments, and stack trace.

This is a convenience tool that combines the output of locals, args, and stacktrace into a single response. Use this for a quick overview of the current state instead of making multiple calls.`,
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"stack_depth": map[string]interface{}{
					"type":        "integer",
					"description": "Maximum number of stack frames to return. Default is 10.",
					"default":     10,
				},
			},
		},
	}
}

func executeDebugInspect(arguments json.RawMessage) (string, error) {
	var args struct {
		StackDepth int `json:"stack_depth"`
	}
	if err := json.Unmarshal(arguments, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if args.StackDepth <= 0 {
		args.StackDepth = 10
	}

	dbg, err := connectDebugger()
	if err != nil {
		return "", err
	}
	defer dbg.Disconnect()

	locals, err := dbg.Locals()
	if err != nil {
		return "", fmt.Errorf("getting locals: %w", err)
	}

	fnArgs, err := dbg.Args()
	if err != nil {
		return "", fmt.Errorf("getting args: %w", err)
	}

	stack, err := dbg.Stacktrace(args.StackDepth)
	if err != nil {
		return "", fmt.Errorf("getting stacktrace: %w", err)
	}

	return jsonResult(map[string]interface{}{
		"locals": locals,
		"args":   fnArgs,
		"stack":  stack,
	})
}

// GetDebugGoroutinesTool returns the debug_goroutines tool definition.
func GetDebugGoroutinesTool() Tool {
	return Tool{
		Name: "debug_goroutines",
		Description: `List all goroutines in the debugged Go process.

Only available for Go/Delve debug sessions. Returns goroutine IDs, statuses, current file/line/function, and wait reasons for blocked goroutines.`,
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}
}

func executeDebugGoroutines(arguments json.RawMessage) (string, error) {
	session, err := debug.LoadSession(".")
	if err != nil {
		return "", fmt.Errorf("reading debug session: %w", err)
	}
	if session == nil {
		return "", fmt.Errorf("no active debug session — use debug_start first")
	}
	if session.Type != "delve" {
		return "", fmt.Errorf("debug_goroutines is only available for Go/Delve sessions (current session type: %s)", session.Type)
	}

	d := debug.NewDelveDebugger()
	if err := d.Connect(session.Addr); err != nil {
		return "", fmt.Errorf("connecting to Delve at %s: %w", session.Addr, err)
	}
	defer d.Disconnect()

	gs, err := d.Goroutines()
	if err != nil {
		return "", err
	}
	return jsonResult(gs)
}

// GetDebugStopTool returns the debug_stop tool definition.
func GetDebugStopTool() Tool {
	return Tool{
		Name: "debug_stop",
		Description: `End the current debug session.

Disconnects from the debugger, terminates the debugged process, and clears the session file. After this, debug_start must be called again to begin a new session.`,
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}
}

func executeDebugStop(arguments json.RawMessage) (string, error) {
	// Load session before stopping so we can clean up the temp directory.
	session, _ := debug.LoadSession(".")

	dbg, err := connectDebugger()
	if err != nil {
		// Even if we can't connect, try to clear the session file and temp dir.
		if session != nil && session.TmpDir != "" {
			_ = os.RemoveAll(session.TmpDir)
		}
		_ = debug.ClearSession(".")
		return jsonResult(map[string]string{"status": "session cleared (debugger was not reachable)"})
	}

	if err := dbg.Stop(); err != nil {
		// Best-effort: clear the session and temp dir even if stop fails.
		if session != nil && session.TmpDir != "" {
			_ = os.RemoveAll(session.TmpDir)
		}
		_ = debug.ClearSession(".")
		return "", fmt.Errorf("stopping debugger: %w", err)
	}

	if session != nil && session.TmpDir != "" {
		if err := os.RemoveAll(session.TmpDir); err != nil {
			return "", fmt.Errorf("removing temp directory: %w", err)
		}
	}

	if err := debug.ClearSession("."); err != nil {
		return "", fmt.Errorf("clearing session file: %w", err)
	}

	return jsonResult(map[string]string{"status": "stopped"})
}