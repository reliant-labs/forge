"""
Positive test — the agnostic-core env MAP (`{str: EnvSource}`) + the k8s
adapter projection `forge.env_project`. ADDITIVE alongside EnvVar/env_merge.

Pins the load-bearing claims of FORGE_KCL_IDIOMS.md §3:

  1. ALL FOUR CHANNELS project to the correct EnvVar shape
     (value / from_secret->secret_ref+secret_key /
      from_config->config_map_ref+config_map_key /
      from_field->field_ref [Downward API]).
  2. INSERTION ORDER preserved — the projection follows the author's
     map-literal order verbatim, so k8s `$(VAR)` dependent expansion works
     (referenced var declared earlier). (Was sorted-by-key — a regression.)
  3. NATIVE MAP-MERGE (`base | overrides`) overrides last-wins with NO
     helper — the map makes the dedup footgun structurally impossible.
  4. EMPTY-STRING value is a legal channel (mirrors EnvVar).
  5. RENDERER-COMPATIBLE — env_project's [EnvVar] renders to the SAME k8s
     container env: a hand-written [EnvVar] list does, through the existing
     render_manifests path.
"""

import forge

# ── (1) all three channels ─────────────────────────────────────────
_env = {
    "LOG_LEVEL" = {value = "info"}
    "DATABASE_URL" = {from_secret = {name = "db", key = "url"}}
    "APP_MODE" = {from_config = {name = "app-config", key = "mode"}}
}

_projected = forge.env_project(_env)

# INSERTION order preserved (authoring order): LOG_LEVEL, DATABASE_URL,
# APP_MODE — NOT alphabetical. This is the fix for the sort-by-key regression
# that silently broke k8s `$(VAR)` interpolation.
assert_count = len(_projected) == 3
assert_order = [e.name for e in _projected] == ["LOG_LEVEL", "DATABASE_URL", "APP_MODE"]

_by_name = {e.name: e for e in _projected}

# value channel
assert_value_channel = _by_name["LOG_LEVEL"].value == "info"
assert_value_no_secret = _by_name["LOG_LEVEL"].secret_ref == "" and _by_name["LOG_LEVEL"].config_map_ref == ""

# from_secret -> secret_ref + secret_key
assert_secret_ref = _by_name["DATABASE_URL"].secret_ref == "db"
assert_secret_key = _by_name["DATABASE_URL"].secret_key == "url"
assert_secret_no_value = _by_name["DATABASE_URL"].value == Undefined

# from_config -> config_map_ref + config_map_key
assert_config_ref = _by_name["APP_MODE"].config_map_ref == "app-config"
assert_config_key = _by_name["APP_MODE"].config_map_key == "mode"
assert_config_no_value = _by_name["APP_MODE"].value == Undefined

# The projected entries are genuine EnvVar instances (pass EnvVar's own check).
assert_is_envvar = typeof(_projected[0]) == "EnvVar"

# ── (2) INSERTION order preserved — authors control ordering ───────
# KCL dicts iterate in insertion order (verified empirically), so the
# projected [EnvVar] follows the author's map-literal order EXACTLY — the
# projection does NOT reorder. This is what makes k8s `$(VAR)` dependent
# expansion authorable: a var can reference an EARLIER-declared var.
_scrambled = {
    "ZULU" = {value = "z"}
    "ALPHA" = {value = "a"}
    "MIKE" = {value = "m"}
}
assert_insertion_order = [e.name for e in forge.env_project(_scrambled)] == ["ZULU", "ALPHA", "MIKE"]

# ── (2b) $(VAR) dependent-expansion ordering ───────────────────────
# The load-bearing case: HOST + PORT declared BEFORE URL, so URL's
# `$(HOST):$(PORT)` reference resolves against entries that appear EARLIER in
# the rendered list — k8s expands them. A sort-by-key projection would have
# put URL before HOST (U < ...no; but PORT<URL yes, HOST<URL) — here we prove
# the author's dependency order survives verbatim.
_dependent = {
    "HOST" = {value = "db.internal"}
    "PORT" = {value = "5432"}
    "DATABASE_URL" = {value = "postgres://$(HOST):$(PORT)/app"}
}
_dep_projected = forge.env_project(_dependent)
assert_dependent_order = [e.name for e in _dep_projected] == ["HOST", "PORT", "DATABASE_URL"]
# The two referenced vars precede the referrer (indices 0,1 < 2).
assert_dependent_refs_earlier = _dep_projected[2].value == "postgres://$(HOST):$(PORT)/app"

