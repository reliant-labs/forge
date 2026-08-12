package devidp

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

// Client talks to a running Zitadel instance.
//
// ONE base URL, unlike a provider that splits its admin API onto a second
// port: Zitadel serves the OIDC endpoints, the resource APIs (`/v2/...`)
// and the management API (`/management/v1/...`) on a single port, and
// authorizes all of them with the same bearer token.
type Client struct {
	// Base is the address this process DIALS. Inside a deploy-time Job
	// that is the in-network address (a compose service name or a
	// cluster-local Service DNS name), which is not necessarily the
	// BROWSER-facing origin — see Host.
	Base string

	// Host overrides the HTTP Host header, for when the address this
	// process dials differs from the address the instance is REGISTERED
	// under (Base above). Empty sends the Host implied by Base.
	//
	// Zitadel is multi-tenant and resolves which instance a request
	// belongs to from the Host header, matched against the instance's
	// ExternalDomain — not from the socket the request arrived on. A Job
	// running inside the compose network dials "idp:8080", but the dev
	// instance is registered under "localhost" (the browser's origin), so
	// every call needs this override or Zitadel answers "Instance not
	// found" with an otherwise perfectly valid, correctly authenticated
	// request.
	Host string

	// Token is the personal access token of the service account declared
	// in idp-steps.yaml, which Zitadel writes to disk on first boot.
	Token string

	// OrgID scopes management calls to one organization. Resolved by
	// DefaultOrg when empty.
	OrgID string

	http *http.Client
}

// requestTimeout bounds a single API call. Generous for a local container,
// short enough that an unreachable IdP fails rather than hangs — WaitReady
// owns the "not up yet" case and reports it properly.
const requestTimeout = 15 * time.Second

// httpClient returns the HTTP client, building the default on first use.
func (c *Client) httpClient() *http.Client {
	if c.http == nil {
		c.http = &http.Client{Timeout: requestTimeout}
	}
	return c.http
}

// Org is a Zitadel organization — the container a project and its users
// live in.
type Org struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// DefaultOrg returns the instance's first organization, which for a
// forge-scaffolded instance is the one idp-steps.yaml declared.
//
// It is looked up rather than pinned because the ID is generated: the NAME
// is declarative (it is in idp-steps.yaml), the snowflake ID it gets is
// not.
func (c *Client) DefaultOrg(ctx context.Context) (Org, error) {
	body, err := c.postJSON(ctx, "/v2/organizations/_search", map[string]any{})
	if err != nil {
		return Org{}, fmt.Errorf("search organizations: %w", err)
	}
	var out struct {
		Result []Org `json:"result"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return Org{}, fmt.Errorf("decode organizations: %w", err)
	}
	if len(out.Result) == 0 {
		return Org{}, fmt.Errorf("the instance reported no organizations")
	}
	return out.Result[0], nil
}

// Project groups applications inside an organization. In Zitadel, a
// token's `aud` is the project id, which is why EnsureProject's return
// value is also the AUDIENCE the backend must enforce.
type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// EnsureProject creates the project, or returns the existing one with the
// same name.
func (c *Client) EnsureProject(ctx context.Context, name string) (Project, error) {
	existing, err := c.listProjects(ctx)
	if err != nil {
		return Project{}, err
	}
	for _, p := range existing {
		if p.Name == name {
			return p, nil
		}
	}
	body, err := c.postJSON(ctx, "/management/v1/projects", map[string]any{"name": name})
	if err != nil {
		return Project{}, fmt.Errorf("create project %q: %w", name, err)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return Project{}, fmt.Errorf("decode project: %w", err)
	}
	return Project{ID: out.ID, Name: name}, nil
}

func (c *Client) listProjects(ctx context.Context) ([]Project, error) {
	body, err := c.postJSON(ctx, "/management/v1/projects/_search", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("search projects: %w", err)
	}
	var out struct {
		Result []Project `json:"result"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode projects: %w", err)
	}
	return out.Result, nil
}

// Application is a registered OAuth client.
type Application struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// ClientID is what the browser sends. Zitadel GENERATES it, which is
	// the reason this package exists at all — everything else about the
	// dev IdP is declared in idp-steps.yaml.
	ClientID string `json:"clientId"`
}

