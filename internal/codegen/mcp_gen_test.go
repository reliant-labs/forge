package codegen

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readManifest is the shared test helper — generates the manifest into
// a fresh tempdir and decodes it back into the testable JSON shape.
// The intermediate map[string]any keeps the assertions loose: tests
// pin only the fields they care about, so an additive extension to
// the manifest schema (e.g. a new "version" field) doesn't have to
// touch every test.
func readManifest(t *testing.T, in MCPGenInput) map[string]any {
	t.Helper()
	tmp := t.TempDir()
	in.ProjectDir = tmp
	if err := GenerateMCPManifest(in); err != nil {
		t.Fatalf("GenerateMCPManifest: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(tmp, "gen", "mcp", "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal manifest: %v\nraw:\n%s", err, raw)
	}
	return got
}

// tools coerces the manifest's "tools" array into []map[string]any
// for ergonomic field lookup. Test helpers like this read better than
// chains of `.(map[string]any)["tools"].([]any)[i].(map[string]any)`.
func tools(t *testing.T, manifest map[string]any) []map[string]any {
	t.Helper()
	raw, ok := manifest["tools"].([]any)
	if !ok {
		t.Fatalf("manifest.tools missing or wrong type: %T", manifest["tools"])
	}
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		out[i] = r.(map[string]any)
	}
	return out
}

// TestGenerateMCP_TwoServicesEmitManifest is the basic end-to-end
// smoke test: two services × two methods each must produce a manifest
// with four tools and the right (name, procedure) pairs.
func TestGenerateMCP_TwoServicesEmitManifest(t *testing.T) {
	in := MCPGenInput{
		ProjectName: "demo",
		Services: []ServiceDef{
			{
				Name:    "UserService",
				Package: "users.v1",
				Methods: []Method{
					{Name: "CreateUser", InputType: "CreateUserRequest", OutputType: "CreateUserResponse"},
					{Name: "ListUsers", InputType: "ListUsersRequest", OutputType: "ListUsersResponse"},
				},
			},
			{
				Name:    "PatientService",
				Package: "patients.v1",
				Methods: []Method{
					{Name: "GetPatient", InputType: "GetPatientRequest", OutputType: "GetPatientResponse"},
					{Name: "DeletePatient", InputType: "DeletePatientRequest", OutputType: "DeletePatientResponse"},
				},
			},
		},
	}
	manifest := readManifest(t, in)
	if got := manifest["project"]; got != "demo" {
		t.Errorf("project = %v, want demo", got)
	}
	if got := manifest["_generated"]; got != "forge" {
		t.Errorf("_generated = %v, want forge", got)
	}
	if got := manifest["schema_version"]; got != "1.1" {
		t.Errorf("schema_version = %v, want 1.1", got)
	}

	tt := tools(t, manifest)
	if len(tt) != 4 {
		t.Fatalf("len(tools) = %d, want 4", len(tt))
	}

	want := []struct {
		name      string
		procedure string
	}{
		{"user_service__create_user", "/users.v1.UserService/CreateUser"},
		{"user_service__list_users", "/users.v1.UserService/ListUsers"},
		{"patient_service__get_patient", "/patients.v1.PatientService/GetPatient"},
		{"patient_service__delete_patient", "/patients.v1.PatientService/DeletePatient"},
	}
	for i, w := range want {
		if got := tt[i]["name"]; got != w.name {
			t.Errorf("tools[%d].name = %v, want %v", i, got, w.name)
		}
		if got := tt[i]["procedure"]; got != w.procedure {
			t.Errorf("tools[%d].procedure = %v, want %v", i, got, w.procedure)
		}
	}
}

// TestGenerateMCP_AuthRequiredFlowsThrough confirms the per-method
// AuthRequired bool reaches the manifest unmolested for both
// values. The default-true semantic lives in the parser; here we
// verify that whatever the parser hands us is what the manifest
// publishes.
func TestGenerateMCP_AuthRequiredFlowsThrough(t *testing.T) {
	in := MCPGenInput{
		ProjectName: "demo",
		Services: []ServiceDef{
			{
				Name:    "MixedService",
				Package: "mixed.v1",
				Methods: []Method{
					{Name: "PrivateCall", InputType: "Empty", OutputType: "Empty", AuthRequired: true},
					{Name: "PublicCall", InputType: "Empty", OutputType: "Empty", AuthRequired: false},
				},
			},
		},
	}
	tt := tools(t, readManifest(t, in))
	if got := tt[0]["auth_required"]; got != true {
		t.Errorf("tools[0].auth_required = %v, want true", got)
	}
	if got := tt[1]["auth_required"]; got != false {
		t.Errorf("tools[1].auth_required = %v, want false", got)
	}
}

