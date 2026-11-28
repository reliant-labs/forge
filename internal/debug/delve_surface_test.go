package debug

import (
	"net"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-delve/delve/service/api"
)

func shrinkStopSettle(t *testing.T) {
	t.Helper()
	old := stopSettle
	stopSettle = 5 * time.Millisecond
	t.Cleanup(func() { stopSettle = old })
}

func TestConnect(t *testing.T) {
	t.Run("a halted target's pid is read", func(t *testing.T) {
		f := &scriptDlv{pid: 77}
		addr := serveScript(t, f).Addr()
		d := NewDelveDebugger()
		if err := d.Connect(addr); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		if d.PID() != 77 || d.Addr() != addr {
			t.Errorf("PID/Addr = %d, %q; want 77, %q", d.PID(), d.Addr(), addr)
		}
		if d.DlvPID() != 0 {
			t.Errorf("DlvPID = %d; a debugger that only connected owns no dlv process", d.DlvPID())
		}
	})

	t.Run("a running target is not asked for its pid", func(t *testing.T) {
		// ProcessPid takes delve's target lock, which a running target holds.
		f := &scriptDlv{pid: 77, state: runningState()}
		addr := serveScript(t, f).Addr()
		d := NewDelveDebugger()
		if err := d.Connect(addr); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		if d.PID() != 0 || f.pidCalled != 0 {
			t.Errorf("PID = %d, ProcessPid calls = %d; want neither for a running target", d.PID(), f.pidCalled)
		}
	})

	t.Run("a server that refuses the API version is an error", func(t *testing.T) {
		l, lerr := net.Listen("tcp", "127.0.0.1:0")
		if lerr != nil {
			t.Skip("no loopback")
		}
		t.Cleanup(func() { _ = l.Close() })
		go listenScript(&scriptDlv{apiErr: "unsupported version"}, l)
		err := NewDelveDebugger().Connect(l.Addr().String())
		if err == nil || !strings.Contains(err.Error(), "negotiating delve API version") {
			t.Fatalf("Connect = %v, want the negotiation error", err)
		}
	})

	t.Run("nothing listening is an error naming the address", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Skip("no loopback")
		}
		addr := l.Addr().String()
		_ = l.Close()
		err = NewDelveDebugger().Connect(addr)
		if err == nil || !strings.Contains(err.Error(), "dialing delve at "+addr) {
			t.Fatalf("Connect = %v, want a dial error naming %s", err, addr)
		}
	})
}

func TestResolvePID(t *testing.T) {
	t.Run("a known pid is returned without a round trip", func(t *testing.T) {
		f := &scriptDlv{pid: 9}
		d := serveScript(t, f)
		d.pid = 55
		pid, err := d.ResolvePID()
		if err != nil || pid != 55 || f.pidCalled != 0 {
			t.Errorf("ResolvePID = (%d, %v), ProcessPid calls %d; want 55 with no call", pid, err, f.pidCalled)
		}
	})

	t.Run("a running target is halted for the query then resumed", func(t *testing.T) {
		f := &scriptDlv{pid: 31, state: runningState()}
		d := serveScript(t, f)
		pid, err := d.ResolvePID()
		if err != nil || pid != 31 {
			t.Fatalf("ResolvePID = (%d, %v), want 31", pid, err)
		}
		if got := f.waitForCommands(t, 2); !slices.Equal(got, []string{api.Halt, api.Continue}) {
			t.Errorf("commands = %v, want halt then continue", got)
		}
		if d.PID() != 31 {
			t.Errorf("the resolved pid must be remembered, PID() = %d", d.PID())
		}
	})

	t.Run("a halted target is left halted", func(t *testing.T) {
		f := &scriptDlv{pid: 31}
		d := serveScript(t, f)
		if pid, err := d.ResolvePID(); err != nil || pid != 31 {
			t.Fatalf("ResolvePID = (%d, %v)", pid, err)
		}
		if got := f.sentCommands(); len(got) != 0 {
			t.Errorf("commands = %v, want none for a halted target", got)
		}
	})

	t.Run("a halt failure is reported", func(t *testing.T) {
		f := &scriptDlv{state: runningState(), errs: map[string]string{"Command:" + api.Halt: "cannot halt"}}
		d := serveScript(t, f)
		_, err := d.ResolvePID()
		if err == nil || !strings.Contains(err.Error(), "halting target") || !strings.Contains(err.Error(), "cannot halt") {
			t.Errorf("ResolvePID = %v, want the halt failure", err)
		}
	})
}

