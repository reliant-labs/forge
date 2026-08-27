"""
Positive test — the `forge.dev/env` ENV-OWNERSHIP stamp (lib/labels.k).

Two forge envs can render into the SAME namespace. When they do, one
deploy silently replaces the other's workloads, and nothing on the
surviving objects records whose render won — the incident this stamp
closes. Every object `render_manifests` emits therefore carries
`forge.dev/env: <env>`, so:

    kubectl get all -n <ns> -l 'forge.dev/env!=dev'

answers "what in this namespace does dev NOT own".

This fixture runs under the GENERIC positive-assertion runner, which
passes NO `-D env=` binding — so it pins the two behaviours that do not
need one:

  1. The KCL DRILL-IN. `Bundle.env` is a schema field: a project that
     wants a different ownership tag than the CLI would supply just sets
     it. There is no CLI flag and no new render option.
  2. The NO-ENV fallback. With neither a binding nor a drill-in, NO
     label is emitted at all — a plain `kcl run` renders byte-identically
     to the pre-stamp output.

The `-D env=` DEFAULT path (the automatic case: a project gets the label
without editing any KCL) is pinned by tests/positive_env_option.k, which
TestKCLModule_EnvOptionPlumbing runs under real bindings.

The other invariant here is the CRITICAL one: the stamp reaches
`metadata.labels` and the POD TEMPLATE's labels, and NEVER a selector.
`spec.selector` is immutable on a Deployment (adding a key makes
`kubectl apply` fail with "field is immutable" on every object that
already exists) and silently detaches endpoints on a Service — so the
asserts below pin both selectors to their exact one-key form.
"""

import forge

_target = forge.ClusterTarget {
    cluster = "k3d-myapp"
    namespace = "myapp-shared"
    registry = "localhost:5050"
}

# A workload (Deployment + Service + RBAC), a CronJob, a one-shot Job
# (CronJob with an empty schedule), and a RAW manifest that no forge
# label builder ever touches — the last one is why the stamp is applied
# to the FINAL manifest list rather than threaded through the builders.
_svc = forge.RenderedWorkload {
    name = "api"
    image = "myapp"
    deploy = _target.deploy | {ports = [8080]}
}

_cron = forge.CronJob {
    name = "nightly"
    image = "myapp"
    schedule = "0 3 * * *"
    command = ["/app/myapp", "nightly"]
}

_job = forge.CronJob {
    name = "migrate"
    image = "myapp"
    schedule = ""
    command = ["/app/myapp", "db", "migrate", "up"]
}

# Raw manifests: one with NO metadata.labels at all, and one that already
# declares `forge.dev/env` by hand (an author overriding the stamp on a
# single object — explicit intent must win, exactly as it does for the
# app-label owner stamp in lib/services.k).
_raw = [
    {
        apiVersion = "cert-manager.io/v1"
        kind = "ClusterIssuer"
        metadata = {name = "letsencrypt"}
        spec = {acme = {server = "https://example.invalid"}}
    }
    {
        apiVersion = "v1"
        kind = "ConfigMap"
        metadata = {
            name = "pinned"
            labels = {"forge.dev/env" = "hand-written"}
        }
        data = {a = "b"}
    }
]

# ── 1. The drill-in: Bundle.env states the ownership tag ────────────
_bundle = forge.Bundle {
    env = "dev-k8s"
    cluster_target = _target
    services = [_svc]
    cronjobs = [_cron, _job]
    additional_manifests = _raw
}

manifests = forge.render_manifests(_bundle)

# ── 2. No drill-in AND no `-D env=` binding: no label anywhere ──────
_plain_bundle = forge.Bundle {
    cluster_target = _target
    services = [_svc]
    additional_manifests = _raw
}

plain_manifests = forge.render_manifests(_plain_bundle)

# ── helpers ────────────────────────────────────────────────────────

_KEY = "forge.dev/env"

_labels_of = lambda m: any -> {str: str} {
    (m.metadata.labels if m.metadata.labels else {}) if m.metadata else {}
}

