# Migration Tips

Notes on bugs discovered during dogfood / migration runs and the fixes
applied. Ordered by bug number; numbering is shared across the audit /
fix cycle (see `PACK_AUDIT.md` for the full audit).

## Skills consolidation: skills proactive, lint reactive (2026-04-30)

Codified a separation between skills (proactive guidance) and lint
(reactive enforcement). Skills explain the WHY and the playbook;
`forge lint --conventions` and friends own the WHAT and the
remediation hint at the moment the violation is introduced.

What changed:

- `proto/conventions` skill **deleted**. Its content was a near-1:1
  restatement of the four `forgeconv-*` lint rules
  (`forgeconv-pk-annotation`, `forgeconv-tenant-annotation`,
  `forgeconv-timestamps`, `forgeconv-one-service-per-file`). The
  unique value (canonical entity shape, multi-tenant patterns,
  cross-service shared types) was merged into the `proto` skill.
- `proto` skill grew explicit "**Enforced by:** `forgeconv-*`" notes
  next to each rule so the user can find the remediation surface.
- Cross-references in `proto-split`, `api/handlers`, `frontend/pages`
  and the project-level `reliant.md.tmpl` were redirected from
  `proto/conventions` → `proto`.
- Skill count: 38 → 37 (one delete; no other consolidations needed —
  the audit found that nearly every other skill is real methodology
  or cookbook content lint cannot capture).

What did NOT change: skills with substantive methodology (debug,
testing, migration playbooks, frontend cookbooks, contracts decision
matrix) stayed verbatim. The audit table in `SKILLS_AUDIT.md` records
the type classification per skill. Migration-shape walkthroughs
(`v0.x-to-contractkit`, `v0.x-to-observe-libs`) stayed full-length per
the deprecation-cycle policy in `migration/upgrade`.

## Proto-first context: greenfield vs migrated (2026-04-30)

Made the proto-vs-migration boundary explicit in docs. Two modes:

- **Greenfield (proto authoritative).** Default `forge new`. Proto
  entity annotations drive ORM, CRUD handlers, frontend hooks, and
  the initial migration. Proto leads, migrations track.
- **Migrated (migrations authoritative).** When porting an existing
  schema, migrations carry the schema and proto entities become
  advisory documentation. `forge generate` does not regenerate
  migrations from proto.

`forge audit`'s existing `proto_migration_alignment` category already
detected this. The audit hint now points at the `proto`, `db`, and
`migration` skills explicitly. Each of those skills now carries a
section explaining the boundary from its own angle:

- `proto` — "Proto-driven entities: greenfield vs migrated"
- `db` — "Proto entities: greenfield vs migrated"
- `migration` — "Proto entity strategy in migrations"

When a migrated project has divergence, the user has three resolutions:
drop proto entities, sync via `forge db proto sync-from-db`, or roll a
migration forward. The audit hint enumerates these.

## Quality-of-life flags landed 2026-04-30

A handful of small-but-painful frictions got fixed in one batch:

- `forge db migrate {up,down,status,version,force}` now accept the dsn
  via `$DATABASE_URL` when `--dsn` is omitted, so the typical workflow
  is just `forge db migrate up` once `DATABASE_URL` is exported.
- `forge deploy <env> --dry-run` now skips the docker build/push and
  k3d bootstrap entirely — dry-run is pure manifest rendering, ~74ms
  vs. tens of seconds.
- `forge.yaml` accepts a project-level `pack_overrides.<pack>.skip_migrations: true`
  knob that declines a pack's shipped migrations at install time, for
  when the project's own migrations supersede the pack's. Useful in
  this exact migration scenario (audit-log + api-key in
  control-plane-next where the schema was already in place).
- `forge add operator <name> --with-placeholder-crd --api-package <pkg>
  --crd-type <Type>` produces a CRD type in a separate
  `api/<version>/<pkg>/types.go` package so the operator binary name
  and the CRD type can diverge (`workspace-controller` reconciles
  `Workspace`). The default — bare `forge add operator <name>` — now
  emits an operator-binary-only scaffold and you add CRDs separately
  with `forge add crd <Name>` (delivered by the parallel CRD agent in
  the same batch).
- The `audit-log` pack now ships a read-side `ListAuditEvents` RPC at
  `proto/audit/v1/audit_log.proto` plus a handler under
  `pkg/middleware/audit/auditlog/handler.go`. Every project that
  installs audit-log gets a free admin view of its audit history.
- Three new base UI primitives — `RowActionsMenu`, `ProgressBar`,
  `StatusDot` — joined the scaffold's `coreComponents`. Frontend packs
  and pages should reuse these instead of inlining their own (e.g.
  the workspaces/daemons row-action kebab menu).

## Internal-package contracts: name everything `Service`/`Deps`/`New`

Forge's `pkg/app/bootstrap.go` codegen is hardcoded to call
`<pkg>.New(<pkg>.Deps{Logger: ..., Config: cfg})` and assign the
result to a `<pkg>.Service` field, then wrap with
`<pkg>.NewTracedService` and `<pkg>.NewMetricService`. The
`mock/middleware/metrics/tracing` codegen, by contrast, picks up
*whichever* interface you name in `contract.go`. So if your contract
declares `type Sender interface { ... }` + `func NewSender(...) Sender`,
the package builds in isolation but `pkg/app/bootstrap.go` won't
compile — it references names that don't exist.

**Always** name the interface `Service`, the constructor `New`, and
the deps struct `Deps`. The deps struct must accept at minimum
`Logger *slog.Logger` and `Config *config.Config` because that's what
bootstrap passes. Pull any package-specific config out of `*Config`
inside your constructor.

The bootstrap template (`internal/templates/project/bootstrap.go.tmpl`)
is the source of this convention; if you find yourself wanting to use
a different name, either rename to `Service` or add a small adapter.

## Descriptor recovery: when `forge generate` chokes on `gen/forge_descriptor.json`

Symptom: `forge generate` errors with `failed to parse config protos:
parse forge descriptor: invalid character '"' after top-level value` or
similar mid-file JSON corruption. Cause: parallel buf plugin
invocations racing on the descriptor write.

Recovery (no data loss — descriptor is fully reconstructed from
proto/):

```bash
cat > gen/forge_descriptor.json <<'EOF'
{"services":null,"entities":null,"configs":null}
EOF
forge generate
```

Tracked in `forge/FORGE_BACKLOG.md` as a forge bug to fix.

## Bug #21 fix: jwt-auth × clerk dev-auth collision

**Symptom.** Installing both `jwt-auth` and `clerk` packs in the same
project produced duplicate-symbol build errors:

```
pkg/middleware/dev_auth.go (jwt-auth) and pkg/middleware/auth_gen.go (clerk)
both declare DevAuthEnabled + DevClaims
```

Both packs are auth providers and users may legitimately want both
installed at once — for example, Clerk for end-user SSO plus jwt-auth
for service-to-service tokens — so mutual exclusion is not the right
fix.

**Fix (option A — per-pack rename).** Symbols renamed to be
pack-prefixed so the two packs no longer collide:

| Pack       | Old symbol         | New symbol               |
| ---------- | ------------------ | ------------------------ |
| `jwt-auth` | `DevAuthEnabled()` | `JWTDevAuthEnabled()`    |
| `jwt-auth` | `DevClaims()`      | `JWTDevClaims()`         |
| `clerk`    | `DevAuthEnabled()` | `ClerkDevAuthEnabled()`  |
| `clerk`    | `DevClaims()`      | `ClerkDevClaims()`       |

All call sites within each pack's templates were updated to use the
new prefixed names. The pack-internal tests
(`internal/packs/jwt_auth_test.go`,
`internal/packs/clerk_test.go`) were updated to assert the prefixed
names and to negative-assert that no bare `DevAuthEnabled` /
`DevClaims` declarations remain — so this regression cannot
silently come back.

**Files changed:**

- `internal/packs/jwt-auth/templates/dev_auth.go.tmpl`
- `internal/packs/jwt-auth/templates/auth_gen_override.go.tmpl`
- `internal/packs/clerk/templates/clerk_auth.go.tmpl`
  (doc-comment reference only)
- `internal/packs/clerk/templates/clerk_auth_gen.go.tmpl`
- `internal/packs/jwt_auth_test.go`
- `internal/packs/clerk_test.go`

**Verified scenarios.** All clean (`go build ./...` + `forge lint`):

1. `jwt-auth` + `clerk` together (the original bug).
2. `jwt-auth` standalone.
3. `clerk` standalone.
4. Full 6-pack gauntlet (`jwt-auth`, `api-key`, `stripe`,
   `audit-log`, `clerk`, `twilio`).

### Future cleanup (option B — shared dev-auth helper)