func TestSetLineBreakpoint(t *testing.T) {
	t.Run("maps the created breakpoint and sends file, line and condition", func(t *testing.T) {
		f := &scriptDlv{createFunc: func(in FakeCreateIn) (api.Breakpoint, error) {
			bp := in.Breakpoint
			bp.ID, bp.FunctionName, bp.TotalHitCount = 3, "main.compute", 5
			return bp, nil
		}}
		d := serveScript(t, f)
		got, err := d.SetBreakpoint("/src/main.go", 12, "i > 3")
		if err != nil {
			t.Fatalf("SetBreakpoint: %v", err)
		}
		want := &BreakpointInfo{ID: 3, File: "/src/main.go", Line: 12, FunctionName: "main.compute", Condition: "i > 3", HitCount: 5}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("SetBreakpoint = %+v, want %+v", got, want)
		}
		if len(f.creates) != 1 || f.creates[0].Breakpoint.File != "/src/main.go" || f.creates[0].Breakpoint.Line != 12 || f.creates[0].Breakpoint.Cond != "i > 3" {
			t.Errorf("server saw %+v", f.creates)
		}
	})

	t.Run("falls through spellings until one resolves", func(t *testing.T) {
		f := &scriptDlv{createFunc: func(in FakeCreateIn) (api.Breakpoint, error) {
			if in.Breakpoint.File != "mod/main.go" {
				return api.Breakpoint{}, errString("could not find " + in.Breakpoint.File)
			}
			bp := in.Breakpoint
			bp.ID = 8
			return bp, nil
		}}
		d := serveScript(t, f)
		got, err := d.SetLineBreakpoint([]string{"", "/abs/main.go", "mod/main.go"}, "", 7, "")
		if err != nil || got.File != "mod/main.go" || got.ID != 8 {
			t.Fatalf("SetLineBreakpoint = (%+v, %v), want the third spelling", got, err)
		}
		var tried []string
		for _, c := range f.creates {
			tried = append(tried, c.Breakpoint.File)
		}
		if !slices.Equal(tried, []string{"/abs/main.go", "mod/main.go"}) {
			t.Errorf("tried %v; an empty spelling must be skipped and the search must stop at the first hit", tried)
		}
	})

	t.Run("a location expression is the last resort", func(t *testing.T) {
		f := &scriptDlv{createFunc: func(in FakeCreateIn) (api.Breakpoint, error) {
			if in.LocExpr == "" {
				return api.Breakpoint{}, errString("no such file")
			}
			return api.Breakpoint{ID: 2, File: "/resolved/x.go", Line: 42}, nil
		}}
		d := serveScript(t, f)
		got, err := d.SetLineBreakpoint([]string{"/a.go"}, "pkg/x.go", 42, "c")
		if err != nil || got.ID != 2 {
			t.Fatalf("SetLineBreakpoint = (%+v, %v)", got, err)
		}
		last := f.creates[len(f.creates)-1]
		if last.LocExpr != "pkg/x.go:42" || last.Breakpoint.Cond != "c" {
			t.Errorf("last request = %+v, want LocExpr pkg/x.go:42 carrying the condition", last)
		}
	})

	t.Run("when every spelling fails the first error is returned", func(t *testing.T) {
		f := &scriptDlv{createFunc: func(in FakeCreateIn) (api.Breakpoint, error) {
			return api.Breakpoint{}, errString("bad: " + in.Breakpoint.File + in.LocExpr)
		}}
		d := serveScript(t, f)
		_, err := d.SetLineBreakpoint([]string{"/first.go", "/second.go"}, "loc.go", 1, "")
		if err == nil || !strings.Contains(err.Error(), "bad: /first.go") {
			t.Fatalf("err = %v, want the FIRST spelling's error", err)
		}
	})

	t.Run("a running target is halted around the call and resumed", func(t *testing.T) {
		f := &scriptDlv{state: runningState()}
		d := serveScript(t, f)
		if _, err := d.SetBreakpoint("a.go", 1, ""); err != nil {
			t.Fatal(err)
		}
		if got := f.waitForCommands(t, 2); !slices.Equal(got, []string{api.Halt, api.Continue}) {
			t.Errorf("commands = %v, want halt then continue", got)
		}
	})

	t.Run("a halted target stays halted", func(t *testing.T) {
		f := &scriptDlv{}
		d := serveScript(t, f)
		if _, err := d.SetBreakpoint("a.go", 1, ""); err != nil {
			t.Fatal(err)
		}
		if got := f.sentCommands(); len(got) != 0 {
			t.Errorf("commands = %v, want none", got)
		}
	})
}

