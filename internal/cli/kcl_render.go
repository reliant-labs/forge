package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/kclrender"
)

// KCLEntities is the typed, dispatched view of the JSON the sibling
// KCL deploy module emits. The typed schema module exports the
// polymorphic `deploy: HostDeploy | K8sCluster | External | Compose |
// BuildOnly` union per service; the JSON discriminator is
// `deploy.type ∈ {"host","cluster","external","compose","build-only"}`
// (services only — operators/cronjobs are always cluster-shaped).
//
// Callers (`forge build --env`, `forge deploy <env>`, `forge up --env`,
// `forge run <svc>`) read this rather than reaching back into forge.yaml
// because deployment placement is a per-env decision that lives in the
// KCL layer, not on services[] in the project config.
type KCLEntities struct {
	// Clusters are the k3d clusters forge ensures exist at the head of
	// `forge up` before any workload deploys. Empty for an env that
	// declares no clusters (today's no-ensure behavior). Ownership is
	// implicit via Cluster.Network / Cluster.RegistryMirror — there is
	// no "primary" cluster.
	Clusters   []ClusterEntity   `json:"clusters,omitempty"`
	Services   []ServiceEntity   `json:"services,omitempty"`
	Operators  []OperatorEntity  `json:"operators,omitempty"`
	Frontends  []FrontendEntity  `json:"frontends,omitempty"`
	CronJobs   []CronJobEntity   `json:"cronjobs,omitempty"`
	Gateways   []GatewayEntity   `json:"gateways,omitempty"`
	HTTPRoutes []HTTPRouteEntity `json:"http_routes,omitempty"`
	GRPCRoutes []GRPCRouteEntity `json:"grpc_routes,omitempty"`
	// SecretProvider is the bundle-level secret provider declaration
	// (WHERE secret values come from for this env). Nil when the bundle
	// declares no provider — preserving today's no-provider behavior.
	SecretProvider *SecretProviderEntity `json:"secret_provider,omitempty"`

	// ManifestNamespace is the namespace stamped on the rendered k8s
	// manifests (`manifests[].metadata.namespace`), recovered even when
	// the project's main.k omits the `output = forge.render(_bundle)`
	// entity echo. Some projects deliberately render only `manifests`
	// (e.g. to keep the deployable image refs single-prefixed), which
	// leaves the entity contract — and therefore every cluster-shaped
	// service's K8sCluster.namespace — absent. We derive the namespace
	// from the manifests so the declared-namespace resolution
	// (k8sClusterNamespaceForEnv → forge deploy/smoke/secrets) keeps
	// working without forcing the user to echo `output` or pass
	// --namespace. Empty when the render carries no namespaced manifests.
	ManifestNamespace string `json:"-"`
}

// SecretProviderEntity is the parsed bundle-level secret provider
// declaration. Type is "dotenv" | "external". Path is the dotenv path
// (dotenv only), resolved relative to the project root by the CLI.
type SecretProviderEntity struct {
	Type string `json:"type"`
	Path string `json:"path,omitempty"`
}

// ClusterEntity mirrors the kcl/schema.k Cluster — a k3d cluster forge
// ensures exists before deploying. The reconcile (clusterPhase) reads
// these and runs `k3d cluster create` for any that are absent.
//
// Ownership is IMPLICIT: a secondary cluster sets Network to the owner
// cluster's network (`k3d-<owner>`) and RegistryMirror="inherit" to
// reuse the owner's registry. The owner declares neither. There is no
// "primary" field and no most-X heuristic.
type ClusterEntity struct {
	Name           string `json:"name"`
	Config         string `json:"config,omitempty"`
	Network        string `json:"network,omitempty"`
	RegistryMirror string `json:"registry_mirror,omitempty"`
	Servers        int    `json:"servers,omitempty"`
	Agents         int    `json:"agents,omitempty"`
	APIPort        int    `json:"api_port,omitempty"`
}

