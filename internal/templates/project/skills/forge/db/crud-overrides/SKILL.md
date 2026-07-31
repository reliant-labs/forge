---
name: crud-overrides
description: Diverge from generated CRUD without forking the projection — override op fields in your owned handlers_crud.go, and scope rows to the caller via op.Filters or a fetch-then-compare.
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

## Scoping rows to the caller

"A customer sees only their own orders" is the same override applied to
`op.Filters` — a hand-written `WHERE` in the owned handler, never codegen:

- **List** — wrap `op.Filters` and `append` an
  `orm.WhereEq("customer_id", claims.UserID)`. The generated closure is **nil
  unless the RPC declares filter fields**, so capture it and nil-check before
  calling it, or you drop the per-field filters. Valid column names are exactly
  those in `db.<Entity>Columns`.
- **Get / Update / Delete** — the single-row repo funcs (`db.Get<Entity>ByID`,
  …) take **no** `orm.QueryOption`, so fetch first, compare ownership, then
  delegate. Return `NotFound` rather than `PermissionDenied` so you don't leak
  that the row exists. The row is read twice; that's the honest cost.
- **Owner reachable only via a join** — `orm.QueryOption` is
  `func(*bun.SelectQuery)` and orm ships no join helper, so drop to raw bun:
  `q.Where("id IN (SELECT intake_id FROM assignments WHERE provider_id = ?)", claims.UserID)`.
  For anything the op can't express, hand-roll the query against `s.deps.DB`.

See also: `db` for the schema/ORM half, `service-layer` for where business
logic belongs.
