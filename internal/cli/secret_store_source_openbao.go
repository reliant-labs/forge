package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

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
//     context, for the writer audience of this cluster;
//  2. log in to the cluster's JWT mount with it, as the writer role. That role
//     (secretaccess.DeriveWriter) is bound to the account and the audience and
//     carries a policy that can create and update, never read, under the env's
//     directory, and read metadata (versions, never values);
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

// OpenBaoEntity is the rendered Bundle.openbao: what the CLI needs to reach a
// self-run store. Fields the render omits take the schema's defaults.
type OpenBaoEntity struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Cluster is the kube context the store runs in; ConnectedCluster is the
	// name `forge cluster connect` registered. The cluster's auth mount id is
	// the connected name when there is one, else the context.
	Cluster          string `json:"cluster,omitempty"`
	ConnectedCluster string `json:"connected_cluster,omitempty"`
	// Service is the Kubernetes Service that fronts the API. Defaults to Name.
	Service   string `json:"service,omitempty"`
	Listeners []struct {
		Port int   `json:"port"`
		TLS  *bool `json:"tls,omitempty"`
	} `json:"listeners,omitempty"`
	KV struct {
		Mount string `json:"mount,omitempty"`
	} `json:"kv,omitempty"`
	Writer struct {
		Type           string `json:"type,omitempty"`
		ServiceAccount string `json:"service_account,omitempty"`
		Role           string `json:"role,omitempty"`
	} `json:"writer,omitempty"`
	Seal struct {
		Key struct {
			Name string `json:"name"`
		} `json:"key"`
	} `json:"seal,omitempty"`
	Storage struct {
		Type     string `json:"type"`
		Password *struct {
			Name string `json:"name"`
		} `json:"password,omitempty"`
	} `json:"storage,omitempty"`
	Recovery struct {
		Escrow struct {
			Name string `json:"name"`
		} `json:"escrow"`
	} `json:"recovery,omitempty"`
}

// BootstrapSourceEntity is the rendered Bundle.bootstrap_source.
type BootstrapSourceEntity struct {
	Type    string `json:"type"`
	Project string `json:"project,omitempty"`
}

func (o *OpenBaoEntity) service() string {
	if o.Service != "" {
		return o.Service
	}
	if o.Name != "" {
		return o.Name
	}
	return "openbao"
}

func (o *OpenBaoEntity) namespace() string {
	if o.Namespace != "" {
		return o.Namespace
	}
	return "openbao"
}

// apiPort is the first TLS listener's port (the schema requires one).
func (o *OpenBaoEntity) apiPort() int {
	for _, l := range o.Listeners {
		if l.TLS == nil || *l.TLS {
			return l.Port
		}
	}
	return 8200
}

func (o *OpenBaoEntity) clusterID() string {
	if o.ConnectedCluster != "" {
		return o.ConnectedCluster
	}
	return o.Cluster
}

// bootstrapNames lists the BootstrapSecret names the component declares that a
// person may import: the seal key and the Postgres password. The recovery
// escrow is excluded because the converge Job writes it from `bao operator
// init` and nothing is ever imported into it.
func (o *OpenBaoEntity) bootstrapNames() []string {
	var out []string
	if n := o.Seal.Key.Name; n != "" {
		out = append(out, n)
	}
	if o.Storage.Password != nil && o.Storage.Password.Name != "" {
		out = append(out, o.Storage.Password.Name)
	}
	sort.Strings(out)
	return out
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

	login secretaccess.WriterLogin
	kube  kubeAccess
	token string
}

func (s *baoStoreSource) describe() string { return s.where }

// signIn exchanges a freshly minted service-account token for a batch token.
func (s *baoStoreSource) signIn(ctx context.Context) error {
	if s.token != "" {
		return nil
	}
	sa, err := s.kube.MintToken(ctx, s.login.Namespace, s.login.ServiceAccount, s.login.Audience, secretaccess.TokenTTL)
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
		return kube, kubeProxyTransport{kube: kube, namespace: bao.namespace(), service: bao.service(), port: bao.apiPort()}, nil
	})
}

// openBaoStoreSourceWith is the construction with the transport injected: the
// checks before any cluster is touched are production's, whoever supplies it.
func openBaoStoreSourceWith(envName string, e *KCLEntities, o storeOptions, connect func(kubeContext string) (kubeAccess, baoTransport, error)) (storeSource, error) {
	bao := e.OpenBao
	if bao == nil {
		return nil, fmt.Errorf("env %q reads from an OpenBaoStore but declares no Bundle.openbao\nfix: declare `openbao = forge.OpenBao {}` in its KCL", envName)
	}
	if bao.Writer.Type == "oidc" {
		return nil, errors.New("this env's OpenBao writer is forge.OpenBaoOIDCWriter, which `forge secret` does not sign in with yet\n" +
			"fix: switch the writer to the default (forge.OpenBaoSAWriter {}), which needs only cluster access")
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
	return newOpenBaoSource(envName, e, bao, kube, tr, kctx)
}

func newOpenBaoSource(envName string, e *KCLEntities, bao *OpenBaoEntity, kube kubeAccess, tr baoTransport, kubeContext string) (*baoStoreSource, error) {
	scope, err := baoScope(e, envName, bao.KV.Mount)
	if err != nil {
		return nil, err
	}
	clusterID := bao.clusterID()
	if clusterID == "" {
		clusterID = kubeContext
	}
	sa := bao.Writer.ServiceAccount
	login, err := secretaccess.WriterRequest{
		Scope:          scope,
		Cluster:        secretaccess.Cluster{ID: clusterID},
		Namespace:      bao.namespace(),
		ServiceAccount: sa,
	}.Login()
	if err != nil {
		return nil, fmt.Errorf("derive the OpenBao writer login: %w", err)
	}
	return &baoStoreSource{
		tr: tr, scope: scope, login: login, kube: kube,
		where: fmt.Sprintf("self-run OpenBao %s/%s (%s)", bao.namespace(), bao.service(), kubeContext),
	}, nil
}

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
