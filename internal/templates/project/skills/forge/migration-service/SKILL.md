---
name: migration-service
description: Migrate a server-shaped project to forge — services, operators, workers, webhooks, packs, multi-binary cmd/, k8s manifests.
---

# Migrate a Server-Shaped Project

Use this skill when the existing project is network-facing: HTTP/gRPC servers, background workers, webhook receivers, k8s deployments. For CLI / library shapes see `migration-cli`. For prerequisites and the overall flow see `migration`.

## Scaffold

```bash
forge project new <name>-next \
  --kind service \
  --mod github.com/<owner>/<name>-next \
  --service <main-service> \
  --frontend <main-fe>
```

`--kind service` is the default; emitting it documents intent. Pass `--service`/`--frontend` for the primary network-facing components; add the rest one at a time after scaffold so each is verified in isolation.

## Add components, one at a time

```bash
forge scaffold service <name>      # additional Connect-RPC services
forge scaffold operator <name>     # k8s operator / controller loop
forge scaffold worker <name>       # background worker (Start/Stop lifecycle)
forge scaffold worker <name> --kind cron --schedule "..."   # cron worker
forge scaffold webhook <name> --service <existing-service>  # webhook on an existing service
```

Run `forge generate && go build ./...` after each. Hyphens are OK: forge stores the hyphenated form as the display name and snake-cases the directory and Go package paths.

## Cross-cutting concerns are code + libraries, not packs

Auth, audit logging, API keys, and frontend components are owned scaffold you edit or thin code over a `forge/pkg/*` library — there is nothing to `install`. The parent **`migration`** skill covers each (which file owns it, which library backs it, how the interceptors compose) and applies unchanged here.

One rule is specific to server-shaped projects:

**Forge deliberately does NOT ship a NATS / Kafka / generic-queue integration, nor a bundled Stripe / Twilio client.** Wire-format conventions (subject naming, message envelopes, retry/DLQ shape) and third-party SDK surfaces are too project-specific. Install the SDK directly (`github.com/nats-io/nats.go`, `github.com/stripe/stripe-go`, `github.com/segmentio/kafka-go`, RabbitMQ, ...) and write a thin wrapper under `internal/<name>/` with a `contract.go` exposing your actual surface. See `adapter` for the wrapper shape.

## Known migration landmines

All surfaced in real migrations — spot the symptom, apply the fix:

- **audit: two interceptors.** The `forge/pkg/audit`-backed DB interceptor and the scaffold's slog-only `middleware.AuditInterceptor` record the same events — wire ONE in `internal/app/compose.go` (`NewComponents`), never both.
- **webhook proto package.** A webhook's proto generates as `db.v1`, NOT `<project>.db.v1`. Proto package names align with the buf module root, not the project name. Templates that try to prefix the project name will lint-fail.
- **Multiple webhooks on the same service** once redeclared shared symbols (`webhookMaxBodySize`, `webhookEvent`, `extractEventID`, `verifyHMACSHA256`); fixed — templates now emit unique names per webhook.

## Green baseline before porting business logic

```bash
forge generate
go mod tidy
go build ./...
forge lint
```

All four must pass on the empty scaffold. Fix failures here, not after porting.

## Set the contracts floor

Before porting any internal package, edit `forge.yaml`:

```yaml
contracts:
  strict: true
  allow_exported_vars: false
  allow_exported_funcs: false
  exclude: []
```

`forge lint --contract` then joins the per-phase gate. See `contracts` for the per-package pattern (interface in `contract.go` first, `forge generate`, then implementation behind it).

## Porting order