// GatewayEntity mirrors the kcl/schema.k Gateway. Listeners are inlined.
// Tls is nil when the gateway is plaintext.
type GatewayEntity struct {
	Name             string                  `json:"name"`
	GatewayClassName string                  `json:"gateway_class_name,omitempty"`
	Host             string                  `json:"host,omitempty"`
	TLS              *GatewayTLSEntity       `json:"tls,omitempty"`
	Listeners        []GatewayListenerEntity `json:"listeners,omitempty"`
	RawPolicy        string                  `json:"raw_policy,omitempty"`
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
type GatewayListenerEntity struct {
	Name       string `json:"name"`
	Port       int    `json:"port"`
	Protocol   string `json:"protocol"`
	PathPrefix string `json:"path_prefix,omitempty"`
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

// HTTPRouteEntity mirrors the kcl/schema.k HTTPRoute. Service is a
// backend Service name; Port is the backend port.
type HTTPRouteEntity struct {
	Name      string `json:"name"`
	Gateway   string `json:"gateway"`
	Listener  string `json:"listener"`
	Service   string `json:"service"`
	Port      int    `json:"port"`
	Host      string `json:"host,omitempty"`
	Path      string `json:"path,omitempty"`
	RawPolicy string `json:"raw_policy,omitempty"`
}

// GRPCRouteEntity mirrors the kcl/schema.k GRPCRoute. Shape matches
// HTTPRouteEntity — the distinction is the rendered Gateway API
// resource kind (GRPCRoute vs HTTPRoute).
type GRPCRouteEntity struct {
	Name      string `json:"name"`
	Gateway   string `json:"gateway"`
	Listener  string `json:"listener"`
	Service   string `json:"service"`
	Port      int    `json:"port"`
	Host      string `json:"host,omitempty"`
	Path      string `json:"path,omitempty"`
	RawPolicy string `json:"raw_policy,omitempty"`
}

// ServiceEntity is one service from rendered KCL. The Deploy field is
// polymorphic — exactly one of Host / Cluster / BuildOnly is populated
// according to Deploy.Type. See [DeployConfigEntity] for the discriminator.
//
// Build-side fields (mirror of the External deploy provider for the
// build step):
//
//   - BuildCmd, when non-empty, is the shell command `forge build`
//     runs via `sh -c` instead of the built-in Go-build pipeline.
//     Tokens (${IMAGE}/${TAG}/${SERVICE}/${TARGETARCH}/${REGISTRY}/
//     ${PROJECT_DIR}/${BUILD_CWD} + keys from BuildEnv) are substituted
//     by the build-side runner (see internal/buildtarget once Phase 2
//     lands).
//   - BuildCwd is the working directory the shell command runs from.
//     Relative paths resolve against the project root. Skip-with-warn
//     when the resolved path doesn't exist on disk.
//   - BuildEnv carries extra env vars merged into the command's
//     environment AND added to the substitution map (built-ins win
//     on conflict).
type ServiceEntity struct {
	Name string `json:"name"`
	// Image is the (registry-less) image name. ImageTag, when set, is the
	// per-service tag PIN the KCL render layer stamps instead of the
	// env-wide tag — surfaced here so audit / parity consumers can see the
	// pin rather than inferring an untagged image. The rendered image ref
	// (registry + tag resolution) is built KCL-side in _image_ref; this is
	// the declaration, not the resolved ref.
	Image    string             `json:"image,omitempty"`
	ImageTag string             `json:"image_tag,omitempty"`
	Deploy   DeployConfigEntity `json:"deploy"`
	// Build is the polymorphic build declaration — exactly one of
	// Go / Docker / Shell is populated according to Build.Type. Mirrors
	// Deploy. When the KCL `build` block is absent (a hand-authored
	// forge.Service that omits it) Build.Type is "" and callers
	// synthesize the GoBuild default via [ServiceEntity.EffectiveBuild].
	Build    BuildConfigEntity `json:"-"`
	EnvVars  []KCLEnvVar       `json:"env_vars,omitempty"`
	Command  []string          `json:"command,omitempty"`
	BuildCmd string            `json:"build_cmd,omitempty"`
	BuildCwd string            `json:"build_cwd,omitempty"`
	BuildEnv map[string]string `json:"build_env,omitempty"`
}

// BuildConfigEntity is the dispatched-by-type view of a service's build
// block — the build-side analogue of [DeployConfigEntity]. The raw JSON
// is a tagged union; Type carries the tag; exactly one of Go/Docker/Shell
// is non-nil after [dispatchServiceBuild] runs. Type=="" means the KCL
// `build` block was absent (null) — callers fall back to the synthesized
// GoBuild default.
type BuildConfigEntity struct {
	Type   string       // "go" | "docker" | "shell" | "" (absent)
	Go     *GoBuild     // populated when Type=="go"
	Docker *DockerBuild // populated when Type=="docker"
	Shell  *ShellBuild  // populated when Type=="shell"
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
type DockerBuild struct {
	OutputName string            `json:"output_name,omitempty"`
	Dockerfile string            `json:"dockerfile,omitempty"`
	Platform   string            `json:"platform,omitempty"`
	Target     string            `json:"target,omitempty"`
	BuildArgs  map[string]string `json:"build_args,omitempty"`
}

// ShellBuild mirrors the kcl/schema.k ShellBuild — a verbatim `sh -c`
// build command (generalizes the old build_cmd escape hatch).
type ShellBuild struct {
	OutputName string `json:"output_name,omitempty"`
	Cmd        string `json:"cmd"`
}

// DeployConfigEntity is the dispatched-by-type view of a service's
// deploy block. The raw JSON shape is a tagged union — Type carries
// the tag; exactly one of Host/Cluster/External/Compose/BuildOnly is
// non-nil after [dispatchServiceDeploy] runs.
type DeployConfigEntity struct {
	Type      string           // "host" | "cluster" | "external" | "compose" | "build-only"
	Host      *HostDeploy      // populated when Type=="host"
	Cluster   *K8sCluster      // populated when Type=="cluster"
	External  *ExternalDeploy  // populated when Type=="external"
	Compose   *ComposeDeploy   // populated when Type=="compose"
	BuildOnly *BuildOnlyDeploy // populated when Type=="build-only"
}

// ExternalDeploy is the deploy block for a generic shell-command
// deploy target — Fly.io / Cloudflare Workers / Cloud Run / ECS /
// Vercel / etc. The forge-side ExternalProvider exec's DeployCmd via
// `sh -c` after substituting ${IMAGE}/${TAG}/${SERVICE}/etc. and runs
// HealthCmd / RollbackCmd through the same path.
type ExternalDeploy struct {
	DeployCmd   string `json:"deploy_cmd,omitempty"`
	RollbackCmd string `json:"rollback_cmd,omitempty"`
	HealthCmd   string `json:"health_cmd,omitempty"`
	// BuildCmd is the build-side mirror of DeployCmd: the shell command
	// `forge build -t external` exec's to construct the deployable
	// image. Optional — External targets without it build their image
	// out-of-band and only deploy through forge. Token set matches
	// DeployCmd (IMAGE/TAG/CODE_VERSION/PROJECT_DIR/ENV/SERVICE + Env
	// keys). See [ServiceEntity.EffectiveBuildCmd].
	BuildCmd string            `json:"build_cmd,omitempty"`
	EnvFile  string            `json:"env_file,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
}

// ComposeDeploy is the deploy block for a docker-compose service.
type ComposeDeploy struct {
	ComposeFile string `json:"compose_file,omitempty"`
	Service     string `json:"service,omitempty"`
	EnvFile     string `json:"env_file,omitempty"`
}

// HostDeploy is the deploy block for a service that runs as a host
// process during `forge up --env=<env>`. The Runner field selects the
// dispatch (go-run / air / binary / delve) and is consumed by
// [runHostServiceWithRunner] + the up orchestrator.
//
// Env composition splits config from secrets:
//
//   - EnvVars: KCL-declared per-env config (DATABASE_URL, NATS_URL,
//     LOG_LEVEL, …). Reproducible, version-controlled.
//   - SecretsFile: path to a gitignored dotenv carrying JUST secrets
//     (STRIPE_*, SUPABASE_*, JWT_PUBLIC_KEY, …). Loaded first; EnvVars
//     is layered on top so KCL wins on conflict.
//
// Previously HostDeploy carried a single `env_file` that conflated
// config and secrets and silently drifted from K8sCluster services
// (which already saw config via the Deployment's `env` block).
type HostDeploy struct {
	Runner      string      `json:"runner,omitempty"`       // "go-run" | "air" | "binary" | "delve"
	AirConfig   string      `json:"air_config,omitempty"`   // path relative to project root, default .air.toml
	EnvVars     []KCLEnvVar `json:"env_vars,omitempty"`     // KCL-declared per-env config
	SecretsFile string      `json:"secrets_file,omitempty"` // path relative to project root; gitignored dotenv
	DelvePort   int         `json:"delve_port,omitempty"`   // when Runner=="delve"; default 2345
	// WorkingDir overrides the launched subprocess's working directory.
	// Relative paths resolve against the project root. Use this for
	// cross-repo binaries whose runner config (e.g. Air's build_cmd
	// paths) resolves relative to a sibling repo. Default: project root.
	WorkingDir string `json:"working_dir,omitempty"`
}

// K8sCluster is the deploy block for a cluster-mode service. Mirrors
// the JSON contract emitted by `_render_k8s_cluster` in kcl/render.k.
//
// Cluster/Namespace/Registry are mandatory env-wide fields the
// KCL-side `K8sCluster` schema declares as required — an empty value
// here indicates a malformed render rather than a legacy shape.
//
// Ingress used to be a per-service field on this struct; it now lives
// at the Bundle level as Gateway/HTTPRoute/GRPCRoute (see
// KCLEntities.Gateways etc.). Routes reference services by name.
type K8sCluster struct {
	// Env-wide knobs — same value across every service in a deploy
	// group.
	Cluster   string `json:"cluster,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Registry  string `json:"registry,omitempty"`
	Domain    string `json:"domain,omitempty"`

	// Per-service knobs.
	Replicas int         `json:"replicas,omitempty"`
	Platform string      `json:"platform,omitempty"` // GOARCH override; empty = use forge.yaml deploy.target_arch
	Ports    []int       `json:"ports,omitempty"`
	EnvVars  []KCLEnvVar `json:"env_vars,omitempty"`
}

// BuildOnlyDeploy is the deploy block for services that produce
// binaries but never get a Deployment — sidecars, CLI builds shipped
// in a release artifact, etc. BuildVariants lets one service emit
// multiple binaries (different ldflags / build tags).
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

// OperatorEntity is one operator from rendered KCL. Operators are
// always cluster-mode (no host/build-only equivalent) so the type is
// flat.
type OperatorEntity struct {
	Name           string      `json:"name"`
	Image          string      `json:"image,omitempty"`
	CRDs           []string    `json:"crds,omitempty"`
	ClusterRBAC    *RBACSpec   `json:"cluster_rbac,omitempty"`
	LeaderElection bool        `json:"leader_election,omitempty"`
	Replicas       int         `json:"replicas,omitempty"`
	Platform       string      `json:"platform,omitempty"`
	EnvVars        []KCLEnvVar `json:"env_vars,omitempty"`
}

// RBACSpec is a placeholder for an operator's cluster RBAC. We only
// surface that it's set; the actual RBAC content is consumed by the KCL
// renderer that produces the YAML manifests.
type RBACSpec struct{}

// FrontendEntity is one frontend from rendered KCL. Frontends are
// host-only in the dev loop (no in-cluster Deployment for the dev env);
// the DevRunner field selects npm/pnpm/yarn.
//
// Deploy is the optional discriminator that lets `forge build` skip the
// production build for frontends that ship via host-mode dev server
// only (no production artifact ever consumed). When absent (legacy
// projects whose KCL doesn't emit a frontend `deploy` block) callers
// fall back to "always build", preserving the pre-discriminator
// behaviour. Unlike ServiceEntity.Deploy this is a thin Type-only
// struct — frontends don't carry per-mode config blocks on the Go
// side; the type discriminator is the only thing the build pipeline
// needs to make the skip/build decision.
type FrontendEntity struct {
	Name      string                `json:"name"`
	Type      string                `json:"type,omitempty"` // "nextjs" | "vite-spa" | "react-native"
	Path      string                `json:"path"`
	DevRunner string                `json:"dev_runner,omitempty"` // "npm" (default) | "pnpm" | "yarn"
	Port      int                   `json:"port,omitempty"`
	EnvFile   string                `json:"env_file,omitempty"`
	EnvVars   []KCLEnvVar           `json:"env_vars,omitempty"`
	Deploy    *FrontendDeployEntity `json:"deploy,omitempty"`
}

// FrontendDeployEntity carries the deploy discriminator for a frontend.
// Today the only populated variant is FirebaseHosting (Type=="firebase");
// the Firebase field is non-nil exactly when Type=="firebase". The Type
// discriminator still drives the build skip-list; the embedded variant
// blocks carry the per-target config the deploy dispatch needs. Adding
// new dispatch keys (e.g. a Vercel variant) later is a pure additive
// change — a new pointer field + a new Type string.
type FrontendDeployEntity struct {
	Type string `json:"type"` // "firebase" (host/cluster/external/compose reserved for future frontend targets)

	// Firebase is populated when Type=="firebase". The Firebase Hosting
	// deploy spec — build output dir, target site/project, base-path
	// mount, and any extra static dirs to assemble into the same site.
	Firebase *FirebaseHostingDeploy `json:"-"`
}

// FirebaseHostingDeploy mirrors the kcl/schema.k FirebaseHosting schema.
// The forge-side FirebaseProvider builds the frontend, assembles
// public_dir + Bundle dirs into a staging tree honoring BasePath, writes
// a firebase.json + .firebaserc, and runs `firebase deploy`.
type FirebaseHostingDeploy struct {
	Project   string              `json:"project"`
	Site      string              `json:"site"`
	Target    string              `json:"target,omitempty"`
	PublicDir string              `json:"public_dir"`
	BasePath  string              `json:"base_path,omitempty"`
	Bundle    []FirebaseBundleDir `json:"bundle,omitempty"`
	Rewrites  []map[string]any    `json:"rewrites,omitempty"`
}

// FirebaseBundleDir is one extra pre-built static directory assembled
// into the hosting site alongside the frontend's own build output.
// Dest empty means the site root.
type FirebaseBundleDir struct {
	Src  string `json:"src"`
	Dest string `json:"dest,omitempty"`
}

// UnmarshalJSON dispatches the frontend deploy block by its `type`
// discriminator. An absent / null deploy leaves the zero value (Type=="").
// Today only "firebase" carries a typed body; unknown types are retained
// as the bare Type string so a forward-compatible KCL render (a deploy
// variant this binary predates) degrades to "skip build / no dispatch"
// rather than erroring the whole render.
func (d *FrontendDeployEntity) UnmarshalJSON(data []byte) error {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	d.Type = probe.Type
	if probe.Type == "firebase" {
		var fb FirebaseHostingDeploy
		if err := json.Unmarshal(data, &fb); err != nil {
			return fmt.Errorf("parse firebase frontend deploy: %w", err)
		}
		d.Firebase = &fb
	}
	return nil
}

// CronJobEntity is one cron-shaped binary from rendered KCL. Empty
// Schedule means "one-shot Job" (deploy waits for `condition=complete`);
// non-empty Schedule means "CronJob" (deploy doesn't wait).
type CronJobEntity struct {
	Name     string      `json:"name"`
	Schedule string      `json:"schedule,omitempty"` // cron expr or @hourly etc.
	Image    string      `json:"image,omitempty"`
	Command  []string    `json:"command,omitempty"`
	EnvVars  []KCLEnvVar `json:"env_vars,omitempty"`
	Platform string      `json:"platform,omitempty"`
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
}

// kclRenderRaw is the JSON shape emitted by `kcl run deploy/kcl/<env>/
// -o json`. We unmarshal into this first, then dispatch each service's
// deploy block by type to populate the typed [KCLEntities].
type kclRenderRaw struct {
	Clusters   []ClusterEntity   `json:"clusters,omitempty"`
	Services   []kclServiceRaw   `json:"services,omitempty"`
	Operators  []OperatorEntity  `json:"operators,omitempty"`
	Frontends  []FrontendEntity  `json:"frontends,omitempty"`
	CronJobs   []CronJobEntity   `json:"cronjobs,omitempty"`
	Gateways   []GatewayEntity   `json:"gateways,omitempty"`
	HTTPRoutes []HTTPRouteEntity `json:"http_routes,omitempty"`
	GRPCRoutes []GRPCRouteEntity `json:"grpc_routes,omitempty"`
	// SecretProvider rides alongside services in the entity output; nil
	// when the bundle declares no provider (KCL omits the key entirely).
	SecretProvider *SecretProviderEntity `json:"secret_provider,omitempty"`
	// Manifests is the rendered k8s object stream
	// (`manifests = forge.render_manifests(...)`). Parsed loosely so we
	// can recover the deploy namespace from object metadata when the
	// entity contract (`output`) is absent. Each entry is a raw k8s
	// object; we only read metadata.namespace off it.
	Manifests []rawManifest `json:"manifests,omitempty"`
}

// rawManifest is a minimal view of one rendered k8s object — just enough
// to read its namespace. The rest of the object is ignored.
type rawManifest struct {
	Metadata struct {
		Namespace string `json:"namespace,omitempty"`
	} `json:"metadata,omitempty"`
}

type kclServiceRaw struct {
	Name     string            `json:"name"`
	Image    string            `json:"image,omitempty"`
	ImageTag string            `json:"image_tag,omitempty"`
	Deploy   json.RawMessage   `json:"deploy"`
	Build    json.RawMessage   `json:"build"`
	EnvVars  []KCLEnvVar       `json:"env_vars,omitempty"`
	Command  []string          `json:"command,omitempty"`
	BuildCmd string            `json:"build_cmd,omitempty"`
	BuildCwd string            `json:"build_cwd,omitempty"`
	BuildEnv map[string]string `json:"build_env,omitempty"`
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
	raw, err := renderKCLRaw(ctx, projectDir, env)
	if err != nil {
		return nil, err
	}
	return parseKCLEntities(raw)
}

// renderKCLRaw is the side-effecting half — shell or fixture file —
// kept separate so parseKCLEntities is unit-testable from a literal []byte.
//
// The env name is passed to KCL as `-D env=<env>` so user main.k files
// can conditionally include manifests via the `option("env")` builtin
// (e.g. only ship in-cluster NATS to k3d, skip it for dev-host where
// docker-compose provides it).
func renderKCLRaw(ctx context.Context, projectDir, env string) ([]byte, error) {
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
	return kclrender.Run(projectDir, kclDir, []string{"env=" + env})
}

// parseKCLEntities turns the JSON bytes into the typed entity set,
// dispatching each service's polymorphic deploy block.
//
// The forge KCL module convention is to wrap the contract document as
//
//	output = forge.render(_bundle)
//
// so the rendered JSON has the shape `{ "output": {services, ...},
// "manifests": [...] }`. We unwrap "output" when present so the
// in-tree contract (raw {services, ...} at root, no wrapper) and the
// module-emitted contract (under "output") both parse.
func parseKCLEntities(data []byte) (*KCLEntities, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return &KCLEntities{}, nil
	}
	// The rendered `manifests` stream lives at the OUTER top level
	// alongside the `output` echo (a project may emit one, the other, or
	// both). We read the namespace off it from the outer bytes BEFORE we
	// unwrap `output` — under the wrapper the manifests aren't visible.
	manifestNS := manifestNamespaceFromOuter(data)

	// Peek for an "output" wrapper. If present, recurse on its bytes —
	// the inner shape is the same kclRenderRaw shape we already parse.
	// (A project may emit ONLY `manifests` — the entity contract is then
	// absent and the entity lists come back empty; the namespace we
	// already recovered above is the fallback the declared-context
	// resolution leans on.)
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(data, &wrapper); err == nil {
		if inner, ok := wrapper["output"]; ok && len(inner) > 0 {
			data = inner
		}
	}
	var raw kclRenderRaw
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse kcl json: %w", err)
	}
	out := &KCLEntities{
		Clusters:          raw.Clusters,
		Operators:         raw.Operators,
		Frontends:         raw.Frontends,
		CronJobs:          raw.CronJobs,
		Gateways:          raw.Gateways,
		HTTPRoutes:        raw.HTTPRoutes,
		GRPCRoutes:        raw.GRPCRoutes,
		SecretProvider:    raw.SecretProvider,
		ManifestNamespace: manifestNS,
	}
	for _, s := range raw.Services {
		deploy, err := dispatchServiceDeploy(s.Name, s.Deploy)
		if err != nil {
			return nil, err
		}
		build, err := dispatchServiceBuild(s.Name, s.Build)
		if err != nil {
			return nil, err
		}
		out.Services = append(out.Services, ServiceEntity{
			Name:     s.Name,
			Image:    s.Image,
			ImageTag: s.ImageTag,
			Deploy:   deploy,
			Build:    build,
			EnvVars:  s.EnvVars,
			Command:  s.Command,
			BuildCmd: s.BuildCmd,
			BuildCwd: s.BuildCwd,
			BuildEnv: s.BuildEnv,
		})
	}
	return out, nil
}

// manifestNamespaceFromOuter recovers the deploy namespace from the
// rendered `manifests` stream — the single namespace stamped on the
// namespaced objects. This is the fallback for projects whose main.k
// renders only `manifests` (no `output = forge.render(_bundle)` entity
// echo), where every cluster-shaped service's K8sCluster.namespace is
// otherwise absent from the parsed entities.
//
// It tallies the distinct, non-empty metadata.namespace values and
// returns the one that dominates: a forge render's namespaced objects
// all carry the env's namespace, while a handful of cluster-scoped
// objects (Namespace, CRD, ClusterRole/Binding) carry none and are
// ignored. If the manifests somehow span multiple namespaces (a
// non-canonical hand-rolled render) the most frequent one wins, so a
// stray cross-namespace object can't hijack the result. Returns "" when
// no namespaced object exists.
func manifestNamespaceFromOuter(outer []byte) string {
	var probe struct {
		Manifests []rawManifest `json:"manifests,omitempty"`
	}
	if err := json.Unmarshal(outer, &probe); err != nil {
		return ""
	}
	counts := map[string]int{}
	for _, m := range probe.Manifests {
		if ns := strings.TrimSpace(m.Metadata.Namespace); ns != "" {
			counts[ns]++
		}
	}
	best, bestN := "", 0
	for ns, n := range counts {
		// Deterministic tiebreak (lexical) so the result is stable across
		// runs regardless of map iteration order.
		if n > bestN || (n == bestN && ns < best) {
			best, bestN = ns, n
		}
	}
	return best
}

// dispatchServiceDeploy unmarshals the raw deploy block, reads the type
// discriminator, and populates exactly one of the three pointers in the
// returned DeployConfigEntity. Returns a useful error when the type is
// missing or unrecognised — bad KCL renders should fail loud rather
// than silently treat a service as one of the three default shapes.
func dispatchServiceDeploy(svcName string, raw json.RawMessage) (DeployConfigEntity, error) {
	if len(raw) == 0 {
		return DeployConfigEntity{}, fmt.Errorf("service %q: deploy block missing", svcName)
	}
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return DeployConfigEntity{}, fmt.Errorf("service %q: parse deploy.type: %w", svcName, err)
	}
	switch strings.ToLower(strings.TrimSpace(probe.Type)) {
	case "host":
		var h HostDeploy
		if err := json.Unmarshal(raw, &h); err != nil {
			return DeployConfigEntity{}, fmt.Errorf("service %q: parse host deploy: %w", svcName, err)
		}
		return DeployConfigEntity{Type: "host", Host: &h}, nil
	case "cluster":
		var c K8sCluster
		if err := json.Unmarshal(raw, &c); err != nil {
			return DeployConfigEntity{}, fmt.Errorf("service %q: parse cluster deploy: %w", svcName, err)
		}
		return DeployConfigEntity{Type: "cluster", Cluster: &c}, nil
	case "external":
		var e ExternalDeploy
		if err := json.Unmarshal(raw, &e); err != nil {
			return DeployConfigEntity{}, fmt.Errorf("service %q: parse external deploy: %w", svcName, err)
		}
		return DeployConfigEntity{Type: "external", External: &e}, nil
	case "compose":
		var c ComposeDeploy
		if err := json.Unmarshal(raw, &c); err != nil {
			return DeployConfigEntity{}, fmt.Errorf("service %q: parse compose deploy: %w", svcName, err)
		}
		return DeployConfigEntity{Type: "compose", Compose: &c}, nil
	case "build-only":
		var b BuildOnlyDeploy
		if err := json.Unmarshal(raw, &b); err != nil {
			return DeployConfigEntity{}, fmt.Errorf("service %q: parse build-only deploy: %w", svcName, err)
		}
		return DeployConfigEntity{Type: "build-only", BuildOnly: &b}, nil
	case "":
		return DeployConfigEntity{}, fmt.Errorf("service %q: deploy.type missing (expected host/cluster/external/compose/build-only)", svcName)
	default:
		return DeployConfigEntity{}, fmt.Errorf("service %q: unrecognised deploy.type %q (expected host/cluster/external/compose/build-only)", svcName, probe.Type)
	}
}

