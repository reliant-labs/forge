---
name: crud-overrides
description: Diverge from generated CRUD without forking the projection — override op fields in your owned handlers_crud.go, and the per-op seams (op.Filters, op.Persist) a per-caller policy attaches to.
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

## Where a per-caller policy attaches

Forge enforces no policy of its own, so if rows are restricted to some callers,
these are the seams that restriction attaches to. What the rule should be is
yours; these are the mechanics of each op, and the constraints are not obvious
from the struct:

- **List** — `op.Filters`. The generated closure is **nil unless the RPC
  declares filter fields**, so capture and nil-check it before calling, or you
  silently drop the per-field filters. Valid column names are exactly
  `db.<Entity>Columns`. A predicate belongs in the query, not after it — the
  response's `total_count` comes from a COUNT over the same filters, so
  post-filtering a page leaves the total reporting rows the caller cannot see.
- **Get / Update / Delete** — the single-row repo funcs (`db.Get<Entity>ByID`,
  …) take **no** `orm.QueryOption`, so a check here means fetch first, compare,
  then delegate. The row is read twice; that is the honest cost.
- **Anything reachable only via a join** — `orm.QueryOption` is
  `func(*bun.SelectQuery)` and orm ships no join helper, so drop to raw bun:
  `q.Where("id IN (SELECT intake_id FROM assignments WHERE provider_id = ?)", claims.UserID)`.
  For anything the op cannot express, hand-roll against `s.deps.DB`.
- **Create** — `op.Entity` runs with the request only; it takes **no ctx**, so
  a value read from the context (claims, a request-scoped id) has to be
  resolved in the handler body and captured, or applied in `op.Persist`, which
  does take one.

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
op.Entity = func(req *pb.CreateJobRequest) (*db.Job, error) {
    e, err := build(req)
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
