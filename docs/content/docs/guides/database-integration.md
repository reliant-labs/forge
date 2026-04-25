---
title: "Database Integration"
description: "Migration-first database workflow, entity types, ORM functions, sqlc queries, and schema evolution"
weight: 40
---

# Database Integration

**SQL migrations are the source of truth for your database schema.** Not proto, not generated code — migrations. Forge scaffolds initial entity types from proto for convenience, but the database layer is developer-owned from day one. `forge generate` never touches `internal/db/` or `db/migrations/`.

## Architecture Overview

The database layer consists of three developer-owned parts:

1. **SQL migrations** — in `db/migrations/`, append-only history. This is the canonical schema definition.
2. **Entity types** — in `internal/db/types.go`. Start as proto type aliases, evolve to concrete structs as API and DB diverge.
3. **ORM functions** — in `internal/db/<entity>_orm.go`. CRUD operations that work with the entity types.

And one generated companion:

4. **sqlc queries** — in `gen/dbquery/` (if `sqlc.yaml` exists). Type-safe Go from hand-written SQL for complex queries.

## Schema Evolution — The Core Workflow

This is the most important concept in Forge's database layer. Your API types (proto) and database types will diverge over time, and that's by design.

### Phase 1: Scaffold (API = DB)

After `forge new` or `forge add service`, entity types are proto aliases:

```go
// internal/db/types.go — scaffolded
package db

import apiv1 "github.com/myorg/myproject/gen/services/users/v1"

type User = apiv1.User
type Organization = apiv1.Organization
```

This works when the API and DB schema are identical. You get running fast without writing types twice.

### Phase 2: Divergence (API ≠ DB)

The database needs columns the API doesn't expose, or the API returns computed fields not in the DB:

1. Replace the alias with a concrete Go struct in `internal/db/types.go`:
   ```go
   type User struct {
       ID           string
       Name         string
       Email        string
       PasswordHash string  // DB-only, not in API
       LoginCount   int     // DB-only
   }
   ```
2. Add converter functions in the handler or a shared package:
   ```go
   func userToProto(u *db.User) *apiv1.User { ... }
   func userFromProto(u *apiv1.User) *db.User { ... }
   ```
3. Update ORM functions to use the concrete struct.

### Phase 3: Independent Evolution

From this point, API types and DB types evolve independently:
- **Proto changes** affect API clients, frontend codegen, and Connect RPC stubs
- **Migration changes** affect the database schema, entity types, and ORM functions
- **Mapper functions** bridge the gap between the two

## Defining Entities

Entity messages live in the service proto file for API purposes. They define what clients see:

```protobuf
// proto/services/users/v1/users.proto
syntax = "proto3";

package services.users.v1;

import "google/protobuf/timestamp.proto";

message User {
  string id = 1;
  string email = 2;
  string name = 3;
  string organization_id = 4;
  string role = 5;
  google.protobuf.Timestamp created_at = 10;
  google.protobuf.Timestamp updated_at = 11;
  google.protobuf.Timestamp deleted_at = 12;
}

message Organization {
  string id = 1;
  string name = 2;
  string slug = 3;
  google.protobuf.Timestamp created_at = 10;
  google.protobuf.Timestamp updated_at = 11;
}
```

## ORM Functions

`internal/db/user_orm.go` provides CRUD operations:

```go
// Create inserts a new User row.
func CreateUser(ctx context.Context, db orm.DBTX, user *User) (*User, error)

// GetByID fetches a User by primary key.
func GetUserByID(ctx context.Context, db orm.DBTX, id string) (*User, error)

// List returns all Users matching the given filters.
func ListUsers(ctx context.Context, db orm.DBTX, opts ...ListOption) ([]*User, error)

// Update modifies an existing User row.
func UpdateUser(ctx context.Context, db orm.DBTX, user *User) (*User, error)

// Delete removes a User by primary key (or sets deleted_at if soft delete is enabled).
func DeleteUser(ctx context.Context, db orm.DBTX, id string) error
```

