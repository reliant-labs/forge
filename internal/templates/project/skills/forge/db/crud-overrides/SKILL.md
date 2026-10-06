---
name: crud-overrides
description: Diverge from generated CRUD without forking the projection — override op fields in your owned handlers_crud.go, the per-op seams (op.Filters, op.Fetch, op.Persist) a per-caller policy attaches to, and what the Update op already does for read-only fields.
---

# Diverging From Generated CRUD

The per-entity CRUD wiring (`handlers_crud_ops_gen.go`, including
`<entity>ToProto`/`<entity>FromProto`) is Tier-1 and keeps regenerating. When
one RPC needs custom behavior — extra wire-only response fields, a different
persist path — do NOT disown the ops file. Your **owned** `handlers_crud.go`
shim takes the generated op and overrides the exported field it needs before
delegating:

```go
// handlers_crud.go (yours) — diverge one op without forking the projection:
func (s *Service) CreateInvoice(ctx context.Context, req *connect.Request[pb.CreateInvoiceRequest]) (*connect.Response[pb.CreateInvoiceResponse], error) {
    op := s.crudCreateInvoiceOp() // generated wiring, still regenerated
    op.Pack = func(e *db.Invoice) (*pb.CreateInvoiceResponse, error) {
        m, err := invoiceToProto(e) // returns an error on a corrupt stored enum
        if err != nil {
            return nil, err
        }
        return &pb.CreateInvoiceResponse{Invoice: m}, nil // + custom wire-only fields
    }
    return crud.HandleCreate(op)(ctx, req)
}
```

Every exported op field (`Entity`, `Pack`, `Persist`, `Mask`, ...) is
overridable the same way; the lifecycle (persist → pack, error mapping) stays
in `forge/pkg/crud`, and schema/proto changes keep flowing through the
regenerated op underneath your override.

## Which file, and when forge emits an op

`handlers_crud.go` is where forge *appends* shims; it is not where a CRUD
method has to *stay*. Move a shim — delegating, overridden, or scoped — to
any file of the handler package and the next `forge generate` treats it the
same. Per CRUD rpc, over every file of the package:

- **no method declares the rpc** → forge appends a delegating shim to
  `handlers_crud.go` and emits `crud<Rpc>Op`;
- **a method declares it and anything in the package calls
  `s.crud<Rpc>Op`** → the op is emitted (and no shim is appended);
- **a method declares it and nothing calls the op** → the rpc is
  implemented by hand: no op is emitted, and the op's own checks (a list
  filter naming no column, an Update on an append-only table) do not apply.
  This is the escape hatch those errors point you at.

`<entity>ToProto` / `<entity>FromProto` stay emitted while anything in the
package calls them — a custom rpc projecting rows keeps its helper even when
every CRUD rpc of that entity is hand-written. Declare your own function of
that name and forge stops emitting it.

## Where a per-caller policy attaches

Forge enforces no policy of its own, so if rows are restricted to some callers,
these are the seams that restriction attaches to. What the rule should be is
yours; these are the mechanics of each op, and the constraints are not obvious
from the struct:

Every request-projecting closure takes a `context.Context`, so the caller's
claims are reachable from all of them. If a table declares `forge:owner`, forge
scaffolds these overrides for you — see `auth/authorization`; what follows is
the mechanics for everything else.

- **List** — `op.Filters`, signature
  `func(ctx, *Req) ([]orm.QueryOption, error)`. The generated closure is **nil
  unless the RPC declares filter fields**, so capture and nil-check it before
  calling, or you silently drop the per-field filters. Valid column names are
  exactly `db.<Entity>Columns`. A predicate belongs in the query, not after it —
  the response's `total_count` comes from a COUNT over the same filters, so
  post-filtering a page leaves the total reporting rows the caller cannot see.
  The `error` return is there so a closure that cannot determine the scope can
  **fail closed**: returning no filter is not "no restriction", it is every row.
- **Get / Update / Delete** — `op.Fetch`, `op.Persist` and `op.PersistMasked`
  take trailing `opts ...orm.QueryOption`. Append your predicate and the
  generated shim composes it into the query, so the check is one statement, not
  a fetch-then-compare. On Update the predicate is evaluated against the
  **stored** row, which is what stops a caller reassigning someone else's row by
  rewriting the owner column in their request.
- **Anything reachable only via a join** — `orm.QueryOption` is
  `func(*bun.SelectQuery)` and orm ships no join helper, so drop to raw bun:
  `q.Where("id IN (SELECT intake_id FROM assignments WHERE provider_id = ?)", claims.UserID)`.
  For anything the op cannot express, hand-roll against `s.deps.DB`.
- **Create** — `op.Entity`, signature `func(ctx, *Req) (Ent, error)`. Stamp an
  owner column here, from the claims on `ctx` and never from a wire field: a
  create that took the owner off the request would let anyone file a row under
  anyone else's name.

### A column that is not on the wire, and full-replace Update

