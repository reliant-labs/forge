package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/pkg/activity"
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

	// elevated holds tokens obtained by ExchangeToken, keyed by the scope that
	// was missing. In memory only: they live at most an hour and are never
	// written to the credentials file.
	elevated elevationCache

	// credMu guards Credential, which a refresh from the credential helper
	// replaces.
	credMu        sync.Mutex
	scopeMu       sync.Mutex
	verifiedToken string
	verifiedOrg   string
}

// NewClient builds a Client with a bounded default timeout. A deploy CLI
// that hangs forever on an unreachable endpoint is worse than one that
// fails: CI would sit until its own job timeout with no diagnosis.
func NewClient(ep Endpoint, cred Credential) *Client {
	if ep.OrgID == "" {
		ep.OrgID = cred.OrgID
	}
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
// non-200 becomes a *[Error] (see error.go), which carries the Connect code,
// the domain reason and the server's error details as DATA. Callers branch
// on those fields — never on the message text, which is display copy the
// server is free to improve.
func (c *Client) Call(ctx context.Context, procedure string, req, out any) error {
	err := c.call(ctx, procedure, req, out, c.credential().Token)
	if rejectedAsUnauthenticated(err) {
		// The credential itself was refused (expired, revoked, ...), before any
		// handler ran, so asking for a fresh one and repeating the call once
		// cannot double an effect.
		if fresh, ok := c.refreshedCredential(); ok {
			err = c.call(ctx, procedure, req, out, fresh.Token)
		}
	}
	scope, ok := c.elevationScope(err)
	if !ok {
		return err
	}
	token, elevErr := c.elevatedToken(ctx, scope)
	if elevErr != nil {
		return c.loginLacksScope(annotateElevation(err, scope, elevErr), scope)
	}
	err = c.call(ctx, procedure, req, out, token)
	if rejectedAsUnauthenticated(err) {
		// The elevated token we held was refused (revoked, or expired by a
		// skewed clock). Trade for a new one, once.
		if fresh, ferr := c.reelevatedToken(ctx, scope, token); ferr == nil && fresh != token {
			return c.call(ctx, procedure, req, out, fresh)
		}
	}
	return err
}

// rejectedAsUnauthenticated reports a 401: the bearer was refused, as opposed to
// what it may do (403) or an outage.
func rejectedAsUnauthenticated(err error) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && apiErr.Unauthenticated()
}

func (c *Client) credential() Credential {
	c.credMu.Lock()
	defer c.credMu.Unlock()
	return c.Credential
}

// call is one HTTP round trip with an explicit bearer.
func (c *Client) call(ctx context.Context, procedure string, req, out any, token string) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode %s request: %w", procedure, err)
	}
	if token != "" {
		org := c.Endpoint.OrgID
		credentialOrg := c.credential().OrgID
		if org != "" && credentialOrg != "" && credentialOrg != org {
			return errors.New("credential organization does not match selected organization")
		}
		if org == "" {
			org = credentialOrg
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(body, &fields)
		for _, key := range []string{"orgId", "org_id"} {
			if raw, ok := fields[key]; ok && string(raw) != "null" {
				var requested string
				if json.Unmarshal(raw, &requested) != nil {
					return errors.New("request organization must be a string")
				}
				if requested != "" && org != "" && requested != org {
					return errors.New("request organization does not match credential organization")
				}
				if org == "" {
					org = requested
				}
			}
		}
		if org != "" && procedure != ProcGetOrganization {
			if err := c.verifyOrganization(ctx, token, org); err != nil {
				return err
			}
		}
		if org != "" && orgScopedProcedure(procedure) {
			if fields == nil {
				fields = map[string]json.RawMessage{}
			}
			delete(fields, "org_id")
			fields["orgId"], _ = json.Marshal(org)
			body, err = json.Marshal(fields)
			if err != nil {
				return err
			}
		}
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
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}
	// Forge-Via names the client to the server's activity stream
	// (forge/pkg/activity): the same command reads "via CLI" from a
	// terminal and "via CI" from a pipeline.
	httpReq.Header.Set(activity.ViaHeader, via())

	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return fmt.Errorf("call %s at %s: %w", procedure, c.Endpoint.URL, err)
	}
	// Nothing was written, so a close error carries no data loss — it can
	// only report a connection this call is already finished with.
	defer func() { _ = resp.Body.Close() }()

	// Bounded read: a misconfigured endpoint that returns a huge HTML
	// error page should not be streamed into memory in full.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read %s response: %w", procedure, err)
	}
	if resp.StatusCode != http.StatusOK {
		return c.connectError(procedure, resp, raw)
	}
	if token != "" && procedure == ProcGetOrganization && c.Endpoint.OrgID != "" {
		if org, err := organizationFromReply(raw); err != nil || org != c.Endpoint.OrgID {
			return errors.New("control plane returned an organization different from the selected organization")
		}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s response: %w", procedure, err)
	}
	return nil
}

// orgScopedProcedure follows control-plane's M1a request-field contract. Keep
// machine-only exchange, unimplemented streams and local-session reporting out.
func orgScopedProcedure(procedure string) bool {
	procedure = strings.TrimPrefix(procedure, "/")
	switch procedure {
	case "controlplane.v1.AccessTokenService/ExchangeToken",
		"controlplane.v1.DeployService/StreamLogs",
		"controlplane.v1.DeployService/ReportLocalSession":
		return false
	}
	for _, service := range []string{"DeployService", "DomainService", "ClusterService", "BrowserIngestService", "AccessTokenService"} {
		if strings.HasPrefix(procedure, "controlplane.v1."+service+"/") {
			return true
		}
	}
	return false
}

// verifyOrganization checks opaque bearers against the authority before any
// scoped effect. Each rotated/elevated token must pass independently.
func (c *Client) verifyOrganization(ctx context.Context, token, org string) error {
	c.scopeMu.Lock()
	defer c.scopeMu.Unlock()
	if c.verifiedToken == token && c.verifiedOrg == org {
		return nil
	}
	var raw json.RawMessage
	if err := c.call(ctx, ProcGetOrganization, map[string]any{}, &raw, token); err != nil {
		return err
	}
	actual, err := organizationFromReply(raw)
	if err != nil {
		return err
	}
	if actual != org {
		return fmt.Errorf("credential organization %q does not match selected organization %q", actual, org)
	}
	c.verifiedToken, c.verifiedOrg = token, org
	return nil
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// via is the Forge-Via value for this process: "ci" under a CI runner, "cli"
// otherwise.
func via() string {
	if buildinfo.IsCI() {
		return "ci"
	}
	return "cli"
}