_pick = lambda ms: [any], kind: str, name: str -> any {
    [m for m in ms if m.kind == kind and m.metadata.name == name][0]
}

_deploy = _pick(manifests, "Deployment", "api")
_service = _pick(manifests, "Service", "api")
_cronjob = _pick(manifests, "CronJob", "nightly")
# A one-shot Job's name carries a hash of its rendered pod spec
# (`migrate-<hash>`), so it is selected by kind rather than exact name.
_one_shot = [m for m in manifests if m.kind == "Job"][0]
_issuer = _pick(manifests, "ClusterIssuer", "letsencrypt")
_pinned = _pick(manifests, "ConfigMap", "pinned")
_ns = _pick(manifests, "Namespace", "myapp-shared")

# ── Invariants ─────────────────────────────────────────────────────

# EVERY object the renderer emits carries the drill-in value — typed
# workloads, RBAC, the Namespace, NetworkPolicies and raw pass-through
# manifests alike. The one exception is the object that named its own
# value by hand (asserted separately below).
assert_every_object_stamped = len([
    m for m in manifests
    if _labels_of(m)[_KEY] != "dev-k8s" and m.metadata.name != "pinned"
]) == 0

# ... including kinds no forge label builder touches.
assert_raw_manifest_stamped = _labels_of(_issuer)[_KEY] == "dev-k8s"
assert_namespace_stamped = _labels_of(_ns)[_KEY] == "dev-k8s"

# An author who sets the label by hand WINS — the stamp merges UNDER
# existing labels, never over them.
assert_hand_written_label_wins = _labels_of(_pinned)[_KEY] == "hand-written"

# Pod templates carry it too, so `kubectl get pods -l forge.dev/env=…`
# is meaningful. Without this every pod matches `forge.dev/env!=<env>`
# and the query is pure noise.
assert_pod_template_stamped = _deploy.spec.template.metadata.labels[_KEY] == "dev-k8s"
assert_cronjob_pod_template_stamped = \
    _cronjob.spec.jobTemplate.spec.template.metadata.labels[_KEY] == "dev-k8s"
assert_job_pod_template_stamped = _one_shot.spec.template.metadata.labels[_KEY] == "dev-k8s"

# THE CRITICAL ONE. Selectors are untouched: Deployment.spec.selector
# .matchLabels is IMMUTABLE in Kubernetes, and a changed Service
# spec.selector silently detaches endpoints. Both must stay the exact
# one-key `app.kubernetes.io/name` form the builders emit.
assert_deployment_selector_untouched = \
    _deploy.spec.selector.matchLabels == {"app.kubernetes.io/name" = "api"}
assert_service_selector_untouched = \
    _service.spec.selector == {"app.kubernetes.io/name" = "api"}

# The existing k8s-recommended labels are preserved verbatim — the stamp
# only ADDS a key, so `--target` selection and multi-cluster routing
# (which read app.kubernetes.io/name) are unaffected.
assert_app_labels_preserved = \
    _labels_of(_deploy)["app.kubernetes.io/name"] == "api" and \
    _labels_of(_deploy)["app.kubernetes.io/managed-by"] == "forge" and \
    _labels_of(_deploy)["app.kubernetes.io/part-of"] == "api"

# No binding, no drill-in => no label at all. This is what keeps a plain
# `kcl run` byte-identical to the pre-stamp render.
assert_no_env_no_label = len([
    m for m in plain_manifests if _KEY in _labels_of(m) and m.metadata.name != "pinned"
]) == 0

# ...and the hand-written one still keeps its own value even then.
assert_no_env_hand_written_preserved = \
    _labels_of(_pick(plain_manifests, "ConfigMap", "pinned"))[_KEY] == "hand-written"

# The un-stamped render is IDENTICAL to what the module emitted before
# the stamp existed: the raw ClusterIssuer had no metadata.labels, and
# with no env resolved it must still have none (the stamp is a total
# no-op, not an "empty labels block" no-op).
assert_no_env_no_labels_block = \
    "labels" not in _pick(plain_manifests, "ClusterIssuer", "letsencrypt").metadata
