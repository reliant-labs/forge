"""
Forge KCL — component render entry points.

Sibling of schema.k / components.k so it sees Component, Server,
Worker, Cron, Operator, Binary, ComponentPort, Resources, EnvVar as
bare names (no import that would fork type identity).

Two responsibilities:

  1. `load_components(path)` — read the denormalized
     `components_gen.json` forge emits and map each entry to the right
     typed Component subtype (defaults applied). The result is a list
     of typed components a per-env main.k overlays (replicas,
     resources, registry, secrets) via the `|` merge operator.

  2. `render_components(components, env)` — normalize the typed
     components into the Kubernetes manifest list for an environment.
     Each component's kind selects its expansion (Server→Deployment+
     Service, Cron→CronJob, …). Namespace-scoped boilerplate (the
     Namespace object, network policies) is added once.

Path contract: `file.read` resolves relative to the process working
directory. forge runs `kcl run deploy/kcl/<env>` from the project root,
and the documented manual command (`kcl run deploy/kcl/dev`) is also
run from the project root — so the default path
`deploy/kcl/components_gen.json` is correct in both. Pass an explicit
path to `load_components` to override.
"""

import file
import json
import .expand as comp_lib
import ..lib.netpol as netpol_lib

# Normalize one raw JSON component dict to the canonical Component
# shape, filling the overlay defaults a typed Component schema would
# apply (replicas, leader_election, the empty image/registry/namespace
# the env overlays). Ports get their protocol/expose defaults.
#
# Returns a plain dict — NOT a typed schema literal — because this runs
# inside a lambda body and KCL v0.11 rejects schema-literal
# instantiation there (see schema.k). The lib expanders take `any` and
# read fields dynamically, so a normalized dict is sufficient; projects
# that author components by hand still get the typed Server/Worker/…
# schemas at module scope.
_load_one = lambda raw: any -> any {
    _ports = [{
        name = p.name
        port = p.port
        protocol = p.protocol if "protocol" in p else "tcp"
        expose = p.expose if "expose" in p else False
    } for p in raw.ports]
    {
        name = raw.name
        kind = raw.kind
        ports = _ports
        env = raw.env if "env" in raw else {}
        command = raw.command if "command" in raw else []
        schedule = raw.schedule if "schedule" in raw else ""
        group = raw.group if "group" in raw else ""
        version = raw.version if "version" in raw else ""
        crds = raw.crds if "crds" in raw else []
        # Overlay defaults — the per-env main.k merges its env knobs on
        # top of these via the `|` operator.
        image = ""
        registry = ""
        namespace = ""
        replicas = 1
        env_vars = []
        platform = ""
        leader_election = True
    }
}

# Load + normalize the components emitted in components_gen.json. Each
# entry is a canonical Component dict ready for `render_components` (or
# a per-env `|` overlay).
load_components = lambda path: str -> [any] {
    _doc = json.decode(file.read(path))
    [_load_one(r) for r in _doc.components]
}

# Read just the project name from components_gen.json. Useful for env
# main.k files that need the project name for image/registry defaults.
load_project = lambda path: str -> str {
    json.decode(file.read(path)).project
}

# The default location of the generated component data, relative to the
# project root (forge's kcl invocation cwd).
COMPONENTS_GEN = "deploy/kcl/components_gen.json"

schema ComponentEnv:
    """Per-environment knobs the component manifest renderer needs.
    Mirrors RenderEnv (schema.k) but keyed to the component model.

    namespace / registry are the env-wide deploy target; image_tag is
    the per-build override (forge passes `-D image_tag=<sha>`).
    """
    namespace: str
    registry: str = ""
    image_tag: str = "latest"
    network_policies: bool = True

    check:
        namespace, "ComponentEnv.namespace is required"

_render_namespace = lambda env: ComponentEnv -> any {
    {
        apiVersion = "v1"
        kind = "Namespace"
        metadata = {
            name = env.namespace
            labels = {
                "app.kubernetes.io/managed-by" = "forge"
                "pod-security.kubernetes.io/enforce" = "restricted"
                "pod-security.kubernetes.io/audit" = "restricted"
                "pod-security.kubernetes.io/warn" = "restricted"
            }
        }
    }
}

# Normalize a list of typed components into the k8s manifest list for
# an environment. The Namespace comes first; each component expands per
# its kind; network policies (when on) close it out.
render_components = lambda components: [any], env: ComponentEnv -> [any] {
    [_render_namespace(env)] \
        + [item for c in components for item in comp_lib.render_component(c, env.namespace, env.registry, env.image_tag)] \
        + (netpol_lib.render_network_policies(env.namespace) if env.network_policies else [])
}
