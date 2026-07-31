---
name: authorization
description: Turning an IdP identity into an application principal — the enrichClaims DB-backed seam and its rejection codes, why request validation belongs to protovalidate instead, and app-owned provisioning via a public Register RPC.
---

# From identity to principal

The token says who the caller is; your database says what they are. `enrichClaims(ctx, claims)` in `pkg/middleware/middleware.go` runs AFTER validation and BEFORE any handler sees the claims — the one chokepoint where an IdP identity becomes an application principal. Hydrate by `claims.Subject`/`UserID`, the stable IdP identifier, never by email (users change it, and two IdPs can assert the same one):

```go
func enrichClaims(ctx context.Context, claims *Claims) (*Claims, error) {
    row, err := userStore.BySubject(ctx, claims.UserID)
    if errors.Is(err, sql.ErrNoRows) {
        // Authenticated by the IdP but unknown here — see Provisioning below.
        return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("no local account"))
    }
    if err != nil { return nil, err }          // a DB outage is not a 401
    if row.SuspendedAt != nil {
        return nil, connect.NewError(connect.CodePermissionDenied, errors.New("account suspended"))
    }
    claims.OrgID, claims.Roles = row.OrgID, row.Roles
    return claims, nil
}
```

**`Enrich` can REJECT, and it is the earliest place you can.** Return a `connect.Error` and its code is preserved verbatim (`PermissionDenied` stays `PermissionDenied`); return a plain error and it becomes `Unauthenticated` wrapped as `identity enrichment failed: …`. Rejecting here stops the request before any handler runs, which is right for whole-principal facts (suspended, no local account, org disabled) and wrong for per-resource decisions — "may this user edit THAT row" needs the row, so it belongs in the handler.

Enrichment runs on every authenticated request, so a query here is on the hot path: index the subject lookup, and cache if it shows up in traces.

**Request VALIDATION is not `Enrich`'s job.** Field-level input checking is already handled, separately and earlier in the chain, by protovalidate from the `buf.validate` rules on your proto messages:

```proto
string email = 1 [(buf.validate.field).string.email = true];
int32  qty   = 2 [(buf.validate.field).int32.gte = 1];
```

That interceptor rejects a malformed request with `InvalidArgument` before your handler runs. Putting field checks in `enrichClaims` would run them on requests that have none of those fields and report a bad email as an authentication failure.

## Provisioning is application code

Your IdP authenticates; it does not populate your `users` table. That first-contact write is yours, and the honest place for it is a **public** RPC the client calls once with an IdP-issued token in hand:

```proto
rpc Register(RegisterRequest) returns (RegisterResponse) {
  option (forge.v1.method) = { auth_required: false };
}
```

`auth_required: false` here does NOT mean unauthenticated — it means the allow-list lets the call through so the handler can do the credential work itself, because `enrichClaims` would reject the caller for having no local row and the request would never arrive. The handler validates the presented token (the same validator `SetupAuth` built), takes the **`sub` the IdP asserted**, and inserts on that. Trust the `sub` from the verified token; never a `user_id` from the request body, which is a caller-supplied string and an account-takeover primitive. Make the insert idempotent on `sub` — a retried or double-clicked registration must not create a second account.

