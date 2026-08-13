---
name: service-layer
description: Business logic behind an interface — Service interface in domain types, Deps struct of interface-typed deps, sentinel errors at the storage edge.
emit: both
---

# Service Layer

Business logic lives behind an interface defined in domain terms. The handler/transport layer is wire glue on top; the storage layer is data plumbing underneath.

## The Service interface

`contract.go` (or your equivalent boundary file) declares what the package does, in the language of the domain.

Doc-comment every exported symbol, starting with its own name (`go doc`'s order; `revive`'s `exported` rule if you enable it).

```go
// Service is the things domain boundary.
type Service interface {
    DoThing(ctx context.Context, in DoThingInput) (DoThingResult, error)
    GetThing(ctx context.Context, id string) (Thing, error)
    ListThings(ctx context.Context, in ListThingsInput) (ListThingsResult, error)
}

// Thing is the domain view of a thing.
type Thing struct {
    ID        string
    Name      string
    OwnerID   string
    CreatedAt time.Time
    UpdatedAt time.Time
}

// DoThingInput is the input to Service.DoThing.
type DoThingInput struct {
    UserID string
    Name   string
}
```

Rules that pay off immediately:

- **One input struct per method**, even with one field today — adding a second tomorrow doesn't break call sites.
- **Domain types, never wire or storage-row types.** Plain `time.Time` / `string` IDs here; wire types (`*timestamppb.Timestamp`) at the handler boundary, storage types (`pgtype.UUID`) at the storage boundary. Translating between them is the handler's job.
- **One canonical name** for single-implementation services (`Service`); a suffixed name (`Storer`, `Authenticator`) only where the suffix distinguishes real alternatives.
- **Declare interfaces at the CONSUMER.** Accept interfaces, return concrete structs. Forge's `New(Deps) Service` is the deliberate exception — that interface is the real test seam. Every `Deps` field follows the rule: a *narrow* interface naming only the methods this service calls, declared in `contract.go` so it gets a mock. `forgeconv-deps-are-interfaces` fails on a concrete-typed one. Converting a field: a nil concrete pointer in an interface is a NON-nil interface, so `if deps.X == nil` silently stops firing — check the guards on any optional dep.
- **One `Service` per domain.** Split into `Reader` + `Writer` only when a real call site needs the narrower one — not up front.

```go
// Smell: a 20-method Repository, of which this service calls three.
type Deps struct {
    Things thingReader // narrow, consumer-declared — not the whole ORM surface
}
type thingReader interface {
    GetThing(ctx context.Context, id string) (Thing, error)
    InsertThing(ctx context.Context, t Thing) error
}
```

### Persistence: use the GENERATED store, don't hand-write an adapter

The rule above is about interfaces you own. **Do not apply it by hand-writing a
Store interface plus a passthrough adapter over the ORM** — forge generates
both, per entity and in aggregate, in `internal/db/store_gen.go`:

```go
type Deps struct {
    Estimates db.EstimateStore   // one entity
    DB        db.Store           // or the aggregate, when a service spans tables
}

est, err := s.deps.Estimates.GetEstimateByID(ctx, id)   // no handle to thread
```

`forge generate` resolves each by type from the ORM client it already owns — **no Infra field, no constructor, nothing to wire.** Just declare the field. The store
holds the database handle, so a method takes only your arguments. Prefer the
narrowest that works — a smaller dep is a smaller fake.

To see the methods available on one, ask your own package — `go doc
./internal/db EstimateStore`, or `forge project shapes --kind store` for the
list. Both answer from the file generated for *your* schema. Reading forge's
generator source instead is a detour that cost one measured run 11 turns.

**Two questions, two commands — neither is a grep.**

| you want | run |
|---|---|
| what YOUR generated code offers (stores, RPCs, handlers, tables) | `forge project shapes --grep Estimate` |
| what a `forge/pkg` LIBRARY offers (`orm`, `crud`, `svcerr`, `tdd`, `testkit`) | `forge project libraries orm crud` |

`forge project libraries orm` answers `RunTransaction` — a symbol one measured
run grepped for **sixteen times** while the skill documenting it was already
loaded. Bare `go doc <pkg>` does not: it prints a struct as `struct{ ... }` with
no methods. `go doc <pkg> <Type>` does work if you already know the type name.