# ── (2c) Downward API — from_field -> k8s valueFrom.fieldRef ───────
_downward = {
    "POD_NAME" = {from_field = "metadata.name"}
    "POD_IP" = {from_field = "status.podIP"}
    "NODE_NAME" = {from_field = "spec.nodeName"}
}
_dw = forge.env_project(_downward)
_dw_by_name = {e.name: e for e in _dw}
assert_field_ref_set = _dw_by_name["POD_NAME"].field_ref == "metadata.name"
assert_field_ref_no_value = _dw_by_name["POD_NAME"].value == Undefined
# Render through the k8s adapter and assert the fieldRef shape.
_dw_target = forge.ClusterTarget {
    cluster = "k3d-downward"
    namespace = "downward-dev"
    registry = "localhost:5050"
}
_dw_svc = forge.RenderedWorkload {
    name = "downward"
    image = "api"
    deploy = _dw_target.deploy | {ports = [8080]}
    env_vars = _dw
}
_dw_manifests = forge.render_manifests(forge.Bundle {cluster_target = _dw_target, services = [_dw_svc]}, "latest", {}, False)
_dw_dep = [m for m in _dw_manifests if m.kind == "Deployment"][0]
_dw_env = {e.name: e for e in _dw_dep.spec.template.spec.containers[0].env}
assert_render_field_ref = _dw_env["POD_IP"].valueFrom.fieldRef == {fieldPath = "status.podIP"}
assert_render_field_no_value = "value" not in _dw_env["NODE_NAME"]

# ── (3) native map-merge, last-wins, no helper ─────────────────────
_base_env = {
    "LOG_LEVEL" = {value = "info"}
    "REGION" = {value = "us-east"}
}
_merged_env = _base_env | {"LOG_LEVEL" = {value = "debug"}, "FEATURE_X" = {value = "on"}}
assert_merge_count = len(_merged_env) == 3
assert_merge_override = _merged_env["LOG_LEVEL"].value == "debug"   # last-wins
assert_merge_preserved = _merged_env["REGION"].value == "us-east"
assert_merge_added = _merged_env["FEATURE_X"].value == "on"
# The map physically CANNOT hold two LOG_LEVEL keys — dedup footgun impossible.
_merged_projected = forge.env_project(_merged_env)
_merged_log = [e for e in _merged_projected if e.name == "LOG_LEVEL"]
assert_merge_one_log = len(_merged_log) == 1
assert_merge_projected_value = _merged_log[0].value == "debug"

# ── (4) empty-string value is a legal channel ──────────────────────
_empty = forge.env_project({"RUNTIME_CLASS_NAME" = {value = ""}})
assert_empty_value_set = _empty[0].value == ""
assert_empty_value_is_value_channel = _empty[0].secret_ref == "" and _empty[0].config_map_ref == ""

# ── (5) renderer-compatible — project == hand-written [EnvVar] ─────
# Same three vars authored two ways: as a keyed map (projected) and as a
# hand-written [EnvVar] list. Render BOTH through render_manifests and assert
# the k8s container env: blocks are IDENTICAL.
_target = forge.ClusterTarget {
    cluster = "k3d-env-project"
    namespace = "env-project-dev"
    registry = "localhost:5050"
}

# The hand-written list, in the SAME insertion order env_project emits
# (matching the _env map literal above: LOG_LEVEL, DATABASE_URL, APP_MODE).
_handwritten = [
    forge.EnvVar {name = "LOG_LEVEL", value = "info"}
    forge.EnvVar {name = "DATABASE_URL", secret_ref = "db", secret_key = "url"}
    forge.EnvVar {name = "APP_MODE", config_map_ref = "app-config", config_map_key = "mode"}
]

_svc_map = forge.RenderedWorkload {
    name = "api-map"
    image = "api"
    deploy = _target.deploy | {ports = [8080]}
    env_vars = forge.env_project(_env)
}
_svc_list = forge.RenderedWorkload {
    name = "api-list"
    image = "api"
    deploy = _target.deploy | {ports = [8080]}
    env_vars = _handwritten
}
_bundle = forge.Bundle {cluster_target = _target, services = [_svc_map, _svc_list]}
manifests = forge.render_manifests(_bundle, "latest", {}, False)

_deploys = {m.metadata.name: m for m in manifests if m.kind == "Deployment"}
_map_env = _deploys["api-map"].spec.template.spec.containers[0].env
_list_env = _deploys["api-list"].spec.template.spec.containers[0].env

# The rendered container env blocks are byte-identical.
assert_render_equivalent = _map_env == _list_env

# And spot-check the actual k8s shapes the projection produced.
assert_render_count = len(_map_env) == 3
_map_by_name = {e.name: e for e in _map_env}
assert_render_value = _map_by_name["LOG_LEVEL"].value == "info"
assert_render_secret = _map_by_name["DATABASE_URL"].valueFrom.secretKeyRef == {name = "db", key = "url"}
assert_render_config = _map_by_name["APP_MODE"].valueFrom.configMapKeyRef == {name = "app-config", key = "mode"}
