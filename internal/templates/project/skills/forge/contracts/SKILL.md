---
name: contracts
description: Using forge's contract system — when to use contract.go, how to write one, what forge generate produces, integrating with tests.
---

# Forge Contracts

Internal Go packages in a forge project expose a Go interface in `contract.go`. The contract is the public surface of the package; everything else is implementation detail. From one `contract.go` `forge generate` produces the test seam (mocks) and the cross-cutting middleware (logging, tracing, metrics) for free.

Applies to greenfield packages and ports alike.

## When does a `contract.go` belong?

Three categories of internal package, three answers:

| Category | Has `contract.go`? | Why |
|----------|--------------------|-----|
| **Single-impl service** (DB-backed, calls external API, holds state) | Yes | Test seam + auto-generated middleware/tracing/metrics. The mock is the value. |
| **Multi-impl with strategy pattern** (pluggable backends — local vs S3 storage, JWT vs Clerk auth) | Yes | The interface IS the abstraction; multiple implementations satisfy it. |
| **Pure utility** (string formatters, math helpers, no I/O / time / state) | Optional — see "Pure-utility packages" below | Nothing meaningful to mock. |

The contract linter enforces this. By default, every `internal/` package with exported methods needs `contract.go`.

## Set the linter floor

In `forge.yaml`:

```yaml
contracts:
  strict: true              # default true — require contract.go
  allow_exported_vars: false # default false — no globals
  allow_exported_funcs: true # default true — allow free functions
  exclude:                   # opt-out for genuine pure utilities
    - "internal/buildinfo"
```

`contracts.strict: true` makes the analyzer mandatory. Run via:

```bash
forge lint --contract
```

Add this to your per-phase gate (below).

### Scope: `internal/` and `pkg/`

The contract analyzer scans **both** `internal/` and `pkg/` by default. The conceptual difference is intent:

- `internal/<name>/` — business logic. The contract pattern is mandatory here; this is where the test seam matters.
- `pkg/<name>/` — library code (utilities, clients, shared types). The contract pattern is *optional* but the linter still scans. Use `contracts.exclude: - pkg/<name>` for genuine library packages where the contract.go shape adds noise without value (e.g. `pkg/httputil`, `pkg/crypto`, packages that wrap a third-party library's idiomatic API).

Repeatedly adding `pkg/<X>` to `contracts.exclude` is a signal: either the package belongs in `internal/` (real business state → follow the contract pattern), or it's genuinely library code and the exclusion is correct. Don't reflexively exclude every `pkg/*` package.

### Packages forge should not manage: `//forge:exclude-contract: <why>`

There is ONE header directive for "forge does not manage this package". It takes a **reason**, on the same line:

```go
// Package algos is a strategy registry.
//
//forge:exclude-contract: strategy registry — one constructor per algorithm, no single New(Deps) to bind
package algos

type Strategy interface {
    Name() string
    Run(ctx context.Context, input []float64) (float64, error)
}

var registry = map[string]Strategy{}

func Register(s Strategy) { registry[s.Name()] = s }
```

It is the per-package equivalent of a `contracts.exclude` entry in `forge.yaml`: the package is skipped by the canonical-shape check, by bootstrap wiring, and by mock generation together. The trade is real — **no contract means no generated mock, no observability decorator and no seam** — so the marker costs something at lint time:

- **A bare marker is a lint error** (`forge-exclude-contract-reason`). Write why, the same way a suppressed error-severity rule needs a reason. Codegen still honours a bare marker, so a missing reason never breaks `forge generate`; it fails `forge lint`.
- **The marker is refused outright on two shapes, whatever the reason says**:
  - `forge-exclude-contract-outbound-io`: the package calls an outbound-I/O entry point — an HTTP client, `database/sql`/pgx, a controller-runtime or client-go client, a gRPC/Connect client, NATS, Redis, an OCI registry client, or a cloud SDK. (Naming those types in a struct is fine; *calling* them is the signal. Local file I/O is not a signal.) **Fix:** make it an adapter — `contract.go` with `// forge:outbound-io` and a narrow `Service`, and `// forge:constructor` on `New(Deps)` (see `adapter`).
  - `forge-exclude-contract-multi-impl`: the package declares an exported interface with two or more implementations in the module, at least one of them in the package itself. An interface with interchangeable implementations IS the contract. **Fix:** move it to `contract.go` (mark it `//forge:contract` if it is not named `Service`).

**The only legitimate reasons**, and they are few:

| Reason | Example |
|---|---|
| Pure functions / data vocabulary: no I/O, time, randomness or state | string-case helpers, a finding/severity enum, a strict decode+validate of a spec |
| Test-support code (its non-test source imports `testing`) | an `httptest` fake of a vendor, a policy-enforcing test server |
| API / CRD type packages | `api/v1alpha1` with deepcopy and `Validate()` |
| A strategy registry (an exported `Register…(I)` taking the interface) | pluggable algorithms, each with its own constructor |
| Analyzer / embed-only packages whose shape a framework dictates | a `go/analysis` sub-package, an `//go:embed` wrapper |

Packages whose kind forge already exempts structurally — the `internal/app` composition seam, supervised workers under `internal/workers/`, controller-runtime reconciler packages (they have `SetupWithManager`) — are not refused for doing I/O: their shape is a forge runtime interface, not a `Service`.

A genuinely-justified exception to a refusal (a conversion that is its own project) uses the ordinary suppression mechanism on the line above the marker, so it is visible, reasoned, and reported by `--show-suppressed`:

```go
//forge:lint-disable-next-line forge-exclude-contract-outbound-io: 40-method DB layer; per-aggregate split tracked in #123
//forge:exclude-contract: sqlc-style data layer, split pending
package db
```

You do **not** need the marker for a package with no interfaces at all (constants, structs, top-level funcs). Those are skipped automatically: a package that declares no interface cannot declare `Service`, and the rule works that out for itself. Other lint rules still apply either way.

## Prefer narrow per-aggregate interfaces over wide facades

The contract pattern works best when `Service` is **focused** — one cohesive responsibility, methods that belong together by domain. A 100+ method `Repository` interface (one struct exposing every DB operation in the system) defeats the pattern:

- Every consumer depends on every method, even the ones it doesn't use.
- Mocks become unusable in tests — you can't easily fake just the 3 methods a handler calls.
- `interfacebloat` will fire, correctly.

The remedy depends on your shape:

| Source shape | Port-time fix |
|---|---|
| Hand-written wide `Repository` interface + a concrete `*PostgresRepository` that satisfies it | Drop the wide `Repository`. Define narrow per-aggregate interfaces (`UserRepository`, `OrgRepository`, `BillingRepository`). The same concrete `*PostgresRepository` satisfies all of them structurally — Go does that for free. Each handler depends on the 10-method interface it actually needs. |
| Multi-aggregate package with wide internal surface | Split into per-aggregate packages: `internal/db/user/`, `internal/db/org/`, `internal/db/billing/`. Each gets its own narrow `contract.go`. Larger refactor; cleanest long-term. |
| sqlc-generated `Queries` struct (one method per query, all in one struct) | Generated code. Add a path-based exclude for the sqlc output dir AND document it ("generated by sqlc; cannot split"). This is the one case where `interfacebloat` is a false positive — sqlc's output shape is non-negotiable. |
| Wide interface but only one or two callers | Either narrow it inline (delete the methods nobody calls) or split per-caller. Don't keep a 50-method interface that two functions use. |

Design for the *callers*, not the implementation. Have the split-or-narrow conversation at port time, not after lint failures pile up.

## Writing a `contract.go`

One file per package, defining one (or a few cohesive) interfaces. Keep methods focused — every method becomes a mock entry, a middleware wrapper, a tracing span.

```go
// internal/email/contract.go
package email

import "context"

type Service interface {
    Send(ctx context.Context, to, subject, body string) error
    SendTemplated(ctx context.Context, to, templateID string, data map[string]any) error
}
```

Naming convention: **`Service` for single-impl, `<Name>er` for multi-impl** (`Storer`, `Authenticator`, `Validator`). Forge templates assume `Service` for the canonical case.

The implementation lives in a sibling file:

```go
// internal/email/email.go
package email

import (
    "context"
    "github.com/sendgrid/sendgrid-go"
)

type sendgridSvc struct {
    client *sendgrid.Client
}

func New(apiKey string) Service {
    return &sendgridSvc{client: sendgrid.NewSendClient(apiKey)}
}

func (s *sendgridSvc) Send(ctx context.Context, to, subject, body string) error {
    // ...
}
```

The constructor returns the interface type (`Service`), not the concrete struct. This forces consumers to depend on the abstraction.

## What `forge generate` produces

For every `contract.go` it finds, one sibling file: **`mock_gen.go`** — holding a mock for **every interface in that file, not just `Service`**. `Service` → `MockService`; a `Store` interface declared beside it → `MockStore`; a `Charger` → `MockCharger`. The scaffold emits the stub immediately, so the mocks exist from the moment the directory does.

**So never hand-roll a fake for a dep interface** — its mock is in the file next to the one you are typing in. The corollary: declare dep interfaces IN `contract.go`. One declared in `service.go` or a `ports.go` gets no mock; `contract.go` is the declared boundary, and mocking only what it declares is deliberate.

`mock_gen.go` is regenerated on every `forge generate`. **Never
hand-edit it.** If the contract changes, edit `contract.go` and re-run.

The generated file carries the canonical forge ownership header — a
`// Code generated by forge. DO NOT EDIT.` line, a `// Source:` pointer
at `contract.go`, and a `// To customize:` pointer back at the contract.
`forge lint --scaffolds` enforces both halves: hand-editing the `_gen.go`
file (or stripping its header) is a build-gating error.

### Diverging from generated CRUD wiring — the owned-shim op override

The same split governs CRUD handlers: the per-entity op wiring (`handlers_crud_ops_gen.go`) is regenerated, the delegating shims in `handlers_crud.go` are yours. To customize one RPC, never edit or disown the generated ops file — take the generated op in your shim, replace the one exported field you need, and keep delegating, so the CRUD lifecycle stays in `forge/pkg/crud` and schema/proto evolution keeps flowing underneath. The **`db` skill carries the worked recipe** (which fields are overridable, the List/single-row variants).

### The other generated sibling: `middleware_gen.go`

A package whose `func New` carries `// forge:constructor` also gets
**`middleware_gen.go`** — a decorator implementing `Service` that routes every
method through the in-process chain assembled in the OWNED `observe_chain.go`
(span, metrics, structured log, panic recovery). Add a method to `Service` and
the wrapper appears on the next `forge generate`. `// forge:no-observe` on the
constructor opts the package out; on one method, routes that method around the
chain.

That decorator is the ONLY per-package observability codegen — request-scoped
observability is Connect interceptors at the handler boundary
(`observe.Chain(observe.Deps{…})`, built in the generated `cmd serve.go`). See
`observability`.

## Using mocks in tests

Mocks come from `mock_gen.go` — one per interface in `contract.go`, so a dep interface has one too. They are drop-in for the real thing:

```go
// internal/user/service_test.go
package user

import (
    "context"
    "testing"

    "github.com/example/myproject/internal/email"
)

func TestCreateUser_SendsWelcomeEmail(t *testing.T) {
    // Set the func-field for the method you fake; unset methods
    // return "MockService.XxxFunc not set".
    mockEmail := &email.MockService{
        SendFunc: func(ctx context.Context, to, subject, body string) error {
            return nil
        },
    }

    svc := New(Deps{Email: mockEmail})
    _, err := svc.CreateUser(context.Background(), &usersv1.CreateUserRequest{
        Email: "alice@example.com",
    })

    assert.NoError(t, err)

    // The mock embeds contractkit.Recorder — assert call patterns:
    if mockEmail.CallCount("Send") != 1 {
        t.Errorf("expected 1 Send call, got %d", mockEmail.CallCount("Send"))
    }
    calls := mockEmail.Calls("Send")
    if calls[0].Args[1] != "alice@example.com" {
        t.Errorf("Send to = %v, want alice@example.com", calls[0].Args[1])
    }
}
```

For integration tests that need the real implementation, use the generated harness in that service's own `helpers_gen_test.go` (`internal/handlers/<svc>/`, `package <svc>`): `<pkg>.NewMigratedTestDB(t)` for a database with your migrations applied, then `<pkg>.NewTest<Svc>(t, <pkg>.WithDB(db))` for the real service wired to it (`<pkg>.With<Svc>Deps(...)` swaps one `Deps` field, `<pkg>.AuthedContext(t)` supplies claims). Typed entity factories live beside that same handler, in `internal/handlers/<svc>/factories_gen_test.go` (`package <svc>`), so a test in that directory calls `New<Entity>(t, db)` unqualified.

## Pure-utility packages — three options

For packages that are exclusively pure functions on no state (`naming`, `format`, math helpers):

### (A) Skip the contract
```yaml
contracts:
  allow_exported_funcs: true
  exclude:
    - "internal/naming"
```
Honest about the lack of test seam. Best for string-case conversions and pure formatters.

### (B) Wrap functions in a `Service` struct
Expose the functions as methods on a struct, define a `Service` interface in `contract.go`. Mockable, but verbose at every call site (`naming.New().ToPascalCase(s)` instead of `naming.ToPascalCase(s)`).

### (C) Hybrid
Keep free functions for ergonomics AND expose a `Service` interface whose methods delegate to them. Pick this when consumers want mockability but you don't want to break call sites.

**Decision rule:** pure utility with no I/O / time / randomness / external state → (A). Anything that touches one of those → (B) or (C). Don't apply this rule to your DB layer, HTTP clients, or anything that calls `time.Now()` — those are stateful and need a mock.

## Greenfield: `forge package new`

For new packages in an existing forge project:

```bash
forge package new <name>
```

Scaffolds `internal/<name>/`:
- `contract.go` with a starter `Service` interface
- `<name>.go` with a stub implementation
- `mock_gen.go` via `forge generate`

It will refuse if the directory already exists. For ports of existing packages, hand-write `contract.go` first (extracted from the source's exported surface), run `forge generate`, then copy the implementation behind the interface. See `migration-cli` and `migration-service` for the porting flow.

## Per-phase lint gate

```bash
forge lint --contract --exported-vars && go build ./... && go test ./...
```

If any of those three fail, the phase isn't done. Catching a contract gap at introduction time is cheap; backfilling six phases later is not.

## Rules

- One `contract.go` per package. The interface IS the public surface.
- Constructors return the interface, not the concrete struct.
- Never hand-edit `mock_gen.go`. Edit `contract.go` and re-run `forge generate`. (Pre-1.7 projects also had `middleware_gen.go` / `tracing_gen.go` / `metrics_gen.go`; those are removed by the next `forge generate`.)
- Naming: `Service` for single-impl, `<Name>er` for multi-impl pluggable backends.
- Pure utilities: pick (A), (B), or (C) explicitly. Don't leave the package half-mocked.
- `contracts.strict: true` from day one. Backfilling later is the most expensive option.

## When this skill is not enough

- **Designing proto-level contracts** (the external API surface) — see `proto` and `services`.
- **Choosing what middleware to apply** (auth, rate-limit, audit) — see `auth`, `observability`.
- **Test patterns** beyond unit-level mocking (integration, e2e) — see `testing` and its sub-skills.
- **Migrating an existing codebase** that doesn't yet have contracts everywhere — see `migration`.