type errString string

func (e errString) Error() string { return string(e) }

func TestSetFunctionBreakpoint(t *testing.T) {
	f := &scriptDlv{}
	d := serveScript(t, f)
	got, err := d.SetFunctionBreakpoint("main.compute", "i == 2")
	if err != nil {
		t.Fatal(err)
	}
	if got.FunctionName != "main.compute" || got.Condition != "i == 2" || got.ID != 1 {
		t.Errorf("SetFunctionBreakpoint = %+v", got)
	}

	f2 := &scriptDlv{errs: map[string]string{"CreateBreakpoint": "location not found"}}
	d2 := serveScript(t, f2)
	if _, err := d2.SetFunctionBreakpoint("nope", ""); err == nil || !strings.Contains(err.Error(), "location not found") {
		t.Errorf("err = %v, want delve's message", err)
	}
}

func TestListAndClearBreakpoints(t *testing.T) {
	f := &scriptDlv{bps: []*api.Breakpoint{
		{ID: 1, File: "a.go", Line: 3, TotalHitCount: 2},
		{ID: 2, FunctionName: "main.f", Cond: "x"},
	}}
	d := serveScript(t, f)
	got, err := d.ListBreakpoints()
	if err != nil {
		t.Fatal(err)
	}
	want := []BreakpointInfo{
		{ID: 1, File: "a.go", Line: 3, HitCount: 2},
		{ID: 2, FunctionName: "main.f", Condition: "x"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListBreakpoints = %+v, want %+v", got, want)
	}

	if err := d.ClearBreakpoint(2); err != nil {
		t.Errorf("ClearBreakpoint: %v", err)
	}

	bad := &scriptDlv{errs: map[string]string{"ListBreakpoints": "boom", "ClearBreakpoint": "gone"}}
	db := serveScript(t, bad)
	if _, err := db.ListBreakpoints(); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("ListBreakpoints err = %v", err)
	}
	if err := db.ClearBreakpoint(9); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Errorf("ClearBreakpoint err = %v", err)
	}
}

func TestWithHaltedStateError(t *testing.T) {
	f := &scriptDlv{errs: map[string]string{"State": "connection reset"}}
	d := serveScript(t, f)
	if _, err := d.ListBreakpoints(); err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("err = %v, want the state error", err)
	}
	if err := d.ClearBreakpoint(1); err == nil {
		t.Error("ClearBreakpoint must fail when the state cannot be read")
	}
}

func TestResume(t *testing.T) {
	t.Run("a halted target is continued", func(t *testing.T) {
		f := &scriptDlv{}
		d := serveScript(t, f)
		if err := d.Resume(); err != nil {
			t.Fatal(err)
		}
		if got := f.waitForCommands(t, 1); !slices.Equal(got, []string{api.Continue}) {
			t.Errorf("commands = %v", got)
		}
	})
	t.Run("a running target is left alone", func(t *testing.T) {
		f := &scriptDlv{state: runningState()}
		d := serveScript(t, f)
		if err := d.Resume(); err != nil {
			t.Fatal(err)
		}
		if got := f.sentCommands(); len(got) != 0 {
			t.Errorf("commands = %v, want none", got)
		}
	})
	t.Run("a state failure is returned", func(t *testing.T) {
		d := serveScript(t, &scriptDlv{errs: map[string]string{"State": "down"}})
		if err := d.Resume(); err == nil {
			t.Error("want an error")
		}
	})
}

