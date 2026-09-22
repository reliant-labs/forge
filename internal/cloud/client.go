package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client calls a hosted control plane over Connect's HTTP+JSON binding:
// POST <base>/<package>.<Service>/<Method> with a JSON body and a JSON
// reply.
//
// WHY HTTP+JSON AND NOT A GENERATED CLIENT. A generated client would
// mean vendoring the control plane's protos into forge, which is the one
// coupling that must not ship — it would make forge's deploy story
// specific to our control plane in exactly the way forge's External
// target is deliberately NOT specific to Fly.io. Connect's JSON binding
// is a documented, stable part of the protocol and needs nothing but
// net/http, so the wire contract stays a contract rather than a shared
// build dependency.
//
// The cost is real and worth naming: forge does not get compile-time
// checking of request and response shapes against the server's protos. A
// server-side field rename surfaces as a missing value at runtime rather
// than a build failure. That is the accepted trade for independence, and
// it is why Call decodes into explicitly-declared local structs — the
// shape forge expects is written down in one place instead of inferred.
type Client struct {
	Endpoint   Endpoint
	Credential Credential
	HTTP       *http.Client
}

// NewClient builds a Client with a bounded default timeout. A deploy CLI
// that hangs forever on an unreachable endpoint is worse than one that
// fails: CI would sit until its own job timeout with no diagnosis.
func NewClient(ep Endpoint, cred Credential) *Client {
	return &Client{
		Endpoint:   ep,
		Credential: cred,
		HTTP:       &http.Client{Timeout: 30 * time.Second},
	}
}

// Call invokes one Connect procedure. procedure is the fully-qualified
// path WITHOUT a leading slash, e.g.
// "controlplane.v1.DeployService/ListReleases".
//
// req is marshalled to JSON and out is unmarshalled from the reply. A
// non-200 is converted by connectError, which preserves the server's own
// message — a bare status code would throw away the only part of the
// response that says what went wrong.
func (c *Client) Call(ctx context.Context, procedure string, req, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode %s request: %w", procedure, err)
	}

	url := c.Endpoint.URL + "/" + strings.TrimPrefix(procedure, "/")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build %s request: %w", procedure, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// The credential is an OPAQUE bearer string. forge does not inspect
	// it, does not validate a prefix, and does not decode it — the issuer
	// owns its format, and a forge-side assumption about it would break
	// the day that format changed.
	httpReq.Header.Set("Authorization", "Bearer "+c.Credential.Token)
	if c.Endpoint.Organization != "" {
		httpReq.Header.Set("X-Forge-Organization", c.Endpoint.Organization)
	}

	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return fmt.Errorf("call %s at %s: %w", procedure, c.Endpoint.URL, err)
	}
	defer resp.Body.Close()

	// Bounded read: a misconfigured endpoint that returns a huge HTML
	// error page should not be streamed into memory in full.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read %s response: %w", procedure, err)
	}
	if resp.StatusCode != http.StatusOK {
		return c.connectError(procedure, resp.StatusCode, raw)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s response: %w", procedure, err)
	}
	return nil
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// connectError renders a failed call as an actionable message.
//
// An auth failure gets special handling because a bare 401 is the least
// useful thing forge could print: the user cannot tell whether they have
// no credential, a wrong one, or the right one for a different
// environment. Naming the endpoint, the source of the credential forge
// actually used, and both ways to supply a better one turns it into a
// message that can be acted on without a second run.
func (c *Client) connectError(procedure string, status int, raw []byte) error {
	var envelope struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &envelope)
	detail := strings.TrimSpace(envelope.Message)
	if detail == "" {
		detail = strings.TrimSpace(string(raw))
	}
	if len(detail) > 500 {
		detail = detail[:500] + "…"
	}

	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf(
			"%s rejected the credential (HTTP %d%s)\n"+
				"  endpoint:   %s   (declared by env %q)\n"+
				"  credential: from %s\n"+
				"fix: forge login            (human — opens a browser)\n"+
				"     export %s=<token>      (CI — a pipeline has no browser)%s",
			procedure, status, codeSuffix(envelope.Code), c.Endpoint.URL, c.Endpoint.Env,
			c.Credential.From, c.Endpoint.TokenEnv, detailSuffix(detail))
	case http.StatusNotImplemented:
		return fmt.Errorf("%s is not implemented by %s%s", procedure, c.Endpoint.URL, detailSuffix(detail))
	default:
		return fmt.Errorf("%s failed (HTTP %d%s) against %s%s",
			procedure, status, codeSuffix(envelope.Code), c.Endpoint.URL, detailSuffix(detail))
	}
}

func codeSuffix(code string) string {
	if code == "" {
		return ""
	}
	return ", " + code
}

func detailSuffix(detail string) string {
	if detail == "" {
		return ""
	}
	return "\n  server said: " + detail
}