The rename is the minimum-invasive fix and unblocks users today, but
it leaves a deeper architectural issue in place: when both auth packs
are installed, **both** still emit a file named
`pkg/middleware/auth_gen.go`, with the second install silently
overwriting the first. That means the project ends up with one
provider's `InitAuth` / `CloseAuth` / `GeneratedAuthInterceptor`
wired and the other provider's helper symbols (`JWTDevAuthEnabled`
etc.) sitting unused. Build and lint pass, but the second-installed
provider is the one that actually runs.

The durable fix is to extract the dev-mode auth helper into the
**scaffold base** (provided unconditionally as
`pkg/middleware/dev_auth.go` with a single canonical
`DevAuthEnabled()` / `DevClaims()` pair) and to give each auth pack
a distinct generated filename (e.g.
`pkg/middleware/jwt_auth_gen.go`, `pkg/middleware/clerk_auth_gen.go`)
plus a small registry so a project can compose multiple providers
explicitly (Clerk for end users + jwt-auth for service-to-service)
rather than racing on filename ownership. That's a real refactor —
out of scope for the Bug #21 unblock, deliberately deferred to a
later session.

## Test convention consistency (integration build tag)

**Symptom.** The `testing/integration` skill said integration tests
must use `//go:build integration`, but a chunk of the scaffolded
test surface — specifically the CRUD test gen and the
`TestUnit_*` frames in the same generated file — was relying on
`if testing.Short() { t.Skip(...) }` instead. Two contradictory
mechanisms were promoted to dogfood users at once: the build tag
physically excludes the file from `go test ./...` (no compile, no
skip-at-runtime), while `testing.Short()` is a runtime opt-out that
only fires when `-short` is passed.

**Decision.** Standardize on the build tag. It matches the existing
`//go:build e2e` convention, matches what `forge test integration`
already does (it always passes `-tags integration`), and is what the
skill was already telling users to do. `testing.Short()` is reserved
for the rare case of a hermetic test with a fast/slow toggle, not
for "this needs a real database — please skip me".

**Files changed:**

- `internal/templates/service/handlers_crud_test_gen.go.tmpl` —
  reduced to **unit** test frames only (`TestUnit_*`, no infra), no
  build tag, `testing.Short()` removed.
- `internal/templates/service/handlers_crud_integration_test.go.tmpl`
  — new template for the `TestCRUDLifecycle_*`, `TestTenantIsolation_*`,
  `TestListPagination_*`, `TestListFilters_*`, `Test{Create,Get,Update,Delete}_*_NotFound/EmptyRequest`
  suites that previously lived alongside the unit frames. Carries
  `//go:build integration` and no `testing.Short()` skips.
- `internal/codegen/crud_gen.go` — `GenerateCRUDTests` now writes
  both `handlers_crud_gen_test.go` (unit) and
  `handlers_crud_integration_test.go` (build-tagged), with
  stale-file cleanup for both.
- `internal/codegen/crud_gen_test.go` — assertions split across the
  two output files; positive assertion that the integration file
  starts with `//go:build integration\n`, negative assertion that
  the unit file does not start with any `//go:build` directive.
- `internal/cli/generate_services.go` — printout updated to mention
  both generated files.
- `internal/templates/project/Taskfile.yml.tmpl` — added
  `task test:integration` (`go test -race -tags=integration -count=1 -timeout=10m ./...`).
  `task test`'s comment updated to call out that integration *and*
  e2e are excluded by build tag.
- `internal/templates/project/skills/forge/testing/integration/SKILL.md`
  and the dogfood mirror `.claude/skills/testing-integration/SKILL.md`
  — added a "Convention: build tag, never `testing.Short()`" section
  with a side-by-side table of when each mechanism is appropriate.

The scaffolded `service/integration_test.go.tmpl` and
`pkg/app/testing.go` (non-test helper) were already correct and
needed no changes.

**Verified scenarios.** All clean:

1. `go build ./...` and the codegen / templates / cli / generator
   test suites all pass on the modified tree.
2. `forge new ttest --kind service --service api` produces a
   project where `task test` runs only unit tests and
   `task test:integration` runs with `-tags=integration`. Both
   pass on the bare scaffold.
3. `head -3 handlers/api/integration_test.go` shows
   `//go:build integration` + `// +build integration` + blank line.
4. The pack-test failures observed in `internal/packs/jwt-auth`,
   `internal/packs/clerk`, `internal/packs/api-key`, and
   `internal/packs/audit-log` predate this change and are tracked
   separately under the parallel pack agent.

## Pack sub-namespacing (collision-by-construction)

The Bug #21 follow-up: the durable fix promised in that section's
"Future cleanup" note. Each pack now installs its middleware code
into a **per-pack Go subpackage** under `pkg/middleware/<packname>/`,
not into `pkg/middleware/` directly. Two auth packs can no longer
overwrite each other's `auth_gen.go` because the files literally
live in different directories with different package decls.

| Pack       | Subpackage path                          | Package decl |
|------------|------------------------------------------|--------------|
| `jwt-auth` | `pkg/middleware/auth/jwtauth/`           | `jwtauth`    |
| `clerk`    | `pkg/middleware/auth/clerk/`             | `clerk`      |
| `api-key`  | `pkg/middleware/auth/apikey/`            | `apikey`     |
| `audit-log`| `pkg/middleware/audit/auditlog/`         | `auditlog`   |
| `stripe`   | `pkg/clients/stripe/`                    | `stripe`     |
| `twilio`   | `pkg/clients/twilio/`                    | `twilio`     |

**One-more-layer nesting (2026-04-30).** Each pack now declares a
`subpath:` hint in `pack.yaml` and writes its non-proto/non-migration
code under that subtree. Auth providers nest under
`pkg/middleware/auth/<provider>/`, audit lives at
`pkg/middleware/audit/auditlog/`, and external-service clients live at
`pkg/clients/<service>/`. The `subpath:` field is informational —
output paths in `files:` and `generate:` remain the source of truth —
but `forge pack list` shows the SUBPATH column so users can see at a
glance what subtree a pack will touch.

The shared `Claims` type, `ContextWithClaims`, `ClaimsFromContext`,
and the `KeyValidator` interface stay in `pkg/middleware/`. Pack
subpackages import them.

**Composition is the user's job.** Connect already gives you
`connect.WithInterceptors(...)`. Forge ships no chain helper, no
capability slot system, no pack registry, no init() side effects.

```go
// In pkg/app/setup.go (you own this file):
import (
    "connectrpc.com/connect"

    "<module>/pkg/middleware/auth/jwtauth"
    clerkauth "<module>/pkg/middleware/auth/clerk"
)

func Setup(logger *slog.Logger) (connect.HandlerOption, error) {
    if err := jwtauth.Init(logger); err != nil {
        return nil, err
    }
    if err := clerkauth.Init(logger); err != nil {
        return nil, err
    }
    return connect.WithInterceptors(
        jwtauth.Interceptor(),
        clerkauth.Interceptor(),
    ), nil
}
```

**Symbol renames.** The Bug #21 prefixes (`JWTDevAuthEnabled`,
`ClerkDevAuthEnabled`, `JWTValidator`, `ClerkJWTValidator`,
`APIKeyValidator`, `AuditInterceptorWithStore`) are now redundant —
the subpackage path namespaces them. They were reverted to plain
package-relative names:

| Pack | Old symbol | New symbol |
|------|------------|------------|
| jwt-auth | `JWTDevAuthEnabled` | `jwtauth.DevAuthEnabled` |
| jwt-auth | `JWTDevClaims` | `jwtauth.DevClaims` |
| jwt-auth | `JWTValidator` | `jwtauth.Validator` |
| jwt-auth | `JWTValidatorConfig` | `jwtauth.ValidatorConfig` |
| jwt-auth | `NewJWTValidator` | `jwtauth.NewValidator` |
| jwt-auth | `InitAuth` / `CloseAuth` / `GeneratedAuthInterceptor` | `jwtauth.Init` / `Close` / `Interceptor` |
| clerk | `ClerkDevAuthEnabled` | `clerk.DevAuthEnabled` |
| clerk | `ClerkDevClaims` | `clerk.DevClaims` |
| clerk | `ClerkJWTValidator` | `clerk.Validator` |
| clerk | `ClerkValidatorConfig` | `clerk.ValidatorConfig` |
| clerk | `NewClerkJWTValidator` | `clerk.NewValidator` |
| clerk | `InitAuth` / `CloseAuth` / `GeneratedAuthInterceptor` | `clerk.Init` / `Close` / `Interceptor` |
| api-key | `APIKeyValidator` / `NewAPIKeyValidator` | `apikey.Validator` / `apikey.NewValidator` |
| audit-log | `AuditInterceptorWithStore` | `auditlog.Interceptor` |