// dispatchServiceBuild unmarshals the raw build block, reads the type
// discriminator, and populates exactly one of the three pointers in the
// returned BuildConfigEntity — the build-side mirror of
// [dispatchServiceDeploy]. An absent / null build block (a hand-authored
// forge.Service that omits `build`) yields the zero value (Type=="");
// callers synthesize the GoBuild default. Unrecognised non-empty types
// fail loud — a bad KCL render should not silently fall back.
func dispatchServiceBuild(svcName string, raw json.RawMessage) (BuildConfigEntity, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return BuildConfigEntity{}, nil
	}
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return BuildConfigEntity{}, fmt.Errorf("service %q: parse build.type: %w", svcName, err)
	}
	switch strings.ToLower(strings.TrimSpace(probe.Type)) {
	case "go":
		var g GoBuild
		if err := json.Unmarshal(raw, &g); err != nil {
			return BuildConfigEntity{}, fmt.Errorf("service %q: parse go build: %w", svcName, err)
		}
		return BuildConfigEntity{Type: "go", Go: &g}, nil
	case "docker":
		var d DockerBuild
		if err := json.Unmarshal(raw, &d); err != nil {
			return BuildConfigEntity{}, fmt.Errorf("service %q: parse docker build: %w", svcName, err)
		}
		return BuildConfigEntity{Type: "docker", Docker: &d}, nil
	case "shell":
		var sh ShellBuild
		if err := json.Unmarshal(raw, &sh); err != nil {
			return BuildConfigEntity{}, fmt.Errorf("service %q: parse shell build: %w", svcName, err)
		}
		return BuildConfigEntity{Type: "shell", Shell: &sh}, nil
	case "":
		return BuildConfigEntity{}, fmt.Errorf("service %q: build.type missing (expected go/docker/shell)", svcName)
	default:
		return BuildConfigEntity{}, fmt.Errorf("service %q: unrecognised build.type %q (expected go/docker/shell)", svcName, probe.Type)
	}
}

