// Package mcpbridge is a Model Context Protocol stdio server that reads
// a forge project's gen/mcp/manifest.json and exposes every RPC tool the
// manifest declares to an MCP client (Claude Code, Claude Desktop, the
// official MCP Inspector, Cline, etc.).
//
// It is the host for the otherwise-orphan gen/mcp/manifest.json: forge
// codegen writes one MCP tool per Connect RPC, and this package turns that
// static descriptor into a live, callable MCP server. Both the `forge mcp
// serve` CLI subcommand and the standalone cmd/forge-mcp binary are thin
// wrappers over Server here, so the protocol behaviour stays defined once.
//
// What it does:
//
//   - Reads a loaded gen/mcp/manifest.json (Manifest).
//   - Speaks MCP over stdio (JSON-RPC 2.0, one message per line — the
//     shape Claude Code / Claude Desktop expect).
//   - Implements initialize, tools/list, tools/call, ping.
//   - tools/call ACTUALLY dispatches to the running Connect service over
//     HTTP+JSON (the same transport `forge api curl` builds): POST to
//     <addr><procedure> with the arguments map as the JSON body; the
//     Connect response becomes structuredContent + a human-readable
//     content block. Connect error envelopes become MCP isError:true
//     results so agents see typed failures.
//
// Why HTTP+JSON: Connect's JSON wire format lets a generic client call any
// procedure without compiling against the project's proto types — the
// bridge stays one implementation for every forge project. Streaming RPCs
// (server/client/bidi) are NOT supported because MCP tool calls are unary;
// streaming tools are excluded from tools/list and tools/call returns an
// explicit error if a client calls one by name anyway.
//
// Auth: the bridge sets a verbatim Authorization header on every dispatched
// RPC when one is configured (Server.AuthHeader). A forge dev server that
// is NOT in AUTH_DEV_MODE runs the auth interceptor, so without a token
// every tools/call returns a clean Connect "unauthenticated" error — which
// itself proves the transport wiring. With a token, real calls succeed.
package mcpbridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

const (
	// ProtocolVersion is the MCP revision this server targets. 2024-11-05
	// is the revision Claude Desktop / Claude Code and the official
	// Inspector negotiate; its Tool.inputSchema is an open object, which
	// is why $defs/$ref schemas pass through unmodified.
	ProtocolVersion = "2024-11-05"
	ServerName      = "forge-mcp"
	ServerVersion   = "0.1.0"
)

// Manifest mirrors the gen/mcp/manifest.json shape forge writes. We keep
// the read shape local (rather than importing the codegen emitter type) so
// the bridge is insulated from forge codegen churn — a project may run this
// against a manifest produced by a different forge version.
type Manifest struct {
	Generated     string `json:"_generated"`
	SchemaVersion string `json:"schema_version"`
	Project       string `json:"project"`
	Tools         []Tool `json:"tools"`
}

