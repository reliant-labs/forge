// MAX-PUBLIC-STRUCTS IS SUPPRESSED HERE, AND IT IS RECORDED DEBT, NOT A NIT.
//
// revive counts exported structs per PACKAGE and reports them on whichever
// file it reaches first, which is this one. So the finding is not about the
// types below in particular: package cli declared 68 exported structs against
// a ceiling of 40 before this branch existed, and it has been over the line
// for a long time. Nothing added here could bring it under 40 — the 13 types
// this branch adds would have to become -28 — so a suppression is the only
// thing that makes the gate report the truth rather than attribute 28
// pre-existing declarations to the commit that happened to touch the file.
//
// The types are exported for ONE reason: they are encoding/json unmarshal
// targets for the JSON the sibling KCL deploy module emits, and json.Unmarshal
// can only populate exported FIELDS. Go requires no exported type for that, but
// the fields must be exported, and the house convention here keeps the type and
// its fields at the same visibility so a reader is not left wondering why a
// lowercase type has uppercase members. Verified before suppressing: none of
// the types this branch adds (RemoteBuild, RemoteBuildSource, ControlPlaneEntity,
// FrontendRuntime, BucketRuntime, FirebaseRuntime, StaticSiteCDN, CacheRule,
// BundleDir) is referenced outside package
// cli, so every one of them is package-private in practice.
//
// THE REAL FIX IS A PACKAGE SPLIT, and this comment is the note that says so.
// The KCL entity schema — every *Entity, every deploy/build variant, roughly
// 50 of the 68 — is a self-contained JSON contract with no dependency on
// cobra or on any command, and it belongs in its own package (internal/kclentity
// or similar) that owns it. That lands package cli under the ceiling honestly
// and lets this directive be deleted. It is a mechanical but wide move that
// would bury this branch's diff, so it is deliberately not done here.
//
//nolint:revive // max-public-structs: 68 of these predate this branch; the honest fix is extracting the KCL entity schema into its own package, described above.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/cluster"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/kclplugin"
	"github.com/reliant-labs/forge/internal/kclrender"
	deployv1alpha1 "github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

// KCLEntities is the typed view of the JSON contract the KCL deploy module
// emits: `output = forge.render(bundle)` (P2 map §9 is normative).
//
// Every runnable thing is a Workload (ADR 0002). Each one carries its
// RESOLVED runtime — host, compose, cluster, hosted or build-only — so the
// placement question "where does this run" is answered per workload, never
// env-wide. Callers (`forge build`, `forge env deploy`, `forge env up`,
// `forge env up`) read this rather than reaching back into forge.yaml,
// because placement is a per-env decision that lives in the KCL layer.
type KCLEntities struct {
	// Project and Env are the bundle's project name and the env it was
	// rendered for. ImageTag is the env's RESOLVED image tag — the tag the
	// Cluster workload records' images carry, and therefore the tag
	// `forge build <env>` defaults to so build and deploy agree by
	// construction.
	Project  string `json:"project,omitempty"`
	Env      string `json:"env,omitempty"`
	ImageTag string `json:"image_tag,omitempty"`

	// Lifecycle is the env's DECLARED apply path (kcl/schema.k
	// Bundle.lifecycle): "local" (a developer's own cluster),
	// "ephemeral" (a throwaway per-run cluster), or EMPTY for a real
	// environment — one reconciled from a bundle rather than applied to
	// directly.
	//
	// Empty is the meaningful default, not a gap: an env that declares
	// nothing is treated as a real one. Read it through
	// DirectApplyAllowed rather than comparing strings, so the
	// no-cluster case stays in one place.
	Lifecycle string `json:"lifecycle,omitempty"`

	// Clusters are the k3d clusters forge ensures exist at the head of
	// `forge env up` before any workload deploys. Empty for an env that
	// declares no clusters. Ownership is implicit via Cluster.Network /
	// Cluster.RegistryMirror — there is no "primary" cluster.
	Clusters []ClusterEntity `json:"clusters,omitempty"`
	// ClusterTarget is the Bundle's DECLARED env-wide target — the kubectl
	// context, namespace, registry and platform the env's support
	// resources (Namespace, ConfigMaps, gateways) deploy to, stated once.
	// Nil for an env that declares none (host-only, compose-only, hosted).
	//
	// Every env-wide question (which namespace, which context, which arch)
	// is answered from HERE when it is present, never inferred from "the
	// first cluster workload".
	ClusterTarget *ClusterTargetEntity `json:"cluster_target,omitempty"`
	// KubeconfigSecrets are cross-cluster kubeconfigs forge mints fresh
	// each up (at the cluster→deploy boundary) and applies as k8s Secrets.
	KubeconfigSecrets []KubeconfigSecretEntity `json:"kubeconfig_secrets,omitempty"`

	// Workloads is every runnable thing in the env, of every kind and
	// every runtime, in declaration order. See WorkloadEntity.
	Workloads []WorkloadEntity `json:"-"`
	// Infra are the env's host-run third-party servers (forge.HostInfra:
	// postgres, zitadel). They are dialled by workloads, not declared by
	// the app, so they are not Workloads (P2 map §3.5).
	Infra []HostInfraEntity `json:"infra,omitempty"`

	Frontends  []FrontendEntity  `json:"frontends,omitempty"`
	Gateways   []GatewayEntity   `json:"gateways,omitempty"`
	HTTPRoutes []HTTPRouteEntity `json:"http_routes,omitempty"`
	GRPCRoutes []GRPCRouteEntity `json:"grpc_routes,omitempty"`
	// HelmCharts are the env's declared platform deps (forge.HelmChart),
	// each a renderable with a NAME the `--target` axis selects. forge
	// expands them via helm-as-a-RENDERER and folds the manifests into the
	// apply stream. Empty => no platform deps. See HelmChartEntity.
	HelmCharts []HelmChartEntity `json:"helm_charts,omitempty"`
	// Databases are the env's managed databases (forge.ManagedDatabase),
	// each bound to the cluster or hosted runtime. See DatabaseEntity.
	Databases []DatabaseEntity `json:"databases,omitempty"`
	// SecretProvider is the bundle-level secret provider declaration
	// (WHERE secret values come from for this env). Nil when the bundle
	// declares no provider.
	SecretProvider *SecretProviderEntity `json:"secret_provider,omitempty"`
	// RenderedSecrets are the Bundle-level forge.RenderedSecret
	// declarations (Bundle.rendered_secrets): Secrets forge renders and
	// applies itself, INDEPENDENT of SecretProvider, each at the
	// cluster/namespace it declares (KCL resolves the cluster_target
	// default, so Cluster is always set). Empty => none.
	RenderedSecrets []RenderedSecretEntity `json:"rendered_secrets,omitempty"`
	// ControlPlane is the bundle-level control-plane declaration (WHICH
	// endpoint this env talks to, and the env var NAME its credential is
	// read from). It is NOT an env mode: it is required when some workload
	// binds the Hosted runtime, and otherwise serves as the env's secret
	// store. Nil when the bundle declares none.
	ControlPlane *ControlPlaneEntity `json:"control_plane,omitempty"`

	// RequiredSecrets are the env's declared external Secret prerequisites
	// (forge.ExternalSecret) — out-of-band Secrets the deploy depends on but
	// forge does NOT create. Drive the render-time checklist + the deploy
	// preflight BLOCK on a declared-required-but-absent Secret/key.
	RequiredSecrets []ExternalSecretEntity `json:"required_secrets,omitempty"`
	// RequiredDNS are the env's declared DNS-record prerequisites
	// (forge.DNSRecord) — surfaced as a render-time checklist note (forge
	// can't authoritatively verify external DNS). Empty => none.
	RequiredDNS []DNSRecordEntity `json:"required_dns,omitempty"`

	// ManifestNamespace is the namespace that dominates the rendered
	// `output.manifests` stream. It is the fallback for the declared
	// namespace when the bundle declares no cluster_target and no cluster
	// workload. Empty when the stream carries no namespaced object.
	ManifestNamespace string `json:"-"`

	// ManifestServiceNames are the metadata.name of every raw k8s Service
	// in `output.manifests` — Services a project injects via KCL (e.g.
	// `additional_manifests`, a workload's owned manifests) that are not a
	// workload of their own. The ingress audit unions these into the
	// known-backend set so a route targeting such a Service resolves
	// instead of false-erroring "unknown service".
	ManifestServiceNames []string `json:"-"`

	// ManifestClusters are the kubectl contexts the rendered stream stamps
	// objects onto (forge.dev/cluster). Each is a deploy destination:
	// buildDeployGroups gives one that no workload runs on a k8s group of
	// its own, so a forge.Manifests group on an otherwise empty cluster is
	// applied there instead of silently never applied.
	ManifestClusters []ManifestClusterEntity `json:"-"`
}

// SecretProviderEntity is the parsed bundle-level secret provider
// declaration. Type is "dotenv" | "external" | "rendered". Path is the
// dotenv path (dotenv only), resolved relative to the project root by the
// CLI. Secrets is the declared Secret set (rendered only).
type SecretProviderEntity struct {
	Type string `json:"type"`
	Path string `json:"path,omitempty"`
	// Secrets is populated for Type=="rendered": the explicit Secret
	// declarations (name + per-key source) forge renders + applies per
	// cluster. Empty for dotenv/external.
	Secrets []RenderedSecretEntity `json:"secrets,omitempty"`
}

