---
name: external-builds
description: Build a service whose source lives outside this Go module — sibling repos, third-party binaries, non-Go languages — via `build_cmd` on KCL `forge.Service`. The user owns the shell command end-to-end (build AND push); forge handles substitution + state.
---

# External builds (`build_cmd`)

`forge.Service.build_cmd` is the build-side escape hatch. When set, `forge build` runs the user's shell command via `sh -c` instead of the built-in Go-build pipeline. The user's command owns BOTH the build AND the docker push — forge never runs `docker push` afterwards.

Mirrors the deploy-side `forge.External` provider in shape: same `sh -c` execution, same `${X}` substitution, same skip-with-warn when the source path is missing on disk.

## When to use `build_cmd`

Use `build_cmd` when:

- **The source lives in a sibling repo** — daemon binaries, agent binaries, anything you cross-compile from another checkout. Set `build_cwd` to the sibling, cross-compile, copy the binary into a build context, and `docker build`.
- **The artifact is a third-party binary** — wrap the upstream release into your own image.
- **The language isn't Go** — Python, Rust, Node, etc. Forge doesn't know how to build them; let your existing toolchain do it and just hand forge an image to push.

Use `path:` (the standard service shape) when:

- The source IS in this Go module. `forge build` knows how to build a normal Go binary, render the Dockerfile, and push.
- You want forge to manage the build artifact (caching, --debug symbols, multi-arch buildx).

Build vs. deploy are orthogonal axes. A single service can use both, either, or neither:

| build side    | deploy side       | shape this matches                                                |
|---------------|-------------------|-------------------------------------------------------------------|
| `path:`       | `forge.K8sCluster`| Normal forge Go service in a cluster you control                  |
| `build_cmd`   | `forge.K8sCluster`| Sibling-repo Go binary deployed as a standard K8s Deployment (cp-forge's daemon-gateway shape) |
| `path:`       | `forge.External`  | Forge-built Go service deployed via `flyctl` / `gcloud run` / etc.|
| `build_cmd`   | `forge.External`  | Sibling-repo binary deployed via a cloud CLI                      |

## Canonical pattern

```kcl
forge.Service {
    name = "daemon-gateway"
    image = "reliant-daemon-gateway"
    build_cmd = r"""
        cd ${PROJECT_DIR}/../reliant
        GOOS=linux GOARCH=${TARGETARCH} CGO_ENABLED=0 \
            go build -buildvcs=false \
            -o ${PROJECT_DIR}/.build/bin/reliant-linux-${TARGETARCH} \
            ./cmd/reliant
        mkdir -p ${PROJECT_DIR}/.build/gw/bin
        cp ${PROJECT_DIR}/.build/bin/reliant-linux-${TARGETARCH} ${PROJECT_DIR}/.build/gw/bin/
        cp ${PROJECT_DIR}/docker/Dockerfile.gw ${PROJECT_DIR}/.build/gw/Dockerfile
        docker build --platform=linux/${TARGETARCH} \
            --build-arg TARGETARCH=${TARGETARCH} \
            -t ${REGISTRY}/${IMAGE}:${TAG} \
            ${PROJECT_DIR}/.build/gw
        docker push ${REGISTRY}/${IMAGE}:${TAG}
    """
    deploy = forge.K8sCluster {
        cluster = "k3d-mything"
        namespace = "mything-dev"
        registry = "localhost:5051"
        ports = [9443]
    }
}
```

Key points:

- The whole command runs through `sh -c` — newlines, `&&`, here-docs, redirects, anything sh can parse.
- The user composes both `docker build` AND `docker push`. Forge does not push for you.
- Use raw strings (`r"""..."""`) so backslashes and `${X}` aren't reinterpreted by KCL.

## Token reference

Mirrors the External (deploy) skill's table — same tokens on both build and deploy sides so users carry one mental model.

| Token            | Meaning                                                                          |
|------------------|----------------------------------------------------------------------------------|
| `${IMAGE}`       | service image string (from `Service.image`)                                      |
| `${TAG}`         | image tag forge resolved (`git describe`, or `--tag` override)                   |
| `${SERVICE}`     | `Service.name`                                                                   |
| `${TARGETARCH}`  | resolved deploy target arch (`amd64` / `arm64`) — for cross-compile + buildx     |
| `${REGISTRY}`    | configured docker registry (from `forge.yaml docker.registry` or push target)    |
| `${PROJECT_DIR}` | absolute project root (the directory holding forge.yaml)                         |
| `${BUILD_CWD}`   | the raw `build_cwd` you declared (use `${PROJECT_DIR}` for the absolute form)    |
| `${YOURS}`       | any key you declare in the `build_env` map                                       |

The built-in tokens win on conflict — if you set `build_env = {"IMAGE": "oops"}`, the IMAGE substitution still resolves to `Service.image`. `forge audit` warns on these collisions (`external_builds` category) so you don't get bitten silently. Rename your key to avoid the conflict.

## `build_cwd` semantics — skip-with-warn

`build_cwd` is the working directory the shell command runs from. Relative paths resolve against `PROJECT_DIR`. Absolute paths pass through.

If the resolved `build_cwd` doesn't exist on disk, `forge build` **skips the service with a warning** and continues. The service doesn't fail the build.

That contract exists for two real cases:

1. **Local dev with an optional sibling repo.** Some developers don't check out the sibling; their build proceeds (it just won't produce that image).
2. **CI without the sibling.** A CI job that only builds the main repo doesn't need to fail because the sibling isn't part of its checkout. Configure a CI job that DOES check out the sibling for the images that depend on it.