// Tool carries every field the MCP spec needs for tools/list plus the
// forge-specific routing metadata (service / method / procedure /
// auth_required / idempotency_key / streaming). MCP clients ignore the
// snake_case extras per the protocol's permissive unknown-field policy;
// the bridge strips them from the tools/list wire response anyway so even
// strict clients accept it.
type Tool struct {
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

// LoadManifest reads and parses a gen/mcp/manifest.json file.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

// Server is an MCP stdio bridge over one project's manifest. Construct it
// directly (all fields exported) and call Run. Logf, when nil, defaults to
// the standard logger on stderr — stdout is reserved for MCP framing.
type Server struct {
	// Manifest is the loaded gen/mcp/manifest.json. Required.
	Manifest *Manifest
	// Addr is the Connect base URL (e.g. http://localhost:8080). Empty →
	// tools/call returns a clear configuration error rather than silently
	// failing to dial. Trailing slash is trimmed at Run so URL
	// concatenation with the procedure path always produces a clean URL.
	Addr string
	// AuthHeader, when non-empty, is set verbatim as the Authorization
	// header on every dispatched RPC. The bridge never inspects or
	// refreshes the token — the caller passes a current value.
	AuthHeader string
	// HTTP is the client used for all Connect dispatches. Required; its
	// Timeout applies per tools/call invocation.
	HTTP *http.Client
	// Logf receives diagnostics. nil → log.Printf (stderr).
	Logf func(format string, args ...any)
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// CallableToolCount counts the tools the MCP surface actually advertises —
// manifest tools minus streaming RPCs, which tools/list excludes because
// the bridge cannot proxy streams.
func (s *Server) CallableToolCount() int {
	n := 0
	for _, t := range s.Manifest.Tools {
		if t.Streaming == "" {
			n++
		}
	}
	return n
}

// Run is the JSON-RPC over stdio main loop. Each line is one JSON-RPC 2.0
// request; responses are framed the same way. The loop exits on stdin EOF,
// which is how MCP clients signal disconnect.
func (s *Server) Run(in io.Reader, out io.Writer) error {
	s.Addr = strings.TrimRight(s.Addr, "/")
	scanner := bufio.NewScanner(in)
	// MCP messages can be large (tools/list with many tools, nested
	// schemas). 16 MiB is well above any realistic request and well below
	// stdio buffer limits.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	enc := json.NewEncoder(out)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.logf("parse error: %v (raw: %q)", err, line)
			_ = enc.Encode(rpcError(nil, -32700, "parse error: "+err.Error()))
			continue
		}
		s.logf("→ method=%s id=%v", req.Method, req.ID)
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
	return &rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcErr{Code: code, Message: msg}}
}

func rpcResult(id json.RawMessage, result any) *rpcResponse {
	return &rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

// dispatch returns nil for notifications (no id), which Run interprets as
// "don't write a response."
func (s *Server) dispatch(req *rpcRequest) *rpcResponse {
	switch req.Method {
	case "initialize":
		return rpcResult(req.ID, map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities": map[string]any{
				// Tools only. Resources, prompts, sampling are out of scope.
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    ServerName,
				"version": ServerVersion,
			},
			"instructions": fmt.Sprintf(
				"Bridge for forge project %q. %d Connect RPC(s) exposed as tools. Call tools/list to enumerate.",
				s.Manifest.Project, s.CallableToolCount()),
		})

	case "notifications/initialized":
		return nil

	case "ping":
		return rpcResult(req.ID, map[string]any{})

	case "tools/list":
		// Return the manifest's tools — already in MCP shape. We strip the
		// forge-specific snake_case metadata so strict-mode clients that
		// reject unknown fields still accept the response. Streaming RPCs
		// are EXCLUDED entirely (MCP tool calls are unary; the bridge
		// cannot proxy streams). inputSchema / outputSchema are forwarded
		// verbatim, including $defs / $ref (schema_version 1.1+): the MCP
		// Tool.inputSchema is an open object, the official MCP TypeScript
		// SDK itself emits $ref schemas, and inlining would reintroduce
		// infinite expansion for recursive messages.
		stripped := make([]map[string]any, 0, len(s.Manifest.Tools))
		for _, t := range s.Manifest.Tools {
			if t.Streaming != "" {
				continue
			}
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
		return rpcResult(req.ID, map[string]any{"tools": stripped})

	case "tools/call":
		return s.callTool(req)

	default:
		return rpcError(req.ID, -32601, "method not found: "+req.Method)
	}
}

