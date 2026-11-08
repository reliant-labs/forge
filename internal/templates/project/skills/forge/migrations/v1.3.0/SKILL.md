---
name: v1.3.0
description: Migrate off the removed forge/pkg/approval package. Pending-approval state machines belong to the application. Copy the implementation from your previously pinned forge into an app-local package, change imports, retain the existing approvals table and verify decision, expiry and redrive behavior before upgrading. This removal is an explicit owner-approved exception to v1 compatibility, not a new major release.
version: v1.3.0
detection: grep -rqs --include='*.go' 'forge/pkg/approval"' .
---

# Upgrading to v1.3.0

## `forge/pkg/approval` is removed

**Use when** a Go file imports `github.com/reliant-labs/forge/pkg/approval`.

### What changed

The entire deprecated, frozen package is removed, including `Approvals`, `Store`,
`Memory`, `Item`, `Propose`, `Decide`, `ExpireDue`, `Cancel`, `Redrive` and
`SchemaSQL`. Pending-approval state machines are application logic, not a forge
primitive. This is an explicit owner-approved exception to forge's v1
compatibility policy; it does not authorize a new major or change that policy
for other APIs. Existing importers will fail to build until migrated.

Forge's own code and templates do not import it. No imports were found in the
sibling reliant or control-plane repositories at removal time. This does not
prove that external consumers are unused.

### What to do before upgrading

Keep your current forge pin while migrating. Copy the implementation from the
exact version your application already uses, so behavior does not change during
the move. From the application module root, with a clean working tree:

```bash
# Ask Go for the source of the package your app actually builds against,
# including any local replace directive. Do this BEFORE changing the pin.
approval_src="$(go list -f '{{.Dir}}' github.com/reliant-labs/forge/pkg/approval)"
mkdir -p internal
# Stop if this destination already exists; reconcile it manually instead.
test ! -e internal/approval &&
  cp -R "$approval_src" internal/approval &&
  chmod -R u+w internal/approval
```

Copy the whole directory, including tests and any fixtures. Change every import
of `github.com/reliant-labs/forge/pkg/approval` to your module's
`internal/approval` path (`go list -m` prints your module path). Remove obsolete
forge deprecation comments from the copied code. Review the copied tests for
repository-relative paths or forge-internal test dependencies and adapt them to
your application's test harness. The implementation may still use other public
forge packages; those remain available. There is no automatic codemod: the app
must choose where it owns the state machine and how to exercise its persistence.

The `approvals` table created from `approval.SchemaSQL` is already yours.
**Do not drop, recreate or clear it.** Preserve schema/search-path configuration,
existing items, unique keys and indexes, decision attribution, follow-up
transactions and the expiry/redrive worker wiring. No database migration is
needed merely to move the Go import.

### Verify

- Search all Go source (including tests and build-tagged files) for the old
  import; none should remain before changing the forge pin.
- Build and test the app-local package and its callers at the old pin, then
  repeat with the new pin.
- Exercise propose/deduplication, approve/reject/cancel, expired proposals,
  follow-up revalidation and transaction rollback against an isolated test DB.
- Verify hand-off failure leaves an approved item eligible for redrive, and
  retrying/redriving does not duplicate the downstream action.

If you already changed the pin and cannot resolve the removed package, retrieve
its source from your previous known-good forge version (recorded in git's
`go.mod` history), migrate as above, then resume the upgrade. Do not substitute
a different approval workflow silently or discard pending decisions.