A column added by migration but absent from the proto is **not** written by
`<entity>FromProto`, so a maskless Update builds an entity carrying its zero
value — and that zero lands on the row, overwriting what was stored. Declare
the column to opt out:

```sql
COMMENT ON COLUMN crews.company_id IS 'forge:immutable';
```

`forge generate` projects that into the Bun tag (`,skipupdate`) and Bun omits
the column from the full-replace `SET` clause, so an override does not need to
re-stamp it. A **masked** Update naming the column still writes it: that is a
caller asserting a value on purpose, not a request built without knowledge of
the column.

Note what this does and does not cover. `forge:immutable` decides what the
framework may **write**; it says nothing about **who** may write the row. If
that second question matters here, `op.Persist` / `op.PersistMasked` are where
it gets answered, and answering it means reading the stored row rather than
trusting the submitted entity.

### An override that reads, checks, then writes

When a `Persist` / `PersistMasked` override (or any custom RPC) loads the
stored row, decides whether the change is allowed, and then writes, wrap the
three steps in `s.deps.DB.RunTx` and use the `ctx` it hands you for all of
them:

```go
// (This op carries no per-caller predicate; if yours does, the read must honor
// opts — see "Where a per-caller policy attaches" above.)
op.PersistMasked = func(ctx context.Context, e *db.Job, fields []string, _ ...orm.QueryOption) error {
    return s.deps.DB.RunTx(ctx, func(ctx context.Context) error {
        stored, err := db.GetJobByID(ctx, s.deps.DB, e.Id) // inside the transaction
        if err != nil {
            return err
        }
        if stored.Status == "COMPLETED" {
            return svcerr.FailedPrecondition("a completed job cannot be edited")
        }
        return db.UpdateJobMasked(ctx, s.deps.DB, e, fields)
    })
}
```

No `SELECT … FOR UPDATE`: the generated delegates join the transaction
carried by `ctx`, and `RunTx`'s SERIALIZABLE-plus-retry turns a concurrent
writer into a re-run that sees the committed row. The closure may run more
than once, so keep side effects out of it (`orm.AfterCommit`).
`service-layer/transactions` explains why this is safe.

### Read-only fields are already handled — do not overlay them

The generated Update op carries the entity's `// forge:read-only` and
`// forge:computed` columns twice, and both halves survive an override that
wraps rather than replaces:

```go
// handlers_crud_ops_gen.go (generated)
ReadOnly: []string{"status", "shipped_at"},       // HandleUpdate refuses a mask naming one
Persist: ... db.UpdateOrder(ctx, s.deps.DB, entity,
        crud.Preserve("status", "shipped_at")) ... // a full replace leaves them as stored
```

So there is no need to reload the stored row and copy the editable fields onto
it, or to filter lifecycle paths out of the mask — the op does both, and the
response reports the stored values (every Update reads back the columns it did
not write). Keep an override for what is genuinely yours: a business rule, a
cross-entity check, scoping.

Two things to know when overriding:

- **Replacing `op.Persist` outright** (not wrapping it) drops `crud.Preserve`
  with it. Pass it yourself, from the op so the list cannot drift:
  `db.UpdateOrder(ctx, s.deps.DB, e, crud.Preserve(op.ReadOnly...))`.
- **A derived value is not written by the Update op.** Setting `row.Total` in
  an Update `op.Entity` hook has no effect on a read-only column — that is the
  guarantee, not a bug. Same-row derivations belong in a `GENERATED` column;
  cross-row ones are written by the code that changes their inputs, with
  `db.Update<Entity>Masked(ctx, s.deps.DB, e, []string{"total_cents"})`.

AIP-203 asks servers to *ignore* output-only `update_mask` paths; forge refuses
them instead, because a 200 that silently changed nothing is the failure this
exists to prevent. An override that wants the AIP behaviour sets
`op.ReadOnly = nil` and filters the paths in `op.PersistMasked`.

## Errors from an override must be classified

Return `svcerr` sentinels (or a `*connect.Error`) from any op closure —
`svcerr.NotFound("order")`, `svcerr.InvalidArgument("...")`. `pkg/crud` reads
the classification and preserves your code and message.

An error that carries **no** classification is treated as a server fault: it
becomes `Internal`, its text is replaced with a safe message, and the original
is kept as a server-side cause for the logs. That is right for a driver or SDK
error nobody triaged, and wrong for a decision you made — so make the decision
explicit rather than returning a bare `errors.New`.

```go
op.Entity = func(ctx context.Context, req *pb.CreateJobRequest) (*db.Job, error) {
    e, err := build(ctx, req)
    if err != nil {
        return nil, err
    }
    if !s.ownsCustomer(ctx, e.CustomerId, claims.OrgID) {
        return nil, svcerr.InvalidArgument("customer_id does not name a customer of this org")
    }
    return e, nil
}
```

See also: `db` for the schema/ORM half, `service-layer` for where business
logic belongs.