1. **Internal utility packages first** (domain types, naming, validation helpers) — fewest deps on the rest of the codebase.
2. **Database layer** (`db/migrations/` plus any hand-written query files). Migrations are the schema source of truth — copy them as-is; don't port the source repo's generated ORM or entity types. `forge generate` shadow-applies them and projects the entity structs/ORM into `internal/db/<entity>_orm.go` for every table that also has CRUD RPCs in a service proto. Write plain postgres DDL (see the `db` skill) — the shadow applies migrations verbatim to a real ephemeral postgres; auxiliary DDL the bare DB can't satisfy is skipped.
3. **Services** (`internal/handlers/<svc>/`). One co-located directory per service: hand-written `contract.go` + impl alongside the generated `handlers_crud_ops_gen.go` CRUD projection. The `*_gen.go` files are rewritten on every `forge generate`; only your hand-written code (the `contract.go` interface, its implementation, domain types) moves over. There is no separate top-level `handlers/<svc>/` tier — collapse any source split into the one `internal/handlers/<svc>/`.
4. **Composition** (`internal/app/compose.go` `NewComponents`, off the owned `internal/app/providers.go` `Infra`/`OpenInfra`). `NewComponents(infra *Infra) (*Components, error)` constructs every component inline in type-topological order and fills each component's interface-typed `Deps` off `infra.<Field>`, resolved BY TYPE. `providers.go` is yours, scaffolded once; `compose.go` is forge-owned and regenerated (`forge project disown internal/app/compose.go` to hand-own — e.g. for late-bound/two-phase construct-then-inject setters). Port wiring intent by declaring each collaborator on `Infra` and filling the `Deps` fields in `NewComponents`; there is no `bootstrap.go` / `wire_gen.go` name-matched layer to wire things for you.
5. **Workers, operators, webhooks** — under `internal/workers/<name>/` and `internal/operators/<name>/` (webhooks attach to their service under `internal/handlers/<svc>/`); implement the lifecycle methods (`Start`, `Stop`, `Reconcile`, webhook event handlers). Constructed in `NewComponents`, surfaced through the generated `WorkerList`/`OperatorList` over `*Components` in `internal/app/lifecycle.go`.

## Port-time design decisions you should NOT defer

A 1-for-1 port is the goal, but some source patterns are smells forge's defaults surface. Fixing them at port time is cheap; later it is a refactor across every caller.

### Wide repository facades — split, don't exclude

A single wide `Repository` (a "god DAO") trips `interfacebloat`. The lint is correct; do NOT path-exclude it. Two clean options:

- **Drop the wide interface entirely.** Codebases with a 100+ method `Repository` usually have narrow per-aggregate interfaces (`UserRepository`, `OrgRepository`, `BillingRepository`) beside it; the concrete `*PostgresRepository` satisfies all of them structurally. Each caller depends on the narrow interface it needs, so no individual interface is over the limit.
- **Split the package.** `internal/db/user/`, `internal/db/org/`, `internal/db/billing/`, each with its own `contract.go` + narrow `Service`. Larger refactor; cleaner long-term.

**Exception: sqlc-generated code.** sqlc emits one method per query into a single `Queries` struct and that output can't be split across packages. If your source uses sqlc (`forge.yaml: sqlc_enabled: true`, or a `sqlc.yaml`), `interfacebloat` is a false positive: add a path-based exclusion for the generated dir AND document it ("generated by sqlc; cannot split"). For hand-written DAOs, split.

**No `Service` alias needed when you split.** No name-matched generator assumes every package exposes a `Service`. The composition resolves deps by type, not by name: a consumer that needs `audit.Repository` is filled from whatever concrete on the `Infra` provider set structurally satisfies it (`*db.PostgresRepository` satisfies `audit.Repository` or the code does not compile). Drop the umbrella `type Service interface { ... }` — it only ever existed to feed the deleted generators.

### Adding a dep is a compile-time edit to the composition

Adding or removing a collaborator means editing the component's `Deps` struct in `internal/handlers/<svc>/contract.go` and ensuring the matching field exists on the owned `Infra` provider set so `NewComponents` can fill it by type. Both are caught by the Go compiler (or surface as a `forge generate` "no provider" report if `Infra` lacks the type). If a port phase drops a vestigial `Logger` field from `<pkg>.Deps`, regen stops filling it and `go build` points straight at any stale reference.

### Goose → golang-migrate

If the source uses goose (one-file migrations with `-- +goose Up` / `-- +goose Down` markers), forge expects golang-migrate (two-file `.up.sql` + `.down.sql`). The conversion is mechanical:

1. Split each file at the `-- +goose Down` line into two files.
2. Drop `-- +goose StatementBegin` / `-- +goose StatementEnd` markers (they wrap single statements; golang-migrate handles that natively).
3. Files declaring `-- +goose NO TRANSACTION` translate to a golang-migrate `x-no-tx-wrap` header on that file.
4. Renumber files starting from the next-available index AFTER any pack-installed migrations (e.g. audit-log occupies 00002, api-key occupies 00003, so source migrations start at 00004).

If source migrations have foreign-key dependencies on tables that pack migrations create (or vice versa), reorder carefully. Pack migrations are not negotiable; renumber yours.

## Test regressions during port — fix the port, never blame the source

If a test passes in the source repo but fails in the port, **it is always a port bug**. Never write "pre-existing in source" in a synthesis or friction note without first running `go test ./<same-package>/...` against the source tree to verify.