// spaOIDCConfig is the OIDC configuration for a browser single-page app.
//
// Two fields are load-bearing and easy to get wrong by hand:
//
//   - accessTokenType JWT. The DEFAULT is `OIDC_TOKEN_TYPE_BEARER`, which
//     mints an OPAQUE access token — a random string, not a JWT. It will
//     never validate against a JWKS however correct the rest of the wiring
//     is, and sign-in itself looks completely successful, so the failure
//     surfaces as every API call 401ing with "token contains an invalid
//     number of segments".
//   - authMethodType NONE. A single-page app is a PUBLIC client: it holds
//     no secret, and PKCE's per-attempt verifier is what replaces one.
//
// devMode does two things, and the second one is what lets a dev frontend
// keep an ephemeral port.
//
//   - It relaxes the redirect-URI rules enough to allow the plain-http
//     localhost callback a dev loop needs.
//   - It turns every registered redirect URI into a GLOB. Zitadel's client
//     converter returns its redirect URIs from `RedirectURIGlobs()` (and its
//     post-logout URIs from `PostLogoutRedirectURIGlobs()`) exactly when the
//     app is in devMode, and zitadel/oidc's checkURIAgainstRedirects falls
//     through from its exact-match test to matching those with doublestar.
//     So "http://localhost:*/<base_path>/auth/callback" (see RedirectGlob)
//     accepts the callback on whatever port the kernel handed this run's
//     frontend.
//
// WILDCARD REDIRECTS ARE OFF-SPEC, AND THAT IS WHY THEY ARE FENCED IN HERE.
// The upstream interface carries an explicit warning: globbing is not
// permitted by the OIDC standard, implementing it has security implications,
// and it should be offered only in rare cases such as a client with DevMode
// enabled. The registered-URI allow-list is a real security control — it is
// what stops an authorization code being redirected to a host an attacker
// owns — so widening it is not a free ergonomic.
//
// It is acceptable at exactly this one call site because of what this
// function configures: the IdP forge scaffolds into docker-compose.yml,
// bound to 127.0.0.1, holding accounts that exist only on a developer's
// machine. The pattern registered widens only the PORT — a single `*`
// does not cross a `/`, so another host and another path are both still
// refused. Nothing about this reaches a real issuer: a project that points
// at one registers its own application there, with its own literal URIs,
// and this code never runs against it (the idp-provision job is scaffolded
// only for the dev environment).
func spaOIDCConfig(redirectURIs, postLogoutURIs []string) map[string]any {
	return map[string]any{
		"redirectUris":             redirectURIs,
		"postLogoutRedirectUris":   postLogoutURIs,
		"responseTypes":            []string{"OIDC_RESPONSE_TYPE_CODE"},
		"grantTypes":               []string{"OIDC_GRANT_TYPE_AUTHORIZATION_CODE", "OIDC_GRANT_TYPE_REFRESH_TOKEN"},
		"appType":                  "OIDC_APP_TYPE_USER_AGENT",
		"authMethodType":           "OIDC_AUTH_METHOD_TYPE_NONE",
		"accessTokenType":          "OIDC_TOKEN_TYPE_JWT",
		"accessTokenRoleAssertion": true,
		"devMode":                  true,
	}
}

// EnsureSPAApplication creates the browser client, or adopts the existing
// one with the same name (refreshing its redirect URIs so a changed
// declaration does not silently break the callback).
//
// The URIs may be GLOB PATTERNS (see RedirectGlob) — this app is always in
// devMode, where Zitadel matches redirect URIs as globs rather than by
// equality. That is what keeps a dev frontend's port kernel-assigned
// instead of pinned; see spaOIDCConfig for the mechanism and for why
// widening an allow-list is defensible only for this dev IdP.
func (c *Client) EnsureSPAApplication(ctx context.Context, projectID, name string, redirectURIs, postLogoutURIs []string) (Application, error) {
	cfg := spaOIDCConfig(redirectURIs, postLogoutURIs)

	existing, err := c.listApplications(ctx, projectID)
	if err != nil {
		return Application{}, err
	}
	for _, a := range existing {
		if a.Name != name {
			continue
		}
		// Re-running must converge the app to the declared shape, not
		// just leave whatever is there: the declared redirect URI may
		// have changed, and an app created before this version may
		// still be registered with a port-bearing literal URI, or be
		// minting opaque tokens.
		//
		// Already-converged is SUCCESS, not an error. Zitadel rejects a
		// no-op update with "No changes", which is the normal answer on
		// the second run of an idempotent command — reporting it as a
		// failure would make re-running the documented recovery step
		// look broken.
		if _, err := c.putJSON(ctx,
			fmt.Sprintf("/management/v1/projects/%s/apps/%s/oidc_config", projectID, a.ID), cfg); err != nil {
			if !isNoChangesErr(err) {
				return Application{}, fmt.Errorf("update application %q: %w", name, err)
			}
		}
		return a, nil
	}

	cfg["name"] = name
	body, err := c.postJSON(ctx, "/management/v1/projects/"+projectID+"/apps/oidc", cfg)
	if err != nil {
		return Application{}, fmt.Errorf("create application %q: %w", name, err)
	}
	var out struct {
		AppID    string `json:"appId"`
		ClientID string `json:"clientId"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return Application{}, fmt.Errorf("decode application: %w", err)
	}
	if out.ClientID == "" {
		return Application{}, fmt.Errorf("the created application carried no clientId")
	}
	return Application{ID: out.AppID, Name: name, ClientID: out.ClientID}, nil
}

func (c *Client) listApplications(ctx context.Context, projectID string) ([]Application, error) {
	body, err := c.postJSON(ctx, "/management/v1/projects/"+projectID+"/apps/_search", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("search applications: %w", err)
	}
	var out struct {
		Result []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			OIDCConfig struct {
				ClientID string `json:"clientId"`
			} `json:"oidcConfig"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode applications: %w", err)
	}
	apps := make([]Application, 0, len(out.Result))
	for _, a := range out.Result {
		apps = append(apps, Application{ID: a.ID, Name: a.Name, ClientID: a.OIDCConfig.ClientID})
	}
	return apps, nil
}

