"""
Negative test — a forge.Bucket on forge.OnHosted is refused with a pointer:
the control plane does not provision buckets yet.
"""

# expect: does not provision buckets yet
# expect: forge.BucketOnS3

import forge
import forge.workloads as fw

_img = "ghcr.io/acme/api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

output = forge.render(forge.Bundle {
    project = "acme"
    control_plane = forge.ControlPlane {endpoint = "https://api.example.com"}
    buckets = [forge.Bucket {name = "uploads", runtime = forge.OnHosted {}}]
    workloads = [fw.Workload {name = "web", image = _img, runtime = forge.OnHosted {}}]
})
