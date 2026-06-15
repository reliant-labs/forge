---
name: proto-split
description: Split a multi-service proto file into per-service files — when to split, how to identify shared types, and the commit-per-phase discipline that keeps each step shippable.
emit: both
---

# Splitting a Multi-Service Proto

One service per `.proto` file is the standard convention. A single file with multiple services is usually inherited from a pre-modular codebase — this skill is the playbook for unwinding it.

For greenfield, just don't put two services in a file.

## Symptoms that you should split

Any of these means the file should be split:

- More than one `service` block in the file. Most lint tools reject this; codegen produces ambiguous outputs.
- The file is over ~1000 lines.
- Cross-service coupling friction — a change to one service's RPCs forces you to recompile every consumer of the file.
- Two services share types and you've started copy-pasting messages between sections.
- You want to give two services different RBAC policies, different middleware, or different deploy cadences.

The right answer is one `.proto` per service. Treat the existing file as a temporary aggregation that needs unwinding.

## Target layout

```
proto/
  shared/v1/types.proto                 # cross-service messages (NEW after split)
  services/
    admin/v1/admin.proto                # was: half of the merged file
    billing/v1/billing.proto            # was: other half
    users/v1/users.proto
```

Each service gets its own directory, its own package, and its own generated code path.

## The process — one commit per phase

The discipline that keeps the split safe is **one commit per phase**, where each commit builds and tests clean. Roll back at the granularity of a phase if something goes wrong downstream.

### 1. Inventory the existing file

For each `service` block:

- List the RPCs.
- List the request/response messages used only by that service.
- List the entity messages used only by that service.

For each message that is NOT clearly owned by one service:

- Used by exactly one service? → goes with that service.
- Used by multiple services? → goes to `shared/v1/types.proto`.
- Used by zero services? → delete it.

A grep across the project for each message name tells you who uses what.

### 2. Create `shared/v1/types.proto` (only if needed)

```proto
syntax = "proto3";
package myproject.shared.v1;

import "google/protobuf/timestamp.proto";

// Auditable is embedded into entities that participate in the audit log.
message Auditable {
  string created_by = 1;
  string updated_by = 2;
  google.protobuf.Timestamp created_at = 3;
  google.protobuf.Timestamp updated_at = 4;
}

message Money {
  string currency = 1;
  int64  amount   = 2;
}
```

Shared types stay value-oriented: no service refs, no service-specific behavior. If a candidate "shared type" carries service-specific concerns, it isn't really shared — split it into per-service variants.

### 3. Create one file per service

Move each service's RPCs and its private messages into its own file:

```proto
// proto/services/admin/v1/admin.proto
syntax = "proto3";
package myproject.services.admin.v1;

import "myproject/shared/v1/types.proto";

service AdminService {
  rpc ListUsers(ListUsersRequest) returns (ListUsersResponse) {}
}

message ListUsersRequest  { /* ... */ }
message ListUsersResponse { /* ... */ }
```

### 4. Update imports

Every place the original mega-file was imported has to be re-pointed:

- Other `.proto` files importing the old file → import the new per-service file or `shared/v1/types.proto`.
- Generated code (Go `*.pb.go`, TypeScript `*_pb.ts`, etc.) — let the regen handle it; never sed-rewrite compiled output.
- Application code referencing the old generated package path → re-point to the new path.

A grep for the old proto package name (`<project>.<old>.v1`) catches all of these.

### 5. Regenerate, build, lint, test

Whatever your codegen pipeline is, run it. Then build, then lint, then test. All four must pass before you ship the split:

- The lint should catch any remaining multi-service files, cross-service proto imports, and missing annotations.
- The build should catch any stale generated code or unresolved imports.
- The test should catch any logic that depended on the merged shape.

### 6. Bury the old file

Once the split builds clean, delete the old multi-service file. Keep the deletion in its own commit so a `git revert` can undo just the split if something goes wrong downstream.

## Shared types: when, what, how

Two failure modes to avoid:

- **Over-sharing**: dumping every type into `shared/v1/` to "save duplication". Most types are owned by one service. Move only what's actually shared.
- **Under-sharing**: copying `Money` into three services. If two services genuinely operate on the same value type, share it.

Test for whether a type belongs in shared:

1. Does at least one *other* service reference this type? If no, it's not shared.
2. Are the semantics identical across consumers? If users-service `Auditable` and audit-service `Auditable` differ, they aren't the same type — keep them per-service.
3. Does the type carry behavior that's service-specific (RBAC, validation, etc.)? If yes, keep it per-service.

Shared types are passive value types. Service-specific concerns stay in the owning service.

## Gotchas

