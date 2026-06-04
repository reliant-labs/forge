---
name: v0.1-to-v0.2
description: Migrate a forge project from v0.1's two-phase Bootstrap+Setup+ApplyDeps DI shape to v0.2's codegen-based wire_gen.go single-phase DI. The change unifies dep construction so validateDeps gates the COMPLETE dep set at New(), eliminating per-RPC nil-check boilerplate. Use when bumping forge_version from v0.1 to v0.2.
---

# Migrating from forge v0.1 to v0.2

forge v0.2 replaces the two-phase Bootstrap → Setup → ApplyDeps DI
pattern with codegen-based dependency wiring (`pkg/app/wire_gen.go`).
The user-facing changes are small and `forge upgrade --to v0.2` covers
the deterministic parts.

## What changed

**Before (v0.1).** Each service had a hand-written
`(*Service).ApplyDeps(deps Deps) error` method. Bootstrap constructed
each service with the bare-Deps trio (Logger / Config / Authorizer),
registered it on the mux, then called user-owned Setup(). Setup
allocated infrastructure (DB pool, NATS, audit sink) into local
variables and called `app.Services.X.ApplyDeps(X.Deps{Repo: repo, ...})`
on each service to mutate its rich deps in-place. The
mux-already-captured pointer is why ApplyDeps had to mutate rather than
re-assign.

**After (v0.2).** `forge generate` emits `pkg/app/wire_gen.go` with
one `wireXxxDeps(app, cfg, logger, devMode)` function per service. The
function returns the COMPLETE Deps struct (logger + config + authorizer
+ everything from app fields). Bootstrap calls
`X.New(wireXxxDeps(app, cfg, logger, devMode))` so `validateDeps()`
gates every required dep at construction time. Setup keeps its job of
building infrastructure but assigns it onto user-extendable fields of
*App (`app.Repo = ...`) rather than calling ApplyDeps on each service.

The user-extension surface is `pkg/app/app_extras.go` (Tier-2
scaffold, never overwritten). Add `Repo db.Repository` to AppExtras,
assign in setup.go, regenerate — wire_gen resolves by exact-name match.

## Why

The v0.1 polish phase added `validateDeps()` so any Deps field declared
required would error at construction time, eliminating per-RPC
nil-checks. But putting `if d.Repo == nil { return ... }` in
validateDeps made Bootstrap's bare-Deps `New()` call fail BEFORE Setup
ever ran (Setup is what supplied Repo via ApplyDeps). The two-phase
pattern made validateDeps unable to do its one job.

v0.2's wire_gen unifies the construction path: there's one `New(deps)`
call per service, and that call gets the FULL deps. validateDeps fires
at the right time. Per-RPC `if s.deps.X == nil` boilerplate becomes
unnecessary for any dep that's now in validateDeps.

## Auto-applied changes (`forge upgrade --to v0.2`)

The codemod handles the deterministic mechanical work:

1. **Walks `pkg/app/setup.go`.** For each
   `app.Services.X.ApplyDeps(X.Deps{...})` call:
   - Extracts the rich-dep field assignments
   - For each field whose name doesn't match an existing *App field,
     prepends `app.<Field> = <value>` lines above the deleted call
   - Deletes the ApplyDeps call

2. **Walks every `handlers/<svc>/handlers.go`.** Removes per-RPC
   `if s.deps.<Field> == nil { return ..., connect.NewError(connect.CodeFailedPrecondition, ...) }`
   guards where `<Field>` is now in validateDeps. Only matches the
   exact pattern (single nil-check at the top of an RPC, returning
   FailedPrecondition with a "is required"-style message). Anything
   that doesn't match is left for the LLM-assisted step below.

3. **Regenerates the project**: a single `forge generate` emits
   `pkg/app/wire_gen.go` and rewrites `pkg/app/bootstrap.go` to call
   `wireXxxDeps(app, cfg, logger, devMode)` instead of the two-phase
   construct-then-ApplyDeps dance. This step also creates
   `pkg/app/app_gen.go` and `pkg/app/app_extras.go` if they don't
   already exist.

The codemod writes a `UPGRADE_NOTES.md` to the project root listing
what it auto-applied and what needs LLM/manual attention.

## Manual / LLM-assisted changes

After the codemod runs, the LLM/user reviews these:

1. **Promote setup-locals to App fields.** v0.1 setups commonly look
   like:

   ```go
   func Setup(app *App, cfg *config.Config) error {
       repo := db.NewRepository(app.DB)
       app.Services.Orders.ApplyDeps(orders.Deps{Repo: repo, ...})
       app.Services.Billing.ApplyDeps(billing.Deps{Repo: repo, ...})
       return nil
   }
   ```

   The codemod handles the ApplyDeps calls; you may need to:
   - Add `Repo db.Repository` to AppExtras in pkg/app/app_extras.go
   - Confirm the upgraded setup.go has `app.Repo = repo` at the top of
     the function (the codemod inserts it; verify it's correct)

2. **Remove per-RPC nil-checks** for any dep that's now in
   validateDeps. The codemod removes the unambiguous form. Anything
   weirder (multiple-field nil-checks, nil-checks gating optional code
   paths, nil-checks with messages other than "X is required") is
   left in place — review case by case. Heuristic: if `validateDeps()`
   on the service rejects a missing field, the per-RPC nil-check
   below is dead code.

3. **Keep nil-checks for legitimately-optional deps.** Some services
   intentionally accept partial deps (e.g. cpnext's `audit_log`
   service keeps `Store` optional so unit tests don't have to wire a
   real persistent backend). Those stay — the per-RPC nil-check is
   the correct shape for an optional dep. Just don't add the field to
   validateDeps.

4. **Update test setup.** Tests that previously called
   `service.ApplyDeps(...)` to inject mocks need to construct the
   service via `service.New(deps)` directly with the mock-populated
   Deps. The pkg/app testing helpers (`NewTestX`) already do this; if
   you have hand-rolled test fixtures, port them.

## Verification

The triple-gate after running `forge upgrade --to v0.2`:

```bash
go build ./... && go test -count=1 ./... && forge lint
```

A clean compile + green tests + clean lint = migration done. Common
failure modes:

- **`undefined: AppExtras` in bootstrap.go** — the upgrade didn't
  emit `pkg/app/app_extras.go`. Run `forge generate` once more to
  emit it.
- **`undefined: ApplyDeps` in setup.go** — the codemod missed an
  ApplyDeps call. Find it (`grep -n ApplyDeps pkg/app/setup.go`),
  delete the call, and assign the rich deps to App fields manually.
- **`pkg/app/wire_gen.go: TODO: wire <Field>`** — wire_gen couldn't
  resolve a Deps field by name against any *App field. Add the
  matching field to AppExtras in pkg/app/app_extras.go, assign in
  setup.go, regenerate.

## Edge cases

- **Services that intentionally want optional deps.** Keep their
  per-RPC nil-checks; just don't add the field to `validateDeps()`.
  The audit_log pattern is the canonical example.
- **Services that share a constructor closure** (e.g. one
  `func newRepo(...)` called from multiple ApplyDeps sites in v0.1).
  After the migration, construct the shared dep once in setup.go,
  assign to `app.<Field>`, and let wire_gen route it to every
  consuming service.
- **Setup functions that depend on app.Services already being
  populated.** v0.1 allowed this because Setup ran AFTER service
  construction. v0.2 runs Setup BEFORE service construction (so
  wire_gen can read the App fields Setup populates). If your Setup
  does `app.Services.X.SomeMethod()`, you need to invert the
  dependency — pass the relevant data through *App fields instead.

## What changed under the hood (for reference)

- `pkg/app/app_gen.go` (NEW, Tier-1) — the canonical *App struct shape.
  Embeds `*AppExtras` so users can append fields without editing
  forge-owned code.
- `pkg/app/app_extras.go` (NEW, Tier-2) — user-owned extension surface.
  Empty AppExtras to start; users add fields here as they grow.
- `pkg/app/wire_gen.go` (NEW, Tier-1) — one `wireXxxDeps()` per
  service. Resolution rules: Logger / Config / Authorizer /
  DB(orm.Context|*sql.DB) → conventional sources; everything else →
  exact-name match against *App fields (including AppExtras-promoted
  fields).
- `pkg/app/bootstrap.go` (MODIFIED, Tier-1) — Bootstrap calls
  `wireXxxDeps(app, cfg, logger, devMode)` then `X.New(deps)` instead
  of the construct-register-ApplyDeps three-step.
- `pkg/app/setup.go` (MODIFIED, Tier-2) — comments updated; user
  assigns infrastructure to App fields (`app.Repo = ...`) instead of
  calling ApplyDeps on each service.
- `handlers/<svc>/service.go` — `(*Service).ApplyDeps()` method
  REMOVED. Service construction goes through `New(deps)` only.
