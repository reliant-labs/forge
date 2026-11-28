package debug

import (
	"errors"
	"net"
	"net/rpc"
	"net/rpc/jsonrpc"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/go-delve/delve/service/api"
)

// scriptDlv is a scriptable Delve rpc2 server covering the whole surface
// DelveDebugger drives. Every request is recorded; each method's outcome comes
// from fields a test sets before serving. It complements fakeDlv, which only
// models the lifecycle and wait paths.
type scriptDlv struct {
	mu sync.Mutex

	state      api.DebuggerState   // what State answers once stateQueue is empty
	stateQueue []api.DebuggerState // consumed one per State call, in order
	contQueue  []api.DebuggerState // consumed one per Continue command, in order
	pid        int
	apiErr     string // SetApiVersion fails with this message

	bps        []*api.Breakpoint
	createFunc func(in FakeCreateIn) (api.Breakpoint, error)
	locals     []api.Variable
	args       []api.Variable
	evalOut    *api.Variable
	frames     []api.Stackframe
	goroutines []*api.Goroutine

	errs  map[string]string   // method (or "Command:<name>") -> error message
	hangs map[string]struct{} // methods that never answer
	hang  chan struct{}

	hangStateAt int // the Nth State call (1-based) never answers; 0 = never
	stateCalls  int
	onCommand   func(name string)

	commands  []string
	creates   []FakeCreateIn
	scopes    []api.EvalScope
	exprs     []string
	stackIn   []FakeStackIn
	detaches  []bool
	pidCalled int
}

type (
	FakeCreateIn struct {
		Breakpoint api.Breakpoint
		LocExpr    string
	}
	FakeCreateOut struct{ Breakpoint api.Breakpoint }
	FakeVarsIn    struct{ Scope api.EvalScope }
	FakeLocalsOut struct{ Variables []api.Variable }
	FakeArgsOut   struct{ Args []api.Variable }
	FakeEvalIn    struct {
		Scope api.EvalScope
		Expr  string
	}
	FakeEvalOut struct{ Variable *api.Variable }
	FakeStackIn struct {
		Id    int64 //nolint:revive // delve's wire name
		Depth int
		Skip  int
	}
	FakeStackOut struct{ Locations []api.Stackframe }
	FakeGoIn     struct{ Start, Count int }
	FakeGoOut    struct {
		Goroutines []*api.Goroutine
		Nextg      int
	}
	FakePidOut struct{ Pid int }
)

// gate applies the scripted hang and error for method.
func (f *scriptDlv) gate(method string) error {
	f.mu.Lock()
	_, hangs := f.hangs[method]
	msg, fails := f.errs[method]
	f.mu.Unlock()
	if hangs {
		<-f.hang
	}
	if fails {
		return errors.New(msg)
	}
	return nil
}

func (f *scriptDlv) SetApiVersion(_ api.SetAPIVersionIn, _ *api.SetAPIVersionOut) error { //nolint:revive // delve's method name
	if f.apiErr != "" {
		return errors.New(f.apiErr)
	}
	return nil
}

func (f *scriptDlv) State(_ FakeStateIn, out *FakeStateOut) error {
	f.mu.Lock()
	f.stateCalls++
	hangNow := f.hangStateAt != 0 && f.stateCalls == f.hangStateAt
	f.mu.Unlock()
	if hangNow {
		<-f.hang
	}
	if err := f.gate("State"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.state
	if len(f.stateQueue) > 0 {
		st, f.stateQueue = f.stateQueue[0], f.stateQueue[1:]
	}
	out.State = &st
	return nil
}

func (f *scriptDlv) Command(cmd api.DebuggerCommand, out *FakeCmdOut) error {
	f.mu.Lock()
	f.commands = append(f.commands, cmd.Name)
	hook := f.onCommand
	f.mu.Unlock()
	if hook != nil {
		hook(cmd.Name)
	}
	if err := f.gate("Command"); err != nil {
		return err
	}
	if err := f.gate("Command:" + cmd.Name); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case cmd.Name == api.Halt:
		f.state.Running = false
	case cmd.Name == api.Continue && len(f.contQueue) > 0:
		out.State, f.contQueue = f.contQueue[0], f.contQueue[1:]
		return nil
	}
	out.State = f.state
	return nil
}

