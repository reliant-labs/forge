---
name: interactor
description: Use-case orchestrators that compose two or more adapters/services. Deps are interfaces only — designed for unit tests with all-mock collaborators.
emit: both
---

# Interactor

An interactor is the package that owns one workflow: a sequence of calls to two or more collaborators, with validation, error wrapping, and (when needed) transaction coordination. It sits *above* adapters and services and *below* the transport / handler layer.

## When to add an interactor

Add an interactor whenever you find yourself wanting to call two or more collaborators in sequence to fulfill one user-facing operation:

- "Charge the card, then write the audit log, then publish a domain event" — an interactor over three deps.
- "Fetch the account, validate eligibility, hit the partner API" — one interactor over two deps + one validation step.
- "Read from queue, transform, write to storage" — one interactor over a queue adapter + a storage adapter.

If a workflow only calls one collaborator, you don't need an interactor — that one method belongs on the collaborator's own interface. Interactors earn their keep by *composing*.

## What goes in

- Input validation at the top of each method (`if in.Foo == "" { return ... }`).
- The sequence of dep calls expressing the use case.
- Error wrapping so the failure chain points at the failing step (`fmt.Errorf("step: %w", err)` in Go; equivalents elsewhere).
- Transaction coordination — open tx, defer rollback, commit on success.
- Domain-event emission after the workflow's commit point.

## What does NOT go in

- Direct calls to HTTP / vendor SDK / queue / storage. That's an adapter's job. Add a `Foo Source` field to `Deps` and call `s.deps.Foo.Fetch(ctx, ...)`.
- Transport-layer logic — request/response shape conversion, validation tied to wire types. That's the handler's job; the handler *calls* the interactor.
- Cross-package state. If two interactors need to share state, the state belongs on a service (or a dedicated state holder both interactors depend on as another `Deps` field).

## Composition: deps are interfaces, always

The reason interactors are testable is that **every collaborator is behind an interface**. Concrete struct pointers in the dep set defeat the all-mock test surface and force tests to drag in real downstreams.

```go
type Service interface {
    ChargeAndAudit(ctx context.Context, in ChargeAndAuditInput) error
}

// Dep interfaces — what this interactor needs FROM each collaborator.
// The interfaces live next to the interactor (here), not next to the
// adapter, so the interactor's full dep surface is documented in one place.
type Charger interface {
    Charge(ctx context.Context, userID string, amount int64) (chargeID string, err error)
}

type Auditor interface {
    Append(ctx context.Context, userID, action, refID string) error
}
```

```go
type Deps struct {
    Logger  *slog.Logger
    Charger Charger // implemented by a concrete adapter in production
    Auditor Auditor
}

type service struct{ deps Deps }

func New(deps Deps) (Service, error) { return &service{deps: deps}, nil }

func (s *service) ChargeAndAudit(ctx context.Context, in ChargeAndAuditInput) error {
    if in.UserID == "" || in.AmountMinor <= 0 {
        return fmt.Errorf("billing-flow: invalid input")
    }
    chargeID, err := s.deps.Charger.Charge(ctx, in.UserID, in.AmountMinor)
    if err != nil {
        return fmt.Errorf("billing-flow: charge: %w", err)
    }
    if err := s.deps.Auditor.Append(ctx, in.UserID, "charge", chargeID); err != nil {
        return fmt.Errorf("billing-flow: audit: %w", err)
    }
    return nil
}
```

The interactor is unaware that `Charger` is Stripe (or Adyen, or a test fake) — that's the point.

## Late-bound dependencies: construct-then-inject in `Build`

Sometimes collaborator B needs a value that only exists *after* collaborator A is constructed (worker A produces a snapshot saver that worker B consumes; service X exposes a registry that interactor Y registers handlers into). Putting that value in B's `Deps` creates a construction-order cycle — `New(Deps)` resolves its dep closure once and has no slot for "set this later".

Construct-then-inject is a first-class plain method call in the composition root, not a framework hook. In `Build`, construct A and B, then call B's setter with A's product:

```go
func BuildServer(infra Infra) (*serverkit.Server, error) {
    snapshotter := snapshot.New(snapshot.Deps{...})
    trader := trader.New(trader.Deps{...})

    // two-phase wiring: B's setter, after both ends exist
    trader.SetSnapshotSaver(snapshotter.SnapshotSaver())
    ...
}
```

This is just ordinary Go in the root you own — no `PostBootstrap`, no `*App` field read by name, no parallel hook system. Near-diamonds and post-construction setters (`bill.WithReliantAPIKeyIssuer(llm)`) are the same pattern: construct, then inject. Any model based on pure constructor topo-ordering deadlocks on the real graph; the composition root supports construct-then-inject explicitly.

