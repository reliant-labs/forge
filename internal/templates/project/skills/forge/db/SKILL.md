---
name: forge/db
description: Manage database migrations, schema introspection, and proto-db sync.
when_to_use:
  - You need to add, modify, or roll back a database migration
  - You want to regenerate sqlc queries after a schema change
  - You need to check what's in the migrated database
  - You want to sync proto DB entities from the live schema
---

# forge/db

Forge uses a migration-first database model. SQL migrations in `db/migrations/` are the source of truth; proto DB entities can be generated from (or validated against) the migrated schema.

All `forge db migrate ...` subcommands **require `--dsn`**. The dev DSN for a default scaffold is:

```
postgres://postgres:postgres@localhost:5432/<project-name>?sslmode=disable
```

Set it once as an env var to avoid retyping:

```
export DATABASE_URL="postgres://postgres:postgres@localhost:5432/<project-name>?sslmode=disable"
```

## Core commands

```
forge db migration new <name>                    # scaffold a .up.sql / .down.sql pair
forge db migrate up     --dsn "$DATABASE_URL"    # apply pending migrations
forge db migrate down   --dsn "$DATABASE_URL"    # rollback most recent migration
forge db migrate status --dsn "$DATABASE_URL"    # current version
forge db migrate version --dsn "$DATABASE_URL"   # same, version only
forge db migrate force  <version> --dsn "$DATABASE_URL"  # force version without running SQL

forge db introspect --dsn "$DATABASE_URL"                # inspect the live schema
forge db introspect --dsn "$DATABASE_URL" --table users  # one table
forge db introspect --dsn "$DATABASE_URL" --format json

forge db proto sync-from-db --dsn "$DATABASE_URL"         # generate proto entities from schema
forge db proto check        --dsn "$DATABASE_URL"         # validate proto entities match schema

forge generate                # regenerate sqlc queries (along with everything else)
task dev-psql                    # open a psql shell against the dev database
```

## Workflow

1. Start the database (usually already running via `forge run`):
   ```
   docker compose up -d postgres
   ```
2. Create a new migration:
   ```
   forge db migration new add_users_email_index
   ```
   This scaffolds a timestamped `.up.sql` / `.down.sql` pair under `db/migrations/`.
3. Write both the up and down SQL. Keep them **reversible** — every `CREATE` in `.up.sql` should have a matching `DROP` in `.down.sql`.
4. Apply:
   ```
   forge db migrate up --dsn "$DATABASE_URL"
   ```
5. Add or update queries in `db/queries/` and regenerate:
   ```
   forge generate
   ```
6. Run integration tests:
   ```
   forge test integration
   ```

## Rules

- Migrations are append-only history. Once a migration has been merged to main, **never edit it** — write a new migration instead.
- Always write the `.down.sql`. The test harness uses down migrations to reset state.
- Never run `forge db migrate down` against staging or prod. Roll forward only in shared environments.
- Do not hand-edit generated sqlc code. Fix the `.sql` query or the schema, then `forge generate`.
- Keep seed data out of migrations. Migrations change schema; seeds live in `db/seed/` or an app-level bootstrap.
- The dev DSN above assumes the default docker-compose postgres from `forge new`. Production DSNs come from secret management, not this doc.

## When this skill is not enough

- You need a schema diff against a running DB → `forge db introspect` or `pg_dump --schema-only`.
- You're debugging a slow query → `task dev-psql` and run `EXPLAIN ANALYZE`.
- You need zero-downtime schema changes → plan a multi-step migration (add nullable → backfill → set not-null). That's a discipline, not a command.
