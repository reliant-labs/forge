// Tests for the forge-mcp stdio bridge. The shape we exercise:
//
//   - JSON-RPC framing round-trip (one request line in, one response
//     line out).
//   - initialize returns the protocol version + capabilities + serverInfo.
//   - tools/list returns the manifest's tools with only the MCP-spec
//     fields (name, description, inputSchema, outputSchema) — the
//     forge-specific snake_case extras are stripped from the wire
//     response so strict MCP clients accept it.
//   - tools/call ACTUALLY DISPATCHES to a Connect-shaped HTTP backend
//     (stood up via httptest in each relevant test) and returns the
//     decoded response as structuredContent + a content text block.
//   - tools/call surfaces Connect application errors as MCP
//     isError=true results, NOT as JSON-RPC errors (per MCP convention:
//     business errors are tool results, not RPC-layer failures).
//   - tools/call propagates Authorization headers when configured.
//   - tools/call refuses streaming RPCs with a clear explanation.
//   - tools/call refuses to dispatch when --addr is not configured.
//   - Unknown tool → JSON-RPC -32602.
//   - Unknown method → JSON-RPC -32601.
//
// The httptest-backed cases prove the bridge is no longer a stub: a
// real HTTP request goes out, a real Connect-shaped response comes
// back, and the MCP client sees the wire data, not synthesized
// placeholders. The first pass of validation found two real spec
// violations (snake_case fields; missing structuredContent); this test
// suite makes a third class — silent stub behavior — impossible to
// re-introduce.

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fixtureManifest is the in-memory equivalent of what
// gen/mcp/manifest.json carries for a small project. Two tools — one
// with an outputSchema (Create) and one without (Ping) — so we can
// exercise the structuredContent branch and the content-only branch
// side by side.
func fixtureManifest() *manifest {
	return &manifest{
		Generated:     "forge",
		SchemaVersion: "1.0",
		Project:       "demo",
		Tools: []tool{
			{
				Name:        "tasks_service__create",
				Description: "Create a task.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":        map[string]any{"type": "string"},
						"description": map[string]any{"type": "string"},
					},
					"required": []any{"name"},
				},
				OutputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"item": map[string]any{"type": "object"},
					},
					"required": []any{"item"},
				},
				Service:      "TasksService",
				Method:       "Create",
				Procedure:    "/services.tasks.v1.TasksService/Create",
				AuthRequired: true,
			},
			{
				Name:        "tasks_service__ping",
				Description: "Liveness check.",
				InputSchema: map[string]any{"type": "object"},
				// No OutputSchema — exercises the content-only branch.
				Service:   "TasksService",
				Method:    "Ping",
				Procedure: "/services.tasks.v1.TasksService/Ping",
			},
		},
	}
}

// roundtrip wires one request line through the server's run() loop
// and returns the decoded response. Mirrors what an MCP client over
// stdio actually does — one JSON-RPC frame per line. addr+authHeader
// are zero-valued for tests that don't exercise tools/call dispatch;
// dispatch tests use roundtripWithBackend instead.
func roundtrip(t *testing.T, m *manifest, raw string) rpcResponse {
	t.Helper()
	return roundtripFull(t, m, "", "", raw)
}

// roundtripWithBackend stands up a real httptest Connect-shaped backend
// in front of the bridge and runs one JSON-RPC frame through. Returns
// (response, captured-backend-state) so individual tests can assert on
// both the MCP-visible result AND the on-wire HTTP request the backend
// saw — that pair is what proves the bridge is dispatching for real
// rather than synthesizing.
type backendCapture struct {
	calls []backendCall
}

type backendCall struct {
	method  string
	path    string
	headers http.Header
	body    map[string]any
}

// roundtripWithBackend uses the supplied handler as the Connect backend.
// Tests pass handlers that simulate success, application errors, slow
// responses, etc. authHeader is propagated as --auth.
func roundtripWithBackend(t *testing.T, m *manifest, authHeader string, handler http.HandlerFunc, raw string) (rpcResponse, *backendCapture) {
	t.Helper()
	cap := &backendCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readJSON(r)
		cap.calls = append(cap.calls, backendCall{
			method:  r.Method,
			path:    r.URL.Path,
			headers: r.Header.Clone(),
			body:    body,
		})
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	resp := roundtripFull(t, m, srv.URL, authHeader, raw)
	return resp, cap
}

