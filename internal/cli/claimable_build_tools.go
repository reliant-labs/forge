package cli

import "context"

// requireClaimableBuildTools checks local prerequisites before allocating an
// anonymous account. A failed local build must not consume a claim or its TTL.
// Static sites and registry-to-registry mirrors need no Docker daemon.
func requireClaimableBuildTools(ctx context.Context, entities *KCLEntities) error {
	for _, w := range entities.Workloads {
		if w.OnRuntime(RuntimeHosted) && (w.Build.Type == "go" || w.Build.Type == "docker") {
			return requireImageDaemon(ctx, "hosted workload "+w.Name)
		}
	}
	return nil
}
