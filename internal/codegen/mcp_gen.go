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
//	  "schema_version": "1.1",
//	  "project": "<project name>",
//	  "tools": [
//	    {
//	      "name": "<service_snake>__<rpc_snake>",
//	      "description": "<RPC doc-comment, or empty>",
//	      "inputSchema":  {"type": "object", "properties": {...}, "required": [...], "$defs": {...}},
//	      "outputSchema": {"type": "object", "properties": {...}, "$defs": {...}},
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
// Schema depth (schema_version 1.1): each inputSchema/outputSchema is a
// SELF-CONTAINED JSON Schema. Nested proto messages are emitted in full
// via a top-level "$defs" block keyed by fully-qualified message/enum
// name, with "$ref": "#/$defs/<fq-name>" at every field site. One
// definition per message regardless of how many fields use it, which
// makes depth unlimited without size blowup and makes recursion
// terminate (self-referential messages reference themselves via $ref
// instead of inlining forever). $defs is per-schema rather than
// manifest-global because MCP clients receive each tool's inputSchema
// as a standalone document — a cross-document $ref would be
// unresolvable on the client side. It also lets input and output
// projections of the same message differ (required lists exist only in
// input schemas; see schemaForType).
//
// Fallback: descriptors produced by forge versions without the deep
// type graph (ServiceDef.Schemas absent) degrade to the historic
// one-level-deep projection so the manifest never regresses below what
// it used to publish.
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

	"github.com/reliant-labs/forge/internal/naming"
)

