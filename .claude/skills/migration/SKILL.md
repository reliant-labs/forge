---
name: migration
description: Migrate an existing project to forge — pre-flight, project shape, module path strategy, package porting order, common gotchas.
---

# Migrating an Existing Project to Forge

Use this skill when porting an existing Go codebase onto a forge-generated scaffold. For greenfield work see `getting-started`.

## Pre-flight

1. **No BSR (buf.build) auth required — Go or frontend.** Fresh forge projects scaffold with no `deps:` in `buf.yaml` and `local:` plugins on both halves of codegen:
   - **Go**: `local: protoc-gen-go` / `local: protoc-gen-connect-go` resolved from `$PATH`. `forge new` auto-runs `forge tools install` to put them there.
   - **TypeScript** (frontends): `local: ./frontends/<name>/node_modules/.bin/protoc-gen-es` from `@bufbuild/protoc-gen-es` pinned in the frontend `package.json`. `forge new` runs `npm install` in each frontend dir before bootstrap codegen.

   Opt back into BSR-hosted plugins with `forge new --buf-plugins=remote` if you prefer no-install (and accept rate limits) on both halves.
2. Install dev tooling forge expects on PATH but does not ship: `buf` (no BSR auth needed), `goimports`, `npm` (only if you have frontends — and frontend TS codegen requires it), `go`. `forge new` warns and continues without them but downstream `forge generate` and lint passes degrade silently.
3. Make sure your host Go version is `>=` the version forge's `pkg/orm` requires. Forge clamps `go.work` upward when it detects an older host, but a host mismatch can still surprise `go build` calls outside forge's own subprocesses.
4. PATH visibility in your shell does NOT propagate to forge subprocesses unless you `export` it. Add `export PATH=/path/to/go/bin:$PATH` to your shell init or invoke forge with the env inline. `buf generate` is run as a forge subprocess and will silently misbehave if it cannot find tools.
5. Load relevant skills upfront — they are the most current source of truth on conventions:
   ```bash
   forge skill load architecture
   forge skill load services
   forge skill load packs
   forge skill load contracts
   ```

## Choose the project shape at scaffold time

```bash
forge new <name>-next --kind <service|cli|library> --mod <module-path>
```

| Kind | Use when |
|------|----------|
| `service` (default) | Network-facing app: Connect-RPC server, middleware stack, observability, k8s deploy. See `migration/service`. |
| `cli` | Cobra-based CLI binary. No `pkg/middleware/`, no `cmd/server.go`, no `deploy/`, no service protos. See `migration/cli`. |
| `library` | Pure Go module with `internal/` and `pkg/` skeletons; no `cmd/` at all. For shared libraries. |

**Pick this at scaffold time.** `--disable` flags only toggle `forge.yaml` features; they do NOT prevent server-shaped files from being emitted. If you scaffold with the wrong kind, wipe and start over rather than pruning by hand.

## Module path strategy

Use a `-next` suffix (e.g. `github.com/owner/project-next`) during the migration so old and new repos build side-by-side. Rename the module path as the final cutover step. The friction is small and the safety upside is large — never try to migrate in-place.

## Service / package naming

- Hyphens are OK in service / worker / operator names (`forge add service admin-server`). Forge stores the hyphenated form as the display name in `forge.yaml` and snake-cases the form used for Go package decls, directory paths (`handlers/admin_server/`), and proto packages.
- `forge.yaml` `path:` is always the snake form. Don't hand-edit it to use hyphens.
- The proto package generated for a pack like `stripe` is `db.v1` (not `<project>.db.v1`) — package names align with directory layout under the buf module root, not the project name.

## Recommended migration order

Same shape for both `service` and `cli` targets:

1. `forge new <name>-next --kind <kind> --mod <path>` — scaffold the empty project.
2. Add components (`forge add operator/service/worker/webhook`) and packs (`forge pack install <pack>`) one at a time. `forge generate` after each.
3. Get a green baseline before porting any business logic: `forge generate && go mod tidy && go build ./... && forge lint`. All four must pass.
4. **Set the contracts floor before porting.** Edit `forge.yaml`:
   ```yaml
   contracts:
     strict: true
     allow_exported_vars: false
     allow_exported_funcs: false
     exclude: []
   ```
   See the `contracts` skill. Enabling this upfront prevents a 5-hour backfill at the end.
5. Port internal packages first (utility code, domain types). Then handlers. Then wiring (`pkg/app/setup.go` is yours; `bootstrap.go` is generated and re-emitted on every `forge generate`).
6. Add `forge lint --contract` to the gate after every port phase. If a port leaves a package without `contract.go`, lint fails — fix before moving on.