Before declaring a migration complete, the synthesizing agent MUST:

1. Run `go test -count=1 ./...` in the ported tree, capture the failing test names.
2. For each failing test, run the equivalent in the source tree (paths usually differ by `internal/service/<x>/` vs `internal/<x>/`).
3. If source passes and the port fails → port bug. Fix or revert that package; never ship a report with red tests.
4. If both fail → can be flagged as inherited, but only with the source-test exit code captured in the synthesis output.

## Lint failures during port — fix the code or `//nolint:`, never path-exclude

When `forge lint` fires on a freshly-ported package, **do NOT** add `internal/<pkg>/` to a path-based exclusion list in `.golangci.yml` — that silences every linter on the package, including the bug-catchers (errcheck, govet, staticcheck, unused).

In order of preference:

1. **Fix the code.** Most `gocognit` / `funlen` / `nestif` flags point at a function that benefits from being split; `interfacebloat` points at a god-interface (see above). Take the small refactor.
2. **`//nolint:gocognit // ported as-is from <source-path>; rewriting risks behavior drift` at the function declaration.** Per-line, with a justification comment.
3. **Path exclusion as a LAST resort, on generated code only.** Things like `gen/`, `internal/<pkg>/embed.go`, or a sqlc output dir. Never on hand-written code.

A clean port lands with at most a handful of `//nolint:` annotations, not a growing list of yaml path exclusions.

## Multi-binary `cmd/` layouts

`cmd/` is entrypoints only: one cobra root (`cmd/main.go`) plus one per-command subcommand file (`cmd/<svc>.go`, `cmd/<binary>.go`), each driving the serve path (`app.OpenInfra` → `app.NewComponents` → the generated `(*Components).Mount<Svc>` onto `serverkit.Server` → `serverkit.Run`). `forge scaffold service` adds the subcommand + its `internal/handlers/<svc>/`. For source binaries that aren't first-class forge components:

- Wrapping a forge-managed service or worker: prefer `forge scaffold` and let forge own the wiring.
- Genuinely standalone (proxies, sidecars, off-service consumers) needing their own Deployment: `forge scaffold binary <name>` — a peer subcommand with its own composition (`OpenInfra` → `NewComponents`). See the `binaries` skill.
- Tiny one-off scripts that don't deserve a contract.go (dev seed, one-shot migration helper): a thin `cmd/<name>.go` subcommand on the root, no `internal/<name>/` package.

## k8s manifests

Forge emits `deploy/kcl/<env>/` (KCL-based manifests, one dir per environment: `dev`, `staging`, `prod`). KCL is canonical — there is no "ship hand-written YAML instead" mode. Either:

- **Adopt KCL** (recommended). Translate hand-written manifest customizations into KCL overrides; use `additional_manifests = [...]` on the Bundle for raw manifest dicts that don't fit a typed entity (ClusterIssuers, SealedSecrets, hand-typed CRDs).
- **Disable the deploy feature.** Set `features.deploy: false` in `forge.yaml` and bring your own manifests. `forge env deploy <env>` and the deploy half of `forge generate` then short-circuit with "feature 'deploy' is disabled".

`forge.yaml` stays strictly top-level (project identity, features, deploy provider). Per-env config (logging, env vars) lives in `deploy/kcl/<env>/` alongside the per-env deploy knobs (cluster/namespace/registry/domain) on `forge.K8sCluster` blocks — there is no second `config.<env>.yaml` format. The single typed config struct (`internal/config`, generated from proto config blocks) serves server, CLI, and standalone binaries via cmdkit; non-server binaries do NOT hand-roll `os.Getenv`/ad-hoc loggers/hardcoded timeouts.

## Final checks

```bash
forge generate          # idempotent on a healthy project
forge lint              # contract + db + general lints
forge build             # binaries + frontends + Docker images
task test              # unit + integration
task test:e2e          # full-stack (requires `forge env up dev` in another shell)
forge env deploy dev        # local k3d
```

## Rules

- One service per proto package.
- Add a component before the code that extends it (e.g. the service before its webhook receiver), then `forge generate`. Never the reverse.
- KCL or hand-rolled manifests, not both.

## When this skill is not enough

- **CLI / library shape** — see `migration-cli`.
- **Contract surface** for ported internal packages — see `contracts`.
- **Auth / audit / third-party integrations** — see `auth` (auth + API keys) and `adapter` (thin wrappers over SDKs like Stripe/Twilio/NATS).
