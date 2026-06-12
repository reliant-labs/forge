---
name: migration-service
description: Migrate a server-shaped project to forge — services, operators, workers, webhooks, packs, multi-binary cmd/, k8s manifests.
---

# Migrate a Server-Shaped Project

Use this skill when the existing project is network-facing: HTTP/gRPC servers, background workers, webhook receivers, k8s deployments. For CLI / library shapes see `migration-cli`. For prerequisites and the overall flow see `migration`.

## Scaffold

```bash
forge new <name>-next \
  --kind service \
  --mod github.com/<owner>/<name>-next \
  --service <main-service> \
  --frontend <main-fe>
```

`--kind service` is the default — emitting it explicitly documents intent. Pass `--service` and `--frontend` for the primary network-facing component(s); add the rest one at a time after scaffold so each can be verified in isolation.

## Add components, one at a time

```bash
forge add service <name>      # additional Connect-RPC services
forge add operator <name>     # k8s operator / controller loop
forge add worker <name>       # background worker (Start/Stop lifecycle)
forge add worker <name> --kind cron --schedule "..."   # cron worker
forge add webhook <name> --service <existing-service>  # webhook on an existing service
```

Run `forge generate && go build ./...` after each. Hyphens are OK in names; forge stores the hyphenated form as the display name and snake-cases the directory and Go package paths.

## Install packs after the components they extend

Order matters. A stripe webhook receiver needs the host service to exist before pack install can wire its handler:

```bash
forge add service billing
forge pack install stripe
forge generate
```

Available packs: `jwt-auth`, `clerk`, `firebase-auth`, `api-key`, `audit-log`, `data-table`, `auth-ui`. Starters (one-time copies): `stripe`, `twilio`, `clerk-webhook`. See `packs` / `starters` skills for details.

**Forge deliberately does NOT ship a NATS / Kafka / generic-queue pack.** Wire-format conventions (subject naming, message envelopes, retry/DLQ shape) are too project-specific — what works for one team's JetStream layout is wrong for the next. For NATS-using projects, install `github.com/nats-io/nats.go` directly and write a thin wrapper under `internal/<name>/` with a `contract.go` exposing your actual publish/subscribe surface. Same applies to Kafka (`github.com/segmentio/kafka-go`), RabbitMQ, etc. Use the `adapter` skill for the wrapper shape.

## Known pack landmines

These have all surfaced in real migrations. Spot the symptom, apply the fix immediately:

- **audit-log pack: nested subpackage layout.** The DB-backed interceptor is `auditlog.Interceptor` in `pkg/middleware/audit/auditlog/` (lives in its own Go subpackage so it cannot collide with the scaffold's slog-only `middleware.AuditInterceptor`). Wire one or the other in `pkg/app/setup.go`, not both — they record the same events.
- **jwt-auth pack on newer keyfunc.** Older revisions called the now-removed `keyfunc.Keyfunc.Cancel()` API. Fixed; if you see it again on a fresh fork, file the bug.
- **stripe pack proto package.** Generates as `db.v1`, NOT `<project>.db.v1`. Proto package names align with the buf module root, not the project name. Templates that try to prefix the project name will lint-fail.
- **Multiple webhooks on the same service** used to redeclare shared symbols (`webhookMaxBodySize`, `webhookEvent`, `extractEventID`, `verifyHMACSHA256`). Fixed; webhook templates now use unique names per webhook.

## Get to a green baseline before porting business logic

```bash
forge generate
go mod tidy
go build ./...
forge lint
```

All four must pass on the empty scaffold. Fix any failures here, not after porting — the failures will be much easier to read against zero ported code.

## Set the contracts floor

Before porting any internal package, edit `forge.yaml`:

```yaml
contracts:
  strict: true
  allow_exported_vars: false
  allow_exported_funcs: false
  exclude: []
```

Then `forge lint --contract` is part of the per-phase gate. See the `contracts` skill for the per-package design pattern (interface in `contract.go` first, `forge generate`, then implementation behind the interface).

## Porting order

1. **Internal utility packages first** (domain types, naming, validation helpers). These have the fewest deps on the rest of the codebase.
2. **Database layer** (`db/migrations/` plus any hand-written query files). Migrations are the schema source of truth — copy them as-is. `forge generate` shadow-applies them and projects the entity structs/ORM into `internal/db/<entity>_orm.go` for every table that also has CRUD RPCs in a service proto; don't port the source repo's generated ORM or entity types. Keep table-defining DDL in the portable pg/sqlite subset (`DEFAULT (now())`, no `::type` casts — see the `db` skill); pg-only auxiliary DDL is skipped by the shadow apply.
3. **Service handlers** (`handlers/<svc>/service.go`). The generated `*_gen.go` files (CRUD, authorizer) get rewritten on every `forge generate`; only your hand-written code moves over.
4. **Custom wiring** (`pkg/app/setup.go`). `bootstrap.go` is generated and re-emitted on every `forge generate` — do not port the source repo's bootstrap.
5. **Workers, operators, webhooks** — implement the lifecycle methods (`Start`, `Stop`, `Reconcile`, webhook event handlers).

## Port-time design decisions you should NOT defer

A 1-for-1 port is the goal, but some source patterns are smells that forge's defaults will surface. Fixing them at port time is cheap; fixing them later is a refactor across every caller.

### Wide repository facades — split, don't exclude

If the source has a single wide `Repository` interface with many methods (a "god DAO"), DO NOT path-exclude `interfacebloat` and move on. The lint is correct: the interface is too wide. At port time you have two clean options:

- **Drop the wide interface entirely.** Most source codebases that have a 100+ method `Repository` also have narrow per-aggregate interfaces (`UserRepository`, `OrgRepository`, `BillingRepository`) sitting next to it. The concrete `*PostgresRepository` satisfies all of them structurally — Go does that for free. Each caller depends on the 10-method interface it actually needs. `interfacebloat` passes naturally because no individual interface is over the limit.
- **Split the package.** `internal/db/user/`, `internal/db/org/`, `internal/db/billing/`, each with its own `contract.go` + narrow `Service`. The aggregate-per-package pattern. Larger refactor; cleaner long-term.

**Exception: sqlc-generated code.** sqlc emits one method per query into a single `Queries` struct, and you can't split that output across packages. If your source uses sqlc (check `forge.yaml: sqlc_enabled: true` or look for `sqlc.yaml`), the wide interface is a generated artifact — `interfacebloat` is a false positive against it. In that one case, add a path-based exclusion for the generated dir AND document it ("generated by sqlc; cannot split"). For hand-written DAOs, split.

**Keep a `Service` alias when you split.** Forge's `pkg/app/testing.go` and `pkg/app/bootstrap.go` generators assume every internal package exposes a `Service` interface. If you split the wide DAO into narrow per-aggregate interfaces (`UserRepository`, `OrgRepository`, etc.), declare a `type Service = Repository` alias (or `type Service interface { UserRepository; OrgRepository; ... }` umbrella) in your `contract.go` so generator-emitted call sites still compile:

```go
// internal/db/contract.go
type UserRepository interface { GetUser(...); UpsertUser(...); ... }
type OrgRepository  interface { GetOrg(...); ListOrgsByUser(...); ... }

// Umbrella so forge-generated wiring (testing.go's db.Service references,
// bootstrap's Deps field for db) compiles without you touching the
// generator. Implementers satisfy this by satisfying each narrow
// interface — Go does that for free.
type Service interface {
    UserRepository
    OrgRepository
    // ...
}

type Deps struct {
    DB *sql.DB
}

func New(d Deps) Service { return &postgresRepo{db: d.DB} }
```

Without the umbrella, every `pkg/app/testing.go` regen breaks with `undefined: db.Service` and you spend the rest of the port chasing the same generator complaint. Don't.

### Regenerate after every `Deps` edit

`forge generate` is incremental and cheap, but it ONLY refreshes `pkg/app/bootstrap.go` / `pkg/app/testing.go` / `pkg/app/wire_gen.go` when invoked. If a port phase drops a field from `<pkg>.Deps` (e.g. removes a vestigial `Logger`) and the next phase runs `go build` / `forge lint` without an intervening `forge generate`, the build fails because `bootstrap.go` is still emitting `pkg.New(pkg.Deps{Logger: ...})` against the new `Deps` struct that no longer has `Logger`. This was the dominant cause of stale-state errors in the v3 control-plane migration.

**Rule for the merge agent**: after ANY edit to any `internal/<pkg>/contract.go` `Deps` struct (or to `pkg/app/app_extras.go`), run `forge generate` BEFORE running `go build` / `go test` / `forge lint`. If the gate fails for what looks like a "stale codegen" reason — `unknown field X in struct literal of type pkg.Deps`, `undefined: pkg.Service`, `cannot use Y as Z value` — the fix is `forge generate`, NOT editing the generated file. The generated file is the symptom; the source-of-truth file is the contract.go you just touched.

### Goose → golang-migrate

If the source uses goose (one-file migrations with `-- +goose Up` / `-- +goose Down` markers), forge expects golang-migrate (two-file `.up.sql` + `.down.sql`). The conversion is mechanical for the common case:

1. Split each file at the `-- +goose Down` line into two files.
2. Drop `-- +goose StatementBegin` / `-- +goose StatementEnd` markers (they wrap single statements; golang-migrate handles that natively).
3. Files declaring `-- +goose NO TRANSACTION` translate to a golang-migrate `x-no-tx-wrap` header on that file.
4. Renumber files starting from the next-available index AFTER any pack-installed migrations (e.g. audit-log occupies 00002, api-key occupies 00003, so source migrations start at 00004).

If source migrations have foreign-key dependencies on tables that pack migrations create (or vice versa), reorder carefully. Pack migrations are not negotiable; renumber yours.

## Test regressions during port — fix the port, never blame the source

If a test passes in the source repo but fails in the cp-forge port, **it is always a port bug**. Never write "pre-existing in source" in a synthesis or friction note without first running `go test ./<same-package>/...` against the source tree to verify. The v2 migration of control-plane had a synthesis agent declare three `svcbilling` tests as "pre-existing source failures" — verified false; source passed, port failed. The regressions were real port bugs (wrong entitlement-org vs first-org selection logic).

Concrete rule for the final gate: before declaring a migration complete, the synthesizing agent MUST:

1. Run `go test -count=1 ./...` in the cp-forge tree, capture the failing test names.
2. For each failing test, run the equivalent in the source tree (paths usually differ by `internal/service/<x>/` vs `internal/<x>/`).
3. If source passes and cp-forge fails → port bug. Either fix or revert that package. Don't ship a "victory" report with red tests.
4. If both fail → can be flagged as inherited, but only with the source-test exit code captured in the synthesis output.

The cost of doing this right is one `go test` invocation per failing test. The cost of getting it wrong is shipping a half-broken port and discovering it weeks later when the failing path matters.

## Lint failures during port — fix the code or `//nolint:`, never path-exclude

When `forge lint` fires on a freshly-ported package, the temptation is to add `internal/<pkg>/` to a path-based exclusion list in `.golangci.yml`. **Don't.** That silences every linter on the package — including the bug-catchers (errcheck, govet, staticcheck, unused) — to make today's port land. You will pay for it in subtle bugs across that package's lifetime.

The right responses, in order of preference:

1. **Fix the code.** Most `gocognit` / `funlen` / `nestif` flags point at a function that genuinely benefits from being split. `interfacebloat` points at a god-interface that should be split (see above). Take the small refactor.
2. **`//nolint:gocognit // ported as-is from <source-path>; rewriting risks behavior drift` at the function declaration.** Per-line, with a justification comment. Reviewers and future-you can see exactly what was exempted and why. Standard Go convention.
3. **Path exclusion as a LAST resort, on generated code only.** Things like `gen/`, `internal/<pkg>/embed.go`, or a sqlc output dir. Never on hand-written code.

Forge's defaults are opinionated by design. A clean port should land with at most a handful of `//nolint:` annotations, not a growing list of yaml path exclusions.

## Multi-binary `cmd/` layouts

`forge add service` emits one `cmd/<service>/main.go` per service. If the source repo has additional binaries (CLI tools, background daemons, ops scripts) that aren't first-class forge components:

- For binaries that wrap forge-managed services or workers, prefer `forge add` and let forge own the wiring.
- For genuinely standalone binaries (proxies, sidecars, off-service consumers) that need their own Deployment, use `forge add binary <name>` — see the `binaries` skill for the decision tree.
- For tiny one-off scripts that don't deserve a contract.go (a dev seed script, a one-shot migration helper), drop them under `cmd/<name>/main.go` directly without an `internal/<name>/` package.

## k8s manifests

Forge emits `deploy/kcl/<env>/` (KCL-based manifests, one dir per environment: `dev`, `staging`, `prod`). KCL is canonical — there is no "disable KCL, ship hand-written YAML" mode. Either:

- **Adopt KCL.** Translate hand-written manifest customizations into KCL overrides. Use `additional_manifests = [...]` on the Bundle for raw manifest dicts that don't fit a typed entity (ClusterIssuers, SealedSecrets, hand-typed CRDs). Recommended.
- **Disable the deploy feature.** Set `features.deploy: false` in `forge.yaml` and bring your own manifests under any tree you like. `forge deploy <env>` and the deploy half of `forge generate` then short-circuit with a clear "feature 'deploy' is disabled" message.

Per-env config that used to live in `forge.yaml -> environments[].config` now lives in sibling `config.<env>.yaml` files next to forge.yaml; per-env deploy knobs (cluster/namespace/registry/domain) live on `forge.K8sCluster` blocks in KCL. See the `environments-to-kcl` migration skill if you're porting a project that pre-dates this split.

## Final checks before declaring done

```bash
forge generate          # idempotent on a healthy project
forge lint              # contract + db + general lints
forge build             # binaries + frontends + Docker images
forge test              # unit + integration
forge test e2e          # full-stack (requires `forge run` in another shell)
forge deploy dev        # local k3d
```

## Rules

- One service per proto package. Hyphens in names are fine; forge handles the snake-case translation everywhere it needs to.
- Pack-after-component, then `forge generate`. Never the reverse.
- `bootstrap.go` is generated — all custom wiring goes in `setup.go`.
- KCL or hand-rolled manifests, not both.
- `forge generate` after every `forge add` and every `forge pack install`. It's idempotent and catches misconfigurations early.

## When this skill is not enough

- **CLI / library shape** — see `migration-cli`.
- **Designing the contract surface** for ported internal packages — see `contracts`.
- **Pack-specific config** (auth, billing, SMS) — see `packs`, `auth`, and the per-pack docs in `forge pack list`.
