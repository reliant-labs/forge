// Package codegen — mcp_gen.go writes gen/mcp/manifest.json, a JSON
// manifest mapping every RPC in the project to an MCP tool schema.
//
// The manifest exists so agent hosts (and any other MCP-aware tool)
// can discover the project's Connect RPCs as callable tools without
// bespoke per-project wiring. It is the static-descriptor sibling of
// `gen/forge_descriptor.json` — same source of truth (the parsed
// proto tree), different consumer (MCP servers vs forge's own
// codegen).
//
// Shape (per RPC, one entry per service.method). Field names match the
// MCP specification's tool descriptor so the `tools` array can be
// returned verbatim as the result of `tools/list` from an MCP server.
// Extra forge-specific metadata (service / method / procedure /
// auth_required / idempotency_key / streaming) sits at the top level
// because MCP clients ignore unknown fields — the same JSON is both a
// valid MCP tool entry AND a forge-aware tool description.
//
//	{
//	  "_generated": "forge",
//	  "schema_version": "1.0",
//	  "project": "<project name>",
//	  "tools": [
//	    {
//	      "name": "<service_snake>__<rpc_snake>",
//	      "description": "<RPC doc-comment, or empty>",
//	      "inputSchema":  {"type": "object", "properties": {...}, "required": [...]},
//	      "outputSchema": {"type": "object", "properties": {...}, "required": [...]},
//	      "service": "<ServiceName>",
//	      "method": "<RpcName>",
//	      "procedure": "/<package>.<ServiceName>/<RpcName>",
//	      "auth_required": true|false,
//	      "idempotency_key": false,
//	      "streaming": "server|client|bidi"  // optional, only on streaming RPCs
//	    }
//	  ]
//	}
//
// Empty service list → no manifest file written.  An MCP host querying
// a service-less project should treat the absent file as "no tools";
// emitting a tools:[] would imply the project deliberately publishes
// zero tools, which is a stronger statement than "this project hasn't
// scaffolded any services yet".
package codegen

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/naming"
)

// MCPGenInput is the per-project input for [GenerateMCPManifest]. The
// shape mirrors K3dPortsGenInput so the call-site in the generate
// pipeline stays uniform across codegen emitters.
type MCPGenInput struct {
	ProjectDir  string                   // project root; output path is gen/mcp/manifest.json relative to here
	ProjectName string                   // emitted as the manifest's "project" field; "" tolerated
	Services    []ServiceDef             // every parsed Connect service; empty → no-op
	Checksums   *checksums.FileChecksums // when set, the rendered manifest is recorded under .forge/checksums.json
}

// mcpManifest is the top-level JSON shape. Field order in the JSON
// output is controlled by struct field order — keep "_generated" /
// "schema_version" / "project" before "tools" so a human peeking at
// the file sees the metadata first.
type mcpManifest struct {
	Generated     string    `json:"_generated"`
	SchemaVersion string    `json:"schema_version"`
	Project       string    `json:"project"`
	Tools         []mcpTool `json:"tools"`
}

// mcpTool is a single RPC's MCP tool descriptor.
//
// Field naming is constrained by the MCP specification — `name`,
// `description`, `inputSchema`, and `outputSchema` MUST be camelCase
// to match what MCP clients (Claude Desktop, the official inspector,
// Cline, etc.) expect when they call `tools/list`. An MCP server can
// return this struct verbatim from `tools/list` and clients will
// consume it correctly.
//
// Everything else (service / method / procedure / auth_required /
// idempotency_key / streaming) is forge-specific metadata. MCP clients
// ignore unknown top-level fields (per the protocol's permissive
// unknown-field policy), so they don't break interop. Forge-aware
// tooling — the MCP bridge in cmd/forge-mcp, audit, doctor — reads
// the snake_case forge metadata.
//
// Field order: identity (name/desc) → schemas (matches the MCP spec's
// example layout) → forge routing → forge policy → optional streaming
// tag. Keeping the MCP-required fields first means a `head`-ed dump
// looks immediately MCP-shaped to a reader.
type mcpTool struct {
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	InputSchema    mcpSchema `json:"inputSchema"`
	OutputSchema   mcpSchema `json:"outputSchema"`
	Service        string    `json:"service"`
	Method         string    `json:"method"`
	Procedure      string    `json:"procedure"`
	AuthRequired   bool      `json:"auth_required"`
	IdempotencyKey bool      `json:"idempotency_key"`
	// Streaming is omitted (json:",omitempty") for unary RPCs so
	// non-streaming tools don't carry a confusing empty field. Values
	// are "client", "server", or "bidi" for the three Connect modes.
	Streaming string `json:"streaming,omitempty"`
}