The `pkg/clerk/` webhook package is unrelated to
`pkg/middleware/auth/clerk/` (the auth interceptor) — both happen to use
package decl `clerk`, so import the auth one with an alias if you
need both in the same file
(`clerkauth "<module>/pkg/middleware/auth/clerk"`).
Same pattern for `apikey` if you import `pkg/apikey` (the store) and
`pkg/middleware/auth/apikey` (the validator) together.

## New packs from the overnight migration

Two packs that recurred during the overnight migration are now first-class:

- **`nats`** — JetStream client + publisher + durable pull-consumer with
  backoff/retry/DLQ. Installs into `pkg/clients/nats/`. Wire it from
  `pkg/app/setup.go`:

  ```go
  nc, js, err := nats.Connect(ctx, nats.Config{URL: cfg.NATSURL})
  if err != nil { return err }
  app.NATSPublisher = nats.NewPublisher(js)
  ```

  control-plane-next's `internal/natsio/` predates this pack and can
  optionally migrate to it; for now it remains bespoke and the pack is
  the recommended starting point for any *new* NATS-using service.

- **`data-table`** — first frontend pack (manifest field
  `kind: frontend`). Installs a TanStack-Table-based generic table into
  every frontend declared in `forge.yaml`. Pairs with forge's
  auto-generated `useEntities` hooks:

  ```tsx
  import { DataTable, columnFor } from "@/components/data-table";
  const { data, isLoading } = useUsers({ limit, search });
  return <DataTable columns={cols} data={data?.users ?? []} isLoading={isLoading} />;
  ```

The pack manifest schema gained `kind:`, `npm_dependencies:`, and
templated `output:` paths (e.g. `{{.FrontendPath}}/...`) to support the
frontend-pack install path. Existing Go pack manifests are unaffected
(absent `kind:` defaults to `go`).

## Frontend packs reuse the base library (layered model)

Frontend packs follow a three-layer model:

1. **Base library** — generic primitives. Two tiers:

   - **Low-level primitives:** `Button`, `Input`, `Label`, `Form`,
     `Card`, `Avatar`, `Tabs`, `Table`, `Select`, `Chip`. These are the
     shared building blocks every frontend pack composes. They were
     added in the same pass that closed the "frontend packs reuse base
     library" gap — both `data-table` and `auth-ui` now compose them
     directly.
   - **Higher-level primitives:** `SearchInput`, `Pagination`,
     `AlertBanner`, `Modal`, `SkeletonLoader`, `Badge`,
     `ToastNotification`, `KeyValueList`, `PageHeader`, `SidebarLayout`,
     `LoginForm`.

   Master copy at `forge/components/components/ui/`; installed
   unconditionally into `frontends/<name>/src/components/ui/` at scaffold
   time (see `coreComponents` in
   `internal/generator/frontend_gen.go`). Files are written
   `overwrite: once` so they are user-owned after first scaffold.
2. **Forge-aware primitives** — hook-aware components shipped with the
   scaffold (e.g. `Nav` driven by `forge.yaml` pages). Live in
   `internal/templates/frontend/nextjs/src/components/`.
3. **Domain packs** — opt-in installs under
   `frontends/<name>/src/components/<pack>/` (e.g. `data-table`).

**Rule.** Frontend pack templates import from layers 1–2; pulling a
third-party UI dep in directly (`@radix-ui/*`, `@headlessui/*`, `@mui/*`,
or `@tanstack/react-table` for JSX) is a convention violation. If a
needed primitive is missing from the library, propose adding it rather
than reimplementing in the pack. Headless engines (TanStack Table,
charting libs) can be allowlisted explicitly:

```yaml
# pack.yaml
allowed_third_party:
  - "@tanstack/react-table"  # Headless engine; we wrap it with base library components.
```

**Lint enforcement.** The new `frontendpacklint` analyzer (run via `forge
lint` or `forge lint --frontend-packs`) flags violations as
**warnings** — it is intentionally non-blocking. Full details in the
`pack-development` skill.

**Reference refactor.** Both shipped frontend packs (`data-table` and
`auth-ui`) compose only the base library:

- `data-table`: `Table` + `TableHeader`/`TableBody`/`TableRow`/`TableHead`/
  `TableCell` (markup), `Button` (Prev/Next + clear-all), `Select`
  (page-size picker), `Chip` (filter pills), `SearchInput` (search box),
  `AlertBanner` (errors), `SkeletonLoader` (loading), base `Pagination`
  (page-number nav). TanStack Table remains the headless sort/filter
  engine (allowlisted via `allowed_third_party`).
- `auth-ui`: `Card` (form surface), `Form` + `FormField` + `FormError`
  (form structure), `Input` + `Label` (text fields), `Button` (submit /
  Sign-in nav button / Continue-with-Google), `AlertBanner` (submit
  errors), `Avatar` (SessionNav user chip). `react-hook-form`, `zod`,
  and `zustand` remain as utility (non-UI) deps.

The `frontendpacklint` analyzer reports zero warnings against either
pack as of this refactor.

## Annotation-only ORM + forge convention lint suite

Forge's ORM codegen used to apply name-based heuristics on top of the
`(forge.v1.entity)` / `(forge.v1.field)` annotation system: `id` →
primary key, `created_at`/`updated_at`/`deleted_at` → managed
timestamps, `email` → unique, `*_id` → foreign key, plus an
"entity-by-shape" path that turned any `proto/db/v1/` message with an
`id` field into an entity even without an explicit annotation. These
heuristics conflicted with the explicit annotation system and silently
shadowed user intent — the overnight migration kept hitting cases where
forge generated code the user didn't ask for and didn't expect.

**Removed in forge v0.6:**

- `looksLikeEntity()` — entities now require an explicit
  `option (forge.v1.entity) = { ... }` annotation. No more "looks like
  an entity, must be one" path-restricted inference.
- Default-PK-by-name (`id` field auto-marked PK) — every entity must
  declare its PK with `[(forge.v1.field) = { pk: true }]`. `forge
  generate` now errors out with a precise remediation message
  (`entity "User" (in proto/...): missing primary key annotation: mark
  the PK field with [(forge.v1.field) = { pk: true }]`).
- `applyFieldInferences()` — `_id` no longer auto-creates an FK
  reference, `email` no longer auto-implies unique, the
  `name/title/status/role/...` not-null heuristic is gone. Use
  `(forge.v1.field) = { ref: ..., unique: true, ... }` explicitly.
- `inferEntityOptions()` — `timestamps: true` and `soft_delete: true`
  are no longer auto-toggled by detecting `created_at`/`deleted_at`
  fields. Set them on the entity option block when you want them.

**Backed by lint:** the new `forgeconv` analyzer suite (under
`internal/linter/forgeconv/`) runs as part of `forge lint` and catches
each of these classes of violation **before** `forge generate` would
fail. Findings carry copy-pasteable remediation text. Rules:

- `forgeconv-one-service-per-file` — one service per .proto, error.
- `forgeconv-pk-annotation` — entity message with no `pk: true` field, error.
- `forgeconv-timestamps` — `*_at` Timestamp field with neither
  entity-level `timestamps: true` nor field-level annotation, error.
- `forgeconv-tenant-annotation` — entity has one `tenant: true` field
  AND another tenant-shaped field name without the annotation, warning.

The `requirecontract` analyzer (internal/<pkg>/contract.go required for
exported methods) is wired through the same `contractlint` binary and
runs in the default `forge lint` flow.

Migration cost: low. Most projects already use explicit annotations
because the `proto` skill has long recommended it. Only projects that
relied on the old "by-name" auto-detection need to add `pk: true`,
`tenant: true`, or `timestamps: true` to their entity definitions.
`forge lint --conventions` lists every spot that needs a fix.

## Frontend pack: `auth-ui`

A second frontend pack (after `data-table`) ships opinionated login /
signup / session UI that pairs with each auth backend pack. Pick the
backend first, then install `auth-ui` with the matching variant:

```bash
forge pack install jwt-auth                              # or clerk / firebase-auth
forge pack install auth-ui                               # default → provider=jwt-auth
forge pack install auth-ui --config provider=clerk       # alt → @clerk/nextjs SDK
forge pack install auth-ui --config provider=firebase-auth
```

What lands in `frontends/<name>/src/components/auth/`:

- `LoginForm.tsx` / `SignupForm.tsx` — react-hook-form + zod validated.
- `SessionNav.tsx` — header avatar dropdown with sign-out and an optional
  tenant switcher.
- `DevModeBanner.tsx` — visible warning when
  `NEXT_PUBLIC_AUTH_DEV_MODE=true`, mirroring the backend pack's
  `dev_mode: true` flag.
