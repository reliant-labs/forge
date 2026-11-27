package mcpkit

// handler.go — /mcp: MCP streamable HTTP over the listener's own mux.
//
// Each POST is stateless: a fresh go-sdk *mcp.Server holding the tools this
// caller may see answers it, and each tools/call is relayed as a Connect
// unary JSON request to the tool's procedure over an in-process transport
// into the listener's mux — so the call passes the very interceptor chain
// (auth, validation, rate limits, audit, tracing) a frontend's call does.
//
// AUTH. The chain is the authority; mcpkit adds no validator. A request with
// no credential is refused with 401 and a WWW-Authenticate pointing at the
// RFC 9728 resource metadata, unless some served tool is public: then it is
// answered with only the public tools. A request WITH a credential has it
// judged by the chain, through AuthCheckProcedure — a no-op rpc the listener
// mounts with its chain — so a credential the project's Authenticator
// refuses is a 401 too, whether forge's authn or the project's own decides.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
	"buf.build/go/protovalidate"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/time/rate"
	"google.golang.org/protobuf/proto"

	"github.com/reliant-labs/forge/pkg/internal/inprocess"
)

// Path is where a listener serves MCP.
const Path = "/mcp"

// MetadataPath is where a listener serves the RFC 9728 protected-resource
// metadata of Path (the well-known URI with the resource's path appended).
const MetadataPath = "/.well-known/oauth-protected-resource" + Path

// ViaHeader is the header every relayed tool call carries, with the value
// "mcp", so the chain (audit, activity) can tell an agent's call from a
// frontend's.
const ViaHeader = "Forge-Via"

// The per-credential limit defaults (Config).
const (
	DefaultRequestsPerMinute = 120
	DefaultBurst             = 30
	DefaultMaxInFlight       = 4
	DefaultMaxRequestBytes   = 4 << 20
)

// TracerName is the OpenTelemetry tracer of the per-request MCP span.
const TracerName = "github.com/reliant-labs/forge/pkg/mcpkit"

// Config is what a listener serves on /mcp. The zero value of every field but
// Tools is a working default.
type Config struct {
	// Tools are the rpcs served as tools: the generated table
	// (middleware.MCPTools in a forge app). Empty serves no tools.
	Tools []Tool

	// RequestsPerMinute, Burst and MaxInFlight bound tool calls per
	// credential (per peer address for a caller with none), so a model that
	// retries a refusal forever is stopped. Zero is the default (120/min,
	// burst 30, 4 in flight); negative is no limit. A limited call is a
	// ResourceExhausted tool error. The chain's own RateLimit slot still
	// applies to each call.
	RequestsPerMinute int
	Burst             int
	MaxInFlight       int

	// AllowedOrigins are the browser origins (scheme://host[:port]) /mcp
	// answers: the listener's CORS allowlist. A request carrying an Origin
	// that is not listed is refused 403 — the DNS-rebinding defence MCP
	// requires, which is why the request's own Host is no evidence (a
	// rebound name arrives with matching Host and Origin). A request with no
	// Origin (a non-browser MCP client) is unaffected.
	AllowedOrigins []string

	// CredentialHeaders names headers beyond Authorization and Cookie that
	// carry a credential the project's Authenticator reads (an API-key
	// header). Each is copied onto relayed calls and counts as a credential.
	CredentialHeaders []string

	// MaxRequestBytes caps one MCP request body. Zero is 4 MiB, the
	// listener's default payload cap.
	MaxRequestBytes int64

	// AuthorizationServers are the OAuth issuers the resource metadata names.
	// Empty names none: a client is expected to send a token it already has.
	AuthorizationServers []string

	// ServerName and ServerVersion are announced at initialize. Empty is
	// "forge" and "1".
	ServerName    string
	ServerVersion string

	// Logger receives the handler's lines; nil is slog.Default().
	Logger *slog.Logger
}

// Handler serves /mcp over upstream, the listener's mux.
type Handler struct {
	cfg      Config
	upstream *http.Client
	logger   *slog.Logger
	tools    map[string]*Tool
	public   bool // some tool is public
	allPub   bool // every tool is public
	streams  *mcp.StreamableHTTPHandler
	limits   *limiter
	tracer   trace.Tracer
}

