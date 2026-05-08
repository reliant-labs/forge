package debug

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/go-delve/delve/service/api"
	"github.com/go-delve/delve/service/rpc2"
)

var defaultLoadConfig = api.LoadConfig{
	FollowPointers:     true,
	MaxVariableRecurse: 1,
	MaxStringLen:       256,
	MaxArrayValues:     64,
	MaxStructFields:    -1,
}

// DelveDebugger implements the Debugger interface using Delve's rpc2 client.
type DelveDebugger struct {
	client *rpc2.RPCClient
	addr   string
	cmd    *exec.Cmd // the dlv process we started (nil if we just connected)
	pid    int
}

// NewDelveDebugger returns a new, unconnected DelveDebugger.
func NewDelveDebugger() *DelveDebugger {
	return &DelveDebugger{}
}

// Start launches dlv exec in headless mode for the given binary and connects.
// If listenPort > 0, it is used as the debugger listen port; otherwise a free port is chosen.
func (d *DelveDebugger) Start(binary string, args []string, listenPort int) error {
	port := listenPort
	if port <= 0 {
		var err error
		port, err = freePort()
		if err != nil {
			return fmt.Errorf("finding free port: %w", err)
		}
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	dlvArgs := []string{
		"exec", "--headless",
		"--listen=" + addr,
		"--api-version=2",
		"--accept-multiclient",
		binary,
	}
	if len(args) > 0 {
		dlvArgs = append(dlvArgs, "--")
		dlvArgs = append(dlvArgs, args...)
	}

	d.cmd = exec.Command("dlv", dlvArgs...)
	// Start dlv in its own process group and detach IO so it survives
	// after the parent (forge) exits.
	d.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("opening %s: %w", os.DevNull, err)
	}
	defer devNull.Close()
	d.cmd.Stdin = devNull
	d.cmd.Stdout = devNull
	d.cmd.Stderr = devNull
	if err := d.cmd.Start(); err != nil {
		return fmt.Errorf("starting dlv: %w", err)
	}

	if err := waitForServer(addr, 5*time.Second); err != nil {
		_ = d.cmd.Process.Kill()
		return fmt.Errorf("waiting for dlv: %w", err)
	}

	d.addr = addr
	d.pid = d.cmd.Process.Pid
	// Release the process handle so the Go runtime does not kill dlv
	// when the parent (forge) process exits.
	_ = d.cmd.Process.Release()
	d.cmd = nil
	return nil
}

