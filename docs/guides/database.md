# Database & ORM

Forge uses a migration-first database workflow. Checked-in SQL migrations in `db/migrations/` are the source of truth for schema evolution, while proto DB entities provide the application contract view used for generated ORM code. Run `forge generate` to regenerate ORM code from your current proto contracts, and use [sqlc](https://sqlc.dev/) alongside the generated ORM for complex queries.

## Migration-First ORM Contracts

The generated ORM is driven by two custom proto extensions defined in `proto/forge/options/v1/`:

- **`EntityOptions`** (message-level) -- Maps a proto message to a database table, with options for indexes, soft delete, and automatic timestamps.
- **`FieldOptions`** (field-level) -- Maps a proto field to a database column, with options for primary key, column type, constraints, references, and validation.

These extensions are included in every Forge project under `proto/forge/options/v1/entity.proto` and `proto/forge/options/v1/field.proto`.

## Defining Entities

Create proto files in `proto/db/` for your database entities. Each message with `entity_options` becomes a database table.

### Basic Entity

```protobuf
// proto/db/user.proto
syntax = "proto3";

package db;

option go_package = "github.com/example/myproject/gen/db;db";

import "forge/options/v1/entity.proto";
import "forge/options/v1/field.proto";
import "google/protobuf/timestamp.proto";

message User {
  option (forge.options.v1.entity_options) = {
    table_name: "users"
    timestamps: true
    soft_delete: true
    indexes: [
      {fields: ["email"], unique: true},
      {fields: ["status"]}
    ]
  };

  string id = 1 [(forge.options.v1.field_options) = {
    primary_key: true
    column_type: "UUID"
    not_null: true
    default_value: "gen_random_uuid()"
  }];

  string name = 2 [(forge.options.v1.field_options) = {
    not_null: true
    validation: {required: true, min_length: 1, max_length: 255}
  }];

  string email = 3 [(forge.options.v1.field_options) = {
    not_null: true
    unique: true
    validation: {required: true, format: "email"}
  }];

  string status = 4 [(forge.options.v1.field_options) = {
    not_null: true
    default_value: "'active'"
    validation: {allowed_values: ["active", "inactive", "suspended"]}
  }];

  string org_id = 5 [(forge.options.v1.field_options) = {
    references: "organizations.id"
    not_null: true
  }];

  google.protobuf.Timestamp created_at = 10;
  google.protobuf.Timestamp updated_at = 11;
  google.protobuf.Timestamp deleted_at = 12;
}
```

### EntityOptions Reference

| Field | Type | Description |
|-------|------|-------------|
| `table_name` | string | Database table name. If empty, derived from the message name (e.g., `User` becomes `users`). |
| `timestamps` | bool | Auto-manage `created_at` and `updated_at` columns. |
| `soft_delete` | bool | Enable soft delete via a `deleted_at` column. `Delete` sets the timestamp instead of removing the row; queries exclude soft-deleted rows. |
| `indexes` | repeated IndexConfig | Indexes to create on this table. Each index has `fields` (column names), optional `name`, and optional `unique` flag. |

### FieldOptions Reference

| Field | Type | Description |
|-------|------|-------------|
| `primary_key` | bool | Mark this field as the primary key. If no field is marked, `id` is assumed. |
| `column_type` | string | Override the database column type (e.g., `"UUID"`, `"VARCHAR(255)"`, `"JSONB"`). |
| `not_null` | bool | Column has a NOT NULL constraint. |
| `unique` | bool | Column has a UNIQUE constraint. |
| `default_value` | string | SQL default value expression (e.g., `"0"`, `"'active'"`, `"NOW()"`). |
| `references` | string | Foreign key reference in `"table.column"` format (e.g., `"users.id"`). |
| `auto_increment` | bool | Column uses auto-increment / serial. |
| `validation` | ValidationRules | Application-layer validation rules. |

### ValidationRules Reference

| Field | Type | Description |
|-------|------|-------------|
| `required` | bool | Field must be non-zero-value. |
| `min_length` | int32 | Minimum string length. |
| `max_length` | int32 | Maximum string length. |
| `pattern` | string | Regex pattern the value must match. |
| `format` | string | Semantic format: `"email"`, `"url"`, `"uuid"`, `"ip"`. |
| `min` | double | Minimum numeric value. |
| `max` | double | Maximum numeric value. |
| `allowed_values` | repeated string | Allowed values (for enum-like fields). |
| `custom` | string | Custom validation function name to call. |

## Generated CRUD Operations

After defining entities and running `forge generate`, you get a `<name>.pb.orm.go` file for each entity in `gen/db/`. For the `User` entity above, the generated code provides these functions:

```go
// Create inserts a new User row.
func CreateUser(ctx context.Context, db orm.DBTX, user *User) (*User, error)

// GetByID fetches a User by primary key.
func GetUserByID(ctx context.Context, db orm.DBTX, id string) (*User, error)

// List returns all Users matching the given filters.
func ListUsers(ctx context.Context, db orm.DBTX, opts ...ListOption) ([]*User, error)

// Update modifies an existing User row.
func UpdateUser(ctx context.Context, db orm.DBTX, user *User) (*User, error)

// Delete removes a User by primary key (or sets deleted_at if soft_delete is enabled).
func DeleteUser(ctx context.Context, db orm.DBTX, id string) error
```

The `orm.DBTX` interface is satisfied by both `*pgxpool.Pool` (direct connection) and `pgx.Tx` (transaction), so the same generated functions work with or without a transaction.

### Using the Generated ORM

```go
package usersservice

import (
    "context"
    "github.com/example/myproject/gen/db"
    "github.com/example/myproject/pkg/orm"
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

func (s *Service) GetUser(ctx context.Context, id string) (*db.User, error) {
    return db.GetUserByID(ctx, s.pool, id)
}
```

## Using sqlc for Complex Queries

The generated ORM handles standard CRUD. For complex queries -- joins, aggregations, CTEs, window functions -- use [sqlc](https://sqlc.dev/) alongside the ORM.

Create a `sqlc.yaml` at the project root:

```yaml
version: "2"
sql:
  - engine: "postgresql"
    queries: "db/queries/"
    schema: "db/migrations/"
    gen:
      go:
        package: "dbqueries"
        out: "gen/dbqueries"
```

Write SQL queries in `db/queries/`:

```sql
-- db/queries/users.sql

-- name: GetUsersByOrg :many
SELECT u.id, u.name, u.email, o.name as org_name
FROM users u
JOIN organizations o ON u.org_id = o.id
WHERE u.org_id = $1
  AND u.deleted_at IS NULL
ORDER BY u.created_at DESC;

-- name: CountActiveUsersByOrg :one
SELECT COUNT(*) as count
FROM users
WHERE org_id = $1
  AND status = 'active'
  AND deleted_at IS NULL;
```

Running `forge generate` automatically invokes `sqlc generate` when it finds a `sqlc.yaml`, producing type-safe Go functions in `gen/dbqueries/`.

## Transaction Patterns

When you need to combine ORM operations with sqlc queries in a single transaction, use the `orm.RunInTx` helper:

```go
import (
    "github.com/example/myproject/gen/db"
    "github.com/example/myproject/gen/dbqueries"
    "github.com/example/myproject/pkg/orm"
    "github.com/jackc/pgx/v5"
    "github.com/jackc/pgx/v5/pgxpool"
)

func (s *Service) TransferUser(ctx context.Context, userID, newOrgID string) error {
    return orm.RunInTx(ctx, s.pool, func(tx pgx.Tx) error {
        // Use the generated ORM within the transaction
        user, err := db.GetUserByID(ctx, tx, userID)
        if err != nil {
            return err
        }

        user.OrgId = newOrgID
        if _, err := db.UpdateUser(ctx, tx, user); err != nil {
            return err
        }

        // Use sqlc queries within the same transaction
        queries := dbqueries.New(tx)
        if err := queries.IncrementOrgMemberCount(ctx, newOrgID); err != nil {
            return err
        }

        // Both operations commit or roll back together
        return nil
    })
}
```

`orm.RunInTx` handles the transaction lifecycle: it begins the transaction, calls your function, commits on success, and rolls back on error (including panic recovery via the underlying pgx driver).

### ORM Client with Transaction Support

For services that need to pass a transactional context through multiple layers, use the `orm.Client`:

```go
client := orm.NewClient(pool)

// Start a transaction
err := orm.RunInTx(ctx, pool, func(tx pgx.Tx) error {
    txClient := client.WithTx(tx)
    // Pass txClient.DB() anywhere you need orm.DBTX
    return db.CreateUser(ctx, txClient.DB(), user)
})
```

## Running Migrations

Forge scaffolds and runs SQL migrations with a migration-first workflow:

- `forge db migration new <name>` creates a blank `.up.sql` / `.down.sql` pair in `db/migrations/`
- `forge db migrate ...` wraps the `golang-migrate` CLI for applying and inspecting migration state
- proto DB entities should be updated to match the migrated schema and then used for ORM code generation

### Creating a Migration

Create and author a new migration directly in SQL:

```bash
forge db migration new add_users_table
```

This creates timestamped migration files in `db/migrations/` using golang-migrate naming conventions.

### Applying and Inspecting Migrations

```bash
# Apply all pending migrations
forge db migrate up --dsn "postgres://localhost:5432/myproject?sslmode=disable"

# Roll back the last migration
forge db migrate down --dsn "postgres://localhost:5432/myproject?sslmode=disable"

# Check the current migration version/status surface
forge db migrate status --dsn "postgres://localhost:5432/myproject?sslmode=disable"
forge db migrate version --dsn "postgres://localhost:5432/myproject?sslmode=disable"
```

These commands require the `migrate` CLI from [golang-migrate](https://github.com/golang-migrate/migrate/tree/master/cmd/migrate).

### Planned Contract Alignment Commands

The `forge db` command now includes placeholder surfaces for the intended migration-first contract workflow:

```bash
forge db introspect
forge db proto sync-from-db
forge db proto check
forge db codegen
```

These commands are currently informational and non-destructive. For now:

- use checked-in SQL migrations as the schema source of truth
- update proto DB entities manually to match the migrated schema
- run `forge generate` to regenerate ORM code

## Workflow Summary

The typical database workflow in a Forge project:

1. **Create or edit** SQL migrations in `db/migrations/`
2. **Apply or inspect** migrations with `forge db migrate ...`
3. **Update proto DB entities** in `proto/db/*.proto` so they reflect the migrated schema
4. **Generate code** with `forge generate` (produces ORM code and runs sqlc)
5. **Use in services** by calling the generated ORM functions, optionally within `orm.RunInTx` for transactional operations

## Related Guides

- [Architecture](architecture.md) -- How proto definitions drive the entire project
- [Getting Started](getting-started.md) -- End-to-end project walkthrough
- [CLI Reference](cli-reference.md) -- Full `forge db` command documentation