---
name: proto-breaking
description: How forge handles proto breaking changes — `buf breaking` gate in CI, the deprecation/version-rev flow, when to break vs deprecate, and the override flag for legitimate breaking changes.
---

# Proto Breaking Changes

Forge generates a `.github/workflows/proto-breaking.yml` workflow that
runs `buf breaking` against the project's `main` branch on every PR
that touches `proto/**`, `buf.yaml`, or `buf.gen.yaml`. The gate is
on by default. This skill covers what trips it, the standard
deprecation flow that ships through it cleanly, the rare hard-break
escape hatches, and how to verify locally before pushing.

## The gate

```yaml
# .github/workflows/proto-breaking.yml (forge-generated, Tier-1)
on:
  pull_request:
    branches: [main]
    paths:
      - 'proto/**'
      - 'buf.yaml'
      - 'buf.gen.yaml'

jobs:
  buf-breaking:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: bufbuild/buf-action@v1
        with:
          breaking: true
          breaking_against: 'https://github.com/${{ github.repository }}.git#branch=main'
```

The workflow is generated whenever `forge.yaml` has `ci.lint.buf_breaking:
true` (the default) **and** the project has at least one service.
Pushes to main itself are exempt — main is the baseline against which
PRs are compared.

The `buf breaking` rules are configured in `buf.yaml`:

```yaml
# buf.yaml
version: v2
breaking:
  use:
    - WIRE_JSON
```

`WIRE_JSON` is the standard category for "anything that breaks
existing wire-format clients" — added/removed enum values, type-
changed fields, removed RPCs, renumbered fields, deleted oneof
variants, JSON-name conflicts. It strikes the right balance for
forge projects (most clients are JSON-over-Connect). Tighten to
`WIRE` if you don't ship JSON; loosen to `FILE` only if you don't
care about cross-version client compatibility (uncommon).

`forge.yaml` toggles whether the workflow is generated, not what
rules run — edit `buf.yaml` directly to change the rule set.

## Deprecation flow (the 99% path)

When you need to evolve a field, message, or RPC, deprecate first
ship the new shape alongside, remove the deprecated shape in a later
release. The flow:

### Step 1 — add the new shape; deprecate the old

```proto
message User {
  string id = 1;
  // Deprecated: use full_name. Remove after 2026-08-01.
  string name = 2 [deprecated = true];
  string full_name = 3;
}
```

This is non-breaking: clients that read `name` keep working, clients
that read `full_name` start working. `buf breaking` is happy. Servers
populate both fields during the deprecation window.

### Step 2 — migrate callers

Update internal callers (handlers, services, frontends) to write
`full_name` and read `full_name`. Leave `name` populated server-side
during the window — old clients still depend on it.

### Step 3 — remove the deprecated field (later release)

After the deprecation window (one minor version is the forge
default), remove the field:

```proto
message User {
  string id = 1;
  reserved 2;                    // tombstone the tag — never reuse
  reserved "name";               // tombstone the field name — never reuse
  string full_name = 3;
}
```

Tombstoning the tag and field name is what lets `buf breaking` pass
on the removal: the type's wire shape is unchanged from the old
client's perspective (tag 2 is gone, but no caller can land on it
again). Without `reserved`, `buf breaking` flags the removal — and
correctly, because a future contributor could renumber tag 2 onto a
new field of a different type.

### Worked example: renaming an RPC

```proto
// 1. Deprecate the old, add the new.
service Users {
  // Deprecated: use FetchUser. Remove after 2026-08-01.
  rpc GetUser(GetUserRequest) returns (GetUserResponse) { option deprecated = true; }
  rpc FetchUser(FetchUserRequest) returns (FetchUserResponse) {}
}

// 2. Implement FetchUser; have GetUser delegate to FetchUser server-side.
// 3. Migrate frontend hooks (forge generate emits both).
// 4. After the window: remove GetUser, GetUserRequest, GetUserResponse.
//    No `reserved` for RPC names — proto allows reuse of method names
//    once removed.
```

## Hard breaks (rare)

Deprecation isn't always possible:

- **Security.** A field leaking PII; an RPC with a fundamentally
  unauthenticated path that needs auth.
- **Type confusion.** A field declared `string` that should have been
  `bytes`; an enum value with the wrong semantics.