// MCPGenInput is the per-project input for [GenerateMCPManifest]. The
// shape mirrors K3dPortsGenInput so the call-site in the generate
// pipeline stays uniform across codegen emitters.
// Embeds GenContext for ProjectDir + Checksums (ModulePath is unused —
// the manifest is JSON, not Go).
type MCPGenInput struct {
	GenContext

	ProjectName string       // emitted as the manifest's "project" field; "" tolerated
	Services    []ServiceDef // every parsed Connect service; empty → no-op
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
	Name        string `json:"name"`
	Description string `json:"description"`
	// InputSchema / OutputSchema are JSON Schema documents built as
	// map[string]any. encoding/json marshals map keys in sorted order,
	// so the output stays byte-deterministic (a checksum-tracked Tier-1
	// requirement) even though the schemas now have a variable shape
	// ($defs / $ref / enum / additionalProperties / format keys appear
	// only where the proto calls for them).
	InputSchema    map[string]any `json:"inputSchema"`
	OutputSchema   map[string]any `json:"outputSchema"`
	Service        string         `json:"service"`
	Method         string         `json:"method"`
	Procedure      string         `json:"procedure"`
	AuthRequired   bool           `json:"auth_required"`
	IdempotencyKey bool           `json:"idempotency_key"`
	// Streaming is omitted (json:",omitempty") for unary RPCs so
	// non-streaming tools don't carry a confusing empty field. Values
	// are "client", "server", or "bidi" for the three Connect modes.
	// forge-mcp EXCLUDES streaming tools from MCP tools/list — they
	// stay in the manifest for non-MCP consumers (audit, doctor,
	// future stream-capable bridges).
	Streaming string `json:"streaming,omitempty"`
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
				InputSchema:    schemaForType(svc, m.InputType, m.InputTypeFQ, false),
				OutputSchema:   schemaForType(svc, m.OutputType, m.OutputTypeFQ, true),
				Streaming:      streamingMode(m),
			})
		}
	}

	manifest := mcpManifest{
		Generated: "forge",
		// 1.1: schemas became deep ($defs + $ref, full nesting). The
		// bump is additive — 1.0 consumers that treated schemas as
		// opaque JSON keep working; consumers that resolved properties
		// see strictly more information.
		SchemaVersion: "1.1",
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
	if err := writeForgeOwned(in.ProjectDir, rel, body, in.Checksums); err != nil {
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

// schemaForType is the single entry point for projecting an RPC's
// request/response message into a JSON Schema. It picks the deep path
// (full nesting via $defs/$ref) when the descriptor carries the type
// graph, and degrades to the historic one-level-deep projection for
// descriptors generated by older forge versions (no fqName, or the
// fqName missing from svc.Schemas).
//
// Shared semantics across both paths (proto3 + protojson, validated
// e2e against a live forge server + the official MCP Inspector):
//
//   - Field names are converted to lowerCamelCase. protojson always
//     emits lowerCamelCase per the proto3 JSON Mapping spec; using
//     the snake_case proto-original name produces a manifest the
//     server's responses never actually match.
//
//   - Output schemas mark NOTHING required, at any level. protojson
//     omits default-valued fields (empty strings, zero numbers, false
//     booleans, empty messages) unless the server opts into
//     EmitUnpopulated; a required output field would fail strict
//     validation on every zero-valued response.
//
//   - Input schemas mark non-optional, non-oneof fields required at
//     every level — those ARE the contract the agent must satisfy.
//     Oneof members are never required (at most one may be set).
func schemaForType(svc ServiceDef, msgName, fqName string, outputSchema bool) map[string]any {
	if fqName != "" {
		if wk, ok := wellKnownSchema(fqName); ok {
			return wk
		}
		if _, ok := svc.Schemas[fqName]; ok {
			return deepSchemaFor(svc, fqName, outputSchema)
		}
	}
	return legacySchemaFromMessage(svc, msgName, outputSchema)
}

// deepSchemaFor builds the self-contained JSON Schema for one root
// message: the root's object schema inlined at the top level (MCP
// requires inputSchema.type == "object" at the root) plus a "$defs"
// block containing every message/enum definition transitively
// referenced from it, keyed by fully-qualified name.
//
// Recursion safety: definitions are collected by a worklist over
// fully-qualified names with a built-set guard, and field sites always
// point INTO $defs via $ref — so a self-referential message contributes
// exactly one definition that $refs itself, never an infinite inline
// expansion. When the root message is itself recursive it appears in
// $defs too (the inlined top-level copy and the $defs entry are
// identical; refs resolve to the $defs one).
func deepSchemaFor(svc ServiceDef, rootFQ string, outputSchema bool) map[string]any {
	needed := map[string]bool{}
	root := deepObjectSchema(svc, rootFQ, outputSchema, needed)

	defs := map[string]any{}
	built := map[string]bool{}
	for {
		// Drain the needed-set deterministically. New names can be
		// added while building (nested messages reference further
		// messages), so loop until a full pass adds nothing.
		pending := make([]string, 0, len(needed))
		for fq := range needed {
			if !built[fq] {
				pending = append(pending, fq)
			}
		}
		if len(pending) == 0 {
			break
		}
		sort.Strings(pending)
		for _, fq := range pending {
			built[fq] = true
			if vals, ok := svc.Enums[fq]; ok {
				defs[fq] = enumSchema(vals)
				continue
			}
			defs[fq] = deepObjectSchema(svc, fq, outputSchema, needed)
		}
	}
	if len(defs) > 0 {
		root["$defs"] = defs
	}
	return root
}

// deepObjectSchema renders one message's object schema from the deep
// type graph. Message/enum-typed fields become {"$ref": "#/$defs/..."}
// and their fully-qualified names are added to needed so the caller
// can materialize the definitions.
func deepObjectSchema(svc ServiceDef, fq string, outputSchema bool, needed map[string]bool) map[string]any {
	fields := svc.Schemas[fq]
	props := make(map[string]any, len(fields))

	// Pre-compute oneof groups so each member's description can name
	// its siblings — "set at most one of: a, b" is only useful when
	// the agent can see the whole group from any one field.
	oneofMembers := map[string][]string{}
	for _, f := range fields {
		if f.Oneof != "" {
			oneofMembers[f.Oneof] = append(oneofMembers[f.Oneof], toLowerCamelCase(f.Name))
		}
	}

	var required []string
	for _, f := range fields {
		name := toLowerCamelCase(f.Name)
		site := deepFieldSite(svc, f, needed)
		if f.Oneof != "" {
			// Oneof exclusivity is documented as a description note
			// rather than an anyOf combinator: anyOf-of-property-sets
			// is mishandled by several MCP clients / LLM providers
			// (OpenAI structured outputs reject non-root anyOf shapes;
			// Gemini's schema subset historically dropped them), while
			// every client passes descriptions through to the model
			// verbatim. The note carries the same information —
			// protojson enforces at-most-one server-side regardless.
			note := fmt.Sprintf("Part of oneof %q — set at most one of: %s.",
				f.Oneof, strings.Join(oneofMembers[f.Oneof], ", "))
			if d, ok := site["description"].(string); ok && d != "" {
				note = d + " " + note
			}
			site["description"] = note
		}
		props[name] = site
		if !outputSchema && !f.Optional && f.Oneof == "" {
			required = append(required, name)
		}
	}

	out := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		// Sorted so the output is deterministic across runs even if a
		// hand-edited proto shuffles field order mid-message.
		sort.Strings(required)
		out["required"] = required
	}
	return out
}

// deepFieldSite renders the schema fragment for a single field site
// from its SchemaFieldDef. Message/enum targets defined in the
// service's type graph become $refs (and land in needed); well-known
// types get their fixed protojson mapping inline; unknown targets
// degrade to bare {"type":"object"} — "structured payload here, fields
// unknown", same contract as the legacy shallow path.
func deepFieldSite(svc ServiceDef, f SchemaFieldDef, needed map[string]bool) map[string]any {
	var site map[string]any
	switch f.Kind {
	case "map":
		// protojson encodes proto maps as JSON objects. Keys are always
		// JSON strings regardless of the proto key kind (int64 keys are
		// stringified), so only the value schema is constrained.
		site = map[string]any{
			"type":                 "object",
			"additionalProperties": deepTypeRef(svc, f.MapValueKind, f.MapValueTypeName, needed),
		}
	case "message", "enum":
		site = deepTypeRef(svc, f.Kind, f.TypeName, needed)
	default:
		site = scalarSchema(f.Kind)
	}
	if f.Repeated {
		site = map[string]any{"type": "array", "items": site}
	}
	return site
}

// deepTypeRef resolves a message/enum (or scalar map-value) target to
// its field-site schema: $ref for graph-defined types, fixed mapping
// for well-knowns, scalar mapping otherwise.
func deepTypeRef(svc ServiceDef, kind, typeName string, needed map[string]bool) map[string]any {
	switch kind {
	case "message":
		if wk, ok := wellKnownSchema(typeName); ok {
			return wk
		}
		if _, ok := svc.Schemas[typeName]; ok {
			needed[typeName] = true
			return map[string]any{"$ref": "#/$defs/" + typeName}
		}
		return map[string]any{"type": "object"}
	case "enum":
		if _, ok := svc.Enums[typeName]; ok {
			needed[typeName] = true
			return map[string]any{"$ref": "#/$defs/" + typeName}
		}
		// Enum outside the graph: protojson still encodes it as a
		// string, we just can't enumerate the values.
		return map[string]any{"type": "string"}
	default:
		return scalarSchema(kind)
	}
}

// enumSchema is the $defs entry for a proto enum: protojson encodes
// enum values as their declared names, so the JSON type is string with
// the allowed-value list verbatim (declaration order — the order is
// part of the proto's documentation value).
func enumSchema(values []string) map[string]any {
	vals := make([]any, len(values))
	for i, v := range values {
		vals[i] = v
	}
	return map[string]any{"type": "string", "enum": vals}
}

// wellKnownSchema maps google.protobuf.* types to their protojson wire
// encodings. These are fixed by the proto3 JSON Mapping spec — emitting
// their proto field structure (e.g. Timestamp's seconds/nanos) would
// describe a shape the server never sends.
func wellKnownSchema(fq string) (map[string]any, bool) {
	switch fq {
	case "google.protobuf.Timestamp":
		return map[string]any{
			"type":        "string",
			"format":      "date-time",
			"description": "RFC 3339 timestamp (protojson encoding of google.protobuf.Timestamp), e.g. \"2026-01-01T12:00:00Z\".",
		}, true
	case "google.protobuf.Duration":
		return map[string]any{
			"type":        "string",
			"description": "Duration as decimal seconds with \"s\" suffix (protojson encoding of google.protobuf.Duration), e.g. \"3.5s\".",
		}, true
	case "google.protobuf.Struct":
		return map[string]any{
			"type":                 "object",
			"additionalProperties": true,
			"description":          "Arbitrary JSON object (google.protobuf.Struct).",
		}, true
	case "google.protobuf.Value":
		// Any JSON value — no "type" key at all is the JSON Schema way
		// to say "unconstrained".
		return map[string]any{
			"description": "Any JSON value (google.protobuf.Value).",
		}, true
	case "google.protobuf.ListValue":
		return map[string]any{
			"type":        "array",
			"description": "Arbitrary JSON array (google.protobuf.ListValue).",
		}, true
	case "google.protobuf.FieldMask":
		return map[string]any{
			"type":        "string",
			"description": "Field mask: comma-separated lowerCamelCase field paths (protojson encoding of google.protobuf.FieldMask).",
		}, true
	case "google.protobuf.Any":
		return map[string]any{
			"type":        "object",
			"description": "protojson Any: object carrying \"@type\" (type URL) plus the packed message's fields.",
		}, true
	case "google.protobuf.Empty":
		return map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}, true
	case "google.protobuf.StringValue", "google.protobuf.BytesValue":
		return map[string]any{"type": "string"}, true
	case "google.protobuf.BoolValue":
		return map[string]any{"type": "boolean"}, true
	case "google.protobuf.Int32Value", "google.protobuf.UInt32Value",
		"google.protobuf.Int64Value", "google.protobuf.UInt64Value",
		"google.protobuf.FloatValue", "google.protobuf.DoubleValue":
		return map[string]any{"type": "number"}, true
	}
	return nil, false
}

