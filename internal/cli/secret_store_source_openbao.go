package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/reliant-labs/forge/pkg/secretaccess"
)

// The self-run OpenBaoStore writer (design decision N2, option a).
//
// `forge secret set` against an env that runs its own OpenBao needs a
// credential that can write the env's values and nothing else, from a person
// who holds only a kubeconfig. The path:
//
//  1. mint a ten-minute token for the forge-rendered `forge-secret-writer`
//     service account with the TokenRequest API, using the operator's kube
//     context, for the audience the render's `openbao` block names;
//  2. log in to the store's own JWT mount (the block's auth_mount) as the
//     writer role. The OpenBao component's converge Job creates that role
//     (pkg/deploy/openbao, RoleWriter): bound to the account and the audience,
//     with a policy that can create and update, never read, under the env's
//     directory and read its metadata (versions, never values). The CLI
//     derives none of that: it logs in to what the render says exists;
//  3. write with the batch token that login returns. A create-only write
//     (import) sends check-and-set 0.
//
// Nothing is stored: the kubeconfig is the trust root, the token lives for the
// command, and OpenBao's audit device records every write.
//
// The store is reached through the API server's service proxy, so the
// operator needs `services/proxy` on the OpenBao Service and no network path
// to the pod. The API server does not verify the backend's certificate on that
// hop; the request carries only a ten-minute token that can write values, so a
// party able to impersonate the backend inside the cluster's network could
// capture a write it was already positioned to observe. The component's TLS
// protects every other client.

// writerRole is the login role of the forge-secret-writer service account, the
// name pkg/deploy/openbao creates on the store's own mount (openbao.RoleWriter).
// It is restated here, not imported: the CLI logs in to a role the component's
// converge Job made, and a test pins the two names together.
const writerRole = "forge-secret-writer"

// writerServiceAccount is the account the component renders for the writer
// (forge.OpenBaoSAWriter's default). The render's `openbao` block does not
// carry it, so a non-default account is a known gap (see deviations).
const writerServiceAccount = "forge-secret-writer"

// OpenBaoEntity is the rendered `openbao` block (kcl/lib/openbao.k): where the
// store is and how a client logs in to it. Declarations only; no value.
type OpenBaoEntity struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Image     string `json:"image,omitempty"`
	// Cluster is the kube context the store runs in; ConnectedCluster is the
	// name `forge cluster connect` registered.
	Cluster          string `json:"cluster,omitempty"`
	ConnectedCluster string `json:"connected_cluster,omitempty"`
	// ServerURL is the in-cluster address (https://<name>.<ns>.svc:<port>).
	ServerURL   string `json:"server_url,omitempty"`
	CAConfigMap string `json:"ca_configmap,omitempty"`
	KVMount     string `json:"kv_mount,omitempty"`
	// SelfCluster is the store's own cluster id: its auth mount is jwt-<id>.
	SelfCluster string `json:"self_cluster,omitempty"`
	AuthMount   string `json:"auth_mount,omitempty"`
	// Audience is what the cluster's tokens must carry to log in.
	Audience       string `json:"audience,omitempty"`
	ServiceAccount string `json:"service_account,omitempty"`
	Org            string `json:"org,omitempty"`
	EnvDir         string `json:"env,omitempty"`
	// BootstrapNames are the BootstrapSecret names the component declares that
	// a person may import (the seal key and the Postgres password).
	BootstrapNames []string `json:"bootstrap_names,omitempty"`
}

// BootstrapSourceEntity is the rendered Bundle.bootstrap_source.
type BootstrapSourceEntity struct {
	Type    string `json:"type"`
	Project string `json:"project,omitempty"`
}

// apiPort is the port of ServerURL (https://<name>.<ns>.svc:<port>).
func (o *OpenBaoEntity) apiPort() int {
	if u, err := url.Parse(o.ServerURL); err == nil {
		if p, err := strconv.Atoi(u.Port()); err == nil && p > 0 {
			return p
		}
	}
	return 8200
}

// baoTransport sends one OpenBao API request. The payload and the response are
// JSON objects; the response may be nil (204).
type baoTransport interface {
	do(ctx context.Context, method, path string, query url.Values, token string, payload any) (status int, body map[string]any, err error)
}

// kubeProxyTransport reaches the store through the API server's service proxy.
type kubeProxyTransport struct {
	kube      kubeAccess
	namespace string
	service   string
	port      int
}

