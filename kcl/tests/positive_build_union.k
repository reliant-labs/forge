"""
Positive test — the polymorphic Build union (GoBuild | DockerBuild |
ShellBuild) renders with a self-describing `type` discriminator, exactly
like the deploy union. Consumed by build.go's dispatch on build.type.
"""

import forge

# GoBuild — the dominant case. cmd is the build target; flags/goarch are
# the per-env knobs an e2e/prod overlay tunes.
_go = forge.K8sWorkload {
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
_docker = forge.K8sWorkload {
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
_shell = forge.K8sWorkload {
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
_plain = forge.K8sWorkload {
    name = "plain"
    deploy = forge.BuildOnly {}
}
_pb = forge.Bundle { services = [_plain] }
_plain_out = forge.render(_pb)
assert_plain_build_null = _plain_out.services[0].build == None

# build_of coerces a plain dict (the components_gen.json / overlay shape)
# to the right typed Build subtype by discriminator — the bridge path.
# This is the case that bare-dict union coercion gets wrong (a shell dict
# also satisfies GoBuild's required cmd), so it must route through build_of.
_bo_go = forge.K8sWorkload { name = "bog", deploy = forge.BuildOnly {}, build = forge.build_of({"type" = "go", "cmd" = "./cmd/bog"}) }
_bo_docker = forge.K8sWorkload { name = "bod", deploy = forge.BuildOnly {}, build = forge.build_of({"type" = "docker", "dockerfile" = "Dockerfile.x"}) }
_bo_shell = forge.K8sWorkload { name = "bos", deploy = forge.BuildOnly {}, build = forge.build_of({"type" = "shell", "cmd" = "make"}) }
_bo_empty = forge.K8sWorkload { name = "boe", deploy = forge.BuildOnly {}, build = forge.build_of({}) }
_bo_out = forge.render(forge.Bundle { services = [_bo_go, _bo_docker, _bo_shell, _bo_empty] })
assert_bo_go = _bo_out.services[0].build.type == "go" and _bo_out.services[0].build.cmd == "./cmd/bog"
assert_bo_docker = _bo_out.services[1].build.type == "docker" and _bo_out.services[1].build.dockerfile == "Dockerfile.x"
assert_bo_shell = _bo_out.services[2].build.type == "shell" and _bo_out.services[2].build.cmd == "make"
assert_bo_empty = _bo_out.services[3].build == None
