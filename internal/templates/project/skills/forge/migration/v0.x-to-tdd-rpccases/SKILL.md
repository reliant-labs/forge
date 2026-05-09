---
name: v0.x-to-tdd-rpccases
description: Migrate handlers_crud_gen_test.go from inline per-RPC test boilerplate to thin shims that delegate to forge/pkg/tdd.RunRPCCases.
---

# Migrating CRUD test scaffolds to `forge/pkg/tdd.RunRPCCases`

Use this skill when `forge upgrade` reports a jump across the version
that ships the `tdd.RPCCase` / `tdd.RunRPCCases` test scaffold shape.
It only affects projects that have at least one auto-generated CRUD
RPC and therefore a forge-managed `handlers/<svc>/handlers_crud_gen_test.go`.

> **New scaffold default (post-Day-4 polish).** New services scaffolded
> by `forge new` / `forge generate` now emit a `handlers_scaffold_test.go`
> that is itself a `tdd.RunRPCCases` shim — one `Test<Method>_Generated`
> function per RPC. This skill is the migration path for projects that
> pre-date that default and were ported by hand into the legacy
> `tests := []struct{name, call}` shape. The forge lint rule
> **`forgeconv-handler-tests-use-tdd`** (run via `forge lint --tests`)
> flags handler tests still on the legacy shape so the gap is visible
> before review.

## 1. What changed

Before this release, `handlers/<svc>/handlers_crud_gen_test.go` inlined
per-RPC test boilerplate: `_, err := svc.<Method>(ctx, req); _ = err`
plus a per-operation FORGE_SCAFFOLD comment, repeated for every
generated method. Each per-RPC body was ~12-15 lines and the scaffold
gave the user a passing-but-empty test to fill in.

After this release the scaffold delegates to
`github.com/reliant-labs/forge/pkg/tdd`. Each per-RPC test is a thin
shim that builds a slice of `tdd.RPCCase` rows and hands them to the
generic runner:

```go
func TestUnit_CreateUser(t *testing.T) {
    t.Parallel()
    svc := app.NewTestUsers(t)

    tdd.RunRPCCases(t, []tdd.RPCCase[pb.CreateUserRequest, pb.CreateUserResponse]{
        // FORGE_SCAFFOLD: add WantErr (failure mode) or Check (happy path) and replace this row.
        {Name: "scaffold_call", Req: connect.NewRequest(&pb.CreateUserRequest{})},
    }, svc.CreateUser)
}
```

The runner constructs the test harness, invokes the handler, and
asserts on either `WantErr` (Connect code) or `Check` (response
matcher) per row. Adding a new failure-mode test is a single
`tdd.RPCCase{Name: "...", Req: ..., WantErr: connect.Code...}` row in
the slice instead of a new `Test<X>_<Mode>` function.

`tdd.RPCCase` is a type alias for `tdd.Case`; `tdd.RunRPCCases` is a
thin wrapper around `tdd.TableRPC`. Hand-written tests may use either
name.

## 2. Behavioural fingerprints (preserved)

The migration is purely a code-shape change. These observable
behaviours are locked by tests in `pkg/tdd/rpc_test.go`:

- `WantErr` is matched via `connect.CodeOf(err)`, never by string
  comparison on the error message.
- Per-row `Setup` hooks run before the handler is invoked and execute
  in declared slice order (one `t.Run` subtest per row).
- A nil error with no `Check` set passes; a non-nil error with no
  `WantErr` set fails the row.
- The default context for handler invocation is `context.Background()`
  unless `Case.Ctx` is non-nil.

## 3. Detection

```bash
# Old shape: per-RPC body inlines svc.<Method>(...); _ = err.
grep -l "_, err := svc\." handlers/*/handlers_crud_gen_test.go

# New shape: per-RPC body delegates to tdd.RunRPCCases.
grep -l "tdd.RunRPCCases\|tdd.RPCCase" handlers/*/handlers_crud_gen_test.go
```

## 4. Migration

### Deterministic part

```bash
forge generate
```

That's it. The generator rewrites `handlers_crud_gen_test.go` for
every service that has CRUD RPCs *and still carries a FORGE_SCAFFOLD
marker*. Files where the user has cleared every marker are user-owned
and are not touched — fix-forward is manual for those (see below).

### Automated part: hand-rolled `tests := []struct{...}` files

Projects that ported handler tests by hand (rather than via forge
codegen) often grow a parallel `_test.go` shape that exercises every
RPC via a `tests := []struct{name string; call func() error}{...}`
slice fed to a single `for _, tt := range tests` loop. This shape
predates `tdd.RunRPCCases` and does not benefit from the per-row
`WantErr` / `Check` ergonomics.