// New builds the /mcp handler. upstream is the listener's mux, on which every
// tool's procedure and AuthCheckProcedure are mounted with the chain. It
// refuses a table with a duplicate or empty tool name or procedure.
func New(cfg Config, upstream http.Handler) (*Handler, error) {
	if upstream == nil {
		return nil, errors.New("mcpkit.New: upstream is required")
	}
	h := &Handler{
		cfg:      cfg,
		upstream: &http.Client{Transport: inprocess.Transport{Handler: upstream}},
		logger:   cfg.Logger,
		tools:    make(map[string]*Tool, len(cfg.Tools)),
		allPub:   len(cfg.Tools) > 0,
		tracer:   otel.Tracer(TracerName),
	}
	if h.logger == nil {
		h.logger = slog.Default()
	}
	for i := range cfg.Tools {
		t := &cfg.Tools[i]
		if t.Name == "" || t.Procedure == "" {
			return nil, fmt.Errorf("mcpkit.New: tool %d has an empty name or procedure", i)
		}
		if _, dup := h.tools[t.Name]; dup {
			return nil, fmt.Errorf("mcpkit.New: tool %q is declared twice", t.Name)
		}
		h.tools[t.Name] = t
		h.public = h.public || t.Public
		h.allPub = h.allPub && t.Public
	}
	h.limits = newLimiter(cfg.RequestsPerMinute, cfg.Burst, cfg.MaxInFlight)
	maxBytes := cfg.MaxRequestBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxRequestBytes
	}
	h.streams = mcp.NewStreamableHTTPHandler(h.serverFor, &mcp.StreamableHTTPOptions{
		Stateless:           true,
		JSONResponse:        true,
		MaxRequestBodyBytes: maxBytes,
		Logger:              h.logger,
	})
	return h, nil
}

type callerKey struct{}

// caller is what the HTTP layer decided about one request.
type caller struct {
	req         *http.Request
	credentials bool
}

// ServeHTTP answers one MCP request: the Origin check, the credential
// decision, then the go-sdk's streamable transport.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.originAllowed(r) {
		http.Error(w, "mcp: origin not allowed", http.StatusForbidden)
		return
	}
	c := &caller{req: r, credentials: h.hasCredential(r.Header)}
	switch {
	case c.credentials:
		if status, err := h.checkCredential(r); err != nil {
			if status == http.StatusUnauthorized {
				h.challenge(w, r, err.Error())
				return
			}
			http.Error(w, "mcp: "+err.Error(), status)
			return
		}
	case !h.public:
		h.challenge(w, r, "a credential is required")
		return
	}
	h.streams.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, c)))
}

// MetadataHandler serves the RFC 9728 protected-resource metadata of /mcp on
// MetadataPath.
func (h *Handler) MetadataHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
			Resource:               baseURL(r) + Path,
			AuthorizationServers:   h.cfg.AuthorizationServers,
			BearerMethodsSupported: []string{"header"},
			ResourceName:           h.serverName(),
		}).ServeHTTP(w, r)
	})
}

