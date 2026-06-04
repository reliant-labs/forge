"""
Forge KCL — NetworkPolicy builders.

Renders the standard forge NetworkPolicy bundle for a namespace:

  * `default-deny` — deny all ingress + egress.
  * `allow-dns` — egress to kube-system on UDP/TCP 53.
  * `allow-internal` — ingress + egress within the namespace.
  * `allow-ingress-controller` — ingress from the ingress-nginx ns.

Render with `forge.render(...)` when `Bundle.network_policies = True`
(the default for prod environments; dev typically skips it).
"""

render_network_policies = lambda namespace: str -> [any] {
    [
        {
            apiVersion = "networking.k8s.io/v1"
            kind = "NetworkPolicy"
            metadata = {name = "default-deny", namespace = namespace}
            spec = {
                podSelector = {}
                policyTypes = ["Ingress", "Egress"]
            }
        }
        {
            apiVersion = "networking.k8s.io/v1"
            kind = "NetworkPolicy"
            metadata = {name = "allow-dns", namespace = namespace}
            spec = {
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
            }
        }
        {
            apiVersion = "networking.k8s.io/v1"
            kind = "NetworkPolicy"
            metadata = {name = "allow-internal", namespace = namespace}
            spec = {
                podSelector = {}
                policyTypes = ["Ingress", "Egress"]
                ingress = [
                    {from = [{podSelector = {}}]}
                ]
                egress = [
                    {to = [{podSelector = {}}]}
                ]
            }
        }
        {
            apiVersion = "networking.k8s.io/v1"
            kind = "NetworkPolicy"
            metadata = {name = "allow-ingress-controller", namespace = namespace}
            spec = {
                podSelector = {}
                policyTypes = ["Ingress"]
                ingress = [
                    {
                        from = [
                            {namespaceSelector = {matchLabels = {"kubernetes.io/metadata.name" = "ingress-nginx"}}}
                        ]
                    }
                ]
            }
        }
    ]
}
