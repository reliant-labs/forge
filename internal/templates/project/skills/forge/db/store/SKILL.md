---
name: store
description: Which tables (and views) get a generated store and how it is shaped — every applied table is projected whatever its rpcs, the forge:no-entity opt-out, entity names derived from the table, and where CRUD wiring differs.
---

# The generated store

`forge generate` projects the **data layer from the applied schema**, not from
the wire. Every applied BASE TABLE gets:

- an entity struct (`<entity>_orm_gen.go`), its `<E>Field*` / `<E>Columns`
  constants and constraint names;
- package-level delegates (`Create<E>`, `Get<E>ByID`, `List<E>`, `Count<E>`,
  `Update<E>`, `Update<E>Masked`, `Delete<E>`);
- an `<E>Store` interface in `store_gen.go` (and the aggregate `Store`);
- a scaffold-once `<entity>_repo_ext.go` seam for hand-written queries.

whether or not any service declares rpcs for the table — a table whose
CRUD-named rpcs are all `// forge:custom`, or that no rpc names at all, still
has its store. **CRUD wiring** (handlers, pages, seed roots) is the separate,
wire-driven projection described in `db`.

## Naming

A table no CRUD rpc claims is named from the table: the singular,
camel-cased the way protoc-gen-go spells a message (`message_content_blocks` →
`MessageContentBlock`, `api_keys` → `ApiKey`). A table a CRUD rpc claims keeps
the wire message's name, so the store and the wiring agree. Two tables that
project to one name fail generate, naming both.

## Opting out

- One table: `COMMENT ON TABLE t IS 'forge:no-entity'` in a migration. No
  store, and no CRUD wiring over it either (a CRUD-named rpc over it is custom).
- The whole data layer: `//forge:no-orm: <why>` in `internal/db`'s package doc
  (`db/orm-and-safety`).

## Keys

A single-column primary key is the `id` the delegates take: `Get<E>ByID`,
`Delete<E>ByID`… A **composite primary key** gets a store addressed by the
whole key — `Get<E>ByPK(ctx, db, k1, k2)` and `Delete<E>ByPK` (one parameter
per key column), `Upsert<E>` (conflict on every key column), plus
Create/List/Count/Update/UpdateMasked addressing the row by the struct's key
fields — but no CRUD wiring, which keys on one id, and no ULID fill or cursor
pagination. A table with no primary key is reported and skipped.
