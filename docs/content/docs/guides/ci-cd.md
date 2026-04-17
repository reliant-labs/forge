---
title: "CI/CD Guide"
description: "Generated GitHub Actions workflows, image promotion, and local development with k3d"
weight: 60
---

# CI/CD Guide

When you create a project with `forge new`, three GitHub Actions workflows are generated in `.github/workflows/`. Together they implement a pipeline where code is linted and tested on every push, container images are built and scanned on merge to main, and deployments happen automatically to staging and manually (or via tag) to production.

## The Three Workflows

### 1. CI (`ci.yml`)

Runs on every push and pull request against `main`. This is the quality gate — nothing merges without passing these checks.

**Jobs in order:**

| Job | What it does |
|-----|--------------|
| **lint** | Runs `golangci-lint`, `buf lint`, and `npm run lint` for any Next.js apps |
| **test** | Runs `go test -race -count=1 ./...` and `npx vitest run` for apps |
| **build** | Runs `go build ./...` and `npm run build` for apps (depends on lint + test) |
| **docker-build** | Builds Docker images for all services and apps (depends on build) |
| **verify-generated** | Runs `forge generate` and checks for uncommitted changes |
| **vuln-scan** | Runs `govulncheck ./...` and `npm audit` for apps (depends on lint + test) |

The `verify-generated` job catches a common mistake: editing proto files but forgetting to run `forge generate` before committing. If the generated code is stale, the job fails with a clear error message.

### 2. Build & Push Images (`build-images.yml`)

Runs on push to `main` (after CI passes). This workflow builds production Docker images and pushes them to the container registry.

The workflow dynamically discovers all services and apps with Dockerfiles using a matrix strategy, so adding a new service doesn't require editing the workflow.

**What happens:**

1. **Discover** — finds all `Dockerfile`s under `services/` and `frontends/`
2. **Build & push** — for each service, builds with Docker Buildx (with GitHub Actions cache), pushes to the configured registry, and tags with `sha-<short-commit-hash>`
3. **Scan** — runs Trivy vulnerability scanner against each pushed image, failing on CRITICAL or HIGH severity

Images are tagged with the git SHA prefix (`sha-abc1234`), which is the canonical identifier for a build. This tag is used for promotion to staging and production.

### 3. Deploy (`deploy.yml`)

Handles deployment to staging and production environments using KCL manifests.

**Trigger conditions:**

| Trigger | Target | Image Tag |
|---------|--------|-----------|
| Successful `build-images` workflow run | Staging | `sha-<commit>` from the build |
| Push of a `v*` tag (e.g., `v1.2.0`) | Production | The version tag |
| Manual `workflow_dispatch` | Staging or Production | User-specified tag |

**Staging deployment** happens automatically after every successful image build on `main`. This means every merge to main reaches staging within minutes.

**Production deployment** happens when you push a version tag. The workflow retags the SHA-based image with the version tag and `latest`, then deploys using the version tag. No rebuild — the same image that was tested in staging is promoted to production.

**Manual deployment** is available via the GitHub Actions UI. You pick the environment (staging or prod) and specify the exact image tag to deploy.

## The Promotion Flow

The image promotion flow avoids rebuilding images between environments:

```
PR merged to main
    │
    ▼
CI passes (lint, test, build, vuln-scan)
    │
    ▼
Build & Push: images tagged sha-abc1234
    │
    ▼
Auto-deploy to staging with sha-abc1234
    │
    ▼
Manual testing / QA in staging
    │
    ▼
Push tag v1.2.0
    │
    ▼
Retag: sha-abc1234 → v1.2.0, latest (using crane, no rebuild)
    │
    ▼
Deploy to production with v1.2.0
```