// ControlPlaneEntity is the parsed bundle-level hosted control-plane
// declaration (kcl/schema.k ControlPlane): WHICH endpoint this env talks
// to, and the NAME of the env var its bearer credential is read from.
//
// It carries no credential and never will. The token VALUE is resolved
// Go-side by internal/cloud from the flag / env var / login file, exactly
// as secret VALUES are resolved by internal/secrets rather than KCL.
type ControlPlaneEntity struct {
	Type     string `json:"type"`
	Endpoint string `json:"endpoint"`
	TokenEnv string `json:"token_env,omitempty"`
	// Organization is the org these hosted artifacts belong to, and it is
	// LOAD-BEARING rather than a hint: it is the `<org>` segment of the push
	// base forge composes. KCL refuses a hosted env that declares none.
	Organization string `json:"organization,omitempty"`
	// RegistryHost is the platform registry this org's artifacts live on,
	// defaulted in KCL to forge.RELIANT_REGISTRY_HOST.
	//
	// This is NOT the registry a workload's image names — that one is still
	// the author's, still on the workload, and used verbatim. This is the
	// subtree a BARE hosted image is composed under, which is the one case
	// the author does not choose because the platform admits exactly one.
	RegistryHost string `json:"registry_host,omitempty"`
}

// RenderedSecretEntity mirrors the kcl/schema.k RenderedSecret — one k8s
// Secret forge renders from declared sources. Keys maps each in-Secret
// key to its value source.
type RenderedSecretEntity struct {
	Name string                             `json:"name"`
	Keys map[string]RenderedSecretKeyEntity `json:"keys"`
	// Cluster / Namespace are the EXPLICIT placement (kubectl context +
	// namespace). Always set for a Bundle.rendered_secrets entry. On a
	// RenderedSecrets provider entry an empty Cluster means "infer": land
	// in each cluster whose services reference it by secret_ref.
	Cluster   string `json:"cluster,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

// RenderedSecretKeyEntity mirrors the kcl/schema.k RenderedSecretKey.
// From is "dotenv" (read .env.<env> at Key) or "literal" (inline Value,
// dev/e2e only). A "dotenv" key carries no Value; a "literal" key carries
// no dotenv Key.
type RenderedSecretKeyEntity struct {
	From  string `json:"from"`
	Key   string `json:"key,omitempty"`
	Value string `json:"value,omitempty"`
}

// ClusterEntity mirrors the kcl/schema.k Cluster — a k3d cluster forge
// ensures exists before deploying. The reconcile (clusterPhase) reads
// these and runs `k3d cluster create` for any that are absent.
//
// Ownership is a REFERENCE: a secondary cluster names its `owner`
// Cluster; the KCL render layer DERIVES the joined network
// (`k3d-<owner.name>`, projected into Network) and the registry-inherit
// behavior (projected into RegistryInherit=true). An owner cluster
// projects an empty Network and RegistryInherit=false. There is no
// "primary" field and no most-X heuristic.
type ClusterEntity struct {
	Name string `json:"name"`
	// Provider selects which ClusterProvider (see cluster_provider.go)
	// ensures this cluster: "k3d" (default), "vcluster", or "gke". Only
	// "k3d" is implemented — see clusterProviderRegistry.
	Provider string `json:"provider,omitempty"`
	// Context is the derived kubectl context (`k3d-<name>`), projected so
	// the reconcile / kubeconfig mint can target the cluster without
	// re-deriving the prefix.
	Context string `json:"context,omitempty"`
	Config  string `json:"config,omitempty"`
	// Image pins the k3s node image used for flag-driven clusters. Config-driven
	// clusters declare the image in their authoritative k3d YAML instead.
	Image string `json:"image,omitempty"`
	// Network is the derived docker network this cluster joins —
	// `k3d-<owner.name>` for a secondary, empty for an owner cluster.
	Network string `json:"network,omitempty"`
	// RegistryInherit is the derived registry-inherit flag — true when an
	// `owner` is set (forge mirrors the owner's registry onto this
	// cluster's node), false for an owner cluster.
	RegistryInherit bool `json:"registry_inherit,omitempty"`
	Servers         int  `json:"servers,omitempty"`
	Agents          int  `json:"agents,omitempty"`
	APIPort         int  `json:"api_port,omitempty"`
	// Ingress, when true, installs the Gateway API stack (pinned Gateway-API
	// CRDs + the Envoy Gateway controller via helm + the `eg` GatewayClass)
	// into this cluster after it's ensured. A fresh k3d cluster ships none of
	// these; an env whose Gateway/HTTPRoute/GRPCRoute resources land on this
	// cluster needs it on. Idempotent (helm upgrade --install + kubectl apply).
	//
	// This is the IMPERATIVE install. An env that declares its Gateway API
	// controller DECLARATIVELY as a forge.HelmChart platform dep
	// (Bundle.helm_charts) leaves Ingress false and sets HostPorts true.
	Ingress bool `json:"ingress,omitempty"`
	// HostPorts, when true, merges the generated deploy/k3d-ports.yaml Gateway
	// listener host-port fragment into the k3d config at create time. Ingress
	// IMPLIES this (an imperatively-installed Gateway also needs the host
	// ports). Set HostPorts explicitly when the controller is installed
	// declaratively (Ingress=false) but the cluster still hosts a Gateway
	// whose listeners must be host-mapped at create time.
	HostPorts bool `json:"host_ports,omitempty"`
	// ClusterCIDR / ServiceCIDR are the pod and Service address blocks k3s
	// allocates from, reaching `k3d cluster create` as the k3s server args
	// --cluster-cidr / --service-cidr. Empty (the default, and every cluster
	// that does not declare them) leaves k3s on its own defaults —
	// 10.42.0.0/16 pods, 10.43.0.0/16 services — so no argument is added and
	// the created cluster is identical to before.
	//
	// They exist because two k3d clusters on ONE docker network both default
	// to the SAME pod CIDR, so the same pod IP exists in both and a local
	// route always wins: a workload in one cluster cannot dial a pod IP in the
	// other. Disjoint blocks are what make that cross-cluster pod dialing
	// possible, which is how the equivalent cloud topology (several clusters
	// in one VPC, on disjoint blocks) already works.
	//
	// Validated KCL-side (shape, and that a cluster's two blocks do not
	// overlap each other) — see kcl/schema.k's Cluster check block.
	ClusterCIDR string `json:"cluster_cidr,omitempty"`
	ServiceCIDR string `json:"service_cidr,omitempty"`
}

// KubeconfigSecretEntity mirrors the kcl/schema.k KubeconfigSecret — a
// cross-cluster kubeconfig forge mints FRESH each up and stores as a k8s
// Secret. The mint step (mintKubeconfigSecrets) resolves the target's
// endpoint at runtime and never persists the IP.
type KubeconfigSecretEntity struct {
	Name          string `json:"name"`
	InCluster     string `json:"in_cluster"`
	TargetCluster string `json:"target_cluster"`
	ContextName   string `json:"context_name"`
	Key           string `json:"key,omitempty"`
	Namespace     string `json:"namespace,omitempty"`
	Reachability  string `json:"reachability,omitempty"`

	// TargetContext is the kubectl context addressing the target
	// cluster. Empty means the k3d derivation (`k3d-<TargetCluster>`,
	// resolved via `k3d kubeconfig get`). Set, it names a context in the
	// operator's own kubeconfig — which is how a non-k3d cluster (GKE)
	// becomes addressable at all.
	TargetContext string `json:"target_context,omitempty"`

	// ServiceAccount, when non-nil, switches the mint from COPYING the
	// operator's credential to MINTING one on the target. See
	// KubeconfigServiceAccountEntity.
	ServiceAccount *KubeconfigServiceAccountEntity `json:"service_account,omitempty"`
}

// KubeconfigServiceAccountEntity mirrors kcl/schema.k's
// KubeconfigServiceAccount — the ServiceAccount + RBAC + long-lived token
// forge converges ON THE TARGET cluster so the minted kubeconfig carries a
// credential a POD can present.
//
// This is the whole of defect F1b: a GKE kubeconfig authenticates with an
// `exec` plugin that resolves only on the operator's machine, so copying it
// into a Secret produces a kubeconfig no pod can use. Minting sidesteps the
// operator's credential entirely.
type KubeconfigServiceAccountEntity struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Namespaces, when non-empty, binds Rules as a Role in each listed
	// namespace instead of one cluster-wide ClusterRole.
	Namespaces []string                     `json:"namespaces,omitempty"`
	Rules      []KubeconfigPolicyRuleEntity `json:"rules"`
}

// KubeconfigPolicyRuleEntity mirrors kcl/schema.k's KubeconfigPolicyRule — one
// rbac.authorization.k8s.io/v1 PolicyRule.
type KubeconfigPolicyRuleEntity struct {
	APIGroups []string `json:"api_groups,omitempty"`
	Resources []string `json:"resources"`
	Verbs     []string `json:"verbs"`
}

// GatewayEntity mirrors the kcl/schema.k Gateway. Listeners are inlined.
// Tls is nil when the gateway is plaintext.
type GatewayEntity struct {
	Name             string                  `json:"name"`
	GatewayClassName string                  `json:"gateway_class_name,omitempty"`
	Host             string                  `json:"host,omitempty"`
	TLS              *GatewayTLSEntity       `json:"tls,omitempty"`
	Listeners        []GatewayListenerEntity `json:"listeners,omitempty"`
	Addresses        []GatewayAddressEntity  `json:"addresses,omitempty"`
}

// GatewayAddressEntity mirrors the kcl/schema.k GatewayAddress — one
// entry in the Gateway's spec.addresses, pinning it to a load-balancer
// address. Type is "NamedAddress" (Value is a GKE reserved static-IP
// reservation name) or "IPAddress" (Value is a literal IP).
type GatewayAddressEntity struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// GatewayListenerEntity mirrors the kcl/schema.k GatewayListener.
// Protocol is "HTTP" | "HTTPS" | "H2C".
//
// Hostname and TLS are the per-listener overrides that let ONE Gateway
// serve several hostnames, each terminating its own certificate. Both
// are empty/nil when the listener inherits the parent Gateway's `host`
// / `tls`; the JSON carries what was DECLARED, not the resolved value,
// so consumers apply the fallback themselves (see EffectiveHost).
type GatewayListenerEntity struct {
	Name       string            `json:"name"`
	Port       int               `json:"port"`
	Protocol   string            `json:"protocol"`
	PathPrefix string            `json:"path_prefix,omitempty"`
	Hostname   string            `json:"hostname,omitempty"`
	TLS        *GatewayTLSEntity `json:"tls,omitempty"`
}

// EffectiveHost resolves the hostname a route on this listener is
// served on: the route's own override wins, then the listener's
// hostname, then the gateway's default host. Mirrors _route_host in
// kcl/lib/gateway.k — the two must agree, or `forge cluster urls` prints
// a URL the cluster does not serve.
//
// Returns "" when nothing resolves; callers substitute their own
// fallback (the dev URL builders use "localhost").
func (g *GatewayEntity) EffectiveHost(listener *GatewayListenerEntity, routeHost string) string {
	if routeHost != "" {
		return routeHost
	}
	if listener != nil && listener.Hostname != "" {
		return listener.Hostname
	}
	return g.Host
}

// GatewayTLSEntity is the TLS block on a Gateway. Mode selects the
// cert origin: "cert_manager" (default — cert-manager Certificate
// emitted alongside the Gateway, CertIssuer names a ClusterIssuer),
// "mkcert" (Secret populated host-side by `forge cluster up` via the
// mkcert binary; CertIssuer unused), or "gke_certmap" (GCP Certificate
// Manager map named by Certmap terminates TLS; CertIssuer / SecretName
// unused — the GKE Gateway controller binds the map via the
// `networking.gke.io/certmap` annotation forge stamps on the Gateway).
type GatewayTLSEntity struct {
	CertIssuer string `json:"cert_issuer,omitempty"`
	SecretName string `json:"secret_name,omitempty"`
	Certmap    string `json:"certmap,omitempty"`
	Mode       string `json:"mode,omitempty"`
}

// RouteRetriesEntity mirrors kcl/schema.k's RouteRetries — how a route
// retries a failed attempt at its backend. On is Envoy Gateway's closed
// trigger set (5xx, reset, connect-failure, …).
type RouteRetriesEntity struct {
	On            []string `json:"on"`
	Attempts      int      `json:"attempts"`
	PerTryTimeout string   `json:"per_try_timeout,omitempty"`
}

// RouteHealthCheckEntity mirrors kcl/schema.k's RouteHealthCheck — an
// ACTIVE health check. ExpectedStatuses is the field that matters: a
// token-authed registry answers /v2/ with 401 when healthy, so a default
// 2xx check would mark every replica down.
type RouteHealthCheckEntity struct {
	Path               string `json:"path"`
	ExpectedStatuses   []int  `json:"expected_statuses,omitempty"`
	Interval           string `json:"interval,omitempty"`
	Timeout            string `json:"timeout,omitempty"`
	UnhealthyThreshold int    `json:"unhealthy_threshold,omitempty"`
	HealthyThreshold   int    `json:"healthy_threshold,omitempty"`
}

// RouteOutlierDetectionEntity mirrors kcl/schema.k's
// RouteOutlierDetection — PASSIVE health checking, which ejects an
// endpoint that is failing real requests.
type RouteOutlierDetectionEntity struct {
	Consecutive5xx           int    `json:"consecutive_5xx,omitempty"`
	ConsecutiveGatewayErrors int    `json:"consecutive_gateway_errors,omitempty"`
	Interval                 string `json:"interval,omitempty"`
	BaseEjectionTime         string `json:"base_ejection_time,omitempty"`
	MaxEjectionPercent       int    `json:"max_ejection_percent,omitempty"`
}

// RouteTrafficEntity mirrors kcl/schema.k's RouteTraffic — the typed
// per-route policy that renders ONE Envoy Gateway BackendTrafficPolicy.
// Timeout is the whole-request timeout (a multi-GB blob upload runs far
// past Envoy's 15s default).
type RouteTrafficEntity struct {
	Retries          *RouteRetriesEntity          `json:"retries,omitempty"`
	Timeout          string                       `json:"timeout,omitempty"`
	HealthCheck      *RouteHealthCheckEntity      `json:"health_check,omitempty"`
	OutlierDetection *RouteOutlierDetectionEntity `json:"outlier_detection,omitempty"`
}

// HTTPRouteEntity mirrors the kcl/schema.k HTTPRoute.
//
// The backend is EITHER Service (a Service name, used verbatim) or
// Workload (a forge.Workload name, resolved per env to that workload's
// Service or — when it runs OnHost on a local k3d cluster — to the host
// process via an Envoy Gateway Backend). Exactly one is set.
//
// Port is 0 when it was INFERRED from the workload's single port: this
// projection carries what was DECLARED, and output.manifests carries the
// resolved backend. A pre-resolved copy here would be a second source of
// truth that can disagree with the manifest.
type HTTPRouteEntity struct {
	Name     string `json:"name"`
	Gateway  string `json:"gateway"`
	Listener string `json:"listener"`
	Service  string `json:"service,omitempty"`
	Workload string `json:"workload,omitempty"`
	Port     int    `json:"port,omitempty"`
	Host     string `json:"host,omitempty"`
	Path     string `json:"path,omitempty"`
	// Traffic is the typed per-route policy; nil when the route declares
	// none. It replaced a `raw_policy` string that nothing in forge ever
	// emitted (ADR-0003 F2).
	Traffic *RouteTrafficEntity `json:"traffic,omitempty"`
}

// GRPCRouteEntity mirrors the kcl/schema.k GRPCRoute. Shape matches
// HTTPRouteEntity — the distinction is the rendered Gateway API
// resource kind (GRPCRoute vs HTTPRoute).
type GRPCRouteEntity struct {
	Name     string              `json:"name"`
	Gateway  string              `json:"gateway"`
	Listener string              `json:"listener"`
	Service  string              `json:"service,omitempty"`
	Workload string              `json:"workload,omitempty"`
	Port     int                 `json:"port,omitempty"`
	Host     string              `json:"host,omitempty"`
	Path     string              `json:"path,omitempty"`
	Traffic  *RouteTrafficEntity `json:"traffic,omitempty"`
}

// HelmChartEntity is one declared platform dependency from rendered KCL
// (the `output.helm_charts` projection of forge.HelmChart). It is the
// declaration only — forge expands it via `helm template --skip-crds`
// Go-side (internal/cluster.RenderHelmChart) and folds the manifests into
// the apply stream, selected by `--target=<name>`. helm is a RENDERER,
// not an installer: there is no release, no `helm install`.
type HelmChartEntity struct {
	Name string `json:"name"`
	// Chart is the chart name for a repo chart; empty for an OCI chart.
	Chart string `json:"chart,omitempty"`
	// Repo is the chart-repo URL; mutually exclusive with OCI.
	Repo string `json:"repo,omitempty"`
	// OCI is the OCI chart ref; mutually exclusive with Repo.
	OCI string `json:"oci,omitempty"`
	// Version is the pinned chart version.
	Version string `json:"version"`
	// Namespace is the namespace the chart renders into (helm template -n).
	Namespace string `json:"namespace"`
	// Values is the helm values overlay, passed through verbatim.
	Values map[string]any `json:"values,omitempty"`
	// CRDs selects which forge-owned CRD bundle to apply FIRST
	// (Established-gated) before the chart's controllers: "" (none),
	// "gateway-api", or "cert-manager". The chart is rendered --skip-crds,
	// so forge owns the CRD surface.
	CRDs string `json:"crds,omitempty"`
	// Cluster is the kubectl CONTEXT this chart installs into, overriding
	// the env's primary cluster for this chart alone. The KCL `cluster`
	// field is a forge.Cluster REFERENCE and the render projects its
	// derived `.context` (`k3d-<name>`) here, so the value is always the
	// context forge applies with and can never drift from the cluster the
	// declaration names. Empty => the env's primary cluster.
	//
	// This is what lets an operator be installed where its custom
	// resources land: control-plane's dev renders `postgresql.cnpg.io/v1
	// Cluster` objects into cp-daemon, so CNPG must be installed there and
	// not in the env's primary control-plane cluster.
	Cluster string `json:"cluster,omitempty"`
	// Manifests are consumer-declared raw k8s manifest dicts that ride this
	// chart's `--target` (the `eg` GatewayClass, cert-manager ClusterIssuers)
	// — the cluster-scoped instances the chart's controller reconciles but
	// the chart itself doesn't ship. Stamped with the chart's app-label and
	// applied AFTER its controllers; excluded from a bare app deploy.
	Manifests []any `json:"manifests,omitempty"`
}

