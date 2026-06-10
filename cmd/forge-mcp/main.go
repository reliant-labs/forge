// Command forge-mcp is a Model Context Protocol stdio server that
// reads gen/mcp/manifest.json and exposes every RPC tool the manifest
// declares to an MCP client (Claude Desktop, the official MCP
// Inspector, Cline, etc.).
//
// What it does:
//
//   - Reads gen/mcp/manifest.json from the project root (or from
//     --manifest <path>).
//   - Speaks MCP over stdio (JSON-RPC 2.0 framed with one message per
//     line — same shape Claude Desktop expects).
//   - Implements initialize, tools/list, tools/call, ping.
//   - tools/call ACTUALLY dispatches the call to the running Connect
//     service over HTTP+JSON, using the procedure path from the
//     manifest. The arguments map becomes the JSON request body; the
//     Connect response becomes structuredContent and a human-readable
//     content block. Connect error envelopes are translated into
//     MCP's isError:true result shape so agents see typed failures.
//
// Why HTTP+JSON: Connect's native wire format lets a generic client
// call any procedure without compiling against the project's proto
// types — the bridge stays one binary for every forge project. Streaming
// RPCs (server/client/bidi) are NOT supported by this bridge because
// MCP tool calls are unary; the bridge returns an explicit error for
// any tool whose manifest entry carries a streaming marker.
//
// Why a separate binary: the forge CLI already does a lot; an MCP
// server has its own lifecycle (started by Claude Desktop, killed on
// disconnect, must keep stdio clean), so it gets its own entry point.
// Projects that scaffold a forge layout can add this binary to their
// Claude Desktop config and instantly see every RPC as an MCP tool.
//
// Usage:
//
//	forge-mcp --addr http://localhost:8080                 # call against a running Connect server
//	forge-mcp --addr http://... --auth "Bearer xxx"        # propagate an Authorization header
//	forge-mcp --addr http://... --auth-env FORGE_MCP_AUTH  # ... or read it from env
//	forge-mcp --addr http://... --manifest <path>          # explicit manifest
//	forge-mcp --addr http://... --project <dir>            # resolves <dir>/gen/mcp/manifest.json
//
// The server prints debug info to stderr (which Claude Desktop and the
// Inspector capture as server logs); stdout is reserved for the MCP
// protocol.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	mcpProtocolVersion = "2024-11-05"
	serverName         = "forge-mcp"
	serverVersion      = "0.1.0"
)

func main() {
	var (
		manifestPath string
		projectDir   string
		addr         string
		authHeader   string
		authEnv      string
		timeoutSec   int
	)
	flag.StringVar(&manifestPath, "manifest", "", "Explicit path to gen/mcp/manifest.json (overrides --project)")
	flag.StringVar(&projectDir, "project", ".", "Project root; manifest is resolved at <project>/gen/mcp/manifest.json")
	flag.StringVar(&addr, "addr", "", "Connect server base URL (e.g. http://localhost:8080). REQUIRED — without it, tools/call errors.")
	flag.StringVar(&authHeader, "auth", "", "Verbatim Authorization header value (e.g. 'Bearer eyJ...'). Mutually exclusive with --auth-env.")
	flag.StringVar(&authEnv, "auth-env", "", "Env var name to read the Authorization header from at startup (e.g. FORGE_MCP_AUTH).")
	flag.IntVar(&timeoutSec, "timeout", 30, "Per-RPC HTTP timeout in seconds")
	flag.Parse()

	// stderr is the only safe channel for diagnostics — stdout is
	// reserved for the MCP protocol framing.
	log.SetOutput(os.Stderr)
	log.SetFlags(0)
	log.SetPrefix("[forge-mcp] ")

	if manifestPath == "" {
		manifestPath = filepath.Join(projectDir, "gen", "mcp", "manifest.json")
	}

	// Auth-header resolution: --auth wins over --auth-env if both are
	// passed, but we warn so the operator notices the conflict — silent
	// preference is exactly the kind of foot-gun forge's loud-by-default
	// pass closes everywhere else.
	if authEnv != "" {
		v := os.Getenv(authEnv)
		if authHeader != "" {
			log.Printf("warning: --auth and --auth-env both set; using --auth verbatim, ignoring $%s", authEnv)
		} else {
			authHeader = v
		}
	}

	manifest, err := loadManifest(manifestPath)
	if err != nil {
		log.Fatalf("load manifest %s: %v", manifestPath, err)
	}
	log.Printf("loaded manifest: project=%s tools=%d addr=%s auth=%v",
		manifest.Project, len(manifest.Tools), addr, authHeader != "")

	srv := &server{
		manifest:     manifest,
		manifestPath: manifestPath,
		addr:         strings.TrimRight(addr, "/"),
		authHeader:   authHeader,
		http:         &http.Client{Timeout: time.Duration(timeoutSec) * time.Second},
	}
	if err := srv.run(os.Stdin, os.Stdout); err != nil && err != io.EOF {
		log.Fatalf("server: %v", err)
	}
}