// roundtripFull is the shared core that all roundtrip helpers route
// through. Keeps the server construction (HTTP client + timeouts) in
// one place so tests don't drift apart in what they exercise.
func roundtripFull(t *testing.T, m *manifest, addr, authHeader, raw string) rpcResponse {
	t.Helper()
	s := &server{
		manifest:   m,
		addr:       strings.TrimRight(addr, "/"),
		authHeader: authHeader,
		http:       &http.Client{Timeout: 5 * time.Second},
	}
	var out bytes.Buffer
	if err := s.run(strings.NewReader(raw+"\n"), &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	var resp rpcResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %q: %v", out.String(), err)
	}
	return resp
}

func readJSON(r *http.Request) (map[string]any, error) {
	var out map[string]any
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func TestInitialize(t *testing.T) {
	resp := roundtrip(t, fixtureManifest(),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if resp.Error != nil {
		t.Fatalf("initialize errored: %+v", resp.Error)
	}
	res := resp.Result.(map[string]any)
	if res["protocolVersion"] != mcpProtocolVersion {
		t.Errorf("protocolVersion = %v, want %v", res["protocolVersion"], mcpProtocolVersion)
	}
	caps := res["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Error("initialize must advertise tools capability")
	}
	info := res["serverInfo"].(map[string]any)
	if info["name"] != serverName {
		t.Errorf("serverInfo.name = %v, want %v", info["name"], serverName)
	}
}

func TestToolsList_StripsForgeMetadataFromWireResponse(t *testing.T) {
	resp := roundtrip(t, fixtureManifest(),
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if resp.Error != nil {
		t.Fatalf("tools/list errored: %+v", resp.Error)
	}
	tools := resp.Result.(map[string]any)["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools count = %d, want 2", len(tools))
	}
	first := tools[0].(map[string]any)
	// MCP-spec fields must be present.
	for _, key := range []string{"name", "description", "inputSchema"} {
		if _, ok := first[key]; !ok {
			t.Errorf("tools[0] missing required MCP field %q", key)
		}
	}
	if _, ok := first["outputSchema"]; !ok {
		t.Errorf("tools[0] missing outputSchema (set in fixture)")
	}
	// Forge-specific snake_case metadata MUST be stripped from the
	// wire response — strict MCP clients (some implementations are
	// less forgiving than the inspector) reject unknown top-level
	// fields. The forge-aware tooling can read the manifest file
	// directly if it wants the routing metadata.
	for _, forbidden := range []string{"service", "method", "procedure", "auth_required", "idempotency_key"} {
		if _, ok := first[forbidden]; ok {
			t.Errorf("tools/list wire response must NOT include forge metadata %q (clients ignore unknowns, but strict ones may reject; ship clean)", forbidden)
		}
	}
	// Second tool has no outputSchema in the fixture — confirm the
	// emitter doesn't add an empty one.
	second := tools[1].(map[string]any)
	if _, ok := second["outputSchema"]; ok {
		t.Error("tools[1] (no fixture outputSchema) must NOT have outputSchema on the wire")
	}
}

func TestToolsCall_RealDispatch_SuccessRoundTrip(t *testing.T) {
	// The bridge MUST actually POST the arguments to the Connect
	// backend and return the backend's response as structuredContent.
	// This is the test that proves it's no longer a stub: if the
	// bridge synthesized a zero-value placeholder, we'd see "" / 0 /
	// false fields instead of the live values the backend returned.
	resp, cap := roundtripWithBackend(t, fixtureManifest(), "",
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"item":{"id":"task-42","name":"buy milk","done":false}}`))
		},
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"tasks_service__create","arguments":{"name":"buy milk"}}}`)
	if resp.Error != nil {
		t.Fatalf("tools/call errored: %+v", resp.Error)
	}

	// On-wire proof: the backend saw a POST to the procedure path with
	// the right body and Connect-Protocol-Version header.
	if len(cap.calls) != 1 {
		t.Fatalf("backend call count = %d, want 1", len(cap.calls))
	}
	call := cap.calls[0]
	if call.method != "POST" {
		t.Errorf("backend method = %s, want POST", call.method)
	}
	if call.path != "/services.tasks.v1.TasksService/Create" {
		t.Errorf("backend path = %s, want /services.tasks.v1.TasksService/Create", call.path)
	}
	if got := call.headers.Get("Connect-Protocol-Version"); got != "1" {
		t.Errorf("Connect-Protocol-Version header = %q, want 1", got)
	}
	if call.body["name"] != "buy milk" {
		t.Errorf("backend body.name = %v, want 'buy milk'", call.body["name"])
	}

	// MCP-visible proof: the backend's actual response landed in
	// structuredContent, not a synthesized placeholder.
	res := resp.Result.(map[string]any)
	if _, ok := res["isError"]; ok {
		t.Errorf("success path must not set isError: %v", res)
	}
	sc := res["structuredContent"].(map[string]any)
	item, ok := sc["item"].(map[string]any)
	if !ok {
		t.Fatalf("structuredContent.item missing or wrong type: %v", sc)
	}
	if item["id"] != "task-42" {
		t.Errorf("structuredContent.item.id = %v, want 'task-42' (live backend value, not zero-fill)", item["id"])
	}
	if item["name"] != "buy milk" {
		t.Errorf("structuredContent.item.name = %v, want 'buy milk'", item["name"])
	}
}

func TestToolsCall_ConnectApplicationError_BecomesIsErrorResult(t *testing.T) {
	// MCP convention: tool-level failures (the call reached the backend
	// and the backend said no) are part of the *result* with
	// isError=true, NOT a JSON-RPC error. JSON-RPC errors are reserved
	// for "couldn't reach the tool at all."
	//
	// IMPORTANT: the error path MUST NOT emit structuredContent. The
	// tool's outputSchema describes the success shape; an {error: ...}
	// object would fail strict schema validation in clients like the
	// official MCP Inspector (caught during e2e validation against a
	// real Connect backend — the inspector rejected the response
	// with "data must have required property 'item'"). The Connect
	// error code + message land in content text; agents read them
	// from there.
	resp, _ := roundtripWithBackend(t, fixtureManifest(), "",
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":"invalid_argument","message":"name is required"}`))
		},
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"tasks_service__create","arguments":{}}}`)
	if resp.Error != nil {
		t.Fatalf("Connect app error must NOT surface as a JSON-RPC error (got %+v); it must be a result with isError=true", resp.Error)
	}
	res := resp.Result.(map[string]any)
	if res["isError"] != true {
		t.Errorf("expected isError=true on Connect application error, got %v", res)
	}
	// Strict-mode clients reject structuredContent that doesn't match
	// outputSchema. The error path must omit structuredContent entirely.
	if _, ok := res["structuredContent"]; ok {
		t.Errorf("error path must NOT emit structuredContent (outputSchema mismatch), got %v", res["structuredContent"])
	}
	// Connect code+message must still be retrievable from the text
	// content block so agents can typed-parse them.
	contents := res["content"].([]any)
	first := contents[0].(map[string]any)
	text := first["text"].(string)
	if !strings.Contains(text, "invalid_argument") {
		t.Errorf("content text must include the Connect error code, got: %s", text)
	}
	if !strings.Contains(text, "name is required") {
		t.Errorf("content text must include the Connect error message, got: %s", text)
	}
}

func TestToolsCall_PropagatesAuthHeader(t *testing.T) {
	// --auth must reach the backend on every dispatched call.
	// Without this, projects with real auth can't use the bridge at
	// all — silent or absent auth would mean every call 401s.
	const want = "Bearer eyJabc.def.ghi"
	_, cap := roundtripWithBackend(t, fixtureManifest(), want,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"item":{}}`))
		},
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"tasks_service__create","arguments":{"name":"x"}}}`)
	if len(cap.calls) != 1 {
		t.Fatalf("backend call count = %d", len(cap.calls))
	}
	if got := cap.calls[0].headers.Get("Authorization"); got != want {
		t.Errorf("backend Authorization = %q, want %q", got, want)
	}
}

func TestToolsCall_NoStructuredContent_WhenOutputSchemaAbsent(t *testing.T) {
	// A tool without an outputSchema must NOT emit structuredContent —
	// the field would imply a schema-conformance claim the bridge
	// can't back up. The content text block carries the raw response
	// so agents still see what the backend said.
	resp, _ := roundtripWithBackend(t, fixtureManifest(), "",
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		},
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"tasks_service__ping","arguments":{}}}`)
	if resp.Error != nil {
		t.Fatalf("tools/call errored: %+v", resp.Error)
	}
	res := resp.Result.(map[string]any)
	if _, ok := res["structuredContent"]; ok {
		t.Error("tools/call on a tool WITHOUT outputSchema must NOT include structuredContent")
	}
	if _, ok := res["content"]; !ok {
		t.Error("tools/call must still return content even without outputSchema")
	}
}

