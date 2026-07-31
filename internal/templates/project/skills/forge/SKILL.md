---
name: forge
description: Start here. The authoritative source for forge's conventions, the four commands that print forge's whole surface, and the pre-flight checklist that stops you hand-writing what forge already scaffolds.
---

# Forge — start here

## The copy you are reading may not be the one your binary ships

Skills are embedded **in the forge binary**, so `forge skill load <name>` always
matches the forge you are running. Anything on disk — `.claude/skills/`, a
harness-preloaded copy, a doc pasted into a prompt — is a render from whenever
`forge generate` last ran here, and can be older than your binary.

**When they disagree, `forge skill load` wins.** If a skill contradicts what you
observe in the project, re-load it before working around it; if it still
contradicts, that is a forge bug worth reporting, not a thing to route around.

```bash
forge skill list              # the catalog: every skill, scope, and one-line description
forge skill load db           # print one, authoritative
forge skill search migration  # find one by keyword across all scopes
```

## Four commands print forge's whole surface

Read these before deciding forge cannot do something. All four are enumerated
from forge's own command tree, recognizers, descriptors and package set — they
cannot drift from behaviour the way a document can.

| Command | Answers |
|---|---|
| `forge project capabilities` | **Every verb forge has**, with a one-line summary — plus every `forge lint` analyzer (including the ones hidden from `--help`) and every `// forge:*` marker. One call, whole surface. |
| `forge project annotations` | The entity-authoring vocabulary: how each proto field is born as a column, the `buf.validate` rules forge projects (with the `CHECK` and zod chain each produces), and the `(forge.v1.service)` / `(forge.v1.method)` options. |
| `forge project libraries` | **Every `forge/pkg/*` library**, what each is for, and the absolute path to the source this project resolves. Run it before porting a utility — and instead of searching the disk for library source. |
| `forge project audit --json` | What THIS project currently is: services, RPCs, entities, drift, stubs, ownership. |

Add `--json` to any of them when you want to query rather than read.

**Never search the filesystem for forge's own source.** `forge project libraries`
gives you the directory, and `go doc <import path>` gives you any package's full
API without opening a file:

```
go doc github.com/reliant-labs/forge/pkg/svcerr
go doc github.com/reliant-labs/forge/pkg/svcerr Wrap
```

## Pre-flight: what forge already does for you

Run this list before writing code. Every row is something an earlier run
hand-wrote and then deleted.

| You are about to… | forge already does it | Load |
|---|---|---|
| add a **database entity** | Mark the message `// forge:entity` and run bare `forge scaffold` (the proto is the only place an entity is declared). You get: the create-table migration, the CRUD quintet in the proto, the managed `id`/`created_at`/`updated_at` fields declared on your message, the entity struct + ORM, the CRUD ops + owned delegations, a CRUD lifecycle test, the list/detail/new/edit pages, nav, and mock data | `db`, `proto` |
| add a **service** | `forge scaffold service <name>` — proto stub, `internal/handlers/<svc>/`, the typed mount, its own cobra subcommand, and a test-harness entry | `services` |
| add a **custom RPC** | declare it in the proto, then `forge scaffold rpc <svc> <Name>` — a correctly-signed pb-through method plus a self-destructing test row. Never hand-write the stub | `api` |
| write a **query the generated CRUD can't express** | `internal/db/<entity>_repo_ext.go` is already there, scaffolded once per entity, with the repo delegates, the `db.Bun()` raw-SQL handle and `orm.Context` documented in its header | `db`, `db/crud-overrides` |
| build a **UI control** | check `src/components/` and `forge component search <thing>` first. A status badge, an enum select, and the foreign-key picker/name pair are already scaffolded and enum-aware; the library ships 70+ more | `frontend`, `frontend/pages` |
| write a **table-driven test** | `forge/pkg/tdd` + the scaffolded per-RPC `handlers_scaffold_<rpc>_test.go`; contract packages get a `mock_gen.go` you configure by field | `testing/patterns` |
| **port a utility** from another codebase (errors, auth, middleware, test harness, money, retries) | `forge project libraries` lists every `forge/pkg/*` package with its purpose and its source path. Adopt before porting — `forge/pkg/svcerr` alone is the most re-implemented thing in forge's history | `forge-libraries` |
| **read a forge library's API** | `go doc <import path>` — never `find`, never `grep` across the disk. `forge project libraries` prints the import paths | `forge-libraries` |
| need **dev data** | `forge db seed apply` derives FK-coherent rows from the applied schema; teach it your domain nouns in `db/seeds/vocab.yaml` | `db/seeding` |
| **run the stack** | `forge run` (host) or `forge env up dev` (full) — both regenerate, migrate and seed on the way up | `dev`, `deploy` |

If the answer is not on this list, `forge project capabilities` has the full
verb set — check there before hand-rolling.

## The shape, in four lines

- **SQL is the schema truth.** `db/migrations/*.up.sql` is the single source; entity structs, the ORM, CRUD wiring and frontend pages are PROJECTIONS of the applied schema.
- **Proto is the wire truth.** Services, RPCs, messages. The two evolve on independent clocks; generated conversions map their intersection by name.
- **An entity is both halves**: CRUD RPCs in the proto AND the matching table. One half alone generates honest nothing.
- **Draft, then `forge generate`.** It is a fast, exact oracle that fails loudly. Never reverse-engineer forge's `internal/**` for syntax.

Depth lives in `architecture`.

## Rules

- Load the skill before guessing — and load it with `forge skill load`, not from a copy on disk.
- Check `forge project capabilities` before concluding forge has no verb for this.
- Check `forge project libraries` before writing a utility, and use `go doc` to read one. Searching the filesystem for forge's source is always the wrong move — the path is one command away.
- Never hand-edit `gen/` or any `*_gen.go`; fix the proto, the migration, or the owned file and regenerate.
