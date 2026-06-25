"""
Positive test — `forge.env_merge(base, overrides)` is the explicit,
public, last-wins replacement for the `base + overrides` list-`+` footgun.

Pins the three load-bearing claims:

  1. NAME-KEYED OVERRIDE-WINS — a name present in both lists survives
     ONCE, carrying the `overrides` value (later wins).
  2. POSITION = last-occurrence — an overridden var moves to the position
     its override occupies, exactly as the render-time `_dedup_env_vars`
     safety net produced for `base + overrides`. This is the
     render-equivalence guarantee: migrating a `+` chain to env_merge does
     NOT reorder the rendered container env.
  3. THE WHOLE POINT — a raw `base + overrides` with a shared name reaches
     the renderer as a DUPLICATE (which kubectl rejects); env_merge
     collapses it BEFORE render, and is idempotent through the renderer's
     own dedup.
"""

import forge

_base = [
    forge.EnvVar { name = "LOG_LEVEL", value = "info" }
    forge.EnvVar { name = "DATABASE_URL", secret_ref = "db", secret_key = "url" }
    forge.EnvVar { name = "REGION", value = "us-east" }
]

_overrides = [
    forge.EnvVar { name = "LOG_LEVEL", value = "debug" }   # supersedes base
    forge.EnvVar { name = "FEATURE_X", value = "on" }       # new
]

_merged = forge.env_merge(_base, _overrides)

# ── (1) override-wins, one entry per name ──────────────────────────
# 3 base names + 1 new name (FEATURE_X); LOG_LEVEL collapses to one.
assert_count = len(_merged) == 4
_by_name = {ev.name: ev.value for ev in _merged}
assert_log_level_overridden = _by_name["LOG_LEVEL"] == "debug"
assert_region_preserved = _by_name["REGION"] == "us-east"
assert_feature_added = _by_name["FEATURE_X"] == "on"

# ── (2) position = last-occurrence (render-equivalent ordering) ────
# base+overrides = [LOG_LEVEL, DATABASE_URL, REGION, LOG_LEVEL', FEATURE_X];
# keeping the LAST occurrence of each name yields, in order:
#   DATABASE_URL, REGION, LOG_LEVEL', FEATURE_X
# (the first LOG_LEVEL is dropped; LOG_LEVEL takes the override's slot).
assert_order_0 = _merged[0].name == "DATABASE_URL"
assert_order_1 = _merged[1].name == "REGION"
assert_order_2 = _merged[2].name == "LOG_LEVEL"
assert_order_3 = _merged[3].name == "FEATURE_X"

# Secret-ref entry round-tripped intact through the merge.
assert_secret_ref_kept = _merged[0].secret_ref == "db" and _merged[0].secret_key == "url"

# ── (3) renders cleanly (no duplicate keys) through render_manifests ─
_target = forge.ClusterTarget {
    cluster = "k3d-env-merge"
    namespace = "env-merge-dev"
    registry = "localhost:5050"
}
_svc = forge.Service {
    name = "api"
    image = "api"
    deploy = _target.deploy | {ports = [8080]}
    env_vars = _merged
}
_bundle = forge.Bundle { cluster_target = _target, services = [_svc] }

manifests = forge.render_manifests(_bundle, "latest", {}, False)
_deployments = [m for m in manifests if m.kind == "Deployment"]
_container_env = _deployments[0].spec.template.spec.containers[0].env

# env_merge already deduped, so the renderer's safety net is a no-op:
# exactly 4 entries, one LOG_LEVEL, carrying the override value.
assert_container_count = len(_container_env) == 4
_log_entries = [e for e in _container_env if e.name == "LOG_LEVEL"]
assert_one_log_level = len(_log_entries) == 1
assert_container_log_value = _log_entries[0].value == "debug"

# ── render-equivalence: env_merge(base, ov) == _dedup(base + ov) ───
# Reproduce the renderer's last-wins dedup locally on the raw concat and
# assert env_merge produces the identical name SEQUENCE. This is the
# byte-for-byte guarantee that swapping `base + ov` for env_merge does not
# change the rendered output.
_concat = _base + _overrides
_dedup_concat = [
    _concat[i] for i in range(len(_concat))
    if len([1 for j in range(i + 1, len(_concat)) if _concat[j].name == _concat[i].name]) == 0
]
assert_render_equivalent_names = \
    [e.name for e in _merged] == [e.name for e in _dedup_concat]
assert_render_equivalent_values = \
    [e.value for e in _merged] == [e.value for e in _dedup_concat]