func TestState(t *testing.T) {
	tests := []struct {
		name      string
		f         *scriptDlv
		want      TargetState
		wantErr   string
		wantNoErr bool
	}{
		{"halted", &scriptDlv{}, TargetHalted, "", true},
		{"running", &scriptDlv{state: runningState()}, TargetRunning, "", true},
		{"exited by state", &scriptDlv{state: api.DebuggerState{Exited: true, ExitStatus: 3}}, TargetExited, "exited with status 3", false},
		{"delve says the process exited", &scriptDlv{errs: map[string]string{"State": "Process 12 has exited with status 0"}}, TargetExited, "has exited", false},
		{"any other error leaves the state halted", &scriptDlv{errs: map[string]string{"State": "broken pipe"}}, TargetHalted, "broken pipe", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := serveScript(t, tt.f)
			d.pid = 12
			got, err := d.State()
			if got != tt.want {
				t.Errorf("State = %v, want %v", got, tt.want)
			}
			if tt.wantNoErr {
				if err != nil {
					t.Errorf("err = %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestCurrentStop(t *testing.T) {
	st := haltedAtBreakpoint()
	st.CurrentThread.File, st.CurrentThread.Line = "main.go", 14
	st.SelectedGoroutine = &api.Goroutine{ID: 5}
	d := serveScript(t, &scriptDlv{state: st})
	got, err := d.CurrentStop()
	if err != nil {
		t.Fatal(err)
	}
	want := &StopState{File: "main.go", Line: 14, Function: "main.tick", Reason: "breakpoint", GoroutineID: 5}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CurrentStop = %+v, want %+v", got, want)
	}

	if _, err := serveScript(t, &scriptDlv{errs: map[string]string{"State": "down"}}).CurrentStop(); err == nil {
		t.Error("want the state error")
	}
}

func TestStateToStopState_NoThread(t *testing.T) {
	got := stateToStopState(&api.DebuggerState{})
	if got.Reason != "step" || got.File != "" || got.GoroutineID != 0 {
		t.Errorf("stateToStopState of an empty state = %+v, want a bare step stop", got)
	}
}

func TestStepCommands(t *testing.T) {
	stepped := api.DebuggerState{CurrentThread: &api.Thread{File: "main.go", Line: 20, Function: &api.Function{Name_: "main.compute"}}}
	tests := []struct {
		name string
		op   func(*DelveDebugger) (*StopState, error)
		cmd  string
	}{
		{"StepOver", (*DelveDebugger).StepOver, api.Next},
		{"StepInto", (*DelveDebugger).StepInto, api.Step},
		{"StepOut", (*DelveDebugger).StepOut, api.StepOut},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &scriptDlv{state: stepped}
			got, err := tt.op(serveScript(t, f))
			if err != nil {
				t.Fatal(err)
			}
			if got.File != "main.go" || got.Line != 20 || got.Function != "main.compute" || got.Reason != "step" {
				t.Errorf("stop = %+v", got)
			}
			if cmds := f.sentCommands(); !slices.Equal(cmds, []string{tt.cmd}) {
				t.Errorf("commands = %v, want [%s]", cmds, tt.cmd)
			}
		})
		t.Run(tt.name+" halts a running target first", func(t *testing.T) {
			f := &scriptDlv{state: runningState()}
			if _, err := tt.op(serveScript(t, f)); err != nil {
				t.Fatal(err)
			}
			if cmds := f.sentCommands(); !slices.Equal(cmds, []string{api.Halt, tt.cmd}) {
				t.Errorf("commands = %v, want halt then %s", cmds, tt.cmd)
			}
		})
		t.Run(tt.name+" surfaces delve's error", func(t *testing.T) {
			f := &scriptDlv{errs: map[string]string{"Command:" + tt.cmd: "cannot step"}}
			if _, err := tt.op(serveScript(t, f)); err == nil || !strings.Contains(err.Error(), "cannot step") {
				t.Errorf("err = %v", err)
			}
		})
		t.Run(tt.name+" fails when the state cannot be read", func(t *testing.T) {
			f := &scriptDlv{errs: map[string]string{"State": "down"}}
			if _, err := tt.op(serveScript(t, f)); err == nil {
				t.Error("want an error")
			}
		})
		t.Run(tt.name+" fails when the target cannot be halted", func(t *testing.T) {
			f := &scriptDlv{state: runningState(), errs: map[string]string{"Command:" + api.Halt: "stuck"}}
			if _, err := tt.op(serveScript(t, f)); err == nil || !strings.Contains(err.Error(), "halting target") {
				t.Errorf("err = %v, want the halt failure", err)
			}
		})
	}
}

func TestEval(t *testing.T) {
	f := &scriptDlv{evalOut: &api.Variable{
		Name: "p", Type: "main.Point", Value: "{...}",
		Children: []api.Variable{{Name: "X", Type: "int", Value: "1"}, {Name: "Y", Type: "int", Value: "2"}},
	}}
	d := serveScript(t, f)
	got, err := d.Eval("p")
	if err != nil {
		t.Fatal(err)
	}
	want := &Variable{Name: "p", Type: "main.Point", Value: "{...}", Children: []Variable{
		{Name: "X", Type: "int", Value: "1"}, {Name: "Y", Type: "int", Value: "2"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Eval = %+v, want %+v", got, want)
	}
	if !slices.Equal(f.exprs, []string{"p"}) || f.scopes[0].GoroutineID != -1 {
		t.Errorf("server saw exprs %v scopes %+v; want expr p in the current goroutine (-1)", f.exprs, f.scopes)
	}

	bad := &scriptDlv{errs: map[string]string{"Eval": "could not find symbol value for q"}}
	if _, err := serveScript(t, bad).Eval("q"); err == nil || !strings.Contains(err.Error(), "could not find symbol") {
		t.Errorf("err = %v", err)
	}
}

func TestLocalsAndArgs(t *testing.T) {
	vars := []api.Variable{{Name: "a", Type: "int", Value: "4"}, {Name: "b", Type: "int", Value: "11"}}
	wantVars := []Variable{{Name: "a", Type: "int", Value: "4"}, {Name: "b", Type: "int", Value: "11"}}
	for _, tt := range []struct {
		name string
		op   func(*DelveDebugger) ([]Variable, error)
		set  func(*scriptDlv)
		rpc  string
	}{
		{"Locals", (*DelveDebugger).Locals, func(f *scriptDlv) { f.locals = vars }, "ListLocalVars"},
		{"Args", (*DelveDebugger).Args, func(f *scriptDlv) { f.args = vars }, "ListFunctionArgs"},
	} {
		t.Run(tt.name+" uses the selected goroutine", func(t *testing.T) {
			st := haltedAtBreakpoint()
			st.SelectedGoroutine = &api.Goroutine{ID: 7}
			f := &scriptDlv{state: st}
			tt.set(f)
			got, err := tt.op(serveScript(t, f))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, wantVars) {
				t.Errorf("got %+v, want %+v", got, wantVars)
			}
			if len(f.scopes) != 1 || f.scopes[0].GoroutineID != 7 {
				t.Errorf("scopes = %+v, want goroutine 7", f.scopes)
			}
		})
		t.Run(tt.name+" falls back to the current goroutine", func(t *testing.T) {
			f := &scriptDlv{}
			tt.set(f)
			if _, err := tt.op(serveScript(t, f)); err != nil {
				t.Fatal(err)
			}
			if f.scopes[0].GoroutineID != -1 {
				t.Errorf("scope = %+v, want goroutine -1 when none is selected", f.scopes[0])
			}
		})
		t.Run(tt.name+" with nothing to report is nil", func(t *testing.T) {
			got, err := tt.op(serveScript(t, &scriptDlv{}))
			if err != nil || got != nil {
				t.Errorf("got (%v, %v), want (nil, nil)", got, err)
			}
		})
		t.Run(tt.name+" reports a scope failure", func(t *testing.T) {
			f := &scriptDlv{errs: map[string]string{"State": "down"}}
			if _, err := tt.op(serveScript(t, f)); err == nil || !strings.Contains(err.Error(), "getting current scope") {
				t.Errorf("err = %v", err)
			}
		})
		t.Run(tt.name+" reports delve's error", func(t *testing.T) {
			f := &scriptDlv{errs: map[string]string{tt.rpc: "frame unavailable"}}
			if _, err := tt.op(serveScript(t, f)); err == nil || !strings.Contains(err.Error(), "frame unavailable") {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestStacktrace(t *testing.T) {
	f := &scriptDlv{frames: []api.Stackframe{
		{
			Location:  api.Location{File: "main.go", Line: 9, Function: &api.Function{Name_: "main.compute"}},
			Arguments: []api.Variable{{Name: "i", Type: "int", Value: "3"}},
			Locals:    []api.Variable{{Name: "a", Type: "int", Value: "6"}},
		},
		{Location: api.Location{File: "main.go", Line: 21}},
	}}
	d := serveScript(t, f)
	got, err := d.Stacktrace(5)
	if err != nil {
		t.Fatal(err)
	}
	want := []StackFrame{
		{Function: "main.compute", File: "main.go", Line: 9, Args: []Variable{{Name: "i", Type: "int", Value: "3"}}, Locals: []Variable{{Name: "a", Type: "int", Value: "6"}}},
		{Function: "", File: "main.go", Line: 21},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Stacktrace = %+v, want %+v", got, want)
	}
	if len(f.stackIn) != 1 || f.stackIn[0].Id != -1 || f.stackIn[0].Depth != 5 {
		t.Errorf("request = %+v, want the current goroutine (-1) at depth 5", f.stackIn)
	}

	for name, bad := range map[string]*scriptDlv{
		"state":      {errs: map[string]string{"State": "down"}},
		"stacktrace": {errs: map[string]string{"Stacktrace": "no frames"}},
	} {
		if _, err := serveScript(t, bad).Stacktrace(3); err == nil {
			t.Errorf("%s failure: want an error", name)
		}
	}
}

func TestGoroutines(t *testing.T) {
	f := &scriptDlv{goroutines: []*api.Goroutine{
		{ID: 1, Status: 2, UserCurrentLoc: api.Location{File: "main.go", Line: 30, Function: &api.Function{Name_: "main.main"}}},
		{ID: 2, Status: 4, CurrentLoc: api.Location{File: "runtime/proc.go", Line: 400, Function: &api.Function{Name_: "runtime.gopark"}}},
		{ID: 3, Status: 99},
	}}
	d := serveScript(t, f)
	got, err := d.Goroutines()
	if err != nil {
		t.Fatal(err)
	}
	want := []GoroutineInfo{
		{ID: 1, Status: "running", Function: "main.main", CurrentFile: "main.go", CurrentLine: 30},
		{ID: 2, Status: "waiting", Function: "runtime.gopark", CurrentFile: "runtime/proc.go", CurrentLine: 400},
		{ID: 3, Status: "unknown(99)"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Goroutines = %+v, want %+v", got, want)
	}

	// A running target is halted first: its goroutine cache is not primed.
	fr := &scriptDlv{state: runningState()}
	if _, err := serveScript(t, fr).Goroutines(); err != nil {
		t.Fatal(err)
	}
	if cmds := fr.sentCommands(); !slices.Equal(cmds, []string{api.Halt}) {
		t.Errorf("commands = %v, want a halt before listing", cmds)
	}

	if _, err := serveScript(t, &scriptDlv{errs: map[string]string{"ListGoroutines": "no allgs"}}).Goroutines(); err == nil {
		t.Error("want delve's error")
	}
	if _, err := serveScript(t, &scriptDlv{errs: map[string]string{"State": "down"}}).Goroutines(); err == nil {
		t.Error("want the state error")
	}
}

func TestGoroutineStatus(t *testing.T) {
	tests := map[uint64]string{
		0: "idle", 1: "runnable", 2: "running", 3: "syscall", 4: "waiting", 6: "dead",
		5: "unknown(5)", 7: "unknown(7)",
	}
	for in, want := range tests {
		if got := goroutineStatus(in); got != want {
			t.Errorf("goroutineStatus(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFuncName(t *testing.T) {
	if got := funcName(nil); got != "" {
		t.Errorf("funcName(nil) = %q, want \"\"", got)
	}
	if got := funcName(&api.Function{Name_: "main.f"}); got != "main.f" {
		t.Errorf("funcName = %q", got)
	}
}

func TestApiVarToVariable_NestedChildren(t *testing.T) {
	in := &api.Variable{Name: "root", Type: "T", Value: "v", Children: []api.Variable{
		{Name: "a", Children: []api.Variable{{Name: "a1", Value: "deep"}}},
		{Name: "b"},
	}}
	want := Variable{Name: "root", Type: "T", Value: "v", Children: []Variable{
		{Name: "a", Children: []Variable{{Name: "a1", Value: "deep"}}},
		{Name: "b"},
	}}
	if got := apiVarToVariable(in); !reflect.DeepEqual(got, want) {
		t.Errorf("apiVarToVariable = %+v, want %+v", got, want)
	}
}

func TestStopAndDetach_KillFlagAndLifecycle(t *testing.T) {
	t.Run("Stop kills the debuggee and forgets the client", func(t *testing.T) {
		f := &scriptDlv{}
		d := serveScript(t, f)
		if err := d.Stop(); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(f.detaches, []bool{true}) {
			t.Errorf("detaches = %v, want one with kill=true", f.detaches)
		}
		if d.client != nil {
			t.Error("Stop must drop the client")
		}
		if err := d.Stop(); err != nil {
			t.Errorf("a second Stop must be a no-op, got %v", err)
		}
	})

	t.Run("Detach leaves the target alive", func(t *testing.T) {
		f := &scriptDlv{}
		d := serveScript(t, f)
		if err := d.Detach(); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(f.detaches, []bool{false}) {
			t.Errorf("detaches = %v, want one with kill=false", f.detaches)
		}
		if err := d.Detach(); err == nil || !strings.Contains(err.Error(), "not connected") {
			t.Errorf("a second Detach = %v, want not connected", err)
		}
	})

	t.Run("a running target is halted before the detach", func(t *testing.T) {
		f := &scriptDlv{state: runningState()}
		d := serveScript(t, f)
		if err := d.Stop(); err != nil {
			t.Fatal(err)
		}
		if cmds := f.sentCommands(); !slices.Equal(cmds, []string{api.Halt}) {
			t.Errorf("commands = %v, want a halt before detach", cmds)
		}
	})

	t.Run("a target that already exited is still detached", func(t *testing.T) {
		f := &scriptDlv{errs: map[string]string{"State": "Process 4 has exited with status 0"}}
		d := serveScript(t, f)
		if err := d.Stop(); err != nil {
			t.Fatalf("Stop on an exited target: %v", err)
		}
		if !slices.Equal(f.detaches, []bool{true}) {
			t.Errorf("detaches = %v", f.detaches)
		}
	})

	t.Run("a halt failure aborts the detach and drops the client", func(t *testing.T) {
		f := &scriptDlv{state: runningState(), errs: map[string]string{"Command:" + api.Halt: "stuck"}}
		d := serveScript(t, f)
		err := d.Detach()
		if err == nil || !strings.Contains(err.Error(), "halting target") {
			t.Fatalf("Detach = %v", err)
		}
		if len(f.detaches) != 0 || d.client != nil {
			t.Errorf("detaches %v client %v; want no detach and a dropped client", f.detaches, d.client)
		}
	})

	t.Run("a detach error is reported", func(t *testing.T) {
		d := serveScript(t, &scriptDlv{errs: map[string]string{"Detach": "refused"}})
		if err := d.Stop(); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("Stop = %v", err)
		}
	})
}

func TestRelease_Errors(t *testing.T) {
	if err := NewDelveDebugger().Release(nil); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Errorf("Release on a fresh debugger = %v, want not connected", err)
	}

	t.Run("a clear failure names the breakpoint and does not resume", func(t *testing.T) {
		f := &scriptDlv{bps: []*api.Breakpoint{{ID: 4}}, errs: map[string]string{"ClearBreakpoint": "locked"}}
		d := serveScript(t, f)
		err := d.Release([]int{4})
		if err == nil || !strings.Contains(err.Error(), "clearing breakpoint 4") {
			t.Fatalf("Release = %v", err)
		}
		if slices.Contains(f.sentCommands(), api.Continue) {
			t.Error("a target forge could not clean up must not be resumed")
		}
		if d.client != nil {
			t.Error("Release always disconnects")
		}
	})

	t.Run("a listing failure is wrapped", func(t *testing.T) {
		d := serveScript(t, &scriptDlv{errs: map[string]string{"ListBreakpoints": "boom"}})
		if err := d.Release([]int{1}); err == nil || !strings.Contains(err.Error(), "listing breakpoints") {
			t.Errorf("Release = %v", err)
		}
	})

	t.Run("a clean release resumes the target", func(t *testing.T) {
		f := &scriptDlv{}
		d := serveScript(t, f)
		if err := d.Release(nil); err != nil {
			t.Fatal(err)
		}
		if got := f.waitForCommands(t, 1); !slices.Equal(got, []string{api.Continue}) {
			t.Errorf("commands = %v, want one continue", got)
		}
	})
}

func TestDisconnect(t *testing.T) {
	d := serveScript(t, &scriptDlv{})
	d.Disconnect()
	if d.client != nil {
		t.Error("Disconnect must drop the client")
	}
	d.Disconnect() // a second call is harmless
}

func TestRPCClientDisconnectContinue(t *testing.T) {
	f := &scriptDlv{}
	d := serveScript(t, f)
	if err := d.client.Disconnect(true); err != nil {
		t.Fatal(err)
	}
	if got := f.waitForCommands(t, 1); !slices.Equal(got, []string{api.Continue}) {
		t.Errorf("commands = %v, want the async continue", got)
	}
}

func TestRPCClientProcessPidIgnoresErrors(t *testing.T) {
	f := &scriptDlv{pid: 12}
	d := serveScript(t, f)
	if got := d.client.ProcessPid(); got != 12 {
		t.Errorf("ProcessPid = %d, want 12", got)
	}
	_ = d.client.Close()
	if got := d.client.ProcessPid(); got != 0 {
		t.Errorf("ProcessPid on a closed connection = %d, want 0", got)
	}
}

func TestWaitForStop_Paths(t *testing.T) {
	shrinkStopSettle(t)
	bpStop := haltedAtBreakpoint()
	tracepoint := func() api.DebuggerState {
		th := &api.Thread{ID: 1, Breakpoint: &api.Breakpoint{ID: 5, Tracepoint: true}}
		return api.DebuggerState{CurrentThread: th, Threads: []*api.Thread{th}}
	}
	manualHalt := api.DebuggerState{CurrentThread: &api.Thread{File: "x.go", Line: 3}}

	t.Run("an exited process is an error naming the status", func(t *testing.T) {
		f := &scriptDlv{contQueue: []api.DebuggerState{{Exited: true, ExitStatus: 2}}}
		d := serveScript(t, f)
		d.pid = 88
		_, err := d.ContinueFor(time.Second)
		if err == nil || !strings.Contains(err.Error(), "process 88 has exited with status 2") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("tracepoints log and continue until a real breakpoint", func(t *testing.T) {
		f := &scriptDlv{contQueue: []api.DebuggerState{tracepoint(), bpStop}}
		d := serveScript(t, f)
		stop, err := d.Continue()
		if err != nil {
			t.Fatal(err)
		}
		if stop.Reason != "breakpoint" || stop.Function != "main.tick" {
			t.Errorf("stop = %+v", stop)
		}
		if cmds := f.sentCommands(); !slices.Equal(cmds, []string{api.Continue, api.Continue}) {
			t.Errorf("commands = %v, want the tracepoint stop continued past", cmds)
		}
	})

	t.Run("a settled manual halt is reported as halted", func(t *testing.T) {
		f := &scriptDlv{contQueue: []api.DebuggerState{manualHalt}}
		d := serveScript(t, f)
		stop, err := d.ContinueFor(5 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if stop.Reason != "halted" || stop.File != "x.go" || stop.Line != 3 {
			t.Errorf("stop = %+v, want a halted stop at x.go:3", stop)
		}
	})

	t.Run("a halt that was only another client's halt-act-resume keeps waiting", func(t *testing.T) {
		f := &scriptDlv{
			state:      bpStop,
			stateQueue: []api.DebuggerState{{}, {Running: true}},
			contQueue:  []api.DebuggerState{manualHalt},
		}
		d := serveScript(t, f)
		stop, err := d.ContinueFor(5 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if stop.Reason != "breakpoint" || stop.Function != "main.tick" {
			t.Errorf("stop = %+v, want the breakpoint the target reached after resuming", stop)
		}
	})

	t.Run("a target that never stops times out as still running", func(t *testing.T) {
		// 1st State: the non-blocking probe (running). 2nd: the blocking wait, which never answers.
		f := &scriptDlv{stateQueue: []api.DebuggerState{runningState()}, hangStateAt: 2}
		d := serveScript(t, f)
		start := time.Now()
		_, err := d.ContinueFor(150 * time.Millisecond)
		if err != ErrStillRunning {
			t.Fatalf("err = %v, want ErrStillRunning", err)
		}
		if time.Since(start) > 5*time.Second {
			t.Error("the wait must end at the deadline")
		}
		if slices.Contains(f.sentCommands(), api.Continue) {
			t.Error("a running target must be waited on, never resumed again")
		}
	})

	t.Run("a failing continue is returned", func(t *testing.T) {
		f := &scriptDlv{errs: map[string]string{"Command:" + api.Continue: "cannot continue"}}
		if _, err := serveScript(t, f).Continue(); err == nil || !strings.Contains(err.Error(), "cannot continue") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("an unreadable initial state is returned", func(t *testing.T) {
		f := &scriptDlv{errs: map[string]string{"State": "down"}}
		if _, err := serveScript(t, f).Continue(); err == nil {
			t.Error("want an error")
		}
	})

	t.Run("WaitForStop on a resumed session reads the pending stop", func(t *testing.T) {
		f := &scriptDlv{state: bpStop}
		stop, err := serveScript(t, f).WaitForStop(time.Second)
		if err != nil || stop.Reason != "breakpoint" {
			t.Fatalf("WaitForStop = (%+v, %v)", stop, err)
		}
	})
}
