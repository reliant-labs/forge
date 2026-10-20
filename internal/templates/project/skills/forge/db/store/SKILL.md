---
name: store
description: Which tables (and opted-in views) get a generated store and how it is shaped — every applied table is projected whatever its rpcs, the forge:no-entity opt-out, entity names derived from the table, and where CRUD wiring differs.
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

A table with no primary key is reported and skipped. A composite primary key
is reported and skipped too.

## Views

A VIEW (regular or materialized) is projected only when it opts in —
`COMMENT ON VIEW v IS 'forge:entity'` (`key=<column>` to name its key, else
`id`) — as a read-only entity: the row struct, `List<E>` / `Count<E>` and
`Get<E>ByID` when it has a key, and a read-only `<E>Store`. Nothing writes. Its
non-key columns are pointers, because the catalog cannot say a view column is
NOT NULL.
