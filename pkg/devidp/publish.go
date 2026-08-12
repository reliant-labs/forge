package devidp

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Identity is the convergence output: the values Zitadel GENERATED when
// the project and its browser application were registered, and that
// nothing else can declare up front (see the package doc comment).
type Identity struct {
	// ClientID is the SPA application's OAuth client id.
	ClientID string
	// Audience is the Zitadel project id — the token's `aud`, and the
	// value the backend must enforce as its own audience.
	Audience string
	// Issuer and JWKSURL are derived from the browser-facing origin, not
	// generated, but travel with the rest of the identity because every
	// consumer needs all four together.
	Issuer  string
	JWKSURL string
}

// AsMap projects Identity to the flat string map both publishers write —
// cluster ConfigMap data and the compose/dev KCL literal share these same
// four keys, so a consumer's declaration (a configMapKeyRef, or a `.`
// lookup into the committed KCL) names the identical key regardless of
// which target rendered it.
func (id Identity) AsMap() map[string]string {
	return map[string]string{
		"client_id": id.ClientID,
		"audience":  id.Audience,
		"issuer":    id.Issuer,
		"jwks_url":  id.JWKSURL,
	}
}

// Publisher converges Identity into wherever this environment's workloads
// read it from. See ConfigMapPublisher (cluster) and KCLFilePublisher
// (compose/dev) — exactly one of the two applies to any given run, decided
// by RunningInCluster.
type Publisher interface {
	Publish(ctx context.Context, id Identity) error
}

// RunningInCluster reports whether this process is running inside a
// Kubernetes pod, using the same signal controller-runtime's leader
// election uses to infer the lease namespace: the ServiceAccount
// namespace mount. It is what decides which Publisher a caller should
// use — a cluster Job writes a ConfigMap; a compose Job writes a
// committed KCL file — without either publisher needing to guess.
func RunningInCluster() bool {
	_, err := os.Stat(inClusterNamespaceFile)
	return err == nil
}

const (
	inClusterNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	inClusterTokenFile     = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	inClusterCACertFile    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// ConfigMapPublisher writes Identity into a named ConfigMap in the pod's
// own namespace, using the ServiceAccount token Kubernetes projects into
// every pod. It needs no external client library: the Kubernetes API is
// plain REST-over-TLS, and this job's whole interaction with it is one
// idempotent PATCH — the same "narrow REST client" shape as the Zitadel
// Client above, not a general-purpose Kubernetes client.
//
// A workload never reads the ConfigMap through this publisher — it
// references the ConfigMap BY NAME via configMapKeyRef, which is the
// entire point of the convergence design: the render never needs to learn
// the generated value, because the declaration names where to find it
// rather than what it is.
type ConfigMapPublisher struct {
	// Name is the ConfigMap this job publishes into. Chosen by the
	// project's own KCL (the same name a consumer's configMapKeyRef
	// names), never generated.
	Name string

	http *http.Client

	// Test-only overrides. Zero values select the real in-cluster paths
	// and the real API server; set by this package's own tests to point
	// at a fake server and fake ServiceAccount files instead of a live
	// cluster.
	apiServerBase string
	namespaceFile string
	tokenFile     string
	caCertFile    string
}

// apiServerBase is the well-known in-cluster address of the Kubernetes
// API server, resolved via the DNS name every cluster provides — not
// KUBERNETES_SERVICE_HOST/PORT, which some CNI configurations leave
// unset for a pod that never otherwise needs it.
const apiServerBase = "https://kubernetes.default.svc"

// Publish creates the ConfigMap if it does not exist, or PATCHes its
// `data` if it does. Idempotent: a second run against an already-current
// ConfigMap is a no-op PATCH, which is what "converges on every deploy"
// requires.
func (p *ConfigMapPublisher) Publish(ctx context.Context, id Identity) error {
	namespace, err := readFile(p.namespaceFileOrDefault())
	if err != nil {
		return fmt.Errorf("read pod namespace: %w", err)
	}
	token, err := readFile(p.tokenFileOrDefault())
	if err != nil {
		return fmt.Errorf("read service account token: %w", err)
	}

	base := p.apiServerBaseOrDefault() + "/api/v1/namespaces/" + namespace + "/configmaps"
	body, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": p.Name, "namespace": namespace},
		"data":       id.AsMap(),
	})
	if err != nil {
		return err
	}

	// PATCH first: the common case is re-converging an already-created
	// ConfigMap on every subsequent deploy, and a PATCH against a
	// resource that does not exist yet fails cleanly (404) rather than
	// half-succeeding. application/merge-patch+json REPLACES the `data`
	// map wholesale on a match, which is exactly right here — this job
	// owns the whole ConfigMap, not a subset of its keys.
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, base+"/"+p.Name, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/merge-patch+json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := p.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("patch configmap %s: %w", p.Name, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("patch configmap %s: HTTP %d", p.Name, resp.StatusCode)
	}

	// Does not exist yet: create it.
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, base, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = p.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("create configmap %s: %w", p.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("create configmap %s: HTTP %d", p.Name, resp.StatusCode)
	}
	return nil
}

