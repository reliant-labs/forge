package debug

// Debugger is the common interface for interacting with a running debugger.
type Debugger interface {
	// Breakpoints
	SetBreakpoint(file string, line int, condition string) (*BreakpointInfo, error)
	SetFunctionBreakpoint(funcName string, condition string) (*BreakpointInfo, error)
	ListBreakpoints() ([]BreakpointInfo, error)
	ClearBreakpoint(id int) error

	// Execution control
	Continue() (*StopState, error)
	StepOver() (*StopState, error)
	StepInto() (*StopState, error)
	StepOut() (*StopState, error)

	// Inspection
	Eval(expr string) (*Variable, error)
	Locals() ([]Variable, error)
	Args() ([]Variable, error)
	Stacktrace(depth int) ([]StackFrame, error)
	Goroutines() ([]GoroutineInfo, error)

	// Lifecycle
	Stop() error
	Disconnect()
}