// ExternalSecretEntity mirrors the kcl/schema.k ExternalSecret — an
// out-of-band Secret a deploy DEPENDS ON but forge does NOT create. forge
// reads these to print a render-time prerequisite checklist and to drive
// the deploy preflight (a declared-required-but-absent Secret/key BLOCKS,
// reusing the same SecretGetter as the secretKeyRef preflight). ValueGroup,
// when set, ties this Secret to others that must carry the SAME logical
// value (the cross-secret byte-match group).
type ExternalSecretEntity struct {
	Name      string   `json:"name"`
	Namespace string   `json:"namespace"`
	Keys      []string `json:"keys"`
	// Reason is a short human note: why the deploy needs this Secret. Shown
	// in the checklist so the operator knows what breaks if it's absent.
	Reason string `json:"reason,omitempty"`
	// ValueGroup is the shared id tying this Secret to other ExternalSecrets
	// that must carry the same logical value (cross-secret byte-match).
	// Empty => a standalone prereq (no byte-match group).
	ValueGroup string `json:"value_group,omitempty"`
}

// DNSRecordEntity mirrors the kcl/schema.k DNSRecord — a DNS record a
// deploy depends on. forge can't authoritatively verify external DNS, so
// this is a render-time checklist entry (and a `--check` note), not a hard
// block. Target is the expected value (LB IP / CNAME target) when known.
type DNSRecordEntity struct {
	Host string `json:"host"`
	Type string `json:"type"`
	// Target is the expected value (LB IP for A/AAAA, CNAME target). Often
	// unknown at author time (ephemeral LB IP), so optional.
	Target string `json:"target,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// WorkloadEntity is one runnable thing (ADR 0002): a service, worker, job,
// cron, operator or tool, declared once as fw.Workload and bound to ONE
// runtime. The same entity drives every consumer — `forge build` reads
// Build, `forge env up` derives the host argv from Build + Spec.Args, the
// deploy dispatcher groups by Runtime, and a Hosted workload's Spec is
// published verbatim as a forge.dev Workload CR.
type WorkloadEntity struct {
	Name string
	// Kind is the workload kind (service|worker|job|cron|operator|tool). It
	// always equals Spec.EffectiveKind().
	Kind string
	// Image is the registry-less artifact name forge builds this workload
	// into ("" = no artifact of its own). It is the build-state / release
	// ledger key. Spec.Image is the RESOLVED reference a runtime pulls.
	Image string
	// BuildImage is Image plus the tag the workload's own image pins
	// ("workspace-base:dev-per-daemon"), else Image. It is the BUILD's
	// identity and so independent of the runtime: a BuildOnly or host
	// workload resolves no Spec.Image, and its pin must still reach
	// `forge build`. Read it through PinnedBuildTag.
	BuildImage string
	// Build is the typed build declaration. Type=="" means forge does not
	// build this workload (a third-party image, a compose service, a
	// sibling binary): there is no synthesized default.
	Build BuildConfigEntity
	// Runtime is where the workload runs, resolved (w.runtime or the
	// bundle's default). Exactly one of its variant pointers is set.
	Runtime RuntimeEntity
	// Spec is the runtime-independent declaration, decoded STRICTLY into
	// forge's wire type — the same bytes a hosted Workload CR carries.
	Spec deployv1alpha1.WorkloadSpec
}

// Runtime discriminators (RuntimeEntity.Type).
const (
	RuntimeHost      = "host"
	RuntimeCompose   = "compose"
	RuntimeCluster   = "cluster"
	RuntimeHosted    = "hosted"
	RuntimeBuildOnly = "build-only"
)

// RuntimeEntity is a workload's resolved runtime. Type carries the tag;
// exactly one variant pointer is non-nil, except for "hosted", whose
// runtime has no fields (the control plane owns every placement fact).
type RuntimeEntity struct {
	Type      string
	Host      *HostRuntime
	Compose   *ComposeRuntime
	Cluster   *ClusterRuntime
	BuildOnly *BuildOnlyDeploy
}

// HostRuntime is forge.Host: run the workload as a local process. The argv
// is NOT declared here — hostlaunch.BuildCmd derives it from the runner, the
// workload's build and its args. Env comes from the workload's spec.env.
type HostRuntime struct {
	Runner    string `json:"runner,omitempty"`     // "go-run" | "air" | "binary" | "delve"
	AirConfig string `json:"air_config,omitempty"` // default .air.toml
	// WorkingDir overrides the launched subprocess's working directory.
	// Relative paths resolve against the project root. Use this for
	// cross-repo binaries whose runner config (e.g. Air's build_cmd
	// paths) resolves relative to a sibling repo. Default: project root.
	WorkingDir string `json:"working_dir,omitempty"`
	// ListenPorts are the host TCP ports this workload itself BINDS. When
	// declared, the `forge env up` port-conflict pre-flight and readiness
	// gate check EXACTLY these; otherwise the workload's declared
	// spec.ports stand in.
	//
	// A POINTER so "not declared" and "declares zero ports" stay distinct.
	// A host workload that legitimately binds nothing (a packaged desktop
	// app) declares `listen_ports = []`; forge must then allocate nothing
	// and gate on nothing.
	ListenPorts *[]int `json:"listen_ports,omitempty"`
	DelvePort   int    `json:"delve_port,omitempty"` // runner delve; default 2345

	// LaunchEnv is RUN state, not contract: env forge decides for this one
	// launch (the ephemeral PORT resolveEphemeralHostPorts allocates) and
	// layers over the workload's declared spec.env. It is never part of
	// the render — a per-run value must not look like a declaration.
	LaunchEnv map[string]string `json:"-"`
}

// ComposeRuntime is forge.Compose: the workload is a docker-compose service.
// Compose owns the container definition, so the workload contributes only its
// name and this block.
//
// Wait is a pointer so "absent from the JSON" is distinguishable from
// "explicitly False": the absent case must resolve to the safe default
// (wait) rather than to the bool zero value.
type ComposeRuntime struct {
	Service     string `json:"service,omitempty"` // default: the workload name
	File        string `json:"file,omitempty"`    // default docker-compose.yml
	EnvFile     string `json:"env_file,omitempty"`
	Wait        *bool  `json:"wait,omitempty"`
	WaitTimeout int    `json:"wait_timeout,omitempty"`
	// Env is the map forge puts in the `docker compose` PROCESS environment
	// — where the compose file's own `${VAR}` references interpolate from.
	// Distinct from EnvFile, which only reaches containers.
	Env map[string]string `json:"env,omitempty"`
	// Shared marks the stack as machine infrastructure every worktree of
	// the repo uses; forge drives it from the primary checkout. See
	// OnCompose.shared in kcl/workload.k.
	Shared bool `json:"shared,omitempty"`
}

// ClusterRuntime is forge.Cluster: a Kubernetes cluster the author operates.
// Cluster IS the kubectl context. Replicas, ports and probes are not here —
// they are the workload's spec, the same on every runtime.
type ClusterRuntime struct {
	Cluster          string   `json:"cluster,omitempty"`
	Namespace        string   `json:"namespace,omitempty"`
	ConnectedCluster string   `json:"connected_cluster,omitempty"`
	Registry         string   `json:"registry,omitempty"`
	Domain           string   `json:"domain,omitempty"`
	Platform         string   `json:"platform,omitempty"` // GOARCH override; empty = forge.yaml deploy.target_arch
	ImagePullSecrets []string `json:"image_pull_secrets,omitempty"`
}

// HostInfraEntity is one `Bundle.infra` entry: a third-party server forge
// runs as a HOST PROCESS instead of a container — the default shape for dev
// infrastructure on a machine that cannot afford a container runtime. See
// internal/hostinfra for the supervisor and
// internal/deploytarget.HostInfraProvider for the dispatch.
type HostInfraEntity struct {
	Name     string `json:"name"`
	Engine   string `json:"engine,omitempty"`
	Port     int    `json:"port,omitempty"`
	Database string `json:"database,omitempty"`
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	DataDir  string `json:"data_dir,omitempty"`
	Version  string `json:"version,omitempty"`

	// engine = "zitadel" only — the dev IdP's backing database and its
	// declarative bootstrap. See the HostInfra schema in kcl/schema.k.
	IDPDatabase     string `json:"idp_database,omitempty"`
	IDPDatabasePort int    `json:"idp_database_port,omitempty"`
	IDPMasterKey    string `json:"idp_masterkey,omitempty"`
	IDPStepsFile    string `json:"idp_steps_file,omitempty"`
	IDPPATPath      string `json:"idp_pat_path,omitempty"`
}

// BuildConfigEntity is the dispatched-by-type view of a workload's build
// block. The raw JSON is a tagged union; Type carries the tag; exactly one
// of Go/Docker/Shell/Remote is non-nil after [dispatchBuild] runs.
// Type=="" means the KCL `build` block was absent (null): forge does not
// build the workload, and nothing is synthesized.
type BuildConfigEntity struct {
	Type   string       // "go" | "docker" | "shell" | "remote" | "" (absent)
	Go     *GoBuild     // populated when Type=="go"
	Docker *DockerBuild // populated when Type=="docker"
	Shell  *ShellBuild  // populated when Type=="shell"
	Remote *RemoteBuild // populated when Type=="remote"
}

// GoBuild mirrors the kcl/schema.k GoBuild. Cmd is the go-build target
// package (e.g. "./cmd/trader"); the rest are the cross-compile + flag
// knobs build.go passes straight to `go build`.
type GoBuild struct {
	OutputName string            `json:"output_name,omitempty"`
	Cmd        string            `json:"cmd"`
	GOOS       string            `json:"goos,omitempty"`
	GOARCH     string            `json:"goarch,omitempty"`
	Ldflags    []string          `json:"ldflags,omitempty"`
	Tags       []string          `json:"tags,omitempty"`
	Flags      []string          `json:"flags,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}

// DockerBuild mirrors the kcl/schema.k DockerBuild — the per-service
// container image build. Reuses forge's existing docker primitives
// (tag/registry/push/build-contexts) as behavior; these fields select
// the dockerfile/platform/target/build_args.
//
// Registry + BuildContexts are the per-service `docker` facts that are NOT
// expressible in a Dockerfile — the push target for THIS service's image and
// the named build contexts THIS service's Dockerfile `COPY --from=`s. Each
// falls back to the project-level forge.yaml `docker.{registry,build_contexts}`
// when unset, so a single-image project keeps declaring them once at the top
// level. KCL renders per env, so these are per-service AND per-env.
type DockerBuild struct {
	OutputName string `json:"output_name,omitempty"`
	Dockerfile string `json:"dockerfile,omitempty"`
	// Context is the MAIN `docker build` context directory, relative to the
	// project root. Empty is the project root ("."), so an unset Context
	// builds byte-identically to before this field existed. Distinct from
	// BuildContexts, which are NAMED --build-context entries. The Dockerfile
	// need not live under it — docker allows -f outside the context.
	Context   string            `json:"context,omitempty"`
	Platform  string            `json:"platform,omitempty"`
	Target    string            `json:"target,omitempty"`
	BuildArgs map[string]string `json:"build_args,omitempty"`
	// BuildContexts maps a `docker buildx --build-context name=value` entry
	// THIS service's Dockerfile needs (a sibling-checkout path the Dockerfile
	// `COPY --from=name`s, a `docker-image://` override, …). Same value shapes
	// as config.DockerConfig.BuildContexts. Empty falls back to the
	// project-level forge.yaml docker.build_contexts.
	BuildContexts map[string]string `json:"build_contexts,omitempty"`
}

// ShellBuild mirrors the kcl/schema.k ShellBuild — the SINGLE shell
// escape hatch: a verbatim `sh -c` build command that owns the whole
// build (and any push).
//
// Execution contract (see internal/buildtarget):
//
//   - cwd == Cwd resolved against the project root (relative paths join
//     the dir holding forge.yaml; absolute pass through). Empty Cwd =>
//     the project root, so relative paths like scripts/build-image.sh,
//     ../sibling-repo, or docker/Dockerfile resolve as a user expects. A
//     Cwd that doesn't exist on disk is a HARD build failure.
//   - Cmd is run VERBATIM. Forge substitutes nothing into it: it is a plain
//     KCL string, so the tag, arch and env are composed in KCL
//     (forge.image_tag(), forge.target_arch(), forge.env()) where those
//     values already live, and every `$VAR` in the command is the shell's.
//   - Env vars are merged onto the command's process environment (declared
//     keys win), which is also how to keep a `${NAME}` spelling in the
//     command: declare NAME in Env and the shell resolves it.
//   - on success forge captures the pushed digest (best-effort) and
//     writes the build-state file so deploy pins the same tag/digest.
//
// Absorbs the former flat Service.build_cmd / build_cwd / build_env trio —
// one declaration surface, one contract.
type ShellBuild struct {
	OutputName string            `json:"output_name,omitempty"`
	Cmd        string            `json:"cmd"`
	Cwd        string            `json:"cwd,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}

// RemoteBuildSource mirrors the kcl/schema.k GitSource as it appears
// inside a RemoteBuild — the repo+ref pin the build is performed against.
//
// Ref is REQUIRED and Commit is absent on purpose: KCL declares the pin a
// human writes (a tag, a branch, a sha), and resolving it to an immutable
// commit is the submitting client's job, not the schema's. A KCL field
// holding a resolved sha would go stale the moment the branch moved,
// while looking authoritative.
type RemoteBuildSource struct {
	Repo   string `json:"repo"`
	Ref    string `json:"ref"`
	Subdir string `json:"subdir,omitempty"`
}

// RemoteBuild mirrors the kcl/schema.k RemoteBuild — an image built by a
// HOSTED build service rather than on the machine running `forge build`.
//
// It is the only member of this union that does NOT build locally, and
// every other difference follows from that. Source is a repo+ref PIN
// rather than a path, because a build service cannot read the caller's
// filesystem. The build is ASYNCHRONOUS, because it may queue behind
// other work — every other build in forge is synchronous, which is worth
// knowing before wiring one into a pipeline that assumes otherwise.
//
// What is absent is the security model, and it is absent by design
// because a hosted service executes the caller's instructions on shared
// infrastructure: no registry, no push credential, no cache reference,
// no tag, and no secret build args. The service derives the destination
// and the cache from the identity it AUTHENTICATED the submission with,
// tags by resolved commit, and holds the registry credential where the
// build never reaches it. See the schema docstring for the argument
// behind each omission.
type RemoteBuild struct {
	OutputName     string            `json:"output_name,omitempty"`
	Source         RemoteBuildSource `json:"source"`
	Dockerfile     string            `json:"dockerfile,omitempty"`
	Target         string            `json:"target,omitempty"`
	Platform       string            `json:"platform,omitempty"`
	BuildArgs      map[string]string `json:"build_args,omitempty"`
	CPUMillicores  int64             `json:"cpu_millicores,omitempty"`
	MemoryBytes    int64             `json:"memory_bytes,omitempty"`
	CacheGiB       int32             `json:"cache_gib,omitempty"`
	TimeoutSeconds int32             `json:"timeout_seconds,omitempty"`
}

// DatabaseEntity is one managed database: its name (the CloudNativePG Cluster
// name), its runtime ("cluster" or "hosted"), where it lands when
// cluster-bound, and its spec, which is forge's Go tier type.
type DatabaseEntity struct {
	Name      string                             `json:"name"`
	Runtime   string                             `json:"runtime,omitempty"`
	Cluster   string                             `json:"cluster,omitempty"`
	Namespace string                             `json:"namespace,omitempty"`
	Spec      deployv1alpha1.ManagedDatabaseSpec `json:"spec"`
}

// Hosted reports whether the control plane runs this database.
func (d DatabaseEntity) Hosted() bool { return d.Runtime == RuntimeHosted }

// BuildOnlyDeploy is the forge.BuildOnly runtime: the workload is built and
// shipped as an artifact, never run — a CLI shipped in a release, etc.
// BuildVariants lets one workload emit several binaries (different ldflags /
// build tags).
type BuildOnlyDeploy struct {
	BuildVariants []BuildVariant `json:"build_variants,omitempty"`
}

// BuildVariant describes one binary produced by a build-only service.
type BuildVariant struct {
	Name       string            `json:"name"`
	Ldflags    []string          `json:"ldflags,omitempty"`
	BuildTags  []string          `json:"build_tags,omitempty"`
	GOOS       string            `json:"goos,omitempty"`
	GOARCH     string            `json:"goarch,omitempty"`
	EnvAtBuild map[string]string `json:"env_at_build,omitempty"`
	OutputName string            `json:"output_name,omitempty"` // default: <service>-<variant>
}

// FrontendEntity is one frontend from rendered KCL: a dev server, or a
// static build forge publishes. Runtime says which (ADR 0002 §6), and every
// consumer dispatches on Runtime.Type. The static build facts (PublicDir,
// BasePath, Bundle, CacheControl) are the frontend's own, identical on every
// runtime that publishes a build.
type FrontendEntity struct {
	Name string `json:"name"`
	Type string `json:"type,omitempty"` // "nextjs" | "vite-spa" | "react-native"
	// Path is the frontend's directory in THIS repository. Empty exactly
	// when Source is set — a cross-repo frontend has no directory here
	// until the source resolver materializes one.
	Path string `json:"path"`
	// Source pins the frontend's code to another repository at a ref
	// (kcl/schema.k GitSource). nil for the ordinary in-repo frontend.
	// Callers that shell into a frontend must resolve this to a directory
	// via internal/gitsource rather than reading Path — see
	// resolveFrontendEntitySources.
	Source    *config.GitSource `json:"source,omitempty"`
	DevRunner string            `json:"dev_runner,omitempty"` // "npm" (default) | "pnpm" | "yarn"
	Port      int               `json:"port,omitempty"`
	EnvFile   string            `json:"env_file,omitempty"`
	EnvVars   []KCLEnvVar       `json:"env_vars,omitempty"`
	// Config is the typed, well-known knobs block (kcl/schema.k
	// FrontendConfig). nil when the frontend declares no `config`. forge
	// expands it into the SAME env stream as EnvVars via frontendConfigEnv,
	// with explicit EnvVars winning on a variable collision. See
	// EffectiveEnvVars.
	Config *FrontendConfigEntity `json:"config,omitempty"`
	// Runtime is where the frontend runs. Always present in a render: the
	// Bundle refuses a frontend with none. Zero only for a frontend bridged
	// in from forge.yaml (mergeConfigFrontends), which is dev-served.
	Runtime FrontendRuntime `json:"runtime"`
	// Image is where an OnHosted frontend's site release is pushed: the
	// reference the frontend DECLARES, registry host included. Empty for every
	// other runtime (the render refuses it there, and requires it on hosted).
	// forge appends the platform's static.v1 layout — see
	// deploytarget.HostedStaticRepository — so the declared reference stays
	// exactly what the author wrote.
	Image string `json:"image,omitempty"`
	// PublicDir is the build's static output dir relative to the frontend's
	// code — declared, else the type's convention (resolved by the render).
	PublicDir string `json:"public_dir,omitempty"`
	// BasePath mounts the frontend under a sub-path ("/admin"); empty is
	// the site root.
	BasePath string `json:"base_path,omitempty"`
	// Bundle is the extra pre-built static dirs assembled into the site.
	Bundle []BundleDir `json:"bundle,omitempty"`
	// CacheControl is the ordered Cache-Control rules forge applies to the
	// objects it uploads (OnBucket only; the render refuses them elsewhere).
	CacheControl []CacheRule `json:"cache_control,omitempty"`
	// RuntimeConfig is the frontend's declared `runtime_config` RESOLVED
	// to literals by the KCL render (forge.WorkloadURL references lowered
	// to the target's URL). It is layered over the typed per-env config
	// when forge writes config.js. EMPTY on a hosted env, where the control
	// plane resolves references and writes the document itself.
	RuntimeConfig map[string]string `json:"runtime_config,omitempty"`
	// RuntimeConfigSpec is the same declaration in the StaticSite spec's
	// wire shape, references KEPT. The hosted provider publishes it as
	// spec.runtimeConfig.
	RuntimeConfigSpec map[string]deployv1alpha1.RuntimeConfigValue `json:"runtime_config_spec,omitempty"`
}

// FrontendConfigEntity mirrors the kcl/schema.k FrontendConfig — the
// typed, well-known frontend knobs forge maps to framework-specific env
// var names (NEXT_PUBLIC_* / VITE_* / EXPO_PUBLIC_*, plus Next.js's
// NEXT_TELEMETRY_DISABLED) and enforces on the build path. Mock is the
// load-bearing knob: "off" (default) | "true" | "hybrid". See
// frontendConfigEnv for the field→variable mapping and the build-path
// mock force in deploy.go.
type FrontendConfigEntity struct {
	APIURL            string `json:"api_url,omitempty"`
	Mock              string `json:"mock,omitempty"`
	OTELEndpoint      string `json:"otel_endpoint,omitempty"`
	Environment       string `json:"environment,omitempty"`
	TelemetryDisabled bool   `json:"telemetry_disabled,omitempty"`
}

// EffectiveEnvVars is the frontend's env-var stream forge injects at dev
// launch and build time: the config-derived variables (frontendConfigEnv)
// with the explicit EnvVars layered OVER them — an explicit entry for the
// same variable WINS, and forge warns on the collision (the typed config
// knob is being shadowed). A frontend with no `config` block returns its
// EnvVars unchanged, so the legacy env_vars-only shape is byte-identical.
func (f FrontendEntity) EffectiveEnvVars() []KCLEnvVar {
	base := frontendConfigEnv(f.Type, f.Config)
	if len(base) == 0 {
		return f.EnvVars
	}
	idx := make(map[string]int, len(base))
	out := make([]KCLEnvVar, 0, len(base)+len(f.EnvVars))
	for _, ev := range base {
		idx[ev.Name] = len(out)
		out = append(out, ev)
	}
	for _, ev := range f.EnvVars {
		if i, ok := idx[ev.Name]; ok {
			fmt.Printf("  ⚠️  frontend %s: env_vars sets %q, overriding the typed config value for that variable.\n", f.Name, ev.Name)
			out[i] = ev
			continue
		}
		idx[ev.Name] = len(out)
		out = append(out, ev)
	}
	return out
}

// KCLEnvVar is a single env var entry from the rendered KCL. Distinct
// type so we don't pull in the project-config EnvVar (which carries
// codegen-specific fields the KCL renderer doesn't know about).
//
// Three projection channels mirror the KCL EnvVar schema (kcl/schema.k):
//
//   - Value: inline literal. The dominant case host-mode consumes.
//   - SecretRef + SecretKey: cluster-mode projection from a Secret
//     (Deployment.env.valueFrom.secretKeyRef). No host equivalent —
//     host-mode picks the value up from the gitignored secrets_file.
//   - ConfigMapRef + ConfigMapKey: cluster-mode projection from a
//     forge-generated ConfigMap.
//
// SecretRef / ConfigMapRef are surfaced (rather than dropped) so the
// `forge doctor parity` diff can attribute cluster-side projected env
// vars to their source rather than treating an empty Value as "unset".
type KCLEnvVar struct {
	Name         string `json:"name"`
	Value        string `json:"value,omitempty"`
	SecretRef    string `json:"secret_ref,omitempty"`
	SecretKey    string `json:"secret_key,omitempty"`
	ConfigMapRef string `json:"config_map_ref,omitempty"`
	ConfigMapKey string `json:"config_map_key,omitempty"`

	// SecretOptional exempts this var's secret_ref from the store
	// pre-flight. Set by config codegen from `optional: true` on a
	// `sensitive` proto field — never hand-authored.
	SecretOptional bool `json:"secret_optional,omitempty"`
}

// kclRenderRaw is the JSON shape of `output` (P2 map §9.1). Workloads are
// decoded in a second step (kclWorkloadRaw) because their build and runtime
// are tagged unions and their spec is decoded strictly.
type kclRenderRaw struct {
	Project           string                   `json:"project,omitempty"`
	Env               string                   `json:"env,omitempty"`
	ImageTag          string                   `json:"image_tag,omitempty"`
	Lifecycle         string                   `json:"lifecycle,omitempty"`
	Clusters          []ClusterEntity          `json:"clusters,omitempty"`
	ClusterTarget     *ClusterTargetEntity     `json:"cluster_target,omitempty"`
	KubeconfigSecrets []KubeconfigSecretEntity `json:"kubeconfig_secrets,omitempty"`
	Workloads         []kclWorkloadRaw         `json:"workloads,omitempty"`
	Infra             []HostInfraEntity        `json:"infra,omitempty"`
	Frontends         []FrontendEntity         `json:"frontends,omitempty"`
	Gateways          []GatewayEntity          `json:"gateways,omitempty"`
	HTTPRoutes        []HTTPRouteEntity        `json:"http_routes,omitempty"`
	GRPCRoutes        []GRPCRouteEntity        `json:"grpc_routes,omitempty"`
	Databases         []DatabaseEntity         `json:"databases,omitempty"`
	HelmCharts        []HelmChartEntity        `json:"helm_charts,omitempty"`
	SecretProvider    *SecretProviderEntity    `json:"secret_provider,omitempty"`
	RenderedSecrets   []RenderedSecretEntity   `json:"rendered_secrets,omitempty"`
	ControlPlane      *ControlPlaneEntity      `json:"control_plane,omitempty"`
	RequiredSecrets   []ExternalSecretEntity   `json:"required_secrets,omitempty"`
	RequiredDNS       []DNSRecordEntity        `json:"required_dns,omitempty"`
	// Manifests is the applyable stream. Parsed loosely: only kind, name
	// and namespace are read here (see rawManifest).
	Manifests []rawManifest `json:"manifests,omitempty"`
}

// kclWorkloadRaw is one `output.workloads[]` entry before dispatch.
type kclWorkloadRaw struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Image string `json:"image"`
	// BuildImage is the build identity: Image plus the tag the workload
	// pins, if any. Emitted for every built workload on every runtime.
	BuildImage string          `json:"build_image"`
	Build      json.RawMessage `json:"build"`
	Runtime    json.RawMessage `json:"runtime"`
	Spec       json.RawMessage `json:"spec"`
}

// rawManifest is a minimal view of one rendered k8s object — just enough
// to read its kind, name and namespace. The rest of the object is ignored.
type rawManifest struct {
	Kind     string `json:"kind,omitempty"`
	Metadata struct {
		Name      string            `json:"name,omitempty"`
		Namespace string            `json:"namespace,omitempty"`
		Labels    map[string]string `json:"labels,omitempty"`
	} `json:"metadata,omitempty"`
}

// ManifestClusterEntity is one kubectl context the rendered stream stamps an
// object onto (`forge.dev/cluster`), with the namespace its namespaced objects
// dominate there ("" when every object on it is cluster-scoped).
type ManifestClusterEntity struct {
	Cluster   string
	Namespace string
}

// manifestClusters returns every distinct `forge.dev/cluster` stamped on the
// rendered stream, sorted by context. The stamp is what the deploy router
// sends an object by, so every stamped context is a place the deploy WRITES —
// whether or not a workload runs there (a forge.Manifests group, a cluster
// database, a secondary cluster's Namespace).
func manifestClusters(manifests []rawManifest) []ManifestClusterEntity {
	byCluster := map[string][]rawManifest{}
	for _, m := range manifests {
		if c := strings.TrimSpace(m.Metadata.Labels[cluster.ClusterRoutingLabel]); c != "" {
			byCluster[c] = append(byCluster[c], m)
		}
	}
	out := make([]ManifestClusterEntity, 0, len(byCluster))
	for c, ms := range byCluster {
		out = append(out, ManifestClusterEntity{Cluster: c, Namespace: manifestNamespace(ms)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cluster < out[j].Cluster })
	if len(out) == 0 {
		return nil
	}
	return out
}

// ClusterTargetEntity is the rendered `cluster_target` (kcl/render.k
// `_render_cluster_target`): the env-wide coordinates a Bundle declares once.
// Only the fields forge resolves env-wide facts from are read.
type ClusterTargetEntity struct {
	Cluster   string `json:"cluster,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Registry  string `json:"registry,omitempty"`
	Domain    string `json:"domain,omitempty"`
	Platform  string `json:"platform,omitempty"`
	// ConnectedCluster is the control-plane cluster this target names, by
	// the name `forge cluster connect` registered. Empty for a k3d cluster,
	// which forge applies to directly.
	ConnectedCluster string `json:"connected_cluster,omitempty"`
}

// field returns one env-wide coordinate by the name the "first K8sCluster
// field" resolvers use, or "" when the target does not declare it.
func (t *ClusterTargetEntity) field(name string) string {
	if t == nil {
		return ""
	}
	switch name {
	case "cluster":
		return t.Cluster
	case "namespace":
		return t.Namespace
	case "registry":
		return t.Registry
	case "domain":
		return t.Domain
	case "platform":
		return t.Platform
	}
	return ""
}

// RenderKCL shells `kcl run deploy/kcl/<env>/ -o json`, parses the
// output, and dispatches each service's deploy block by Type into the
// right pointer. Returns an error when:
//
//   - The KCL directory doesn't exist (env not configured)
//   - `kcl` is not on PATH (caller needs to install it)
//   - The JSON output is malformed
//   - A service's deploy.type is none of "host"/"cluster"/"build-only"
//
// The override env var FORGE_KCL_RENDER_FIXTURE points at a JSON file
// whose contents are read in lieu of shelling kcl. Used by unit tests so
// they can exercise the dispatch logic without a real KCL toolchain.
func RenderKCL(ctx context.Context, projectDir, env string) (*KCLEntities, error) {
	return RenderKCLWith(ctx, projectDir, env, nil)
}

// RenderKCLWith is RenderKCL plus additional forge-derived `-D` bindings.
//
// It exists for the build render, which must bind values a plain render cannot
// know: `image_tag` (the tag THIS build resolved, which outranks the env's own
// default) and `target_arch`. Both are read by the KCL that composes a
// ShellBuild's `cmd`, and that command is run verbatim — so if forge did not
// bind them before the render, the command string would carry the wrong tag or
// arch and there is no later substitution pass to correct it.
//
// extra is `key=value` with the value already KCL-quoted by the caller, the
// same contract renderKCLRaw's own dArgs use. A key the caller binds here wins
// over nothing else: these are reserved names no project may set.
func RenderKCLWith(ctx context.Context, projectDir, env string, extra []string) (*KCLEntities, error) {
	raw, err := renderKCLRaw(ctx, projectDir, env, extra...)
	if err != nil {
		return nil, err
	}
	entities, err := parseKCLEntities(raw)
	if err != nil {
		return nil, err
	}
	// Refused HERE, at the render, because this is the one place every path
	// that could ship the env passes through — and the author is still
	// looking at the KCL that is wrong. See refuseUnboundClusterTargets.
	if err := refuseUnboundClusterTargets(env, entities); err != nil {
		return nil, err
	}
	return entities, nil
}

// renderKCLRaw is the side-effecting half — shell or fixture file —
// kept separate so parseKCLEntities is unit-testable from a literal []byte.
//
// The env name is passed to KCL as `-D env=<env>` so user main.k files
// can conditionally include manifests via the `option("env")` builtin
// (e.g. only ship in-cluster NATS to k3d, skip it for dev-host where
// docker-compose provides it).
func renderKCLRaw(ctx context.Context, projectDir, env string, extra ...string) ([]byte, error) {
	if fixture := os.Getenv("FORGE_KCL_RENDER_FIXTURE"); fixture != "" {
		return os.ReadFile(fixture)
	}
	if env == "" {
		return nil, fmt.Errorf("RenderKCL: env required")
	}
	kclDir := filepath.Join(projectDir, "deploy", "kcl", env)
	if _, err := os.Stat(kclDir); err != nil {
		return nil, fmt.Errorf("kcl dir %s: %w", kclDir, err)
	}
	// Render the JSON contract through the shared embedded kpm + kcl-go
	// seam (no external `kcl` binary). `-D env=<env>` drives the per-env
	// conditionals in the deploy module. workDir = projectDir so the
	// deploy-as-data main.k's `file.read("deploy/kcl/...")` resolves.
	// option("worktree")/option("branch") are bound inside kclrender.Run for
	// EVERY render (withDevStackDArgs), not here — a project keys its
	// namespace on them, so a render that omits them looks in a different
	// namespace than the deploy that applied the objects.
	// activeRenderOptionDArgs() adds the project's own `-D name=value` options
	// (`forge env up -D …`) — opaque to forge, meaningful only to this env's
	// KCL. nil unless the caller passed one, so every other render is
	// unchanged. Deliberately NOT added to the manifest render in
	// internal/cluster: those options are bound by `env up` only, and a
	// cluster apply must render from the repo alone.
	// Every render READS the resolve_port store, so a port allocated once
	// stays the same port for every command that asks afterwards.
	//
	// Only `env up` / `env deploy` used to arm it (activateDevStack, which
	// arms the WRITABLE store and is a no-op to re-arm here). Every other
	// render re-probed from scratch — and a probe cannot distinguish "busy
	// because a stranger took it" from "busy because THIS stack's own
	// postgres is serving on it", so it stepped off the very port it had
	// been asked about. `forge env config dev` reported one DSN while the
	// running stack was on another, and `forge db reset` got a dead port.
	// The store is the tie-break, and it only works if everyone reads it.
	kclplugin.UsePortStoreReadOnly(filepath.Join(projectDir, ".forge", "ports-"+env+".json"))

	dArgs := append([]string{"env=" + env}, activeRenderOptionDArgs()...)
	dArgs = append(dArgs, extra...)
	return kclrender.Run(projectDir, kclDir, dArgs)
}

// parseKCLEntities turns a render's JSON into the typed entity set.
//
// The document MUST be `{"output": {...}}` — the single entrypoint
// `output = forge.render(bundle)`. A top-level `manifests` var is REFUSED:
// the stream now carries forge.dev Workload records that only forge can
// expand, so a second, directly-applyable top-level stream would be a path
// that looks right and silently is not. A contract with neither key (a
// hand-written fixture of the bare `output` object) parses as that object.
func parseKCLEntities(data []byte) (*KCLEntities, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return &KCLEntities{}, nil
	}
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return nil, fmt.Errorf("parse kcl json: %w", err)
	}
	if _, ok := wrapper["manifests"]; ok {
		if _, isOutput := wrapper["workloads"]; !isOutput {
			return nil, fmt.Errorf("kcl render has a top-level `manifests` var: main.k must end with `output = forge.render(bundle)`, " +
				"the one entrypoint (the applyable stream is output.manifests, expanded by forge)")
		}
	}
	if inner, ok := wrapper["output"]; ok && len(inner) > 0 {
		data = inner
	}
	var raw kclRenderRaw
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse kcl json: %w", err)
	}
	out := &KCLEntities{
		Project:              raw.Project,
		Env:                  raw.Env,
		ImageTag:             raw.ImageTag,
		Lifecycle:            raw.Lifecycle,
		Clusters:             raw.Clusters,
		ClusterTarget:        raw.ClusterTarget,
		KubeconfigSecrets:    raw.KubeconfigSecrets,
		Infra:                raw.Infra,
		Frontends:            raw.Frontends,
		Gateways:             raw.Gateways,
		HTTPRoutes:           raw.HTTPRoutes,
		GRPCRoutes:           raw.GRPCRoutes,
		HelmCharts:           raw.HelmCharts,
		Databases:            raw.Databases,
		SecretProvider:       raw.SecretProvider,
		RenderedSecrets:      raw.RenderedSecrets,
		ControlPlane:         raw.ControlPlane,
		RequiredSecrets:      raw.RequiredSecrets,
		RequiredDNS:          raw.RequiredDNS,
		ManifestNamespace:    manifestNamespace(raw.Manifests),
		ManifestServiceNames: manifestServiceNames(raw.Manifests),
		ManifestClusters:     manifestClusters(raw.Manifests),
	}
	seen := map[string]bool{}
	for _, w := range raw.Workloads {
		ent, err := decodeWorkload(w)
		if err != nil {
			return nil, err
		}
		if seen[ent.Name] {
			return nil, fmt.Errorf("workload %q is declared twice: a workload name is unique across the env", ent.Name)
		}
		seen[ent.Name] = true
		out.Workloads = append(out.Workloads, ent)
	}
	return out, nil
}

