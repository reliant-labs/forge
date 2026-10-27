---
name: deploy/daemonless-build
description: Building and pushing the project image with no Docker daemon — when forge assembles the image itself, which Dockerfiles it accepts, what it refuses and why, how --image-builder chooses, and what the recorded digest is.
---

# Building the project image without Docker

`forge env deploy <env>` and `forge env build <env> --push` need no Docker
daemon for the PROJECT image (the one `cmd/<bin>` image every service, worker
and job shares). A CI runner, a cloud sandbox or a container deploys with the
same commands as a laptop.

## What forge does

forge has already cross-compiled the Go binaries for the target platform
(`GOOS=linux GOARCH=<arch>`, `-trimpath`). Instead of running the Dockerfile's
builder stage in a daemon, it:

1. pulls the final stage's `FROM` image for the target platform, from its
   registry, with the credentials `docker login` (or forge's own platform
   login) stored;
2. appends one layer per `COPY` (the binaries, `db/migrations`, ...);
3. applies the stage's `ENTRYPOINT`, `CMD`, `USER`, `WORKDIR`, `ENV`, `LABEL`,
   `EXPOSE`, `STOPSIGNAL` and `HEALTHCHECK`;
4. pushes to every reference the build pushes (`:latest`, `:<tag>`).

The pushed manifest's digest is what the build state, the release ledger and
`forge env deploy` record and pin. No `docker`, `buildx` or daemon is involved
in the build, the push or the digest read.

## Choosing the builder

```
forge env deploy prod                              # auto
forge env deploy prod --image-builder docker       # always docker build
forge env deploy prod --image-builder in-process   # never docker
```

`auto` (the default) keeps `docker build` whenever a daemon answers, and
assembles in-process only when none does. `in-process` forces the daemonless
path even where Docker is available, which is how to check a Dockerfile still
qualifies before a CI run that has no daemon.

## What qualifies

The final stage (`--target production` equivalent: the last stage) may contain:
`ARG`, `ENV`, `LABEL`, `WORKDIR`, `USER`, `EXPOSE`, `STOPSIGNAL`,
`ENTRYPOINT`, `CMD`, `HEALTHCHECK`, `COPY`, and `RUN apk add ca-certificates`.

- `COPY --from=<stage> <path> <dest>` must name a binary forge's host build
  produced (`/app/bin/<cmd dir>` or the workload's `output_name`). A path forge
  did not build is refused.
- `COPY <src> <dest>` reads the project context, honouring `.dockerignore`.
- `RUN apk add ca-certificates` is not executed; it is checked against the base
  image, which must already hold `/etc/ssl/certs/ca-certificates.crt` (alpine,
  distroless and debian bases do).

Anything else is refused with the Dockerfile line and never skipped: a `RUN`
that executes code, `ADD`, heredocs, `COPY --chown`, `--mount`, `ONBUILD`,
`SHELL`, `VOLUME`, `COPY --from=<image>`. Skipping one would ship an image that
differs from the `docker build` one with nobody told.

## When it cannot build the image

The error names the first offending line and every way out:

- run where a Docker daemon is reachable (start Docker, or set `DOCKER_HOST`);
- build it remotely with `build = forge.RemoteBuild`: declared in the schema,
  but this forge cannot submit one yet;
- bring the final stage back to the scaffold's shape: move the `RUN` steps into
  the builder stage or a base image.

`DockerBuild` workloads and frontend images are built only by `docker build`.
Without a daemon they fail with the same two options instead of docker's own
connection error.

## Differences from a docker-built image

Same entrypoint, cmd, user, workdir, env, exposed ports, healthcheck, platform
and binary bytes. Different by construction:

- the config history has one empty-layer entry per config instruction and no
  `buildkit.dockerfile.v0` comment;
- `RUN apk add ca-certificates` adds no layer, so the image lacks the package's
  per-certificate files and `update-ca-certificates`. The CA bundle that Go,
  busybox `wget` and OpenSSL read is the base's own;
- the manifest is OCI, with the base image's media types.

An in-process image exists only in a registry: there is no local daemon to load
it into, so it requires a push (`forge env deploy` always pushes).

## Reproducible digests

Layers are sorted, root-owned, with every timestamp the build's
`SOURCE_DATE_EPOCH` (the last commit touching the Dockerfile and context, else
the environment's `SOURCE_DATE_EPOCH`, else the Unix epoch), and the config
carries no wall-clock time. The same binary and base give the same digest.