## Pack interactions

- Install packs **after** the components they extend. Stripe webhook handler? Add the service first, then `forge pack install stripe`.
- Each pack installs its code into a nested per-pack subpackage chosen by the pack's `subpath:` field. Auth providers nest under `pkg/middleware/auth/<provider>/` (e.g. `pkg/middleware/auth/jwtauth/`, `pkg/middleware/auth/clerk/`, `pkg/middleware/auth/apikey/`); the audit pack lives at `pkg/middleware/audit/auditlog/`; external-service clients (e.g. `stripe`, `twilio`) live at `pkg/clients/<service>/`. `forge pack list` shows the SUBPATH column. Compose interceptors in `pkg/app/setup.go` with `connect.WithInterceptors(jwtauth.Interceptor(), clerkauth.Interceptor(), ...)` — Forge ships no chain helper.
- `forge generate` after every `forge pack install` to wire pack contributions through the codegen pipeline.

## Per-package porting recipe (source-copy approach)

For each existing package you want under the new tree:

```bash
cp -r /path/to/source/internal/<pkg> /path/to/<name>-next/internal/
find /path/to/<name>-next/internal/<pkg> -name '*.go' \
  -exec sed -i 's|<old-module-path>|<new-module-path>|g' {} +
cd /path/to/<name>-next && go mod tidy && go build ./... && go test ./internal/<pkg>/...
```

For templates / packs / manifests, restrict the rewrite to the runtime-import path (`pkg/...`) only. Doc-strings and `go install ...@version` references that point at the canonical tool MUST be left alone:

```bash
find /path/to/<name>-next/internal/<pkg> -type f \
  \( -name '*.tmpl' -o -name '*.go' -o -name 'pack.yaml' \) \
  -exec sed -i 's|<old>/pkg|<new>/pkg|g' {} +
grep -rn '<old-module-path>[^-]' /path/to/<name>-next/internal/<pkg>
```

Triage every grep hit. README/`go install` references should remain canonical; runtime imports get rewritten.

## Common gotchas

- **Pre-port grep `^\s*"<old-module>/` in the source-side package** to enumerate every internal import path before copying. The per-internal counts tell you which other packages are needed and surface non-`internal/` deps (repo-root packages, `cli/` public-embed) that are easy to miss.
- **Pin transitive deps the source repo pins.** Before `go mod tidy`, copy version pins for any dep with a fast-moving API (Delve, gRPC, k8s libs, cobra) into the target `go.mod`. A blind `tidy` resolves to latest-compatible and silently breaks API skew.
- **`//go:embed` of in-repo assets.** Use `cp -r` (preserves dotfiles like `.dockerignore.tmpl`) and verify any string constants that match against embedded content still align after the import-path rewrite. Without that check, embed mismatches no-op silently and downstream scaffolds break at runtime.
- **Generated proto descriptors and sed do not mix.** A blanket `sed s|forge|forge-next|` rewrites the `go_package` string inside `*.pb.go` rawDesc bytes but does NOT update the varint length prefix → runtime panic in `protobuf/internal/filedesc.unmarshalSeedOptions`. Regenerate via `buf generate` instead of sed-rewriting compiled descriptors.
- **Manifest files (`pack.yaml`, etc.) are a third file class** alongside `.go` and templates. Pack manifests have a top-level `dependencies:` list of Go module paths. Include manifests in the rewrite glob.

## Halt-and-report rule on forge bugs

If you hit a forge issue mid-migration, **halt the migration**, file/fix the forge bug, then resume. Don't paper over — friction is exactly what dogfooding is for. Forge improvements take priority over completing the migration on schedule.

## Tips for delegating port work to sub-agents

- Tell them which forge skills to load before starting (`architecture`, `services`, `contracts`, `migration` plus this skill's children).
- Tell them which fixes are already in (so they don't relitigate).
- Be explicit about whether to use `forge package new` or copy source directly.
- Specify env: `export PATH=/path/to/go/bin:$PATH`.
- Halt-and-report rule on new forge bugs (don't fix forge themselves — bring back to the orchestrator).

## Sub-skills

- `migration/service` — server-shaped projects (services, operators, workers, webhooks, packs, k8s)
- `migration/cli` — CLI / library projects (`cmd/`, second binaries, when contract.go isn't worth it)

For contract design — applies during migrations and to greenfield code alike — see the top-level `contracts` skill.