// decodeWorkload dispatches one raw workload: its runtime and build unions,
// and its spec, STRICTLY. A spec key forge's WorkloadSpec does not know is an
// error rather than silently dropped — a dropped field is exactly the drift
// one wire type exists to make impossible, and the hosted control plane
// decodes the same bytes the same way.
func decodeWorkload(w kclWorkloadRaw) (WorkloadEntity, error) {
	if strings.TrimSpace(w.Name) == "" {
		return WorkloadEntity{}, fmt.Errorf("a workload in output.workloads has no name")
	}
	rt, err := dispatchRuntime(w.Name, w.Runtime)
	if err != nil {
		return WorkloadEntity{}, err
	}
	build, err := dispatchBuild(w.Name, w.Build)
	if err != nil {
		return WorkloadEntity{}, err
	}
	var spec deployv1alpha1.WorkloadSpec
	if trimmed := bytes.TrimSpace(w.Spec); len(trimmed) > 0 && string(trimmed) != "null" {
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&spec); err != nil {
			return WorkloadEntity{}, fmt.Errorf("workload %q: decode spec: %w", w.Name, err)
		}
	}
	kind := string(spec.EffectiveKind())
	if w.Kind != "" && w.Kind != kind {
		return WorkloadEntity{}, fmt.Errorf("workload %q: kind %q disagrees with spec.kind %q", w.Name, w.Kind, kind)
	}
	return WorkloadEntity{Name: w.Name, Kind: kind, Image: w.Image, BuildImage: w.BuildImage, Build: build, Runtime: rt, Spec: spec}, nil
}

