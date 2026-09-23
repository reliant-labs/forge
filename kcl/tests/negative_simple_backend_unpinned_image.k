"""
Negative test — SimpleBackend rejects an image with no tag or digest.

forge does NOT build a SimpleBackend's image and does not stamp the
env-wide tag onto it, so there is no later step at which an unpinned
reference becomes pinned. `ghcr.io/acme/app` resolves to `:latest` at
pull time, which means a redeploy — or merely a pod reschedule onto a
node with a colder cache — can silently change what runs, with the
declaration and the deploy record both still reading `ghcr.io/acme/app`.

That is precisely the class of drift the immutable-digest machinery
elsewhere in forge exists to prevent (see `_image_ref`'s image_digests
branch), and it cannot help here because forge never pushed this image
and captured no digest for it. So the pin has to be the app owner's, and it
has to be required at declaration time.
"""

import forge

_unpinned = forge.RenderedWorkload {
    name = "api"
    deploy = forge.SimpleBackend {
        cluster = "hosted"
        namespace = "acme-prod"
        image = "ghcr.io/acme/app"
        ports = [8080]
    }
}

_bundle = forge.Bundle {
    cluster_target = forge.ClusterTarget {
        cluster = "hosted"
        namespace = "acme-prod"
        registry = "ghcr.io/reliant-labs"
    }
    services = [_unpinned]
}

output = forge.render(_bundle)
