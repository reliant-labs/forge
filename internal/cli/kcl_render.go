package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// KCLEntities is the typed, dispatched view of the JSON the sibling
// KCL deploy module emits. Agent A's typed schema module exports
// polymorphic `deploy: HostDeploy | K8sDeploy | BuildOnly` per service;
// the JSON discriminator is `deploy.type ∈ {"host","cluster","build-only"}`
// (services only — operators/cronjobs are always cluster-shaped).
//
// Callers (`forge build --env`, `forge deploy <env>`, `forge up --env`,
// `forge run <svc>`) read this rather than reaching back into forge.yaml
// because deployment placement is a per-env decision that lives in the
// KCL layer, not on services[] in the project config.
type KCLEntities struct {
	Services  []ServiceEntity  `json:"services,omitempty"`
	Operators []OperatorEntity `json:"operators,omitempty"`
	Frontends []FrontendEntity `json:"frontends,omitempty"`
	CronJobs  []CronJobEntity  `json:"cronjobs,omitempty"`
}

// ServiceEntity is one service from rendered KCL. The Deploy field is
// polymorphic — exactly one of Host / Cluster / BuildOnly is populated
// according to Deploy.Type. See [DeployConfigEntity] for the discriminator.
type ServiceEntity struct {
	Name    string             `json:"name"`
	Image   string             `json:"image,omitempty"`
	Deploy  DeployConfigEntity `json:"deploy"`
	EnvVars []KCLEnvVar        `json:"env_vars,omitempty"`
	Command []string           `json:"command,omitempty"`
}

// DeployConfigEntity is the dispatched-by-type view of a service's
// deploy block. The raw JSON shape is a tagged union — Type carries
// the tag; exactly one of Host/Cluster/BuildOnly is non-nil after
// [unmarshalDeploy] runs.
type DeployConfigEntity struct {
	Type      string           // "host" | "cluster" | "build-only"
	Host      *HostDeploy      // populated when Type=="host"
	Cluster   *K8sDeploy       // populated when Type=="cluster"
	BuildOnly *BuildOnlyDeploy // populated when Type=="build-only"
}

// HostDeploy is the deploy block for a service that runs as a host
// process during `forge up --env=<env>`. The Runner field selects the
// dispatch (go-run / air / binary / delve) and is consumed by
// [runHostServiceWithRunner] + the up orchestrator.
type HostDeploy struct {
	Runner    string `json:"runner,omitempty"`     // "go-run" | "air" | "binary" | "delve"
	AirConfig string `json:"air_config,omitempty"` // path relative to project root, default .air.toml
	EnvFile   string `json:"env_file,omitempty"`   // path relative to project root, default .env.<env>
	DelvePort int    `json:"delve_port,omitempty"` // when Runner=="delve"; default 2345
}

// K8sDeploy is the deploy block for a cluster-mode service. Replicas
// / Ingress / Ports / Platform mirror the KCL schema's K8sDeploy fields.
type K8sDeploy struct {
	Replicas int             `json:"replicas,omitempty"`
	Ingress  *K8sIngressSpec `json:"ingress,omitempty"`
	Platform string          `json:"platform,omitempty"` // GOARCH override; empty = use forge.yaml deploy.target_arch
	Ports    []int           `json:"ports,omitempty"`
}

// K8sIngressSpec is the rendered ingress for a cluster service.
type K8sIngressSpec struct {
	Host string `json:"host,omitempty"`
	Path string `json:"path,omitempty"`
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
	Name        string            `json:"name"`
	Ldflags     []string          `json:"ldflags,omitempty"`
	BuildTags   []string          `json:"build_tags,omitempty"`
	EnvAtBuild  map[string]string `json:"env_at_build,omitempty"`
	OutputName  string            `json:"output_name,omitempty"` // default: <service>-<variant>
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
type FrontendEntity struct {
	Name      string `json:"name"`
	Type      string `json:"type,omitempty"`       // "nextjs" | "vite-spa" | "react-native"
	Path      string `json:"path"`
	DevRunner string `json:"dev_runner,omitempty"` // "npm" (default) | "pnpm" | "yarn"
	Port      int    `json:"port,omitempty"`
	EnvFile   string `json:"env_file,omitempty"`
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
type KCLEnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
}

// kclRenderRaw is the JSON shape emitted by `kcl run deploy/kcl/<env>/
// -o json`. We unmarshal into this first, then dispatch each service's
// deploy block by type to populate the typed [KCLEntities].
type kclRenderRaw struct {
	Services  []kclServiceRaw  `json:"services,omitempty"`
	Operators []OperatorEntity `json:"operators,omitempty"`
	Frontends []FrontendEntity `json:"frontends,omitempty"`
	CronJobs  []CronJobEntity  `json:"cronjobs,omitempty"`
}

type kclServiceRaw struct {
	Name    string          `json:"name"`
	Image   string          `json:"image,omitempty"`
	Deploy  json.RawMessage `json:"deploy"`
	EnvVars []KCLEnvVar     `json:"env_vars,omitempty"`
	Command []string        `json:"command,omitempty"`
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
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "kcl", "run", kclDir, "-o", "json")
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("kcl run %s: %w", kclDir, err)
	}
	return out.Bytes(), nil
}

// parseKCLEntities turns the JSON bytes into the typed entity set,
// dispatching each service's polymorphic deploy block.
func parseKCLEntities(data []byte) (*KCLEntities, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return &KCLEntities{}, nil
	}
	var raw kclRenderRaw
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse kcl json: %w", err)
	}
	out := &KCLEntities{
		Operators: raw.Operators,
		Frontends: raw.Frontends,
		CronJobs:  raw.CronJobs,
	}
	for _, s := range raw.Services {
		deploy, err := dispatchServiceDeploy(s.Name, s.Deploy)
		if err != nil {
			return nil, err
		}
		out.Services = append(out.Services, ServiceEntity{
			Name:    s.Name,
			Image:   s.Image,
			Deploy:  deploy,
			EnvVars: s.EnvVars,
			Command: s.Command,
		})
	}
	return out, nil
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
		var c K8sDeploy
		if err := json.Unmarshal(raw, &c); err != nil {
			return DeployConfigEntity{}, fmt.Errorf("service %q: parse cluster deploy: %w", svcName, err)
		}
		return DeployConfigEntity{Type: "cluster", Cluster: &c}, nil
	case "build-only":
		var b BuildOnlyDeploy
		if err := json.Unmarshal(raw, &b); err != nil {
			return DeployConfigEntity{}, fmt.Errorf("service %q: parse build-only deploy: %w", svcName, err)
		}
		return DeployConfigEntity{Type: "build-only", BuildOnly: &b}, nil
	case "":
		return DeployConfigEntity{}, fmt.Errorf("service %q: deploy.type missing (expected host/cluster/build-only)", svcName)
	default:
		return DeployConfigEntity{}, fmt.Errorf("service %q: unrecognised deploy.type %q (expected host/cluster/build-only)", svcName, probe.Type)
	}
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
