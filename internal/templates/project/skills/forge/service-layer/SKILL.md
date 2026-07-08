---
name: service-layer
description: Business logic behind an interface — Service interface in domain types, Deps struct for collaborators, sentinel errors at the storage edge.
emit: both
---

# Service Layer

The service layer is where your application's business logic lives, behind an interface defined in domain terms. The handler/transport layer is wire glue on top; the storage layer is data plumbing underneath. The service is the actual app.

## The Service interface

`contract.go` (or your equivalent boundary file) declares what the package does, in the language of the domain — not in wire types, not in DB row types.

```go
type Service interface {
    DoThing(ctx context.Context, in DoThingInput) (DoThingResult, error)
    GetThing(ctx context.Context, id string) (Thing, error)
    ListThings(ctx context.Context, in ListThingsInput) (ListThingsResult, error)
}

type Thing struct {
    ID        string
    Name      string
    OwnerID   string
    CreatedAt time.Time
    UpdatedAt time.Time
}

type DoThingInput struct {
    UserID string
    Name   string
}
```

Rules that pay off immediately:

- **One input struct per method**, even when it has one field today. Adding a second field tomorrow doesn't break call sites.
- **Domain types, not wire types.** No transport-layer types here; no storage-row types either. The service is the seam between wire and storage.
- **Plain language-native types.** Plain `time.Time`, plain `string` IDs. Wrap in wire-specific types (`*timestamppb.Timestamp`) at the handler boundary; convert to storage-specific types (`pgtype.UUID`) at the storage boundary.
- **One canonical name** for single-implementation services. (Go convention: `Service`.) Use suffixed names (`Storer`, `Authenticator`) only for multi-implementation strategies where the suffix carries meaning.
- **Declare interfaces at the CONSUMER, not the implementation.** Accept interfaces (flexibility in), return concrete structs out (no speculative abstraction). Forge's `New(Deps) Service` returning the `Service` interface is the deliberate exception — that interface is the real mock/test seam, not speculation. But collaborators in `Deps` follow the rule: each is a *narrow* interface naming only the methods this service calls, declared here at the consumer. A wide interface that each caller only uses a slice of is a smell — split it per-consumer.

```go
// Smell: a 20-method Repository, of which this service calls three.
// Better: depend on the slice you use, declared next to the service.
type Deps struct {
    Things thingReader // narrow, consumer-declared — not the whole ORM surface
}
type thingReader interface {
    GetThing(ctx context.Context, id string) (Thing, error)
    InsertThing(ctx context.Context, t Thing) error
}
```

## The implementation

Behind the interface, the implementation type is unexported. Construction goes through a constructor that returns the interface.

```go
type Deps struct {
    DB    *db.Queries
    Now   func() time.Time
    NewID func() string
}

type svc struct {
    deps Deps
}

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

Notes on the shape:

- **`Deps` struct, not positional args.** Adding a dep doesn't churn every call site, and `Deps{DB: ..., Now: time.Now}` is self-documenting.
- **Constructor returns the interface.** Consumers depend on the interface, never on the concrete type. The concrete type stays unexported.
- **Inject side effects** — `time.Now` and ID generators are the big two. Tests get deterministic timestamps and IDs without monkey-patching.
- **Domain errors flow up.** Convert storage-driver errors to your sentinel set at the storage boundary.

## Error domain types

Use a shared sentinel set across services — don't redeclare `ErrNotFound` / `ErrAlreadyExists` / `ErrInvalidArgument` / `ErrPermissionDenied` per package. The shared set is what lets the handler layer map a service error to a transport-layer status code uniformly.

```go
// At the storage boundary, convert driver errors to sentinels:
if errors.Is(err, db.ErrNoRows) {
    return Thing{}, ErrNotFound
}
return Thing{}, fmt.Errorf("get thing: %w", err)
```

For domain-specific structured detail (e.g., a validation failure naming the field), use a typed error AND wrap a sentinel so the handler-side mapping still works:

```go
type ValidationError struct {
    Field  string
    Reason string
}

func (e ValidationError) Error() string {
    return fmt.Sprintf("validation: %s: %s", e.Field, e.Reason)
}

