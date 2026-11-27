package serverkit_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/reliant-labs/forge/pkg/authn"
	"github.com/reliant-labs/forge/pkg/mcpkit"
	"github.com/reliant-labs/forge/pkg/serverkit"
)

const (
	mcpGetProc    = "/mcptest.v1.WidgetService/GetWidget"
	mcpPublicProc = "/mcptest.v1.WidgetService/ListPublic"
	mcpFailProc   = "/mcptest.v1.WidgetService/Explode"
	mcpHiddenProc = "/mcptest.v1.WidgetService/DeleteWidget"
)

// mcpAuth admits "Bearer good", and anyone on the public procedure — the
// project authenticator /mcp defers to.
type mcpAuth struct{}

func (mcpAuth) Claims() func(context.Context) (*authn.Claims, bool) { return authn.ClaimsFromContext }

func (mcpAuth) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().Procedure != mcpPublicProc && req.Header().Get("Authorization") != "Bearer good" {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("bad credential"))
		}
		return next(ctx, req)
	}
}
func (mcpAuth) WrapStreamingClient(n connect.StreamingClientFunc) connect.StreamingClientFunc {
	return n
}
func (mcpAuth) WrapStreamingHandler(n connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return n
}

func mcpRoutes() serverkit.Routes {
	return serverkit.RoutesFunc(func(mux *http.ServeMux, opts ...connect.HandlerOption) []string {
		echo := func(_ context.Context, req *connect.Request[structpb.Struct]) (*connect.Response[structpb.Struct], error) {
			out, _ := structpb.NewStruct(map[string]any{"via": req.Header().Get(mcpkit.ViaHeader), "in": req.Msg.AsMap()})
			return connect.NewResponse(out), nil
		}
		fail := func(context.Context, *connect.Request[structpb.Struct]) (*connect.Response[structpb.Struct], error) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("widget not found"))
		}
		for _, p := range []string{mcpGetProc, mcpPublicProc, mcpHiddenProc} {
			mux.Handle(p, connect.NewUnaryHandler(p, echo, opts...))
		}
		mux.Handle(mcpFailProc, connect.NewUnaryHandler(mcpFailProc, fail, opts...))
		return nil
	})
}

var mcpTools = []mcpkit.Tool{
	{Name: "widget_get_widget", Procedure: mcpGetProc, Description: "Get a widget.", ReadOnly: true, Idempotent: true},
	{Name: "widget_list_public", Procedure: mcpPublicProc, Description: "Public widgets.", ReadOnly: true, Public: true},
	{Name: "widget_explode", Procedure: mcpFailProc, Description: "Fails.", Destructive: true},
}

func mcpListener(t *testing.T, cfg serverkit.ListenerConfig, tools []mcpkit.Tool) string {
	t.Helper()
	chain, err := serverkit.NewChain(serverkit.ChainDeps{Auth: serverkit.AuthFor(serverkit.AudienceUser, mcpAuth{}), Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	l, err := serverkit.NewListener(serverkit.ListenerDeps{
		Config: cfg, Chain: chain, Routes: mcpRoutes(), Logger: discardLogger(),
		MCP: &mcpkit.Config{Tools: tools},
	})
	if err != nil {
		t.Fatal(err)
	}
	base, _ := startListener(t, l)
	return base
}

type headerRT struct {
	token string
	next  http.RoundTripper
}

func (h headerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	if h.token != "" {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+h.token)
	}
	return h.next.RoundTrip(r)
}

func mcpConnect(t *testing.T, base, token string) (*mcp.ClientSession, error) {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	return client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:             base + mcpkit.Path,
		HTTPClient:           &http.Client{Transport: headerRT{token, http.DefaultTransport}},
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}, nil)
}

func toolNames(t *testing.T, s *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	res, err := s.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		out[tool.Name] = tool
	}
	return out
}