// manifestNamespace returns the namespace that dominates the rendered
// stream. A forge render's namespaced objects all carry the env's
// namespace, while cluster-scoped objects (Namespace, CRD, ClusterRole)
// carry none and are ignored. If the stream spans several namespaces the
// most frequent wins (lexical tiebreak), so a stray cross-namespace object
// cannot hijack the result. "" when no namespaced object exists.
func manifestNamespace(manifests []rawManifest) string {
	counts := map[string]int{}
	for _, m := range manifests {
		if ns := strings.TrimSpace(m.Metadata.Namespace); ns != "" {
			counts[ns]++
		}
	}
	best, bestN := "", 0
	for ns, n := range counts {
		if n > bestN || (n == bestN && ns < best) {
			best, bestN = ns, n
		}
	}
	return best
}

// manifestServiceNames collects the metadata.name of every raw k8s Service
// (kind == "Service") in the rendered stream. Nil when there are none.
func manifestServiceNames(manifests []rawManifest) []string {
	var names []string
	for _, m := range manifests {
		if m.Kind != "Service" {
			continue
		}
		if n := strings.TrimSpace(m.Metadata.Name); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// dispatchRuntime decodes a workload's resolved runtime block by its `type`
// tag. A missing or unknown runtime fails loud: a workload with no runtime is
// a workload nothing runs, and guessing one would put it somewhere its author
// did not say.
func dispatchRuntime(name string, raw json.RawMessage) (RuntimeEntity, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return RuntimeEntity{}, fmt.Errorf("workload %q: no runtime (every workload in an env binds its own)", name)
	}
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return RuntimeEntity{}, fmt.Errorf("workload %q: parse runtime.type: %w", name, err)
	}
	decode := func(v any) error {
		if err := json.Unmarshal(trimmed, v); err != nil {
			return fmt.Errorf("workload %q: parse %s runtime: %w", name, probe.Type, err)
		}
		return nil
	}
	switch probe.Type {
	case RuntimeHost:
		var h HostRuntime
		return RuntimeEntity{Type: RuntimeHost, Host: &h}, decode(&h)
	case RuntimeCompose:
		var c ComposeRuntime
		return RuntimeEntity{Type: RuntimeCompose, Compose: &c}, decode(&c)
	case RuntimeCluster:
		var c ClusterRuntime
		return RuntimeEntity{Type: RuntimeCluster, Cluster: &c}, decode(&c)
	case RuntimeHosted:
		return RuntimeEntity{Type: RuntimeHosted}, nil
	case RuntimeBuildOnly:
		var b BuildOnlyDeploy
		return RuntimeEntity{Type: RuntimeBuildOnly, BuildOnly: &b}, decode(&b)
	case "":
		return RuntimeEntity{}, fmt.Errorf("workload %q: runtime.type missing (expected host/compose/cluster/hosted/build-only)", name)
	default:
		return RuntimeEntity{}, fmt.Errorf("workload %q: unrecognised runtime.type %q (expected host/compose/cluster/hosted/build-only)", name, probe.Type)
	}
}

