package cli

import (
	"fmt"
	"strings"
)

// UncollectedTelemetryOutput is one `output.uncollected_telemetry` entry: a
// cluster telemetry target the project declared that nothing collects,
// because forge owns collection with a destination and its node agent runs
// on no cluster of the target's (no forge workload runs there, and no
// forge.NodeAgent in Observability.node_agents targets it). A target forge
// renders on its own (origin "default") is never listed.
type UncollectedTelemetryOutput struct {
	// ServiceName is the target's service.name.
	ServiceName string `json:"service_name"`
	// Cluster is the kube context it scrapes.
	Cluster string `json:"cluster"`
}

// NodeAgentOutput is one `output.node_agents` entry: where a declared
// forge.NodeAgent (Observability.node_agents) runs forge's node agent.
type NodeAgentOutput struct {
	// Cluster is the kube context of the NodeAgent's target.
	Cluster string `json:"cluster"`
	// ConnectedCluster is the target's connected_cluster: the connected
	// cluster a control-plane env reaches the cluster through. Empty when
	// the target names none.
	ConnectedCluster string `json:"connected_cluster,omitempty"`
}

// uncollectedTelemetryWarnings are the render's warnings for
// output.uncollected_telemetry: one per cluster, naming its targets and the
// declaration that collects them (a forge.NodeAgent). A warning only: v1.13.0 placed no agent on such
// a cluster either, and placing one by default would make the cluster a
// target of the env, which a local or ephemeral lifecycle, a single-cluster
// Flux applier or control-plane credential delivery can refuse.
func uncollectedTelemetryWarnings(entities *KCLEntities) []string {
	if entities == nil {
		return nil
	}
	var clusters []string
	names := map[string][]string{}
	for _, u := range entities.UncollectedTelemetry {
		if _, ok := names[u.Cluster]; !ok {
			clusters = append(clusters, u.Cluster)
		}
		names[u.Cluster] = append(names[u.Cluster], fmt.Sprintf("%q", u.ServiceName))
	}
	var out []string
	for _, c := range clusters {
		verb := "scrapes"
		if len(names[c]) > 1 {
			verb = "scrape"
		}
		out = append(out, fmt.Sprintf("warning: forge.Telemetry %s %s cluster %q, where forge runs no node agent (no forge workload runs there, and no forge.NodeAgent targets it), so it is not collected. "+
			"Declare forge.NodeAgent {target = <the forge.ClusterTarget of %q>} in Observability.node_agents to run forge's node agent there; that makes %q a cluster the env deploys to, so a local or ephemeral lifecycle needs it to be a cluster forge creates, applier = \"flux\" needs it to be the env's one cluster, and token_from_control_plane needs a connected cluster for it (the target's connected_cluster)",
			strings.Join(names[c], ", "), verb, c, c, c))
	}
	return out
}
