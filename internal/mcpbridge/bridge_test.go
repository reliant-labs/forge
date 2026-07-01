// Tests for the mcpbridge stdio server. The shape we exercise:
//
//   - JSON-RPC framing round-trip (one request line in, one response out).
//   - initialize returns protocolVersion + capabilities + serverInfo.
//   - tools/list returns the manifest's tools with only the MCP-spec
//     fields (forge snake_case metadata stripped from the wire).
//   - tools/list excludes streaming RPCs (MCP tools are unary).
//   - tools/list forwards $defs / $ref verbatim.
//   - tools/call ACTUALLY DISPATCHES to a Connect-shaped HTTP backend
//     (httptest) and returns the decoded response as structuredContent.
//   - tools/call surfaces Connect application errors as isError=true
//     results, not JSON-RPC errors, and omits structuredContent there.
//   - tools/call propagates the Authorization header.
//   - tools/call refuses streaming RPCs and missing-addr config.
//   - Unknown tool → -32602; unknown method → -32601.
package mcpbridge

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fixtureManifest is the in-memory equivalent of gen/mcp/manifest.json for
// a small project. Two tools — one with an outputSchema (Create), one
// without (Ping) — so we exercise both the structuredContent branch and
// the content-only branch.
func fixtureManifest() *Manifest {
	return &Manifest{
		Generated:     "forge",
		SchemaVersion: "1.1",
		Project:       "demo",
		Tools: []Tool{
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
					"type":       "object",
					"properties": map[string]any{"item": map[string]any{"type": "object"}},
					"required":   []any{"item"},
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
				Service:     "TasksService",
				Method:      "Ping",
				Procedure:   "/services.tasks.v1.TasksService/Ping",
			},
		},
	}
}

func roundtrip(t *testing.T, m *Manifest, raw string) rpcResponse {
	t.Helper()
	return roundtripFull(t, m, "", "", raw)
}

type backendCapture struct{ calls []backendCall }

type backendCall struct {
	method  string
	path    string
	headers http.Header
	body    map[string]any
}

func roundtripWithBackend(t *testing.T, m *Manifest, authHeader string, handler http.HandlerFunc, raw string) (rpcResponse, *backendCapture) {
	t.Helper()
	cap := &backendCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readJSON(r)
		cap.calls = append(cap.calls, backendCall{
			method: r.Method, path: r.URL.Path, headers: r.Header.Clone(), body: body,
		})
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return roundtripFull(t, m, srv.URL, authHeader, raw), cap
}