// scalarSchema maps a bare proto scalar kind to its JSON Schema type.
// All numerics collapse to "number" rather than "integer"/"number"
// because Connect's JSON wire format sends 64-bit ints as strings —
// the agent host can't safely assume integerness without consulting
// the proto directly; "number" is the looser, safer label. bytes maps
// to string (base64 on the wire per protojson).
func scalarSchema(kind string) map[string]any {
	switch kind {
	case "string":
		return map[string]any{"type": "string"}
	case "bool":
		return map[string]any{"type": "boolean"}
	case "int32", "int64", "uint32", "uint64",
		"sint32", "sint64",
		"fixed32", "fixed64", "sfixed32", "sfixed64",
		"float", "double":
		return map[string]any{"type": "number"}
	case "bytes":
		return map[string]any{"type": "string"}
	default:
		// Unrecognized kind — same safe fallback as the legacy path.
		return map[string]any{"type": "object"}
	}
}

// legacySchemaFromMessage is the pre-1.1 one-level-deep projection,
// kept verbatim as the fallback for descriptors that predate the deep
// type graph (ServiceDef.Schemas absent). When the message isn't known
// (e.g. a cross-file message an old descriptor didn't carry fields
// for), we emit an empty object schema — the agent host knows "this
// RPC takes an object; we don't know its fields" which is strictly
// better than the bogus alternative of "this RPC takes nothing".
func legacySchemaFromMessage(svc ServiceDef, msgName string, outputSchema bool) map[string]any {
	props := map[string]any{}
	out := map[string]any{
		"type":       "object",
		"properties": props,
	}
	fields, ok := svc.Messages[msgName]
	if !ok {
		return out
	}

	var required []string
	for _, f := range fields {
		name := toLowerCamelCase(f.Name)
		props[name] = legacyFieldShape(f.ProtoType)
		if !outputSchema && !f.IsOptional {
			required = append(required, name)
		}
	}
	if len(required) > 0 {
		// Sorted for determinism (see deepObjectSchema).
		sort.Strings(required)
		out["required"] = required
	}
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

// legacyFieldShape maps a legacy MessageFieldDef.ProtoType string to
// the minimal JSON-Schema type entry:
//
//   - repeated X (the descriptor encodes this as "[]X") → array with
//     items of X's mapping
//   - scalars → scalarSchema's table
//   - anything else (nested message types, enums — the legacy
//     descriptor doesn't disambiguate) → "object", the safer fallback
//     (the agent host gets "structured payload here")
func legacyFieldShape(protoType string) map[string]any {
	// Repeated fields: strip the "[]" prefix and recurse for the
	// element type.
	if len(protoType) >= 2 && protoType[0] == '[' && protoType[1] == ']' {
		return map[string]any{"type": "array", "items": legacyFieldShape(protoType[2:])}
	}
	return scalarSchema(protoType)
}