- **Fundamental redesign.** The old shape is no longer expressible
  with new requirements (e.g. multi-tenancy retrofit on a
  single-tenant API).

For these, version-rev:

```proto
// proto/services/users/v1/users.proto
package myproject.services.users.v1;

service Users {
  // Frozen — receive maintenance fixes only.
  rpc GetUser(GetUserRequest) returns (GetUserResponse) {}
}

// proto/services/users/v2/users.proto
package myproject.services.users.v2;

service Users {
  rpc GetUser(GetUserRequest) returns (GetUserResponse) {}
}
```

The flow:

1. Copy the v1 proto to a v2 package. Make the breaking changes in v2.
2. `forge generate` emits Go and TS clients for both versions.
3. Implement v2 handlers; either delegate v1 to v2 server-side (with
   the bug intact) or keep v1's logic frozen.
4. Migrate frontends and external callers from v1 to v2.
5. Mark the v1 service `option deprecated = true;`.
6. After the deprecation window, delete the v1 directory entirely
   (this DOES break v1 clients — the version rev is the
   advertisement that they need to migrate).

`buf breaking` is happy with all of this because v2 is a new package;
v1 is unchanged until you delete it.

## Override flags

For legitimately breaking changes the gate must allow — internal-only
tools where you control every client, or a v1-deletion as part of the
hard-break flow — bypass `buf breaking` for the single PR:

**Per-PR commit-message escape (recommended for one-offs).** Add the
following line to the PR's merge commit:

```
[skip-buf-breaking] reason: removing v1 after deprecation window
```

The action recognises the marker and emits a warning instead of
failing. The marker is auditable in git history and forces the author
to write down a reason.

**Per-file `buf breaking` ignore.** When a single file legitimately
breaks (e.g. an internal-only proto consumed only by your own server
process), add it to `buf.yaml`:

```yaml
breaking:
  use:
    - WIRE_JSON
  ignore:
    - proto/internal/heartbeat/v1/heartbeat.proto
```

Use sparingly — every entry on the ignore list is a permanent
exemption, not a one-time bypass.

**Disable the workflow entirely (not recommended).** Set
`ci.lint.buf_breaking: false` in `forge.yaml` and re-run `forge
generate` — the workflow file is removed. Only do this for projects
where the protos are an internal implementation detail and there are
no external clients (most forge projects DO have external clients,
even if it's just their own frontend bundle deployed independently).

## Verification

Before pushing, run `buf breaking` locally against the same baseline
CI uses:

```bash
# Compare the working tree against the main branch on origin.
buf breaking --against '.git#branch=origin/main'

# Compare against a remote URL (matches CI exactly).
buf breaking --against 'https://github.com/<owner>/<repo>.git#branch=main'

# Compare against a specific commit (useful for stacked-PR debugging).
buf breaking --against '.git#ref=<sha>'
```

A clean exit means the PR will pass the gate. A non-empty output
lists each rule violation with file/line — fix or deprecate-then-rev
each one.

`forge ci` does not currently wrap `buf breaking` (the command is
short enough that the wrapper has no leverage). Just call `buf` directly.

## Rules

- Deprecate first, remove later. The deprecation window is one minor
  version unless the team has explicit reason to extend it.
- `reserved` field tags AND names when removing fields. Otherwise
  `buf breaking` flags the removal and correctly so.
- Version-rev for hard breaks. Copy `proto/<svc>/v1/` to `v2/`, make
  the changes there, deprecate `v1`, delete `v1` after the window.
- The `[skip-buf-breaking]` PR commit marker is the per-PR escape
  hatch. The reason after `reason:` is required and audited.
- Local verification: `buf breaking --against '.git#branch=origin/main'`
  reproduces the CI gate exactly.
- Don't loosen `buf.yaml`'s `breaking.use` rules below `WIRE_JSON`
  unless you don't ship JSON. `FILE` is too loose for a project with
  an actual client.

## When this skill is not enough

- **CI workflow generation and tier boundaries** — see `ci`.
- **Proto file structure, annotations, CRUD conventions** — see `proto`.
- **Splitting a service's protos into multiple files** — see
  `proto-split`.
- **The forge upgrade flow** (when forge itself ships a breaking
  proto-shape change) — see `migration/upgrade`.
