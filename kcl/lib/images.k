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

# The KEY `image` is looked up under in the `image_digests` map.
#
# A DIGEST-pinned image has no key: it already names its bytes, and nothing
# may override that.
#
# A TAG-pinned image (`reliant:e2e`) is keyed VERBATIM, tag included, because
# a digest is only true of the exact tag it was captured for. The bare name
# `reliant` may carry a digest captured for some OTHER tag of the same image
# — pinning that here would ship bytes that were never pushed under this tag,
# which is a worse failure than the stale tag it was meant to fix.
#
# An UNPINNED image is keyed by its bare name, as it always was: the env tag
# is the only tag in play, so the bare name is unambiguous.
digest_key = lambda image: str -> str {
    "" if "@" in image else image
}

# The full reference for `image`:
#
#   1. It names its own registry host: no registry prefix (it is a third
#      party's, or already fully qualified).
#   2. It pins its own DIGEST: kept verbatim — it already names its bytes.
#   3. `image_digests` has a digest under this image's key (digest_key):
#      the digest is appended. For an unpinned image that is
#      `<registry>/<image>@<digest>`; for a TAG-pinned image the tag is KEPT
#      and the digest appended — `<registry>/<image>:<tag>@<digest>` — so the
#      reference stays readable while the kubelet pulls by digest.
#   4. A tag-pinned image with no digest for that tag: kept verbatim.
#   5. Else the env tag: `[<registry>/]<image>:<tag>`.
#
# WHY A TAG-PINNED IMAGE IS DIGEST-PINNED TOO. An image that pins its own tag
# is still one forge BUILDS and PUSHES (control-plane's e2e env pins
# `reliant:e2e`, and forge's ShellBuild pushes it). Returning it verbatim
# meant a rebuild under the same tag left spec.image byte-identical, so the
# Deployment did not roll out and — with imagePullPolicy IfNotPresent — the
# nodes kept the previous digest, while the deploy reported success. That is
# a silent no-op deploy, the exact failure digest pinning exists to prevent.
image_ref = lambda image: str, registry: str, tag: str, digests: {str:str} -> str {
    _reg = "" if has_registry_host(image) else registry
    _base = "${_reg}/${image}" if _reg else image
    _key = digest_key(image)
    _digest = digests[_key] if (_key and _key in digests) else ""
    _base + "@" + _digest if _digest \
        else (_base if has_tag_or_digest(image) \
        else "${_base}:${tag}")
}
