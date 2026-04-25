# Database & ORM

Forge uses a migration-first database workflow. Checked-in SQL migrations in `db/migrations/` are the source of truth for schema evolution. Entity messages are defined in the service proto files alongside RPCs, with type aliases and ORM CRUD functions in `internal/db/`. For complex queries, use [sqlc](https://sqlc.dev/) alongside the ORM.

## Architecture

The database layer consists of three parts:

1. **Entity messages** — defined in the service proto (`proto/services/<svc>/v1/<svc>.proto`) alongside RPC definitions
2. **Type aliases + ORM functions** — in `internal/db/`:
   - `types.go` — re-exports proto types: `type User = apiv1.User`
   - `<entity>_orm.go` — CRUD functions: `CreateUser`, `GetUserByID`, `ListUsers`, `UpdateUser`, `DeleteUser`
3. **SQL migrations** — in `db/migrations/`, append-only history of schema changes

`forge generate` does **NOT** regenerate the database layer. It regenerates handlers, frontend hooks, and proto stubs. The DB layer (migrations, ORM functions, types) is owned by the developer/LLM and evolves independently.

## Defining Entities

Entity messages live in the service proto. Each message that represents a database entity should have standard fields:

```protobuf
// proto/services/users/v1/users.proto
syntax = "proto3";

package services.users.v1;

import "google/protobuf/timestamp.proto";

// Entity message — also used as the DB type via alias in internal/db/types.go
message User {
  string id = 1;
  string name = 2;
  string email = 3;
  string status = 4;
  string org_id = 5;
  google.protobuf.Timestamp created_at = 10;
  google.protobuf.Timestamp updated_at = 11;
  google.protobuf.Timestamp deleted_at = 12;
}

// ... RPC definitions using User
service UsersService {
  rpc CreateUser(CreateUserRequest) returns (CreateUserResponse);
  rpc GetUser(GetUserRequest) returns (GetUserResponse);
  rpc ListUsers(ListUsersRequest) returns (ListUsersResponse);
  rpc UpdateUser(UpdateUserRequest) returns (UpdateUserResponse);
  rpc DeleteUser(DeleteUserRequest) returns (DeleteUserResponse);
}
```

## Type Aliases and ORM Functions

After scaffolding, `internal/db/types.go` contains aliases:

```go
package db

import apiv1 "github.com/example/myproject/gen/services/users/v1"

type User = apiv1.User
```

And `internal/db/user_orm.go` provides CRUD operations:

```go
package db

import (
    "context"
    "github.com/example/myproject/pkg/orm"
)

func CreateUser(ctx context.Context, db orm.DBTX, user *User) (*User, error) { ... }
func GetUserByID(ctx context.Context, db orm.DBTX, id string) (*User, error) { ... }
func ListUsers(ctx context.Context, db orm.DBTX, opts ...ListOption) ([]*User, error) { ... }
func UpdateUser(ctx context.Context, db orm.DBTX, user *User) (*User, error) { ... }
func DeleteUser(ctx context.Context, db orm.DBTX, id string) error { ... }
```

The `orm.DBTX` interface is satisfied by both `*pgxpool.Pool` (direct connection) and `pgx.Tx` (transaction), so the same functions work with or without a transaction.

### Using the ORM

```go
package usersservice

import (
    "context"
    "github.com/example/myproject/internal/db"
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

## Schema Evolution — When API and DB Diverge

Initially, entity types are aliases to proto messages. This works when the API and DB schema are identical. When they diverge:

1. Replace the alias with a concrete Go struct in `internal/db/types.go`:
   ```go
   type User struct {
       ID           string
       Name         string
       Email        string
       PasswordHash string  // DB-only, not in API
       LoginCount   int     // DB-only, not in API
   }
   ```
2. Add converter functions:
   ```go
   func UserToProto(u *User) *apiv1.User { ... }
   func UserFromProto(u *apiv1.User) *User { ... }
   ```
3. Update ORM functions to use the concrete struct.

## Using sqlc for Complex Queries

The ORM handles standard CRUD. For complex queries — joins, aggregations, CTEs, window functions — use [sqlc](https://sqlc.dev/) alongside the ORM.

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
    "github.com/example/myproject/internal/db"
    "github.com/example/myproject/gen/dbqueries"
    "github.com/example/myproject/pkg/orm"
    "github.com/jackc/pgx/v5"
    "github.com/jackc/pgx/v5/pgxpool"
)

func (s *Service) TransferUser(ctx context.Context, userID, newOrgID string) error {
    return orm.RunInTx(ctx, s.pool, func(tx pgx.Tx) error {
        // Use ORM within the transaction
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

        return nil
    })
}
```

`orm.RunInTx` handles the transaction lifecycle: it begins the transaction, calls your function, commits on success, and rolls back on error.

### ORM Client with Transaction Support

For services that need to pass a transactional context through multiple layers, use the `orm.Client`:

```go
client := orm.NewClient(pool)

err := orm.RunInTx(ctx, pool, func(tx pgx.Tx) error {
    txClient := client.WithTx(tx)
    return db.CreateUser(ctx, txClient.DB(), user)
})
```

## Running Migrations

Forge scaffolds and runs SQL migrations with a migration-first workflow:

- `forge db migration new <name>` creates a blank `.up.sql` / `.down.sql` pair in `db/migrations/`
- `forge db migrate ...` wraps the `golang-migrate` CLI for applying and inspecting migration state
- The initial migration (`00001_init.up.sql`) is scaffolded from plan entity definitions

### Creating a Migration

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

The `forge db` command includes placeholder surfaces for additional workflow commands:

```bash
forge db introspect
forge db proto sync-from-db
forge db proto check
forge db codegen
```

These commands are currently informational and non-destructive. For now:

- use checked-in SQL migrations as the schema source of truth
- update entity types in `internal/db/types.go` to match the migrated schema
- update ORM functions in `internal/db/<entity>_orm.go` as needed

## Workflow Summary

The typical database workflow in a Forge project:

1. **Create or edit** SQL migrations in `db/migrations/`
2. **Apply or inspect** migrations with `forge db migrate ...`
3. **Update entity types** in `internal/db/types.go` if needed (keep alias or create concrete struct)
4. **Update ORM functions** in `internal/db/<entity>_orm.go` to match schema changes
5. **Generate code** with `forge generate` (runs sqlc, regenerates handlers and frontend — but NOT the DB layer)
6. **Use in services** by calling the ORM functions, optionally within `orm.RunInTx` for transactional operations

## Related Guides

- [Architecture](architecture.md) -- How proto definitions drive the entire project
- [Getting Started](getting-started.md) -- End-to-end project walkthrough
- [CLI Reference](cli-reference.md) -- Full `forge db` command documentation