// dispatchBuild unmarshals the raw build block, reads the type
// discriminator, and populates exactly one of the three pointers in the
// returned BuildConfigEntity. An absent / null build block yields the zero
// value (Type==""): the workload is not built by forge, and nothing is
// synthesized. Unrecognised non-empty types fail loud — a bad KCL render
// should not silently fall back.
func dispatchBuild(svcName string, raw json.RawMessage) (BuildConfigEntity, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return BuildConfigEntity{}, nil
	}
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return BuildConfigEntity{}, fmt.Errorf("workload %q: parse build.type: %w", svcName, err)
	}
	switch strings.ToLower(strings.TrimSpace(probe.Type)) {
	case "go":
		var g GoBuild
		if err := json.Unmarshal(raw, &g); err != nil {
			return BuildConfigEntity{}, fmt.Errorf("workload %q: parse go build: %w", svcName, err)
		}
		return BuildConfigEntity{Type: "go", Go: &g}, nil
	case "docker":
		var d DockerBuild
		if err := json.Unmarshal(raw, &d); err != nil {
			return BuildConfigEntity{}, fmt.Errorf("workload %q: parse docker build: %w", svcName, err)
		}
		return BuildConfigEntity{Type: "docker", Docker: &d}, nil
	case "shell":
		var sh ShellBuild
		if err := json.Unmarshal(raw, &sh); err != nil {
			return BuildConfigEntity{}, fmt.Errorf("workload %q: parse shell build: %w", svcName, err)
		}
		return BuildConfigEntity{Type: "shell", Shell: &sh}, nil
	case "remote":
		var rb RemoteBuild
		if err := json.Unmarshal(raw, &rb); err != nil {
			return BuildConfigEntity{}, fmt.Errorf("workload %q: parse remote build: %w", svcName, err)
		}
		return BuildConfigEntity{Type: "remote", Remote: &rb}, nil
	case "":
		return BuildConfigEntity{}, fmt.Errorf("workload %q: build.type missing (expected go/docker/shell/remote)", svcName)
	default:
		return BuildConfigEntity{}, fmt.Errorf("workload %q: unrecognised build.type %q (expected go/docker/shell/remote)", svcName, probe.Type)
	}
}