// mcpSchema is the minimal JSON-Schema slice we emit for each
// request/response message. We deliberately stay at the "object with
// typed properties" level rather than recursing into nested messages —
// per the v1 spec the agent host gets the shape, not the full type
// graph. A nested message field surfaces as `{"type": "object"}` with
// no further detail; if the agent host needs richer schemas it can
// consult the source proto.
type mcpSchema struct {
	Type       string                   `json:"type"`
	Properties map[string]mcpFieldShape `json:"properties"`
	// Required uses json:",omitempty" so empty-required cases emit no
	// key at all rather than `"required": null`. JSON Schema treats a
	// missing `required` as "no required fields", which matches what
	// we'd mean by an empty list.
	Required []string `json:"required,omitempty"`
}

// mcpFieldShape is the per-field type entry inside a schema's
// properties map. Items is the array-element type when Type=="array".
// We use a pointer so non-array fields omit the key entirely.
type mcpFieldShape struct {
	Type  string         `json:"type"`
	Items *mcpFieldShape `json:"items,omitempty"`
}

// GenerateMCPManifest writes gen/mcp/manifest.json. Returns nil with
// no file written when in.Services is empty — see the package doc for
// the "tools:[] vs absent file" rationale.
//
// The function is idempotent: identical input produces byte-identical
// output. Services and methods are emitted in their source order; the
// caller controls what that order is by passing in the descriptor
// directly. We do NOT re-sort here because the descriptor's parse
// order already matches the proto file's declaration order, and
// shuffling would obscure that.
func GenerateMCPManifest(in MCPGenInput) error {
	if in.ProjectDir == "" {
		return fmt.Errorf("mcp manifest gen: ProjectDir required")
	}
	if len(in.Services) == 0 {
		return nil
	}

	tools := make([]mcpTool, 0, totalMethods(in.Services))
	for _, svc := range in.Services {
		serviceSnake := naming.ToSnakeCase(svc.Name)
		for _, m := range svc.Methods {
			tools = append(tools, mcpTool{
				Name:           serviceSnake + "__" + naming.ToSnakeCase(m.Name),
				Description:    "", // proto leading comments aren't carried in the descriptor; v1 fallback per spec
				Service:        svc.Name,
				Method:         m.Name,
				Procedure:      fmt.Sprintf("/%s.%s/%s", svc.Package, svc.Name, m.Name),
				AuthRequired:   m.AuthRequired,
				IdempotencyKey: false,
				InputSchema:    schemaFromMessage(svc, m.InputType, false),
				OutputSchema:   schemaFromMessage(svc, m.OutputType, true),
				Streaming:      streamingMode(m),
			})
		}
	}

	manifest := mcpManifest{
		Generated:     "forge",
		SchemaVersion: "1.0",
		Project:       in.ProjectName,
		Tools:         tools,
	}

	// MarshalIndent for human-grep-ability. The manifest is a
	// generated artifact a human (or an agent) might read directly
	// when debugging — pretty-printed JSON is worth the extra bytes.
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal MCP manifest: %w", err)
	}
	// MarshalIndent doesn't append a trailing newline; add one so the
	// file ends cleanly (matches every other text artifact forge
	// emits and keeps `diff` / `git` outputs tidy).
	body = append(body, '\n')

	rel := filepath.Join("gen", "mcp", "manifest.json")
	if _, err := checksums.WriteGeneratedFile(in.ProjectDir, rel, body, in.Checksums, true); err != nil {
		return fmt.Errorf("write %s: %w", rel, err)
	}
	return nil
}

// totalMethods sums methods across services so we can pre-size the
// tools slice. Microscopic optimization but it keeps the allocation
// count predictable and the intent explicit.
func totalMethods(svcs []ServiceDef) int {
	n := 0
	for _, s := range svcs {
		n += len(s.Methods)
	}
	return n
}

// streamingMode returns the MCP streaming label for a method. Unary
// RPCs return "" so the JSON emitter omits the field entirely.
func streamingMode(m Method) string {
	switch {
	case m.ClientStreaming && m.ServerStreaming:
		return "bidi"
	case m.ServerStreaming:
		return "server"
	case m.ClientStreaming:
		return "client"
	default:
		return ""
	}
}