The aggregate exposes one accessor per entity, and either form rebinds to a
transaction with `WithTx`, so a multi-step use case commits as a unit.

**`RunTransaction` is on the ORM CLIENT, not on the store.** The store is the
data surface; the client owns the transaction. Declare both when a use case
spans entities — a measured run grepped for `RunTransaction` **sixteen times**
and then wrote throwaway compile probes to discover it is not a `Store` method:

```go
type Deps struct {
    DB  db.Store    // the data surface: DB.Estimates(), DB.Jobs(), …
    ORM *orm.Client // the transaction boundary: ORM.RunTransaction(…)
}

err := deps.ORM.RunTransaction(ctx, func(tx orm.Context) error {
    s := deps.DB.WithTx(tx) // rebind every store onto this transaction
    if err := s.Estimates().UpdateEstimate(ctx, est); err != nil {
        return err
    }
    return s.Jobs().CreateJob(ctx, job)
})
```

`go doc ./internal/db Store` shows `WithTx` on the store; `forge project
libraries orm` shows `RunTransaction` on the client.

Two measured runs missed this and hand-wrote the layer anyway: 723 lines across
four packages in one, 464 across two in the other, almost all of it one-line
passthroughs like `return db.GetEstimateByID(ctx, s.db, id)`. If you find
yourself writing that method, stop and use the generated store.

**A query the generated CRUD cannot express** — a join, an aggregate, a CTE,
raw SQL — goes in that entity's `internal/db/<entity>_repo_ext.go` seam, which
is scaffolded once and never regenerated. Declare a narrow consumer-side
interface for it (the rule above applies again here) rather than expecting it on
the generated store, which is regenerated and would go stale against it.

## The implementation

Behind the interface, the implementation type is unexported. Construction goes through a constructor that returns the interface.

```go
// Deps is what the service needs, injected at construction.
type Deps struct {
    DB    *db.Queries
    Now   func() time.Time
    NewID func() string
}

type svc struct {
    deps Deps
}

// New builds the service, defaulting the optional seams.
func New(deps Deps) (Service, error) {
    if deps.Now == nil   { deps.Now = time.Now }
    if deps.NewID == nil { deps.NewID = newULID }
    return &svc{deps: deps}, nil
}

func (s *svc) DoThing(ctx context.Context, in DoThingInput) (DoThingResult, error) {
    if in.Name == "" {
        return DoThingResult{}, ValidationError{Field: "name", Reason: "required"}
    }
    row, err := s.deps.DB.InsertThing(ctx, &db.Thing{
        ID:        s.deps.NewID(),
        Name:      in.Name,
        OwnerID:   in.UserID,
        CreatedAt: s.deps.Now(),
    })
    if err != nil {
        if db.IsUniqueViolation(err) {
            return DoThingResult{}, ErrAlreadyExists
        }
        return DoThingResult{}, fmt.Errorf("insert thing: %w", err)
    }
    return DoThingResult{ID: row.ID, CreatedAt: row.CreatedAt}, nil
}
```