// shell returns this workload's ShellBuild, or nil when it is not built by
// a shell command. EffectiveBuildCmd / Cwd / Env read off it, and the
// external-build dispatcher selects workloads for which it is non-nil.
func (w WorkloadEntity) shell() *ShellBuild {
	if w.Build.Type == "shell" {
		return w.Build.Shell
	}
	return nil
}

// EffectiveBuildCmd is the shell build command, or "" for a workload that
// is not shell-built (the external-build dispatcher's "skip" signal).
func (w WorkloadEntity) EffectiveBuildCmd() string {
	if sh := w.shell(); sh != nil {
		return sh.Cmd
	}
	return ""
}

// EffectiveBuildCwd is the shell build's working directory (empty => the
// project root). "" for a non-shell build.
func (w WorkloadEntity) EffectiveBuildCwd() string {
	if sh := w.shell(); sh != nil {
		return sh.Cwd
	}
	return ""
}

// EffectiveBuildEnv is the env merged into the shell build command's
// environment and substitution map. nil for a non-shell build.
func (w WorkloadEntity) EffectiveBuildEnv() map[string]string {
	if sh := w.shell(); sh != nil {
		return sh.Env
	}
	return nil
}

// GoBuild returns the workload's GoBuild, or nil.
func (w WorkloadEntity) GoBuild() *GoBuild {
	if w.Build.Type == "go" {
		return w.Build.Go
	}
	return nil
}

