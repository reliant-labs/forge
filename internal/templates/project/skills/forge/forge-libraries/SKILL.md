---
name: forge-libraries
description: Adopt-or-port judgment for forge's public libraries — forge/pkg for Go and @reliantlabs/forge-web-runtime for the frontend. Read this BEFORE porting a utility from an existing codebase — the equivalent may already exist. `forge project libraries <pkg>` prints any package's full signatures; never read forge's source to find one.
emit: both
---

**Need a forge/pkg signature? `forge project libraries <pkg>`.** One call,
any number of packages, full exported API:

```
forge project libraries crud             every signature crud exports
forge project libraries crud orm svcerr  three packages, one call
forge project libraries orm.Context      one type, with its methods
```

**Do not use `go doc <pkg>` for this, and do not read forge's source.**
`go doc` renders a struct or interface as `struct{ ... }` and lists no
methods, so `go doc .../pkg/crud` never mentions `Repo.UpdateMasked`. That
dead end is measured, not hypothetical: one run spent **35.5 minutes across
89 turns** grepping `forge/pkg` for signatures — `pkg/crud/repo.go` alone 14
times — and three of those units never wrote a line of code. `go doc -all`
is complete but ~10x larger, mostly prose.

**Run `forge project libraries` (no arguments) first** for the index. forge
ships TWO runtime libraries and the command prints both:

- **`forge/pkg`** (Go) — what each package is for, from each package's own
  doc comment, and which copy this project resolves. Name a package to get
  its signatures.
- **`@reliantlabs/forge-web-runtime`** (frontend) — where npm installed it, its
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
forge project libraries          # every subpackage + its purpose
forge project libraries crud     # crud's FULL exported API, signatures and all
forge project libraries --json
```

With no arguments it prints one line per subpackage, plus the absolute directory holding the `forge/pkg` source **this project resolves** — a local checkout when a `go.work` points at one, the module cache otherwise. That directory is there so you can confirm *which copy* you compile against; it is not a place to go reading. Every field is derived: the directory from `go list -m`, the package set from that directory, each purpose from the package's own doc comment. It therefore describes the pkg version you actually compile against, and it cannot drift the way a table in a document does.

**Name a package and it prints the answer** — every func with its parameters, every struct with its fields, every interface and type with its methods, parsed from that same resolved source with the doc prose stripped. Several packages in one call; `all` for everything (large).

**Never search the disk for library source, and don't reach for `go doc <pkg>` here.** The package view of `go doc` collapses every struct and interface to `struct{ ... }` and lists no methods at all — it cannot answer "what are `Repo.UpdateMasked`'s parameters", which is the question people actually have. `go doc -all <pkg>` *is* complete, if you want the prose too, at roughly ten times the size.

`go doc` remains the right tool for **your own project's** generated code, where the types you care about are interfaces and `go doc` renders those in full:

```
go doc ./internal/db Store           # the aggregate store, every method
go doc ./internal/db EstimateStore   # one entity's store
```

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

Each row's full API is one call away: `forge project libraries <name>`.

## What's NOT here

- **HTTP / Connect transport plumbing** — `connectrpc.com/connect` itself, not forge. Forge doesn't wrap Connect; it embraces it.
- **Database driver** — `jackc/pgx`. Forge is postgres-pinned and doesn't ship its own driver; `forge/pkg/orm` builds on top.
- **OTel SDK init** — `forge/pkg/serverkit` owns it. serverkit calls `observe.Setup` internally from `serverkit.Config.OTLPEndpoint` + `ServiceName` (projected from the project's typed config in the generated `cmd serve.go`); there is no per-project `cmd/otel.go` shim. `forge/pkg/observe` is the *interceptor* layer; serverkit drives the SDK bootstrap. To customize sampling / resource attrs, configure it through serverkit's config rather than a hand-rolled `cmd/otel.go`.
- **Stripe / Twilio / NATS clients** — owned adapter code you write over the vendor SDK (`forge skill load adapter`), not `forge/pkg/*` libraries. Forge ships no packs or bundled third-party integrations.

## When this skill is not enough

- The current package set — `forge project libraries`.
- The full exported API of any package — `forge project libraries <pkg>`.
- The forge codegen pipeline and the owned composition root that wires these libraries — see the `architecture` skill.
- When to write a custom adapter vs. extend a forge package — see the `adapter` skill.