// EffectiveBuild returns the build declaration build.go should execute
// for this service, resolving the absent-block case to the synthesized
// GoBuild default ("./cmd/<name>"). This is the ONE place the default
// lives — so a hand-authored forge.Service that omits `build`, a project
// on an older KCL render, and the deploy-as-data bridge all converge on
// the same answer without build.go re-deriving it.
//
// An EXPLICIT `build` block always wins. When the block is absent the
// default is deploy-type-aware: only forge-built deploy targets (host,
// cluster, build-only) synthesize the ./cmd/<name> GoBuild. A `compose`
// service has NO Go artifact (it's a docker-compose unit), and an
// `external` service / one carrying a top-level `build_cmd` owns its own
// build via the shell dispatcher — synthesizing a GoBuild for either
// would make forge `go build ./cmd/<name>` a package that doesn't exist
// (e.g. a sibling-repo binary or a compose aggregator). Those return the
// zero BuildConfigEntity (Type=="") so goBuildTargetsFromKCL skips them.
func (s ServiceEntity) EffectiveBuild() BuildConfigEntity {
	if s.Build.Type != "" {
		return s.Build
	}
	switch s.Deploy.Type {
	case "compose", "external":
		return BuildConfigEntity{}
	}
	if s.BuildCmd != "" {
		return BuildConfigEntity{}
	}
	return BuildConfigEntity{
		Type: "go",
		Go: &GoBuild{
			Cmd:        "./cmd/" + s.Name,
			OutputName: s.Name,
		},
	}
}