func (p *ConfigMapPublisher) httpClient() *http.Client {
	if p.http != nil {
		return p.http
	}
	pool := x509.NewCertPool()
	if ca, err := os.ReadFile(p.caCertFileOrDefault()); err == nil {
		pool.AppendCertsFromPEM(ca)
	}
	p.http = &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool},
		},
	}
	return p.http
}

func (p *ConfigMapPublisher) apiServerBaseOrDefault() string {
	if p.apiServerBase != "" {
		return p.apiServerBase
	}
	return apiServerBase
}

func (p *ConfigMapPublisher) namespaceFileOrDefault() string {
	if p.namespaceFile != "" {
		return p.namespaceFile
	}
	return inClusterNamespaceFile
}

func (p *ConfigMapPublisher) tokenFileOrDefault() string {
	if p.tokenFile != "" {
		return p.tokenFile
	}
	return inClusterTokenFile
}

func (p *ConfigMapPublisher) caCertFileOrDefault() string {
	if p.caCertFile != "" {
		return p.caCertFile
	}
	return inClusterCACertFile
}

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// KCLFilePublisher writes Identity as a committed KCL literal — the
// compose/dev equivalent of a ConfigMap, for a target with no ConfigMap
// concept at all. The env's config.k imports this file directly
// (`import .idp_identity_gen as idp`), so a render never needs to learn
// the generated value: it is already a fact in a file the import graph
// already reaches, the same way control-plane's
// identity_internal_console_gen.k works.
//
// COMMITTED, not gitignored — a fresh clone must render a complete
// configuration offline, before its own IdP container has ever booted.
// Re-running with the SAME values leaves the file byte-identical (see
// Publish), so a converged dev stack produces no spurious diff.
type KCLFilePublisher struct {
	// Path is the KCL file this job owns, e.g.
	// deploy/kcl/dev/idp_identity_gen.k.
	Path string
}

// Publish writes id as a wholly-owned generated KCL module: one dict,
// `idp_identity`, with the same four keys ConfigMapPublisher writes under
// AsMap — one shared vocabulary across both targets.
func (p *KCLFilePublisher) Publish(_ context.Context, id Identity) error {
	body := renderIdentityKCL(id)
	if existing, err := os.ReadFile(p.Path); err == nil && string(existing) == body {
		return nil // byte-identical: no write, no timestamp churn
	}
	if err := os.MkdirAll(filepath.Dir(p.Path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(p.Path), err)
	}
	return os.WriteFile(p.Path, []byte(body), 0o644)
}

func renderIdentityKCL(id Identity) string {
	values := id.AsMap()
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("# Code generated by this project's `auth idp-provision` job. DO NOT EDIT.\n")
	b.WriteString("#\n")
	b.WriteString("# The dev IdP identity converged by the idp-provision job (deploy/kcl/\n")
	b.WriteString("# workloads.k), rewritten on every run. client_id and audience are the two\n")
	b.WriteString("# values a dev identity setup cannot declare up front — Zitadel GENERATES\n")
	b.WriteString("# them when the project and the SPA application are registered.\n")
	b.WriteString("#\n")
	b.WriteString("# COMMITTED, not gitignored: a fresh clone renders a complete configuration\n")
	b.WriteString("# offline, before its own IdP container has ever booted. A stale committed\n")
	b.WriteString("# value is not a trap — convergence outranks it, and the next successful\n")
	b.WriteString("# run overwrites it with what the issuer actually issued.\n")
	b.WriteString("#\n")
	b.WriteString("# These are PUBLIC values served to the browser (client_id, issuer) or read\n")
	b.WriteString("# by the backend to validate a token (audience, jwks_url) — none is a\n")
	b.WriteString("# secret; a client id is an identifier, not a credential.\n\n")
	b.WriteString("idp_identity = {\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "    %q = %q\n", k, values[k])
	}
	b.WriteString("}\n")
	return b.String()
}