- `auth-store.ts` — Zustand: `{user, session, isLoading, isAuthenticated}`.

Templates branch on `{{ .PackConfig.provider }}`. The right SDK is pulled
in via the manifest's `provider_npm_dependencies` map at install time, so
`provider=jwt-auth` does not download `@clerk/nextjs` or `firebase`.

This is also the first pack to use the new `--config key=value` flag on
`forge pack install`, which shallow-merges overrides on top of the
manifest's `config.defaults` before templates render. Add the flag to
your own packs by exposing knobs in `pack.yaml`'s `config.defaults` and
referencing them as `{{ index .PackConfig "<key>" }}` in templates.

## tdd library and per-RPC test scaffolds

`forge/pkg/tdd` is a generics-based table-driven test library that the
test scaffolders now use. Per-RPC unit / integration / E2E tests, and
the new contract test scaffold for `internal/<pkg>/contract.go`
packages, are tiny shims (~5–10 lines per case) that hand a slice of
cases to a library helper. The library, not the project, carries the
`for _, tc := range cases { t.Run(...) }` boilerplate and the error-code
assertion.

Import path: `github.com/reliant-labs/forge/pkg/tdd`. Module path: same
sub-module as `pkg/orm` and `pkg/middleware`. Forge's scaffolded
projects pick up the import automatically once `forge generate` runs;
your `go.mod` will gain a `require github.com/reliant-labs/forge/pkg
v…` line on the first `go mod tidy`.

What changed in the scaffolds:

- `handlers/<svc>/handlers_test.go` — one `Test<Method>(t)` per RPC,
  built around `tdd.TableRPC[Req, Resp]`. Streaming RPCs still get a
  TODO comment instead of a body.
- `handlers/<svc>/integration_test.go` — same shape, build-tagged
  `integration`, runs through `app.NewTest<Service>Server` and the
  generated Connect client.
- `e2e/<svc>/service_test.go` — same shape, build-tagged `e2e`, runs
  against the live binary started in `TestMain`.
- `internal/<pkg>/contract_test.go` — new scaffold, scaffolded once by
  `forge package new` and `forge generate`. Uses
  `tdd.TableContract` over a slice of `tdd.ContractCase`. User-owned
  after the first scaffold — forge will not overwrite it.

If you have an existing project that hand-wrote per-RPC tests in the
old verbose shape, the new scaffolds will not overwrite them — only
brand-new services and the once-only contract test pick up the new
template. To migrate by hand, see the `testing/patterns` skill for the
copy-paste shape.

Pre-existing E2E helpers were trimmed: the local `assertConnectError`
in `e2e/<svc>/helpers_test.go` was removed in favor of
`tdd.AssertConnectError`. If your project previously called the local
helper, the build will surface the unused-function path on first regen
— either delete the local helper or import the library version.

Library deps: only `connectrpc.com/connect` (already a project
dependency) plus the standard library. The `tdd.SetupMockDB` helper
expects callers to blank-import `github.com/mattn/go-sqlite3` from a
test file — Forge projects with a database already do this in
`pkg/app/testing.go`, so no new import is needed there.


## Auth & tenant migrated to library (30-04-2026)

`pkg/middleware/auth_gen.go` and `pkg/middleware/tenant_gen.go` are now thin
shims over `forge/pkg/auth` and `forge/pkg/tenant`:

- **Before:** `auth_gen.go` was ~211 lines of provider-branched JWT/API-key
  validation logic. `tenant_gen.go` was ~106 lines of context plumbing.
- **After:** `auth_gen.go` is ~40 lines (config struct + 1-call interceptor
  factory). `tenant_gen.go` is ~38 lines (3 thin helpers + interceptor
  factory). Both delegate to the library.

`pkg/middleware/claims.go` has changed shape: `Claims` is now
`type Claims = auth.Claims` (an alias). User code that references
`middleware.Claims` keeps compiling; the canonical type lives in
`pkg/auth`.

If you have a custom Claims with extra fields, the alias breaks. Today
that case is unsupported — open an issue and we'll fall back to the
generic-takes-claims-as-type-parameter pattern. control-plane-next does
not have custom fields and is not affected.

Behaviour preserved exactly:
- `JWT_SECRET` env-var fallback when `JWTConfig.Secret` is empty.
- `/Health/` substring procedures always skip auth.
- Skip list from forge.yaml is honoured.
- Tenant interceptor still requires a non-empty tenant claim (returns
  PermissionDenied) unless `tenant.Config{Optional: true}` is set.

Library entry points:

```go
import (
    "github.com/reliant-labs/forge/pkg/auth"
    "github.com/reliant-labs/forge/pkg/tenant"
)

// Auth
v, err := auth.NewValidator(auth.Config{
    Provider: auth.ProviderJWT,
    JWT:      auth.JWTConfig{SigningMethod: "HS256", Secret: "..."},
})
ic := v.Interceptor(auth.InterceptorOptions{}, ContextWithClaims)

// Tenant
ic := tenant.NewInterceptor(tenant.Config{ClaimField: "org_id"}, ClaimsFromContext)
ctx := tenant.WithTenantID(ctx, "tenant-x")
id, err := tenant.Require(ctx)
```

`tenant_gen.go` is now generated unconditionally so that
`pkg/app/testing.go`'s `middleware.ContextWithTenantID` reference resolves
even when multi-tenant is disabled. Previously the file was only generated
when entities had `tenant_key` annotations, which broke the testing
helpers in fresh service scaffolds.

## Contract gen → `pkg/contractkit` shim + library shape

The four contract-driven `*_gen.go` files (mock, middleware, tracing,
metrics) are now thin shims that delegate to runtime helpers in
`github.com/reliant-labs/forge/pkg/contractkit`. The library owns the
cross-cutting logic; templates emit per-method bodies of 3-5 lines that
hand off to `contractkit.LogCallErr` / `TraceStart` /
`Metrics.RecordCall` etc.

What this means in practice:

- **No new authoring change.** `contract.go` is still the only file you
  hand-write. `forge generate` emits the same four output filenames in
  the same locations. Existing tests that assert on
  `MockService.SendFunc` / `Calls("Send")` keep working — the public
  surface is preserved.

- **Mocks now embed `contractkit.Recorder`.** Every method call is
  recorded with its arguments. Tests can assert via
  `mock.CallCount("Send")` and `mock.Calls("Send")[0].Args[i]` without
  the test author having to wire the recording themselves. The previous
  pattern of setting `XxxFunc` to capture args still works; the
  recorder is additive.

- **The "func not set" error string is preserved exactly.** Generated
  mocks return `contractkit.MockNotSet("MockService", "Send")`, which
  formats to `"MockService.SendFunc not set"` — identical to the old
  `fmt.Errorf` line. If you have tests that match that substring,
  nothing changes.

- **Middleware / tracing / metrics shapes are unchanged.** The slog
  attribute keys (`"duration"`, `"error"`) are identical, the span
  name is `<Iface>.<Method>`, the metric names are
  `<package>.calls / .errors / .duration`, and the method-name
  attribute is the same. Span error recording uses the same
  `RecordError` + `SetStatus(codes.Error, err.Error())` calls.

- **One library upgrade rolls everywhere.** Bumping the slog field set
  or adding a new metric attribute is a `pkg/contractkit` change; no
  per-project regen is required to take it.

After pulling forge with the contractkit migration in place, run
`forge generate` once in your project to flip the four `*_gen.go`
files to the shim shape, then `go mod tidy` so
`github.com/reliant-labs/forge/pkg` is in your module's deps.

## Forge upgrade story (`forge_version` + per-version migration skills)

`forge.yaml` now records the forge binary version it was scaffolded
against under a top-level `forge_version` key, set automatically at
`forge new` time. The field is the project's *forge baseline* — the
version of the tool that owns the generated artifacts on disk right
now — and it is bumped by `forge upgrade` (never by `forge generate`).

### Why it exists

Three breaking template/library shape changes shipped in close
succession (contractkit, auth, tenant). Without a recorded baseline
there is no way for a project to know which migration playbooks apply.
Pinning the forge version turns each release-boundary into something
the tool can detect and the LLM can act on.

### How `forge generate` consumes it

Every `forge generate` run compares `cfg.ForgeVersion` against
`buildinfo.Version()` and prints a one-line warning when they differ:

```
⚠️  forge.yaml declares forge_version: 1.4.0 but binary is 1.6.0. Run 'forge upgrade' to migrate.
```

Legacy projects (no `forge_version` field) get a slightly different
nudge:

```
⚠️  no forge_version declared in forge.yaml — run 'forge upgrade' to set baseline (binary is 1.6.0).
```

