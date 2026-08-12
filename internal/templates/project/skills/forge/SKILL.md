---
name: forge
description: Start here, and this is enough. The end-to-end greenfield sequence, the entity-authoring rules, what scaffold births, the four commands that print forge's whole surface, and the pre-flight checklist that stops you hand-writing what forge already scaffolds.
---

# Forge — start here

Self-sufficient for greenfield work: empty directory to a running app without
loading another skill. Depth is named at the point you would need it — load it
then, not before.

## Greenfield, end to end

```bash
forge project new my-app --mod github.com/acme/my-app \
  --service customers --service jobs --service estimates --frontend web
cd my-app
# author each service proto: entity messages + custom rpcs (rules below)
forge scaffold --dry-run     # print the plan
forge scaffold               # birth entities, then generate
forge lint
go build ./...
forge run                    # host services + frontends, on a fresh seeded DB
```

### Name every service up front

`forge project new --service a --service b --frontend web` writes
`cmd/<bin>/main.go` with every service constructor already in the
`cmd.Execute(...)` arg list.

A later `forge scaffold service <name>` appends its constructor to that same
call. `main.go` stays owned code, so the append is surgical and forge declines
rather than guesses when it cannot find the `cmd.Execute(...)` call — then it
prints the exact import and argument to add by hand.

Batch the ones you discover later: `forge scaffold service sales jobs billing`
runs the generate pipeline once for all three instead of once each.

A bare `forge project new` scaffolds **zero services**: binary shell, `internal/app`
composition root, buf/proto scaffolding, Taskfile/CI/deploy. The binary is a
deployment unit that mounts services, not a domain entity, so forge never invents
a `<project>Service` from the binary name.

### The `project new` flags that matter

| Flag | Purpose |
|---|---|
| `--mod` | **required.** Go module path (`github.com/acme/my-app`) |
| `--service <name>` | repeatable or comma-separated. Name after a domain entity, never after the binary |
| `--frontend <name>` | repeatable or comma-separated Next.js frontends |
| `--in-place` | scaffold into the current directory; takes no positional name |
| `--name` | names an `--in-place` project whose directory is a worktree or branch name rather than the product |
| `--path <dir>` | parent directory for the project (default `.`) |

Everything else has a working default — `--kind` (`service` / `cli` / `library`,
each with its own `features:` block), `--harness`, `--disable`, `--go-version`,
`--force`: `forge project new --help`.

## Authoring a service proto

Three rules cover what agents get wrong.

### 1. Mark entities, and let scaffold inject their CRUD

Give the message a leading `// forge:entity` comment. Bare `forge scaffold`
injects the missing CRUD quintet (Create/Get/Update/Delete/List) into the
service proto, writes the create-table migration pair, and declares the managed
`id` / `created_at` / `updated_at` fields.

```protobuf
// forge:entity
message Customer {
  string name = 2;
  string email = 3 [(buf.validate.field).string.email = true];
}
```

There is no CLI field grammar — enums, `optional` (nullable), `buf.validate`
rules, `forge:read-only` / `forge:secret` / `forge:append-only` and foreign-key
references are all spelled in the message. Full vocabulary:
`forge project annotations`.

`protovalidate` vendors itself: add `import "buf/validate/validate.proto";` and
`forge generate` pulls the protos into your `proto/` tree on the first run that
imports them. Fixed-length codes take an exact rule — `string.len = N` (ISO
currency 3, country 2) or `string.const` — because `buf lint` rejects
`min_len == max_len`.

### 2. Every custom rpc is yours to declare

**Forge injects CRUD rpcs only.** Authoring a `FooRequest`/`FooResponse` message
pair does not create an rpc — nothing scans for orphan message pairs. If the
`service {}` block does not name it, the rpc does not exist, and you find out at
`go build`.

Declare it in the block yourself:

```protobuf
service EstimateService {
  // CRUD quintet injected here by `forge scaffold`
  rpc RecalculateEstimate(RecalculateEstimateRequest) returns (RecalculateEstimateResponse);
}
```

Then `forge scaffold` emits the pb-through stub (a method on `*Service`
returning Unimplemented) in `internal/handlers/<svc>/rpc_<name>.go`.
`forge scaffold rpc <svc> <Name>` does the same for one rpc, and prints a proto
snippet to paste when the rpc is not in the proto yet.

Every rpc is closed to unauthenticated callers unless it carries
`auth_required: false` (`proto`, `auth`).

### 3. Declare the authoritative route when two FKs reach one parent

When a table reaches the same parent two ways — `estimates.customer_id` directly
**and** `estimates.job_id → jobs.customer_id` — both foreign keys are valid and
the two routes can name different rows. Nothing in the schema says which wins,
so `forge db seed` refuses to write those rows until you declare it.

This shapes your entity model, so decide it while authoring the proto, not when
the seeder complains. Declare it as a comment on the FK constraint, in a
migration:

