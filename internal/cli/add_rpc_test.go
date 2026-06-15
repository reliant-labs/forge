// File: internal/cli/add_rpc_test.go
//
// Tests for `forge add rpc <svc> <Name> [--stream M]`. They exercise:
//
//   - happy paths for each stream mode (unary/server/client/bidi)
//     produce the canonical Connect signature shape — locked in so a
//     codegen refactor that touches buildMethodSignature doesn't quietly
//     drift the scaffold against generated code.
//   - proto snippet places `stream` in the right slot for each mode.
//   - FS preconditions: handler dir must exist, target file must not.
//   - parseStreamMode accepts the documented aliases and rejects others.
//   - subcommand is registered on `forge add`.

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseStreamMode(t *testing.T) {
	cases := []struct {
		in      string
		want    rpcStreamMode
		wantErr bool
	}{
		{"", rpcUnary, false},
		{"unary", rpcUnary, false},
		{"server", rpcServerStream, false},
		{"SERVER", rpcServerStream, false}, // case-insensitive
		{"client", rpcClientStream, false},
		{"bidi", rpcBidiStream, false},
		{"bidirectional", rpcBidiStream, false},
		{"both", rpcBidiStream, false},
		{"  server  ", rpcServerStream, false}, // trim whitespace
		{"nonsense", 0, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			got, err := parseStreamMode(c.in)
			if c.wantErr {
				if err == nil {
					t.Errorf("parseStreamMode(%q) expected error, got nil", c.in)
				}
				return
			}
			if err != nil {
				t.Errorf("parseStreamMode(%q) unexpected error: %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("parseStreamMode(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

// TestBuildRPCHandlerStub_SignaturesPerMode pins the Connect signature
// for each stream mode against the shapes codegen emits in
// internal/codegen/generator.go's buildMethodSignature. If codegen
// ever changes those, this test surfaces the divergence — the scaffold
// must match what `forge generate` produces or hand-written RPCs
// silently fail to satisfy the generated Service interface.
func TestBuildRPCHandlerStub_SignaturesPerMode(t *testing.T) {
	cases := []struct {
		name string
		mode rpcStreamMode
		want string // substring that uniquely identifies the signature shape
	}{
		{
			name: "unary",
			mode: rpcUnary,
			want: "func (s *Service) Foo(ctx context.Context, req *connect.Request[FooRequest]) (*connect.Response[FooResponse], error)",
		},
		{
			name: "server-stream",
			mode: rpcServerStream,
			want: "func (s *Service) Foo(ctx context.Context, req *connect.Request[FooRequest], stream *connect.ServerStream[FooResponse]) error",
		},
		{
			name: "client-stream",
			mode: rpcClientStream,
			want: "func (s *Service) Foo(ctx context.Context, stream *connect.ClientStream[FooRequest]) (*connect.Response[FooResponse], error)",
		},
		{
			name: "bidi-stream",
			mode: rpcBidiStream,
			want: "func (s *Service) Foo(ctx context.Context, stream *connect.BidiStream[FooRequest, FooResponse]) error",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := buildRPCHandlerStub("billing", "Foo", c.mode)
			if !strings.Contains(got, c.want) {
				t.Errorf("stub for %s missing canonical signature\nwant substring:\n%s\nfull stub:\n%s", c.name, c.want, got)
			}
			if !strings.HasPrefix(got, "package billing\n") {
				t.Errorf("stub must declare `package billing`, got prefix:\n%s", got[:40])
			}
			if !strings.Contains(got, "connect.CodeUnimplemented") {
				t.Errorf("stub must return Unimplemented so go build succeeds before the impl lands, got:\n%s", got)
			}
		})
	}
}

// TestBuildRPCProtoSnippet_StreamKeywordPlacement pins where the
// `stream` keyword appears in the proto rpc line per Connect/buf's
// expected shape. A bad position here would leave the user pasting
// proto that fails buf lint.
func TestBuildRPCProtoSnippet_StreamKeywordPlacement(t *testing.T) {
	cases := []struct {
		name string
		mode rpcStreamMode
		want string
	}{
		{"unary", rpcUnary, "rpc Foo(FooRequest) returns (FooResponse);"},
		{"server", rpcServerStream, "rpc Foo(FooRequest) returns (stream FooResponse);"},
		{"client", rpcClientStream, "rpc Foo(stream FooRequest) returns (FooResponse);"},
		{"bidi", rpcBidiStream, "rpc Foo(stream FooRequest) returns (stream FooResponse);"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := buildRPCProtoSnippet("Foo", c.mode)
			if !strings.Contains(got, c.want) {
				t.Errorf("proto snippet for %s missing rpc line\nwant:\n%s\nfull snippet:\n%s", c.name, c.want, got)
			}
			if !strings.Contains(got, "message FooRequest {") || !strings.Contains(got, "message FooResponse {") {
				t.Errorf("proto snippet missing message stubs:\n%s", got)
			}
		})
	}
}

func TestRunAddRPC_HappyPath(t *testing.T) {
	dir := withTempProject(t, minimalServiceForgeYAML)
	writeComponentsJSON(t, dir)
	handlerDir := filepath.Join(dir, "handlers", "tasks")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatalf("mkdir handlers/tasks: %v", err)
	}

	if err := runAddRPC("tasks", "TailEvents", rpcServerStream); err != nil {
		t.Fatalf("runAddRPC: %v", err)
	}

	// rpc_ prefix + snake_case filename keeps the per-RPC files
	// visually grouped in directory listings.
	want := filepath.Join(handlerDir, "rpc_tail_events.go")
	body, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("read scaffolded file: %v", err)
	}
	content := string(body)
	if !strings.Contains(content, "func (s *Service) TailEvents(") {
		t.Errorf("scaffolded file missing handler signature, got:\n%s", content)
	}
	if !strings.Contains(content, "ServerStream[TailEventsResponse]") {
		t.Errorf("server-stream mode should produce ServerStream signature, got:\n%s", content)
	}
}

func TestRunAddRPC_RejectsLowercaseStart(t *testing.T) {
	dir := withTempProject(t, minimalServiceForgeYAML)
	writeComponentsJSON(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, "handlers", "tasks"), 0o755); err != nil {
		t.Fatalf("mkdir handlers/tasks: %v", err)
	}

	// Connect RPC method names must be exported — start uppercase.
	// validateIdentifier accepts lowercase starts (it's also used for
	// filenames), so we have a tighter check just for RPC names.
	err := runAddRPC("tasks", "tailEvents", rpcUnary)
	if err == nil {
		t.Fatal("expected error for lowercase RPC name, got nil")
	}
	if !strings.Contains(err.Error(), "uppercase") {
		t.Errorf("error should mention uppercase requirement, got: %v", err)
	}
}

func TestRunAddRPC_MissingHandlerDir(t *testing.T) {
	dir := withTempProject(t, minimalServiceForgeYAML)
	writeComponentsJSON(t, dir)
	// No handlers/tasks dir on disk.

	err := runAddRPC("tasks", "ListByOwner", rpcUnary)
	if err == nil {
		t.Fatal("expected error when handler directory missing, got nil")
	}
	if !strings.Contains(err.Error(), "no handler directory") {
		t.Errorf("error should explain missing dir, got: %v", err)
	}
}

func TestRunAddRPC_FileAlreadyExists(t *testing.T) {
	dir := withTempProject(t, minimalServiceForgeYAML)
	writeComponentsJSON(t, dir)
	handlerDir := filepath.Join(dir, "handlers", "tasks")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatalf("mkdir handlers/tasks: %v", err)
	}
	existing := filepath.Join(handlerDir, "rpc_list_by_owner.go")
	if err := os.WriteFile(existing, []byte("package tasks\n// user-edited\n"), 0o644); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}

	err := runAddRPC("tasks", "ListByOwner", rpcUnary)
	if err == nil {
		t.Fatal("expected error when target file exists, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error should explain conflict, got: %v", err)
	}
	// The user-edited file must not have been clobbered.
	body, _ := os.ReadFile(existing)
	if !strings.Contains(string(body), "user-edited") {
		t.Error("existing file was overwritten; refusal path is broken")
	}
}

func TestAddRPCSubcommandRegistered(t *testing.T) {
	root := newAddCmd()
	var found bool
	for _, sub := range root.Commands() {
		if sub.Name() == "rpc" {
			found = true
			if sub.Args == nil {
				t.Error("rpc subcommand should declare cobra.ExactArgs(2)")
			}
			if sub.Flag("stream") == nil {
				t.Error("rpc subcommand should expose --stream flag")
			}
			break
		}
	}
	if !found {
		t.Fatal("rpc subcommand not registered on `forge add`")
	}
	if !strings.Contains(root.Long, "rpc") {
		t.Error("`forge add --help` Long string should advertise the rpc subcommand")
	}
}
