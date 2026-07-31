---
name: api-keys
description: API keys as a bearer credential — the owned table/store, forge/pkg/apikey's hash + lookup-prefix primitives, and wiring a KeyValidator into SetupAuth alongside or instead of JWTs.
---

# API keys

API keys are OWNED application code. Forge ships only the security primitives as a library, `forge/pkg/apikey` (`Generate`/`Hash`/`LookupPrefix`/`Verify`); you own the table, store, and validator wiring. Keys are high-entropy random tokens, so `pkg/apikey` hashes with SHA-256 (a fast hash is correct — bcrypt/argon2 defend low-entropy passwords, not a 190-bit random key) and verifies in constant time.

1. **Add the table** — declare `// forge:entity message ApiKey` in the proto and run `forge scaffold`, then edit the birth migration so the row stores the **hash** and an indexed **lookup prefix**, never the plaintext (`prefix TEXT`, `key_hash TEXT`, `user_id TEXT`, plus `scopes`/`expires_at`/`revoked_at`). Mark `key_hash` `// forge:secret` so reads never return it.
2. **Own a store** — issue returns plaintext ONCE and persists only the hash; verify is one indexed read by prefix then a constant-time compare:

   ```go
   import "github.com/reliant-labs/forge/pkg/apikey"
   plaintext, _ := apikey.Generate()           // "fk_..." — return to caller once
   hash := apikey.Hash(plaintext)              // store this, never the plaintext
   prefix, err := apikey.LookupPrefix(presented) // indexed handle; rejects malformed early
   row := store.findByPrefix(ctx, prefix)
   if row == nil || row.RevokedAt != nil || !apikey.Verify(presented, row.KeyHash) {
       return nil, errUnauthenticated
   }
   ```

3. **Wire it in `SetupAuth`** — implement `auth.KeyValidator` (`ValidateKey(ctx, key) (*auth.Claims, error)`) over your store, and pass `Provider: "both"` to accept a Bearer JWT OR an API key.

A key-only service needs no OIDC config at all: `Provider: "api_key"`, no issuer, no JWKS. This is the case the dev IdP container is gated OFF for — see the `auth` skill's dev-loop section.

## Rules

- Store the SHA-256 **hash** plus an indexed **prefix**; never the plaintext, and never a reversible encryption of it.
- Return the plaintext exactly once, at issue. A key you can re-display is a key you are storing.
- Compare with `apikey.Verify` (constant time), not `==` — a byte-by-byte compare leaks the prefix through timing.
- Revocation and expiry are checked in YOUR store lookup; `pkg/apikey` knows nothing about your rows.
