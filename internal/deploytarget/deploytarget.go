// Package deploytarget owns the per-service deploy dispatch — the
// surface that maps a rendered KCL Service.deploy block to a concrete
// pipeline that ships the service somewhere.
//
// Architectural shift from the pre-v2 shape: deploy config used to
// live in two places — `forge.yaml -> environments[]` for the env-wide
// knobs (cluster, namespace, registry, domain) and KCL's `K8sDeploy`
// for the per-service knobs (replicas, ingress, ports). v2 collapses
// both onto KCL by introducing per-service deploy-target schemas that
// also carry the env-wide info (`K8sCluster`, `VMDocker`, `Compose`).
// KCL refs DRY the common case across many services:
//
//	_prod_k8s = forge.K8sCluster {
//	    cluster = "prod-cluster"; namespace = "kalshi-prod"
//	    registry = "ghcr.io/reliant/kalshi"
//	}
//	forge.Service { name = "trader"; deploy = _prod_k8s }
//	forge.Service { name = "admin";  deploy = _prod_k8s | { replicas = 5 } }
//
// This package walks the rendered services, groups by deploy target
// (so services that share a cluster/host/compose-file flow through one
// pipeline invocation), and dispatches to the right Provider.
//
// Providers in this release:
//
//   - K8sClusterProvider — full Go implementation. Wraps
//     internal/cluster.Apply (the existing render-KCL → kubectl-apply
//     → wait-rollouts pipeline). Group-level cluster/namespace come
//     from the first service's K8sCluster.{Cluster,Namespace}.
//   - VMDockerProvider   — stub. Returns "not yet implemented" so the
//     declared shape is visible but the SSH dispatch is deferred.
//   - ComposeProvider    — stub. Same deal for docker-compose.
//
// HostDeploy and BuildOnly aren't providers — `forge run` / `forge up`
// own the host story, and BuildOnly is consumed by `forge build`.
// The dispatcher skips both rather than routing them through a Provider.
package deploytarget

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Provider is the dispatch surface for one deploy target type. Each
// concrete provider owns the pipeline for its target (k8s, vm-docker,
// compose, etc.); the dispatcher in forge deploy hands it a
// ServiceGroup and the provider does the rest.
//
// Rollback is invoked on Deploy failure with the last-known-good tag
// the dispatcher has tracked. Rollback errors are logged but the
// group's overall outcome remains "failed" — rollback is a recovery
// affordance, not a way to mask the underlying problem.
type Provider interface {
	// Name returns the provider's stable identifier — used in log
	// output and error messages so users can tell which provider
	// produced a given line.
	Name() string

	// Deploy ships every service in the group. The provider owns the
	// in-pipeline ordering (e.g. K8sClusterProvider does one
	// kubectl-apply for all services at once because they share a
	// cluster/namespace).
	Deploy(ctx context.Context, group ServiceGroup) error

	// Rollback reverts every service in the group to lastGoodTag.
	// Best-effort: per-service failures are logged and accumulated
	// into the returned error rather than aborting the loop.
	Rollback(ctx context.Context, group ServiceGroup, lastGoodTag string) error
}

// ServiceGroup is a set of services that share a deploy target —
// same provider type, same cluster/host/compose-file. The dispatcher
// groups by the (Provider, target-identifier) tuple so each group can
// flow through one provider invocation.
//
// The common env-wide fields (Cluster/Namespace/Registry/Domain) live
// on the group because K8sCluster refs make them identical across
// every service in the group. The provider reads them off the group
// rather than re-deriving them from each ResolvedService.
type ServiceGroup struct {
	// Env is the environment name (matches the deploy/kcl/<env>/
	// directory name and the `forge deploy <env>` CLI arg).
	Env string

	// ProviderID identifies the provider type — "k8s-cluster",
	// "vm-docker", "compose". Used by the dispatcher to look the
	// provider up and by log output to tag lines per-group.
	ProviderID string

	// Services is the per-service list. Each entry's Deploy field is
	// the dispatched view of the rendered KCL — see ResolvedService.
	Services []ResolvedService

	// ImageTag is the tag forge built (or is about to build) for
	// these services. Passed through to the provider so it can stamp
	// the image references correctly.
	ImageTag string

	// Common K8sCluster fields. Pulled from the first service in the
	// group (KCL refs guarantee they're identical across the group).
	// Empty for non-cluster providers.
	Cluster   string
	Namespace string
	Registry  string
	Domain    string
}

