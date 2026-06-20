// File: internal/cli/add_rpc.go
//
// `forge add rpc <svc> <Name> [--stream bidi|client|server]` scaffolds a
// single new RPC into an existing service. CRUD codegen (crud_gen.go)
// covers the Create/Get/List/Update/Delete shapes; this command is for
// hand-written RPCs — especially streaming ones, where the Connect
// signature (`*connect.BidiStream[...]`, `*connect.ClientStream[...]`,
// `*connect.ServerStream[...]`) is fiddly enough that even experienced
// users mistype it.
//
// Deliberate non-goals:
//
//   - We do NOT edit the .proto file. Proto files have hand-curated
//     section markers, ordering, and comments; an automated injector
//     would regress those constantly. Instead, we print the snippet so
//     the user can paste it into the right `service { ... }` block.
//
//   - We do NOT extend CRUD/hooks codegen to streaming methods. CRUD is
//     unary-shaped by contract; streaming is intentionally hand-written
//     below the proto line.

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/naming"
)

// rpcStreamMode is the parsed --stream flag. The zero value is "unary".
type rpcStreamMode int

const (
	rpcUnary rpcStreamMode = iota
	rpcServerStream
	rpcClientStream
	rpcBidiStream
)

// parseStreamMode maps the --stream flag value to the internal mode enum.
// Empty string == unary so the flag is genuinely optional; an explicit
// "unary" is also accepted because some users will type it for symmetry.
func parseStreamMode(s string) (rpcStreamMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "unary":
		return rpcUnary, nil
	case "server":
		return rpcServerStream, nil
	case "client":
		return rpcClientStream, nil
	case "bidi", "bidirectional", "both":
		return rpcBidiStream, nil
	default:
		return 0, fmt.Errorf("unknown stream mode %q: valid modes are unary (default), server, client, bidi", s)
	}
}

// newAddRPCCmd is the cobra surface for `forge add rpc`.
func newAddRPCCmd() *cobra.Command {
	var streamFlag string
	cmd := &cobra.Command{
		Use:   "rpc <svc> <Name>",
		Short: "Scaffold a single hand-written RPC (handler stub + proto snippet)",
		Long: `Add a new RPC to an existing service. Writes a handler stub with the
correct Connect signature and prints the proto snippet to paste into
the service's .proto file. Run 'forge generate' afterwards to refresh
codegen.

The --stream flag picks the Connect signature shape:
  --stream server   server-streaming (req → many responses)
  --stream client   client-streaming (many requests → resp)
  --stream bidi     bidirectional streaming
  (omit)            unary (request → response)

The proto edit is left to you because proto files have hand-curated
section markers and ordering that an automated injector would
regress. Copy the printed snippet into the service block.

Examples:
  forge add rpc tasks ListTasksByOwner
  forge add rpc events TailEvents --stream server
  forge add rpc chat Chat --stream bidi`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode, err := parseStreamMode(streamFlag)
			if err != nil {
				return cliutil.UserErr("forge add rpc", err.Error(), "",
					"pass --stream server, --stream client, --stream bidi, or omit for unary")
			}
			return runAddRPC(args[0], args[1], mode)
		},
	}
	cmd.Flags().StringVar(&streamFlag, "stream", "", "stream mode (server, client, bidi); omit for unary")
	return cmd
}