// goRunCmdForService returns the `go run` target package for a host-mode
// service — its effective GoBuild.cmd (the same package `forge build`
// compiles), so the host-run target tracks the build target exactly
// instead of a hardcoded ./cmd. Falls back to ./cmd/<name> for a service
// whose effective build isn't a GoBuild (docker/shell), which has no
// meaningful go-run target but still needs a sane string.
func goRunCmdForService(s ServiceEntity) string {
	if b := s.EffectiveBuild(); b.Type == "go" && b.Go != nil && b.Go.Cmd != "" {
		return b.Go.Cmd
	}
	return "./cmd/" + s.Name
}

// EffectiveBuildCmd returns the shell command the external-build
// dispatcher should run for this service, resolving the two sources of
// truth in precedence order:
//
//  1. The top-level Service.build_cmd (the generic build-side escape
//     hatch — works for any deploy type).
//  2. The External target's build_cmd (the build-side mirror of
//     deploy_cmd, declared next to it on a forge.External deploy block).
//
// The top-level field wins when both are set so an explicit
// Service.build_cmd override stays authoritative. Returns "" when
// neither is declared — the dispatcher's "no build_cmd" signal.
func (s ServiceEntity) EffectiveBuildCmd() string {
	if s.BuildCmd != "" {
		return s.BuildCmd
	}
	if s.Deploy.External != nil {
		return s.Deploy.External.BuildCmd
	}
	return ""
}