// schemaFromMessage projects a single proto message (by name) into
// the JSON-Schema slice we publish. When the message isn't known
// (e.g. google.protobuf.Empty, or a cross-file message the descriptor
// didn't carry fields for), we emit an empty object schema — the
// agent host knows "this RPC takes an object; we don't know its
// fields" which is strictly better than the bogus alternative of
// "this RPC takes nothing".
//
// outputSchema=true switches the projection to match how a real
// Connect server emits JSON in practice (proto3 + protojson):
//
//   - Field names are converted to lowerCamelCase. protojson always
//     emits lowerCamelCase per the proto3 JSON Mapping spec; using
//     the snake_case proto-original name produces a manifest the
//     server's responses never actually match. Caught by e2e
//     validation against a live forge-generated server — the official
//     MCP Inspector rejected List responses because the schema said
//     `total_count` but the server emitted `totalCount`.
//
//   - No fields are marked `required`. Proto3 + protojson omit
//     default-valued fields (empty strings, zero numbers, false
//     booleans, empty messages) from the JSON encoding unless the
//     server explicitly opts into EmitUnpopulated. Marking output
//     fields as required would mean every response with a zero-valued
//     field fails strict schema validation — which is exactly what
//     happened with `next_page_token: ""` in the same e2e pass.
//
// For input schemas, field names are also lowerCamelCase'd so the
// agent constructs arguments under the same name protojson accepts
// canonically (protojson accepts both casings on input, but
// publishing a consistent name avoids round-trip confusion). Required
// stays populated for input — non-optional fields ARE the contract
// the agent must satisfy.
func schemaFromMessage(svc ServiceDef, msgName string, outputSchema bool) mcpSchema {
	out := mcpSchema{
		Type:       "object",
		Properties: map[string]mcpFieldShape{},
	}
	fields, ok := svc.Messages[msgName]
	if !ok {
		return out
	}

	var required []string
	for _, f := range fields {
		name := toLowerCamelCase(f.Name)
		out.Properties[name] = fieldShapeFor(f.ProtoType)
		if !outputSchema && !f.IsOptional {
			required = append(required, name)
		}
	}
	// Sort required so the output is deterministic across runs (proto
	// field order is preserved by the descriptor, but a hand-edited
	// proto could shuffle them mid-message; stable required list keeps
	// the JSON diff-friendly).
	sort.Strings(required)
	out.Required = required
	return out
}

// toLowerCamelCase converts a snake_case proto field name to the
// lowerCamelCase form protojson uses on the wire. Matches the rule
// from the proto3 JSON Mapping spec: split on underscores; lowercase
// the first segment; capitalize the first letter of every subsequent
// segment; concatenate. Single-word names (already valid lowerCamel)
// pass through unchanged.
func toLowerCamelCase(s string) string {
	if s == "" {
		return s
	}
	parts := strings.Split(s, "_")
	out := strings.ToLower(parts[0])
	for _, p := range parts[1:] {
		if p == "" {
			continue
		}
		out += strings.ToUpper(p[:1]) + p[1:]
	}
	return out
}

// fieldShapeFor maps a proto type name to the minimal JSON-Schema
// type entry. The mapping table is intentionally short:
//
//   - string → "string"
//   - bool → "boolean"
//   - numeric scalars (int32/int64/uint32/uint64/sint32/sint64/
//     fixed32/fixed64/sfixed32/sfixed64/float/double) → "number"
//   - bytes → "string" (base64 is the on-wire convention for MCP)
//   - repeated X (the descriptor encodes this as "[]X") → array with
//     items of X's mapping
//   - anything else (nested message types) → "object"
//
// We collapse all numerics to "number" rather than picking
// "integer" vs "number" because Connect's JSON wire format sends
// 64-bit ints as strings — the agent host can't safely assume
// integerness without consulting the proto directly. "number" is the
// looser, safer label.
func fieldShapeFor(protoType string) mcpFieldShape {
	// Repeated fields. The descriptor encodes these as "[]<elemType>"
	// (matching how Go renders them in MessageFieldDef.ProtoType for
	// list fields).  Strip the prefix and recurse for the element.
	if len(protoType) >= 2 && protoType[0] == '[' && protoType[1] == ']' {
		elem := fieldShapeFor(protoType[2:])
		return mcpFieldShape{Type: "array", Items: &elem}
	}
	switch protoType {
	case "string":
		return mcpFieldShape{Type: "string"}
	case "bool":
		return mcpFieldShape{Type: "boolean"}
	case "int32", "int64", "uint32", "uint64",
		"sint32", "sint64",
		"fixed32", "fixed64", "sfixed32", "sfixed64",
		"float", "double":
		return mcpFieldShape{Type: "number"}
	case "bytes":
		return mcpFieldShape{Type: "string"}
	default:
		// Nested messages, enums, well-known types — all collapse to
		// "object" per the v1 spec. Enums are arguably "string" but
		// the descriptor doesn't disambiguate enum-vs-message at
		// MessageFieldDef granularity, and "object" is the safer
		// fallback (the agent host gets "structured payload here").
		return mcpFieldShape{Type: "object"}
	}
}
