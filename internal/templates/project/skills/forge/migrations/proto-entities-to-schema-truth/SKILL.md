---
name: proto-entities-to-schema-truth
description: Migrate a forge project from proto-annotated entities ((forge.v1.entity)/(forge.v1.field), proto/db entity files) to schema-truth entities projected from db/migrations. The annotations are retired and ignored; SQL is the schema language now.
relevance: migration
---

# Migrating from proto entities to schema truth

Use this skill when a project still carries `(forge.v1.entity)` /
`(forge.v1.field)` annotations on proto messages, or a `proto/db/`
directory of entity protos. Those are the OLD entity model. In the new
model there is **no proto-side schema declaration at all**:
`db/migrations/*.up.sql` is the single source of truth, and
`forge generate` applies the migrations to an in-memory shadow database,
introspects the tables, and projects entity structs, the ORM
(`internal/db/<entity>_orm.go`), CRUD wiring, and frontend pages from
the real schema.

## 1. What changed

Before, a message carrying `option (forge.v1.entity) = {...}` (plus
per-field `(forge.v1.field)` options) declared a database entity, and
the ORM was generated from the proto. After:

- **The annotations are retired and IGNORED.** Their definitions remain
  in `forge/v1/forge.proto` only as deprecated tombstones so legacy
  protos keep compiling for one release.
- **Entities are detected from two halves:** a service proto declaring
  the CRUD RPCs (`Create<X>`/`Get<X>`/`Update<X>`/`Delete<X>`/`List<Xs>`
  — the wire truth) AND the applied schema having the matching table
  (pluralized snake_case — the storage truth). One without the other
  generates nothing.
- **Behavior comes from real columns, not annotation fields:**
  `deleted_at` ⇒ soft delete, `created_at`+`updated_at` ⇒ managed
  timestamps, `tenant_id` ⇒ tenant scoping, text columns ⇒ the list
  `search` filter, every column ⇒ the order_by/filter allowlist.
- **The entity struct follows the schema:** `time.Time` for timestamp
  columns, pointers for nullable columns, native slices for arrays. The
  generated `<entity>ToProto` / `<entity>FromProto` conversions map the
  intersection of wire fields and columns by name.

Your migrations were already the de-facto truth — proto entities were a
parallel declaration that drifted. The flip removes the drift channel.

## 2. The generate-time notice

On a project that still carries the annotation, `forge generate` prints
one line per affected message:

```
ℹ️  <file>: message <Name> carries the retired (forge.v1.entity) annotation — it is now ignored.
   SQL is the schema: db/migrations drive the ORM/entity projections.
   Your migrations are already the truth; delete the annotation (and any proto/db entity files).
```

Nothing breaks — the annotation is simply ignored — but the notice
repeats until you finish the flip.

## 3. Detection

```bash
grep -rln "forge.v1.entity\|forge.v1.field" proto/
ls proto/db 2>/dev/null            # old dedicated entity-proto directory
```

## 4. Migration

1. **Make sure every entity's table exists in `db/migrations/`.** For
   long-lived projects it already does. If an entity only ever existed
   as an annotated proto (no migration), write the create-table
   migration now — `forge add entity <name> ... --no-rpcs` emits one if
   the CRUD RPCs already exist.
2. **Delete the entity annotations** — the `option (forge.v1.entity)`
   blocks and `[(forge.v1.field) = {...}]` field options. Keep the
   messages themselves: they are the wire contract. Keep the
   `(forge.v1.service)` / `(forge.v1.method)` annotations — those are
   alive and well.
3. **Delete `proto/db/` entity files** (if present) and any imports of
   them. Entity wire messages belong in the service proto.
4. **Check the portable subset.** The shadow apply runs your migrations
   on a real ephemeral postgres (exactly as your project's tests via
   `pkg/testkit` already do): parenthesize function defaults
   (`DEFAULT (now())`), drop `::type` casts (`DEFAULT '{}'`, not
   `DEFAULT '{}'::jsonb`). Pg-only auxiliary DDL (extensions, functions,
   triggers, comments, pg-specific DML) is skipped harmlessly; a failing
   `CREATE/ALTER/DROP TABLE` or `CREATE INDEX` is a hard generate error.
5. **Run the projection:**

   ```bash
   forge generate
   go build ./... && go test ./...
   ```

## 5. Review the deltas — SQL wins

Diff the regenerated `internal/db/<entity>_orm.go` (and
`handlers_crud_ops_gen.go`). Deltas appear **exactly where the proto and
the SQL had drifted** — a column the proto never knew about now appears
on the struct; an annotated field whose column was dropped disappears.
The schema is the truth, so the introspected projection is the correct
one; if a delta is genuinely wrong, the fix is a new migration, never an
edit to generated code.

Wire-side nothing changes: the service-proto messages keep evolving
independently, and the conversions map the intersection by name —
wire-only fields never reach the DB, column-only fields never leak onto
the wire.

## 6. Verification

```bash
forge generate          # no retired-annotation notices left
forge lint
go build ./... && go test ./...
grep -rn "forge.v1.entity\|forge.v1.field" proto/   # no hits
```

## 7. Rollback

The flip is metadata-only — no data, no live-schema change. `git revert`
restores the annotated protos, which the current forge still compiles
(tombstoned definitions) but ignores. There is no behavior to roll back
to: the projections come from the migrations either way.

## See also

- `db` skill — the full schema-truth model (type vocabulary, conventions, portable subset).
- `proto` skill — CRUD RPC naming, the wire half of entity detection.
- `architecture` skill — the generate pipeline overview.
