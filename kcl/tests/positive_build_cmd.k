"""
Positive test — Service.build_cmd renders cleanly. External build-target
shell-command escape hatch consumed by the build pipeline in
internal/cli/build.go (and forge/internal/buildtarget once Phase 2 lands).
"""

import forge

# A cross-repo build that cross-compiles a sibling Go binary, packages
# it via a forge-side Dockerfile, and pushes the resulting image to the
# project's registry. Raw-string literals (r"...") keep KCL from
# interpreting ${X} as KCL interpolation — the tokens land in the
# rendered JSON verbatim so the Go-side runner can expand them.
_svc = forge.Service {
    name = "daemon-gateway"
    image = "reliant-daemon-gateway"
    build_cmd = r"""
        cd ${BUILD_CWD} && \
        GOOS=linux GOARCH=${TARGETARCH} CGO_ENABLED=0 go build -o /tmp/bin ./cmd/reliant && \
        docker build -t ${REGISTRY}/${IMAGE}:${TAG} -f ${PROJECT_DIR}/docker/Dockerfile.daemon-gateway.dev /tmp && \
        docker push ${REGISTRY}/${IMAGE}:${TAG}
    """
    build_cwd = "../reliant"
    build_env = {"BUILD_CWD" = "../reliant"}
    deploy = forge.K8sCluster {
        cluster = "k3d-dev"
        namespace = "dev"
        registry = "localhost:5051"
    }
}

_bundle = forge.Bundle { services = [_svc] }
output = forge.render(_bundle)

assert_build_cmd_set = output.services[0].build_cmd != None
assert_build_cwd_set = output.services[0].build_cwd == "../reliant"
assert_build_env_set = output.services[0].build_env["BUILD_CWD"] == "../reliant"
assert_deploy_unchanged = output.services[0].deploy.type == "cluster"

# Minimal shape — service WITHOUT build_cmd still renders with the
# expected null + empty defaults so consumers see a stable shape.
_plain = forge.Service {
    name = "ordinary"
    image = "ordinary"
    deploy = forge.K8sCluster {
        cluster = "k3d-dev"
        namespace = "dev"
        registry = "localhost:5051"
    }
}
_b = forge.Bundle { services = [_plain] }
_plain_out = forge.render(_b)
assert_plain_build_cmd_null = _plain_out.services[0].build_cmd == None
assert_plain_build_cwd_null = _plain_out.services[0].build_cwd == None
assert_plain_build_env_empty = len(_plain_out.services[0].build_env) == 0