// challenge is the 401 that starts an MCP client's sign-in.
func (h *Handler) challenge(w http.ResponseWriter, r *http.Request, reason string) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata=%q`, baseURL(r)+MetadataPath))
	http.Error(w, "mcp: unauthenticated: "+reason, http.StatusUnauthorized)
}

func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (h *Handler) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	for _, o := range h.cfg.AllowedOrigins {
		if strings.EqualFold(strings.TrimRight(o, "/"), origin) {
			return true
		}
	}
	return false
}

// credentialHeaders are the headers copied onto relayed calls.
func (h *Handler) credentialHeaders() []string {
	return append([]string{"Authorization", "Cookie"}, h.cfg.CredentialHeaders...)
}

func (h *Handler) hasCredential(hdr http.Header) bool {
	for _, k := range h.credentialHeaders() {
		if hdr.Get(k) != "" {
			return true
		}
	}
	return false
}

// checkCredential asks the chain, through AuthCheckProcedure, whether r's
// credential is good. It returns the HTTP status to refuse with.
func (h *Handler) checkCredential(r *http.Request) (int, error) {
	res, err := h.relay(r.Context(), r, AuthCheckProcedure, []byte("{}"))
	if err != nil {
		return http.StatusBadGateway, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	if res.StatusCode == http.StatusOK {
		return 0, nil
	}
	ce := decodeConnectError(res.StatusCode, body)
	switch ce.Code {
	case "unauthenticated":
		return http.StatusUnauthorized, errors.New(ce.Message)
	case "permission_denied":
		return http.StatusForbidden, errors.New(ce.Message)
	case "unimplemented", "not_found":
		// No check mounted: fail closed.
		return http.StatusUnauthorized, errors.New("the listener mounts no credential check")
	default:
		return http.StatusServiceUnavailable, fmt.Errorf("credential check: %s: %s", ce.Code, ce.Message)
	}
}

// relay POSTs body to procedure over the in-process transport, carrying the
// outer request's credentials, its trace context and ViaHeader.
func (h *Handler) relay(ctx context.Context, outer *http.Request, procedure string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(inprocess.WithOrigin(ctx, outer), http.MethodPost, inprocess.BaseURL+procedure, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set(ViaHeader, "mcp")
	for _, k := range h.credentialHeaders() {
		for _, v := range outer.Header.Values(k) {
			req.Header.Add(k, v)
		}
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	return h.upstream.Do(req)
}

func (h *Handler) serverName() string {
	if h.cfg.ServerName != "" {
		return h.cfg.ServerName
	}
	return "forge"
}

// serverFor builds the stateless server for one request, holding the tools
// its caller may see: every tool with a credential, else the public ones.
func (h *Handler) serverFor(r *http.Request) *mcp.Server {
	c, _ := r.Context().Value(callerKey{}).(*caller)
	if c == nil {
		return nil
	}
	version := h.cfg.ServerVersion
	if version == "" {
		version = "1"
	}
	s := mcp.NewServer(&mcp.Implementation{Name: h.serverName(), Version: version},
		&mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}}})
	s.AddReceivingMiddleware(h.traceMiddleware)
	for i := range h.cfg.Tools {
		t := &h.cfg.Tools[i]
		if !c.credentials && !t.Public {
			continue
		}
		s.AddTool(sdkTool(t), h.toolHandler(c, t))
	}
	return s
}

func sdkTool(t *Tool) *mcp.Tool {
	in := t.InputSchema
	if len(in) == 0 {
		in = json.RawMessage(`{"type":"object"}`)
	}
	st := &mcp.Tool{
		Name:        t.Name,
		Description: t.Description,
		InputSchema: in,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   t.ReadOnly,
			IdempotentHint: t.Idempotent,
		},
	}
	// destructiveHint defaults to true in MCP; say false unless destructive.
	destructive := t.Destructive
	st.Annotations.DestructiveHint = &destructive
	if len(t.OutputSchema) > 0 {
		st.OutputSchema = t.OutputSchema
	}
	return st
}

// traceMiddleware is the parent span of one MCP request.
func (h *Handler) traceMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		attrs := []attribute.KeyValue{attribute.String("mcp.method.name", method)}
		name := method
		if p, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok && p != nil {
			attrs = append(attrs, attribute.String("gen_ai.tool.name", p.Name))
			name = method + " " + p.Name
		}
		ctx, span := h.tracer.Start(ctx, name, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attrs...))
		defer span.End()
		res, err := next(ctx, method, req)
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
		} else if tr, ok := res.(*mcp.CallToolResult); ok && tr.IsError {
			span.SetStatus(codes.Error, "tool error")
		}
		return res, err
	}
}

// toolHandler relays one tools/call to t's procedure.
func (h *Handler) toolHandler(c *caller, t *Tool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := json.RawMessage(req.Params.Arguments)
		if len(bytes.TrimSpace(args)) == 0 || bytes.Equal(bytes.TrimSpace(args), []byte("null")) {
			args = json.RawMessage("{}")
		}
		if !json.Valid(args) || bytes.TrimSpace(args)[0] != '{' {
			return nil, fmt.Errorf("tool %s: arguments must be a JSON object", t.Name)
		}
		release, ok := h.limits.acquire(h.limitKey(c))
		if !ok {
			return errorResult("resource_exhausted", "too many MCP tool calls for this credential; slow down", nil), nil
		}
		defer release()
		res, err := h.relay(ctx, c.req, t.Procedure, args)
		if err != nil {
			return errorResult("unavailable", err.Error(), nil), nil
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil {
			return errorResult("unavailable", "reading the response: "+err.Error(), nil), nil
		}
		if res.StatusCode != http.StatusOK {
			ce := decodeConnectError(res.StatusCode, body)
			return errorResult(ce.Code, ce.Message, ce.violations()), nil
		}
		if len(bytes.TrimSpace(body)) == 0 {
			body = []byte("{}")
		}
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: string(body)}},
			StructuredContent: json.RawMessage(body),
		}, nil
	}
}

func errorResult(code, message string, violations []string) *mcp.CallToolResult {
	text := code + ": " + message
	for _, v := range violations {
		text += "\n- " + v
	}
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// limitKey is the caller's credential, hashed; the peer address without one.
func (h *Handler) limitKey(c *caller) string {
	sum := sha256.New()
	var any bool
	for _, k := range h.credentialHeaders() {
		for _, v := range c.req.Header.Values(k) {
			any = true
			_, _ = io.WriteString(sum, k+"\x00"+v+"\x00")
		}
	}
	if any {
		return "cred:" + hex.EncodeToString(sum.Sum(nil))
	}
	host, _, err := net.SplitHostPort(c.req.RemoteAddr)
	if err != nil {
		host = c.req.RemoteAddr
	}
	return "peer:" + host
}

// connectError is the Connect protocol's JSON error body.
type connectError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details []struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"details"`
}

