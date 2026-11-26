"""
Positive test — `provenance_sbom = off` with `vuln_gate` unset renders, and the
unset gate resolves to AUDIT: with no SBOM there is nothing to scan, so the
enforce default would make every release unpassable (which the
vuln_gate-enforce-over-provenance-off check refuses for a DECLARED enforce).
An env that turned SBOMs off before enforce became the default keeps rendering
exactly as it did. A declared mode still wins.
"""

import forge
import forge.workloads as fw

_k3d = forge.ClusterTarget {cluster = "k3d-demo", namespace = "demo-dev"}

output = forge.render(forge.Bundle {
    project = "demo"
    delivery_policy = forge.DeliveryPolicy {provenance_sbom = forge.Control {mode = "off"}}
    workloads = [fw.Workload {name = "api", image = "ghcr.io/acme/api", runtime = forge.OnCluster {target = _k3d}}]
})

_p = output.delivery_policy
assert_provenance_sbom_off = _p.provenance_sbom.mode == "off"
assert_vuln_gate_unset_audits = _p.vuln_gate.mode == "audit"
# the rest of the default gate is untouched
assert_vuln_default_policy = _p.vuln_gate.block_kev and _p.vuln_gate.block_severity == "critical" and _p.vuln_gate.warn_severity == "high"

_declared = forge.render(forge.Bundle {
    project = "demo"
    delivery_policy = forge.DeliveryPolicy {
        provenance_sbom = forge.Control {mode = "off"}
        vuln_gate = forge.VulnPolicy {mode = "off"}
    }
    workloads = [fw.Workload {name = "api", image = "ghcr.io/acme/api", runtime = forge.OnCluster {target = _k3d}}]
})
assert_declared_mode_wins = _declared.delivery_policy.vuln_gate.mode == "off"