func TestMCP_ListAndCall(t *testing.T) {
	base := mcpListener(t, loopback(), mcpTools)
	s, err := mcpConnect(t, base, "good")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	tools := toolNames(t, s)
	if len(tools) != 3 {
		t.Fatalf("tools/list = %v", tools)
	}
	get := tools["widget_get_widget"]
	if !get.Annotations.ReadOnlyHint || !get.Annotations.IdempotentHint || *get.Annotations.DestructiveHint {
		t.Errorf("get annotations = %+v", get.Annotations)
	}
	if ex := tools["widget_explode"]; !*ex.Annotations.DestructiveHint || ex.Annotations.ReadOnlyHint {
		t.Errorf("explode annotations = %+v", ex.Annotations)
	}

	res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: "widget_get_widget", Arguments: map[string]any{"id": "w1"}})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(raw), `"via":"mcp"`) || !strings.Contains(string(raw), `"id":"w1"`) {
		t.Errorf("structuredContent = %s", raw)
	}

	res, err = s.CallTool(t.Context(), &mcp.CallToolParams{Name: "widget_explode", Arguments: map[string]any{}})
	if err != nil || !res.IsError || res.Content[0].(*mcp.TextContent).Text != "not_found: widget not found" {
		t.Fatalf("connect error: %v %+v", err, res)
	}

	// A procedure mounted on the mux but absent from the table is no tool.
	if _, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: "widget_delete_widget", Arguments: map[string]any{}}); err == nil {
		t.Fatal("a tool not in the table was callable")
	}
}

func TestMCP_Auth(t *testing.T) {
	private := []mcpkit.Tool{mcpTools[0]}
	base := mcpListener(t, loopback(), private)
	for _, token := range []string{"", "bad"} {
		res, err := http.Post(base+mcpkit.Path, "application/json", strings.NewReader(`{}`))
		if token != "" {
			req, _ := http.NewRequest(http.MethodPost, base+mcpkit.Path, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+token)
			res, err = http.DefaultClient.Do(req)
		}
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized || !strings.Contains(res.Header.Get("WWW-Authenticate"), mcpkit.MetadataPath) {
			t.Errorf("token %q: %d %q", token, res.StatusCode, res.Header.Get("WWW-Authenticate"))
		}
		if _, err := mcpConnect(t, base, token); err == nil {
			t.Errorf("token %q: an MCP client connected", token)
		}
	}
	res, err := http.Get(base + mcpkit.MetadataPath)
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	_ = json.NewDecoder(res.Body).Decode(&meta)
	res.Body.Close()
	if meta["resource"] != base+mcpkit.Path {
		t.Errorf("metadata = %v", meta)
	}

	// A foreign browser origin is refused (DNS rebinding).
	req, _ := http.NewRequest(http.MethodPost, base+mcpkit.Path, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer good")
	req.Header.Set("Origin", "https://evil.example")
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusForbidden {
		t.Errorf("foreign origin: %v %v", err, res)
	}
}

func TestMCP_MixedPublicWithoutCredential(t *testing.T) {
	base := mcpListener(t, loopback(), mcpTools)
	s, err := mcpConnect(t, base, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tools := toolNames(t, s)
	if len(tools) != 1 || tools["widget_list_public"] == nil {
		t.Fatalf("anonymous tools/list = %v", tools)
	}
	if res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: "widget_list_public", Arguments: map[string]any{}}); err != nil || res.IsError {
		t.Fatalf("public call: %v %+v", err, res)
	}
	if _, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: "widget_get_widget", Arguments: map[string]any{}}); err == nil {
		t.Fatal("an anonymous caller called a non-public tool")
	}
}

func TestMCP_DisabledPerEnv(t *testing.T) {
	cfg := serverkit.Config{Addr: "127.0.0.1:0", MCPDisabled: true}.Listener()
	base := mcpListener(t, cfg, mcpTools)
	req, _ := http.NewRequest(http.MethodPost, base+mcpkit.Path, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer good")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("/mcp with MCPDisabled = %d, want 404", res.StatusCode)
	}
}