func (t kubeProxyTransport) do(ctx context.Context, method, path string, query url.Values, token string, payload any) (int, map[string]any, error) {
	var raw []byte
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		raw = b
	}
	headers := map[string]string{"Content-Type": "application/json"}
	if token != "" {
		headers["X-Vault-Token"] = token
	}
	status, body, err := t.kube.ServiceProxy(ctx, serviceProxyRequest{
		Namespace: t.namespace, Scheme: "https", Service: t.service, Port: t.port,
		Method: method, Path: "v1/" + path, Query: query, Headers: headers, Body: raw,
	})
	if err != nil {
		return 0, nil, err
	}
	var out map[string]any
	if len(strings.TrimSpace(string(body))) > 0 {
		// A proxy error page is not JSON; the status carries the answer then.
		_ = json.Unmarshal(body, &out)
	}
	return status, out, nil
}

// baoStoreSource is the self-run store, signed in as the writer.
type baoStoreSource struct {
	tr    baoTransport
	scope secretaccess.Scope
	where string

	login baoWriterLogin
	kube  kubeAccess
	token string
}

// baoWriterLogin is everything a client needs to sign in as the writer.
type baoWriterLogin struct {
	LoginPath      string // auth/<auth_mount>/login
	Role           string
	Audience       string
	Namespace      string // of the writer's service account (the store's)
	ServiceAccount string
}

func (s *baoStoreSource) describe() string { return s.where }

// signIn exchanges a freshly minted service-account token for a batch token.
func (s *baoStoreSource) signIn(ctx context.Context) error {
	if s.token != "" {
		return nil
	}
	sa, err := s.kube.MintToken(ctx, s.login.Namespace, s.login.ServiceAccount, s.login.Audience, tokenTTL)
	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("%w\nfix: the writer service account is rendered with the OpenBao component; run `forge env deploy` for this env first", err)
		}
		return err
	}
	status, body, err := s.tr.do(ctx, http.MethodPut, s.login.LoginPath, nil, "", map[string]any{"role": s.login.Role, "jwt": sa})
	if err != nil {
		return redactValue(err, sa)
	}
	if status != http.StatusOK {
		return fmt.Errorf("sign in to OpenBao as the writer role %q failed (HTTP %d%s)\n"+
			"fix: the role is created by the OpenBao component's converge Job; check it ran for this env, and that this cluster is registered (forge cluster connect)",
			s.login.Role, status, baoErrors(body))
	}
	tok := dig(body, "auth", "client_token")
	if tok == "" {
		return errors.New("OpenBao answered the writer login without a token")
	}
	s.token = tok
	return nil
}

func (s *baoStoreSource) put(ctx context.Context, name, value string, createOnly bool) (uint32, error) {
	if err := s.signIn(ctx); err != nil {
		return 0, err
	}
	// The value is a JSON string under "value": OpenBao's audit log HMACs
	// strings and passes numbers through in the clear, so it is never coerced
	// to anything else (the same rule the hosted store enforces).
	payload := map[string]any{"data": map[string]any{"value": value}}
	if createOnly {
		payload["options"] = map[string]any{"cas": 0}
	}
	status, body, err := s.tr.do(ctx, http.MethodPut, s.scope.SecretPath(name), nil, s.token, payload)
	if err != nil {
		return 0, redactValue(err, value)
	}
	switch {
	case status == http.StatusOK:
		return uint32(digNum(body, "data", "version")), nil
	case createOnly && status == http.StatusBadRequest && strings.Contains(strings.Join(baoErrorList(body), " "), "check-and-set"):
		return 0, errStoreValueExists
	case status == http.StatusForbidden:
		return 0, redactValue(fmt.Errorf("OpenBao refused the write (HTTP 403): the writer policy does not cover this path%s", baoErrors(body)), value)
	}
	return 0, redactValue(fmt.Errorf("OpenBao write failed (HTTP %d%s)", status, baoErrors(body)), value)
}

func (s *baoStoreSource) presence(ctx context.Context, names []string) (map[string]uint32, error) {
	if err := s.signIn(ctx); err != nil {
		return nil, err
	}
	versions := map[string]uint32{}
	for _, n := range names {
		status, body, err := s.tr.do(ctx, http.MethodGet, s.metadataPath(n), nil, s.token, nil)
		if err != nil {
			return nil, err
		}
		switch status {
		case http.StatusOK:
			if v, ok := currentUsableVersion(body); ok {
				versions[n] = v
			}
		case http.StatusNotFound:
		default:
			return nil, fmt.Errorf("read metadata of %s: OpenBao answered HTTP %d%s", n, status, baoErrors(body))
		}
	}
	return versions, nil
}

// metadataPath is the KV-v2 metadata path of a name. Metadata carries version
// numbers and timestamps, never a value.
func (s *baoStoreSource) metadataPath(name string) string {
	return strings.Replace(s.scope.SecretPath(name), "/data/", "/metadata/", 1)
}