```sql
COMMENT ON CONSTRAINT estimates_customer_id_fkey ON estimates IS
    'forge:ref derived-from=job_id';
```

| declaration | meaning |
|---|---|
| `forge:ref derived-from=<column>` | the route leaving by `<column>` decides; this column is seeded from it |
| `forge:ref authoritative` | this column is the truth; the other edge is narrowed to agree |
| `forge:ref independent` | genuinely unrelated facts |

`authoritative` is usually right: the record states its own subject and the
longer route is a back-reference. Depth, and the cases forge resolves silently:
`db/seeding`.

## The shape, in four lines

- **SQL is the schema truth.** `db/migrations/*.up.sql` is the single source; entity structs, the ORM, CRUD wiring and frontend pages are PROJECTIONS of the applied schema. Do not declare schema in proto.
- **Proto is the wire truth.** Services, rpcs, messages. The two evolve on independent clocks; generated conversions map their intersection by name.
- **An entity is both halves**: CRUD rpcs in the proto AND the matching table. One half alone generates honest nothing.
- **Draft, then `forge generate`.** It is a fast, exact oracle that fails loudly. Never reverse-engineer forge's `internal/**` for syntax.

The application lives under `internal/`, nested by role: `internal/handlers/<svc>/`
(one service — owned and generated files in one directory), `internal/workers/<name>/`,
`internal/operators/<name>/`, `internal/app/` (owned `providers.go` + generated
`compose.go`), `internal/db/`. `pkg/` is the generated substrate the app imports;
`cmd/` is entrypoints only. Full tree and the composition root: `architecture`.

### The scaffolded frontend is starter code

The dashboard grid, sidebar nav, sign-in screen and generated CRUD pages exist
to prove the stack is wired — Rails scaffolding, in React. They are all yours to
rewrite, and rewriting them is expected. Forge withholds a visual identity
deliberately: an invented one is harder to replace than an obviously neutral
one. Before showing any of it to a user, load `frontend/design` — it asks for a
brief first and will not invent an aesthetic without one.

## Evolving the schema

Migrations lead; projections follow.

```bash
forge db migration new add_login_tracking    # creates the .up.sql/.down.sql pair
# write plain postgres DDL — postgres-pinned, so ::casts, JSONB and TEXT[] all work
forge generate                               # re-project the entity struct/ORM
forge db migrate up --dsn "$DATABASE_URL"    # apply against a live database
```

Generated conversions map the intersection of wire fields and columns by name: a
DB-only column never leaks onto the wire, a wire-only field never reaches the DB.
Add either side freely. Forge stores no ownership of its own — if rows belong to
someone, that is a column you write in the migration, and adding it later costs a
backfill. Depth: `db`.

## Implementing business logic

Write it in `internal/handlers/<svc>/`, on the `*Service` methods. A custom rpc is
a method on `*Service` working with the wire (pb) types and the `internal/db/` ORM
through `s.deps`. CRUD rpcs delegate to `pkg/crud` via the owned shims in
`handlers_crud.go`. There is no `contract.go`/interface split inside a handler
package — the `*Service` methods **are** the handlers. When an rpc's logic
deserves transport-free tests or cross-service reuse, scaffold a standalone
package (`forge scaffold package <name>`) and call it via `s.deps`. Depth: `api`,
`service-layer`, `architecture`.

## Four commands print forge's whole surface

Read these before deciding forge cannot do something. All four are enumerated
from forge's own command tree, recognizers, descriptors and package set, so they
cannot drift the way a document can.

| Command | Answers |
|---|---|
| `forge project capabilities` | **Every verb forge has**, with a one-line summary — plus every `forge lint` analyzer (including the ones hidden from `--help`) and every `// forge:*` marker. |
| `forge project annotations` | The entity-authoring vocabulary: how each proto field is born as a column, the `buf.validate` rules forge projects, and the `(forge.v1.service)` / `(forge.v1.method)` options. |
| `forge project libraries` | **Every `forge/pkg/*` library**, what each is for, and the absolute path to the source this project resolves. |
| `forge project audit --json` | What THIS project currently is: services, rpcs, entities, drift, stubs, ownership. |

Add `--json` to any of them to query rather than read.

**Never search the filesystem for forge's own source.** `forge project libraries`
gives you the directory, and `go doc <import path>` gives you any package's full
API without opening a file:

```
go doc github.com/reliant-labs/forge/pkg/svcerr
go doc github.com/reliant-labs/forge/pkg/svcerr Wrap
```

## Running it

