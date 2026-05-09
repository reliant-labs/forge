package tdd_test

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	"github.com/reliant-labs/forge/pkg/tdd"
)

// fakeReq / fakeResp are stand-ins for proto messages — TableRPC never
// inspects them, only passes them through, so a struct is enough.
type fakeReq struct{ Name string }
type fakeResp struct{ Greeting string }

func helloHandler(_ context.Context, req *connect.Request[fakeReq]) (*connect.Response[fakeResp], error) {
	if req.Msg.Name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name required"))
	}
	if req.Msg.Name == "boom" {
		return nil, connect.NewError(connect.CodeInternal, errors.New("kaboom"))
	}
	return connect.NewResponse(&fakeResp{Greeting: "hi " + req.Msg.Name}), nil
}

func TestTableRPC_HappyAndError(t *testing.T) {
	cases := []tdd.Case[fakeReq, fakeResp]{
		{
			Name: "happy",
			Req:  connect.NewRequest(&fakeReq{Name: "ada"}),
			Check: func(t *testing.T, resp *connect.Response[fakeResp]) {
				if resp.Msg.Greeting != "hi ada" {
					t.Fatalf("got %q, want %q", resp.Msg.Greeting, "hi ada")
				}
			},
		},
		{
			Name:    "missing name → invalid argument",
			Req:     connect.NewRequest(&fakeReq{}),
			WantErr: connect.CodeInvalidArgument,
		},
		{
			Name:    "boom → internal",
			Req:     connect.NewRequest(&fakeReq{Name: "boom"}),
			WantErr: connect.CodeInternal,
		},
	}

	tdd.TableRPC(t, cases, helloHandler)
}

func TestTableRPC_SetupRuns(t *testing.T) {
	var setupCalls int
	cases := []tdd.Case[fakeReq, fakeResp]{
		{
			Name:  "row 1",
			Req:   connect.NewRequest(&fakeReq{Name: "x"}),
			Setup: func(_ *testing.T) { setupCalls++ },
		},
		{
			Name:  "row 2",
			Req:   connect.NewRequest(&fakeReq{Name: "y"}),
			Setup: func(_ *testing.T) { setupCalls++ },
		},
	}
	tdd.TableRPC(t, cases, helloHandler)
	if setupCalls != 2 {
		t.Fatalf("setup ran %d times, want 2", setupCalls)
	}
}

func TestRunRPCCases_AliasMatchesTableRPC(t *testing.T) {
	// RunRPCCases is the codegen-facing alias of TableRPC. Verify it
	// runs identically: error-code rows, happy-path rows, multiple
	// cases per test, and per-case Setup hooks executing in declared
	// order.
	var setupOrder []string
	cases := []tdd.RPCCase[fakeReq, fakeResp]{
		{
			Name:  "happy",
			Req:   connect.NewRequest(&fakeReq{Name: "ada"}),
			Setup: func(_ *testing.T) { setupOrder = append(setupOrder, "happy") },
			Check: func(t *testing.T, resp *connect.Response[fakeResp]) {
				if resp.Msg.Greeting != "hi ada" {
					t.Fatalf("got %q, want %q", resp.Msg.Greeting, "hi ada")
				}
			},
		},
		{
			Name:    "missing → invalid argument",
			Req:     connect.NewRequest(&fakeReq{}),
			WantErr: connect.CodeInvalidArgument,
			Setup:   func(_ *testing.T) { setupOrder = append(setupOrder, "missing") },
		},
		{
			Name:    "boom → internal",
			Req:     connect.NewRequest(&fakeReq{Name: "boom"}),
			WantErr: connect.CodeInternal,
			Setup:   func(_ *testing.T) { setupOrder = append(setupOrder, "boom") },
		},
	}

	tdd.RunRPCCases(t, cases, helloHandler)

	if want := []string{"happy", "missing", "boom"}; len(setupOrder) != len(want) {
		t.Fatalf("setup ran %v, want %v", setupOrder, want)
	} else {
		for i := range want {
			if setupOrder[i] != want[i] {
				t.Fatalf("setup[%d] = %q, want %q", i, setupOrder[i], want[i])
			}
		}
	}
}

func TestRunRPCCases_RPCCaseIsCaseAlias(t *testing.T) {
	// Compile-time check: RPCCase must be a type *alias* (assignable
	// from Case without conversion), not a named-type wrapper.
	var c tdd.Case[fakeReq, fakeResp]
	var rc tdd.RPCCase[fakeReq, fakeResp] = c // assigns iff alias
	_ = rc
}

func TestTableRPC_AnyOutcomeAcceptsErrorAndSuccess(t *testing.T) {
	// AnyOutcome rows tolerate either a Connect error or a happy
	// response — used for scaffold-style "the handler dispatches" rows
	// before real assertions land.
	cases := []tdd.Case[fakeReq, fakeResp]{
		{
			Name:       "scaffold accepts success",
			Req:        connect.NewRequest(&fakeReq{Name: "ada"}),
			AnyOutcome: true,
		},
		{
			Name:       "scaffold accepts CodeInvalidArgument",
			Req:        connect.NewRequest(&fakeReq{}),
			AnyOutcome: true,
		},
		{
			Name:       "scaffold accepts CodeInternal",
			Req:        connect.NewRequest(&fakeReq{Name: "boom"}),
			AnyOutcome: true,
		},
	}
	tdd.TableRPC(t, cases, helloHandler)
}

func TestTableRPC_AnyOutcomeStillRunsCheckOnSuccess(t *testing.T) {
	// Check is consulted on successful responses even in AnyOutcome
	// mode, so happy-path assertions can be pinned without losing the
	// scaffold's lenience on error paths.
	var checkCalled bool
	cases := []tdd.Case[fakeReq, fakeResp]{
		{
			Name:       "happy with check",
			Req:        connect.NewRequest(&fakeReq{Name: "ada"}),
			AnyOutcome: true,
			Check: func(_ *testing.T, _ *connect.Response[fakeResp]) {
				checkCalled = true
			},
		},
	}
	tdd.TableRPC(t, cases, helloHandler)
	if !checkCalled {
		t.Fatal("Check should still run on a successful response in AnyOutcome mode")
	}
}

func TestAssertConnectError(t *testing.T) {
	t.Run("matching code passes", func(t *testing.T) {
		err := connect.NewError(connect.CodeNotFound, errors.New("nope"))
		// Use a sub-T so a fail in AssertConnectError doesn't fail the whole test.
		fakeT := &testing.T{}
		tdd.AssertConnectError(fakeT, err, connect.CodeNotFound)
		if fakeT.Failed() {
			t.Fatal("AssertConnectError flagged a matching error as failure")
		}
	})
	t.Run("nil error fails", func(t *testing.T) {
		fakeT := &testing.T{}
		// AssertConnectError calls t.Fatalf, which panics with a goexit
		// inside a subtest goroutine; capture by running in a goroutine.
		done := make(chan struct{})
		go func() {
			defer func() {
				_ = recover()
				close(done)
			}()
			tdd.AssertConnectError(fakeT, nil, connect.CodeNotFound)
		}()
		<-done
		if !fakeT.Failed() {
			t.Fatal("AssertConnectError accepted nil error")
		}
	})
}