1. **Never sed-rewrite compiled `*.pb.go` (or equivalent generated descriptor binaries).** A blanket `sed -i 's|old.v1|new.v1|g'` rewrites the package string inside the rawDesc bytes but does NOT update the varint length prefix encoding it. Result: runtime panic when the descriptor unmarshals. Always regenerate from your codegen pipeline.
2. **Cross-service imports defeat the split.** If service A's proto imports service B's proto, the split hasn't really happened — that's still coupling. Hoist whatever B owned that A needed into `shared/v1/types.proto`.
3. **Forgetting to regenerate before building.** Without regen, the gen tree still has the merged package. Build error at best, silent stale code at worst.
4. **Application code referencing the old gen package.** A grep for the old gen import path catches these — every file referencing the old package in the split services needs updating.

## Verification checklist

- [ ] Each new `.proto` file has exactly one `service` block.
- [ ] No service imports another service's `.proto`.
- [ ] `shared/v1/types.proto` (if created) has no service references.
- [ ] Generated code has been regenerated; build is clean.
- [ ] Lint, test, and any breaking-change gate (e.g. `buf breaking`) all pass.
- [ ] The old merged `.proto` file is deleted in its own commit.

## Rules

- **One service per `.proto` file.** Lint enforces this.
- **Cross-service shared messages live in `shared/v1/types.proto`.** No service-to-service proto imports.
- **Regenerate, never sed-rewrite** compiled descriptor output.
- **One commit per phase.** Each commit builds and tests clean.

<!-- @forge-only:start -->
## Forge tooling

Forge enforces the one-service-per-file rule via `forge lint`. Codegen runs through `forge generate` and emits Go and TypeScript clients per the split.

### Forge-specific layout details

```
proto/
  forge/v1/forge.proto                  # vendored — never edit
  shared/v1/types.proto                 # cross-service messages
  services/
    admin/v1/admin.proto
    billing/v1/billing.proto
    users/v1/users.proto
```

Each service maps to its own gen package (`gen/services/<svc>/v1/`).

Service files carry forge annotations:

```proto
service AdminService {
  option (forge.v1.service) = { name: "admin" version: "v1" };

  rpc ListUsers(ListUsersRequest) returns (ListUsersResponse) {
    option (forge.v1.method) = { auth_required: true };
  }
}
```

Apply the rules from the `proto` skill: explicit annotations on PK / tenant / timestamps / soft-delete; one service per file; CRUD naming. `forge lint --conventions` enforces them.

### Update `forge.yaml`

Each service gets its own entry under `services:`:

```yaml
services:
  admin:
    path: services/admin
    proto: proto/services/admin/v1/admin.proto
  billing:
    path: services/billing
    proto: proto/services/billing/v1/billing.proto
```

The `path:` field is the snake form of the service name — never the hyphenated display form. Forge stores it that way; don't hand-edit to use hyphens. See `architecture` → **Naming conventions** for the full kebab / snake / Pascal / camel mapping.

### Move handlers

Each service's handlers move from the merged directory to its own:

```
handlers/admin_billing/        # before — ambiguous
handlers/admin/                # after
  service.go
  validators.go
  errors.go
handlers/billing/              # after
  service.go
```

Each handler file imports its new gen package. Internal services (`internal/<svc>/contract.go`) probably already split cleanly; if they don't, see `service-layer` for the per-domain shape.

### Regen, build, lint, test (forge commands)

```bash
forge generate
go mod tidy
go build ./...
forge lint
forge test
```

### Canonical forge example

Reference split: an `admin.proto` aggregating `AdminService` + `BillingService` + cross-cutting messages.

- `proto/services/admin/v1/admin.proto` — `AdminService` and its private RPCs/messages.
- `proto/services/billing/v1/billing.proto` — `BillingService` and its private RPCs/messages.
- `proto/shared/v1/types.proto` — `Money`, `Auditable`, `Page` — used by both.
- `forge.yaml` updated to declare both services.
- Handlers moved into `handlers/admin/` and `handlers/billing/`.

Single commit per step (inventory → shared types → per-service files → forge.yaml → handlers move → delete old file) so each commit builds and tests clean.

### Forge-specific gotcha

**`forge.yaml` `path:` always in snake form.** Use `admin_server`, not `admin-server` — even though the display name in the `services:` map can be hyphenated, the `path:` always matches the on-disk directory.

## When this skill is not enough (forge sub-skills)

- **Greenfield proto layout & annotation reference** — see `proto`.
- **Re-orienting handlers and services post-split** — see `api` and `service-layer`.
- **Migration playbook** for porting a legacy multi-service codebase end-to-end — see `migration` and `migration-service`.
<!-- @forge-only:end -->