// manifest mirrors the gen/mcp/manifest.json shape forge writes. We
// don't import the forge codegen package because forge-mcp is meant to
// be vendor-able into projects that pin a different forge version than
// what they're running against — keeping the read shape local insulates
// the bridge from forge codegen churn.
type manifest struct {
	Generated     string `json:"_generated"`
	SchemaVersion string `json:"schema_version"`
	Project       string `json:"project"`
	Tools         []tool `json:"tools"`
}

// tool carries every field the MCP spec needs for tools/list plus the
// forge-specific routing metadata (service / method / procedure /
// auth_required / idempotency_key / streaming). MCP clients ignore the
// snake_case extras per the protocol's permissive policy.
type tool struct {
	Name           string         `json:"name"`
	Description    string         `json:"description,omitempty"`
	InputSchema    map[string]any `json:"inputSchema"`
	OutputSchema   map[string]any `json:"outputSchema,omitempty"`
	Service        string         `json:"service,omitempty"`
	Method         string         `json:"method,omitempty"`
	Procedure      string         `json:"procedure,omitempty"`
	AuthRequired   bool           `json:"auth_required,omitempty"`
	IdempotencyKey bool           `json:"idempotency_key,omitempty"`
	Streaming      string         `json:"streaming,omitempty"`
}

func loadManifest(path string) (*manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

type server struct {
	manifest     *manifest
	manifestPath string
	// addr is the Connect base URL (e.g. http://localhost:8080).
	// Empty → tools/call returns a clear configuration error rather
	// than silently failing to dial. Stripped of trailing slash at
	// construction so URL concatenation with the procedure path
	// always produces a clean URL.
	addr string
	// authHeader, when non-empty, is set as the Authorization header
	// on every dispatched RPC. The bridge does NOT inspect or refresh
	// the token — it's the operator's job to pass a current value via
	// --auth or --auth-env.
	authHeader string
	// http is the shared client used for all Connect RPC dispatches.
	// Timeout is set at construction and applies per call (one
	// timeout per tools/call invocation, not cumulative).
	http *http.Client
}

// run is the JSON-RPC over stdio main loop. Each line is one
// JSON-RPC 2.0 request; responses are framed the same way. Loop exits
// on stdin EOF, which is how MCP clients signal disconnect.
func (s *server) run(in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	// MCP messages can be large (tools/list with many tools, nested
	// schemas). 16 MiB is well above any realistic request size and
	// well below stdio buffer limits.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	enc := json.NewEncoder(out)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			log.Printf("parse error: %v (raw: %q)", err, line)
			// JSON-RPC says we cannot reply to an unparseable
			// request with a correlatable id, but we can return a
			// parse-error response with null id per spec.
			_ = enc.Encode(rpcError(nil, -32700, "parse error: "+err.Error()))
			continue
		}
		log.Printf("→ method=%s id=%v", req.Method, req.ID)
		resp := s.dispatch(&req)
		if resp == nil {
			// Notifications (no id) get no response per JSON-RPC 2.0.
			continue
		}
		if err := enc.Encode(resp); err != nil {
			return fmt.Errorf("write response: %w", err)
		}
	}
	return scanner.Err()
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func rpcError(id json.RawMessage, code int, msg string) *rpcResponse {
	if id == nil {
		id = json.RawMessage(`null`)
	}
	return &rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcErr{Code: code, Message: msg},
	}
}

func rpcResult(id json.RawMessage, result any) *rpcResponse {
	return &rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
}

// dispatch returns nil for notifications (no id), which the loop
// interprets as "don't write a response."
func (s *server) dispatch(req *rpcRequest) *rpcResponse {
	switch req.Method {
	case "initialize":
		return rpcResult(req.ID, map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities": map[string]any{
				// We expose tools and nothing else. Resources, prompts,
				// sampling, etc. are out of scope for the bridge.
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    serverName,
				"version": serverVersion,
			},
			"instructions": fmt.Sprintf(
				"Bridge for forge project %q. %d Connect RPC(s) exposed as tools. Call tools/list to enumerate.",
				s.manifest.Project, len(s.manifest.Tools)),
		})

	case "notifications/initialized":
		// Notification → no response.
		return nil

	case "ping":
		// Per MCP spec, ping responds with an empty result. Used by
		// clients to detect a dead server.
		return rpcResult(req.ID, map[string]any{})

	case "tools/list":
		// Return the manifest's tools verbatim — they're already in MCP
		// shape. We strip the forge-specific snake_case metadata for
		// strict-mode clients that reject unknown fields; spec-compliant
		// clients are forgiving but a few aren't.
		stripped := make([]map[string]any, 0, len(s.manifest.Tools))
		for _, t := range s.manifest.Tools {
			entry := map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": t.InputSchema,
			}
			if t.OutputSchema != nil {
				entry["outputSchema"] = t.OutputSchema
			}
			stripped = append(stripped, entry)
		}
		return rpcResult(req.ID, map[string]any{
			"tools": stripped,
		})

	case "tools/call":
		return s.callTool(req)

	default:
		return rpcError(req.ID, -32601, "method not found: "+req.Method)
	}
}

