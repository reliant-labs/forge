---
name: forge-libraries
description: Adopt-or-port judgment for forge's public libraries — forge/pkg for Go and @reliant-labs/web-runtime for the frontend. Read this BEFORE porting a utility from an existing codebase — the equivalent may already exist. Run `forge project libraries` for the live index and the source location of both.
emit: both
---

**Run `forge project libraries` first.** forge ships TWO runtime libraries and
the command prints both:

- **`forge/pkg`** (Go) — where it resolves to for THIS project and what each
  package is for, from the toolchain's own module resolution and each
  package's doc comment. Follow with `go doc <import path>` for the full API.
- **`@reliant-labs/web-runtime`** (frontend) — where npm installed it, its
  entry points, and every module with the symbols it exports, from the
  package's own manifest and barrel. `frontend-runtime` has the composition
  rules; the runtime is a package, so there is nothing to edit or regenerate.

Every field is derived, so neither half can drift from what actually ships.

Never scan the disk for forge's source. A recursive search rooted outside the
repo is refused, and when several agents do it at once it starves every one of
them. Both libraries have been hunted that way; both are one command away.


# Forge Libraries (`github.com/reliant-labs/forge/pkg/*`)

Forge ships a set of public Go libraries under `github.com/reliant-labs/forge/pkg/`. They're independent of any particular forge project — you can `go get` them into any Go module that wants a Connect-RPC stack with the same conventions.

**Read this before porting a utility from another codebase.** The migration of `control-plane` to forge surfaced four would-be re-implementations that already existed here, plus several that *should* have been adopted but weren't because the porter didn't know what was available.

## The index is a command, not a list

```
forge project libraries          # every subpackage + its purpose, and WHERE the source is
forge project libraries --json
```

It prints the absolute directory holding the `forge/pkg` source **this project resolves** — a local checkout when a `go.work` points at one, the module cache otherwise — then one line per subpackage. Every field is derived: the directory from `go list -m`, the package set from that directory, each purpose from the package's own doc comment. It therefore describes the pkg version you actually compile against, and it cannot drift the way a table in a document does.

**Never search the disk for library source.** For the full API of one package, ask the toolchain:

```
go doc github.com/reliant-labs/forge/pkg/svcerr        # the whole package surface
go doc github.com/reliant-labs/forge/pkg/svcerr Wrap   # one symbol
```

This is faster than opening files, works identically whether the source sits in a checkout or the module cache, and needs no path.

## Decision rule: adopt-or-port

When porting a utility from an existing codebase to forge:

1. **Run `forge project libraries` first.** If a forge package covers the surface, adopt it. The migration of control-plane to forge skipped two ports outright (`forge/pkg/svcerr`, tracing) because forge equivalents existed and were strict supersets.
2. **If forge covers ~80%**, adopt the forge package and add a thin project-local extension for the missing 20%. Don't fork forge into your project tree.
3. **If forge doesn't cover it**, write your project-local package under `internal/<name>/` (top-level `pkg/` is only for code with real external importers — see the architecture skill). Don't bend forge's package to fit a domain it wasn't designed for.

## The traps

These are the packages people re-implement. The list is judgment, not inventory — `forge project libraries` is the inventory.

| Instead of writing… | Adopt |
|---|---|
| a parallel set of error sentinels, or a hand-rolled `error → connect.Code` switch | `forge/pkg/svcerr`. The single biggest "I almost ported it before realizing it existed" trap from the migration. Handlers call `svcerr.Wrap(err)`; service code returns the sentinels. |
| per-RPC table-test boilerplate | `forge/pkg/tdd` (`RunRPCCases`), plus the generated `mock_gen.go` you configure by field. |
| a bootstrap harness for integration tests | `forge/pkg/testkit` — discard logger, real postgres via `forge/pkg/pgtest`, httptest harness, claims-bearing `AuthedContext`. Extend it; don't roll your own. |
| your own JWT / JWKS / HMAC validator | `forge/pkg/auth` for the validators, `forge/pkg/authn` for the interceptor mechanism. Tune the policy hooks in your owned `pkg/middleware/middleware.go`. |
| API-key generation, hashing, or comparison | `forge/pkg/apikey`. You own the table and the store; these are the get-it-wrong-and-you're-breached parts. |
| a CORS / security-headers / request-id / rate-limit middleware | `forge/pkg/middleware`. Your composition root wires the chain; don't photocopy the middlewares. |
| logging / tracing / metrics interceptors | `forge/pkg/observe`. The scaffold already wires `observe.Chain`; reach in only for opt-in per-method instrumentation. |
| CRUD lifecycle plumbing in a handler | `forge/pkg/crud`. Forge auto-wires it from your CRUD RPCs — don't bypass it. |
| `*sql.DB` scan boilerplate | `forge/pkg/orm`, consumed by the generated `internal/db/<entity>_orm.go`. Schema truth is `db/migrations/`; entities are projections of it. |

Each row's own package doc has the detail: `go doc <import path>`.

## What's NOT here

- **HTTP / Connect transport plumbing** — `connectrpc.com/connect` itself, not forge. Forge doesn't wrap Connect; it embraces it.
- **Database driver** — `jackc/pgx`. Forge is postgres-pinned and doesn't ship its own driver; `forge/pkg/orm` builds on top.
- **OTel SDK init** — `forge/pkg/serverkit` owns it. serverkit calls `observe.Setup` internally from `serverkit.Config.OTLPEndpoint` + `ServiceName` (projected from the project's typed config in the generated `cmd serve.go`); there is no per-project `cmd/otel.go` shim. `forge/pkg/observe` is the *interceptor* layer; serverkit drives the SDK bootstrap. To customize sampling / resource attrs, configure it through serverkit's config rather than a hand-rolled `cmd/otel.go`.
- **Stripe / Twilio / NATS clients** — owned adapter code you write over the vendor SDK (`forge skill load adapter`), not `forge/pkg/*` libraries. Forge ships no packs or bundled third-party integrations.

## When this skill is not enough

- The current package set, and where its source lives — `forge project libraries`.
- Implementation details of any individual package — `go doc <import path>`.
- The forge codegen pipeline and the owned composition root that wires these libraries — see the `architecture` skill.
- When to write a custom adapter vs. extend a forge package — see the `adapter` skill.