// ResolvedService is one service in a group, with its deploy block
// already dispatched by type. Exactly one of K8sCluster/VMDocker/
// Compose is non-nil; the dispatcher discards services with
// HostDeploy/BuildOnly (those aren't in any deploy-target group).
type ResolvedService struct {
	Name string

	// Exactly one of the following is non-nil. Discriminated by the
	// owning ServiceGroup.ProviderID, but kept as separate pointers
	// so each provider's Deploy method can type-assert against its
	// own concrete shape without a runtime switch.
	K8sCluster *K8sClusterSpec
	VMDocker   *VMDockerSpec
	Compose    *ComposeSpec
}

// K8sClusterSpec is the per-service portion of a K8sCluster deploy
// target. Env-wide fields (cluster/namespace/registry/domain) live on
// the ServiceGroup, not here.
type K8sClusterSpec struct {
	Replicas int
	Platform string
	Ports    []int
	// Ingress holds the ingress shape if declared; nil otherwise.
	Ingress *IngressSpec
}

// IngressSpec is the per-service ingress declaration projected from KCL.
type IngressSpec struct {
	Host string
	Path string
}

// VMDockerSpec is the per-service Docker-on-VM deploy spec. Mirrors
// the kcl/schema.k VMDocker schema.
type VMDockerSpec struct {
	SSHHost     string
	Image       string
	Tag         string
	DeployCmd   string
	RollbackCmd string
	HealthCmd   string
	EnvFile     string
}

// ComposeSpec is the per-service docker-compose deploy spec. Mirrors
// the kcl/schema.k Compose schema.
type ComposeSpec struct {
	ComposeFile string
	Service     string
	EnvFile     string
}

// ErrProviderNotImplemented is returned by stub providers (VMDocker,
// Compose) — the schema exists so projects can declare the shape they
// want; the runtime dispatch lands in a future release.
var ErrProviderNotImplemented = errors.New("forge: deploy provider not yet implemented in this release")

// Registry holds the set of Providers registered with the dispatcher.
// In forge today there's one canonical registry built by NewRegistry;
// tests can construct their own to swap in fakes.
type Registry struct {
	providers map[string]Provider
}

// NewRegistry returns a Registry pre-populated with the canonical
// forge providers (k8s-cluster + vm-docker stub + compose stub).
// Callers that need to inject test doubles should construct an empty
// Registry and Register the doubles directly.
func NewRegistry() *Registry {
	r := &Registry{providers: map[string]Provider{}}
	r.Register(K8sClusterProvider{})
	r.Register(VMDockerProvider{})
	r.Register(ComposeProvider{})
	return r
}

// Register adds (or replaces) a provider under its declared Name().
func (r *Registry) Register(p Provider) {
	r.providers[p.Name()] = p
}

// Lookup returns the provider for an id, or nil if none registered.
// Callers should treat a nil return as "no provider for this target
// type" and emit a friendly error pointing at the migration skill.
func (r *Registry) Lookup(id string) Provider {
	return r.providers[id]
}

