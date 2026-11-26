"""
Negative test — a host MinIO and the compose MinIO (forge.minio_workload) in one env.
The compose MinIO reads its root user from MINIO_ROOT_USER, the same store
key a host MinIO named `minio` is generated under, so the pair is refused.
"""

# expect: their root-credential keys collide

import forge
import forge.workloads as fw

_compose = forge.BucketOnMinIO {port = 9123, service = "s3"}
_buckets = [
    forge.Bucket {name = "uploads", runtime = forge.BucketOnMinIO {infra = "minio"}}
    forge.Bucket {name = "legacy", runtime = _compose}
]

output = forge.render(forge.Bundle {
    project = "acme"
    lifecycle = "local"
    buckets = _buckets
    infra = [forge.HostInfra {name = "minio", engine = "minio", port = 9200}]
    workloads = [forge.minio_workload(_compose, _buckets)]
    secret_provider = forge.FileSecrets {path = "secrets/dev.yaml"}
})
