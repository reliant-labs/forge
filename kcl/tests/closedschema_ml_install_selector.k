"""
Closed-schema test — forge.Install has no scrape fields either.

A scrape of what forge installed is a plain Observability.telemetry entry
(forge.Telemetry, namespaces = [<chart namespace>]), as for an adopted
component.
"""

# expect: selector
# expect: Install

import forge

_i = forge.Install {
    kind = "dcgm-exporter"
    chart = forge.HelmChart {name = "dcgm-exporter", repo = "https://nvidia.github.io/dcgm-exporter/helm-charts", chart = "dcgm-exporter", version = "4.1.1", namespace = "forge-ml", delivery = "bundle"}
    selector = {"app.kubernetes.io/name": "dcgm-exporter"}
}
