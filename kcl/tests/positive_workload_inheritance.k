"""
Positive test — a base workload declaration is COMPLETE, and each
environment REFINES it with `|` rather than reconstructing it.

This is the property the workload vocabulary exists to provide, and it
is the one a per-env overlay comprehension could not give:

  1. The base is valid and complete ON ITS OWN — ports and all. It is
     not a fragment awaiting assembly by an env file.
  2. An env states ONLY the difference. Everything else is inherited.
  3. `|` preserves the SCHEMA TYPE, so the `check:` rules in
     forge.workloads re-run on the merged result. That is what makes
     refinement safe: an env cannot refine a valid base into an invalid
     workload without failing at render time, in that env.
     (The rejection half is negative_workload_bad_refinement.k — a
      check that fires cannot be asserted from inside the same file.)

Together these are why a base declaration is a schema instance rather
than a plain value: a value cannot carry the checks.
"""

import forge.workloads as fw

# --- The BASE declaration, as `deploy/kcl/workloads.k` states it. -----------
# Complete: it names its own port and is a valid workload with no help
# from any environment.
api = fw.Workload {
    name = "api"
    kind = "service"
    ports = [fw.Port {name = "http", port = 8080, expose = True}]
    build = {type = "go", cmd = "./cmd/demo", output_name = "demo"}
}

# --- DEV refines nothing but placement. -------------------------------------
_dev = [api | {image = "demo", registry = "localhost:5050", namespace = "demo-dev"}]

# --- PROD refines the port AND the replica count. ---------------------------
_prod = [api | {
    image = "demo"
    registry = "ghcr.io/acme"
    namespace = "demo-prod"
    replicas = 3
    ports = [fw.Port {name = "http", port = 9090, expose = True}]
}]

_dev_env = fw.WorkloadEnv {namespace = "demo-dev", project = "demo", network_policies = False}
_prod_env = fw.WorkloadEnv {namespace = "demo-prod", project = "demo", network_policies = False}

dev_manifests = fw.render_workloads(_dev, _dev_env)
prod_manifests = fw.render_workloads(_prod, _prod_env)

_dev_deploy = [m for m in dev_manifests if m.kind == "Deployment" and m.metadata.name == "api"][0]
_prod_deploy = [m for m in prod_manifests if m.kind == "Deployment" and m.metadata.name == "api"][0]
_dev_svc = [m for m in dev_manifests if m.kind == "Service" and m.metadata.name == "api"][0]
_prod_svc = [m for m in prod_manifests if m.kind == "Service" and m.metadata.name == "api"][0]

# (1) The base is complete: dev overrode NO port, and still gets the real
# one the base declared — not a fallback the expansion invented.
assert_dev_port_inherited = _dev_svc.spec.ports[0].port == 8080
assert_dev_container_port_inherited = \
    _dev_deploy.spec.template.spec.containers[0].ports[0].containerPort == 8080

# (2) Prod's refinement took effect, on BOTH the Service and the container.
assert_prod_port_overridden = _prod_svc.spec.ports[0].port == 9090
assert_prod_container_port_overridden = \
    _prod_deploy.spec.template.spec.containers[0].ports[0].containerPort == 9090
assert_prod_replicas_overridden = _prod_deploy.spec.replicas == 3

# (3) Refining prod did NOT mutate the base or leak into dev. Each env
# renders from the same declaration independently.
assert_dev_replicas_untouched = _dev_deploy.spec.replicas == 1
assert_base_unchanged = api.ports[0].port == 8080

# (4) Everything NOT refined is inherited rather than restated: prod never
# mentions `kind`, `name` or `build`, and has them all.
assert_kind_inherited = len([m for m in prod_manifests if m.kind == "Service"]) == 1
assert_name_inherited = _prod_deploy.metadata.name == "api"

# (5) The merged result is still a Workload, which is WHY the checks
# re-run on it. A plain dict would type-check as `{str:any}` and enforce
# nothing.
assert_refinement_preserves_schema = typeof(api | {replicas = 2}) == "Workload"