// TestGenerateMCP_StreamingMarked covers each streaming-mode value
// and the unary default (field omitted entirely thanks to
// omitempty). The MCP host uses this field to route to the right
// transport adapter — a missing field MUST mean unary.
func TestGenerateMCP_StreamingMarked(t *testing.T) {
	in := MCPGenInput{
		ProjectName: "demo",
		Services: []ServiceDef{
			{
				Name:    "StreamingService",
				Package: "s.v1",
				Methods: []Method{
					{Name: "Unary", InputType: "Empty", OutputType: "Empty"},
					{Name: "ServerStream", InputType: "Empty", OutputType: "Empty", ServerStreaming: true},
					{Name: "ClientStream", InputType: "Empty", OutputType: "Empty", ClientStreaming: true},
					{Name: "Bidi", InputType: "Empty", OutputType: "Empty", ClientStreaming: true, ServerStreaming: true},
				},
			},
		},
	}
	tt := tools(t, readManifest(t, in))

	if _, present := tt[0]["streaming"]; present {
		t.Errorf("unary tool should omit streaming field, got %v", tt[0]["streaming"])
	}
	if got := tt[1]["streaming"]; got != "server" {
		t.Errorf("server-stream streaming = %v, want server", got)
	}
	if got := tt[2]["streaming"]; got != "client" {
		t.Errorf("client-stream streaming = %v, want client", got)
	}
	if got := tt[3]["streaming"]; got != "bidi" {
		t.Errorf("bidi streaming = %v, want bidi", got)
	}
}

// TestGenerateMCP_ProtoTypesMapToJSONSchema covers the full
// proto→JSON-Schema mapping table for the v1 spec: string / int32 /
// bool / repeated string. Each field's expected JSON-Schema type is
// pinned. Required-list discipline (optional fields stay out) is
// asserted alongside.
func TestGenerateMCP_ProtoTypesMapToJSONSchema(t *testing.T) {
	in := MCPGenInput{
		ProjectName: "demo",
		Services: []ServiceDef{
			{
				Name:    "TypesService",
				Package: "types.v1",
				Methods: []Method{
					{Name: "Mixed", InputType: "MixedRequest", OutputType: "MixedResponse"},
				},
				Messages: map[string][]MessageFieldDef{
					"MixedRequest": {
						{Name: "name", ProtoType: "string"},
						{Name: "age", ProtoType: "int32"},
						{Name: "active", ProtoType: "bool"},
						{Name: "tags", ProtoType: "[]string"},
						{Name: "optional_note", ProtoType: "string", IsOptional: true},
					},
					"MixedResponse": {
						{Name: "ok", ProtoType: "bool"},
					},
				},
			},
		},
	}
	tt := tools(t, readManifest(t, in))
	in0 := tt[0]["inputSchema"].(map[string]any)
	if in0["type"] != "object" {
		t.Errorf("inputSchema.type = %v, want object", in0["type"])
	}
	props := in0["properties"].(map[string]any)

	checkType := func(field, want string) {
		t.Helper()
		f, ok := props[field].(map[string]any)
		if !ok {
			t.Fatalf("properties.%s missing", field)
		}
		if got := f["type"]; got != want {
			t.Errorf("properties.%s.type = %v, want %v", field, got, want)
		}
	}
	// Field names are emitted in lowerCamelCase to match what
	// protojson uses on the wire (proto3 JSON Mapping spec). The
	// proto field `optional_note` becomes `optionalNote`.
	checkType("name", "string")
	checkType("age", "number")
	checkType("active", "boolean")
	checkType("tags", "array")
	checkType("optionalNote", "string")

	// "tags" must declare its element type so the agent host can
	// generate a typed array argument.
	tags := props["tags"].(map[string]any)
	items, ok := tags["items"].(map[string]any)
	if !ok {
		t.Fatalf("tags.items missing or wrong type: %T", tags["items"])
	}
	if got := items["type"]; got != "string" {
		t.Errorf("tags.items.type = %v, want string", got)
	}

	// Required = every non-optional field, sorted. optional_note must
	// NOT appear; the other four must.
	requiredRaw, ok := in0["required"].([]any)
	if !ok {
		t.Fatalf("inputSchema.required missing: %#v", in0)
	}
	got := make([]string, len(requiredRaw))
	for i, r := range requiredRaw {
		got[i] = r.(string)
	}
	want := []string{"active", "age", "name", "tags"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("required = %v, want %v", got, want)
	}
}