// runAddRPC validates inputs, writes the handler stub, and prints the
// proto snippet. Mirrors `forge add handler-file` for filesystem
// preconditions: service must exist, target file must not.
func runAddRPC(svc, rpcName string, mode rpcStreamMode) error {
	ctxLabel := fmt.Sprintf("forge add rpc %s %s", svc, rpcName)

	if err := validateIdentifier(svc); err != nil {
		return cliutil.WrapUserErr(ctxLabel, "invalid service name", "",
			"use a name starting with a letter, containing letters/digits/_/-", err)
	}
	if err := validateIdentifier(rpcName); err != nil {
		return cliutil.WrapUserErr(ctxLabel, "invalid RPC name", "",
			"use a PascalCase name starting with an uppercase letter (e.g. ListTasksByOwner)", err)
	}
	// The RPC name becomes a Go method on the service receiver, so it
	// must start uppercase. validateIdentifier accepts lower-case starts
	// for filenames/package names; we tighten the rule here.
	if !startsUpper(rpcName) {
		return cliutil.UserErr(ctxLabel,
			fmt.Sprintf("RPC name %q must start with an uppercase letter", rpcName),
			"",
			"PascalCase your name so it becomes an exported method (e.g. ListTasksByOwner, not listTasksByOwner)")
	}

	root, err := projectRoot()
	if err != nil {
		return err
	}
	if err := requireServiceKind(root, "rpc"); err != nil {
		return err
	}

	// Tombstoned services (mentioned only in a pkg/app/services.go
	// comment — types-only) have no handler scaffold by design: adding
	// an RPC handler here would contradict the registration file. An
	// UNLISTED (newly added, not yet registered) service still has a
	// scaffold and falls through — implementing before registering is a
	// supported flow. Best-effort parse: a broken registry falls
	// through to the handler-dir check below.
	if reg, regErr := loadServiceRegistry(root); regErr == nil && reg.state(svc) == registrationTombstoned {
		return cliutil.UserErr(ctxLabel,
			fmt.Sprintf("service %q is types-only — %s deliberately does not register it (its row was deleted; see the comment there), so this binary has no handler scaffold to add an RPC to", svc, serviceRegistryRelPath),
			serviceRegistryRelPath,
			fmt.Sprintf("add the RPC to the proto only (the types/client still generate), implement it in the binary the %s comment names, or restore the `%s(app, cfg, logger, opts...),` row to serve it here", serviceRegistryRelPath, codegen.ServiceRowFuncName(svc)))
	}

	pkg := naming.ServicePackage(svc)
	handlerDir := filepath.Join(root, "internal", "handlers", pkg)
	if _, err := os.Stat(handlerDir); os.IsNotExist(err) {
		return cliutil.UserErr(ctxLabel,
			fmt.Sprintf("service %q has no handler directory at %s", svc, handlerDir),
			"",
			fmt.Sprintf("run `forge add service %s` first to scaffold the service", svc))
	}

	// One file per RPC keeps the diff readable and avoids any merge with
	// existing handler files. mock_gen.go discovers methods across every
	// non-test .go file in the package, so the new file is auto-picked
	// up at the next `forge generate`.
	fileName := "rpc_" + naming.ToSnakeCase(rpcName) + ".go"
	targetPath := filepath.Join(handlerDir, fileName)
	if _, err := os.Stat(targetPath); err == nil {
		return cliutil.UserErr(ctxLabel,
			fmt.Sprintf("file %s already exists", targetPath),
			"",
			"pick a different <Name> (or delete the existing file first if you really want to start over)")
	}

	content := buildRPCHandlerStub(pkg, rpcName, mode)
	if err := os.WriteFile(targetPath, []byte(content), 0o644); err != nil {
		return cliutil.WrapUserErr(ctxLabel, "write rpc file", targetPath,
			"verify the directory is writable", err)
	}

	fmt.Printf("✅ Created %s\n", targetPath)
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  1. Paste the snippet below into your service's .proto file")
	fmt.Println("     (inside the `service { ... }` block; the request/response messages")
	fmt.Println("     go at the bottom alongside the other message definitions).")
	fmt.Println("  2. Run `forge generate` to refresh codegen.")
	fmt.Println("  3. Fill in the handler body — the stub returns Unimplemented.")
	fmt.Println()
	fmt.Println("─── Proto snippet ──────────────────────────────────────────────────")
	fmt.Print(buildRPCProtoSnippet(rpcName, mode))
	fmt.Println("────────────────────────────────────────────────────────────────────")
	return nil
}

// startsUpper reports whether s starts with an ASCII uppercase letter.
// Connect-RPC method names are exported Go identifiers, so we require
// the standard A-Z start here even though validateIdentifier is laxer.
func startsUpper(s string) bool {
	if s == "" {
		return false
	}
	return s[0] >= 'A' && s[0] <= 'Z'
}