func (f *scriptDlv) CreateBreakpoint(in FakeCreateIn, out *FakeCreateOut) error {
	f.mu.Lock()
	f.creates = append(f.creates, in)
	create := f.createFunc
	f.mu.Unlock()
	if err := f.gate("CreateBreakpoint"); err != nil {
		return err
	}
	if create == nil {
		out.Breakpoint = in.Breakpoint
		out.Breakpoint.ID = 1
		return nil
	}
	bp, err := create(in)
	out.Breakpoint = bp
	return err
}

func (f *scriptDlv) ListBreakpoints(_ FakeListIn, out *FakeListOut) error {
	if err := f.gate("ListBreakpoints"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out.Breakpoints = f.bps
	return nil
}

func (f *scriptDlv) ClearBreakpoint(in FakeClearIn, out *FakeClearOut) error {
	if err := f.gate("ClearBreakpoint"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out.Breakpoint = &api.Breakpoint{ID: in.Id}
	return nil
}

func (f *scriptDlv) ListLocalVars(in FakeVarsIn, out *FakeLocalsOut) error {
	f.mu.Lock()
	f.scopes = append(f.scopes, in.Scope)
	f.mu.Unlock()
	if err := f.gate("ListLocalVars"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out.Variables = f.locals
	return nil
}

func (f *scriptDlv) ListFunctionArgs(in FakeVarsIn, out *FakeArgsOut) error {
	f.mu.Lock()
	f.scopes = append(f.scopes, in.Scope)
	f.mu.Unlock()
	if err := f.gate("ListFunctionArgs"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out.Args = f.args
	return nil
}

func (f *scriptDlv) Eval(in FakeEvalIn, out *FakeEvalOut) error {
	f.mu.Lock()
	f.scopes = append(f.scopes, in.Scope)
	f.exprs = append(f.exprs, in.Expr)
	f.mu.Unlock()
	if err := f.gate("Eval"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out.Variable = f.evalOut
	return nil
}

func (f *scriptDlv) Stacktrace(in FakeStackIn, out *FakeStackOut) error {
	f.mu.Lock()
	f.stackIn = append(f.stackIn, in)
	f.mu.Unlock()
	if err := f.gate("Stacktrace"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out.Locations = f.frames
	return nil
}

func (f *scriptDlv) ListGoroutines(_ FakeGoIn, out *FakeGoOut) error {
	if err := f.gate("ListGoroutines"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out.Goroutines = f.goroutines
	return nil
}

func (f *scriptDlv) ProcessPid(_ FakeEmpty, out *FakePidOut) error {
	f.mu.Lock()
	f.pidCalled++
	out.Pid = f.pid
	f.mu.Unlock()
	return nil
}

func (f *scriptDlv) Detach(in FakeDetachIn, _ *FakeEmpty) error {
	f.mu.Lock()
	f.detaches = append(f.detaches, in.Kill)
	f.mu.Unlock()
	return f.gate("Detach")
}

func (f *scriptDlv) sentCommands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.commands)
}

// waitForCommands polls until the server has recorded at least n commands: a
// resume is written without waiting for its reply, so the server sees it a
// moment after the client call returns.
func (f *scriptDlv) waitForCommands(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := f.sentCommands(); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("server saw only %v, want %d commands", f.sentCommands(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// listenScript serves f on addr (host:port) until stop is closed or the
// returned listener is closed.
func listenScript(f *scriptDlv, l net.Listener) {
	srv := rpc.NewServer()
	if err := srv.RegisterName("RPCServer", f); err != nil {
		panic(err)
	}
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go srv.ServeCodec(jsonrpc.NewServerCodec(conn))
	}
}

// serveScript serves f on a loopback port and returns a DelveDebugger
// connected to it, plus f for assertions.
func serveScript(t *testing.T, f *scriptDlv) *DelveDebugger {
	t.Helper()
	f.hang = make(chan struct{})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen on loopback: %v", err)
	}
	go listenScript(f, l)
	t.Cleanup(func() {
		close(f.hang)
		_ = l.Close()
	})
	c, err := newRPCClient(l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &DelveDebugger{client: c, addr: l.Addr().String()}
}

func runningState() api.DebuggerState { return api.DebuggerState{Running: true} }
