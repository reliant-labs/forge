# Scaffolding a service — the pb-through model, boot alive

**Status:** design spec (current). Author target: forge maintainers + the forge-one-shot workflow owners.

> **History.** An earlier version of this document specified a *rich RPC "vertical"*: `forge scaffold rpc` scaffolded a six-step thin handler, a per-service **domain package** (`internal/<svc>` with a `Service` interface, `<Name>Input`/`<Name>Result` types, proto↔domain converters, and a mock), a compiling impl stub, and test tables on both sides — plus `--interactor` / `--all-unwired` flags and a phase-3 "vertical sweep" in `forge scaffold`. That whole vertical was **removed**. Forge's two RPC codegen architectures collapsed into ONE shape — "pb-through". This document specifies the model that replaced it; the removed surface is catalogued in §6.

**Thesis (unchanged).** A forge app should boot ALIVE: after an agent (or a human) declares the two truths — the wire contract in proto and the schema in migrations — `forge generate` + `forge run` must produce a running, navigable, **populated** product: pages that render plausible, FK-coherent data behind a logged-in dev identity, on production rails. Forge earns its wow by *generating* structure — ORM, CRUD wiring, handler stubs, seed data, frontend pages — deterministically projected from the two truths or scaffolded once as a compiling, lint-clean starting point. The agent spends its messages only on what only an agent can decide: the schema, the wire shapes, and the business-logic bodies.

---

## 1. The two truths, and the deterministic/inferential line

| Truth | Location | How forge reads it |
|---|---|---|
| **Schema truth** | `db/migrations/*.up.sql` | Applied verbatim to an ephemeral Postgres shadow and introspected back (columns, PKs, indexes, FKs). No annotations; the applied schema *is* the model. |
| **Wire truth** | `proto/services/<svc>/v1/*.proto` | `buf generate` with protoc-gen-forge in descriptor mode → `gen/forge_descriptor.json`. |
| **Topology** | `forge.yaml` | Components, features, auth provider, environments. |

An **entity** exists only when both halves agree: CRUD-shaped RPCs in a service proto *and* a matching table (pluralized snake_case) in the applied schema. Everything below the two truths is deterministic projection (same inputs, same bytes, regenerated freely). Everything at or above them is a decision — forge scaffolds affordances (compiling starting points, printed snippets, hints) but never *derives* one truth from the other. Forge writes `db/migrations/` at exactly one kind of moment (an explicit `forge scaffold entity`, including the one-time `// forge:entity` birth); after birth, no command writes or modifies a migration from proto state.

---

## 2. The pb-through model — ONE shape

**Every RPC is a method on the handler package's `*Service`**, in `internal/handlers/<svc>/`, working with the generated **pb (wire) types** and `s.deps`. There is no second architecture, no per-service domain package, no generated proto↔domain converters.

```go
// internal/handlers/<svc>/service.go — scaffolded once, yours
type Service struct {
    <svc>v1connect.Unimplemented<Svc>ServiceHandler
    deps Deps
}
```

The handler package holds, co-located in one directory:

| File | Ownership | Contents |
|---|---|---|
| `service.go` | scaffold-once | `Deps` (+ `validateDeps`), the concrete `*Service`, `New(Deps) (*Service, error)` |
| `rpc_<name>.go` (one per RPC) | scaffold-once | one custom (non-CRUD) RPC method on `*Service` |
| `handlers_crud.go` | scaffold-once + append | thin CRUD delegations to `pkg/crud` |
| `handlers_crud_ops_gen.go` | Tier-1 (regenerated) | per-RPC CRUD op constructors (field/filter/response wiring) |
| `validators.go` | scaffold-once | pure wire-validation functions |
| `handlers_scaffold_<rpc>_test.go` (one per RPC) | scaffold-once | `tdd.RunRPCCases` table (self-destructing rows) |

There is **no** `contract.go` and **no** `handlers_gen.go` in a handler package — the `*Service` methods are the handlers. A generated mock of the service's *Connect RPC surface* lands in the shared `internal/handlers/mocks/<svc>_mock.go` (package `mocks`), for a peer that depends on this service.

### 2a. CRUD RPC

A proto CRUD-shape message marked `// forge:entity` plus its born table makes the `Create/Get/List/Update/Delete` RPCs entity-backed. Each delegates to the generic `pkg/crud` runtime through a generated Tier-1 op and an owned thin shim:

```go
// handlers_crud.go (yours — customize by replacing the delegation)
func (s *Service) CreateInvoice(ctx context.Context, req *connect.Request[pb.CreateInvoiceRequest]) (*connect.Response[pb.CreateInvoiceResponse], error) {
    return crud.HandleCreate(s.crudCreateInvoiceOp())(ctx, req)
}
```

The shim never names entity fields, so schema changes flow through the regenerated `handlers_crud_ops_gen.go` and the shim never rots. To diverge one RPC, override an exported field of the op (`Pack`, `Persist`, `Auth`, …) in the shim and keep delegating. This path is **unchanged** by the pb-through collapse.

