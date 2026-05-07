---
name: v0.x-to-authz-lib
description: Migrate per-service authorizer_gen.go from inline matching logic to a thin shim over forge/pkg/authz. Same public API (NewGeneratedAuthorizer / Can / CanAccess); decision logic now lives in one tested library.
---

# Migrating from inline `authorizer_gen.go` to `forge/pkg/authz`

Use this skill when `forge upgrade` reports a jump across the version
that ships `forge/pkg/authz` (typically `1.7.x → 1.8.x`). It supersedes
the inline-matching shape the legacy authorizer template carried.

## 1. What changed

Forge versions before this release emitted a ~110-line
`handlers/<svc>/authorizer_gen.go` per service. The file inlined the
complete decision logic (empty-procedure check, unknown-procedure deny,
auth-not-required pass-through, role-match loop) on top of two
proto-derived data maps (`methodRoles`, `methodAuthRequired`). Every
service carried its own copy of identical matching code, and the only
way to refactor that logic was to re-render every project's templates.

Forge 1.8+ emits a ~35-line shim that delegates to `forge/pkg/authz`:

- `methodRoles` and `methodAuthRequired` stay in the shim — they are
  per-service data.
- `NewGeneratedAuthorizer()` returns `*authz.Authorizer` (a type alias
  for `*GeneratedAuthorizer`), constructed with the new
  `authz.RolesDecider` configured by the two maps.
- An `init()` wires `middleware.ClaimsFromContext` into the library via
  `authz.SetClaimsLookup`, side-stepping the `middleware → authz →
  middleware` import cycle.

The library provides the policy primitives:

- `authz.Authorizer` — implements `middleware.Authorizer.Can` /
  `CanAccess`, with panic recovery and connect.Error normalisation.
- `authz.Decider` — the user-extension point. The interface
  `Decide(ctx, method, claims) error` describes a single authorization
  decision; `RolesDecider`, `DenyAll`, and `AllowAll` are built-in
  implementations.
- `authz.RolesDecider` — proto-derived RBAC matcher, the one the
  generated shim uses.

Public API preserved exactly: `NewGeneratedAuthorizer()` still returns
something with `Can(ctx, claims, action, resource)` and
`CanAccess(ctx, procedure)`, and the user-owned `authorizer.go`
delegates as before. `TestAuthorizerDenyByDefault` still passes
verbatim — empty procedure, unknown procedure, and `Can(ctx, nil, "", "")`
all deny.

## 2. Detection

How to tell which shape the project currently uses:

```bash
# Old shape: GeneratedAuthorizer is a struct, not a type alias, and
# the file is ~110 lines per service.
grep -l "type GeneratedAuthorizer struct" handlers/*/authorizer_gen.go 2>/dev/null \
  || echo "NEW SHAPE — already on forge/pkg/authz"

# Quick LOC check: old-shape authorizer_gen.go weighs in around 110
# lines; new-shape is closer to 35 plus one row per RPC.
wc -l handlers/*/authorizer_gen.go 2>/dev/null
```

## 3. Migration (deterministic part)

```bash
# Optional safety: list everything that's about to be regenerated.
git diff --name-only -- 'handlers/*/authorizer_gen.go' > /tmp/authz-files-before.txt

# Apply: regenerate every authorizer_gen.go in-place.
forge generate

# Verify: build should be clean. If it's not, see section 4 — almost
# certainly a hand-written reference reaches into a private symbol that
# only existed in the old shape.
go build ./...
```

`forge generate` rewrites every per-service `authorizer_gen.go` to the
new shim. The user-owned `authorizer.go` keeps compiling untouched
because `GeneratedAuthorizer` is now a type alias for
`*authz.Authorizer` and exposes the same `Can` / `CanAccess` methods.

## 4. Migration (manual part)

What user code might need to change:

- **Custom `authorizer.go` that swaps the inner authorizer.** The
  scaffolded shape is:

  ```go
  type Authorizer struct {
      generated *GeneratedAuthorizer
  }
  func NewAuthorizer() *Authorizer { return &Authorizer{generated: NewGeneratedAuthorizer()} }
  ```

  This still works because `*GeneratedAuthorizer == *authz.Authorizer`.
  No edits needed unless you want to opt out of the proto-annotated
  RBAC entirely. To do that:

  ```go
  // handlers/users/authorizer.go (user-owned)
  package users

  import (
      "context"

      "github.com/reliant-labs/forge/pkg/auth"
      "github.com/reliant-labs/forge/pkg/authz"

      "<module>/pkg/middleware"
  )

  type Authorizer struct{ inner *authz.Authorizer }

  func NewAuthorizer() *Authorizer {
      return &Authorizer{inner: authz.New(myDecider{})}
  }

  func (a *Authorizer) CanAccess(ctx context.Context, procedure string) error {
      return a.inner.CanAccess(ctx, procedure)
  }
  func (a *Authorizer) Can(ctx context.Context, claims *middleware.Claims, action, resource string) error {
      return a.inner.Can(ctx, claims, action, resource)
  }

  type myDecider struct{}
  func (myDecider) Decide(ctx context.Context, method string, claims *auth.Claims) error {
      // …project-specific policy (RBAC, OPA, ABAC, …)
      return nil
  }
  ```

- **Direct references to `*GeneratedAuthorizer` as a struct.** The type
  alias means `*GeneratedAuthorizer` and `*authz.Authorizer` are
  interchangeable, but code that took `(g GeneratedAuthorizer)` *by
  value* won't compile (`authz.Authorizer` is constructed via
  `authz.New(...)`; the zero value isn't useful). Pass the pointer
  through instead. Search:

  ```bash
  grep -rn "GeneratedAuthorizer{}\|var .* GeneratedAuthorizer$" --include="*.go" .
  ```

- **Reaching into private symbols `methodRoles` / `methodAuthRequired`.**
  These are package-private in the same handler package. Existing
  references inside `authorizer.go` (rare) keep working because
  `authorizer.go` shares the package with the regenerated
  `authorizer_gen.go`. If you reached for them from outside the handler
  package — don't; expose a method on your `Authorizer` instead.

- **Custom Decider implementations.** If you already wrote one to
  bypass the generated logic, switch from the legacy hand-rolled
  `Authorizer` struct to constructing `authz.New(yourDecider{})` and
  returning that from `NewAuthorizer()`. The contract is one method:

  ```go
  type Decider interface {
      Decide(ctx context.Context, method string, claims *auth.Claims) error
  }
  ```

  The library handles panic recovery and connect.Error normalisation; a
  decider can return any error and the boundary will wrap appropriately.

## 5. Verification

```bash
go build ./... && go test ./... && forge lint
```

Plus a quick sanity check on the regenerated shim:

```bash
grep -l "type GeneratedAuthorizer = authz.Authorizer" handlers/*/authorizer_gen.go
# every authorizer_gen.go should match
```

If all three pass, `forge upgrade` will bump `forge_version` in
`forge.yaml` to the target version automatically.

## 6. Rollback

If something breaks:

```bash
git revert <forge-generate-commit>      # undo the regen
forge upgrade --to 1.7.x                # pin back to the prior version
```

`--to 1.7.x` requires having the older forge build on `PATH` first;
install with `go install github.com/reliant-labs/forge/cmd/forge@vX.Y.Z`.

The `forge_version` field in `forge.yaml` will be reset to `1.7.x` so
subsequent `forge generate` runs won't warn about a mismatch with the
older binary.

## See also

- `auth` skill — the authentication layer that produces the claims
  `authz.Decider` consumes. `auth.Claims` (= `middleware.Claims` via
  alias) flows through unchanged.
- `api` skill — `required_roles` and `auth_required` proto annotations
  that drive `methodRoles` / `methodAuthRequired`.
- `migration/v0.x-to-contractkit` — canonical example of a per-version
  migration skill following this same six-section shape.
