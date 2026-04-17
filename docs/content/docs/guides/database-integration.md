---
title: "Database Integration"
description: "Migration-first database workflow, proto DB contracts, sqlc queries, and the two-model pattern"
weight: 40
---

# Database Integration

Forge uses a migration-first approach to database access. Checked-in SQL migrations are the source of truth for schema evolution, while proto DB entities provide the application contract view used for generated ORM code. The **protoc-gen-forge-orm** plugin (built into the `forge` binary) generates thin CRUD operations over `database/sql` — not a heavy ORM like GORM or Ent. **sqlc** generates type-safe Go code from hand-written SQL for complex queries. Both tools produce code that works with the same `database/sql` transaction interface, so you can mix generated CRUD and custom queries within a single transaction.

## Defining Proto Entities

Entity proto files live in `proto/db/` and use two custom annotation extensions:

- `(forge.options.v1.entity_options)` on the message — defines table name, indexes, soft-delete, and timestamp management
- `(forge.options.v1.field_options)` on individual fields — defines primary keys, column types, constraints, defaults, and foreign keys

Here is a complete example:

```protobuf
// proto/db/v1/entities.proto
syntax = "proto3";

package db.v1;

option go_package = "github.com/myorg/myproject/gen/db/v1;dbv1";

import "forge/options/v1/entity.proto";
import "forge/options/v1/field.proto";
import "google/protobuf/timestamp.proto";

message User {
  option (forge.options.v1.entity_options) = {
    table_name: "users"
    soft_delete: true
    timestamps: true
    indexes: [
      {name: "idx_users_email", fields: ["email"], unique: true},
      {name: "idx_users_org", fields: ["organization_id"]}
    ]
  };

  string id = 1 [(forge.options.v1.field_options) = {
    primary_key: true
    not_null: true
    column_type: "UUID"
    default_value: "gen_random_uuid()"
  }];

  string email = 2 [(forge.options.v1.field_options) = {
    not_null: true
    unique: true
    validation: {required: true, format: "email", max_length: 255}
  }];

  string name = 3 [(forge.options.v1.field_options) = {
    not_null: true
    validation: {required: true, min_length: 1, max_length: 100}
  }];

  string organization_id = 4 [(forge.options.v1.field_options) = {
    not_null: true
    references: "organizations.id"
  }];

  string role = 5 [(forge.options.v1.field_options) = {
    not_null: true
    default_value: "'member'"
    validation: {allowed_values: ["admin", "member", "viewer"]}
  }];

  google.protobuf.Timestamp created_at = 10;
  google.protobuf.Timestamp updated_at = 11;
  google.protobuf.Timestamp deleted_at = 12;
}

message Organization {
  option (forge.options.v1.entity_options) = {
    table_name: "organizations"
    timestamps: true
  };

  string id = 1 [(forge.options.v1.field_options) = {
    primary_key: true
    not_null: true
    column_type: "UUID"
    default_value: "gen_random_uuid()"
  }];

  string name = 2 [(forge.options.v1.field_options) = {
    not_null: true
  }];

  string slug = 3 [(forge.options.v1.field_options) = {
    not_null: true
    unique: true
  }];

  google.protobuf.Timestamp created_at = 10;
  google.protobuf.Timestamp updated_at = 11;
}
```

### Entity Annotation Reference

**EntityOptions** (message-level):

| Field | Type | Description |
|-------|------|-------------|
| `table_name` | string | Database table name. If empty, derived from message name. |
| `indexes` | repeated IndexDef | Table indexes |
| `soft_delete` | bool | Add a `deleted_at` column; filter deleted rows in queries |
| `timestamps` | bool | Auto-manage `created_at` and `updated_at` columns |

**FieldOptions** (field-level):

| Field | Type | Description |
|-------|------|-------------|
| `primary_key` | bool | Mark as primary key |
| `column_type` | string | Override SQL type (e.g., `"UUID"`, `"JSONB"`, `"VARCHAR(255)"`) |
| `not_null` | bool | NOT NULL constraint |
| `unique` | bool | UNIQUE constraint |
| `default_value` | string | SQL default expression (e.g., `"NOW()"`, `"gen_random_uuid()"`) |
| `references` | string | Foreign key in `"table.column"` format |
| `auto_increment` | bool | Auto-increment / serial |
| `validation` | ValidationRules | Application-layer validation rules |

**ValidationRules** (nested in FieldOptions):