`forge test migrate-tdd` is a codemod that converts the common shape
to per-RPC `TestXxx_Generated` functions delegating to
`tdd.RunRPCCases`. Run from the project root:

```bash
forge test migrate-tdd                 # walk handlers/ and rewrite in-place
forge test migrate-tdd --dry-run       # show what would change without writing
forge test migrate-tdd --path some/svc # restrict to a subtree
```

Two input shapes are recognised, picked by the `call` field's
signature:

- **service-receiver** (`call func() error`): emits
  `svc.Method` as the handler arg.
- **client-receiver** (`call func(client X) error`): emits
  `client.Method` and preserves the two-value
  `_, client := app.NewTestXServer(t)` constructor.

Files that don't match the recognised shape are skipped with a
printed reason and are never partially rewritten. Imports are
patched: `forge/pkg/tdd` is added, and any imports that the rewrite
left unreferenced (e.g. the `*v1connect` client type alias, or
`context` once the per-row `connect.NewRequest` calls move into
`tdd.RPCCase.Req`) are dropped.

Each emitted scaffold row uses `AnyOutcome: true` to match the
lenient semantics of the hand-rolled shape (which logged but did not
fail on errors). Replace `AnyOutcome` with `WantErr` (failure mode)
or `Check` (happy path) once the handler is real — the
`AnyOutcome` flag is documented in `pkg/tdd/rpc.go` as the
scaffold-stage knob.

### Manual part

Two cases need hand-attention:

1. **You cleared the FORGE_SCAFFOLD markers and added real
   assertions.** The file is user-owned and forge will not regenerate
   it. Either leave it alone (the old shape still compiles — `tdd` is
   a new dependency, not a removal) or port to the new shape by hand:

   ```go
   // before
   func TestUnit_CreateUser(t *testing.T) {
       t.Parallel()
       svc := app.NewTestUsers(t)
       _, err := svc.CreateUser(context.Background(),
           connect.NewRequest(&pb.CreateUserRequest{}))
       require.ErrorIs(t, err, ...)
   }

   // after
   func TestUnit_CreateUser(t *testing.T) {
       t.Parallel()
       svc := app.NewTestUsers(t)
       tdd.RunRPCCases(t, []tdd.RPCCase[pb.CreateUserRequest, pb.CreateUserResponse]{
           {Name: "missing_name", Req: connect.NewRequest(&pb.CreateUserRequest{}), WantErr: connect.CodeInvalidArgument},
       }, svc.CreateUser)
   }
   ```

2. **You wrote sibling tests in another `_test.go` file in the same
   package** (e.g. `handlers_create_user_test.go`). Those are
   user-owned and are unaffected. The new dependency on
   `github.com/reliant-labs/forge/pkg/tdd` lands at the package level —
   running `go mod tidy` after `forge generate` is enough.

## 5. Verification

```bash
go build ./... && go test ./... && forge lint
```

Plus a quick sanity check on the regenerated shape:

```bash
grep -c "tdd.RunRPCCases" handlers/*/handlers_crud_gen_test.go    # one per CRUD RPC
```

The scaffold-marker linter (`forge lint --scaffolds`) still runs and
warns if a `_test.go` file is committed with `FORGE_SCAFFOLD:`
markers, same as before.

## 6. Rollback

If something breaks:

```bash
git revert <forge-generate-commit>      # undo the regen
forge upgrade --to <prior-version>      # pin back
```

The behavioural fingerprints (Connect-error code matching, per-row
setup ordering, default context) are unchanged across the migration,
so a rollback should never be needed for behaviour reasons. The most
likely break is a hand-written `_test.go` that referenced the old
inlined harness shape (`_ = err`) — fix forward by porting that file
to `tdd.RunRPCCases` per the section-4 example.

## See also

- `testing/unit` skill — the canonical unit-test shape for forge
  handlers.
- `pkg/tdd/doc.go` — full library surface and design rationale.
- `migration/v0.x-to-crud-lib` — sibling migration that shrank the
  generated CRUD *handler* (not the test) to a thin shim. Same pattern.
- `forge lint --tests` — runs `forgeconv-handler-tests-use-tdd`, the
  warning-level lint that nudges hand-rolled `tests := []struct{name,
  call}` table tests toward `tdd.RunRPCCases`. New scaffolds emit the
  `tdd.RunRPCCases` shape by default; the lint catches projects that
  pre-date that default.
