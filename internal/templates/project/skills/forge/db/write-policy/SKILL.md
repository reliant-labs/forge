---
name: write-policy
description: Column write policy declared in the migration that owns the column — row-ownership columns, forge:immutable against accidental full-replace overwrites, and forge:version optimistic concurrency.
---

# Column write policy

A column's write policy — may a full-replace UPDATE touch it? — is schema
truth, so it is declared where the column is, as a `COMMENT ON COLUMN` in the
migration that owns it. `forge generate` introspects those comments via
`col_description()` and projects them onto the generated ORM.

## If rows belong to someone, that is a column

Forge stores no ownership of its own — no implicit tenant, no ambient owner. If
rows in a table belong to a user, an org or an account, the only place that fact
can live is a **column you write in the birth migration**: `auth_required` says
only whether a caller must be signed in (`proto`), and forge ships no
authorization (`auth`). Adding the column later is a migration plus a backfill,
so decide now.

Keeping it off the wire is the usual shape, since a client that could propose an
owner could propose someone else's — but a full-replace Update builds its entity
from the request, so a request that never carried the column arrives with it
zero-valued and overwrites the stored value. `forge:immutable` stops that. The
queries and checks around it are yours; `db/crud-overrides` covers the seams.

## `forge:immutable`

```sql
-- up
COMMENT ON COLUMN crews.company_id IS 'forge:immutable';

-- down
COMMENT ON COLUMN crews.company_id IS NULL;
```

projects onto the generated Bun struct tag:

```go
CompanyId string `bun:"company_id,notnull,skipupdate"`
```

Bun's `,skipupdate` omits the column from a full-replace UPDATE's `SET` clause —
the stored value survives untouched. It is still writable on `INSERT`, and a
**masked** Update naming the column explicitly still writes it: that is the
caller asserting a value on purpose.

Reach for it on any column a full-replace Update could zero out by accident: a
server-assigned owner id, an externally-issued identifier (a Stripe customer ID,
an OAuth subject), an audit stamp set once at creation.

**DB-only columns are not automatically protected.** A column with no matching
proto field says nothing about whether it may be rewritten — an internal rating
or a denormalized cache is absent from the API and mutable by design. Only
`forge:immutable` makes a column un-rewritable; absence from the wire never does.

## `forge:version` — concurrent writes to the same row

By default the last writer wins, silently: two clients that each change a
different field produce one surviving version and no error anywhere. That is
right for a settings row or a status flag, and wrong for anything where a lost
edit is a real loss — a document body, an inventory count, a balance.

Opting a table in takes **both halves** — the column, and a field on the wire
message so the caller can hand back what it read:

```sql
-- up
ALTER TABLE crews ADD COLUMN version BIGINT NOT NULL DEFAULT 0;
COMMENT ON COLUMN crews.version IS 'forge:version';

-- down
ALTER TABLE crews DROP COLUMN version;
```

```protobuf
message Crew {
  // ...
  int64 version = 12;  // forge:read-only
}
```

The wire field is required, not cosmetic: a version the caller cannot read and
return is always the zero value, so shipping the column alone makes every update
after a row's first one fail forever. `forge generate` refuses that and names the
field to add. `forge:read-only` is its right shape — readable on Get/List and on
the entity an Update carries, absent from the Create/Update request messages,
since the repository owns the increment.

Every `Update` and masked Update then carries the caller's version into its
`WHERE` clause and increments it in the same statement — one atomic
compare-and-swap, no explicit transaction and no raised isolation level. A stale
writer matches no row and gets **`Aborted`** (reason `version_conflict`) rather
than a silent overwrite. `Aborted` names a mechanical recovery: **re-read the
row, re-apply the change to the fresh values, write again** — retrying the
identical request fails the same way. It stays distinct from `NotFound`, so a UI
can tell "somebody beat you to it" from "this no longer exists."

The column is **never client-settable** — not on Create, not through an
`update_mask` naming it (that is an `unknown_field` error). A client able to
choose its own version could satisfy its own concurrency check. Entities with no
`forge:version` column are unaffected: no predicate, no extra query.

See also: `db` for the schema/ORM half, `db/seeding` for `forge:ref` (the same
`COMMENT ON` vocabulary applied to a foreign-key constraint).