The `orm.DBTX` interface is satisfied by both `*pgxpool.Pool` and `pgx.Tx`, so the same functions work with or without a transaction.

### Using the ORM

```go
package usersservice

import (
    "context"
    "github.com/myorg/myproject/internal/db"
    "github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
    pool *pgxpool.Pool
}

func (s *Service) CreateUser(ctx context.Context, name, email string) (*db.User, error) {
    user := &db.User{
        Name:  name,
        Email: email,
    }
    return db.CreateUser(ctx, s.pool, user)
}
```

## Running Code Generation

```bash
forge generate
```

This regenerates Go stubs, Connect handlers, TypeScript clients, frontend hooks, and runs sqlc — but does **NOT** regenerate `internal/db/` or `db/migrations/`. Those are yours to maintain.

## Running Migrations

Forge follows a migration-first workflow:

- `forge db migration new <name>` scaffolds blank SQL migration files in `db/migrations/`
- `forge db migrate ...` wraps the `golang-migrate` CLI for applying and inspecting migration state
- The initial migration (`00001_init.up.sql`) is scaffolded from plan entity definitions

### Creating Migrations

Create a new blank SQL migration pair:

```bash
forge db migration new add_users_table
```

This creates timestamped `.up.sql` and `.down.sql` files in `db/migrations/`.

### Applying and Managing Migrations

```bash
forge db migrate up --dsn "postgres://user:pass@localhost:5432/mydb"       # Apply pending
forge db migrate down --dsn "postgres://user:pass@localhost:5432/mydb"     # Roll back the most recent migration
forge db migrate status --dsn "postgres://user:pass@localhost:5432/mydb"   # Show the current status surface
forge db migrate version --dsn "postgres://user:pass@localhost:5432/mydb"  # Show the current version
```

