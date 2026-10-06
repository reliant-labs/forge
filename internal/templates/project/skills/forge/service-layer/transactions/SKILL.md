---
name: transactions
description: Transactions in a forge service — s.deps.DB.RunTx carries a SERIALIZABLE, conflict-retried transaction in ctx so stores and generated delegates join it; orm.AfterCommit for side effects; the lock-free state-transition recipe; how RunTransaction/WithTx relate.
---

# Transactions: `RunTx` carries the transaction in `ctx`

**The transaction lives on `DB orm.Context`, not on the store.** The store is
the data surface; `orm.Context` owns the transaction. Declare both when a use
case writes atomically — `DB orm.Context` is the canonical field forge already
wires, so it costs nothing:

```go
type Deps struct {
    DB        orm.Context      // the transaction boundary: DB.RunTx(ctx, …)
    Estimates db.EstimateStore // the data surface
    Jobs      db.JobStore
}

err := s.deps.DB.RunTx(ctx, func(ctx context.Context) error {
    est, err := s.deps.Estimates.GetEstimateByID(ctx, id) // joins: ctx carries the tx
    if err != nil {
        return err
    }
    if err := s.deps.Estimates.UpdateEstimate(ctx, est); err != nil {
        return err
    }
    return s.deps.Jobs.CreateJob(ctx, job)
})
```

Every query made with the `ctx` fn receives — store methods, generated
delegates (`db.GetJobByID(ctx, s.deps.DB, id)`), `pkg/crud`, `db.Bun()`
builders and raw SQL through `s.deps.DB` — runs inside the transaction. There
is no handle to thread and nothing to rebind. A `RunTx` whose `ctx` already
carries a transaction on the same database **joins** it (its options are
ignored), so a service method that wraps its own work in `RunTx` composes into
a caller's larger one unchanged. A transaction is only ever used for its own
database: a `ctx` carrying one for database A never runs a statement sent to B.

## What `RunTx` guarantees, and what it asks of you

- **SERIALIZABLE by default**, retried with jittered backoff on serialization
  failure and deadlock (SQLSTATE `40001` / `40P01`), up to
  `orm.DefaultTxMaxAttempts`. Still conflicting after that → a `svcerr.Aborted`
  error, which `svcerr.Wrap` maps to `CodeAborted`.
- **`fn` may run more than once.** A retry rolls back the failed attempt's
  database work and calls `fn` again from the top, so `fn` must have no effect
  outside the database. Send the email, publish the event, enqueue the job with
  `orm.AfterCommit(ctx, func(ctx context.Context) { … })` — it runs once,
  after the outermost commit; a rolled-back or retried attempt discards it.
  Outside a transaction `AfterCommit` returns `orm.ErrNoTransaction`.
- `fn`'s own error comes back verbatim, so your sentinels survive.
- `s.deps.DB.RunTxReadOnly(ctx, fn)` for a multi-statement read that must see
  one consistent snapshot (SERIALIZABLE READ ONLY DEFERRABLE: no predicate
  locks, never aborted). `RunTxWithOptions(ctx, orm.TxOptions{…}, fn)` sets
  `Isolation`, `ReadOnly` or `MaxAttempts` explicitly; anything weaker than
  SERIALIZABLE is a correctness decision that needs a reason at the call site.
- The transaction is one connection: don't fan `ctx` out to goroutines inside
  `fn`.

## Recipe: a state-transition RPC

Approve an estimate, pay an invoice, move a job to SCHEDULED: read the row,
check the transition is legal, write the change. That is the whole method —
**no `SELECT … FOR UPDATE`, no per-entity `Lock<Entity>` helper:**

```go
func (s *svc) RecordPayment(ctx context.Context, in RecordPaymentInput) (Invoice, error) {
    var out *db.Invoice
    err := s.deps.DB.RunTx(ctx, func(ctx context.Context) error {
        inv, err := s.deps.Invoices.GetInvoiceByID(ctx, in.InvoiceID) // get
        if err != nil {
            return err
        }
        if inv.Status != "SENT" {                                        // check
            return svcerr.FailedPrecondition("only a sent invoice can take a payment")
        }
        if in.AmountCents > inv.BalanceCents {
            return svcerr.FailedPrecondition("payment exceeds the balance")
        }
        inv.PaidCents += in.AmountCents                                  // masked update
        if err := s.deps.Invoices.UpdateInvoiceMasked(ctx, inv, []string{"paid_cents"}); err != nil {
            return err
        }
        out = inv
        return orm.AfterCommit(ctx, func(ctx context.Context) {
            s.deps.Events.PaymentRecorded(ctx, inv.Id) // side effect: after commit, once
        })
    })
    return toInvoice(out), err
}
```

Why that is safe without a lock: two concurrent payments both read the same
balance, and under SERIALIZABLE postgres detects that the two transactions
cannot be ordered one after the other. It aborts one with `40001`; `RunTx`
re-runs that one from the top, where it reads the **committed** balance and
takes the branch that is now correct. A lock helper protects only the code
paths that remember to call it; serializable isolation protects every
read-check-write in the transaction, including the next one someone adds.

Assign whatever the RPC returns from inside `fn` (as `out` above) so the value
comes from the attempt that committed.

## `RunTransaction` and `WithTx`

`RunTransaction(ctx, func(tx orm.Context) error)` is the older,
handle-passing API: database-default isolation, one attempt, no retry. It is
kept for existing callers and pairs with the stores' `WithTx(tx)`. Its `fn`
receives a handle, not a transaction-carrying `ctx`, so code called with the
outer `ctx` and the plain client does NOT participate — use the handle, or
`tx.RunTx(ctx, fn)` to get such a `ctx`. It joins a `RunTx` transaction
already in `ctx`. Write new code with `RunTx`.

## Migrating hand-written lock helpers

A `Lock<Entity>(ctx, tx, id)` that runs `SELECT … FOR UPDATE` becomes the
generated `Get<Entity>ByID(ctx, s.deps.DB, id)` (or the store's method) called
with `RunTx`'s `ctx`; the `RunTransaction(ctx, func(tx orm.Context) …)`
around it becomes `s.deps.DB.RunTx(ctx, func(ctx context.Context) …)`, and
every `tx` argument inside becomes `s.deps.DB`. Delete the helper. Move any
side effect inside the closure into `orm.AfterCommit`.

See also: `service-layer` for the Deps/store shape, `db/crud-overrides` for
read-check-write inside a CRUD op override.