| Field | Type | Description |
|-------|------|-------------|
| `required` | bool | Must be non-zero-value |
| `min_length` | int32 | Minimum string length |
| `max_length` | int32 | Maximum string length |
| `pattern` | string | Regex pattern |
| `format` | string | Semantic format: `"email"`, `"url"`, `"uuid"`, `"ip"` |
| `min` | double | Minimum numeric value |
| `max` | double | Maximum numeric value |
| `allowed_values` | repeated string | Enum-like allowed values |
| `custom` | string | Custom validation function name |

## Running Code Generation

```bash
forge generate
```

This detects `proto/db/` and automatically runs the built-in `protoc-gen-forge-orm` plugin to generate ORM code in `gen/db/v1/`. The generated code provides thin CRUD methods over `database/sql` — create, read, update, delete, and list with basic filtering.

## Running Migrations

Forge follows a migration-first workflow:

- `forge db migration new <name>` scaffolds blank SQL migration files in `db/migrations/`
- `forge db migrate ...` wraps the `golang-migrate` CLI for applying and inspecting migration state
- proto DB entities are updated to match the migrated schema and then used as the ORM contract view

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
2. Update proto DB entities in `proto/db/` to reflect the migrated schema
3. Run `forge generate` to regenerate ORM code
4. Use review and tests to catch schema/contract drift

### Migration Workflow

The typical workflow when making schema changes:

1. Create or edit SQL migrations in `db/migrations/`
2. Run `forge db migrate up --dsn=...` to apply them
3. Update proto entity definitions in `proto/db/`
4. Run `forge generate` to regenerate ORM code
5. Commit both the migration files and the proto contract updates

## Using sqlc for Complex Queries

The generated ORM handles simple CRUD, but real applications need joins, aggregations, CTEs, and other complex queries. Forge uses [sqlc](https://sqlc.dev) for these — you write SQL, sqlc generates type-safe Go code.

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

A Forge service typically works with two types of model: the **proto message** (your API contract) and the **database row** (from ORM or sqlc). The service layer converts between them.

```go
package usersservice

import (
    "context"
    "database/sql"

    "connectrpc.com/connect"
    usersv1 "github.com/myorg/myproject/gen/services/users/v1"
    dbv1 "github.com/myorg/myproject/gen/db/v1"
    "github.com/myorg/myproject/gen/dbquery"
)

type Service struct {
    db      *sql.DB
    queries *dbquery.Queries  // sqlc-generated
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

This separation is intentional. Proto messages define the wire format (what callers see). Database rows define the storage format (what the DB returns). The two often differ — joins, computed fields, and different column naming conventions mean a direct mapping is the exception, not the rule.

## Transaction Patterns

Both the generated ORM and sqlc work with `database/sql` transactions. You can mix ORM writes with sqlc queries in a single transaction:

```go
func (s *Service) CreateUser(
    ctx context.Context,
    req *connect.Request[usersv1.CreateUserRequest],
) (*connect.Response[usersv1.User], error) {
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil {
        return nil, connect.NewError(connect.CodeInternal, err)
    }
    defer tx.Rollback()

    // ORM-generated create (uses the tx)
    user := &dbv1.User{
        Email:          req.Msg.Email,
        Name:           req.Msg.Name,
        OrganizationId: req.Msg.OrgId,
        Role:           "member",
    }
    if err := user.Insert(ctx, tx); err != nil {
        return nil, connect.NewError(connect.CodeInternal, err)
    }

    // sqlc query within the same transaction
    qtx := s.queries.WithTx(tx)
    count, err := qtx.CountActiveUsers(ctx, req.Msg.OrgId)
    if err != nil {
        return nil, connect.NewError(connect.CodeInternal, err)
    }

    // Business logic check
    if count > 100 {
        return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("org user limit reached"))
    }

    if err := tx.Commit(); err != nil {
        return nil, connect.NewError(connect.CodeInternal, err)
    }

    return connect.NewResponse(&usersv1.User{
        Id:    user.Id,
        Email: user.Email,
        Name:  user.Name,
    }), nil
}
```

The key to this working is that both ORM and sqlc accept a `DBTX` interface (satisfied by both `*sql.DB` and `*sql.Tx`), so you pass the transaction handle where needed.

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

- **[Architecture]({{< relref "../architecture" >}})** — how proto/db/ fits into the project structure
- **[KCL Deployment Guide]({{< relref "kcl" >}})** — init containers for running migrations in Kubernetes
- **[CI/CD Guide]({{< relref "ci-cd" >}})** — running migrations in deployment pipelines
- **[CLI Reference]({{< relref "../reference/cli" >}})** — full `forge db` command reference