// TestGenerateMCP_NoServicesNoManifest documents the
// empty-service-list contract: no file is written. The companion
// rationale is in the package doc — emitting `tools: []` would imply
// the project deliberately publishes zero tools, which is a stronger
// statement than the truth ("the project hasn't scaffolded a
// service yet"). MCP hosts treat the absent file as "no tools".
func TestGenerateMCP_NoServicesNoManifest(t *testing.T) {
	tmp := t.TempDir()
	if err := GenerateMCPManifest(MCPGenInput{GenContext: GenContext{ProjectDir: tmp}, ProjectName: "demo"}); err != nil {
		t.Fatalf("GenerateMCPManifest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "gen", "mcp", "manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("expected no manifest file for empty services, got err=%v", err)
	}
}

// TestGenerateMCP_Determinism pins the byte-stable contract: running
// twice with the same input produces identical output. forge's
// checksum-tracked Tier-1 contract depends on this — any
// non-deterministic field would flag every regen as a "user-edited"
// file.
func TestGenerateMCP_Determinism(t *testing.T) {
	in := MCPGenInput{
		ProjectName: "demo",
		Services: []ServiceDef{
			{
				Name:    "Svc",
				Package: "s.v1",
				Methods: []Method{
					{Name: "A", InputType: "AReq", OutputType: "AResp"},
				},
				Messages: map[string][]MessageFieldDef{
					"AReq":  {{Name: "x", ProtoType: "string"}, {Name: "y", ProtoType: "int32"}},
					"AResp": {{Name: "z", ProtoType: "bool"}},
				},
			},
		},
	}
	tmp1, tmp2 := t.TempDir(), t.TempDir()
	in1, in2 := in, in
	in1.ProjectDir, in2.ProjectDir = tmp1, tmp2
	if err := GenerateMCPManifest(in1); err != nil {
		t.Fatalf("first render: %v", err)
	}
	if err := GenerateMCPManifest(in2); err != nil {
		t.Fatalf("second render: %v", err)
	}
	a, _ := os.ReadFile(filepath.Join(tmp1, "gen", "mcp", "manifest.json"))
	b, _ := os.ReadFile(filepath.Join(tmp2, "gen", "mcp", "manifest.json"))
	if string(a) != string(b) {
		t.Errorf("non-deterministic manifest output:\n--- run1 ---\n%s\n--- run2 ---\n%s", a, b)
	}
}

// ---------------------------------------------------------------------------
// Deep-schema tests (manifest schema_version 1.1): full nesting via a
// per-schema $defs block keyed by fully-qualified name, $ref at field
// sites. Each test pins one schema shape: nested message, repeated
// message, map, enum, recursion, oneof, well-known Timestamp.
// ---------------------------------------------------------------------------

// deepServiceInput builds an MCPGenInput around a single one-method
// service whose deep type graph is given by schemas/enums. The method's
// input is rootFQ; output is google.protobuf.Empty so outputSchema stays
// out of the way unless a test overrides it.
func deepServiceInput(rootFQ string, schemas map[string][]SchemaFieldDef, enums map[string][]string) MCPGenInput {
	short := rootFQ[strings.LastIndex(rootFQ, ".")+1:]
	return MCPGenInput{
		ProjectName: "demo",
		Services: []ServiceDef{
			{
				Name:    "DeepService",
				Package: "deep.v1",
				Methods: []Method{
					{
						Name:         "Call",
						InputType:    short,
						InputTypeFQ:  rootFQ,
						OutputType:   "Empty",
						OutputTypeFQ: "google.protobuf.Empty",
					},
				},
				Schemas: schemas,
				Enums:   enums,
			},
		},
	}
}

// inputSchemaOf extracts the first tool's inputSchema.
func inputSchemaOf(t *testing.T, manifest map[string]any) map[string]any {
	t.Helper()
	tt := tools(t, manifest)
	in, ok := tt[0]["inputSchema"].(map[string]any)
	if !ok {
		t.Fatalf("inputSchema missing or wrong type: %T", tt[0]["inputSchema"])
	}
	return in
}

// defsOf extracts a schema's $defs block.
func defsOf(t *testing.T, schema map[string]any) map[string]any {
	t.Helper()
	defs, ok := schema["$defs"].(map[string]any)
	if !ok {
		t.Fatalf("$defs missing or wrong type: %T (schema: %#v)", schema["$defs"], schema)
	}
	return defs
}

// TestGenerateMCP_Deep_NestedMessage: a message field becomes a $ref
// into $defs, and the referenced definition carries the nested fields
// in full (including its own required list — input semantics apply at
// every level).
func TestGenerateMCP_Deep_NestedMessage(t *testing.T) {
	in := deepServiceInput("deep.v1.CreateOrderRequest",
		map[string][]SchemaFieldDef{
			"deep.v1.CreateOrderRequest": {
				{Name: "customer_name", Kind: "string"},
				{Name: "address", Kind: "message", TypeName: "deep.v1.Address"},
			},
			"deep.v1.Address": {
				{Name: "street", Kind: "string"},
				{Name: "zip_code", Kind: "string", Optional: true},
			},
		}, nil)
	schema := inputSchemaOf(t, readManifest(t, in))

	props := schema["properties"].(map[string]any)
	addr, ok := props["address"].(map[string]any)
	if !ok {
		t.Fatalf("properties.address missing: %#v", props)
	}
	if got := addr["$ref"]; got != "#/$defs/deep.v1.Address" {
		t.Errorf("address.$ref = %v, want #/$defs/deep.v1.Address", got)
	}

	defs := defsOf(t, schema)
	def, ok := defs["deep.v1.Address"].(map[string]any)
	if !ok {
		t.Fatalf("$defs missing deep.v1.Address: %#v", defs)
	}
	defProps := def["properties"].(map[string]any)
	if got := defProps["street"].(map[string]any)["type"]; got != "string" {
		t.Errorf("Address.street.type = %v, want string", got)
	}
	if _, ok := defProps["zipCode"]; !ok {
		t.Errorf("Address.zip_code must surface as lowerCamel zipCode: %#v", defProps)
	}
	// Nested required: street yes, zipCode (optional) no.
	req, ok := def["required"].([]any)
	if !ok || len(req) != 1 || req[0] != "street" {
		t.Errorf("Address.required = %#v, want [street]", def["required"])
	}
}

// TestGenerateMCP_Deep_RepeatedMessage: repeated message fields become
// arrays whose items $ref the element definition.
func TestGenerateMCP_Deep_RepeatedMessage(t *testing.T) {
	in := deepServiceInput("deep.v1.BatchRequest",
		map[string][]SchemaFieldDef{
			"deep.v1.BatchRequest": {
				{Name: "items", Kind: "message", TypeName: "deep.v1.Item", Repeated: true},
			},
			"deep.v1.Item": {
				{Name: "id", Kind: "int64"},
			},
		}, nil)
	schema := inputSchemaOf(t, readManifest(t, in))

	items := schema["properties"].(map[string]any)["items"].(map[string]any)
	if got := items["type"]; got != "array" {
		t.Errorf("items.type = %v, want array", got)
	}
	elem := items["items"].(map[string]any)
	if got := elem["$ref"]; got != "#/$defs/deep.v1.Item" {
		t.Errorf("items.items.$ref = %v, want #/$defs/deep.v1.Item", got)
	}
	if _, ok := defsOf(t, schema)["deep.v1.Item"]; !ok {
		t.Error("$defs must contain deep.v1.Item")
	}
}

// TestGenerateMCP_Deep_Maps: proto maps become JSON objects with
// additionalProperties — scalar values inline, message values $ref.
func TestGenerateMCP_Deep_Maps(t *testing.T) {
	in := deepServiceInput("deep.v1.TagRequest",
		map[string][]SchemaFieldDef{
			"deep.v1.TagRequest": {
				{Name: "labels", Kind: "map", MapKeyKind: "string", MapValueKind: "string"},
				{Name: "parts", Kind: "map", MapKeyKind: "int64", MapValueKind: "message", MapValueTypeName: "deep.v1.Part"},
			},
			"deep.v1.Part": {
				{Name: "sku", Kind: "string"},
			},
		}, nil)
	schema := inputSchemaOf(t, readManifest(t, in))
	props := schema["properties"].(map[string]any)

	labels := props["labels"].(map[string]any)
	if got := labels["type"]; got != "object" {
		t.Errorf("labels.type = %v, want object", got)
	}
	ap := labels["additionalProperties"].(map[string]any)
	if got := ap["type"]; got != "string" {
		t.Errorf("labels.additionalProperties.type = %v, want string", got)
	}

	parts := props["parts"].(map[string]any)
	pap := parts["additionalProperties"].(map[string]any)
	if got := pap["$ref"]; got != "#/$defs/deep.v1.Part" {
		t.Errorf("parts.additionalProperties.$ref = %v, want #/$defs/deep.v1.Part", got)
	}
	if _, ok := defsOf(t, schema)["deep.v1.Part"]; !ok {
		t.Error("$defs must contain deep.v1.Part")
	}
}

// TestGenerateMCP_Deep_Enum: enum fields $ref a string-typed definition
// listing the allowed value names (protojson encodes enums by name).
func TestGenerateMCP_Deep_Enum(t *testing.T) {
	in := deepServiceInput("deep.v1.SetStatusRequest",
		map[string][]SchemaFieldDef{
			"deep.v1.SetStatusRequest": {
				{Name: "status", Kind: "enum", TypeName: "deep.v1.Status"},
			},
		},
		map[string][]string{
			"deep.v1.Status": {"STATUS_UNSPECIFIED", "STATUS_ACTIVE", "STATUS_ARCHIVED"},
		})
	schema := inputSchemaOf(t, readManifest(t, in))

	status := schema["properties"].(map[string]any)["status"].(map[string]any)
	if got := status["$ref"]; got != "#/$defs/deep.v1.Status" {
		t.Errorf("status.$ref = %v, want #/$defs/deep.v1.Status", got)
	}
	def := defsOf(t, schema)["deep.v1.Status"].(map[string]any)
	if got := def["type"]; got != "string" {
		t.Errorf("Status.type = %v, want string", got)
	}
	vals := def["enum"].([]any)
	want := []string{"STATUS_UNSPECIFIED", "STATUS_ACTIVE", "STATUS_ARCHIVED"}
	if len(vals) != len(want) {
		t.Fatalf("Status.enum = %#v, want %v", vals, want)
	}
	for i, w := range want {
		if vals[i] != w {
			t.Errorf("Status.enum[%d] = %v, want %v (declaration order must hold)", i, vals[i], w)
		}
	}
}

// TestGenerateMCP_Deep_Recursive: a self-referential message must
// terminate via $ref — the definition appears exactly once in $defs and
// references itself, instead of inlining forever.
func TestGenerateMCP_Deep_Recursive(t *testing.T) {
	in := deepServiceInput("deep.v1.Tree",
		map[string][]SchemaFieldDef{
			"deep.v1.Tree": {
				{Name: "label", Kind: "string"},
				{Name: "children", Kind: "message", TypeName: "deep.v1.Tree", Repeated: true},
			},
		}, nil)
	schema := inputSchemaOf(t, readManifest(t, in))

	// Root inlines the object schema; children $ref the $defs entry.
	children := schema["properties"].(map[string]any)["children"].(map[string]any)
	elem := children["items"].(map[string]any)
	if got := elem["$ref"]; got != "#/$defs/deep.v1.Tree" {
		t.Errorf("children.items.$ref = %v, want #/$defs/deep.v1.Tree", got)
	}

	defs := defsOf(t, schema)
	if len(defs) != 1 {
		t.Fatalf("$defs should hold exactly the one recursive definition, got %d: %#v", len(defs), defs)
	}
	def := defs["deep.v1.Tree"].(map[string]any)
	// The definition's own children field must ALSO be a $ref (this is
	// the recursion-termination property).
	defChildren := def["properties"].(map[string]any)["children"].(map[string]any)
	if got := defChildren["items"].(map[string]any)["$ref"]; got != "#/$defs/deep.v1.Tree" {
		t.Errorf("$defs Tree.children.items.$ref = %v, want self-$ref", got)
	}
}

// TestGenerateMCP_Deep_Oneof: oneof members are never required and each
// carries a description naming the group and all its members — the
// exclusivity contract an agent needs to construct valid arguments.
// (Description note instead of anyOf: several MCP clients / LLM
// providers mishandle non-root anyOf; descriptions pass through
// everywhere.)
func TestGenerateMCP_Deep_Oneof(t *testing.T) {
	in := deepServiceInput("deep.v1.NotifyRequest",
		map[string][]SchemaFieldDef{
			"deep.v1.NotifyRequest": {
				{Name: "subject", Kind: "string"},
				{Name: "email_address", Kind: "string", Oneof: "target"},
				{Name: "phone_number", Kind: "string", Oneof: "target"},
			},
		}, nil)
	schema := inputSchemaOf(t, readManifest(t, in))
	props := schema["properties"].(map[string]any)

	for _, member := range []string{"emailAddress", "phoneNumber"} {
		site, ok := props[member].(map[string]any)
		if !ok {
			t.Fatalf("properties.%s missing", member)
		}
		desc, _ := site["description"].(string)
		if !strings.Contains(desc, `oneof "target"`) ||
			!strings.Contains(desc, "emailAddress") ||
			!strings.Contains(desc, "phoneNumber") {
			t.Errorf("%s.description must name the oneof group and all members, got %q", member, desc)
		}
	}

	// Only the plain field is required; oneof members never are.
	req := schema["required"].([]any)
	if len(req) != 1 || req[0] != "subject" {
		t.Errorf("required = %#v, want [subject] (oneof members must not be required)", req)
	}
}

// TestGenerateMCP_Deep_Timestamp: google.protobuf.Timestamp maps to the
// protojson wire encoding (RFC 3339 string), inline at the field site —
// NOT a $defs entry describing seconds/nanos the server never sends.
func TestGenerateMCP_Deep_Timestamp(t *testing.T) {
	in := deepServiceInput("deep.v1.ScheduleRequest",
		map[string][]SchemaFieldDef{
			"deep.v1.ScheduleRequest": {
				{Name: "run_at", Kind: "message", TypeName: "google.protobuf.Timestamp"},
			},
		}, nil)
	schema := inputSchemaOf(t, readManifest(t, in))

	runAt := schema["properties"].(map[string]any)["runAt"].(map[string]any)
	if got := runAt["type"]; got != "string" {
		t.Errorf("runAt.type = %v, want string", got)
	}
	if got := runAt["format"]; got != "date-time" {
		t.Errorf("runAt.format = %v, want date-time", got)
	}
	if _, hasDefs := schema["$defs"]; hasDefs {
		t.Errorf("well-known-only schema must not emit $defs: %#v", schema["$defs"])
	}
}

// TestGenerateMCP_Deep_OutputSchemaNeverRequired: output schemas omit
// required at EVERY level (protojson drops default-valued fields, so a
// required output field fails strict validation on zero values). The
// nested definition built for the output schema must not carry a
// required list either.
func TestGenerateMCP_Deep_OutputSchemaNeverRequired(t *testing.T) {
	in := MCPGenInput{
		ProjectName: "demo",
		Services: []ServiceDef{
			{
				Name:    "DeepService",
				Package: "deep.v1",
				Methods: []Method{
					{
						Name:         "Get",
						InputType:    "Empty",
						InputTypeFQ:  "google.protobuf.Empty",
						OutputType:   "GetResponse",
						OutputTypeFQ: "deep.v1.GetResponse",
					},
				},
				Schemas: map[string][]SchemaFieldDef{
					"deep.v1.GetResponse": {
						{Name: "item", Kind: "message", TypeName: "deep.v1.Item"},
					},
					"deep.v1.Item": {
						{Name: "id", Kind: "int64"},
						{Name: "name", Kind: "string"},
					},
				},
			},
		},
	}
	tt := tools(t, readManifest(t, in))
	out := tt[0]["outputSchema"].(map[string]any)
	if _, ok := out["required"]; ok {
		t.Errorf("outputSchema must not have required: %#v", out["required"])
	}
	item := defsOf(t, out)["deep.v1.Item"].(map[string]any)
	if _, ok := item["required"]; ok {
		t.Errorf("output $defs entries must not have required: %#v", item["required"])
	}

	// The Empty input maps to a plain empty object schema.
	inSchema := tt[0]["inputSchema"].(map[string]any)
	if got := inSchema["type"]; got != "object" {
		t.Errorf("Empty inputSchema.type = %v, want object", got)
	}
}

// TestGenerateMCP_Deep_FallbackToShallow: a descriptor without the deep
// type graph (old forge version — no InputTypeFQ / Schemas) must keep
// producing the historic one-level-deep schema, not an empty one.
func TestGenerateMCP_Deep_FallbackToShallow(t *testing.T) {
	in := MCPGenInput{
		ProjectName: "demo",
		Services: []ServiceDef{
			{
				Name:    "OldService",
				Package: "old.v1",
				Methods: []Method{
					{Name: "Do", InputType: "DoRequest", OutputType: "DoResponse"},
				},
				Messages: map[string][]MessageFieldDef{
					"DoRequest":  {{Name: "name", ProtoType: "string"}, {Name: "payload", ProtoType: "message"}},
					"DoResponse": {{Name: "ok", ProtoType: "bool"}},
				},
			},
		},
	}
	schema := inputSchemaOf(t, readManifest(t, in))
	props := schema["properties"].(map[string]any)
	if got := props["name"].(map[string]any)["type"]; got != "string" {
		t.Errorf("fallback name.type = %v, want string", got)
	}
	if got := props["payload"].(map[string]any)["type"]; got != "object" {
		t.Errorf("fallback payload.type = %v, want object (legacy opaque message)", got)
	}
	req := schema["required"].([]any)
	if len(req) != 2 {
		t.Errorf("fallback required = %#v, want [name payload]", req)
	}
}

// TestGenerateMCP_Deep_Determinism: the deep path (maps + worklist) must
// stay byte-deterministic — same input, identical bytes — or every
// regen would flag the manifest as user-edited under checksum tracking.
func TestGenerateMCP_Deep_Determinism(t *testing.T) {
	in := deepServiceInput("deep.v1.Root",
		map[string][]SchemaFieldDef{
			"deep.v1.Root": {
				{Name: "a", Kind: "message", TypeName: "deep.v1.A"},
				{Name: "b", Kind: "message", TypeName: "deep.v1.B"},
				{Name: "status", Kind: "enum", TypeName: "deep.v1.Status"},
				{Name: "labels", Kind: "map", MapKeyKind: "string", MapValueKind: "message", MapValueTypeName: "deep.v1.A"},
			},
			"deep.v1.A": {{Name: "x", Kind: "string"}, {Name: "next", Kind: "message", TypeName: "deep.v1.B"}},
			"deep.v1.B": {{Name: "y", Kind: "int32"}, {Name: "back", Kind: "message", TypeName: "deep.v1.A"}},
		},
		map[string][]string{"deep.v1.Status": {"S0", "S1"}})

	tmp1, tmp2 := t.TempDir(), t.TempDir()
	in1, in2 := in, in
	in1.ProjectDir, in2.ProjectDir = tmp1, tmp2
	if err := GenerateMCPManifest(in1); err != nil {
		t.Fatalf("first render: %v", err)
	}
	if err := GenerateMCPManifest(in2); err != nil {
		t.Fatalf("second render: %v", err)
	}
	a, _ := os.ReadFile(filepath.Join(tmp1, "gen", "mcp", "manifest.json"))
	b, _ := os.ReadFile(filepath.Join(tmp2, "gen", "mcp", "manifest.json"))
	if string(a) != string(b) {
		t.Errorf("non-deterministic deep manifest:\n--- run1 ---\n%s\n--- run2 ---\n%s", a, b)
	}
	// Mutually-recursive A↔B must both land in $defs exactly once.
	var m map[string]any
	if err := json.Unmarshal(a, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	defs := defsOf(t, inputSchemaOf(t, m))
	for _, fq := range []string{"deep.v1.A", "deep.v1.B", "deep.v1.Status"} {
		if _, ok := defs[fq]; !ok {
			t.Errorf("$defs missing %s", fq)
		}
	}
}
