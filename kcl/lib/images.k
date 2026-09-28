"""
Image reference resolution for the cluster runtime.

A cluster workload's `spec.image` is resolved at render, against its
target's registry, the env's tag and the `-D image_digests` pin map, so the
Workload record names exactly what the cluster pulls. The rules are the ones
forge has always applied (they moved here from the deleted KCL Kubernetes
renderer).
"""

# Does `image` already carry its OWN tag or digest (`name:tag`,
# `name@sha256:...`), so the env tag must NOT be appended? A registry-port
# colon (`localhost:5050/foo`) sits BEFORE the last `/`, so a tag colon is
# the rightmost `:` after the rightmost `/`. A digest is any `@`.
has_tag_or_digest = lambda image: str -> bool {
    _last_slash = image.rfind("/")
    _tail = image[_last_slash + 1:] if _last_slash >= 0 else image
    ("@" in image) or (":" in _tail)
}

# Does `image` already name its OWN registry host (`gcr.io/x/y`,
# `localhost:5050/z`), so the env registry must NOT be prefixed? Docker's own
# heuristic: the FIRST path segment is a registry host iff it contains a `.`
# or a `:`, or is exactly `localhost`. Bare `nginx` and `library/nginx` take
# the env registry. Prefixing a third-party ref produced unpullable images
# (`<registry>/gcr.io/...`) that failed only as ImagePullBackOff.
has_registry_host = lambda image: str -> bool {
    _first_slash = image.find("/")
    _head = image[:_first_slash] if _first_slash > 0 else ""
    _head != "" and (("." in _head) or (":" in _head) or (_head == "localhost"))
}

# `image` without its own registry host and without any `:tag` / `@digest`
# — the REPOSITORY a build pushes to under `${REGISTRY}/${IMAGE}:${TAG}`,
# and the key build state and the release ledger record. The org path is
# kept (`acme/app:v2` -> `acme/app`), because image_ref keeps it too: build
# and deploy name the same repository only if both keep it.
repository = lambda image: str -> str {
    _no_digest = image.split("@")[0]
    _last_slash = _no_digest.rfind("/")
    _colon = _no_digest.rfind(":")
    _untagged = _no_digest[:_colon] if _colon > _last_slash else _no_digest
    _untagged[_untagged.find("/") + 1:] if has_registry_host(_untagged) else _untagged
}

# The full reference for `image`:
#
#   1. It names its own registry host: no registry prefix (it is a third
#      party's, or already fully qualified).
#   2. It pins its own tag or digest: kept verbatim.
#   3. `image_digests` has a digest for this BARE name (the map
#      `forge env deploy` passes, keyed on the image name, so two workloads
#      sharing an image share its digest): `<registry>/<image>@<digest>`.
#   4. Else the env tag: `[<registry>/]<image>:<tag>`.
image_ref = lambda image: str, registry: str, tag: str, digests: {str:str} -> str {
    _reg = "" if has_registry_host(image) else registry
    _base = "${_reg}/${image}" if _reg else image
    _digest = digests[image] if (image in digests) else ""
    _base if has_tag_or_digest(image) \
        else ("${_base}@${_digest}" if _digest \
        else "${_base}:${tag}")
}
