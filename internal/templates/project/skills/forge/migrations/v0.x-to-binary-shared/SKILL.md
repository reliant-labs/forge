---
name: v0.x-to-binary-shared
description: Migrate a multi-service forge project from binary=per-service (one Application per service in KCL, one image build per service) to binary=shared (one Go binary, cobra subcommand per service, one Docker image, MultiServiceApplication for KCL deploy). Use when CI image builds dominate cycle time or when services share substantial internal libraries.
---

# Migrating to `binary: shared`

Use this skill when a multi-service forge project is paying real cost
for the per-service-image shape — typically one of:

- CI image builds dominate cycle time (N services = N `docker build`
  invocations = N image push/pulls per deploy).
- Services share substantial internal libraries (`internal/<pkg>/`)
  and you want one binary in production for the same reason you have
  one binary in local dev.
- The deploy story has grown to N near-identical Application blocks
  in `deploy/kcl/<env>/main.k` that differ only in name + replicas +
  args.

`binary: shared` collapses these to one image, one Dockerfile build,
and one `MultiServiceApplication` block in KCL. Each service is still
its own Deployment / Service / RBAC scope (so you can scale, roll, and
debug them independently); the only thing that changes is what runs
inside the container.

## When to migrate

- Multi-service projects with 3+ services that share `internal/`.
- Projects whose CI/deploy time is dominated by image builds rather
  than test/lint runs (control-plane-next sized this at 11 services
  → ~30 min of build/push, vs ~3 min for one shared image).
- Projects where the only reason services were split was operational
  isolation (separate scaling, replicas) — not because the binaries
  themselves were genuinely different.

## When NOT to migrate

- Single-service projects — there's nothing to share.
- Projects whose services genuinely have divergent runtime
  dependencies (e.g. one needs CGO + libpq, another is pure Go). One
  shared image must include the union of all services' runtime
  requirements.
- Projects that need to roll out one service independently of the
  others as a hard requirement. With `binary: shared`, all services
  ship the same image SHA — you can still roll forward per-service
  Deployments at different cadences (KCL emits N Deployments), but
  the image SHA they reference is the same. A bug fix in service A
  ships service B's code at the same time. The blast radius isn't
  bigger than the multi-binary case in practice (you'd already be
  testing the union before merging), but if your release engineering
  treats services as independent units you may not want this.

## Migration steps

The mechanical changes are small. The decision-making is mostly
"do you actually want this?".

### 1. Edit forge.yaml

Add the `binary:` field at the top level:

```yaml
name: my-project
module_path: github.com/example/my-project
kind: service
binary: shared    # new — explicit opt-in
version: 0.2.0
```

Default (the field absent) is `per-service`, so existing projects keep
their existing shape until they choose this opt-in.

### 2. Regenerate

Run `forge generate`. The pipeline rewrites:

- **`pkg/app/bootstrap.go`** — `BootstrapOnly` switches to lazy
  per-service construction. Calling `BootstrapOnly(mux, logger, cfg,
  []string{"api"})` now constructs ONLY the `api` service's
  dependency graph; the worker's, billing's, etc. are skipped. This
  is the runtime win the shared mode delivers — `./<bin> api` boots
  faster and uses less memory than `./<bin> server`.
- **`cmd/main.go`** — replaced with the shared-binary cobra root
  (cmd-shared-main.go.tmpl). Functionally identical to the canonical
  cmd-root for top-level routing; the difference is the help text and
  documentation comments.
- **`cmd/<svc>.go`** — one new file per service (`cmd/api.go`,
  `cmd/worker.go`, etc.). Each is a thin cobra subcommand that
  delegates to `runServer(cmd, []string{"<svc>"})`. The canonical
  `./<bin> server [<svc>...]` form continues to work.
- **`deploy/kcl/<env>/main.k`** — replaced with the
  `MultiServiceApplication` shape (one `image:`, N
  `SubCommandService` entries via `render.multi_service_apps(multi)`).

`forge upgrade` is the safer command for established projects: it
shows a dry-run diff first and prompts before overwriting any
checksum-protected file. `forge generate` always rewrites the Tier-1
files (cmd/main.go, cmd/server.go, etc.) and leaves Tier-2 alone.

### 3. Verify the image build

The Dockerfile is unchanged — it already builds one binary at
`/usr/local/bin/<project>`. The KCL `MultiServiceApplication.command`
field points each Deployment at this path. Check:

```
docker build -t my-project:test .
docker run --rm my-project:test --help    # should list all services
docker run --rm my-project:test api &     # should boot the api service
```

### 4. Verify deploy

```
kcl run deploy/kcl/dev/main.k > /tmp/m.yaml
grep "image:" /tmp/m.yaml | sort -u    # should be ONE distinct image
grep "name: " /tmp/m.yaml | grep "Deployment\b\|name: <project>-" | sort -u
```

You should see exactly one image reference (the shared binary) and N
Deployments named `<project>-<service>`.

### 5. Adjust CI

If your CI matrix-builds one image per service, collapse it to one
image build. The matrix is still useful for tests (so per-service
`go test ./handlers/<svc>` runs in parallel), but `docker build` only
needs to happen once per commit.

## Trade-offs (recap)

| Aspect             | per-service           | shared                |
|--------------------|----------------------|-----------------------|
| Image builds/push  | N per commit         | 1 per commit          |
| Image size         | Smaller per service  | Larger (union of deps)|
| Per-service scale  | Independent          | Independent (still N Deployments) |
| Per-service deploy | Per-service SHA      | All-services SHA      |
| Local dev          | `./<bin>-<svc>` or `./<svc>/cmd` | `./<bin> <svc>` |
| CI complexity      | Matrix per service   | Single build          |
| Debugging          | Service-scoped binary| Service-scoped subcommand |

## Reverting

To go back to `binary: per-service`:

1. Set `binary: per-service` (or remove the field) in `forge.yaml`.
2. Run `forge generate`. The bootstrap reverts to all-services
   construction; cmd/main.go reverts to the canonical cmd-root; the
   per-service `cmd/<svc>.go` files become orphans — delete them
   manually (`forge generate` does not currently sweep them — see
   FORGE_BACKLOG.md "stale cmd/ orphans on binary mode change" if
   present, otherwise this is the deletion you have to do by hand).
3. KCL `deploy/kcl/<env>/main.k` reverts to N `Application` blocks.

## Related skills

- `architecture` — binary modes overview.
- `deploy` — `MultiServiceApplication` for KCL.
- `services` — adding a new service post-migration (works the same
  way in either mode; in shared mode `forge add service <name>` also
  emits the `cmd/<name>.go` cobra subcommand).