func roundtripFull(t *testing.T, m *Manifest, addr, authHeader, raw string) rpcResponse {
	t.Helper()
	s := &Server{
		Manifest:   m,
		Addr:       addr,
		AuthHeader: authHeader,
		HTTP:       &http.Client{Timeout: 5 * time.Second},
		Logf:       func(string, ...any) {}, // silence test noise
	}
	var out bytes.Buffer
	if err := s.Run(strings.NewReader(raw+"\n"), &out); err != nil {
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
	if res["protocolVersion"] != ProtocolVersion {
		t.Errorf("protocolVersion = %v, want %v", res["protocolVersion"], ProtocolVersion)
	}
	caps := res["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Error("initialize must advertise tools capability")
	}
	if info := res["serverInfo"].(map[string]any); info["name"] != ServerName {
		t.Errorf("serverInfo.name = %v, want %v", info["name"], ServerName)
	}
}

func TestToolsList_StripsForgeMetadataFromWireResponse(t *testing.T) {
	resp := roundtrip(t, fixtureManifest(), `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if resp.Error != nil {
		t.Fatalf("tools/list errored: %+v", resp.Error)
	}
	tools := resp.Result.(map[string]any)["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools count = %d, want 2", len(tools))
	}
	first := tools[0].(map[string]any)
	for _, key := range []string{"name", "description", "inputSchema"} {
		if _, ok := first[key]; !ok {
			t.Errorf("tools[0] missing required MCP field %q", key)
		}
	}
	if _, ok := first["outputSchema"]; !ok {
		t.Errorf("tools[0] missing outputSchema (set in fixture)")
	}
	for _, forbidden := range []string{"service", "method", "procedure", "auth_required", "idempotency_key"} {
		if _, ok := first[forbidden]; ok {
			t.Errorf("tools/list wire response must NOT include forge metadata %q", forbidden)
		}
	}
	second := tools[1].(map[string]any)
	if _, ok := second["outputSchema"]; ok {
		t.Error("tools[1] (no fixture outputSchema) must NOT have outputSchema on the wire")
	}
}

func TestToolsCall_RealDispatch_SuccessRoundTrip(t *testing.T) {
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
	if len(cap.calls) != 1 {
		t.Fatalf("backend call count = %d, want 1", len(cap.calls))
	}
	call := cap.calls[0]
	if call.method != "POST" {
		t.Errorf("backend method = %s, want POST", call.method)
	}
	if call.path != "/services.tasks.v1.TasksService/Create" {
		t.Errorf("backend path = %s", call.path)
	}
	if got := call.headers.Get("Connect-Protocol-Version"); got != "1" {
		t.Errorf("Connect-Protocol-Version = %q, want 1", got)
	}
	if call.body["name"] != "buy milk" {
		t.Errorf("backend body.name = %v, want 'buy milk'", call.body["name"])
	}
	res := resp.Result.(map[string]any)
	if _, ok := res["isError"]; ok {
		t.Errorf("success path must not set isError: %v", res)
	}
	sc := res["structuredContent"].(map[string]any)
	item, ok := sc["item"].(map[string]any)
	if !ok {
		t.Fatalf("structuredContent.item missing: %v", sc)
	}
	if item["id"] != "task-42" {
		t.Errorf("structuredContent.item.id = %v, want 'task-42' (live backend value)", item["id"])
	}
}

func TestToolsCall_ConnectApplicationError_BecomesIsErrorResult(t *testing.T) {
	resp, _ := roundtripWithBackend(t, fixtureManifest(), "",
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":"invalid_argument","message":"name is required"}`))
		},
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"tasks_service__create","arguments":{}}}`)
	if resp.Error != nil {
		t.Fatalf("Connect app error must NOT be a JSON-RPC error (got %+v)", resp.Error)
	}
	res := resp.Result.(map[string]any)
	if res["isError"] != true {
		t.Errorf("expected isError=true, got %v", res)
	}
	if _, ok := res["structuredContent"]; ok {
		t.Errorf("error path must NOT emit structuredContent, got %v", res["structuredContent"])
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "invalid_argument") || !strings.Contains(text, "name is required") {
		t.Errorf("content text must carry the Connect code+message, got: %s", text)
	}
}

// TestToolsCall_Unauthenticated_BecomesIsErrorResult proves the auth story:
// a forge dev server NOT in AUTH_DEV_MODE runs the auth interceptor, so a
// tokenless call returns a Connect "unauthenticated" error. That clean
// error IS the proof the transport reached the backend.
func TestToolsCall_Unauthenticated_BecomesIsErrorResult(t *testing.T) {
	resp, cap := roundtripWithBackend(t, fixtureManifest(), "",
		func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"code":"unauthenticated","message":"missing bearer token"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"item":{"id":"ok"}}`))
		},
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"tasks_service__create","arguments":{"name":"x"}}}`)
	if len(cap.calls) != 1 {
		t.Fatalf("backend must be reached even without a token; calls=%d", len(cap.calls))
	}
	res := resp.Result.(map[string]any)
	if res["isError"] != true {
		t.Fatalf("tokenless call must surface isError=true, got %v", res)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "unauthenticated") {
		t.Errorf("content must name the unauthenticated code, got: %s", text)
	}
}

func TestToolsCall_PropagatesAuthHeader(t *testing.T) {
	const want = "Bearer eyJabc.def.ghi"
	_, cap := roundtripWithBackend(t, fixtureManifest(), want,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"item":{}}`))
		},
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"tasks_service__create","arguments":{"name":"x"}}}`)
	if len(cap.calls) != 1 {
		t.Fatalf("backend call count = %d", len(cap.calls))
	}
	if got := cap.calls[0].headers.Get("Authorization"); got != want {
		t.Errorf("backend Authorization = %q, want %q", got, want)
	}
}

func TestToolsCall_NoStructuredContent_WhenOutputSchemaAbsent(t *testing.T) {
	resp, _ := roundtripWithBackend(t, fixtureManifest(), "",
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		},
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"tasks_service__ping","arguments":{}}}`)
	if resp.Error != nil {
		t.Fatalf("tools/call errored: %+v", resp.Error)
	}
	res := resp.Result.(map[string]any)
	if _, ok := res["structuredContent"]; ok {
		t.Error("tool WITHOUT outputSchema must NOT include structuredContent")
	}
	if _, ok := res["content"]; !ok {
		t.Error("tools/call must still return content")
	}
}