// callTool implements MCP tools/call by dispatching the named tool to
// the running Connect service over HTTP+JSON. The bridge knows nothing
// about the project's proto types — Connect's JSON wire format makes
// the dispatch generic: POST to <addr><procedure> with the arguments
// map as the JSON body; the response JSON becomes structuredContent
// and a pretty-printed content block.
//
// Failure modes returned to the MCP client:
//
//   - Unknown tool name → JSON-RPC -32602 (invalid params).
//   - Streaming RPC → JSON-RPC -32603 with an explanation (MCP tools
//     are unary; streaming proxying isn't implemented).
//   - --addr not configured → JSON-RPC -32603 with a setup hint.
//   - Network / dial failure → JSON-RPC -32603 with the cause.
//   - Connect application error (4xx/5xx with the Connect error
//     envelope) → result with isError=true + the decoded error as
//     structuredContent. This matches the MCP convention: tool-level
//     failures are part of the result, not RPC-layer errors.
//
// The MCP isError contract is important — agents reason about
// "the tool ran but failed" differently from "the tool couldn't be
// called at all." Network errors are infrastructure; Connect errors
// are business.
func (s *server) callTool(req *rpcRequest) *rpcResponse {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return rpcError(req.ID, -32602, "invalid params: "+err.Error())
	}
	var matched *tool
	for i := range s.manifest.Tools {
		if s.manifest.Tools[i].Name == params.Name {
			matched = &s.manifest.Tools[i]
			break
		}
	}
	if matched == nil {
		return rpcError(req.ID, -32602, "unknown tool: "+params.Name)
	}
	if matched.Streaming != "" {
		return rpcError(req.ID, -32603, fmt.Sprintf(
			"tool %q is a %s-streaming RPC; MCP tools are unary and the bridge does not proxy streams. "+
				"Call this procedure directly via a Connect client, or split it into unary helpers.",
			params.Name, matched.Streaming,
		))
	}
	if s.addr == "" {
		return rpcError(req.ID, -32603,
			"forge-mcp was started without --addr; tools/call has no Connect backend to reach. "+
				"Restart with --addr http://<host>:<port> pointing at your running service.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.http.Timeout)
	defer cancel()
	connectResp, connectErr, dispatchErr := s.dispatchConnect(ctx, matched, params.Arguments)
	if dispatchErr != nil {
		return rpcError(req.ID, -32603, fmt.Sprintf("dispatch %s: %v", matched.Procedure, dispatchErr))
	}

	// Application-level Connect error → MCP isError=true result.
	// The Connect error envelope (code + message) goes into content as
	// human-readable text. We DELIBERATELY do not emit structuredContent
	// on the error path: the tool's declared outputSchema describes the
	// SUCCESS shape; an {error: ...} object wouldn't conform to it and
	// the official MCP Inspector strict-validates and rejects the
	// response (caught during e2e validation). Agents can still parse
	// the typed error from the content text — Connect error codes
	// (not_found, permission_denied, invalid_argument, etc.) are a
	// small, well-known vocabulary.
	if connectErr != nil {
		return rpcResult(req.ID, map[string]any{
			"isError": true,
			"content": []map[string]any{
				{"type": "text", "text": fmt.Sprintf(
					"%s.%s failed: %s — %s",
					matched.Service, matched.Method,
					connectErr.Code, connectErr.Message,
				)},
			},
		})
	}

	// Success → content (human-readable JSON dump) plus
	// structuredContent (the decoded response itself). Per MCP spec,
	// structuredContent must conform to outputSchema; for real
	// Connect responses we trust the wire — if the server's response
	// shape diverges from the proto schema the manifest was generated
	// from, the inspector / client will surface the mismatch. Forge's
	// own contract.go discipline keeps these in sync.
	bodyJSON, _ := json.MarshalIndent(connectResp, "", "  ")
	result := map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": fmt.Sprintf(
				"%s.%s → %s\n%s",
				matched.Service, matched.Method, matched.Procedure, string(bodyJSON),
			)},
		},
	}
	if matched.OutputSchema != nil {
		result["structuredContent"] = connectResp
	}
	return rpcResult(req.ID, result)
}