For the related case where a typed Deps field can't reference its target yet because the owning lane hasn't merged, see `forge:placeholder` in the `api` skill — that's a generate-time mechanism for cross-lane parallel work, distinct from the runtime construct-then-inject case above.

## How to test

All-mock deps. Hand-rolled fakes for small surfaces, generated mocks for large ones. The shape:

```go
type fakeCharger struct {
    chargeID string
    err      error
}
func (f *fakeCharger) Charge(_ context.Context, _ string, _ int64) (string, error) {
    return f.chargeID, f.err
}

type fakeAuditor struct {
    appended []auditCall
}
func (f *fakeAuditor) Append(_ context.Context, u, a, r string) error {
    f.appended = append(f.appended, auditCall{u, a, r})
    return nil
}

func TestChargeAndAudit_HappyPath(t *testing.T) {
    auditor := &fakeAuditor{}
    svc, _ := New(Deps{Charger: &fakeCharger{chargeID: "ch_1"}, Auditor: auditor})
    _ = svc.ChargeAndAudit(ctx, ChargeAndAuditInput{UserID: "u1", AmountMinor: 100})
    if len(auditor.appended) != 1 || auditor.appended[0].refID != "ch_1" {
        t.Fatalf("auditor not called with charge id: %+v", auditor.appended)
    }
}
```

The two assertions interactor tests typically need:

1. **Composition order.** "When step 1 succeeds, step 2 is called with step 1's output." Hand-rolled fakes that record their inputs make this trivial.
2. **Failure short-circuiting.** "When step 1 fails, step 2 is not called." This is the pattern that catches half-applied workflows.

## Rules

- **Every dep is an interface, not a concrete type.** Concrete deps defeat the all-mock surface and force tests to drag in real downstreams.
- **Two or more collaborators.** A one-dep interactor is a smell — that method belongs on the dep itself.
- **Interactors don't call third-party systems directly**, only adapters that wrap them.
- **Tests use all-mock deps**; never live downstreams.
- **Validation lives at the interactor's edge** (top of each method); adapters trust validated input.

<!-- @forge-only:start -->
## Forge scaffolding

Scaffold an interactor with:

```
forge add package billing-flow --type interactor
```

This emits the canonical package layout:

```
internal/billing-flow/
  contract.go          # // forge:interactor — Service + dep interfaces
  interactor.go        # service struct + composition; New(Deps) Service
  interactor_test.go   # all-mock deps; assert composition order
```

Plus two placeholder dep interfaces (`Source`, `Sink`) that demonstrate the composition pattern — replace them with the real interfaces your workflow needs.

## Marker comment and lint enforcement

Every interactor package's `contract.go` carries a `// forge:interactor` marker comment. One lint rule depends on it:

- `forgeconv-interactor-deps-are-interfaces` — every field on `Deps` in a `// forge:interactor`-marked package must be an interface type, not a concrete struct pointer. Concrete pointers defeat the all-mock test surface.

## Wiring in the typed composition root (`Build`)

The interactor is wired in the binary's owned composition root (`internal/app/build.go`), as typed interface fills in one place: adapters first, then the interactor on top, then the handler depending on the interactor. Each fill is a local variable resolved by type — no `*App` fields, no name-matching, no string-keyed registry.

```go
func BuildServer(infra Infra) (*serverkit.Server, error) {
    charger := stripeadapter.New(stripeadapter.Deps{...})
    auditor := audit.New(audit.Deps{...})

    flow, err := billingflow.New(billingflow.Deps{
        Logger:  infra.Logger,
        Charger: charger, // adapter satisfies the dep interface
        Auditor: auditor,
    })
    if err != nil {
        return nil, err
    }

    billingHandler := billinghandler.New(billinghandler.Deps{Flow: flow})
    // ... mount billingHandler, assemble the Server
}
```

Each collaborator is filled by its interface, so swapping the real in-process adapter for a Connect client or a mock is a one-line change here with the interactor untouched. That is also what makes "spin up this interactor with all collaborators mocked" a few-line call against `Build`.

## When this skill is not enough (forge sub-skills)

- **The collaborator boundary itself** (third-party calls, response mapping) — see `adapter`.
- **The Service / Deps / New shape** — see `service-layer`.
- **Handler-side validation and error wrapping** — see `api`.
- **Translating vendor errors into svcerr sentinels at the boundary** — see `service-layer`'s errors section and `forge/pkg/svcerr`.
<!-- @forge-only:end -->
