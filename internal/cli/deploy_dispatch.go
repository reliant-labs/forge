package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/reliant-labs/forge/internal/cluster"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/deploytarget"
)

// buildDeployGroups walks the rendered entities and produces the
// deploy groups the dispatcher will dispatch in turn.
//
// Two paths feed the same return value:
//
//  1. Services that carry K8sCluster fields (Cluster/Namespace/Registry
//     non-empty on the rendered K8sDeploy union view) group natively
//     by the (Cluster, Namespace, Registry) tuple.
//
//  2. Services using the legacy K8sDeploy schema (env-wide fields
//     empty) are collected into a SINGLE synthetic group keyed by the
//     forge.yaml-derived (cluster, namespace, registry) tuple. This
//     preserves the pre-v2 single-apply behaviour for projects that
//     haven't migrated.
//
// vm-docker and compose services flow through GroupServices unchanged.
// host / build-only / no-deploy services are skipped.
func buildDeployGroups(envName string, cfg *config.ProjectConfig, entities *KCLEntities, fallbackNamespace, fallbackRegistry string) ([]deploytarget.ServiceGroup, error) {
	if entities == nil {
		return nil, nil
	}
	var raw []deploytarget.RawService
	for _, svc := range entities.Services {
		switch svc.Deploy.Type {
		case "cluster":
			c := svc.Deploy.Cluster
			if c == nil {
				continue
			}
			// Legacy K8sDeploy → empty env-wide fields. Synthesize
			// the group keys from forge.yaml + flag defaults so
			// pre-v2 projects keep working.
			clusterName := c.Cluster
			namespace := c.Namespace
			registry := c.Registry
			domain := c.Domain
			if namespace == "" {
				namespace = fallbackNamespace
			}
			if registry == "" {
				registry = fallbackRegistry
			}
			if clusterName == "" {
				// Use the expected kubectl context for grouping —
				// it's the natural per-cluster key.
				clusterName = expectedClusterForEnv(cfg, envName)
			}
			raw = append(raw, deploytarget.RawService{
				Name: svc.Name,
				K8sCluster: &deploytarget.RawK8sCluster{
					Cluster:   clusterName,
					Namespace: namespace,
					Registry:  registry,
					Domain:    domain,
					Spec: &deploytarget.K8sClusterSpec{
						Replicas: c.Replicas,
						Platform: c.Platform,
						Ports:    c.Ports,
						Ingress:  ingressSpecFromK8s(c.Ingress),
					},
				},
			})
		case "vm-docker":
			v := svc.Deploy.VMDocker
			if v == nil {
				continue
			}
			raw = append(raw, deploytarget.RawService{
				Name: svc.Name,
				VMDocker: &deploytarget.VMDockerSpec{
					SSHHost:     v.SSHHost,
					Image:       v.Image,
					Tag:         v.Tag,
					DeployCmd:   v.DeployCmd,
					RollbackCmd: v.RollbackCmd,
					HealthCmd:   v.HealthCmd,
					EnvFile:     v.EnvFile,
				},
			})
		case "compose":
			cm := svc.Deploy.Compose
			if cm == nil {
				continue
			}
			raw = append(raw, deploytarget.RawService{
				Name: svc.Name,
				Compose: &deploytarget.ComposeSpec{
					ComposeFile: cm.ComposeFile,
					Service:     cm.Service,
					EnvFile:     cm.EnvFile,
				},
			})
		default:
			// host, build-only, "" — skipped.
		}
	}
	return deploytarget.GroupServices(envName, raw)
}

func ingressSpecFromK8s(in *K8sIngressSpec) *deploytarget.IngressSpec {
	if in == nil {
		return nil
	}
	return &deploytarget.IngressSpec{Host: in.Host, Path: in.Path}
}

// dispatchDeployGroups runs every group through its provider. Per-
// group failures abort the loop (deploy.go's pre-v2 behavior was
// fail-fast on the single apply) but each failure is wrapped to
// include the provider id + group target so users can tell at a
// glance which group failed.
//
// Rollback: when a group's Deploy fails AND lastGoodTag is non-empty,
// the function asks the provider to roll back to lastGoodTag. Rollback
// errors are logged but the original Deploy error is still returned —
// rollback is a recovery affordance, not a way to mask the underlying
// failure.
func dispatchDeployGroups(ctx context.Context, registry *deploytarget.Registry, groups []deploytarget.ServiceGroup, lastGoodTag string) error {
	if registry == nil {
		return errors.New("deploy dispatch: nil provider registry")
	}
	for _, group := range groups {
		p := registry.Lookup(group.ProviderID)
		if p == nil {
			return fmt.Errorf("deploy dispatch: no provider for %q (group: %s)", group.ProviderID, deploytarget.FormatGroupSummary(group))
		}
		fmt.Printf("\n%s\n", deploytarget.FormatGroupSummary(group))
		if err := p.Deploy(ctx, group); err != nil {
			if lastGoodTag != "" {
				if rerr := p.Rollback(ctx, group, lastGoodTag); rerr != nil {
					fmt.Printf("  Note: rollback also failed: %v\n", rerr)
				}
			}
			return fmt.Errorf("deploy %s: %w", group.ProviderID, err)
		}
	}
	return nil
}

// applyOptsBuilderFromContext returns an ApplyOptsBuilder closure
// that captures the deploy-wide opts (mainK, image tag, env config,
// dry-run, prune, host-skip, one-shot jobs) and emits a per-group
// cluster.ApplyOpts. For K8sCluster groups the group's Namespace
// overrides the closure's namespace — that's the new path where the
// per-service deploy block dictates the namespace rather than
// forge.yaml.
func applyOptsBuilderFromContext(mainK, imageTag, fallbackNamespace string, envCfgKV map[string]string, dryRun, prune bool, hostSkip map[string]struct{}, oneShotJobs []string) func(deploytarget.ServiceGroup) cluster.ApplyOpts {
	return func(group deploytarget.ServiceGroup) cluster.ApplyOpts {
		ns := group.Namespace
		if ns == "" {
			ns = fallbackNamespace
		}
		return cluster.ApplyOpts{
			MainK:        mainK,
			ImageTag:     imageTag,
			Namespace:    ns,
			EnvConfigKV:  envCfgKV,
			DryRun:       dryRun,
			DryRunFramed: true,
			Prune:        prune,
			HostSkip:     hostSkip,
			OneShotJobs:  oneShotJobs,
		}
	}
}