func TestToolsCall_RefusesStreamingRPC(t *testing.T) {
	m := fixtureManifest()
	m.Tools = append(m.Tools, Tool{
		Name: "tasks_service__tail", InputSchema: map[string]any{"type": "object"},
		Service: "TasksService", Method: "Tail",
		Procedure: "/services.tasks.v1.TasksService/Tail", Streaming: "server",
	})
	resp, cap := roundtripWithBackend(t, m, "",
		func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("backend must not be called for streaming RPC")
		},
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"tasks_service__tail","arguments":{}}}`)
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "streaming") {
		t.Fatalf("streaming RPC must error with a streaming explanation, got %+v", resp.Error)
	}
	if len(cap.calls) != 0 {
		t.Errorf("streaming refusal must short-circuit before dispatch; backend saw %d calls", len(cap.calls))
	}
}

func TestToolsCall_NoAddr_ReturnsConfigurationError(t *testing.T) {
	resp := roundtrip(t, fixtureManifest(),
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"tasks_service__create","arguments":{"name":"x"}}}`)
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "--addr") {
		t.Fatalf("no-addr config must name --addr, got %+v", resp.Error)
	}
}

func TestToolsCall_UnknownTool(t *testing.T) {
	resp := roundtrip(t, fixtureManifest(),
		`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"nope","arguments":{}}}`)
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("unknown tool must be -32602, got %+v", resp.Error)
	}
}

func TestUnknownMethod(t *testing.T) {
	resp := roundtrip(t, fixtureManifest(), `{"jsonrpc":"2.0","id":11,"method":"nope/please"}`)
	if resp.Error == nil || resp.Error.Code != -32601 {
		t.Fatalf("unknown method must be -32601, got %+v", resp.Error)
	}
}

func TestPing(t *testing.T) {
	resp := roundtrip(t, fixtureManifest(), `{"jsonrpc":"2.0","id":12,"method":"ping"}`)
	if resp.Error != nil {
		t.Fatalf("ping errored: %+v", resp.Error)
	}
	if res := resp.Result.(map[string]any); len(res) != 0 {
		t.Errorf("ping result must be empty, got %v", res)
	}
}

func TestNotification_NoResponse(t *testing.T) {
	s := &Server{Manifest: fixtureManifest(), Logf: func(string, ...any) {}}
	var out bytes.Buffer
	if err := s.Run(strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"), &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("notifications must not produce a response, got: %s", out.String())
	}
}

func TestToolsList_ExcludesStreamingTools(t *testing.T) {
	m := fixtureManifest()
	m.Tools = append(m.Tools,
		Tool{Name: "tasks_service__tail", InputSchema: map[string]any{"type": "object"}, Streaming: "server"},
		Tool{Name: "tasks_service__sync", InputSchema: map[string]any{"type": "object"}, Streaming: "bidi"},
	)
	resp := roundtrip(t, m, `{"jsonrpc":"2.0","id":13,"method":"tools/list"}`)
	if resp.Error != nil {
		t.Fatalf("tools/list errored: %+v", resp.Error)
	}
	listed := resp.Result.(map[string]any)["tools"].([]any)
	if len(listed) != 2 {
		t.Fatalf("tools/list returned %d, want 2 (unary only)", len(listed))
	}
	s := &Server{Manifest: m}
	if got := s.CallableToolCount(); got != 2 {
		t.Errorf("CallableToolCount = %d, want 2", got)
	}
}

func TestToolsList_PassesDefsAndRefsThrough(t *testing.T) {
	m := fixtureManifest()
	m.Tools[0].InputSchema = map[string]any{
		"type":       "object",
		"properties": map[string]any{"node": map[string]any{"$ref": "#/$defs/demo.v1.Node"}},
		"required":   []any{"node"},
		"$defs": map[string]any{
			"demo.v1.Node": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"label":    map[string]any{"type": "string"},
					"children": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/demo.v1.Node"}},
				},
			},
		},
	}
	resp := roundtrip(t, m, `{"jsonrpc":"2.0","id":14,"method":"tools/list"}`)
	if resp.Error != nil {
		t.Fatalf("tools/list errored: %+v", resp.Error)
	}
	got := resp.Result.(map[string]any)["tools"].([]any)[0].(map[string]any)["inputSchema"].(map[string]any)
	defs, ok := got["$defs"].(map[string]any)
	if !ok {
		t.Fatalf("$defs must pass through verbatim, got: %#v", got)
	}
	node := defs["demo.v1.Node"].(map[string]any)
	ref := node["properties"].(map[string]any)["children"].(map[string]any)["items"].(map[string]any)["$ref"]
	if ref != "#/$defs/demo.v1.Node" {
		t.Errorf("recursive $ref must survive, got %v", ref)
	}
}
