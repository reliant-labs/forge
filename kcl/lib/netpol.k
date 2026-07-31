"""
Forge KCL — NetworkPolicy builders.

Renders the standard forge NetworkPolicy bundle for a namespace:

  * `default-deny` — deny all ingress + egress.
  * `allow-dns` — egress to kube-system on UDP/TCP 53.
  * `allow-internal` — ingress + egress within the namespace.
  * `allow-egress-external` — egress to the specific ports the app
    dials outside the cluster (HTTPS, the database, …).
  * `allow-egress-telemetry` — egress to the OTLP collector's namespace.
  * `allow-ingress-controller` — ingress from the namespace the Gateway
    / ingress controller actually runs in.

Why the last three exist
========================
`default-deny` on Egress blackholes everything the allow-list misses,
and the previous bundle allowed only DNS and same-namespace traffic. A
forge app dials a managed Postgres (5432, usually a private VPC address
OUTSIDE the cluster), an OTLP collector in another namespace, and HTTPS
for JWKS / object storage / third-party APIs. All three were denied, so
every environment that turned network policies on — staging and prod,
by default — could not reach its own database.

`allow-ingress-controller` named `ingress-nginx` while the Gateway
forge renders is a Gateway API Gateway whose controller lives
elsewhere. A policy that allows traffic from a namespace nothing runs
in is a policy that denies ingress, which is why the source namespace
is now DECLARED (ComponentEnv.ingress_namespace) rather than assumed.

The external egress rule is port-scoped rather than
destination-scoped: forge cannot know the address of a managed
database, and an `ipBlock` `except` list for the cluster's own CIDRs is
cluster-specific. Restricting egress to the ports the app actually
dials is portable, meaningfully narrower than allow-all, and — most
importantly — TRUE, which "no rule at all" was not.
"""

_policy = lambda name: str, namespace: str, spec: any -> any {
    {
        apiVersion = "networking.k8s.io/v1"
        kind = "NetworkPolicy"
        metadata = {name = name, namespace = namespace}
        spec = spec
    }
}

# `egress_ports` are TCP ports the workload may dial ANYWHERE (443 for
# HTTPS, 5432 for a managed Postgres, …). `telemetry_namespace` and
# `ingress_namespace` are namespaces, not guesses: empty means "not
# declared", and the corresponding policy is OMITTED rather than
# rendered against a namespace that may not exist.
render_network_policies = lambda namespace: str, egress_ports: [int] = [443, 5432], telemetry_namespace: str = "", ingress_namespace: str = "" -> [any] {
    _base = [
        _policy("default-deny", namespace, {
            podSelector = {}
            policyTypes = ["Ingress", "Egress"]
        })
        _policy("allow-dns", namespace, {
            podSelector = {}
            policyTypes = ["Egress"]
            egress = [
                {
                    to = [
                        {namespaceSelector = {matchLabels = {"kubernetes.io/metadata.name" = "kube-system"}}}
                    ]
                    ports = [
                        {protocol = "UDP", port = 53}
                        {protocol = "TCP", port = 53}
                    ]
                }
            ]
        })
        _policy("allow-internal", namespace, {
            podSelector = {}
            policyTypes = ["Ingress", "Egress"]
            ingress = [
                {from = [{podSelector = {}}]}
            ]
            egress = [
                {to = [{podSelector = {}}]}
            ]
        })
    ]
    _external = [_policy("allow-egress-external", namespace, {
        podSelector = {}
        policyTypes = ["Egress"]
        egress = [
            {
                to = [{ipBlock = {cidr = "0.0.0.0/0"}}]
                ports = [{protocol = "TCP", port = p} for p in egress_ports]
            }
        ]
    })] if egress_ports else []
    _telemetry = [_policy("allow-egress-telemetry", namespace, {
        podSelector = {}
        policyTypes = ["Egress"]
        egress = [
            {
                to = [
                    {namespaceSelector = {matchLabels = {"kubernetes.io/metadata.name" = telemetry_namespace}}}
                ]
                ports = [
                    {protocol = "TCP", port = 4317}
                    {protocol = "TCP", port = 4318}
                ]
            }
        ]
    })] if telemetry_namespace else []
    _ingress = [_policy("allow-ingress-controller", namespace, {
        podSelector = {}
        policyTypes = ["Ingress"]
        ingress = [
            {
                from = [
                    {namespaceSelector = {matchLabels = {"kubernetes.io/metadata.name" = ingress_namespace}}}
                ]
            }
        ]
    })] if ingress_namespace else []
    _base + _external + _telemetry + _ingress
}