The flip side: if you actually need that build to happen, make sure your CI workflow checks out the sibling repo into the expected path **before** running `forge build`. `forge doctor` warns on a missing `build_cwd` so you spot the gap during local dev too.

## Orthogonality: build vs deploy

The build escape hatch (`build_cmd`) and the deploy escape hatch (`forge.External`'s `deploy_cmd`) are independent. They don't have to be set together; they don't conflict; they don't share state beyond the standard `${IMAGE}` / `${TAG}` / `${SERVICE}` triple.

A few worked combos:

### `build_cmd` + `forge.K8sCluster` — the cp-forge shape

Sibling-repo Go binary, deployed as a standard k8s Deployment. The user's `build_cmd` produces an image at `${REGISTRY}/${IMAGE}:${TAG}` and pushes it; `forge deploy <env>` reads the per-service build-state file (`.forge/state/build-<env>-<service>.json`) and renders a normal Deployment manifest that pulls the same tag. See the canonical pattern above.

### `build_cmd` + `forge.External` — sibling repo, exotic deploy

```kcl
forge.Service {
    name = "edge"
    image = "registry.fly.io/edge"
    build_cmd = r"""
        cd ${PROJECT_DIR}/../edge-repo
        cargo build --release --target x86_64-unknown-linux-musl
        docker build -t ${REGISTRY}/${IMAGE}:${TAG} .
        docker push ${REGISTRY}/${IMAGE}:${TAG}
    """
    deploy = forge.External {
        deploy_cmd = r"flyctl deploy --image ${IMAGE}:${TAG} --app edge-prod"
        rollback_cmd = r"flyctl deploy --image ${IMAGE}:${LAST_TAG} --app edge-prod"
    }
}
```

Rust binary in a sibling repo, deployed to Fly.io via `flyctl`.

### `path:` + `forge.K8sCluster` — the standard shape

Don't reach for `build_cmd`. Use the normal service-skeleton.

### `path:` + `forge.External` — forge builds, you deploy

Don't reach for `build_cmd`. Use the External deploy skill (`external-deploy-recipes` in the scaffolded project).

## When NOT to use `build_cmd`

- The source is in this Go module → `path:`. Forge has a build pipeline for that case; `build_cmd` is more code you'd have to maintain.
- You want forge to manage the build artifact (caching, --debug for delve, multi-arch buildx) → `path:`. `build_cmd` opts out of all of that.
- You only need to do something exotic at **deploy** time → use `forge.External` for the deploy side; keep the build side standard.

## Per-env build state

Every successful `build_cmd` run writes `.forge/state/build-<env>-<service>.json` with the resolved (image, tag, registry, pushed_at). `forge deploy <env>` reads it to pin the same tag the build produced. The file is informational and per-(env, service) so concurrent external builds of the same service across envs don't clobber each other.

`forge audit` surfaces the latest recorded build per env under the `external_builds` category, so you can see "when did dev last build the daemon-gateway image?" without grep.

## Doctor + audit surfaces

- **`forge doctor`** emits one check per `build_cmd` service:
  - warn when `build_cwd` is missing on disk (the same condition that triggers skip-with-warn at build time);
  - warn when the first token of `build_cmd` isn't on PATH (heuristic — skipped for `cd ...` and `KEY=value ...` openings);
  - info line with the substituted `build_cmd` against placeholder tokens, so you spot `${TYPOED}` before running build.
- **`forge audit`** has an `external_builds` category:
  - per-service entry with `build_cwd` resolved state, `build_env` keys, and any conflict tokens;
  - latest recorded build state per env (image / tag / pushed_at);
  - warns on missing `build_cwd` or `build_env` key collisions.

Both are observation, not enforcement. They surface the gaps so you can fix them before they bite at deploy time.