func decodeConnectError(status int, body []byte) connectError {
	var ce connectError
	if err := json.Unmarshal(body, &ce); err != nil || ce.Code == "" {
		ce.Code = codeForHTTP(status)
		if ce.Message == "" {
			ce.Message = strings.TrimSpace(string(body))
		}
		if ce.Message == "" {
			ce.Message = http.StatusText(status)
		}
	}
	return ce
}

// violations renders protovalidate's field violations, the detail an
// InvalidArgument from the chain's validation slot carries.
func (ce connectError) violations() []string {
	if ce.Code != "invalid_argument" {
		return nil
	}
	var out []string
	for _, d := range ce.Details {
		if d.Type != "buf.validate.Violations" {
			continue
		}
		raw, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(d.Value, "="))
		if err != nil {
			continue
		}
		var vs validate.Violations
		if proto.Unmarshal(raw, &vs) != nil {
			continue
		}
		for _, v := range vs.GetViolations() {
			field := protovalidate.FieldPathString(v.GetField())
			if field == "" {
				out = append(out, v.GetMessage())
				continue
			}
			out = append(out, field+": "+v.GetMessage())
		}
	}
	return out
}

// codeForHTTP is the Connect code of a bare HTTP status (connect's own
// mapping), for an answer with no Connect error body.
func codeForHTTP(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_argument"
	case http.StatusUnauthorized:
		return "unauthenticated"
	case http.StatusForbidden:
		return "permission_denied"
	case http.StatusNotFound:
		return "unimplemented"
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return "unavailable"
	default:
		return "unknown"
	}
}

// limiter is the per-key rate and concurrency bound.
type limiter struct {
	mu       sync.Mutex
	perMin   int
	burst    int
	inFlight int
	keys     map[string]*limitEntry
	lastGC   time.Time
}

type limitEntry struct {
	rate *rate.Limiter
	sem  chan struct{}
	seen time.Time
}

func newLimiter(perMin, burst, inFlight int) *limiter {
	if perMin == 0 {
		perMin = DefaultRequestsPerMinute
	}
	if burst == 0 {
		burst = DefaultBurst
	}
	if inFlight == 0 {
		inFlight = DefaultMaxInFlight
	}
	return &limiter{perMin: perMin, burst: burst, inFlight: inFlight, keys: map[string]*limitEntry{}, lastGC: time.Now()}
}

// acquire admits one call for key, returning its release; false is limited.
func (l *limiter) acquire(key string) (func(), bool) {
	now := time.Now()
	l.mu.Lock()
	if now.Sub(l.lastGC) > 10*time.Minute {
		for k, e := range l.keys {
			if now.Sub(e.seen) > 10*time.Minute && len(e.sem) == 0 {
				delete(l.keys, k)
			}
		}
		l.lastGC = now
	}
	e := l.keys[key]
	if e == nil {
		e = &limitEntry{}
		if l.perMin > 0 {
			e.rate = rate.NewLimiter(rate.Limit(float64(l.perMin)/60), max(l.burst, 1))
		}
		if l.inFlight > 0 {
			e.sem = make(chan struct{}, l.inFlight)
		}
		l.keys[key] = e
	}
	e.seen = now
	l.mu.Unlock()
	if e.rate != nil && !e.rate.AllowN(now, 1) {
		return nil, false
	}
	if e.sem == nil {
		return func() {}, true
	}
	select {
	case e.sem <- struct{}{}:
		return func() { <-e.sem }, true
	default:
		return nil, false
	}
}
