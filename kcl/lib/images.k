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

# The full reference for `image`:
#
#   1. It names its own registry host: passed through (a third party's).
#   2. It pins its own tag or digest: `<registry>/<image>`, verbatim tag.
#   3. `image_digests` has a digest for this BARE name (the map
#      `forge env deploy` passes, keyed on the image name, so two workloads
#      sharing an image share its digest): `<registry>/<image>@<digest>`.
#   4. Else `<registry>/<image>:<tag>`.
image_ref = lambda image: str, registry: str, tag: str, digests: {str:str} -> str {
    _own = has_registry_host(image)
    _reg = "" if _own else registry
    _digest = digests[image] if (image in digests) else ""
    image if _own \
        else ("${_reg}/${image}" if has_tag_or_digest(image) and _reg \
        else (image if has_tag_or_digest(image) \
        else ("${_reg}/${image}@${_digest}" if _digest and _reg \
        else ("${_reg}/${image}:${tag}" if _reg else "${image}:${tag}"))))
}
