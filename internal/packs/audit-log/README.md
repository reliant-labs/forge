# Audit Log Pack

Audit logging with DB persistence — automatically records who called which RPC, when, how long it took, and whether it succeeded, with async writes that don't add latency to responses.

## Installation

```bash
forge pack install audit-log
```

## What Gets Generated

| File | Description |
|------|-------------|
| `pkg/audit/store.go` | `AuditStore` interface and `DBAuditStore` implementation with `Log` and `Query` methods |
| `pkg/middleware/audit_gen.go` | Connect interceptor that captures audit entries for every RPC call (regenerated on `forge generate`) |
| `db/migrations/0003_audit_log.up.sql` | Migration creating the `audit_log` table with indexes on user, procedure, and timestamp |
| `db/migrations/0003_audit_log.down.sql` | Rollback migration |

## Configuration

```yaml
audit_log:
  enabled: true
  persist_to_db: true
```

## Usage

### Adding the Interceptor

Wire the audit interceptor into your Connect handler chain. Pass a `DBAuditStore` for database persistence, or `nil` for slog-only mode:

```go
store := audit.NewDBAuditStore(db)
interceptors := connect.WithInterceptors(
    middleware.AuditInterceptor(logger, store),
)
```

Every RPC call is logged with: procedure name, caller identity (from auth claims), peer address, duration, status, error details, and OpenTelemetry trace ID when available.

### Async DB Writes

Database writes happen in a background goroutine with a 5-second timeout, so audit logging never blocks RPC responses. Structured log output via slog is always synchronous, giving you immediate observability even if the DB write is delayed.

### Querying the Audit Log

```go
entries, err := store.Query(ctx, audit.AuditFilter{
    UserID:    "user_123",
    Procedure: "/services.users.v1.UserService/GetUser",
    Since:     time.Now().Add(-24 * time.Hour),
    Limit:     50,
})
```

The `AuditFilter` supports filtering by `UserID`, `Procedure`, `Since`/`Until` time range, and `Limit`/`Offset` pagination.

### Audit Entry Fields

Each `AuditEntry` contains: `ID`, `Timestamp`, `UserID`, `Email`, `Procedure`, `PeerAddress`, `DurationMs`, `Status` (ok/error), `ErrorCode`, `ErrorMessage`, and `Metadata` (JSONB).

## Database Schema

The migration creates an `audit_log` table with indexed columns for `user_id`, `procedure`, and `timestamp` to support efficient filtering and time-range queries.

## Removal

```bash
forge pack remove audit-log
```