// GroupServices walks a rendered service list and returns the deploy
// groups it should be split into. Services with deploy types `host`
// and `build-only` are NOT included — those are owned by `forge run`
// and `forge build`.
//
// Cluster grouping rule: services sharing a (Cluster, Namespace,
// Registry) tuple end up in one group. This handles the typical
// pattern (single K8sCluster ref attached to many services) AND
// per-service overrides via `_prod_k8s | { replicas = 5 }` (which
// preserves cluster/namespace/registry so the override service joins
// the same group). When a service has a legacy K8sDeploy (no env-wide
// fields), the group's Cluster/Namespace/Registry are taken from
// forge.yaml at the dispatch layer.
//
// VMDocker grouping rule: services sharing an SSHHost end up in one
// group. Compose grouping rule: services sharing a ComposeFile end up
// in one group.
//
// The returned groups are sorted by ProviderID then by the
// target-identifier so test output is deterministic.
func GroupServices(env string, services []RawService) ([]ServiceGroup, error) {
	// Key per group: providerID + target-identifier.
	groups := map[string]*ServiceGroup{}
	keyOrder := []string{}

	for _, s := range services {
		switch {
		case s.K8sCluster != nil:
			key := fmt.Sprintf("k8s-cluster|%s|%s|%s",
				s.K8sCluster.Cluster, s.K8sCluster.Namespace, s.K8sCluster.Registry)
			grp, ok := groups[key]
			if !ok {
				grp = &ServiceGroup{
					Env:        env,
					ProviderID: "k8s-cluster",
					Cluster:    s.K8sCluster.Cluster,
					Namespace:  s.K8sCluster.Namespace,
					Registry:   s.K8sCluster.Registry,
					Domain:     s.K8sCluster.Domain,
				}
				groups[key] = grp
				keyOrder = append(keyOrder, key)
			}
			grp.Services = append(grp.Services, ResolvedService{
				Name:       s.Name,
				K8sCluster: s.K8sCluster.Spec,
			})

		case s.VMDocker != nil:
			key := fmt.Sprintf("vm-docker|%s", s.VMDocker.SSHHost)
			grp, ok := groups[key]
			if !ok {
				grp = &ServiceGroup{
					Env:        env,
					ProviderID: "vm-docker",
				}
				groups[key] = grp
				keyOrder = append(keyOrder, key)
			}
			grp.Services = append(grp.Services, ResolvedService{
				Name:     s.Name,
				VMDocker: s.VMDocker,
			})

		case s.Compose != nil:
			key := fmt.Sprintf("compose|%s", s.Compose.ComposeFile)
			grp, ok := groups[key]
			if !ok {
				grp = &ServiceGroup{
					Env:        env,
					ProviderID: "compose",
				}
				groups[key] = grp
				keyOrder = append(keyOrder, key)
			}
			grp.Services = append(grp.Services, ResolvedService{
				Name:    s.Name,
				Compose: s.Compose,
			})

		default:
			// Host / BuildOnly / nil — skipped by the deploy dispatch.
		}
	}

	out := make([]ServiceGroup, 0, len(keyOrder))
	// Deterministic order: sort by key for stable output.
	sort.Strings(keyOrder)
	for _, k := range keyOrder {
		out = append(out, *groups[k])
	}
	return out, nil
}

// RawService is the input shape for GroupServices — one entry per
// rendered Service, with the deploy union already dispatched to the
// matching variant. Exactly one of K8sCluster / VMDocker / Compose
// is non-nil for services the dispatcher should ship; all three nil
// means "skip" (host / build-only / no deploy declared).
type RawService struct {
	Name string

	// K8sCluster carries both the env-wide fields (used for grouping)
	// and the per-service spec (carried through to the provider).
	K8sCluster *RawK8sCluster

	VMDocker *VMDockerSpec
	Compose  *ComposeSpec
}

// RawK8sCluster combines the env-wide K8sCluster fields (which key
// the group) with the per-service spec (which the provider consumes).
// Kept separate from the group-level fields so GroupServices can read
// them without unpacking K8sClusterSpec twice.
type RawK8sCluster struct {
	Cluster   string
	Namespace string
	Registry  string
	Domain    string
	Spec      *K8sClusterSpec
}

// FormatGroupSummary returns a one-line description of a group for
// CLI output. Shape:
//
//	[<provider>] <target>: <svc-1>, <svc-2>, ...
func FormatGroupSummary(g ServiceGroup) string {
	names := make([]string, 0, len(g.Services))
	for _, s := range g.Services {
		names = append(names, s.Name)
	}
	target := groupTarget(g)
	return fmt.Sprintf("[%s] %s: %s", g.ProviderID, target, strings.Join(names, ", "))
}

func groupTarget(g ServiceGroup) string {
	switch g.ProviderID {
	case "k8s-cluster":
		return fmt.Sprintf("cluster=%s ns=%s", g.Cluster, g.Namespace)
	case "vm-docker":
		if len(g.Services) > 0 && g.Services[0].VMDocker != nil {
			return "ssh=" + g.Services[0].VMDocker.SSHHost
		}
		return "ssh=?"
	case "compose":
		if len(g.Services) > 0 && g.Services[0].Compose != nil {
			return "file=" + g.Services[0].Compose.ComposeFile
		}
		return "file=?"
	default:
		return ""
	}
}
