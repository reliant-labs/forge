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

And the BUILD side of the same pin: a forge-built workload's artifact (the
entity's `image`, the repository its build pushes to) is the declared image's
repository with the tag stripped, so the registry, that repository and the
resolved tag recompose exactly the spec.image above. `image = "reliant:e2e"`
on a workload named
`api-server` builds IMAGE=reliant (TAG=e2e, read back off spec.image in
Go), never IMAGE=api-server — the ErrImagePull the control-plane migration
hit. An unset image still derives from the build (output_name, else name).
"""

# kcl-args: image_tag="abc1234-dirty"

import forge
import forge.workloads as fw

_target = forge.ClusterTarget {cluster = "k3d-dev", namespace = "dev", registry = "localhost:5050"}
_shell = forge.ShellBuild {cmd = "echo built"}

output = forge.render(forge.Bundle {
    project = "dev"
    workloads = [_w | {runtime = forge.OnCluster {target = _target}} if not _w.runtime else _w for _w in [
        fw.Workload {name = "api", image = "api"}
        fw.Workload {name = "reliant", image = "reliant:v1.4.2"}
        fw.Workload {name = "vendor", image = "ghcr.io/acme/vendor:3.0.0"}
        fw.Workload {name = "pinned-digest", image = "ghcr.io/acme/db@sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd"}
        fw.Workload {name = "baretag", image = "vendorapp:3.0.0"}
        fw.Workload {name = "portreg", image = "localhost:6000/foo"}
        fw.Workload {name = "migrate", kind = "job", image = "reliant:v1.4.2", args = ["db", "migrate", "up"]}
        fw.Workload {name = "nightly", kind = "cron", image = "api", args = ["sweep"], schedule = "@daily"}
        fw.Workload {name = "ctrl", kind = "operator", image = "ctrl:v9.9.9", crds = ["Widget"]}
        # Built workloads (P5's shape): the artifact is the image's repository.
        fw.Workload {name = "api-server", image = "reliant:e2e", args = ["api"], build = _shell}
        fw.Workload {name = "org-built", image = "acme/app:v2", args = ["x"], build = _shell}
        fw.Workload {name = "untagged-built", image = "demo", args = ["x"], build = _shell}
        fw.Workload {name = "docker-built", image = "gw:v7", args = ["x"], build = forge.DockerBuild {output_name = "not-this"}}
        fw.Workload {name = "derived", args = ["x"], build = forge.GoBuild {cmd = "./cmd/demo", output_name = "demo-bin"}}
    ]]
})

_img = {m.metadata.name: m.spec.image for m in output.manifests if m.kind == "Workload"}
_artifact = {w.name: w.image for w in output.workloads}

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

# The build artifact is the pinned image's repository, and REGISTRY/IMAGE
# plus the pinned tag is the deploy ref.
assert_pinned_built_spec = _img["api-server"] == "localhost:5050/reliant:e2e"
assert_pinned_built_artifact = _artifact["api-server"] == "reliant"
assert_org_built_artifact = _artifact["org-built"] == "acme/app" and _img["org-built"] == "localhost:5050/acme/app:v2"
assert_untagged_built_artifact = _artifact["untagged-built"] == "demo" and _img["untagged-built"] == "localhost:5050/demo:abc1234-dirty"
assert_docker_built_artifact = _artifact["docker-built"] == "gw" and _img["docker-built"] == "localhost:5050/gw:v7"
assert_derived_artifact = _artifact["derived"] == "demo-bin" and _img["derived"] == "localhost:5050/demo-bin:abc1234-dirty"
# A workload forge does not build has no artifact.
assert_unbuilt_has_no_artifact = _artifact["reliant"] == ""
