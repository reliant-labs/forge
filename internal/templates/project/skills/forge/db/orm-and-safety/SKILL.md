---
name: db/orm-and-safety
description: The two per-project database opt-outs that live next to the code they affect, not in forge.yaml — `//forge:no-orm` in internal/db to skip the generated ORM, and `-- forge:allow-destructive` in a migration to allow one destructive change — plus how the database itself is derived.
---

# ORM opt-out and migration-safety directives

forge.yaml has no `database.driver`, `database.migrations_dir`, `database.seed`, or
`allowed_destructive` — each was derived, defaulted, or moved to the code it affects.

## The database is derived

A service project with `db/migrations/` has a postgres database and that directory is where
migrations live. Delete the directory and the project has no database: no ORM, no
migrations feature. `forge project features` shows the reason for each.

## Skipping the ORM: `//forge:no-orm`

Some projects hand-write their data layer and must not get generated entity structs over it.
Put the marker in the doc comment of `internal/db`, with the reason:

```go
// Package db is the hand-owned repository layer.
//
//forge:no-orm: the repository is hand-written; entities are not projected
package db
```

**Why a marker, not a forge.yaml key.** The exemption describes a property of `internal/db`,
so a reader of that package sees it, with its reason, where `//forge:exclude-contract: <why>`
already works. It opts out of the ORM projection only — migrations, codegen and the database
stay on.

forge reads it from non-test, non-generated `.go` files **directly in** `internal/db/`. A line
that merely mentions it in prose, a `_gen.go` file, a `_test.go` file, or another package does
not count. `forge project features` shows `orm` off with `(//forge:no-orm in internal/db)`.

## Allowing one destructive migration: `-- forge:allow-destructive`

`forge lint --migration-safety` refuses `DROP COLUMN`, `DROP TABLE` and `TRUNCATE`. When the
change is intentional, mark that migration file with a SQL comment line:

```sql
-- forge:allow-destructive: the column was never read; removed after the 2026-09 release
ALTER TABLE daemons DROP COLUMN last_heartbeat;
```

- It is **per file** and **in the file**: the exemption sits next to the SQL it excuses. There
  is no forge.yaml allowlist and no second spelling (`-- forge-safety: allow-destructive` is
  gone and no longer silences anything).
- `forge project audit` lists every migration that carries it (`allow_destructive_files`).
- NOT NULL findings have their own separate directive, `-- forge:allow-unsafe-not-null`.

**Editing an applied migration.** forge does not checksum migrations, so adding the comment
to an already-merged file does not disturb forge. Whether your migration RUNNER records a hash
of the file is the runner's business — check it before touching a merged file, and never change
the statements themselves.
