"""
Negative test — Observability.agent.clusters lists one kube context twice,
or holds an empty entry. Each entry is a cluster the agent runs on, named
once.
"""
# kcl-args: case=duplicate
# kcl-args: case=empty
# expect: ObservabilityAgent.clusters

import forge

_case = option("case", default="duplicate")
output = forge.render(forge.Bundle {
    project = "demo"
    env = "prod"
    observability = forge.Observability {
        destination = forge.ObservabilityDestination {endpoint = "https://otlp.example.com"}
        agent = forge.ObservabilityAgent {clusters = ["gpu", "gpu"] if _case == "duplicate" else ["gpu", ""]}
    }
})