// buildRPCHandlerStub returns the handler-file content for the chosen
// stream mode. The four signature shapes mirror codegen's
// buildMethodSignature (internal/codegen/generator.go) so a regenerated
// project doesn't diff against the scaffold output.
//
// The stub returns connect.CodeUnimplemented for unary/client-streaming
// (where the return signature includes a *Response) and a plain
// connect.NewError(...) for server/bidi (where the return is just
// `error`). Either way `forge generate && go build` succeeds out of
// the box; the user fills in the body when ready.
func buildRPCHandlerStub(pkg, rpcName string, mode rpcStreamMode) string {
	reqType := rpcName + "Request"
	respType := rpcName + "Response"
	errExpr := fmt.Sprintf(
		`connect.NewError(connect.CodeUnimplemented, fmt.Errorf("handler for %%s not yet implemented", %q))`,
		rpcName,
	)

	var sig, body string
	switch mode {
	case rpcServerStream:
		sig = fmt.Sprintf(
			"func (s *Service) %s(ctx context.Context, req *connect.Request[%s], stream *connect.ServerStream[%s]) error",
			rpcName, reqType, respType,
		)
		body = "\treturn " + errExpr
	case rpcClientStream:
		sig = fmt.Sprintf(
			"func (s *Service) %s(ctx context.Context, stream *connect.ClientStream[%s]) (*connect.Response[%s], error)",
			rpcName, reqType, respType,
		)
		body = "\treturn nil, " + errExpr
	case rpcBidiStream:
		sig = fmt.Sprintf(
			"func (s *Service) %s(ctx context.Context, stream *connect.BidiStream[%s, %s]) error",
			rpcName, reqType, respType,
		)
		body = "\treturn " + errExpr
	default: // rpcUnary
		sig = fmt.Sprintf(
			"func (s *Service) %s(ctx context.Context, req *connect.Request[%s]) (*connect.Response[%s], error)",
			rpcName, reqType, respType,
		)
		body = "\treturn nil, " + errExpr
	}

	// gen import path is hand-templated because we don't have the
	// project module here. The user will fix the placeholder when the
	// file fails to compile — same shape as `forge add handler-file`,
	// which intentionally leaves the request/response imports out so
	// users add only what they need.
	return fmt.Sprintf(`package %s

// %s is a hand-written RPC stub. Returned by `+"`forge add rpc`"+`.
// Fill in the body when ready; the stub returns Unimplemented so
// `+"`go build`"+` succeeds before you have an implementation.
//
// Replace the dotted import path below with your project's generated
// proto package (the same import the existing handlers in this
// package use for %s / %s).

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	// %s "<your-module>/gen/services/<service>/v1"
)

%s {
%s
}
`,
		pkg,
		rpcName,
		reqType,
		respType,
		pkg+"pb",
		sig,
		body,
	)
}

// buildRPCProtoSnippet returns the proto block the user pastes into
// the service's .proto file. The snippet has two parts: the rpc line
// (goes inside the service block) and the request/response messages
// (go at the bottom).
//
// We print the rpc line with the streaming keyword in the right slot
// per https://protobuf.dev/reference/protobuf/proto3-spec/#service_definition —
// the same shape Connect/buf expect.
func buildRPCProtoSnippet(rpcName string, mode rpcStreamMode) string {
	reqType := rpcName + "Request"
	respType := rpcName + "Response"

	var rpcLine string
	switch mode {
	case rpcServerStream:
		rpcLine = fmt.Sprintf("  rpc %s(%s) returns (stream %s);", rpcName, reqType, respType)
	case rpcClientStream:
		rpcLine = fmt.Sprintf("  rpc %s(stream %s) returns (%s);", rpcName, reqType, respType)
	case rpcBidiStream:
		rpcLine = fmt.Sprintf("  rpc %s(stream %s) returns (stream %s);", rpcName, reqType, respType)
	default:
		rpcLine = fmt.Sprintf("  rpc %s(%s) returns (%s);", rpcName, reqType, respType)
	}

	return fmt.Sprintf(`
// Paste inside `+"`service <YourService> { ... }`"+`:
%s

// Paste alongside the other message definitions:
message %s {
  // TODO: request fields
}

message %s {
  // TODO: response fields
}

`, rpcLine, reqType, respType)
}
