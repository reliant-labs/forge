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
	if got := manifest["schema_version"]; got != "1.0" {
		t.Errorf("schema_version = %v, want 1.0", got)
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
	if err := GenerateMCPManifest(MCPGenInput{ProjectDir: tmp, ProjectName: "demo"}); err != nil {
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