### 2b. Custom (non-CRUD) RPC

A custom RPC surfaces as an owned **pb-through stub** on `*Service`, in its **own file** (`rpc_<snake_name>.go`), carrying the `// forge:gen unwired-stub` marker and returning `CodeUnimplemented`:

```go
// rpc_ping.go — emitted by generate; you implement it in place using s.deps
func (s *Service) Ping(ctx context.Context, req *connect.Request[pb.PingRequest]) (*connect.Response[pb.PingResponse], error) {
    return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("handler for %s not yet implemented", "Ping"))
}
```

You fill the body in place: validate, extract auth, do the work through `s.deps` (the repo for data access, a collaborator for orchestration), pack the response, `svcerr.Wrap` errors. The scaffold's self-destructing `tdd.RunRPCCases` row goes red the moment you implement it. If a custom RPC later becomes entity-backed (a table is added), CRUD gen **excises** the marked stub and takes over with a delegating shim — no duplicate-method compile error; the emptied `rpc_<name>.go` is removed with it.

One RPC per file is the whole point of the name: the file layout is invisible to Go and to every forge reader (all of them walk the directory), so it is chosen for the one thing it does decide — who has to merge with whom. Two authors implementing two RPCs of the same service never touch the same file, and no workflow has to pre-split anything before fanning out. `forge scaffold rpc` and `forge generate` write the same filename for the same RPC, so the order you work in does not change the layout.

---

## 3. The domain-layer escape hatch

The transport-agnostic use-case layer is preserved — as an *opt-in standalone package*, not a per-RPC generated artifact. A team that wants a unit-testable, reusable domain layer scaffolds one and calls it from a pb-through handler:

```bash
forge scaffold package billing                 # internal/billing/ — contract.go + service.go + mock_gen.go
forge scaffold package billing-flow            # same shape, used as a use-case orchestrator over >=2 deps
```

The package owns a `Service` interface (`contract.go`), its implementation, and gets a free `mock_gen.go`. The handler's `Deps` gains a field of that interface type; forge's **by-type DI** (`NewComponents` in `internal/app/compose.go`, off the owned `internal/app/providers.go` `Infra`) fills it automatically — no name-matching, no registry. The handler owns the trivial pb→domain mapping at the boundary explicitly. See the `contracts`, `service-layer`, and `interactor` skills.

---

## 4. `forge scaffold rpc`

Adds one custom RPC to an existing service. Two modes, both landing on the pb-through shape:

- **RPC already in the service proto:** the pb-through stub is exactly what the generate pipeline emits, so the command just runs generate in-process (self-healing a stale descriptor). The method lands wired on `*Service` (or as a CRUD shim if entity-backed).
- **RPC not in the proto yet:** writes a correctly-signed handler stub and prints the proto snippet to paste (`--stream server|client|bidi`; omit for unary). The proto edit is left to the user — proto files have hand-curated section markers an injector would regress.

Deliberate non-goals: it does not edit the `.proto`, and it does not derive schema from RPC shapes (migrations stay the single owner of schema).

---

## 5. `forge scaffold` — the one command after proto edits

A visible, phased chain of existing scaffold-once primitives — never a new writer. **Two phases** (the old phase-3 vertical sweep is gone):

- **Phase 1 — entity births.** Every `// forge:entity`-marked message with no applied table gets its missing CRUD quintet injected into the service proto (one-time) and an owned create-table migration pair. Already-tabled marked messages are inert (reported; evolution is a new migration); envelope shapes (Request/Response names, pagination fields) are refused loudly. One bad entity never aborts the batch.
- **Phase 2 — projection.** Runs the generate pipeline in-process when phase 1 wrote anything or the descriptor is stale against the raw protos; skipped loudly otherwise. Projection emits the CRUD wiring for every entity-backed RPC and the **pb-through stub** for every new custom RPC.

`--dry-run` prints the plan and writes nothing. `--service <svc>` narrows every phase. Idempotent: a re-run with nothing missing is a clean no-op.

---

## 6. What was removed (historical)

The pb-through collapse deleted the following. They are catalogued here so old references resolve:

- **The `--interactor` and `--all-unwired` flags** on `forge scaffold rpc`.
- **The "typed vertical" / "RPC vertical"** — the per-RPC scaffold of a thin handler + domain package + impl stub + test skeletons.
- **The per-service domain package** (`internal/<svc>` with a `Service` interface, `<Name>Input`/`<Name>Result` domain types, proto↔domain converters, and a mock). The domain-layer capability survives only as the opt-in standalone package of §3.
- **Phase 3 of `forge scaffold`** — the vertical sweep over unwired custom RPCs. Scaffold is now phase 1 (births) + phase 2 (generate).
- **The `internal/scaffold` RPC-vertical renderer** (`rpcvertical*.go`).

A custom RPC is now, and only, a pb-through method on `*Service`; a domain layer is now, and only, a package you scaffold and call by type.
