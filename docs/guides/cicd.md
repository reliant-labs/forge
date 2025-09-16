# CI/CD Pipelines

Every Forge project includes GitHub Actions workflows that cover linting, testing, building, vulnerability scanning, and deployment. This guide explains what each workflow does, how they fit together, and how to customize them for your needs.

## Generated Workflows Overview

When you create a project with `forge new`, three workflow files are generated in `.github/workflows/`:

| Workflow | Trigger | Purpose |
|----------|---------|---------|
| `ci.yml` | Push to main, PRs | Lint, test, build, verify generated code, vulnerability scan |
| `build-images.yml` | Push to main | Build Docker images for all services, push to registry, Trivy scan |
| `deploy.yml` | After image build, version tags, manual dispatch | Deploy to staging (auto) or production (manual) |

Additionally, the project gets:
- `.github/dependabot.yml` -- Automated dependency update PRs
- `.github/pull_request_template.md` -- PR template
- `.github/CODEOWNERS` -- Code ownership rules

## ci.yml: The Main Pipeline

The CI workflow runs on every push to `main` and every pull request. It has six jobs that execute in a dependency chain:

```
lint → test → build
              ├── docker-build
              ├── verify-generated
              └── vuln-scan
```

### Lint

The lint job runs three sets of linters:

1. **golangci-lint** -- Runs the standard Go linting suite using the project's `.golangci.yml` configuration.
2. **buf lint** -- Validates proto files against the buf configuration, checking naming conventions, field numbering, and proto best practices.
3. **TypeScript linters** (conditional) -- If `frontends/*/package.json` exists, runs `npm run lint` and `npm run typecheck` for each Next.js frontend.

### Test

The test job runs after lint passes:

1. **Go tests** -- `go test -race -coverprofile=coverage.out ./...` runs all Go tests with the race detector enabled. Coverage output is uploaded as an artifact.
2. **Frontend tests** (conditional) -- If Next.js frontends exist, runs `npm test` for each frontend.

### Build

The build job verifies that all code compiles:

1. **Go build** -- `go build ./...` compiles all Go packages without producing binaries.
2. **App builds** (conditional) -- `npm run build` for each Next.js app, verifying that the production build succeeds.

### Docker Build

Runs in parallel with verify-generated and vuln-scan, after the build job passes. It builds the Docker image from the single `deploy/Dockerfile` tagged with `:ci`. This verifies that the Docker image builds successfully without pushing it to a registry.

### Verify Generated Code

This job catches a common mistake: changing a proto file without running `forge generate`. It:

1. Runs `buf generate` and `go generate ./...`
2. Checks `git diff --exit-code`
3. Fails with an error message if there are uncommitted changes to generated files

This ensures the generated code in `gen/` always matches the proto sources in the repository.

### Vulnerability Scan

Two vulnerability checks run in parallel:

1. **govulncheck** -- Scans Go dependencies against the Go vulnerability database.
2. **npm audit** (conditional) -- Runs `npm audit --audit-level=high` for each Next.js app. This currently runs with `|| true` to avoid failing on advisory-only issues -- tighten this for stricter security posture.

## build-images.yml: Docker Build Matrix

This workflow triggers on pushes to `main` and handles building production Docker images. It uses a dynamic matrix strategy that discovers services automatically.

### Build and Push

The workflow builds the single Docker image from `deploy/Dockerfile`:

1. Logs into the container registry using `secrets.REGISTRY_PASSWORD`
2. Uses Docker Buildx for efficient layer caching via GitHub Actions cache
3. Tags images with `sha-<short-sha>` format
4. Pushes to the configured registry

### Trivy Scanning