| Command | What it does |
|---|---|
| `forge run` | Host services + frontends; auto-seeds a fresh dev DB on first boot |
| `forge env up dev` | Full stack: Docker infra + Go services (hot reload) + frontends |
| `forge env up <env>` | Build + deploy + host launch + frontend dev — reads `deploy/kcl/<env>/` |
| `forge env deploy dev` | Deploy to local k3d (or whatever dev's KCL targets) |
| `forge generate` | Re-project from protos + applied migrations. Safe anytime; never touches business logic |
| `forge lint` | Go + proto + frontend linters |
| `forge build` | Binaries + frontends. Docker images only with `--docker` (or `--push <registry>`) |
| `task test` / `task test:e2e` | Unit + frontends / E2E (needs a stack up); `task test:all` adds integration |

Auth is enforced in every mode — present a real bearer token (`auth`). There is
no per-service port: one binary serves every service on one mux (`PORT`, default
`8080`), and the dev loop gives the backend and each frontend a free port the
kernel picks at launch, printed in the launch banner. Depth: `dev`, `deploy`.

## Pre-flight: what forge already does for you

Run this list before writing code. Every row is something an earlier run
hand-wrote and then deleted.

| You are about to… | forge already does it | Load |
|---|---|---|
| add a **database entity** | mark the message `// forge:entity`, run bare `forge scaffold`. You get the create-table migration, the CRUD quintet, the managed fields, the entity struct + ORM, CRUD ops, a lifecycle test, the list/detail/new/edit pages, nav, and mock data | `db`, `proto` |
| add a **service** | `forge scaffold service <name>` — proto stub, `internal/handlers/<svc>/`, the typed mount, its own cobra subcommand, a test-harness entry, and the append into `cmd/<bin>/main.go` | `services` |
| add a **custom rpc** | declare it in the proto, then `forge scaffold rpc <svc> <Name>` — a correctly-signed pb-through method plus a self-destructing test row. Never hand-write the stub | `api` |
| write a **query the generated CRUD can't express** | `internal/db/<entity>_repo_ext.go` is already there, with the repo delegates, the `db.Bun()` raw-SQL handle and `orm.Context` documented in its header | `db`, `db/crud-overrides` |
| build a **UI control** | check `src/components/` and `forge component search <thing>` first. A status badge, an enum select, and the foreign-key picker/name pair are already scaffolded and enum-aware; the library ships 70+ more | `frontend`, `frontend/pages` |
| write a **table-driven test** | `forge/pkg/tdd` + the scaffolded per-rpc `handlers_scaffold_<rpc>_test.go`; contract packages get a `mock_gen.go` you configure by field | `testing/patterns` |
| **port a utility** (errors, auth, middleware, test harness, money, retries) | `forge project libraries` lists every `forge/pkg/*` package with its purpose and source path. Adopt before porting — `forge/pkg/svcerr` is the most re-implemented thing in forge's history | `forge-libraries` |
| **read a forge library's API** | `go doc <import path>` — never `find`, never `grep` across the disk | `forge-libraries` |
| need **dev data** | `forge db seed apply` derives FK-coherent rows from the applied schema; teach it your domain nouns in `db/seeds/vocab.yaml` | `db/seeding` |

`forge scaffold` also births a `worker`, `operator`, `crd`, `binary`, `adapter`,
`webhook`, `package`, `library`, `frontend`, `scenario` and `handler-file` —
`forge scaffold --help` for the arity of each, `services` for which to pick.
Everything scaffolded is yours from birth; re-running with nothing missing is a
clean no-op, and `--dry-run` prints the plan. Forge records none of it in
`forge.yaml`: services, workers, binaries and operators are discovered from the
code that declares them. Only frontends and packages take a config entry.

If the answer is not on this list, `forge project capabilities` has the full verb
set — check there before hand-rolling.

## Skills come from the binary

`forge skill load <name>` always matches the forge you are running. Anything on
disk — `.claude/skills/`, a harness-preloaded copy — is a render from whenever
`forge generate` last ran here, and can be older. **When they disagree,
`forge skill load` wins.**

```bash
forge skill list              # the catalog: every skill, scope, one-line description
forge skill load db           # print one, authoritative
forge skill search migration  # find one by keyword
```

## Rules

- Load the skill before guessing — with `forge skill load`, not from a copy on disk.
- Check `forge project capabilities` before concluding forge has no verb for this.
- Check `forge project libraries` before writing a utility, and `go doc` to read one.
- Never hand-edit `gen/` or any `*_gen.go`. `internal/app/providers.go` is owned code you wire; `internal/app/compose.go` is forge-owned and regenerated every run.
- Run `forge generate` after any proto or migration change. It never touches business logic or migrations.
- Declare schema in migrations, never in proto; the ORM follows the schema.
- Use `forge scaffold` to scaffold — never copy-paste an existing directory.
- Uncertain proto/migration/annotation syntax: draft it, run `forge generate`, read the error, fix. Never reverse-engineer forge's `internal/**` source.
- One service per proto package; its owned and generated files co-locate in one `internal/handlers/<svc>/`.
- Use `task test`, not raw `go test` — `Taskfile.yml` sets the tags, and CI runs that same target.
