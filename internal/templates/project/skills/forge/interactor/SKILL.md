---
name: interactor
description: Use-case orchestrators that compose >=2 adapters/services. Deps are interfaces only — designed for unit-tests with all-mock collaborators.
---

# Interactor

An interactor is the package that owns one workflow: a sequence of
calls to two or more collaborators, with validation, error wrapping,
and (when needed) transaction coordination. It sits *above* adapters
and services and *below* RPC handlers.

```
internal/billing-flow/
  contract.go          # // forge:interactor — Service + dep interfaces
  interactor.go        # service struct + composition; New(Deps) Service
  interactor_test.go   # all-mock deps; assert composition order
```

Scaffold one with:

```
forge add package billing-flow --type interactor
```

This emits the four files above with the canonical Service / Deps /
New(Deps) Service shape, the `// forge:interactor` marker on
contract.go, and two placeholder dependency interfaces (`Source`,
`Sink`) that demonstrate the composition pattern.

## When to add an interactor

Add an interactor whenever you find yourself wanting to call two or
more collaborators in sequence to fulfill one user-facing operation:

- "Charge the card, then write the audit log, then publish a
  domain event" — that's an interactor over three deps.
- "Fetch the account, validate eligibility, hit the partner API" —
  one interactor over two deps + one validation step.
- "Read from queue, transform, write to storage" — one interactor
  over a queue adapter + a storage adapter.

If a workflow only calls one collaborator, you don't need an
interactor — that one method belongs on the collaborator's `Service`.
Interactors earn their keep by *composing*.

## What goes in

- Input validation (`if in.Foo == "" { return svcerr.InvalidArgument("foo") }`).
- The sequence of dep calls expressing the use case.
- Error wrapping with `fmt.Errorf("step: %w", err)` so the
  failure chain points at the failing step.
- Transaction coordination (open tx, defer rollback, commit on
  success).
- Domain-event emission after the workflow's commit point.

## What does NOT go in

- Direct calls to `net/http` / vendor SDK / queue / storage.
  That's an adapter's job. Add a `Foo Source` field to `Deps` and call
  `s.deps.Foo.Fetch(ctx, ...)`.
- Connect RPC handler logic — protobuf↔domain conversion, request
  validation tied to wire types. That's a handler's job; the handler
  *calls* the interactor.
- Cross-package state. If two interactors need to share state, the
  state belongs on a service (or a dedicated state holder both
  interactors depend on as another `Deps` field).

## Composition: deps are interfaces, always

The reason interactors are testable is that every collaborator is
behind an interface. The lint rule
`forgeconv-interactor-deps-are-interfaces` enforces this — every
field on `Deps` in a `// forge:interactor`-marked package must be an
interface type, not a concrete struct pointer.

```go
// internal/billing-flow/contract.go
// forge:interactor
package billing_flow

import "context"

type Service interface {
    ChargeAndAudit(ctx context.Context, in ChargeAndAuditInput) error
}

type ChargeAndAuditInput struct {
    UserID      string
    AmountMinor int64
}

// Dep interfaces — what this interactor needs FROM each collaborator.
// The interfaces live next to the interactor (here), not next to the
// adapter, so the interactor's full dep surface is documented in one
// place.
type Charger interface {
    Charge(ctx context.Context, userID string, amount int64) (chargeID string, err error)
}

type Auditor interface {
    Append(ctx context.Context, userID, action, refID string) error
}
```

