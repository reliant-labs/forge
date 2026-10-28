---
name: deploy/project-image
description: The project image (the one every service, worker and job runs) — how forge assembles it from forge.yaml's image block with no Dockerfile or Docker daemon, every field and default, extra runtime files, base pinning, multi-arch, and when to build from a Dockerfile instead.
---

# The project image

The PROJECT image is the one `cmd/<bin>` image every service, worker and job
shares. A project that declares it in `forge.yaml` gets it built by forge
itself, the way ko builds one:

1. pull `base` for the target platform from its registry;
2. add one layer holding the binaries `forge build` just cross-compiled
   (`GOOS=linux`, `CGO_ENABLED=0`, `-trimpath`), each at
   `<workdir>/<GoBuild output_name>`;
3. add one layer holding the declared `files`;
4. set the entrypoint, cmd, user, workdir and env;
5. push to every reference the build pushes (`:latest` and `:<tag>`, or the
   release tag alone), with the credentials `docker login` or forge's own
   platform login stored.

No Dockerfile is read, and no `docker`, `buildx` or daemon is involved in the
build, the push or the digest. `forge env deploy <env>` therefore works the
same on a laptop, a CI runner, a cloud sandbox or inside a container. The
pushed manifest digest is what the build state, the release ledger and the
deploy pin record.

## Declaring it

```yaml
# forge.yaml
image:
  base: gcr.io/distroless/static-debian12@sha256:<digest>   # required
  cmd: [server]
  files:
    db/migrations: /app/db/migrations
```

| field | default | meaning |
|---|---|---|
| `base` | (required) | the image the binaries are layered onto. `scratch` is an empty base |
| `workdir` | `/app` | working directory, and where the binaries land |
| `user` | `65532:65532` | distroless's nonroot user |
| `entrypoint` | `[<workdir>/<binary>]` | the binary named after the project, else the only one; set it when there are several and none has the project's name |
| `cmd` | none | the default arguments. A workload's `args` replace it, so keep it to the default subcommand |
| `env` | none | added to the base's environment, replacing a variable of the same name |
| `files` | none | project-relative file or directory → absolute in-image path, for what the process reads at runtime and does not embed |

`forge project new` writes `base` (distroless/static, pinned) and
`cmd: [server]`. The entrypoint is the binary and CMD is only its subcommand
because Kubernetes `args` replace CMD: `args = ["db", "migrate", "up"]` must
reach the binary, not exec a program called `db`.

## Pin the base by digest

A tag is resolved at build time, so two builds of one commit can differ;
forge prints the digest it resolved and asks you to pin it. Pin the digest of
the multi-arch INDEX (`crane digest <ref>`, `docker buildx imagetools inspect
<ref>`): one pin then serves amd64 and arm64, each build pulling its own
platform's manifest from it. A digest of one platform's manifest fails a build
for the other platform with `base image is linux/amd64, not linux/arm64`.

A pull-through mirror is just the reference: write the mirror host into
`base`.

## Platform

The image is built for one platform, `linux/<arch>`, the arch the env
declares (`ClusterTarget.platform`, a hosted control plane's platform) or
`--target-arch`, and the host binaries are compiled for that same arch. The
ELF guard refuses a binary of the wrong arch before anything is pushed.

## Reproducible digests

Entries are sorted and root-owned, every timestamp is the build's
`SOURCE_DATE_EPOCH` (the last commit to the repository, else the
environment's `SOURCE_DATE_EPOCH`, else the Unix epoch), and the config
carries no wall-clock time. The same base, binaries and files give the same
digest.

## Nothing to push

`forge build --docker` without a push loads the image into the local Docker
daemon (`docker load`) under its local tags. With no daemon either, the build
fails: an image with no registry and no daemon has nowhere to live.

## When the image needs more than this

The model has no RUN step on purpose: forge does not execute or interpret
Dockerfiles. An image that needs a package installed or a file generated at
build time has three ways forward:

- bake it into a base image of your own and name that as `base`;
- delete the `image:` block and add a production stage to the root
  `Dockerfile`: forge then runs `docker build` on it (it needs a Docker daemon,
  and fails up front without one);
- give that workload its own `forge.DockerBuild` (any Dockerfile, built with
  `docker build`), or `forge.ShellBuild` for anything else.

The scaffolded root `Dockerfile` holds only the `dev` and `debug` stages
docker-compose builds; it is not the project image.

## Moving an existing project to the model

1. Add the `image:` block. Mirror the Dockerfile's production stage: its
   `FROM` is `base`, its `COPY` of non-binary files are `files`, its
   `ENTRYPOINT`/`CMD`/`USER`/`WORKDIR`/`ENV` are the fields above.
2. Delete the production stage (forge no longer reads it).
3. `forge env build <env> --push`, then compare the two images'
   `docker buildx imagetools inspect --raw` config before deploying.

The digests differ from a `docker build` by construction (no buildkit
history, no `HEALTHCHECK`/`EXPOSE` metadata, the base's manifest media type),
and the container behaves the same. A release is built by one builder, so
promotion by digest is unaffected.