These commands require the `migrate` CLI from [golang-migrate](https://github.com/golang-migrate/migrate/tree/master/cmd/migrate).

### Planned Schema/Contract Commands

Forge also exposes placeholder command surfaces for the broader migration-first ORM workflow:

```bash
forge db introspect
forge db proto sync-from-db
forge db proto check
forge db codegen
```

These commands are currently informational and non-destructive. Until the backing implementations land:

1. Create and apply SQL migrations in `db/migrations/`
2. Update entity types in `internal/db/types.go` to match the migrated schema
3. Update ORM functions in `internal/db/<entity>_orm.go` as needed
4. Run `forge generate` to regenerate handler scaffolds and run sqlc
5. Use review and tests to catch schema/contract drift

### Migration Workflow

The typical workflow when making schema changes:

1. Create or edit SQL migrations in `db/migrations/`
2. Run `forge db migrate up --dsn=...` to apply them
3. Update entity types in `internal/db/types.go` (keep alias or create concrete struct)
4. Update ORM functions in `internal/db/<entity>_orm.go` to match
5. Run `forge generate` to update handler scaffolds and run sqlc
6. Commit the migration files, type changes, and ORM updates

## Using sqlc for Complex Queries

The ORM handles simple CRUD, but real applications need joins, aggregations, CTEs, and other complex queries. Forge uses [sqlc](https://sqlc.dev) for these — you write SQL, sqlc generates type-safe Go code.

Enable sqlc in `forge.yaml`:

```yaml
database:
  driver: postgres
  migrations_dir: db/migrations
  sqlc_enabled: true
```

Then create a `sqlc.yaml` at the project root:

```yaml
version: "2"
sql:
  - engine: "postgresql"
    queries: "db/queries/"
    schema: "db/migrations/"
    gen:
      go:
        package: "dbquery"
        out: "gen/dbquery"
        sql_package: "pgx/v5"
        emit_json_tags: true
        emit_result_struct_pointers: true
```

Write queries in `db/queries/`:

```sql
-- db/queries/users.sql

-- name: GetUserWithOrg :one
SELECT
    u.id, u.email, u.name, u.role,
    o.name AS org_name, o.slug AS org_slug
FROM users u
JOIN organizations o ON o.id = u.organization_id
WHERE u.id = $1 AND u.deleted_at IS NULL;

-- name: ListUsersByOrg :many
SELECT id, email, name, role, created_at
FROM users
WHERE organization_id = $1
  AND deleted_at IS NULL
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: CountActiveUsers :one
SELECT COUNT(*) FROM users
WHERE organization_id = $1 AND deleted_at IS NULL;
```

Run `forge generate` — it detects `sqlc.yaml` and runs `sqlc generate` as part of the pipeline. The generated code appears in `gen/dbquery/`.

## The Two Model Types Pattern

A Forge service typically works with two types of model: the **proto message** (your API contract) and the **database row** (from ORM or sqlc). Initially these are the same type (via alias), but as your app evolves they diverge.

When using sqlc for complex queries (joins, computed fields), the sqlc-generated row types naturally differ from proto messages. Convert between them in the service layer:

```go
package usersservice

import (
    "context"

    "connectrpc.com/connect"
    usersv1 "github.com/myorg/myproject/gen/services/users/v1"
    "github.com/myorg/myproject/gen/dbquery"
)

type Service struct {
    pool    *pgxpool.Pool
    queries *dbquery.Queries
}

func (s *Service) GetUser(
    ctx context.Context,
    req *connect.Request[usersv1.GetUserRequest],
) (*connect.Response[usersv1.User], error) {
    // Use sqlc for a join query
    row, err := s.queries.GetUserWithOrg(ctx, req.Msg.Id)
    if err != nil {
        if err == sql.ErrNoRows {
            return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("user not found"))
        }
        return nil, connect.NewError(connect.CodeInternal, err)
    }

    // Convert database row → proto message
    return connect.NewResponse(&usersv1.User{
        Id:       row.ID,
        Email:    row.Email,
        Name:     row.Name,
        Role:     row.Role,
        OrgName:  row.OrgName,
    }), nil
}
```

## Transaction Patterns

Both the ORM and sqlc work with pgx transactions. You can mix ORM writes with sqlc queries in a single transaction:

```go
func (s *Service) CreateUser(
    ctx context.Context,
    req *connect.Request[usersv1.CreateUserRequest],
) (*connect.Response[usersv1.User], error) {
    return orm.RunInTx(ctx, s.pool, func(tx pgx.Tx) error {
        // ORM create within the transaction
        user := &db.User{
            Email:          req.Msg.Email,
            Name:           req.Msg.Name,
            OrganizationId: req.Msg.OrgId,
            Role:           "member",
        }
        created, err := db.CreateUser(ctx, tx, user)
        if err != nil {
            return err
        }

        // sqlc query within the same transaction
        qtx := s.queries.WithTx(tx)
        count, err := qtx.CountActiveUsers(ctx, req.Msg.OrgId)
        if err != nil {
            return err
        }

        if count > 100 {
            return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("org user limit reached"))
        }

        return nil
    })
}
```

## Database Setup via Wiring

Database connections are constructed in the generated `pkg/app/wire.go` and passed to services via constructor injection:

```go
// pkg/app/wire.go — GENERATED
func BuildDeps(cfg *configv1.Config) (*Deps, error) {
    pool, err := pgxpool.New(ctx, cfg.DatabaseUrl)
    if err != nil {
        return nil, err
    }

    queries := dbquery.New(pool)

    return &Deps{
        DB:      pool,
        Queries: queries,
    }, nil
}
```

Services receive the database connection through their `Deps` struct:

```go
type Deps struct {
    DB      *pgxpool.Pool
    Queries *dbquery.Queries
}
```

## Related Topics

- **Architecture** — how entity definitions fit into the project structure
- **[KCL Deployment Guide]({{< relref "kcl" >}})** — init containers for running migrations in Kubernetes
- **[CI/CD Guide]({{< relref "ci-cd" >}})** — running migrations in deployment pipelines
- **[CLI Reference]({{< relref "../reference/cli" >}})** — full `forge db` command reference
