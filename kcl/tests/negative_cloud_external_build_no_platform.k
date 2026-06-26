"""
Negative test — a CLOUD cluster_target (a real remote registry) that ships a
REAL external image build (a non-no-op ShellBuild) MUST declare `platform`.

This is the arm64-image-on-amd64-node footgun: without `platform`, the
external build pipeline falls through to the dev HOST's arch
(resolveExternalBuildTargetArch → runtime.GOARCH), so a `forge build` from an
arm64 Mac pushes arm64 images that exec-format-error / ImagePullBackOff on
amd64 cloud nodes. The Bundle check converts that silent deploy-time break
into a KCL-load failure naming `platform`.
"""

import forge

# Cloud target — a real remote registry, but NO platform stated.
_prod = forge.ClusterTarget {
    cluster = "gke_proj_us-central1_prod"
    namespace = "app-prod"
    registry = "ghcr.io/org/app"
}

# A service whose image is produced by a REAL external build (cross-compile +
# docker build + push) — its ${TARGETARCH} comes from the env platform.
_sibling = forge.Service {
    name = "sibling"
    image = "sibling"
    build = forge.ShellBuild {
        cmd = r"""
        GOOS=linux GOARCH=${TARGETARCH} go build -o bin/sibling ./cmd/sibling
        docker build --platform=linux/${TARGETARCH} -t ${REGISTRY}/${IMAGE}:${TAG} .
        docker push ${REGISTRY}/${IMAGE}:${TAG}
        """
    }
    deploy = _prod.deploy
}

output = forge.render(forge.Bundle {
    cluster_target = _prod
    services = [_sibling]
})