The key property is that the exact binary that was tested in CI and validated in staging is the same binary that runs in production. The `retag-release` job uses [crane](https://github.com/google/go-containerregistry/tree/main/cmd/crane) to add tags to existing images without pulling or pushing layers.

## Deployment Mechanics

All three deploy jobs follow the same pattern:

1. Install KCL
2. Render manifests: `kcl run deploy/kcl/<env>/main.k -D image_tag=<tag>`
3. Apply: `kubectl apply --server-side -f -`
4. Wait for rollouts: `kubectl rollout status deployment/<name> --timeout=300s`
5. Verify health: `curl -sf http://<service>/healthz`

The KCL step is what makes environments configurable without workflow changes. Each environment's `main.k` defines its own replica counts, resource limits, ingress rules, and environment variables. The `image_tag` is the only parameter that changes between deploys.

## Customizing Pipelines

### Adding a Job

To add a new job (e.g., running E2E tests before staging deploy), edit `.github/workflows/ci.yml`:

```yaml
  e2e:
    name: E2E Tests
    needs: [build]
    runs-on: ubuntu-latest
    services:
      postgres:
        image: postgres:16
        env:
          POSTGRES_PASSWORD: test
        ports:
          - 5432:5432
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: ${{ env.GO_VERSION }}
      - name: Run E2E tests
        run: go test -v ./e2e/...
        env:
          DATABASE_URL: postgres://postgres:test@localhost:5432/postgres?sslmode=disable
```

### Changing the Registry

The registry is configured in the workflow templates. Update `build-images.yml` and `deploy.yml`:

```yaml
env:
  REGISTRY: "ghcr.io/myorg"
```

Also update `forge.yaml`:

```yaml
environments:
  - name: staging
    registry: ghcr.io/myorg
  - name: prod
    registry: ghcr.io/myorg
```

### Adding Deployment Environments

To add a new environment (e.g., `qa`):

1. Create `deploy/kcl/qa/main.k` with the environment configuration
2. Add the environment to `forge.yaml`
3. Add a deploy job to `.github/workflows/deploy.yml` with the appropriate trigger

## Local Development with k3d

For local Kubernetes development, Forge uses [k3d](https://k3d.io), which runs k3s (lightweight Kubernetes) inside Docker containers.

### How `forge deploy dev` Works

When you run `forge deploy dev`, the CLI:

1. **Checks for a k3d cluster.** If none exists, it creates one using `deploy/k3d.yaml` if present, or with sensible defaults:
   ```bash
   k3d cluster create dev \
     --registry-create dev-registry:0.0.0.0:5050 \
     --servers 1 \
     --no-lb
   ```

2. **Builds Docker images** for each service with a Dockerfile.

3. **Pushes to the local registry** at `localhost:5050`. The k3d cluster is pre-configured to pull from this registry.

4. **Renders KCL manifests** for the dev environment, which references `localhost:5050` as the image registry.

5. **Applies manifests** and waits for rollouts.

### The k3d Configuration

If `deploy/k3d.yaml` exists, it is used for cluster creation. A typical config:

```yaml
apiVersion: k3d.io/v1alpha5
kind: Simple
metadata:
  name: dev
servers: 1
registries:
  create:
    name: dev-registry
    host: "0.0.0.0"
    hostPort: "5050"
options:
  k3s:
    extraArgs:
      - arg: --disable=traefik
        nodeFilters: [server:*]
      - arg: --disable=metrics-server
        nodeFilters: [server:*]
```

Traefik and the metrics-server are disabled by default for faster startup and lower resource usage in development.

### k3d vs forge run

| Feature | `forge run` | `forge deploy dev` |
|---------|---------------|----------------------|
| Where it runs | Host processes (Air, npm) | k3d Kubernetes cluster |
| Hot reload | Yes (Air watches files) | No (need to rebuild/redeploy) |
| Infrastructure | docker-compose | In-cluster or docker-compose |
| Kubernetes features | Not available | Full K8s (Services, Ingress, NetworkPolicy) |
| Use when | Developing service logic | Testing K8s manifests, networking, deployment |

Most day-to-day development uses `forge run` for fast iteration. Use `forge deploy dev` when you need to test Kubernetes-specific behavior.

## Related Topics

- **[Getting Started]({{< relref "../getting-started" >}})** — the full workflow from project creation to deployment
- **[KCL Deployment Guide]({{< relref "kcl" >}})** — how environment manifests are configured
- **[CLI Reference]({{< relref "../reference/cli" >}})** — `deploy`, `build`, and `test` command details