// Unwrap to the sentinel so generic error handling still maps correctly.
func (e ValidationError) Unwrap() error { return ErrInvalidArgument }
```

Discipline:

- **Wrap upstream errors** with `fmt.Errorf("...: %w", err)`. Add context; never replace.
- **Convert at the storage edge.** Driver-specific errors don't leak into the rest of the service. The unique-violation code from your DB becomes `ErrAlreadyExists`; the no-rows sentinel becomes `ErrNotFound`.
- **Custom typed errors** are for cases that need extra structured detail. Always implement `Unwrap() error` returning the matching sentinel so generic handling still maps them correctly.

## Testing

The service is the natural unit-test boundary. Two patterns matter:

- **Pure logic** (validation, transforms, calculations): table-driven tests on the package directly. No mocks needed.
- **DB-touching paths**: integration tests against a real database. A mock of the service interface is for *consumers* (handlers, other services) — not for testing the service against itself.

```go
func TestThingsService_DoThing(t *testing.T) {
    fixedTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
    s, _ := things.New(things.Deps{
        DB:    fakedb.New(t),
        Now:   func() time.Time { return fixedTime },
        NewID: func() string { return "thing_01" },
    })

    res, err := s.DoThing(ctx, things.DoThingInput{UserID: "u1", Name: "n"})
    if err != nil {
        t.Fatalf("unexpected: %v", err)
    }
    if res.ID != "thing_01" || !res.CreatedAt.Equal(fixedTime) {
        t.Fatalf("got %+v", res)
    }
}
```

## Multi-method services

For a domain with many methods, keep them in one `Service` interface as long as they all belong to the same domain. Splitting `Service` into `Reader` + `Writer` interfaces is a sometimes-useful refinement (callers that only read can depend on `Reader`), but start with one interface and split when a real call site needs the narrower one.

## Rules

- **Service interface in domain terms**, not wire or DB terms.
- **Constructor returns the interface**, not the concrete type. Implementation type stays unexported.
- **Deps struct for dependencies**; inject `time.Now` and ID generators so tests are deterministic.
- **Sentinel errors at the storage edge.** Convert driver errors to your shared sentinel set. Custom typed errors `Unwrap()` to a sentinel.
- **Wrap upstream errors** with `%w`; add context, never replace.
- **Never import wire types into the service.** Translation is the handler's job.

<!-- @forge-only:start -->
## Forge package layout

Every service domain in a forge project lives in ONE `internal/handlers/<svc>/` directory — owned and generated files co-located in a single component dir under the `internal/handlers/` role subtree (no two-tier `handlers/<svc>/` + `internal/<svc>/` split):

```
internal/handlers/things/
  contract.go        # Service interface — the domain surface
  service.go         # implementation behind the interface
  errors.go          # domain-typed error sentinels and types
  handlers_gen.go    # generated Connect handlers (svcerr.Wrap)
internal/handlers/mocks/
  things_mock.go     # generated by `forge generate` — your test seam, free (package mocks)
