---
name: v0.x-to-crud-lib
description: Migrate handlers_crud_gen.go from inline lifecycle code to thin per-RPC shims that delegate to forge/pkg/crud.
---

# Migrating CRUD handlers to `forge/pkg/crud`

Use this skill when `forge upgrade` reports a jump across the version
that ships `forge/pkg/crud`. It only affects projects that have at
least one auto-generated CRUD RPC (`Create<Entity>` /
`Get<Entity>` / `List<Entities>` / `Update<Entity>` / `Delete<Entity>`).

## 1. What changed

Before this release, `handlers/<svc>/handlers_crud_gen.go` inlined the
entire CRUD lifecycle per RPC: auth check, tenant check, error
wrapping, base64 cursor encode/decode, page-size clamp,
`orm.ValidateOrderBy`, `+1` fetch + trim — all repeated for every
generated method.

After this release the lifecycle lives in `github.com/reliant-labs/forge/pkg/crud`.
The generated handler is now a single delegation per RPC:

```go
func (s *Service) CreateUser(ctx context.Context, req *connect.Request[pb.CreateUserRequest]) (*connect.Response[pb.CreateUserResponse], error) {
    return crud.HandleCreate(crud.CreateOp[pb.CreateUserRequest, pb.CreateUserResponse, *db.User]{
        EntityLower: "user",
        Auth: func(ctx context.Context) error {
            claims, err := middleware.GetUser(ctx)
            if err != nil { return err }
            return s.deps.Authorizer.Can(ctx, claims, middleware.ActionCreate, "user")
        },
        Tenant:  middleware.RequireTenantID,                                    // omitted when entity isn't tenant-scoped
        Entity:  func(req *pb.CreateUserRequest) *db.User { return &db.User{Name: req.Name, Email: req.Email} },
        Persist: func(ctx context.Context, tid string, e *db.User) error { return db.CreateUser(ctx, s.deps.DB, e, tid) },
        Pack:    func(e *db.User) *pb.CreateUserResponse { return &pb.CreateUserResponse{User: e} },
    })(ctx, req)
}
```

The struct literal is the only thing forge can know that the library
cannot — RPC types, the proto→entity field copy, the `db.<Name>` call
site, the response field name. Everything else lives in `pkg/crud`.

## 2. Behavioural fingerprints (preserved)

The migration is purely a code-shape change. These observable strings
and behaviours are locked by tests in `pkg/crud/crud_test.go`:

- `"<op> <entity>: %w"` envelope at `connect.CodeInternal` for
  Create/List/Update/Delete.
- Same envelope at `connect.CodeNotFound` for Get.
- `"invalid page token"` at `connect.CodeInvalidArgument` when
  `page_token` is non-empty but doesn't decode.
- `"update <entity>: <field> is required"` at
  `connect.CodeInvalidArgument` when the request entity field is nil.
- Page-size default 50, clamped to 100, fetched with `+1` and trimmed.
- `orm.ValidateOrderBy(req.OrderBy)` validation, default
  `<pk_column> ASC` ordering when no `order_by` is supplied.

If your project asserts on any of these strings (e.g. integration tests
that grep error messages), no edits are needed.

## 3. Detection

```bash
# Old shape: per-RPC body inlines auth/tenant/cursor logic.
grep -l "base64.RawURLEncoding\|ValidateOrderBy" handlers/*/handlers_crud_gen.go

# New shape: per-RPC body is a single crud.HandleX delegation.
grep -l "crud.HandleCreate\|crud.HandleGet\|crud.HandleList\|crud.HandleUpdate\|crud.HandleDelete" handlers/*/handlers_crud_gen.go
```

## 4. Migration

```bash
forge generate
```

That's it. The generator rewrites `handlers_crud_gen.go` for every
service that has CRUD RPCs. Hand-written handlers in sibling files
(e.g. `handlers_create.go` with a `func (s *Service) CreateUser(...)`)
are detected and skipped — the generator never overwrites
user-implemented methods.

Two things to verify after regeneration:

1. **The build is clean.** `go build ./...`. The new shape pulls in
   `github.com/reliant-labs/forge/pkg/crud`. If `go.mod` doesn't pick
   that up automatically, run `go mod tidy`.
2. **Hand-written CRUD handlers still take priority.** Anywhere the
   project has a hand-written `CreateUser` etc. in a sibling file, the
   gen output for that method drops out — same boundary as before.

## 5. Customization escape hatch

To override a generated CRUD handler, write the same RPC by hand in a
sibling file:

```go
// handlers/api/handlers_create_user_custom.go
package api

func (s *Service) CreateUser(ctx context.Context, req *connect.Request[pb.CreateUserRequest]) (*connect.Response[pb.CreateUserResponse], error) {
    // your full implementation
}
```

The generator scans existing user-owned handler files and skips any
RPC whose name already exists. The skip logs an informational line so
you can confirm the generator saw your override.

If you want to keep the lifecycle but customize one piece (e.g. a
Create that emits a domain event before persisting), implement the
handler by hand and call into `crud.HandleCreate` yourself, swapping
the closure you care about:

```go
func (s *Service) CreateUser(ctx context.Context, req *connect.Request[pb.CreateUserRequest]) (*connect.Response[pb.CreateUserResponse], error) {
    return crud.HandleCreate(crud.CreateOp[...]{
        // ... same wiring as the gen output ...
        Persist: func(ctx context.Context, tid string, e *db.User) error {
            if err := db.CreateUser(ctx, s.deps.DB, e, tid); err != nil { return err }
            s.deps.Events.Publish(ctx, "user.created", e.ID)
            return nil
        },
    })(ctx, req)
}
```

This is the recommended pattern for "I want most of the lifecycle but
one custom hook" — strictly better than copying the entire generated
body.

## 6. Verification

```bash
go build ./... && go test ./... && forge lint
```

Plus a quick sanity check on the regenerated shape:

```bash
grep -c "crud.Handle" handlers/*/handlers_crud_gen.go    # one per CRUD RPC
```

## 7. Rollback

If something breaks:

```bash
git revert <forge-generate-commit>      # undo the regen
forge upgrade --to <prior-version>      # pin back
```

The behavioural fingerprints (error wording, page-size defaults,
cursor encoding scheme) are unchanged across the migration, so a
rollback should never be needed for behaviour reasons. The most likely
break is a hand-written handler that wrapped a generated one and is
now resolving against a slightly different per-method body shape — fix
forward by porting that handler to the closure-injection pattern in
section 5.

## See also

- `api` skill — the canonical CRUD handler shape.
- `architecture` skill — the generate pipeline overview.
- `pkg/crud/doc.go` — full library surface and design rationale.
