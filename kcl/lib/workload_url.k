"""
forge.WorkloadURL resolution — the ONE place a workload's public URL is
decided for an environment forge renders itself.

A `forge.WorkloadURL { workload = "api" }` names a sibling workload and means
"its public URL, in this environment". Where that URL comes from depends on
who knows it:

  * HOSTED (the Bundle declares `control_plane` and deploys tiers through
    it): nobody in the render knows. The platform allocates hostnames, so the
    reference is carried into the published spec unchanged — StaticSite
    `runtimeConfig.<KEY>.workloadURL`, SimpleBackend `env[].workloadURL` —
    and the control plane resolves it, and re-resolves it when the target's
    URL moves.
  * Everything else (host / compose / cluster / a LOCAL control-plane env):
    the render knows, because every port and hostname is declared here. The
    reference is resolved NOW, to a literal, so the dev document and the
    manifests carry a URL and nothing downstream sees a reference.

Resolution order for a named workload, first match wins:

  1. An HTTPRoute (or GRPCRoute) whose `service` is the workload, through a
     declared Gateway listener: <scheme>://<host>:<port><path>. This is the
     URL a browser actually reaches in a cluster env.
  2. A self-hosted public SimpleBackend with a custom domain: https://<domain>.
  3. A HOST service with declared `listen_ports`: http://localhost:<port>.
  4. A frontend (dev-served): http://localhost:<port><base_path>.

Anything else is REFUSED with a message naming the workload and the fix.
Refusing beats guessing: a guessed URL is a frontend that calls the wrong
origin and a backend whose CORS admits nobody, both of which surface as a
"CORS error" that names neither cause. The one deliberate refusal worth
knowing: a host service with NO `listen_ports` is given an ephemeral port by
`forge env up` AFTER the render, so the render cannot know it. Declare
`listen_ports = [plugin.resolve_port(...)]` (the scaffold does).
"""

# The resolved URL of workload `name`, or "" when nothing declares one.
# Callers turn "" into a refusal naming the reference; see _require_url.
resolve = lambda name: str, bundle: any -> str {
    _route_urls = [_route_url(r, bundle) for r in (bundle.http_routes + bundle.grpc_routes) if r.service == name]
    _route_urls_ok = [u for u in _route_urls if u]
    _sb_domains = [s.deploy.spec.domains[0] for s in bundle.services \
        if s.name == name and s.deploy and s.deploy.type == "simple-backend" \
        and s.deploy.spec.network == "public" and s.deploy.spec.domains]
    _host_ports = [s.deploy.listen_ports[0] for s in bundle.services \
        if s.name == name and s.deploy and s.deploy.type == "host" and s.deploy.listen_ports] \
        + [w.host.listen_ports[0] for w in bundle.workloads \
        if w.name == name and w.host != Undefined and w.host.listen_ports]
    _frontends = [f for f in bundle.frontends if f.name == name]
    _route_urls_ok[0] if _route_urls_ok else \
        ("https://" + _sb_domains[0] if _sb_domains else \
        ("http://localhost:" + str(_host_ports[0]) if _host_ports else \
        ("http://localhost:" + str(_frontends[0].port) + _frontend_base(_frontends[0]) if _frontends else "")))
}

# Every name a reference may target in this Bundle. A reference to anything
# else is a typo or a stale name, refused at load.
workload_names = lambda bundle: any -> [str] {
    [s.name for s in bundle.services] + [w.name for w in bundle.workloads] + [f.name for f in bundle.frontends]
}

_frontend_base = lambda f: any -> str {
    f.deploy.base_path if f.deploy and f.deploy.type == "static-site" and f.deploy.base_path else ""
}

_route_url = lambda r: any, bundle: any -> str {
    _gws = [g for g in bundle.gateways if g.name == r.gateway]
    _ls = [l for g in _gws for l in g.listeners if l.name == r.listener]
    _l = _ls[0] if _ls else None
    _host = (r.host if r.host else (_l.hostname if _l.hostname else _gws[0].host)) if _l else ""
    _scheme = "https" if _l and _l.protocol == "HTTPS" else "http"
    _default_port = (_l.protocol == "HTTPS" and _l.port == 443) or (_l.protocol != "HTTPS" and _l.port == 80) if _l else False
    _path = ((_l.path_prefix + r.path).replace("//", "/")) if _l else ""
    (_scheme + "://" + (_host if _host else "localhost") + ("" if _default_port else ":" + str(_l.port)) + (_path.rstrip("/") if _path != "/" else "")) if _l else ""
}