The check is silent when the binary reports `dev`/`(devel)` so local
forge development against in-tree projects doesn't spam the console.

### How `forge upgrade` consumes it

`forge upgrade` (and `forge upgrade --to <version>`) walks the
embedded skill registry for any `migration/v<from>-to-<feature>`
skill whose `from` prefix matches the project's current
`forge_version` major family. Matching skills are surfaced before any
destructive work runs, with `forge skill load <path>` instructions
inline. The deterministic migration steps (regen, build) run as part
of the upgrade itself; the manual steps are left for the LLM to
follow from the loaded skill.

After a successful, non-dry-run upgrade, `forge_version` is bumped to
the target.

### Per-version migration skill convention

Skills live at `internal/templates/project/skills/forge/migration/v<from>-to-<feature>/SKILL.md`.
The canonical example is `migration/v0.x-to-contractkit`. Every skill
follows the same six-section shape — see the `migration/upgrade`
skill for the convention reference.

Workaround for the "dots in skill paths" concern: Go's `embed` package
and forge's skill registry both treat dotted directory names as
opaque strings, so `v0.x-to-contractkit` works as a skill path with
no special handling. The `forge skill load` and `forge skill list`
commands accept dots verbatim.

### Upgrade path complete (2026-04-30)

The five per-version migration skills covering every shape change
shipped this session are now in the registry:

- `migration/v0.x-to-contractkit` — mock / middleware / tracing /
  metrics codegen → `forge/pkg/contractkit` shim.
- `migration/v0.x-to-observe-libs` — per-package wrapper codegen →
  `forge/pkg/observe` Connect interceptors.
- `migration/v0.x-to-crud-lib` — `handlers_crud_gen.go` inline
  lifecycle → `forge/pkg/crud` delegation shims.
- `migration/v0.x-to-pack-starter-split` — stripe / twilio /
  clerk-webhook demoted from packs to one-time-copy starters.
- `migration/v0.x-to-env-config` — hand-curated KCL env-var groups →
  `forge.yaml environments[].config` + sensitive-field projection.

`forge upgrade --dry-run` from a `0.0.0`-pinned project surfaces all
five with their `forge skill load <path>` commands. The deterministic
parts (regen, build) run as part of the upgrade itself; the manual
steps live in the skills for the LLM to follow.

## Publish cycle: `forge/pkg` extractions need a release tag or a `replace`

When forge extracts a new helper into `forge/pkg/<x>` (contractkit,
auth, tenant, tdd, …), generated user code immediately starts
importing `github.com/reliant-labs/forge/pkg/<x>`. Until that path is
available at the project's pinned `forge/pkg` version, `go build`
fails with `module ... found, but does not contain package
.../pkg/<x>`.

Two ways to unblock the project:

1. **Tag a `forge/pkg` release.** Bump `forge/pkg`, push a
   `pkg/v<n+1>.<m>.<p>` tag, then update the project's `go.mod` to
   the new version. This is the only option for downstream consumers
   that don't sit in the same monorepo.
2. **Add a project-level `replace` directive.** During the
   intermediate state (forge has the new package locally; nothing has
   been tagged yet), point the project at the local checkout:

   ```
   // REPLACE: temporary until forge/pkg is released with <x>.
   //          Remove this once forge/pkg @ v<next> is tagged.
   replace github.com/reliant-labs/forge/pkg => /path/to/forge/pkg
   ```

   Every developer / CI runner needs the same checkout path, so
   prefer a relative path inside the same monorepo when possible.

This applies whether the extraction is a new package or new symbols
in an existing one. `forge generate` cannot detect the version skew —
it emits the import unconditionally — so the publish cycle is a
human-coordinated step.

Recent example: control-plane-next pinned
`forge/pkg v0.0.0-20260427223930-…`, which predated the
`contractkit / auth / tenant / tdd` packages. We added the local
`replace` directive in continuation 7 so the migration's intermediate
state could ship to git and CI could verify the project still
builds; the directive is annotated with the "remove once tagged"
comment above and will be deleted as part of the `forge/pkg v0.7.0`
release prep.

## Operational fixes (continuation 7)

Three small fixes that surfaced during the contractkit / auth / tenant
migration:

1. **`contract_test.go` scaffold** —
   `internal/cli/generate_middleware.go` was passing only the leaf
   directory name to the contract-test template, so nested packages
   (e.g. `internal/mcp/database`) imported `<module>/internal/database`
   and failed to compile. Multi-interface packages also got a scaffold
   that called the wrong constructor. Fixed by passing the full
   module-relative `ImportPath` to the template and skipping the
   scaffold entirely for multi-interface packages (one `ℹ️` log line
   tells the user to write the tests manually).

2. **Webhook `webhook_routes_gen.go` `//go:build ignore`** — false
   positive in the codegen audit. The renderer
   (`templates.stripBuildIgnore` inside `RenderFromFS`) drops the
   directive at write time, so user projects' webhook routes do
   compile. CODEGEN_AUDIT.md updated.

3. **`replace` directive in control-plane-next** — see the
   "Publish cycle" section above.

## Forge as a forge project — post-session cleanup (30-04-2026)

After the cutover that made forge itself a forge-managed project, a
verification pass surfaced two latent issues that were fixed in
place rather than left for users to discover.

### `contracts.exclude` widened for the two new analyzer linters

`forge.yaml`'s `contracts.exclude` originally listed three
analyzer-style sub-packages (`internal/linter/{contract,dblint,migrationlint}`).
The session shipped two more under the same convention —
`internal/linter/forgeconv` (the proto convention analyzer) and
`internal/linter/frontendpacklint` (the frontend pack convention
analyzer) — both using the Go analysis framework's exported-`Analyzer`
package-var idiom. Without the exclusion, `contractlint` flagged
each as "exported methods but no contract.go". Both were added to
`contracts.exclude` with a one-line rationale per package.

`forge_version: 0.0.0` and `kind: cli` were verified present (the
upgrade-story agent had already added the field; the cutover agent
had set the kind).

### `forge lint --contract` now honours go.work for self-hosted modules

`internal/cli/lint.go` previously forced `GOWORK=off` and
`GOFLAGS=-mod=mod` when running the contract analyzer subprocess.
This works for any project that doesn't ship its own `forge/pkg/<x>`
(the common case), but breaks when the project IS the source of those
packages — forge itself wires `forge/pkg` in via a top-level
`go.work`, and turning workspace mode off causes the analyzer to
fall back to the published `pkg @ v0.0.0-...` version which predates
contractkit/auth/tenant/tdd.

The fix: a new `hasWorkspaceGoMod()` helper walks up from cwd looking
for a `go.work`. When present, the GOWORK/GOFLAGS overrides are
skipped (workspace mode is incompatible with `-mod=mod` anyway). The
GOPROXY default is still applied, so transitive dependency fetches
still work.

### contract_test.go scaffold respects non-canonical interface names

The contract scaffolder was emitting `pkg.New(pkg.Deps{})` for any
single-interface package, regardless of the interface's name. Forge's
own `internal/packs/` declares a `Manager` interface (not `Service`),
which doesn't have a `New(Deps) Manager` constructor — the resulting
scaffold was a compile-error against the package's actual API.