// jwksPath is where Zitadel serves its signing keys. It is NOT under
// /.well-known/, and not the OIDC-conventional /oidc/jwks either.
const jwksPath = "/oauth/v2/keys"

// Issuer is the `iss` the API server must enforce, and JWKSURL is where it
// fetches signing keys — both derived from the SAME browser-facing origin
// this project's dev IdP is registered under (forge's dev loop runs the
// backend as a host process reaching the IdP at that same address; see
// the `auth/dev-loop` skill for why an in-network address does not work
// here). Zitadel's issuer is the bare origin, no path suffix.
func Issuer(browserOrigin string) string {
	return strings.TrimRight(browserOrigin, "/")
}

func JWKSURL(browserOrigin string) string {
	return strings.TrimRight(browserOrigin, "/") + jwksPath
}

// WaitReady blocks until the IdP answers its readiness probe, or ctx
// expires.
//
// /debug/ready is Zitadel's own unauthenticated readiness route, and it
// reports READY rather than merely listening — which matters here because
// the first boot runs the declarative setup (schema, instance, org, admin
// user, PAT) before serving, and a probe that only checked for an open
// socket would hand back a client that 404s every call.
func (c *Client) WaitReady(ctx context.Context) error {
	probe := strings.TrimRight(c.Base, "/") + "/debug/ready"
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, probe, http.NoBody)
		if err != nil {
			return err
		}
		resp, err := c.httpClient().Do(c.withHost(req))
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode < 300 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("identity provider not ready at %s: %w", c.Base, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

func (c *Client) postJSON(ctx context.Context, path string, payload any) ([]byte, error) {
	return c.sendJSON(ctx, http.MethodPost, path, payload)
}

func (c *Client) putJSON(ctx context.Context, path string, payload any) ([]byte, error) {
	return c.sendJSON(ctx, http.MethodPut, path, payload)
}

func (c *Client) sendJSON(ctx context.Context, method, path string, payload any) ([]byte, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method,
		strings.TrimRight(c.Base, "/")+path, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(c.withHost(c.authorize(req)))
}

func (c *Client) authorize(req *http.Request) *http.Request {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	// The management API is org-scoped; without this header a call lands
	// on whichever org the credential's own user belongs to.
	if c.OrgID != "" {
		req.Header.Set("x-zitadel-orgid", c.OrgID)
	}
	return req
}

// withHost applies the Host override. Set on req.Host rather than via
// Header.Set("Host", …), which net/http ignores for the request line.
func (c *Client) withHost(req *http.Request) *http.Request {
	if c.Host != "" {
		req.Host = c.Host
	}
	return req
}

// do performs the request and returns the body, converting a non-2xx into
// an error that carries the provider's own message — the API reports
// precisely what it rejected, and paraphrasing it loses the only useful
// detail.
//
// Zitadel answers some errors 200-with-an-error-body over its gRPC-gateway
// (a `code` field and a `message`), so a status check alone is not enough
// to tell success from failure.
func (c *Client) do(req *http.Request) ([]byte, error) {
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: HTTP %d: %s",
			req.Method, req.URL.Path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if readErr != nil {
		return nil, readErr
	}
	if err := gatewayError(body); err != nil {
		return nil, fmt.Errorf("%s %s: %w", req.Method, req.URL.Path, err)
	}
	return body, nil
}

// noChangesID is the error Zitadel returns when an update would leave the
// resource exactly as it is. Matched on the stable error ID rather than the
// human-readable message, which is localized.
const noChangesID = "COMMAND-1m88i"

// isNoChangesErr reports whether err is Zitadel's "the resource already has
// these values" refusal — the expected answer when an idempotent command
// runs a second time and everything already agrees.
func isNoChangesErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), noChangesID)
}

// gatewayError reports the error in a 2xx body that is actually a gRPC
// status, and nil for a normal response.
func gatewayError(body []byte) error {
	var probe struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil // not a JSON object; the caller's own decode will judge it
	}
	if probe.Code == nil || *probe.Code == 0 || probe.Message == "" {
		return nil
	}
	return fmt.Errorf("%s (code %d)", probe.Message, *probe.Code)
}