Shape notes: **`Deps` struct, not positional args** (adding a dep doesn't churn call sites); **inject side effects** — `time.Now`, ID generators — so tests get deterministic timestamps/IDs without monkey-patching.

## Error domain types

Use a shared sentinel set across services — don't redeclare `ErrNotFound` / `ErrAlreadyExists` / `ErrInvalidArgument` / `ErrPermissionDenied` per package. The shared set is what lets the handler layer map a service error to a transport-layer status code uniformly.

Sentinels **stay exported** — the export IS the `errors.Is` seam. When `revive`'s `exported` asks one for a doc comment, write the doc comment; its other suggestion ("or be unexported") deletes every cross-package `errors.Is` against it, silently, and the compiler says nothing.

```go
// Convert driver errors to sentinels at the storage edge (no-rows → ErrNotFound,
// unique-violation → ErrAlreadyExists); wrap everything else with %w.
if errors.Is(err, db.ErrNoRows) {
    return Thing{}, ErrNotFound
}
return Thing{}, fmt.Errorf("get thing: %w", err)
```

For domain-specific structured detail (e.g., a validation failure naming the field), use a typed error AND wrap a sentinel so the handler-side mapping still works:

```go
// ValidationError reports a field that failed validation.
type ValidationError struct {
    Field  string
    Reason string
}

// Error implements error.
func (e ValidationError) Error() string {
    return fmt.Sprintf("validation: %s: %s", e.Field, e.Reason)
}

// Unwrap returns the sentinel so generic error handling still maps correctly.
func (e ValidationError) Unwrap() error { return ErrInvalidArgument }
```

## Testing

The service is the natural unit-test boundary. Two patterns: **pure logic** (validation, transforms) — table-driven tests on the package directly, no mocks; **DB-touching paths** — integration tests against a real database. A mock of the service interface is for *consumers* (handlers, other services), not for testing the service against itself. Construct it with the seams pinned — `New(Deps{DB: fakedb.New(t), Now: func() time.Time { return fixedTime }, NewID: func() string { return "thing_01" }})` — so timestamps and IDs are assertable. Test shape itself: `testing-unit`, `testing-patterns`.

<!-- @forge-only:start -->
## Forge package layout

A forge service is a **pb-through handler package** at `internal/handlers/<svc>/`: the `*Service` and its RPC methods work with wire types + `s.deps` (see `api`). This Service-interface-in-domain-terms pattern is NOT baked into that package — there's no `contract.go`/`Service`-interface split inside a handler package, and forge never generates domain types or proto↔domain converters. You reach for it when an RPC's logic earns a transport-agnostic, unit-testable home: scaffold a **standalone package** (`forge scaffold package <name>`) and call it from the handler.

```
internal/things/              # the standalone domain package — `forge scaffold package things`
  contract.go                 #   Service interface — yours
  things.go                   #   implementation behind the interface (yours)
  errors.go                   #   domain error sentinels (yours, optional)
  observe_chain.go            #   owned observe seam the decorator routes through
  mock_gen.go                 #   generated from contract.go — your test seam
internal/handlers/things/     # the pb-through handler package that calls it
  service.go                  #   concrete *Service + Deps (Deps.Things is a things.Service)
  rpc_<name>.go               #   one custom RPC method per file, calling s.deps.Things
  handlers_crud.go            #   owned CRUD delegations; handlers_crud_ops_gen.go is generated
internal/handlers/mocks/things_mock_gen.go  # generated mock of the Connect RPC surface
```

`forge generate` reads each domain package's `contract.go`, emits its `mock_gen.go`, and wires the package into the handler **by type** in `NewComponents`. It never generates a `contract.go` — that interface is yours.

**In-process observability is opt-in and generated.** Edge concerns (logging/tracing/metrics/recovery/request-id) are applied at the Connect boundary by `forge/pkg/observe` interceptors; INSIDE the process, a slim per-method decorator `middleware_gen.go` (regenerated from the `Service` interface, a sibling of `mock_gen.go`) routes each call through the owned, scaffold-once `observe_chain.go` next to `contract.go`. Opt in with `// forge:constructor` on `func New` (scaffolds stamp it by default); the decorator wraps every method (spans + call/error/duration metrics, no argument capture; see `observability`) and the compose site emits `pkg.NewServiceWithForgeMiddleware(pkg.New(...))`. `observe_chain.go` (recovery → span → metrics → structured log, nil-safe) is THE place to add a custom `observe.ComponentMiddleware`, drop a layer, or change the success-log level. Opt out with `// forge:no-observe` on `func New` (whole package) or on an interface method's doc comment (one method). Handler packages (concrete `*Service`) are NOT wrapped — otelconnect owns the RPC edge. A wired component with neither marker is flagged by the `enforce-component-observe` lint (kill-switch `config.enforce_component_observe: off`).

## Forge svcerr sentinels

Use `forge/pkg/svcerr` sentinels and constructors directly (`svcerr.NotFound("user")`, `svcerr.PermissionDenied("admin only")`) — that shared set is what makes `svcerr.Wrap(err)` uniform. Convert at the storage edge, and have custom typed errors `Unwrap()` to the matching sentinel (`func (e ValidationError) Unwrap() error { return svcerr.ErrInvalidArgument }`).

**What you wrap with `%w` is for the OPERATOR.** The client message is the constructor's detail, never the accumulated string, so ids and DSNs you log stay off the wire (read them back with `svcerr.Cause`). Full sentinel/code table: see `api`.

## Wiring in the composition root

The service is constructed in `NewComponents` (the regenerated `internal/app/compose.go`) — no god-hook, no name-matched `*App` fields. `NewComponents` calls `things.New` and fills each `Deps` field **by type** off the owned `*Infra` (built in `internal/app/providers.go`, `OpenInfra`):

```go
c.Things = things.New(things.Deps{
    Repo:  infra.Repo,                                     // interface, resolved by type — not by field name
    Now:   time.Now,                                       // Clock seam — auto-wired
    NewID: func() string { return ulid.Make().String() },  // IDGen seam — auto-wired
})
```

Each dep is passed as an *interface*, so `things` can't tell the in-process service from a Connect client or a mock; splitting it out to its own Deployment later is a one-line swap in `OpenInfra` (`infra.Users = userclient.New(conn)`), consumer untouched. To hand-customize (a carved cross-edge, a two-phase setter), `forge project disown internal/app/compose.go`. See `architecture` → **The composition root** for the full model.

### Deterministic time & IDs — the Clock / IDGen seam

If a service wants a testable clock or ID source in its OWN logic, declare a
Deps field typed exactly `func() time.Time` (a **Clock**) or `func() string`
(an **IDGen**) — name it whatever reads well (`Now`, `Clock`, `NewID`, `IDs`),
as `Deps` does under **The implementation** above.

These are wired **by type**, automatically: `compose.go` fills them with the real impls (`time.Now` + a ULID generator) — you do NOT hand-wire them or add them to `Infra`; the generated test harness (`NewTest<Svc>`) defaults them to the same real impls (override via `With<Svc>Deps` for a frozen clock). They are guaranteed non-nil, so **do NOT** mark them `//forge:optional-dep` or nil-guard their use (marking one optional opts OUT of the seam). To supply a custom live impl, declare an `Infra` field of the SAME NAME (it wins over the seam), or disown `compose.go`. The DB layer already stamps IDs/timestamps at the persistence chokepoint — this seam is only for services that want deterministic time/ids in their own logic.

## Optional Deps fields

Required deps live in `validateDeps()` so they fail fast at construction. An **optional** dep — a field a service can run without (a NATS publisher used only on the rollback path, an audit fallback) — is tagged `// forge:optional-dep` on the line directly above the field. Then `validateDeps()` must NOT check it, per-RPC `if s.deps.X != nil { ... }` guards stay, and optionality is expressed in the composition (`infra.<Field>` left nil / omitted from the `Deps` literal — the explicit construction site makes the absence visible in one place). Misplaced markers are caught by `forge lint --conventions` (`forgeconv-optional-dep-marker-position`). Same mechanism as the `api` skill.

## Forge-specific rules

- **Never hand-edit the generated mock** (`internal/handlers/mocks/<svc>_mock.go`). Edit `contract.go` and re-run `forge generate`.
- **`Service` is the canonical interface name** for single-impl (no annotation). If a role name reads better (`Gateway`, `Provider`), keep it and put a `//forge:service` (or `//forge:contract`) marker directly above the interface — codegen + the contract-name lint key off the marker under any name. `// forge:constructor` does the same for a constructor forge must FIND (`Open`, `Connect`, `NewReadOnly`); the contract-name lint still wants the canonical `New(Deps) (Service, error)` in an internal package, so keep `New` as the entry point and mark the others. `Deps` stays canonical. Use `<Name>er` only for multi-impl strategies.
- **Construct and wire in `NewComponents`** — every `Deps` field by interface, by type.

## When this skill is not enough (forge sub-skills)

Whether a package needs a contract at all (pure utilities) → `contracts`. The handler half (validation, `svcerr.Wrap`, proto↔internal conversion) → `api`. Multiple implementations / strategy pattern → `contracts`. Cross-service orchestration (never inline in a handlers package) → `interactor`. DB schema and ORM → `db`. Naming for the `Service`/`Deps`/`New` triple and directory paths → `architecture` → **Naming conventions**.
<!-- @forge-only:end -->