// callTool implements MCP tools/call by dispatching the named tool to the
// running Connect service over HTTP+JSON. Failure modes returned to the
// client:
//
//   - Unknown tool name → JSON-RPC -32602.
//   - Streaming RPC → JSON-RPC -32603 (MCP tools are unary).
//   - Addr not configured → JSON-RPC -32603 with a setup hint.
//   - Network / dial failure → JSON-RPC -32603 with the cause.
//   - Connect application error (4xx/5xx envelope) → result with
//     isError=true + the decoded error in the content text block. This
//     matches the MCP convention: tool-level failures are part of the
//     result, RPC-layer failures are JSON-RPC errors.
func (s *Server) callTool(req *rpcRequest) *rpcResponse {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return rpcError(req.ID, -32602, "invalid params: "+err.Error())
	}
	var matched *Tool
	for i := range s.Manifest.Tools {
		if s.Manifest.Tools[i].Name == params.Name {
			matched = &s.Manifest.Tools[i]
			break
		}
	}
	if matched == nil {
		return rpcError(req.ID, -32602, "unknown tool: "+params.Name)
	}
	if matched.Streaming != "" {
		return rpcError(req.ID, -32603, fmt.Sprintf(
			"tool %q is a %s-streaming RPC; MCP tools are unary and the bridge does not proxy streams "+
				"(streaming tools are excluded from tools/list for this reason). "+
				"Call this procedure directly via a Connect client, or split it into unary helpers.",
			params.Name, matched.Streaming,
		))
	}
	if s.Addr == "" {
		return rpcError(req.ID, -32603,
			"forge mcp serve was started without a Connect backend address; tools/call has nowhere to dial. "+
				"Restart with --addr http://<host>:<port> pointing at your running service.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.HTTP.Timeout)
	defer cancel()
	connectResp, connectErr, dispatchErr := s.dispatchConnect(ctx, matched, params.Arguments)
	if dispatchErr != nil {
		return rpcError(req.ID, -32603, fmt.Sprintf("dispatch %s: %v", matched.Procedure, dispatchErr))
	}

	// Application-level Connect error → MCP isError=true result. We do NOT
	// emit structuredContent on the error path: the tool's outputSchema
	// describes the SUCCESS shape, and an {error: ...} object would fail
	// strict validation in clients like the MCP Inspector. The Connect
	// code + message land in content text; agents typed-parse them there.
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

	// Success → content (human-readable dump) plus structuredContent (the
	// decoded response). structuredContent is only emitted when the tool
	// declares an outputSchema — otherwise the field would imply a
	// conformance claim the bridge can't back.
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
//	{"code": "unauthenticated", "message": "missing bearer token"}
//
// We don't import connectrpc.com/connect — keeping the bridge
// dependency-light. The envelope is stable per the Connect protocol spec.
type connectErrorEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// dispatchConnect performs the HTTP+JSON Connect call. Returns
// (responseBody, nil, nil) on success, (nil, *envelope, nil) for an
// application-level Connect error, or (nil, nil, error) for a transport /
// parse failure. The three-way split keeps the caller's branching
// unambiguous: "did the call reach the server" and "did the server accept
// it" are distinct failure modes agents reason about differently.
func (s *Server) dispatchConnect(ctx context.Context, t *Tool, args map[string]any) (map[string]any, *connectErrorEnvelope, error) {
	if args == nil {
		// Connect/JSON requires a JSON body even for no-argument RPCs;
		// empty object is the canonical "no fields set" value.
		args = map[string]any{}
	}
	body, err := json.Marshal(args)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal arguments: %w", err)
	}
	url := s.Addr + t.Procedure
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("build request: %w", err)
	}
	// Connect-Protocol-Version pins the wire version (JSON variant, not
	// gRPC-Web framing). This is exactly the shape `forge api curl` emits.
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Connect-Protocol-Version", "1")
	if s.AuthHeader != "" {
		httpReq.Header.Set("Authorization", s.AuthHeader)
	}

	resp, err := s.HTTP.Do(httpReq)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode >= 400 {
		var env connectErrorEnvelope
		if jsonErr := json.Unmarshal(respBody, &env); jsonErr != nil {
			snippet := string(respBody)
			if len(snippet) > 512 {
				snippet = snippet[:512] + "..."
			}
			return nil, nil, fmt.Errorf("HTTP %d, non-Connect body: %s", resp.StatusCode, snippet)
		}
		if env.Code == "" {
			return nil, nil, errors.New("HTTP " + resp.Status + " with empty Connect error envelope")
		}
		return nil, &env, nil
	}

	if len(respBody) == 0 {
		// Empty 2xx body is a legal zero-field Connect response.
		return map[string]any{}, nil, nil
	}
	var out map[string]any
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, nil, fmt.Errorf("decode success body: %w (body: %s)", err, string(respBody))
	}
	return out, nil, nil
}