// LongRunning reports whether the workload is a process that stays up
// (service, worker, operator) rather than one that runs to completion (job,
// cron) or never runs (tool).
func (w WorkloadEntity) LongRunning() bool {
	switch deployv1alpha1.WorkloadKind(w.Kind) {
	case deployv1alpha1.KindService, deployv1alpha1.KindWorker, deployv1alpha1.KindOperator:
		return true
	}
	return false
}

// IsJob reports whether the workload is a one-shot job.
func (w WorkloadEntity) IsJob() bool {
	return deployv1alpha1.WorkloadKind(w.Kind) == deployv1alpha1.KindJob
}

// OnRuntime reports whether the workload is bound to the named runtime.
func (w WorkloadEntity) OnRuntime(runtime string) bool { return w.Runtime.Type == runtime }

// EnvVars projects the workload's spec.env onto forge's KCLEnvVar channels,
// so the secret preflight, namespace guards and host env layering read one
// shape. A value maps to value, a SecretRef to secret_ref/secret_key, and a
// ConfigMapRef to config_map_ref/config_map_key.
//
// ManagedSecret, DatabaseRef, WorkloadURL and FieldRef are NOT projected,
// deliberately. ManagedSecret and DatabaseRef read Secrets the env's secret
// store does not render by name (forge-managed-secrets is materialized from
// the managed store, "<db>-app" is published by CloudNativePG), so
// pre-flighting them as store keys would report each as missing. An
// unresolved WorkloadURL has no value yet, and a FieldRef exists only inside
// a pod.
func (w WorkloadEntity) EnvVars() []KCLEnvVar {
	var out []KCLEnvVar
	for _, e := range w.Spec.Env {
		switch {
		case e.SecretRef != nil:
			out = append(out, KCLEnvVar{Name: e.Name, SecretRef: e.SecretRef.Name, SecretKey: e.SecretRef.Key, SecretOptional: e.SecretRef.Optional})
		case e.ConfigMapRef != nil:
			out = append(out, KCLEnvVar{Name: e.Name, ConfigMapRef: e.ConfigMapRef.Name, ConfigMapKey: e.ConfigMapRef.Key})
		case e.ManagedSecret == nil && e.DatabaseRef == nil && e.WorkloadURL == nil && e.FieldRef == nil:
			out = append(out, KCLEnvVar{Name: e.Name, Value: e.Value})
		}
	}
	return out
}

// HostEnv is the literal env a HOST launch of this workload receives from its
// declaration: every spec.env value channel, with the runtime's per-run
// LaunchEnv layered on top. Secret channels resolve separately (scoped
// through the secret provider), and a reference with no host literal
// (ConfigMapRef, FieldRef, DatabaseRef, ManagedSecret, WorkloadURL) has
// nothing to contribute here.
func (w WorkloadEntity) HostEnv() map[string]string {
	out := map[string]string{}
	for _, ev := range w.EnvVars() {
		if ev.Name != "" && ev.Value != "" && ev.SecretRef == "" && ev.ConfigMapRef == "" {
			out[ev.Name] = ev.Value
		}
	}
	if w.Runtime.Host != nil {
		for k, v := range w.Runtime.Host.LaunchEnv {
			out[k] = v
		}
	}
	return out
}

// HostPorts is every TCP port a HOST launch of this workload binds, in
// declaration order and de-duplicated: the runtime's listen_ports when
// declared (an EMPTY list is the statement "binds nothing"), else the
// workload's spec.ports. There is no env-var heuristic: a workload declares
// its ports, and guessing them from *_PORT variables misread dependency
// addresses (TEMPORAL_PORT) as bind ports.
func (w WorkloadEntity) HostPorts() []int {
	var in []int
	if h := w.Runtime.Host; h != nil && h.ListenPorts != nil {
		in = *h.ListenPorts
	} else {
		for _, p := range w.Spec.Ports {
			in = append(in, int(p.Port))
		}
	}
	var out []int
	seen := map[int]bool{}
	for _, p := range in {
		if p <= 0 || p >= 65536 || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// HostPort is the workload's canonical (summary / URL) host port: the first
// declared listen port, else its `http` port, else its first port. 0 when it
// binds nothing.
func (w WorkloadEntity) HostPort() int {
	if h := w.Runtime.Host; h != nil && h.ListenPorts != nil {
		if ps := w.HostPorts(); len(ps) > 0 {
			return ps[0]
		}
		return 0
	}
	for _, p := range w.Spec.Ports {
		if p.Name == deployv1alpha1.DefaultHTTPPortName {
			return int(p.Port)
		}
	}
	if ps := w.HostPorts(); len(ps) > 0 {
		return ps[0]
	}
	return 0
}

// FindWorkload returns the named workload, or nil.
func (e *KCLEntities) FindWorkload(name string) *WorkloadEntity {
	if e == nil {
		return nil
	}
	for i := range e.Workloads {
		if e.Workloads[i].Name == name {
			return &e.Workloads[i]
		}
	}
	return nil
}

// WorkloadNames returns the names of every workload bound to the named
// runtime (RuntimeHost, RuntimeCluster, ...), in declaration order. An
// empty runtime returns every workload's name.
func (e *KCLEntities) WorkloadNames(runtime string) []string {
	if e == nil {
		return nil
	}
	var out []string
	for _, w := range e.Workloads {
		if runtime == "" || w.Runtime.Type == runtime {
			out = append(out, w.Name)
		}
	}
	return out
}

// WorkloadsOn returns the workloads bound to the named runtime, in
// declaration order.
func (e *KCLEntities) WorkloadsOn(runtime string) []WorkloadEntity {
	if e == nil {
		return nil
	}
	var out []WorkloadEntity
	for _, w := range e.Workloads {
		if w.Runtime.Type == runtime {
			out = append(out, w)
		}
	}
	return out
}

// HasHosted reports whether anything in the env is bound to the control
// plane: a Hosted workload, a hosted database, or an OnHosted frontend.
func (e *KCLEntities) HasHosted() bool {
	if e == nil {
		return false
	}
	for _, w := range e.Workloads {
		if w.OnRuntime(RuntimeHosted) {
			return true
		}
	}
	for _, d := range e.Databases {
		if d.Hosted() {
			return true
		}
	}
	for _, f := range e.Frontends {
		if frontendIsHosted(f) {
			return true
		}
	}
	return false
}

// frontendIsHosted reports whether the frontend is bound to forge.OnHosted:
// published to the control plane as a StaticSite.
func frontendIsHosted(f FrontendEntity) bool {
	return f.Runtime.Type == FrontendRuntimeHosted
}