Fixed in `internal/cli/generate_middleware.go`: when a single-interface
contract names something other than `Service`, the scaffold is
skipped with a one-line `ℹ️` message ("interface %q is not the
canonical Service shape; write tests manually"). The previous
multi-interface skip path is unchanged.

### Verification output

After the three fixes:

- `forge lint` — all linters green.
- `forge lint --conventions` — pass.
- `forge lint --frontend-packs` — pass.
- `forge generate` — idempotent on repeated invocation; the only
  remaining "noise" is the `go mod tidy` step, which can't resolve
  the local `forge/pkg` module without workspace mode (Go's `mod
  tidy` is single-module). The build/test/lint pipeline is green; the
  tidy is a known-cosmetic artifact of the chicken-and-egg.
- `forge upgrade --dry-run` — surfaces `migration/v0.x-to-contractkit`
  and lists ~23 candidate file updates (the migration's regen step,
  cleanly enumerated).
- `forge skill list | wc -l` — 37 skills.

## Per-environment config (forge.yaml + sibling files + KCL gen)

Forge collapses the env-var-soup pattern that grew in
`deploy/kcl/base.k` (NATS_ENV, STRIPE_ENV, AUTH_ENV, ...) into a
generated per-env module emitted by `forge generate`.

### What changed

1. **Proto annotations gained two fields**:
   - `sensitive: bool` — projects the field to a Kubernetes Secret
     (`secret_ref`-shaped EnvVar) instead of an inline value.
   - `category: string` — groups related fields under a named
     `<CATEGORY>_ENV` list (e.g. `category: "stripe"` → `STRIPE_ENV`).
   These are additive (field numbers 6 and 7 on `ConfigFieldOptions`);
   no breaking change.
2. **forge.yaml** gained `environments[<name>].config` — a per-env
   key-value map keyed by proto field name (snake_case). Use this for
   dev/staging values that aren't secret. Sensitive values can be
   `${secret-ref-name}` strings to override the default secret name.
3. **Sibling files** `config.<env>.yaml` next to forge.yaml are merged
   on top of the inline map, sibling-wins. Use this for prod where you
   don't want non-secret toggles cluttering forge.yaml.
4. **`forge generate`** emits `deploy/kcl/<env>/config_gen.k` for every
   env. The hand-edited `main.k` for each env imports it and
   concatenates `cfg.APP_ENV + cfg.<CATEGORY>_ENV` into
   `Application.env_vars`.
5. **`forge run --env <env>`** exports the merged per-env config as
   process env-vars to the running binary. Sensitive fields are
   skipped (developers set them locally via direnv / .env).
6. **`forge deploy <env>`** passes non-sensitive scalars to KCL via
   `-D key=value`.

### Migration recipe (control-plane shape)

A project with a hand-curated DB_ENV / NATS_ENV / STRIPE_ENV / etc. in
`deploy/kcl/base.k`:

1. Add `sensitive: true` to every secret-backed field in
   `proto/config/v1/config.proto`.
2. Add `category: "<bucket>"` to fields you want grouped (e.g.
   `category: "stripe"` for STRIPE_API_KEY, STRIPE_WEBHOOK_SECRET).
3. Run `forge generate`. Inspect `deploy/kcl/<env>/config_gen.k`.
4. Replace `env_vars = base.DB_ENV + base.NATS_ENV + base.STRIPE_ENV +
   ...` in each `main.k` with `env_vars = cfg.APP_ENV + cfg.STRIPE_ENV
   + ...`.
5. Delete the hand-curated lambdas from `base.k` once the new shape
   compiles cleanly.

### Smoke verification

```bash
forge new envtest --mod github.com/example/envtest --kind service --service api
cd envtest
# Edit proto/config/v1/config.proto: add sensitive: true to database_url.
# Edit forge.yaml prod env to add `database_url: ${prod-db-credentials}`.
forge generate
cat deploy/kcl/prod/config_gen.k    # database_url uses secret_ref = "prod-db-credentials"
grep -r "DATABASE_URL" deploy/kcl/  # only secret_ref shape, no hardcoded env-var groups
```

## Operators: `forge/pkg/controller` + `forge add crd` (30-04-2026)

Operator scaffolding split into two commands and offloaded most of
the lifecycle into a runtime library:

- **`forge/pkg/controller`** — the controller-runtime sibling of
  `forge/pkg/contractkit`. Carries a generic `Reconciler[T client.Object]`
  base, `Done/Requeue/Stop` Result helpers, common predicates
  (`SkipDeletion`, `HasAnnotation`, `HasLabel`, `AnnotationChanged`),
  the multi-cluster `ClusterClientManager` lifted from
  `control-plane-next/operators/workspace_controller/`, a
  capped-exponential `Backoff` helper, and a small `controllertest`
  envtest harness that skips cleanly when binaries are missing.
- **`forge add operator <name>`** — emits only the operator package
  scaffold (`doc.go` + `operator.go` with the `New / Deps /
  Controller / SetupWithManager / AddToScheme` symbols expected by
  `pkg/app/bootstrap.go`). Pass `--with-placeholder-crd` to keep
  the legacy combined `types.go + controller.go +
  controller_test.go` shape — same flag set as before with
  `--api-package` and `--crd-type` accepted underneath.
- **`forge add crd <Name>`** — emits `api/<version>/<name>_types.go`
  + `operators/<operator>/<name>_controller.go` +
  `<name>_controller_test.go`. The controller is a thin shim that
  embeds `controller.Reconciler[*v1alpha1.<Name>]` and implements
  `ReconcileSpec` + `FinalizeSpec`. Three shapes: `state-machine`
  (default — Spec.State drives observable phases), `config` (no
  state, declarative-only), `composite` (owns sub-resources).

Multiple CRDs in the same version package coexist via a shared
`api/<version>/groupversion.go` (created once on the first
`forge add crd` run) plus per-type `init()`-time
`SchemeBuilder.Register` calls. Test function names are CRD-prefixed
(`TestWorkspaceReconcile_NotFound`, `TestDatabaseReconcile_HappyPath`)
so multiple CRDs in the same operator package don't collide.

The operator's `Controller` struct now holds a `Reconcilers
[]ManagerSetup` slice that per-CRD reconcilers append themselves to
in `cmd/<op>/main.go` (or `pkg/app/setup.go`). The bootstrap-emitted
`SetupWithManager` walks the slice. This keeps the bootstrap template
shape unchanged — `Controller`, `Deps`, `New`, `AddToScheme`, and
`SetupWithManager` all exist on the generated operator package — while
allowing the per-CRD reconcilers to live in their own files with
their own deps.

The `ServiceConfig` block in `forge.yaml` gained `group`, `version`,
and `crds:` fields. `crds` is a list of `{name, group, version, shape}`
entries; `forge add crd` appends to it after generating the files.
Future `forge generate` runs can use this as the source of truth
for re-rendering scheme registration etc.

Library deps: `sigs.k8s.io/controller-runtime v0.23.3` plus the
matching `k8s.io/api / apimachinery / client-go / apiextensions-apiserver`
versions are now part of `forge/pkg`'s `go.mod`. Projects using only
`forge/pkg/contractkit / auth / tenant / tdd / orm` won't pull these
in (they're only direct deps of `pkg/controller`).

Smoke-tested via the standard sequence:

```bash
tmp=$(mktemp -d) && cd "$tmp"
forge new opt --mod github.com/example/opt --kind service --service api
cd opt
# Add the local replace until forge/pkg gets a release tag with the controller package:
echo 'replace github.com/reliant-labs/forge/pkg => /home/sean/src/reliant-labs/forge/pkg' >> go.mod
go mod tidy
forge add operator manager --group reliant.dev --version v1alpha1
forge add crd Workspace --shape state-machine --operator manager
forge add crd Database  --shape config        --operator manager
forge add crd Cluster   --shape composite     --operator manager
go mod tidy
go build ./...
go test ./operators/...
```

All three shapes generate, build, and pass their fake-client unit
tests. envtest-backed coverage is opt-in: the unit-test scaffold uses
the fake client, and tests gated on `controllertest.New(t)` skip
gracefully when envtest binaries aren't installed.

## Introspection commands landed 2026-04-30

Three new top-level commands give an LLM (or human) a fast way to orient
in a forge project without grepping ten directories:

- `forge audit` — comprehensive project snapshot. Categories: forge
  version pin, project shape (services + RPC counts, workers,
  operators, frontends, packs), convention compliance roll-up,
  codegen state (last generate, hand-edits to forge-space files,
  orphan `_gen` files), pack health, proto-vs-migration alignment,
  FORGE_SCAFFOLD marker counts, deps health. `--json` for sub-agents.
- `forge map` — annotated project tree. Each path is labelled
  `[user-owned]`, `[forge-space, regenerated]`, `[scaffold,
  FORGE_SCAFFOLD markers present]`, or `[forge-space, hand-edited
  (drift from regen)]`. `--depth N`, `--filter <subtree>`, `--json`.
- `forge generate --explain` — provenance log printed after the
  pipeline runs. For each tracked output: source proto / contract.go /
  entity, the reason it was rewritten or skipped (idempotent no-op).

Use these aggressively from sub-agents. `forge audit --json |
jq '.categories.codegen'` is a one-liner for "is this project's
generated state clean?"; `forge map --filter handlers/` is a
one-liner for "what files in handlers/ are mine vs forge's?".

## Forge ownership boundary sharpened 2026-04-30

The single biggest brittleness fix for LLM-driven forge work: every
scaffolded or generated file now carries an unambiguous signal about who
owns it. Three classes:

1. **Pure forge-space** (`_gen` suffix, `// Code generated by forge. DO
   NOT EDIT.` header, regenerated every `forge generate`). The header is
   uniform across every template: a "DO NOT EDIT" line, a `// Source: …`
   pointer at the input that drove the file, and a `// To customize: …`
   pointer at the user-owned sibling. Templates updated:
   `mock_gen.go`, `middleware_gen.go`, `metrics_gen.go`, `tracing_gen.go`
   (the four contract decorator templates in `internal/generator/contract/`),
   `service/handlers_gen.go.tmpl`, `service/handlers_crud_gen.go.tmpl`,
   `service/authorizer_gen.go.tmpl`, `middleware/auth_gen.go.tmpl`,
   `middleware/tenant_gen.go.tmpl`, `webhook/webhook_routes_gen.go.tmpl`,
   `project/bootstrap.go.tmpl`, `project/bootstrap_testing.go.tmpl`,
   `project/config.go.tmpl`, `project/migrate.go.tmpl`,
   `project/cmd-cli-main.go.tmpl`, `project/cmd-cli-version.go.tmpl`.

2. **Scaffold-with-placeholders** (`FORGE_SCAFFOLD:` markers indicate
   the user's job). The `_gen` filename is preserved but the regen
   semantics are now "until-customized": as long as any `FORGE_SCAFFOLD:`
   comment remains, forge regenerates the file; the moment every marker
   is removed the file becomes user-owned. Currently only
   `handlers/<svc>/handlers_crud_gen_test.go` and
   `handlers/<svc>/handlers_crud_integration_test.go` use this — they
   ship per-RPC test frames the user is supposed to fill in. The
   write-skipping logic lives in `internal/codegen/crud_gen.go`'s new
   `writeScaffoldFile` helper.

3. **Pure user-space** (scaffolded once, never touched again). Three
   templates now carry a single `FORGE_SCAFFOLD:` line on the
   placeholder bodies the user is expected to replace:
   `service/handlers.go.tmpl` (per-RPC `// FORGE_SCAFFOLD: implement
   business logic` comments), `project/setup.go.tmpl` (the empty-body
   "wire your custom dependencies here" line), and
   `service/handlers_crud_integration_test.go.tmpl` (header-level review
   prompt).

### New lint analyzer: `forge lint --scaffolds`

Lives at `internal/linter/scaffolds/`. Two rules:

- **scaffold-not-customized** (error): a file contains a
  `FORGE_SCAFFOLD:` marker. If the user committed it, they didn't finish
  the scaffold.
- **gen-missing-header** (error): an `_gen.go` file is missing the
  canonical `// Code generated by forge. DO NOT EDIT.` line.
- **gen-missing-source** (warning): an `_gen.go` file is missing the
  `// Source: …` pointer.

The walk skips heavyweight directories (`gen/`, `node_modules/`,
`.git/`, `vendor/`, `bin/`, `dist/`, `.forge/`, `.next/`). Wired into
`forge lint` (runs alongside the other linters) and accessible via
`forge lint --scaffolds` for targeted runs. Test fixtures under
`internal/linter/scaffolds/testdata/{clean,scaffold_marker_present,
gen_missing_header,gen_missing_source}`.

### Verification

The standard smoke sequence catches the integration:

```bash
tmp=$(mktemp -d) && cd "$tmp"
forge new test --kind service --service api --frontend web
cd test
forge generate
grep "FORGE_SCAFFOLD" handlers/api/*_test*.go   # markers present
forge lint --scaffolds                          # fires (scaffold-not-customized)
sed -i 's|// FORGE_SCAFFOLD:.*||g' handlers/api/handlers_crud_gen_test.go
forge lint --scaffolds                          # passes
```

The full ownership table lives in `forge/FORGE_OWNERSHIP_AUDIT.md`.

## Pack → starter split for business integrations (2026-04-30)

Three packs that drifted into business-integration territory got
demoted to starters: **stripe**, **twilio**, and the **clerk webhook
user-sync** half of the clerk pack. The JWT/JWKS auth side of clerk
stays a pack — that is pure infrastructure and benefits from forge
keeping it up to date.

### Why split

Per-project divergence on these three was 100% — every control-plane,
every dogfood, every migration we touched rewrote the
emitted-by-forge stripe / twilio / clerk-webhook code. Centrally
maintaining business logic creates more bugs than it prevents (this
session alone bit us with stripe proto pkg, twilio template-escape,
clerk svix import).

The split:

| Concern | Status |
|---------|--------|
| `jwt-auth`, `firebase-auth`, `api-key`, `audit-log`, `nats`, `auth-ui`, `data-table` | **stay packs** — pure infrastructure |
| `clerk` (JWKS validator, dev-mode bypass, Connect interceptor) | **stays a pack** — auth-side is infrastructure |
| `stripe` | **starter** — `forge starter add stripe --service billing` |
| `twilio` | **starter** — `forge starter add twilio --service notifications` |
| `clerk` webhook user-sync (`pkg/clerk/webhook.go`) | **starter** — `forge starter add clerk-webhook --service api` |

Starters live at `internal/starters/<name>/` with a tiny
`starter.yaml` (name, description, files, deps, notes). No
migrations, no install lifecycle, no `forge.yaml` tracking. The
command copies files and exits; the user owns the code from the first
byte forward.

### Migrating an existing project that has stripe / twilio installed

If an existing `forge.yaml` lists `stripe:` or `twilio:` under
`packs:`, the new forge build will warn at generate time
(`Warning: installed pack "stripe" not found`) and skip the entry.
The user's existing handler code is untouched — forge does not own it.

To clean up:

```yaml
# forge.yaml — drop these lines
packs:
- stripe       # ← remove
- twilio       # ← remove
```

Re-run `forge generate`. The user's existing pack-emitted code
(`pkg/clients/stripe/`, `pkg/clients/twilio/`, etc.) stays in place —
it was already customized in every project, so leaving it as user code
is the right call. Going forward, `forge starter add <name>` is the
escape hatch when a clean re-scaffold is needed.

### Verification

```bash
forge pack list      # no stripe / twilio entries; clerk still listed
forge starter list   # stripe, twilio, clerk-webhook all present

tmp=$(mktemp -d) && cd "$tmp"
forge new starttest --mod github.com/example/starttest --kind service --service api
cd starttest
forge starter add stripe --service api
ls pkg/clients/stripe/    # client.go, webhook.go present
go get github.com/stripe/stripe-go/v82 && go mod tidy && go build ./...
```

The starter notes echo every Go dep the user needs to add — forge no
longer auto-runs `go get` / `go mod tidy` for these scaffolds, because
dependency-version churn is the user's call.

## observability codegen → observe.* libs (30-04-2026)

**What changed.** The per-internal-package `middleware_gen.go`,
`tracing_gen.go`, and `metrics_gen.go` files are gone. Observability
is now a Connect interceptor concern at the handler boundary
(`forge/pkg/observe.LoggingInterceptor` / `TracingInterceptor` /
`MetricsInterceptor` / `RecoveryInterceptor` / `RequestIDInterceptor`)
plus opt-in helpers for inner-call instrumentation
(`observe.LogCall` / `observe.TraceCall` / `observe.NewCallMetrics`).
The canonical chain is one call: `observe.DefaultMiddlewares(deps)`.

**Why.** Per-method observability codegen at the package boundary was
the wrong granularity. Most observability needs are request-scoped;
Connect interceptors handle them once at the edge. Inner-call
observability is rarely needed — when it is, the user can opt in at
the explicit call site rather than paying for blanket wrappers.

**Migration.** Run `forge generate` once. The contract generator
sweeps stale wrappers from every directory it (re)generates a mock
in. `pkg/app/bootstrap.go` and `cmd/server.go` are regenerated to
drop the per-package `NewTracedService` / `NewMetricService` calls
and adopt `observe.DefaultMiddlewares`. Hand-written code that
referenced `Instrumented<Iface>` / `Traced<Iface>` / `Metric<Iface>`
types must be updated to use the bare interface — search:

```bash
grep -rn "Instrumented\|TracedService\|MetricService" --include="*.go" .
```

The mock side (`mock_gen.go`) is unchanged; existing tests keep
working as-is. See `migration/v0.x-to-observe-libs` for the full
upgrade story.

### Verification

```bash
tmp=$(mktemp -d) && cd "$tmp"
forge new obstest --mod github.com/example/obstest --kind service --service api
cd obstest
forge add package emailer
forge generate
ls internal/emailer/  # mock_gen.go only — no middleware/tracing/metrics_gen.go
grep DefaultMiddlewares cmd/server.go    # canonical chain wired
```

## MultiServiceApplication — collapsing image builds (2026-04-30)

For projects shipping one Go binary that exposes multiple cobra
subcommands (one-binary, many-services pattern — control-plane-next
ships 11 services this way), use `MultiServiceApplication` in
`deploy/kcl/<env>/main.k` instead of repeating `Application` blocks.
The image build/push runs once; each Deployment selects its behaviour
via `args:` (the cobra subcommand).

```kcl
import deploy.kcl.schema
import deploy.kcl.render

multi = schema.MultiServiceApplication {
    name = "platform"
    image = "platform"
    command = ["/usr/local/bin/platform"]
    shared_env_vars = base.OTEL_ENV
    services = [
        schema.SubCommandService {
            name = "admin-server"
            args = ["server", "admin-server"]
            ports = [schema.ServicePort {port = 80, target_port = 8080}]
        }
        schema.SubCommandService {
            name = "billing"
            args = ["worker", "billing"]
            replicas = 2
        }
        # ... one entry per service
    ]
}

env = schema.Environment {
    name = "dev"
    namespace = "platform-dev"
    image_registry = registry
    image_tag = image_tag
    applications = render.multi_service_apps(multi)
}

manifests = render.render_environment(env)
```

`render.multi_service_apps(multi)` expands one MultiServiceApplication
into `{name: Application}` — one child Application per
SubCommandService, all sharing the parent's `image:`. Each child
inherits the parent's `command` / `shared_env_vars` / `labels` /
`annotations`, then layers in per-service overrides.

When to reach for it:

- One Go binary, N cobra subcommands (or `forge run server <svc>`
  shape — same thing on the runtime side).
- You want one image build/push instead of N.
- Per-service replicas/resources/env still need to differ.

When to skip it:

- Different binaries per service — keep them as separate
  `Application` entries.
- Single service — `Application` directly is simpler.

The Go-side codegen mode (`binary: shared` in `forge.yaml`, generated
cobra-subcommand-per-service, bootstrap-per-service) is deferred (see
`FORGE_BACKLOG.md`). The runtime infrastructure to run a subset of
services from one binary already exists today via
`Bootstrap`/`BootstrapOnly` — `cmd-server.go` accepts
`server [services...]` and only mounts the requested ones. The MSA
schema is the deploy-side counterpart.

## CRUD codegen → `forge/pkg/crud` (30-04-2026)

**What changed.** `handlers/<svc>/handlers_crud_gen.go` no longer
inlines the auth check, tenant resolve, error envelope, cursor
encode/decode, page-size clamp, and order-by validation per RPC.
Each generated method is now a single delegation:

```go
func (s *Service) CreateUser(ctx context.Context, req *connect.Request[pb.CreateUserRequest]) (*connect.Response[pb.CreateUserResponse], error) {
    return crud.HandleCreate(crud.CreateOp[pb.CreateUserRequest, pb.CreateUserResponse, *db.User]{
        EntityLower: "user",
        Auth:        func(ctx context.Context) error { /* GetUser + Authorizer.Can */ },
        Tenant:      middleware.RequireTenantID,                        // omitted when entity isn't tenant-scoped
        Entity:      func(req *pb.CreateUserRequest) *db.User { return &db.User{Name: req.Name, Email: req.Email} },
        Persist:     func(ctx context.Context, tid string, e *db.User) error { return db.CreateUser(ctx, s.deps.DB, e, tid) },
        Pack:        func(e *db.User) *pb.CreateUserResponse { return &pb.CreateUserResponse{User: e} },
    })(ctx, req)
}
```

**Why.** The CRUD lifecycle is canonical and shared across every
project. Inlining it per RPC meant every change to error mapping or
pagination semantics required re-rendering every project. Lifting it
into `pkg/crud` lets behaviour evolve in one library upgrade. The
per-entity bits that *can't* be generic — proto→entity field copy,
`db.<Name>` call site, response field name — stay in the generated
shim where forge can see the proto descriptor.

**Behavioural fingerprints preserved verbatim:**

- `"<op> <entity>: %w"` envelope at `CodeInternal` (Create/List/Update/Delete).
- Same envelope at `CodeNotFound` for Get.
- `"invalid page token"` at `CodeInvalidArgument` when `page_token` doesn't decode.
- `"update <entity>: <field> is required"` at `CodeInvalidArgument` when the request entity is nil.
- Default page size 50, max 100, `+1` fetch + trim.
- Order-by validation via `orm.ValidateOrderBy`, default `<pk> ASC` ordering.

**Migration.** Run `forge generate` once. No code changes needed for
hand-written handlers — those are still skipped by the gen pipeline
when they exist in user-owned files (the per-service "user-space wins"
boundary). The escape hatch is unchanged: implement the same RPC by
hand in a sibling file and the gen output for that method drops out.

**Verification.**

```bash
tmp=$(mktemp -d) && cd "$tmp"
forge new crudtest --mod github.com/example/crudtest --kind service --service api
cd crudtest
# Add a User entity with CRUD RPCs
forge generate
wc -l handlers/api/handlers_crud_gen.go     # ~30-40 lines per RPC, including struct-init wiring
go build ./...
```

## binary: shared — Layer B (2026-04-30)

`binary: shared` is now a first-class codegen mode (Layer B of the
`MultiServiceApplication` dispatch — Layer A, the KCL schema + helper,
shipped in the previous session). Multi-service projects that want
one image and one cobra subcommand per service can now opt in at
scaffold time:

```bash
forge new my-project --mod github.com/example/my-project \
  --kind service --binary shared \
  --service api --service worker --service billing
```

This produces `cmd/main.go` (cobra root), `cmd/api.go`, `cmd/worker.go`,
`cmd/billing.go` (per-service subcommand wrappers), a `pkg/app/bootstrap.go`
whose `BootstrapOnly` lazily constructs services per-name, and
`deploy/kcl/<env>/main.k` files that emit a single
`MultiServiceApplication` instead of N `Application` blocks.

**Migrating an existing project.** Set `binary: shared` in `forge.yaml`
and run `forge generate`. Tier-1 file regeneration replaces
`cmd/main.go` with the shared-binary cobra root and emits per-service
files. KCL deploy templates are re-rendered with the
`MultiServiceApplication` shape. Per-service cobra files
(`cmd/<svc>.go`) are scaffolded once and not re-rendered by subsequent
`forge generate` invocations — they're mechanical (single delegate
call) and safe to hand-edit.

**Trade-offs (recap).** One image build instead of N (cuts CI time on
multi-service projects roughly proportional to service count). All
services ship the same image SHA per release — bug fixes in service A
roll service B's code at the same time. Per-service Deployments still
exist, so independent scaling/replicas are unchanged. See
`migration/v0.x-to-binary-shared/SKILL.md` for the full migration
guide.

**Where the codegen branches:**

- `internal/config/config.go` — `Binary string`, `EffectiveBinary()`,
  `IsBinaryShared()`, `ProjectBinaryPerService` / `ProjectBinaryShared`
  constants.
- `internal/cli/new.go` — `--binary` flag, `validateNewArgs` returns
  the normalized binary mode, `runNew` plumbs it through.
- `internal/generator/project.go` — `Binary` + `AdditionalServices`
  fields on `ProjectGenerator`; `isBinaryShared()` gates the cmd/
  template choice and per-service `cmd/<svc>.go` emission.
- `internal/generator/upgrade.go` — `managedFilesForCfg(cfg)` swaps
  `cmd-root.go.tmpl` for `cmd-shared-main.go.tmpl` based on
  `cfg.EffectiveBinary()`. Tier-1 regeneration via `RegenerateInfraFiles`
  honors this so generate cycles don't clobber the shared-binary main.
- `internal/generator/project_deploy.go` — `binary: shared` selects
  the `kcl/<env>/main-shared.k.tmpl` variants and passes the full
  service list as KCL `services:` entries.
- `internal/templates/project/bootstrap.go.tmpl` — `BinaryShared`
  branch in `BootstrapOnly` that constructs services lazily inside
  their name-gated blocks. Per-service mode keeps the prior
  "all-services constructed, mux-filter at registration" shape.
- `internal/templates/project/cmd-shared-main.go.tmpl`,
  `cmd-shared-service.go.tmpl` — new templates for the cobra root +
  per-service subcommand stubs.
- `internal/templates/deploy/kcl/{dev,staging,prod}/main-shared.k.tmpl`
  — KCL `MultiServiceApplication` per-env variants. Each renders the
  full project's service list as `SubCommandService` entries via
  `render.multi_service_apps(...)`.

**Verification.**

```bash
tmp=$(mktemp -d) && cd "$tmp"
forge new shared1 --mod github.com/example/shared1 \
  --kind service --service api --service billing --binary shared
cd shared1
ls cmd/                       # main.go + api.go + billing.go + server.go
grep "binary:" forge.yaml     # binary: shared
kcl run deploy/kcl/dev/main.k > /tmp/m.yaml
grep "image:" /tmp/m.yaml | sort -u   # one distinct image (shared1)
grep "name: shared1-api\|name: shared1-billing" /tmp/m.yaml  # both Deployments
```

A sibling pre-existing-condition (the workspace `go mod tidy` issue
documented in the backlog) means the scaffolded project may need a
manual `go mod edit -replace=github.com/reliant-labs/forge/pkg=...`
during local dev iteration — unrelated to Layer B.
