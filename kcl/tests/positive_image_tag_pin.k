"""
Positive test — how a cluster workload's `spec.image` is RESOLVED in the
lowering (map §2.3: resolved KCL-side so `-D image_tag` / `-D image_digests`
plumbing stays in one place).

  * `image = "api"` (a registry-less artifact name) → <target registry>/api:<env tag>.
  * A per-workload pinned tag (`image = "reliant:v1.4.2"` with no registry
    host) → <target registry>/reliant:v1.4.2 — the env tag is NOT fanned onto
    a sibling image that was never pushed under it (the ErrImagePull bug).
  * An image naming its OWN registry host and `:tag` passes through
    verbatim — the env registry is NOT prefixed (the old
    `localhost:5050/ghcr.io/acme/vendor:3.0.0` ImagePullBackOff).
  * An `@sha256:` digest ref passes through verbatim.
  * A registry PORT colon is not a tag (`localhost:5050/foo` gets the tag).
  * Jobs, crons and operators resolve the same way (one rule, every kind).
"""

# kcl-args: image_tag="abc1234-dirty"

import forge
import forge.workloads as fw

_target = forge.ClusterTarget {cluster = "k3d-dev", namespace = "dev", registry = "localhost:5050"}

output = forge.render(forge.Bundle {
    project = "dev"
    runtime = forge.OnCluster {target = _target}
    workloads = [
        fw.Workload {name = "api", image = "api"}
        fw.Workload {name = "reliant", image = "reliant:v1.4.2"}
        fw.Workload {name = "vendor", image = "ghcr.io/acme/vendor:3.0.0"}
        fw.Workload {name = "pinned-digest", image = "ghcr.io/acme/db@sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd"}
        fw.Workload {name = "baretag", image = "vendorapp:3.0.0"}
        fw.Workload {name = "portreg", image = "localhost:6000/foo"}
        fw.Workload {name = "migrate", kind = "job", image = "reliant:v1.4.2", args = ["db", "migrate", "up"]}
        fw.Workload {name = "nightly", kind = "cron", image = "api", args = ["sweep"], schedule = "@daily"}
        fw.Workload {name = "ctrl", kind = "operator", image = "ctrl:v9.9.9", crds = ["Widget"]}
    ]
})

_img = {m.metadata.name: m.spec.image for m in output.manifests if m.kind == "Workload"}

assert_image_tag_is_env_tag = output.image_tag == "abc1234-dirty"
assert_env_tag = _img["api"] == "localhost:5050/api:abc1234-dirty"
assert_pinned = _img["reliant"] == "localhost:5050/reliant:v1.4.2"
assert_explicit_host_passthrough = _img["vendor"] == "ghcr.io/acme/vendor:3.0.0"
assert_digest_passthrough = _img["pinned-digest"] == \
    "ghcr.io/acme/db@sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd"
assert_bare_tagged_keeps_registry = _img["baretag"] == "localhost:5050/vendorapp:3.0.0"
assert_port_colon_is_not_a_tag = _img["portreg"] == "localhost:6000/foo:abc1234-dirty"
assert_job_pinned = _img["migrate"] == "localhost:5050/reliant:v1.4.2"
assert_cron_env_tag = _img["nightly"] == "localhost:5050/api:abc1234-dirty"
assert_operator_pinned = _img["ctrl"] == "localhost:5050/ctrl:v9.9.9"
# The entity carries the same resolved ref (the release ledger reads it).
assert_entity_matches_record = all w in output.workloads { w.spec.image == _img[w.name] }
