"""
Positive test — the polymorphic Build union (GoBuild | DockerBuild |
ShellBuild) renders with a self-describing `type` discriminator, exactly
like the deploy union. Consumed by build.go's dispatch on build.type.
"""

import forge

# GoBuild — the dominant case. cmd is the build target; flags/goarch are
# the per-env knobs an e2e/prod overlay tunes.
_go = forge.Service {
    name = "trader"
    build = forge.GoBuild {
        cmd = "./cmd/trader"
        goarch = "arm64"
        flags = ["-cover"]
        ldflags = ["-s", "-w", "-X main.version=1.2.3"]
        env = {"CGO_ENABLED" = "0"}
    }
    deploy = forge.K8sCluster {
        cluster = "k3d-dev"
        namespace = "dev"
        registry = "localhost:5051"
    }
}

# DockerBuild — image is produced via docker build with this dockerfile
# + platform + target.
_docker = forge.Service {
    name = "gateway"
    build = forge.DockerBuild {
        dockerfile = "Dockerfile.gateway"
        platform = "linux/amd64"
        target = "runtime"
        build_args = {"VERSION" = "1.2.3"}
    }
    deploy = forge.K8sCluster {
        cluster = "k3d-dev"
        namespace = "dev"
        registry = "localhost:5051"
    }
}

# ShellBuild — verbatim command escape hatch.
_shell = forge.Service {
    name = "sidecar"
    build = forge.ShellBuild { cmd = "echo hi > out.txt" }
    deploy = forge.BuildOnly {}
}

_bundle = forge.Bundle { services = [_go, _docker, _shell] }
output = forge.render(_bundle)

assert_go_type = output.services[0].build.type == "go"
assert_go_cmd = output.services[0].build.cmd == "./cmd/trader"
assert_go_goarch = output.services[0].build.goarch == "arm64"
assert_go_flags = output.services[0].build.flags == ["-cover"]
assert_go_env = output.services[0].build.env["CGO_ENABLED"] == "0"

assert_docker_type = output.services[1].build.type == "docker"
assert_docker_dockerfile = output.services[1].build.dockerfile == "Dockerfile.gateway"
assert_docker_platform = output.services[1].build.platform == "linux/amd64"
assert_docker_target = output.services[1].build.target == "runtime"

assert_shell_type = output.services[2].build.type == "shell"
assert_shell_cmd = output.services[2].build.cmd == "echo hi > out.txt"

# A service that omits `build` projects null — the Go side synthesizes
# the GoBuild { cmd = "./cmd/<name>" } default.
_plain = forge.Service {
    name = "plain"
    deploy = forge.BuildOnly {}
}
_pb = forge.Bundle { services = [_plain] }
_plain_out = forge.render(_pb)
assert_plain_build_null = _plain_out.services[0].build == None