// StartAttach launches dlv attach in headless mode for the given PID.
func (d *DelveDebugger) StartAttach(pid int) error {
	port, err := freePort()
	if err != nil {
		return fmt.Errorf("finding free port: %w", err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	dlvArgs := []string{
		"attach", "--headless",
		"--listen=" + addr,
		"--api-version=2",
		"--accept-multiclient",
		strconv.Itoa(pid),
	}

	d.cmd = exec.Command("dlv", dlvArgs...)
	if err := d.cmd.Start(); err != nil {
		return fmt.Errorf("starting dlv attach: %w", err)
	}

	if err := waitForServer(addr, 5*time.Second); err != nil {
		_ = d.cmd.Process.Kill()
		return fmt.Errorf("waiting for dlv: %w", err)
	}

	d.addr = addr
	d.pid = pid
	return nil
}

// Connect connects to an already-running Delve instance at addr.
// It applies a timeout so the caller isn't blocked indefinitely when
// Delve's RPC handler is stuck (e.g. due to stale CLOSE_WAIT connections).
func (d *DelveDebugger) Connect(addr string) error {
	type result struct {
		client *rpc2.RPCClient
		pid    int
	}
	ch := make(chan result, 1)
	go func() {
		c := rpc2.NewClient(addr)
		pid := c.ProcessPid()
		ch <- result{client: c, pid: pid}
	}()

	select {
	case r := <-ch:
		d.client = r.client
		d.addr = addr
		d.pid = r.pid
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("timeout connecting to debugger at %s (server may be busy)", addr)
	}
}

// Addr returns the listen address of the Delve server.
func (d *DelveDebugger) Addr() string { return d.addr }

// PID returns the PID of the debugged process.
func (d *DelveDebugger) PID() int { return d.pid }

// ---------------------------------------------------------------------------
// Breakpoints
// ---------------------------------------------------------------------------

func (d *DelveDebugger) SetBreakpoint(file string, line int, condition string) (*BreakpointInfo, error) {
	bp, err := d.client.CreateBreakpoint(&api.Breakpoint{
		File: file,
		Line: line,
		Cond: condition,
	})
	if err != nil {
		return nil, err
	}
	return apiBreakpointToInfo(bp), nil
}

func (d *DelveDebugger) SetFunctionBreakpoint(funcName string, condition string) (*BreakpointInfo, error) {
	bp, err := d.client.CreateBreakpoint(&api.Breakpoint{
		FunctionName: funcName,
		Cond:         condition,
	})
	if err != nil {
		return nil, err
	}
	return apiBreakpointToInfo(bp), nil
}

func (d *DelveDebugger) ListBreakpoints() ([]BreakpointInfo, error) {
	bps, err := d.client.ListBreakpoints(false)
	if err != nil {
		return nil, err
	}
	out := make([]BreakpointInfo, len(bps))
	for i, bp := range bps {
		out[i] = *apiBreakpointToInfo(bp)
	}
	return out, nil
}

func (d *DelveDebugger) ClearBreakpoint(id int) error {
	_, err := d.client.ClearBreakpoint(id)
	return err
}

// ---------------------------------------------------------------------------
// Execution control
// ---------------------------------------------------------------------------

func (d *DelveDebugger) Continue() (*StopState, error) {
	ch := d.client.Continue()
	for state := range ch {
		if state == nil {
			continue
		}
		if state.Err != nil {
			return nil, state.Err
		}
		return stateToStopState(state), nil
	}
	return nil, fmt.Errorf("continue: channel closed without state")
}

func (d *DelveDebugger) StepOver() (*StopState, error) {
	state, err := d.client.Next()
	if err != nil {
		return nil, err
	}
	return stateToStopState(state), nil
}

func (d *DelveDebugger) StepInto() (*StopState, error) {
	state, err := d.client.Step()
	if err != nil {
		return nil, err
	}
	return stateToStopState(state), nil
}

func (d *DelveDebugger) StepOut() (*StopState, error) {
	state, err := d.client.StepOut()
	if err != nil {
		return nil, err
	}
	return stateToStopState(state), nil
}

// ---------------------------------------------------------------------------
// Inspection
// ---------------------------------------------------------------------------

func (d *DelveDebugger) Eval(expr string) (*Variable, error) {
	scope := api.EvalScope{GoroutineID: -1}
	v, err := d.client.EvalVariable(scope, expr, defaultLoadConfig)
	if err != nil {
		return nil, err
	}
	out := apiVarToVariable(v)
	return &out, nil
}

func (d *DelveDebugger) Locals() ([]Variable, error) {
	scope, err := d.currentScope()
	if err != nil {
		return nil, fmt.Errorf("getting current scope: %w", err)
	}
	vars, err := d.client.ListLocalVariables(scope, defaultLoadConfig)
	if err != nil {
		return nil, err
	}
	return apiVarsToVariables(vars), nil
}

func (d *DelveDebugger) Args() ([]Variable, error) {
	scope, err := d.currentScope()
	if err != nil {
		return nil, fmt.Errorf("getting current scope: %w", err)
	}
	vars, err := d.client.ListFunctionArgs(scope, defaultLoadConfig)
	if err != nil {
		return nil, err
	}
	return apiVarsToVariables(vars), nil
}

func (d *DelveDebugger) Stacktrace(depth int) ([]StackFrame, error) {
	frames, err := d.client.Stacktrace(-1, depth, 0, api.StacktraceOptions(0), &defaultLoadConfig)
	if err != nil {
		return nil, err
	}
	out := make([]StackFrame, len(frames))
	for i, f := range frames {
		out[i] = StackFrame{
			Function: funcName(f.Function),
			File:     f.File,
			Line:     f.Line,
			Args:     apiVarsToVariables(f.Arguments),
			Locals:   apiVarsToVariables(f.Locals),
		}
	}
	return out, nil
}

func (d *DelveDebugger) Goroutines() ([]GoroutineInfo, error) {
	goroutines, _, err := d.client.ListGoroutines(0, 0)
	if err != nil {
		return nil, err
	}
	out := make([]GoroutineInfo, len(goroutines))
	for i, g := range goroutines {
		loc := g.UserCurrentLoc
		if loc.File == "" {
			loc = g.CurrentLoc
		}
		out[i] = GoroutineInfo{
			ID:          g.ID,
			Status:      goroutineStatus(g.Status),
			Function:    funcName(loc.Function),
			CurrentFile: loc.File,
			CurrentLine: loc.Line,
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

func (d *DelveDebugger) Stop() error {
	if d.client != nil {
		_ = d.client.Detach(true)
		d.client = nil
	}
	if d.cmd != nil && d.cmd.Process != nil {
		_ = d.cmd.Process.Kill()
		_ = d.cmd.Wait()
		d.cmd = nil
	}
	return nil
}

func (d *DelveDebugger) Disconnect() {
	if d.client != nil {
		_ = d.client.Disconnect(false) // false = don't kill the debugged process
		d.client = nil
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// currentScope returns an EvalScope for the currently selected goroutine.
func (d *DelveDebugger) currentScope() (api.EvalScope, error) {
	state, err := d.client.GetState()
	if err != nil {
		return api.EvalScope{}, err
	}
	goroutineID := int64(-1)
	if state.SelectedGoroutine != nil {
		goroutineID = state.SelectedGoroutine.ID
	}
	return api.EvalScope{GoroutineID: goroutineID}, nil
}

func stateToStopState(state *api.DebuggerState) *StopState {
	ss := &StopState{}

	if state.CurrentThread != nil {
		ss.File = state.CurrentThread.File
		ss.Line = state.CurrentThread.Line
		if state.CurrentThread.Function != nil {
			ss.Function = state.CurrentThread.Function.Name()
		}
		if state.CurrentThread.Breakpoint != nil {
			ss.Reason = "breakpoint"
		}
	}

	if state.SelectedGoroutine != nil {
		ss.GoroutineID = state.SelectedGoroutine.ID
	}

	if ss.Reason == "" {
		ss.Reason = "step"
	}

	return ss
}

func apiBreakpointToInfo(bp *api.Breakpoint) *BreakpointInfo {
	return &BreakpointInfo{
		ID:           bp.ID,
		File:         bp.File,
		Line:         bp.Line,
		FunctionName: bp.FunctionName,
		Condition:    bp.Cond,
		HitCount:     bp.TotalHitCount,
	}
}

func apiVarToVariable(v *api.Variable) Variable {
	out := Variable{
		Name:  v.Name,
		Type:  v.Type,
		Value: v.Value,
	}
	if len(v.Children) > 0 {
		out.Children = make([]Variable, len(v.Children))
		for i := range v.Children {
			out.Children[i] = apiVarToVariable(&v.Children[i])
		}
	}
	return out
}

func apiVarsToVariables(vars []api.Variable) []Variable {
	if len(vars) == 0 {
		return nil
	}
	out := make([]Variable, len(vars))
	for i := range vars {
		out[i] = apiVarToVariable(&vars[i])
	}
	return out
}

func funcName(fn *api.Function) string {
	if fn == nil {
		return ""
	}
	return fn.Name()
}

func goroutineStatus(status uint64) string {
	switch status {
	case 0:
		return "idle"
	case 1:
		return "runnable"
	case 2:
		return "running"
	case 3:
		return "syscall"
	case 4:
		return "waiting"
	case 6:
		return "dead"
	default:
		return fmt.Sprintf("unknown(%d)", status)
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port, nil
}

func waitForServer(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for dlv at %s", addr)
}