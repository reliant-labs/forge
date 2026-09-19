"""
Positive test — the polymorphic Build union (GoBuild | DockerBuild |
ShellBuild | RemoteBuild) renders with a self-describing `type`
discriminator, exactly like the deploy union. Consumed by build.go's
dispatch on build.type.
"""

import forge

# GoBuild — the dominant case. cmd is the build target; flags/goarch are
# the per-env knobs an e2e/prod overlay tunes.
_go = forge.RenderedWorkload {
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
_docker = forge.RenderedWorkload {
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
_shell = forge.RenderedWorkload {
    name = "sidecar"
    build = forge.ShellBuild { cmd = "echo hi > out.txt" }
    deploy = forge.BuildOnly {}
}

# RemoteBuild — the artifact is produced by a HOSTED build service, not
# on this machine. Source is a repo+ref PIN rather than a path, because a
# build service cannot read the caller's filesystem.
_remote = forge.RenderedWorkload {
    name = "hosted"
    build = forge.RemoteBuild {
        source = forge.GitSource {
            repo = "github.com/acme/api"
            ref = "v1.2.3"
            subdir = "svc"
        }
        dockerfile = "docker/Dockerfile"
        platform = "linux/arm64"
        target = "runtime"
        build_args = {"VERSION" = "1.2.3"}
        cpu_millicores = 4000
        memory_bytes = 17179869184
        cache_gib = 50
        timeout_seconds = 1800
    }
    deploy = forge.BuildOnly {}
}

_bundle = forge.Bundle { services = [_go, _docker, _shell, _remote] }
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

assert_remote_type = output.services[3].build.type == "remote"
# The SOURCE PIN is what makes a remote build possible at all — a build
# service has no access to the caller's disk, so losing repo/ref here
# would submit a build with nothing to build.
assert_remote_repo = output.services[3].build.source.repo == "github.com/acme/api"
assert_remote_ref = output.services[3].build.source.ref == "v1.2.3"
assert_remote_subdir = output.services[3].build.source.subdir == "svc"
assert_remote_dockerfile = output.services[3].build.dockerfile == "docker/Dockerfile"
assert_remote_platform = output.services[3].build.platform == "linux/arm64"
# The size fields are the BILLING dimensions, so a render that dropped
# them would bill a default rather than what was declared.
assert_remote_cpu = output.services[3].build.cpu_millicores == 4000
assert_remote_mem = output.services[3].build.memory_bytes == 17179869184
assert_remote_cache = output.services[3].build.cache_gib == 50
assert_remote_timeout = output.services[3].build.timeout_seconds == 1800

# A service that omits `build` projects null — the Go side synthesizes
# the GoBuild { cmd = "./cmd/<name>" } default.
_plain = forge.RenderedWorkload {
    name = "plain"
    deploy = forge.BuildOnly {}
}
_pb = forge.Bundle { services = [_plain] }
_plain_out = forge.render(_pb)
assert_plain_build_null = _plain_out.services[0].build == None

# build_of coerces a plain dict (the workloads.k / overlay shape)
# to the right typed Build subtype by discriminator — the bridge path.
# This is the case that bare-dict union coercion gets wrong (a shell dict
# also satisfies GoBuild's required cmd), so it must route through build_of.
_bo_go = forge.RenderedWorkload { name = "bog", deploy = forge.BuildOnly {}, build = forge.build_of({"type" = "go", "cmd" = "./cmd/bog"}) }
_bo_docker = forge.RenderedWorkload { name = "bod", deploy = forge.BuildOnly {}, build = forge.build_of({"type" = "docker", "dockerfile" = "Dockerfile.x"}) }
_bo_shell = forge.RenderedWorkload { name = "bos", deploy = forge.BuildOnly {}, build = forge.build_of({"type" = "shell", "cmd" = "make"}) }
_bo_remote = forge.RenderedWorkload { name = "bor", deploy = forge.BuildOnly {}, build = forge.build_of({"type" = "remote", "source" = forge.GitSource {repo = "github.com/acme/x", ref = "main"}}) }
_bo_empty = forge.RenderedWorkload { name = "boe", deploy = forge.BuildOnly {}, build = forge.build_of({}) }
_bo_out = forge.render(forge.Bundle { services = [_bo_go, _bo_docker, _bo_shell, _bo_remote, _bo_empty] })
assert_bo_go = _bo_out.services[0].build.type == "go" and _bo_out.services[0].build.cmd == "./cmd/bog"
assert_bo_docker = _bo_out.services[1].build.type == "docker" and _bo_out.services[1].build.dockerfile == "Dockerfile.x"
assert_bo_shell = _bo_out.services[2].build.type == "shell" and _bo_out.services[2].build.cmd == "make"
# The RemoteBuild dict routed through build_of must bind to RemoteBuild
# and NOT to an earlier union member. This is the case bare-dict coercion
# gets wrong: KCL's union matching is structural-first, so a dict is
# checked against GoBuild before RemoteBuild, and only the discriminator-
# aware constructor picks the intended member. A mis-bind would not error
# loudly — it would render a build of the WRONG TYPE, which the Go side
# would then dispatch to a local builder for a source that is not on disk.
assert_bo_remote = _bo_out.services[3].build.type == "remote" and _bo_out.services[3].build.source.repo == "github.com/acme/x"
# Defaults survive the coercion path: a dict that omits them must not
# render zeros, because these are the billing dimensions and the cap.
assert_bo_remote_defaults = _bo_out.services[3].build.cpu_millicores == 2000 and _bo_out.services[3].build.timeout_seconds == 900
assert_bo_empty = _bo_out.services[4].build == None
