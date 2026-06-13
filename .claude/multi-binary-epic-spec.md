# Forge epic: multi-binary support

Branch: **feat/gateways** (currently at the component-model epic tip, 38941c9 or newer).
**No backwards compat** (forge unreleased) — delete old shapes, don't deprecate. Binary:
build `cmd/forge` → `/tmp/forge-bin/forge`.

## Why
`kind: binary` today conflates two genuinely different shapes. Grounded facts:
- `forge add binary` (internal/generator/binary_gen.go) scaffolds a cobra SUBCOMMAND
  `cmd/<name>.go` baked into the ONE app binary, and PHOTOCOPIES signal/shutdown/
  metrics-port boilerplate into every `internal/<name>/`.
- The Dockerfile (internal/templates/project/Dockerfile.tmpl) builds exactly `./cmd` → one
  binary, one image. A standalone `cmd/<name>/main.go` (cp-forge's workspace-proxy) gets NO
  image built — invisible to the whole build/deploy toolchain.
- Phase D filed fr-70df595480 (p2): components_gen.json renders `command: ["/app/proj","name"]`
  with no separate-image representation; wants an `image:`/`separate_image:` sub-shape.

## The decision (locked)
Add an **`image: shared | standalone`** attribute to `kind: binary` (default `shared`). NOT a new
kind — it's a packaging axis.
- **shared** (default, today's model): cobra subcommand `cmd/<name>.go`, built into the app image,
  Deployment runs `["/app/<proj>","<name>"]`.
- **standalone**: own `cmd/<name>/main.go` (`package main`), own compiled binary, own container
  image, own deps; Deployment runs its own image entrypoint. For reverse proxies / sidecars /
  heavy-dep or independently-scaled processes (cp-forge workspace-proxy is the validation fixture).

```yaml
- name: workspace-proxy
  kind: binary
  image: standalone        # own main, own image
  ports: { http: 8080 }
```

Three increments, hard ordering. Process rules (EVERY worktree agent): first
`git reset --hard $(git rev-parse feat/gateways)` + confirm HEAD; `cd` explicitly in every Bash;
COMMIT EARLY per coherent step; test tiers = `go test -short ./...` (~8s) inner loop, package
tests before commit, plain e2e gate `go test -tags e2e -count=1 ./internal/cli/` once at end after
committing; commits end `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.

---

## Phase 1 — Foundation (schema + scaffold + binarykit)

1. **Schema** (internal/config): binary components gain `Image string` (`shared`|`standalone`,
   default shared) and an optional `Build` sub-block (dockerfile target name, base image override,
   build args) for standalone. Validate: standalone path = `cmd/<name>/main.go`; shared path =
   `cmd/<name>.go`. Default-shared keeps every existing binary unchanged.
2. **`pkg/binarykit` inherited library** (the photocopier→library fix — THE important one): extract
   the lifecycle boilerplate the binary scaffold currently photocopies (signal handling, graceful
   shutdown, the metrics-server-on-a-separate-port, structured logging setup, config load, /healthz
   + /metrics) into an inherited `pkg/binarykit` (mirror how pkg/appkit/serverkit/authn are inherited
   libraries, NOT generated copies). A binary's `internal/<name>/` then hand-writes only business
   logic + a `Run(ctx)`; binarykit owns the process lifecycle. Ship it in the forge pkg module so it
   vendors like the others.
3. **Scaffold** `forge add binary [--standalone]`:
   - shared (default): `cmd/<name>.go` subcommand + `internal/<name>/` that USES binarykit (no more
     photocopied boilerplate).
   - standalone: `cmd/<name>/main.go` own main that does NOT call server Bootstrap — wires binarykit
     + `internal/<name>/` business logic. Own `package main`.
4. Update the binary scaffold templates + binary_gen.go; migration of existing binaries is a no-op
   (they're `image: shared`). Skills (binaries, architecture) updated.

DONE = `forge add binary x` and `forge add binary y --standalone` both scaffold + build; a fresh
project with a standalone binary builds `./cmd/y`; binarykit is inherited (not copied); tests +
e2e gate green.

## Phase 2 — Deploy half (Dockerfile + build + KCL image)

1. **Dockerfile → multi-target**: shared base stages (mod download + source copy) + the default app
   target (`./cmd`) + one `production-<name>` target per standalone binary building `./cmd/<name>`
   into its own runner image. `--target` selects.
2. **`forge build`** (internal/cli/build.go): build N images — the main app image + one per
   standalone binary — each with its own registry tag. Today it's hardwired to one.
3. **components_gen.json + KCL** (Phase-C model): add an `image` ref per component. shared binaries /
   server / worker / cron / operator → the main app image. standalone binary → its own image. The KCL
   `Binary` subtype (kcl/components/) picks main-app-image vs own-image + entrypoint. Per-env overlays
   carry each standalone image's tag/registry.

DONE = on a scaffold with a standalone binary, `forge build` produces 2 images; `kcl run
deploy/kcl/dev` renders the standalone binary's Deployment with ITS image, the shared subcommand with
the app image; idempotent.

## Phase 3 — Edges (CI + dev-loop + audit)

1. **CI** (generate_ci): the workflow gains a build/push job per image (matrix over standalone
   binaries + app), optional isolated test/vuln-scan per standalone.
2. **`forge run`** dev loop: standalone binary → `go run ./cmd/<name>` as a separate supervised child
   (K2's multi-child supervisor already runs multiple processes); shared subcommand → `./app <name>`.
3. **audit/map**: surface the image topology (which components share the app image vs ship their own)
   and CATCH the gap that's silent today — a standalone binary declared with no Dockerfile target / no
   CI job / no deploy image ref. (Sibling to kalshi's fr: audit checks main.k presence, not that
   deploy renders.)

DONE = CI builds all images; `forge run` boots a standalone binary as its own dev process; `forge
audit` reports image topology and flags a standalone-with-no-image-target.

---

## Validation fixture (Phase D follow-up, after epic lands + binary rebuilt)
cp-forge workspace-proxy: re-scaffold/migrate to `image: standalone` so the hand-rolled
cmd/workspace-proxy/main.go collapses onto binarykit; fr-70df595480 closes. (Separate small task,
not part of the forge epic.)

## Reframe to keep room for (do NOT build now)
A `shared` binary with its own Deployment == "a worker scaled as its own process." The same `image:`
axis should leave room to later promote an in-process worker to its own pod. Design binarykit + the
KCL Binary subtype so that's a natural extension, not a rewrite.
