---
name: v0.x-to-testkit
description: Move pkg/app/bootstrap_testing.go's inlined sub-helpers (discard logger, in-memory SQLite, httptest harness, permissive authorizer, WithTestTenant) onto forge/pkg/testkit. The wiring shim stays codegen.
---

# Migrating from inlined bootstrap-testing helpers to `forge/pkg/testkit`

Use this skill when `forge upgrade` reports a jump across the version
that ships `forge/pkg/testkit`. It is a low-impact migration: the
generated `pkg/app/testing.go` keeps the per-service `NewTest<Service>`
and `NewTest<Service>Server` factories, but the four sub-helpers they
relied on (discard logger, in-memory SQLite ORM context, httptest
server, permissive authorizer) move into the library.

## 1. What changed

Prior versions inlined four helpers into every project's
`pkg/app/testing.go`:

- A `testAuthorizer` struct with no-op `CanAccess` / `Can` methods.
- A `newTestDB(t *testing.T) orm.Context` function that called
  `sql.Open("sqlite3", ":memory:")` + `orm.NewClientWithDB` + a
  `t.Cleanup` close.
- A `slog.New(slog.NewTextHandler(io.Discard, nil))` literal in
  `defaultTestConfig`.
- An `httptest.NewServer(mux)` + `t.Cleanup(srv.Close)` pair inside
  every `NewTest<Service>Server`.
- (Multi-tenant projects only) A `WithTestTenant` helper that called
  `middleware.ContextWithTenantID(ctx, id)`.

Forge now imports `github.com/reliant-labs/forge/pkg/testkit` and the
generated file calls into it:

- `testkit.DiscardLogger()` returns the discard slog.
- `testkit.NewSQLiteMemDB(t)` returns an `orm.Context` backed by a
  fresh `:memory:` SQLite (driver + dialect blank-imported by the
  library, so projects no longer need either import).
- `testkit.NewTestServer(t, register)` runs an `httptest.Server`
  with `register` mounting one or more services on its mux; the
  server is closed via `t.Cleanup`.
- `testkit.PermissiveAuthorizer{}` is an Authorizer impl that allows
  every call. It implements the project's `middleware.Authorizer`
  interface because `middleware.Claims` is a type alias for
  `auth.Claims`.
- `testkit.WithTestTenant(ctx, id)` sets the tenant ID via the same
  context key the production tenant interceptor uses.

The wiring shim — `NewTest<Service>` constructing a per-service
`Deps`, `WithLogger`/`WithConfig`/`WithAuthorizer`/`WithDB`/`With<Svc>Deps`
options, and the `NewTest<Service>Server` Connect-client wiring —
stays in the generated file. None of that compresses into a library:
each project's per-service Deps shape is different, and the
proto-Connect client constructor is per-RPC-package.

The behavioural fingerprint is preserved. Specifically:

- Per-call SQLite databases are still isolated.
- The discard logger drops every record and never errors.
- `PermissiveAuthorizer` always returns `nil` (matches the previous
  `testAuthorizer{}`).
- `WithTestTenant` writes to the same context key as the production
  tenant interceptor reads from.

## 2. Detection

How to tell which shape the project currently uses:

```bash
# Old shape: testAuthorizer / newTestDB live inline.
grep -l "func newTestDB\|type testAuthorizer" pkg/app/testing.go 2>/dev/null \
  || echo "NEW SHAPE — sub-helpers already in testkit"

# New shape: testkit imported, four sub-helpers called from defaultTestConfig.
grep -c "testkit\." pkg/app/testing.go 2>/dev/null
```

## 3. Migration (deterministic part)

`forge generate` rewrites `pkg/app/testing.go` into the new shape
automatically.

```bash
# Apply: regenerate the test harness in-place.
forge generate

# Verify: build should be clean.
go build ./...

# Verify: in-package tests still pass.
go test ./...
```

If your project tracks `forge/pkg` via a release tag, no `go.mod`
change is required — the new `forge/pkg/testkit` subpackage rides
along the next time you bump the `forge/pkg` version. Projects that
use a `replace` directive against a local checkout pick up the new
package automatically.

## 4. Migration (manual part)

What user code might need to change:

- **References to the old `testAuthorizer{}` literal.** Search the
  project for direct uses outside `pkg/app/testing.go`:

  ```bash
  grep -rn "testAuthorizer\b" --include="*.go" .
  ```

  Replace with `testkit.PermissiveAuthorizer{}`. The type now lives
  in `forge/pkg/testkit` and is exported.

- **References to the old `newTestDB(t)` function.** The function
  was unexported and lived in `package app`, so direct external
  references are unlikely, but in-package tests that called it
  should switch to `testkit.NewSQLiteMemDB(t)`:

  ```bash
  grep -rn "newTestDB(" --include="*.go" .
  ```

- **Hand-built httptest servers in test files.** If you copied the
  `httptest.NewServer(mux) + t.Cleanup(srv.Close)` pattern into a
  test file, you can simplify it to:

  ```go
  srv := testkit.NewTestServer(t, func(mux *http.ServeMux) {
      svc.Register(mux)
  })
  ```

  Not required — the old pattern still works.

- **The `WithTestTenant` re-export.** Multi-tenant projects keep a
  thin `app.WithTestTenant(ctx, id)` wrapper around
  `testkit.WithTestTenant(ctx, id)`. Existing test code that calls
  `app.WithTestTenant(...)` continues to work without modification.

## 5. Verification

```bash
go build ./... && go test ./... && forge lint
```

Plus:

```bash
# Confirm the four sub-helpers are now testkit-backed.
grep "testkit.DiscardLogger\|testkit.NewSQLiteMemDB\|testkit.PermissiveAuthorizer\|testkit.NewTestServer" \
    pkg/app/testing.go

# Confirm the legacy inlined helpers are gone.
grep "type testAuthorizer\|func newTestDB" pkg/app/testing.go && \
    echo "WARN: legacy helpers still present"
```

## 6. Rollback

If something breaks:

```bash
git revert <forge-generate-commit>      # undo the regen
forge upgrade --to <prior-version>      # pin back to the prior version
```

`forge_version` in `forge.yaml` will be reset, so subsequent
`forge generate` runs use the older template shape.

## See also

- `testing-unit` skill — how to use the testkit helpers from a
  hand-written unit test (the harness pattern `pkg/app/testing.go`
  follows is the same one your tests can compose against).
- `migration/v0.x-to-observe-libs` — the parallel observability
  library migration. Both follow the "library + thin shim" pattern.
- `migration/v0.x-to-crud-lib` — the CRUD library migration; same
  shape, larger surface.