After the image is built and pushed, [Trivy](https://trivy.dev/) scans it for CRITICAL and HIGH severity vulnerabilities. The scan fails the job if any are found, preventing vulnerable images from being deployed.

## deploy.yml: Promotion Flow

The deploy workflow supports three trigger patterns:

### Staging Auto-Deploy

When `build-images.yml` completes successfully on `main`, the staging deployment runs automatically:

1. Determines the image tag from the triggering commit SHA
2. Installs the KCL CLI
3. Runs `kcl run deploy/kcl/staging/main.k -D image_tag=<tag>` and pipes to `kubectl apply`
4. Waits for all deployments to roll out (`kubectl rollout status`)
5. Verifies health endpoints by running a curl pod in-cluster

This means every merge to `main` that passes CI is automatically deployed to staging.

### Production via Version Tag

Push a version tag to trigger production deployment:

```bash
git tag v1.2.0
git push origin v1.2.0
```

This triggers a two-step process:

1. **Retag** -- The `retag-release` job uses [crane](https://github.com/google/go-containerregistry/blob/main/cmd/crane/README.md) to tag the existing SHA-based images with the version tag. No rebuild is needed -- the same image that was tested in staging gets the version tag.
2. **Deploy** -- The `deploy-prod` job runs KCL against `deploy/kcl/prod/main.k` with the version tag, applies manifests, waits for rollouts, and verifies health endpoints.

The `prod` environment in GitHub requires manual approval, so a team member must approve the deployment in the GitHub UI before it proceeds.

### Manual Dispatch

For ad-hoc deployments or rollbacks, use the workflow dispatch:

1. Go to Actions > Deploy > Run workflow
2. Select the target environment (staging or prod)
3. Enter the image tag (e.g., `sha-abc1234` or `v1.0.0`)

## Customizing Workflows

### Changing the Go Version

All workflows use the `GO_VERSION` environment variable. Update it in `ci.yml`:

```yaml
env:
  GO_VERSION: "1.25"
```

### Adding a Database to Tests

If your tests need a database, add a service container to the test job:

```yaml
test:
    runs-on: ubuntu-latest
    services:
      postgres:
        image: postgres:16
        env:
          POSTGRES_USER: test
          POSTGRES_PASSWORD: test
          POSTGRES_DB: testdb
        ports:
          - 5432:5432
        options: >-
          --health-cmd pg_isready
          --health-interval 10s
          --health-timeout 5s
          --health-retries 5
    steps:
      # ... existing steps ...
      - name: Go tests
        run: go test -race -coverprofile=coverage.out ./...
        env:
          DATABASE_URL: postgres://test:test@localhost:5432/testdb?sslmode=disable
```

### Changing the Container Registry

Update the `REGISTRY` env in `build-images.yml` and `deploy.yml`:

```yaml
env:
  REGISTRY: "ghcr.io/your-org"
```

And configure the `REGISTRY_PASSWORD` secret in your GitHub repository settings. For GitHub Container Registry, use a Personal Access Token with `write:packages` scope.

### Adding E2E Tests

Add an e2e job that runs after the docker-build job in `ci.yml`:

```yaml
e2e:
    name: E2E Tests
    needs: docker-build
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: ${{ env.GO_VERSION }}
      - name: Start services
        run: docker compose -f deploy/docker-compose.yml up -d
      - name: Run e2e tests
        run: go test -v ./e2e/...
      - name: Cleanup
        if: always()
        run: docker compose -f deploy/docker-compose.yml down
```

### Tightening Security

To make npm audit a hard failure:

```yaml
- name: NPM audit
  run: |
    for dir in frontends/*/; do
      if [ -f "$dir/package.json" ]; then
        (cd "$dir" && npm ci && npm audit --audit-level=high)
      fi
    done
```

Remove the `|| true` that the default template includes.

## Secrets Required

| Secret | Used by | Description |
|--------|---------|-------------|
| `REGISTRY_PASSWORD` | `build-images.yml`, `deploy.yml` | Container registry authentication |
| kubectl credentials | `deploy.yml` | Kubernetes cluster access (configure via your cloud provider's auth action) |

## Related Guides

- [KCL Deployments](kcl.md) -- How the deploy manifests are structured
- [Getting Started](getting-started.md) -- Creating a project with CI/CD
- [CLI Reference](cli-reference.md) -- `forge build`, `forge test`, `forge deploy` flags