// currentUsableVersion reads a metadata document: the current version, unless
// it was deleted or destroyed.
func currentUsableVersion(body map[string]any) (uint32, bool) {
	data, _ := body["data"].(map[string]any)
	cur := uint32(digNum(body, "data", "current_version"))
	if cur == 0 {
		return 0, false
	}
	versions, _ := data["versions"].(map[string]any)
	if v, ok := versions[fmt.Sprint(cur)].(map[string]any); ok {
		if d, _ := v["deletion_time"].(string); d != "" {
			return 0, false
		}
		if destroyed, _ := v["destroyed"].(bool); destroyed {
			return 0, false
		}
	}
	return cur, true
}

// openBaoStoreSource builds the self-run store for the env: the OpenBao the
// bundle declares, reached through the kube context it runs in.
func openBaoStoreSource(ctx context.Context, envName string, e *KCLEntities, o storeOptions) (storeSource, error) {
	return openBaoStoreSourceWith(envName, e, o, func(kctx string) (kubeAccess, baoTransport, error) {
		kube, err := newKubeAccess(kctx)
		if err != nil {
			return nil, nil, err
		}
		bao := e.OpenBao
		return kube, kubeProxyTransport{kube: kube, namespace: bao.Namespace, service: bao.Name, port: bao.apiPort()}, nil
	})
}

// openBaoStoreSourceWith is the construction with the transport injected: the
// checks before any cluster is touched are production's, whoever supplies it.
func openBaoStoreSourceWith(envName string, e *KCLEntities, o storeOptions, connect func(kubeContext string) (kubeAccess, baoTransport, error)) (storeSource, error) {
	bao := e.OpenBao
	if bao == nil {
		return nil, fmt.Errorf("env %q reads from an OpenBaoStore but declares no Bundle.openbao\nfix: declare `openbao = forge.OpenBao {}` in its KCL", envName)
	}
	kctx := o.KubeContext
	if kctx == "" {
		kctx = bao.Cluster
	}
	if kctx == "" && e.ClusterTarget != nil {
		kctx = e.ClusterTarget.Cluster
	}
	kube, tr, err := connect(kctx)
	if err != nil {
		return nil, err
	}
	return newOpenBaoSource(e, bao, kube, tr, kctx)
}

// newOpenBaoSource resolves where the env's values live and how to sign in,
// from the render's `openbao` block. The directory is the block's (org, env),
// not the CLI's --env: the component wrote its policy for those.
func newOpenBaoSource(e *KCLEntities, bao *OpenBaoEntity, kube kubeAccess, tr baoTransport, kubeContext string) (*baoStoreSource, error) {
	org, env := bao.Org, bao.EnvDir
	if org == "" {
		org = e.Project
	}
	if org == "" || env == "" || bao.AuthMount == "" || bao.Audience == "" {
		return nil, errors.New("the env's render carries an incomplete `openbao` block (org, env, auth_mount and audience are required to sign in)\n" +
			"fix: upgrade forge so the render describes the store, then re-run")
	}
	scope := secretaccess.Scope{Mount: bao.KVMount, Org: org, Env: env}
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("the OpenBao directory in the render is not usable: %w", err)
	}
	return &baoStoreSource{
		tr: tr, scope: scope, kube: kube,
		login: baoWriterLogin{
			LoginPath: "auth/" + bao.AuthMount + "/login", Role: writerRole, Audience: bao.Audience,
			Namespace: bao.Namespace, ServiceAccount: writerServiceAccount,
		},
		where: fmt.Sprintf("self-run OpenBao %s/%s (%s)", bao.Namespace, bao.Name, kubeContext),
	}, nil
}

// tokenTTL is the lifetime of the writer's service-account token and of the
// batch token it buys (both ten minutes, the component's role maximum).
const tokenTTL = 10 * time.Minute

func isNotFound(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "NotFound"))
}

// dig reads a nested string from a decoded JSON object.
func dig(m map[string]any, path ...string) string {
	var cur any = m
	for _, p := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = obj[p]
	}
	s, _ := cur.(string)
	return s
}

func digNum(m map[string]any, path ...string) float64 {
	var cur any = m
	for _, p := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return 0
		}
		cur = obj[p]
	}
	f, _ := cur.(float64)
	return f
}

func baoErrorList(body map[string]any) []string {
	raw, _ := body["errors"].([]any)
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		if str, ok := r.(string); ok {
			out = append(out, str)
		}
	}
	return out
}

// baoErrors renders OpenBao's error list for a message. These strings name
// paths and policies, never request bodies.
func baoErrors(body map[string]any) string {
	if l := baoErrorList(body); len(l) > 0 {
		return ": " + strings.Join(l, "; ")
	}
	return ""
}
