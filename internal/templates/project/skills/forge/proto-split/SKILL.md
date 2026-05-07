---
name: proto-split
description: Split a multi-service proto file into per-service files — when to do it, how to split shared types, what `forge.yaml` needs.
---

# Splitting a Multi-Service Proto

`forge lint` rejects multiple `service` declarations in one `.proto` file. If your project has one — usually inherited from a pre-forge codebase — this is the playbook.

For greenfield, just don't put two services in a file. See `proto`.

## Symptoms that you should split

You're looking at one `.proto` file. Any of these means split:

- More than one `service` block in the file. `forge lint` will fail; codegen produces ambiguous outputs.
- The file is over ~1000 lines.
- Cross-service coupling friction — a change to one service's RPCs forces you to recompile every consumer of the file.
- Two services share types and you've started copy-pasting messages between sections.
- You want to give two services different RBAC policies, different middleware, or different deploy cadences.

The right answer is one `.proto` per service. Treat the existing file as a temporary aggregation that needs unwinding.

## Target layout

```
proto/
  forge/v1/forge.proto                  # vendored — never edit
  shared/v1/types.proto                 # cross-service messages (NEW after split)
  services/
    admin/v1/admin.proto                # was: half of admin.proto
    billing/v1/billing.proto            # was: other half
    users/v1/users.proto
```

Each service gets its own directory, its own package (`<project>.services.<svc>.v1`), and its own gen package (`gen/services/<svc>/v1/`).

## Process

### 1. Inventory the existing file

For each `service` block:

- List the RPCs.
- List the request/response messages used only by that service.
- List the entity messages used only by that service.

For each message that is NOT clearly owned by one service:

- Used by exactly one service? → goes with that service.
- Used by multiple services? → goes to `proto/shared/v1/types.proto`.
- Used by zero services? → delete it.

This pass is mechanical but thorough. A grep across the project for each message name tells you who uses what.

### 2. Create `proto/shared/v1/types.proto` if needed

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

Shared types stay value-oriented (no service refs, no service-specific behavior). If a candidate "shared type" carries service-specific concerns, it isn't really shared — split it into per-service variants.

### 3. Create one file per service

Move each service's RPCs and its private messages into its own file:

```proto
// proto/services/admin/v1/admin.proto
syntax = "proto3";
package myproject.services.admin.v1;

import "forge/v1/forge.proto";
import "myproject/shared/v1/types.proto";

service AdminService {
  option (forge.v1.service) = { name: "admin" version: "v1" };

  rpc ListUsers(ListUsersRequest) returns (ListUsersResponse) {
    option (forge.v1.method) = { auth_required: true };
  }
  // ...
}

message ListUsersRequest  { /* ... */ }
message ListUsersResponse { /* ... */ }
```

Apply the rules from `proto` to each: explicit annotations on PK / tenant / timestamps / soft-delete; one service per file; CRUD naming. `forge lint --conventions` enforces them.

### 4. Update imports

Every place the original mega-file was imported has to be re-pointed:

- Other `.proto` files importing the old file → import the new per-service file or `shared/v1/types.proto`.
- Go code referencing `gen/.../v1/<old>_pb.go` → re-point to the new gen path.
- TypeScript hooks generated under the old name → regenerated automatically; no manual edit.
- `frontends/<name>/src/...` direct imports of the old `*_pb.ts` → re-point.

A grep for the old proto package name (`<project>.<old>.v1`) catches all of these.

### 5. Update `forge.yaml`

Each service gets its own entry under `services:`. If you split `admin.proto` into `admin` + `billing`:

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

### 6. Move handlers

Each service's handlers move from the merged directory to its own:

```
handlers/admin_billing/        # before — ambiguous
handlers/admin/                # after
  service.go
  validators.go
  errors.go
  ...
handlers/billing/              # after
  service.go
  ...
```

Each handler file imports its new gen package. Internal services (`internal/<svc>/contract.go`) probably already split cleanly; if they don't, see `service-layer` for the per-domain shape.

### 7. Regenerate, build, lint, test

```bash
forge generate
go mod tidy
go build ./...
forge lint
forge test
```

All four must pass before you ship the split. `forge lint` specifically catches:

- Two services in any remaining file.
- Cross-service proto imports (you should be importing only `shared/v1/types.proto`).
- Missing or stale annotations.

### 8. Bury the old file

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

## Canonical example: the admin proto split

This session's reference split: an `admin.proto` aggregating `AdminService` + `BillingService` + a handful of cross-cutting messages. The split was:

- `proto/services/admin/v1/admin.proto` — `AdminService` and its private RPCs/messages.
- `proto/services/billing/v1/billing.proto` — `BillingService` and its private RPCs/messages.
- `proto/shared/v1/types.proto` — `Money`, `Auditable`, `Page` — used by both.
- `forge.yaml` updated to declare both services.
- Handlers moved into `handlers/admin/` and `handlers/billing/`.

Single commit per step (inventory → shared types → per-service files → forge.yaml → handlers move → delete old file) so each commit builds and tests clean. Roll back at the granularity of a single phase if needed.

## Gotchas

1. **Sed-rewriting compiled descriptors.** A blanket `sed -i 's|old.v1|new.v1|g'` rewrites `go_package` strings inside `*.pb.go` rawDesc bytes but does NOT update the varint length prefix. Result: runtime panic in `protobuf/internal/filedesc.unmarshalSeedOptions`. Always **regenerate** via `forge generate`, never sed compiled output.
2. **Cross-service imports.** If service A's proto imports service B's proto, the split hasn't really happened — that's still coupling. Hoist whatever B owned that A needed into `shared/v1/types.proto`.
3. **`forge.yaml` `path:` in display form.** Use snake form (`admin_server`, not `admin-server`) — even though the display name in the `services:` map can be hyphenated, the `path:` always matches the on-disk directory.
4. **Forgetting `forge generate` before `go build`.** Without regen, the gen tree still has the merged package. Build error at best, silent stale code at worst.
5. **Handler files referencing the old gen package.** A grep for the old gen import path catches these — every handler file in the split services needs the import updated.

## Verification checklist

- [ ] Each new `.proto` file has exactly one `service` block.
- [ ] No service imports another service's `.proto`.
- [ ] `proto/shared/v1/types.proto` (if created) has no service references.
- [ ] `forge.yaml` declares each split service with `path:` in snake form.
- [ ] Handler directories are split per service, each owning its own gen import.
- [ ] `forge generate && go mod tidy && go build ./... && forge lint && forge test` all green.
- [ ] Old merged `.proto` file is deleted.

## Rules

- One service per `.proto` file. The lint rule isn't optional.
- Cross-service shared messages live in `proto/shared/v1/types.proto`. No service-to-service proto imports.
- Use snake form for `forge.yaml` `path:` and on-disk directories. Hyphens are display-only.
- Regenerate via `forge generate` after a split. Never sed compiled `*.pb.go`.
- One commit per phase of the split. Each commit builds and tests clean.

## When this skill is not enough

- **Greenfield proto layout & annotation reference** — see `proto`.
- **Re-orienting handlers and services** post-split — see `api/handlers` and `service-layer`.
- **Migration playbook** for porting a legacy multi-service codebase end-to-end — see `migration` and `migration/service`.