```go
// internal/billing-flow/interactor.go
package billing_flow

import (
    "context"
    "fmt"
    "log/slog"
)

type Deps struct {
    Logger  *slog.Logger
    Charger Charger // implemented by stripe_adapter.Service in production
    Auditor Auditor // implemented by audit.Service in production
}

type service struct{ deps Deps }

func New(deps Deps) Service { return &service{deps: deps} }

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

Concrete adapters (`stripe_adapter.Service`, `audit.Service`) get
wired in `pkg/app/setup.go`:

```go
app.BillingFlow = billing_flow.New(billing_flow.Deps{
    Logger:  app.Logger,
    Charger: app.Stripe, // implements billing_flow.Charger
    Auditor: app.Audit,  // implements billing_flow.Auditor
})
```

The interactor is unaware that `Charger` is Stripe (or Adyen, or a
test fake) — that's the point.

## How to test

All-mock deps. Hand-rolled (small surfaces) or generated mocks (large
surfaces). The shape:

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
    err      error
}
func (f *fakeAuditor) Append(_ context.Context, u, a, r string) error {
    f.appended = append(f.appended, auditCall{u, a, r})
    return f.err
}

func TestChargeAndAudit_HappyPath(t *testing.T) {
    t.Parallel()
    auditor := &fakeAuditor{}
    svc := New(Deps{
        Logger:  slog.Default(),
        Charger: &fakeCharger{chargeID: "ch_1"},
        Auditor: auditor,
    })
    if err := svc.ChargeAndAudit(ctx, ChargeAndAuditInput{UserID: "u1", AmountMinor: 100}); err != nil {
        t.Fatalf("unexpected: %v", err)
    }
    if len(auditor.appended) != 1 || auditor.appended[0].refID != "ch_1" {
        t.Fatalf("auditor not called with charge id: %+v", auditor.appended)
    }
}

func TestChargeAndAudit_ChargeFailsShortCircuits(t *testing.T) {
    t.Parallel()
    auditor := &fakeAuditor{}
    svc := New(Deps{
        Logger:  slog.Default(),
        Charger: &fakeCharger{err: errors.New("decline")},
        Auditor: auditor,
    })
    if err := svc.ChargeAndAudit(ctx, ChargeAndAuditInput{UserID: "u1", AmountMinor: 100}); err == nil {
        t.Fatal("expected error")
    }
    if len(auditor.appended) != 0 {
        t.Fatalf("auditor must not be called when charge fails: %+v", auditor.appended)
    }
}
```

The two assertions interactor tests typically need:

1. **Composition order.** "When step 1 succeeds, step 2 is called with
   step 1's output." Hand-rolled fakes that record their inputs make
   this trivial.
2. **Failure short-circuiting.** "When step 1 fails, step 2 is not
   called." This is the pattern that catches half-applied workflows.

## Wiring in `pkg/app/setup.go`

The interactor sits between adapters and handlers. Setup wires the
adapters first, then the interactor on top, then the handler depends
on the interactor:

```go
func Setup(app *App) error {
    app.Stripe = stripe_adapter.New(stripe_adapter.Deps{...})
    app.Audit  = audit.New(audit.Deps{...})

    app.BillingFlow = billing_flow.New(billing_flow.Deps{
        Logger:  app.Logger,
        Charger: app.Stripe, // adapter satisfies the dep interface
        Auditor: app.Audit,
    })

    app.BillingHandler = billinghandler.New(billinghandler.Deps{
        Flow: app.BillingFlow, // handler depends on interactor's Service
    })
    return nil
}
```

## Rules

- `// forge:interactor` marker comment on the package doc in
  `contract.go`. Lint enforces this.
- Every field in `Deps` is an interface type, not a concrete struct
  pointer (`forgeconv-interactor-deps-are-interfaces`). Concrete
  pointers defeat the all-mock test surface.
- Two or more collaborators in `Deps`. A one-dep interactor is a
  smell — that method belongs on the dep itself.
- Interactors don't call third-party systems, only adapters that wrap
  them.
- Tests use all-mock deps; never live downstreams.
- Validation lives at the interactor's edge (top of each method);
  adapters trust validated input.

## When this skill is not enough

- **The collaborator boundary itself** (third-party calls, response
  mapping) — see `adapter`.
- **The Service / Deps / New shape** — see `service-layer`.
- **Handler-side validation and error wrapping** — see `api/handlers`.
- **Translating vendor errors into svcerr sentinels at the boundary** —
  see `service-layer`'s errors section and `forge/pkg/svcerr`.