func TestToolsCall_RefusesStreamingRPC(t *testing.T) {
	// MCP tools are unary. Proxying streaming RPCs is out of scope
	// for the bridge — explicit refusal beats half-working dispatch.
	m := fixtureManifest()
	m.Tools = append(m.Tools, tool{
		Name:        "tasks_service__tail",
		InputSchema: map[string]any{"type": "object"},
		Service:     "TasksService", Method: "Tail",
		Procedure: "/services.tasks.v1.TasksService/Tail",
		Streaming: "server",
	})
	resp, cap := roundtripWithBackend(t, m, "",
		func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("backend must not be called for streaming RPC; got %s %s", r.Method, r.URL.Path)
		},
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"tasks_service__tail","arguments":{}}}`)
	if resp.Error == nil {
		t.Fatal("streaming RPC must error")
	}
	if !strings.Contains(resp.Error.Message, "streaming") {
		t.Errorf("error must explain streaming refusal, got: %s", resp.Error.Message)
	}
	if len(cap.calls) != 0 {
		t.Errorf("streaming refusal must short-circuit BEFORE the HTTP dispatch; backend saw %d calls", len(cap.calls))
	}
}

func TestToolsCall_NoAddr_ReturnsConfigurationError(t *testing.T) {
	// If forge-mcp is launched without --addr, tools/call cannot
	// possibly succeed. A clear configuration error tells the operator
	// what to fix instead of a confusing "connection refused" on
	// every call.
	resp := roundtrip(t, fixtureManifest(),
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"tasks_service__create","arguments":{"name":"x"}}}`)
	if resp.Error == nil {
		t.Fatal("no-addr config must produce an error")
	}
	if !strings.Contains(resp.Error.Message, "--addr") {
		t.Errorf("error must name --addr so the operator knows how to fix it, got: %s", resp.Error.Message)
	}
}