// EffectiveBuildEnv returns the env-var map merged into the external
// build command's environment + substitution map. Mirrors
// EffectiveBuildCmd's precedence: the top-level Service.build_env wins;
// otherwise an External target's `env` map (the same map deploy_cmd
// sees) is used so build_cmd and deploy_cmd share one config surface.
func (s ServiceEntity) EffectiveBuildEnv() map[string]string {
	if s.BuildCmd != "" {
		return s.BuildEnv
	}
	if s.Deploy.External != nil {
		return s.Deploy.External.Env
	}
	return s.BuildEnv
}

// FindService returns the named service from the entity set, or nil.
// Convenience for callers that need to look up a service before
// dispatching on Deploy.Type.
func (e *KCLEntities) FindService(name string) *ServiceEntity {
	for i := range e.Services {
		if e.Services[i].Name == name {
			return &e.Services[i]
		}
	}
	return nil
}

// HostServiceNames returns the names of every service with
// Deploy.Type == "host". The build skip-list and the up orchestrator's
// host phase both consume this.
func (e *KCLEntities) HostServiceNames() []string {
	var out []string
	for _, s := range e.Services {
		if s.Deploy.Type == "host" {
			out = append(out, s.Name)
		}
	}
	return out
}

// ClusterServiceNames returns the names of every service with
// Deploy.Type == "cluster". Used by deploy / up to choose which services
// participate in `kubectl apply` and rollout-wait.
func (e *KCLEntities) ClusterServiceNames() []string {
	var out []string
	for _, s := range e.Services {
		if s.Deploy.Type == "cluster" {
			out = append(out, s.Name)
		}
	}
	return out
}

// BuildOnlyServiceNames returns the names of every service with
// Deploy.Type == "build-only". Build emits binaries (per variant) for
// these; deploy skips them entirely.
func (e *KCLEntities) BuildOnlyServiceNames() []string {
	var out []string
	for _, s := range e.Services {
		if s.Deploy.Type == "build-only" {
			out = append(out, s.Name)
		}
	}
	return out
}
