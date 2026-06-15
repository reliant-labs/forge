"""
Positive test — numeric `image_tag` types as `str` (forge-deploy-prod regression).

`forge deploy` passes the image tag to KCL as a `-D image_tag=<tag>`
binding. The tag is a `git describe` value; on prod it is all-digit
(e.g. "3826648"). A BARE `-D image_tag=3826648` makes KCL coerce the
value to `int`, which then violates `RenderEnv.image_tag: str` and the
deploy fails with `image_tag: str, got int`. Non-numeric tags ("dev",
"v1.2.3") happen to type as `str`, which is why only numeric-tagged
envs broke.

The fix passes the tag as a QUOTED KCL string literal so KCL types it
as `str`. Run me the way the fix does:

    kcl run -D image_tag='"3826648"' kcl/tests/positive_image_tag_numeric.k

The asserts below pin that the option value is a `str`, carries the
expected value, AND is accepted by RenderEnv.image_tag's `str` field
without an EvaluationError.
"""

import forge

# Read the tag exactly as the manifest renderer does.
_tag = option("image_tag") or "latest"

# A minimal cluster so RenderEnv's required `cluster: K8sCluster` is
# satisfied. cluster/namespace/registry are the required fields.
_k8s = forge.K8sCluster {
    cluster = "k3d-myapp"
    namespace = "myapp-prod"
    registry = "localhost:5050"
}

# Constructing this proves the `str` schema field accepts _tag without
# an EvaluationError — the exact step that fails on a bare numeric -D.
_env = forge.RenderEnv {
    cluster = _k8s
    image_tag = _tag
}

# ── Invariants ─────────────────────────────────────────────────────

# The quoted -D form types the option as `str`, not `int`.
assert_tag_is_str = typeof(_tag) == "str"

# The quotes are stripped — the value is the bare digits as a string.
assert_tag_value = _tag == "3826648"

# RenderEnv accepted the value into its `str` field (round-trip).
assert_env_image_tag = _env.image_tag == "3826648"
