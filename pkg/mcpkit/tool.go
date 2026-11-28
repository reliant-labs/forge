// Package mcpkit serves a forge app's opted-in RPCs as MCP tools.
//
// An rpc opts in with `(forge.v1.method).mcp` (or `(forge.v1.service).mcp`);
// a mutating rpc also needs `mcp_mutating: true`. `forge generate` projects
// the opted-in rpcs into a table of Tool values (pkg/middleware/mcp_tools_gen.go
// in the app), and the app's API listener serves that table on /mcp: MCP
// streamable HTTP, stateless, each tools/call an in-process Connect call
// through the listener's own interceptor chain. Design:
// docs/proposals/mcp-from-rpcs.md.
package mcpkit

import "encoding/json"

// Tool is one rpc served as an MCP tool. forge generate writes the table;
// nothing hand-writes one outside tests.
type Tool struct {
	// Name is the MCP tool name: `<service>_<rpc>` in snake case with the
	// service's `Service` suffix dropped, at most 64 characters of [a-z0-9_].
	Name string
	// Procedure is the rpc's Connect procedure, "/<pkg>.<Service>/<Method>".
	Procedure string
	// Description is the rpc's leading proto comment.
	Description string
	// InputSchema is the JSON Schema (draft 2020-12) of the request message's
	// protojson form, a self-contained object schema.
	InputSchema json.RawMessage
	// OutputSchema is the JSON Schema of the response message's protojson
	// form. nil announces no output schema.
	OutputSchema json.RawMessage
	// ReadOnly is set for an rpc declaring idempotency_level =
	// NO_SIDE_EFFECTS; it is announced as readOnlyHint.
	ReadOnly bool
	// Destructive is set for a mutating rpc served with mcp_mutating: true;
	// it is announced as destructiveHint.
	Destructive bool
	// Idempotent is `(forge.v1.method).idempotent` or idempotency_level =
	// IDEMPOTENT; it is announced as idempotentHint.
	Idempotent bool
	// Public is set for an rpc declaring auth_required: false. A caller
	// with no credential is shown only the public tools.
	Public bool
}