// connectErrorEnvelope mirrors Connect's JSON error shape:
//
//	{"code": "not_found", "message": "user 42 not found"}
//
// We don't import connectrpc.com/connect into the bridge — keeping
// it dependency-light means the binary stays small and projects can
// vendor it without pulling forge's pkg/ tree. The envelope is stable
// per the Connect protocol spec.
type connectErrorEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// dispatchConnect performs the actual HTTP+JSON Connect call. Returns
// (responseBody, nil, nil) on success, (nil, *envelope, nil) for an
// application-level Connect error, or (nil, nil, error) for a transport
// / parse failure. Splitting these three cases at the type level keeps
// the caller branching unambiguous — "did the call reach the server"
// and "did the server accept the call" are distinct failure modes that
// agents reason about differently.
func (s *server) dispatchConnect(ctx context.Context, t *tool, args map[string]any) (map[string]any, *connectErrorEnvelope, error) {
	if args == nil {
		// Connect/JSON requires a JSON body even for no-argument RPCs;
		// empty object is the canonical "no fields set" value and
		// matches what Connect's own JSON encoder emits.
		args = map[string]any{}
	}
	body, err := json.Marshal(args)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal arguments: %w", err)
	}
	url := s.addr + t.Procedure
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("build request: %w", err)
	}
	// Connect-Protocol-Version header pins the wire version so the
	// server knows we're speaking Connect's JSON variant (and not, e.g.,
	// gRPC-Web framing). Both Content-Type and Accept are
	// application/json — the simplest Connect-over-HTTP shape.
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Connect-Protocol-Version", "1")
	if s.authHeader != "" {
		httpReq.Header.Set("Authorization", s.authHeader)
	}

	resp, err := s.http.Do(httpReq)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read response body: %w", err)
	}

	// Connect returns the error envelope as the response body when an
	// RPC fails — same Content-Type, different status code (4xx/5xx).
	if resp.StatusCode >= 400 {
		var env connectErrorEnvelope
		if jsonErr := json.Unmarshal(respBody, &env); jsonErr != nil {
			// Non-JSON error body → infrastructure failure (proxy 502,
			// HTML 404, etc.), not a Connect application error. Surface
			// it as a dispatch error with enough body for debugging.
			snippet := string(respBody)
			if len(snippet) > 512 {
				snippet = snippet[:512] + "..."
			}
			return nil, nil, fmt.Errorf("HTTP %d, non-Connect body: %s", resp.StatusCode, snippet)
		}
		if env.Code == "" {
			// Connect's spec requires `code` on every error envelope;
			// missing → treat as malformed and surface upstream.
			return nil, nil, errors.New("HTTP " + resp.Status + " with empty Connect error envelope")
		}
		return nil, &env, nil
	}

	var out map[string]any
	if len(respBody) == 0 {
		// Empty 2xx body is a legal zero-field Connect response;
		// google.protobuf.Empty round-trips as `{}` typically, but
		// some servers omit the body altogether. Normalize.
		return map[string]any{}, nil, nil
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, nil, fmt.Errorf("decode success body: %w (body: %s)", err, string(respBody))
	}
	return out, nil, nil
}

// zeroValueFromSchema synthesizes the smallest JSON value that satisfies
// the given JSON Schema. Handles the subset forge codegen emits:
// `object` with `properties` + `required`, `string`, `number`,
// `integer`, `boolean`, `array` with `items`. Unknown / missing
// `type` falls back to nil (JSON null), which is the safest default
// when the schema is under-specified.
//
// The placeholder is NOT meant to look like real data — it satisfies
// the spec's "structuredContent must conform to outputSchema" rule so
// the Inspector and Claude Desktop accept the response. Real values
// land when callTool is upgraded to dispatch the actual Connect RPC.
func zeroValueFromSchema(schema map[string]any) any {
	t, _ := schema["type"].(string)
	switch t {
	case "object":
		out := map[string]any{}
		props, _ := schema["properties"].(map[string]any)
		required, _ := schema["required"].([]any)
		for _, r := range required {
			key, ok := r.(string)
			if !ok {
				continue
			}
			sub, _ := props[key].(map[string]any)
			if sub == nil {
				out[key] = nil
				continue
			}
			out[key] = zeroValueFromSchema(sub)
		}
		return out
	case "string":
		return ""
	case "number", "integer":
		return 0
	case "boolean":
		return false
	case "array":
		// Empty arrays satisfy any items schema and any required
		// constraint that doesn't pin a minimum length.
		return []any{}
	default:
		return nil
	}
}