```

`forge generate` reads `contract.go`, emits `handlers_gen.go` alongside your owned files, and writes the service mock to the shared `internal/handlers/mocks/` directory (package `mocks`, one `<svc>_mock.go` per service).

Per-package observability wrappers (`middleware_gen.go`, `tracing_gen.go`, `metrics_gen.go`) were removed in 1.7. Logging, tracing, metrics, recovery, and request-id are applied uniformly at the Connect handler boundary via `forge/pkg/observe` interceptors (`observe.Chain(observe.Deps{…})` in the generated `cmd/<bin>/cmd/serve.go`). Per-method opt-in helpers — `observe.LogCall` / `observe.TraceCall` / `observe.NewCallMetrics` — are available for the rare case of an explicit child span at a service-to-service call site.

## Forge svcerr sentinels

Use `forge/pkg/svcerr` sentinels and constructors directly — `svcerr.NotFound("user")`, `svcerr.PermissionDenied("admin only")`, `svcerr.InvalidArgument("field required")`, `svcerr.AlreadyExists("resource")`. Do not redeclare `ErrNotFound` / `ErrAlreadyExists` per package — the shared sentinel set is what makes `svcerr.Wrap(err)` work uniformly in every handler.

```go
if errors.Is(err, db.ErrNoRows) {
    return Thing{}, svcerr.NotFound("thing")
}
return Thing{}, fmt.Errorf("get thing: %w", err)
```

Custom typed errors `Unwrap() error` to the matching `svcerr.Err*` sentinel:

```go
func (e ValidationError) Unwrap() error { return svcerr.ErrInvalidArgument }
```

## Wiring in the composition root (`internal/app/compose.go`)

The service is constructed in the explicit composition root, `NewComponents` in the regenerated `internal/app/compose.go`. There is no god-hook and no name-matched `*App` fields — `NewComponents` calls `things.New` and hands each collaborator in by type, reading the collaborator off the owned `*Infra` (built in `internal/app/providers.go`, `OpenInfra`):

```go
// internal/app/compose.go (regenerated — Deps filled inline off *Infra)
func NewComponents(infra *Infra) (*Components, error) {
    c := &Components{}

    c.Things = things.New(things.Deps{
        Repo:  infra.Repo,   // an interface, resolved by type — not by field name
        Now:   time.Now,
        NewID: ulid.Make,
    })
    // ... every other component constructed inline off *Infra ...
    return c, nil
}
```

A collaborator is passed as its *interface* (`Users user.Service`), so `things` can't tell whether it got the in-process service, a Connect client, or a mock. Which concrete value fills `infra.Users` is chosen once in `OpenInfra` (`internal/app/providers.go`) — splitting `things` out to its own Deployment later is a one-line swap there (`infra.Users = userclient.New(conn)`), with the consumer untouched. To hand-customize the composition (a carved authz, a two-phase setter), `forge disown internal/app/compose.go` and own the bytes.

The service body carries no observability code — `observe.Chain(observe.Deps{…})` in the generated `cmd serve.go` wraps it at the handler boundary.

## Optional Deps fields

Required deps live in `validateDeps()` so they fail fast at construction. **Optional deps** — fields a service can run without (a NATS publisher used only on the rollback path, an audit fallback, an optional gateway feature) — are tagged with `// forge:optional-dep` on the line directly above the field:

```go
type Deps struct {
    Logger *slog.Logger
    Cfg    config.ThingsConfig
    Repo   Repository

    // NATSPublisher publishes domain events; nil disables rollback.
    // forge:optional-dep
    NATSPublisher EventPublisher
}
```

The marker tells:

- **`validateDeps()`** — do NOT add a check. The marker says "nil is OK". (Required deps are still gated here at construction.)
- **the upgrade codemod** — leave per-RPC `if s.deps.X != nil { ... }` guards alone.

Optionality is expressed in the composition: `infra.<Field>` is left nil (or the optional field is omitted from the `Deps` literal) for the optional collaborator. There is no name-matched wiring layer to "emit a typed-zero" — the construction site (`NewComponents` off `OpenInfra`) is explicit, so the absence is visible at the one place.

Misplaced markers (on the struct itself, on a method docstring, etc.) are caught by `forge lint --conventions` (`forgeconv-optional-dep-marker-position`).

## Forge-specific rules

- **Never hand-edit the generated mock** (`internal/handlers/mocks/<svc>_mock.go`). Edit `contract.go` and re-run `forge generate`.
- **`Service` is the canonical name** for single-impl. `forge generate` assumes it. Use `<Name>er` only for multi-impl strategies.
- **Construct and wire the service in the explicit composition (`internal/app/compose.go`, `NewComponents`, off the owned `internal/app/providers.go` `Infra`/`OpenInfra`).** Collaborators are passed by interface, resolved by type — never assigned to name-matched `*App` fields. Observability is applied at the handler boundary by `forge/pkg/observe` interceptors.

## When this skill is not enough (forge sub-skills)

- **Choosing whether a package needs a contract** at all (pure utilities) — see `contracts`.
- **The handler half** (validation, `svcerr.Wrap`, proto↔internal conversion) — see `api`.
- **Multiple implementations / strategy pattern** — see `contracts`, "Multi-impl with strategy pattern".
- **Cross-service orchestration** — keep the orchestration in one service that depends on the others' `Service` interfaces; see `interactor`.
- **DB schema and ORM details** — see `db`.
- **Naming conventions** for the `Service` / `Deps` / `New` triple and snake_case directory paths, plus the owned composition root (`Build`) — see `architecture` → **Naming conventions**.
<!-- @forge-only:end -->
