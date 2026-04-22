# API Key Pack

API key lifecycle management — create, list, revoke, and rotate keys with SHA-256 hashing, prefix-based lookup, and scoped permissions.

## Installation

```bash
forge pack install api-key
```

## What Gets Generated

| File | Description |
|------|-------------|
| `pkg/apikey/store.go` | `KeyStore` interface and `DBKeyStore` implementation with create, revoke, rotate, list, and validate operations |
| `pkg/middleware/api_key_validator_gen.go` | `APIKeyValidator` that bridges the key store into the auth middleware's `Claims` system (regenerated on `forge generate`) |
| `db/migrations/0002_api_keys.up.sql` | Migration creating the `api_keys` table with prefix index and UUID primary key |
| `db/migrations/0002_api_keys.down.sql` | Rollback migration |

## Configuration

```yaml
auth:
  provider: both          # Accepts both JWT and API key authentication
  api_key:
    header: X-API-Key     # HTTP header to read the key from
```

## Usage

### Creating Keys

```go
store := apikey.NewDBKeyStore(db)
key, plaintext, err := store.CreateKey(ctx, userID, "My CI Key", []string{"read", "write"})
// plaintext = "fk_A1b2C3d4..." — show once, never stored
```

Keys are generated as `fk_` + 32 random base62 characters. Only the SHA-256 hash is stored; the plaintext is returned exactly once at creation time.

### Validating Keys

The generated `APIKeyValidator` integrates with the auth middleware. When a request arrives with an `X-API-Key` header, the validator looks up the key by its 8-character prefix, compares the full SHA-256 hash, checks expiry, and returns `Claims` with the key owner's user ID and scopes mapped to roles.

```go
validator := middleware.NewAPIKeyValidator(store, logger)
```

The validator also updates `last_used_at` asynchronously on each successful validation.

### Revoking & Rotating Keys

```go
err := store.RevokeKey(ctx, keyID)           // Soft-revoke (sets revoked_at)
newKey, plaintext, err := store.RotateKey(ctx, keyID)  // Revoke old + create new with same metadata
```

Rotation is atomic: it reads the old key's user, name, and scopes, revokes it, and creates a fresh key with the same attributes.

### Listing Keys

```go
keys, err := store.ListKeys(ctx, userID)  // Returns non-revoked keys only
```

## Database Schema

The migration creates an `api_keys` table with columns: `id` (UUID), `prefix`, `key_hash`, `user_id`, `name`, `scopes` (text array), `expires_at`, `revoked_at`, `last_used_at`, `created_at`. A partial index on `prefix` filters out revoked keys for fast lookups.

## Dependencies

- `golang.org/x/crypto`

## Removal

```bash
forge pack remove api-key
```