func TestToolsCall_UnknownTool(t *testing.T) {
	resp := roundtrip(t, fixtureManifest(),
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"does_not_exist","arguments":{}}}`)
	if resp.Error == nil {
		t.Fatal("unknown tool should produce an error")
	}
	if resp.Error.Code != -32602 {
		t.Errorf("error code = %d, want -32602 (invalid params)", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "does_not_exist") {
		t.Errorf("error message should name the offending tool, got: %s", resp.Error.Message)
	}
}

func TestUnknownMethod(t *testing.T) {
	resp := roundtrip(t, fixtureManifest(),
		`{"jsonrpc":"2.0","id":6,"method":"nope/please"}`)
	if resp.Error == nil {
		t.Fatal("unknown method should produce an error")
	}
	if resp.Error.Code != -32601 {
		t.Errorf("error code = %d, want -32601 (method not found)", resp.Error.Code)
	}
}

func TestZeroValueFromSchema(t *testing.T) {
	cases := []struct {
		name   string
		schema map[string]any
		want   any
	}{
		{"string", map[string]any{"type": "string"}, ""},
		{"number", map[string]any{"type": "number"}, 0},
		{"integer", map[string]any{"type": "integer"}, 0},
		{"boolean", map[string]any{"type": "boolean"}, false},
		{"array", map[string]any{"type": "array"}, []any{}},
		{"empty-object", map[string]any{"type": "object"}, map[string]any{}},
		// Object with required fields zeroes each required field per
		// its declared type. Optional fields are omitted — the spec
		// only requires that `required` constraints are satisfied.
		{
			"object-with-required",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":     map[string]any{"type": "string"},
					"count":  map[string]any{"type": "number"},
					"active": map[string]any{"type": "boolean"},
					"omit":   map[string]any{"type": "string"}, // not required → omitted
				},
				"required": []any{"id", "count", "active"},
			},
			map[string]any{
				"id":     "",
				"count":  0,
				"active": false,
			},
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := zeroValueFromSchema(c.schema)
			// Compare via JSON round-trip so map ordering doesn't
			// matter and so we don't have to write a full deep-equal.
			wantJSON, _ := json.Marshal(c.want)
			gotJSON, _ := json.Marshal(got)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("got %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

func TestPing(t *testing.T) {
	// Per MCP spec, ping responds with an empty result. Used by
	// clients to detect a dead server.
	resp := roundtrip(t, fixtureManifest(),
		`{"jsonrpc":"2.0","id":7,"method":"ping"}`)
	if resp.Error != nil {
		t.Fatalf("ping errored: %+v", resp.Error)
	}
	res, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("ping result must be a map, got %T", resp.Result)
	}
	if len(res) != 0 {
		t.Errorf("ping result must be empty, got %v", res)
	}
}

func TestNotification_NoResponse(t *testing.T) {
	// JSON-RPC 2.0: notifications (no id) get no response. MCP uses
	// this for `notifications/initialized`.
	s := &server{manifest: fixtureManifest()}
	var out bytes.Buffer
	if err := s.run(
		strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"),
		&out,
	); err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("notifications must not produce a response, got: %s", out.String())
	}
}

// TestToolsList_ExcludesStreamingTools: streaming RPCs cannot be called
// through the bridge (MCP tools are unary), so tools/list must not
// advertise them — an agent that sees a tool in the list must be able
// to call it. The streaming entries stay in the manifest (with their
// "streaming" marker) for non-MCP consumers; initialize's instructions
// count must match what tools/list actually returns.
func TestToolsList_ExcludesStreamingTools(t *testing.T) {
	m := fixtureManifest()
	m.Tools = append(m.Tools,
		tool{
			Name:        "tasks_service__tail",
			InputSchema: map[string]any{"type": "object"},
			Service:     "TasksService", Method: "Tail",
			Procedure: "/services.tasks.v1.TasksService/Tail",
			Streaming: "server",
		},
		tool{
			Name:        "tasks_service__sync",
			InputSchema: map[string]any{"type": "object"},
			Service:     "TasksService", Method: "Sync",
			Procedure: "/services.tasks.v1.TasksService/Sync",
			Streaming: "bidi",
		},
	)

	resp := roundtrip(t, m, `{"jsonrpc":"2.0","id":11,"method":"tools/list"}`)
	if resp.Error != nil {
		t.Fatalf("tools/list errored: %+v", resp.Error)
	}
	listed := resp.Result.(map[string]any)["tools"].([]any)
	if len(listed) != 2 {
		t.Fatalf("tools/list returned %d tools, want 2 (the unary ones only)", len(listed))
	}
	for _, raw := range listed {
		entry := raw.(map[string]any)
		name := entry["name"].(string)
		if name == "tasks_service__tail" || name == "tasks_service__sync" {
			t.Errorf("streaming tool %q must not be advertised in tools/list", name)
		}
	}

	// initialize's instructions count the callable tools, not the
	// manifest total — agents trust that number.
	s := &server{manifest: m}
	if got := s.callableToolCount(); got != 2 {
		t.Errorf("callableToolCount = %d, want 2", got)
	}
}

// TestToolsList_PassesDefsAndRefsThrough: manifest schema_version 1.1
// emits deep schemas with top-level "$defs" and "$ref" at field sites.
// The bridge forwards them verbatim — the MCP Tool.inputSchema
// meta-schema is an open object (only type/properties/required are
// pinned), so $defs is spec-valid, and inlining at serve time would
// reintroduce infinite expansion for recursive messages.
func TestToolsList_PassesDefsAndRefsThrough(t *testing.T) {
	m := fixtureManifest()
	m.Tools[0].InputSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"node": map[string]any{"$ref": "#/$defs/demo.v1.Node"},
		},
		"required": []any{"node"},
		"$defs": map[string]any{
			"demo.v1.Node": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"label": map[string]any{"type": "string"},
					// Self-referential — exactly the shape that must
					// survive pass-through untouched.
					"children": map[string]any{
						"type":  "array",
						"items": map[string]any{"$ref": "#/$defs/demo.v1.Node"},
					},
				},
			},
		},
	}

	resp := roundtrip(t, m, `{"jsonrpc":"2.0","id":12,"method":"tools/list"}`)
	if resp.Error != nil {
		t.Fatalf("tools/list errored: %+v", resp.Error)
	}
	listed := resp.Result.(map[string]any)["tools"].([]any)
	got := listed[0].(map[string]any)["inputSchema"].(map[string]any)

	defs, ok := got["$defs"].(map[string]any)
	if !ok {
		t.Fatalf("$defs must pass through tools/list verbatim, got: %#v", got)
	}
	node, ok := defs["demo.v1.Node"].(map[string]any)
	if !ok {
		t.Fatalf("$defs entry demo.v1.Node missing: %#v", defs)
	}
	children := node["properties"].(map[string]any)["children"].(map[string]any)
	ref := children["items"].(map[string]any)["$ref"]
	if ref != "#/$defs/demo.v1.Node" {
		t.Errorf("recursive $ref must survive pass-through, got %v", ref)
	}
	nodeProp := got["properties"].(map[string]any)["node"].(map[string]any)
	if nodeProp["$ref"] != "#/$defs/demo.v1.Node" {
		t.Errorf("field-site $ref must survive pass-through, got %v", nodeProp["$ref"])
	}
}
