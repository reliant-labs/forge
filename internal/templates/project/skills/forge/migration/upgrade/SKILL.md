---
name: upgrade
description: Upgrade a forge project to a newer forge binary version — version pinning in forge.yaml, per-version migration skills, and the deprecation cycle policy.
---

# Upgrading a forge project

Use this skill when the forge binary on your PATH is newer than the
`forge_version` recorded in `forge.yaml`, or when `forge generate` warns
about a version mismatch.

## How forge tracks versions

Every `forge.yaml` carries a `forge_version` field set at scaffold time
to the version of `forge new` that produced the project. The field is
updated only by `forge upgrade` (never silently by `forge generate`),
so it is a faithful record of the last forge release the project's
generated artifacts were produced against.

Legacy projects that predate the field are treated as `0.0.0`. They
get a one-time nudge from `forge generate` to run `forge upgrade` so
the baseline can be pinned.

## How to upgrade

```bash
# Inspect the current pin and the binary version.
grep forge_version forge.yaml
forge version

# Preview what the upgrade would change.
forge upgrade --dry-run        # alias for --check

# Apply.
forge upgrade                  # bumps to the binary's version
forge upgrade --to 1.5.0       # bumps to a specific version

# Force-overwrite user-modified frozen files (rare; usually you want
# to inspect the diff first and reconcile manually).
forge upgrade --force
```

`forge upgrade` runs in three phases:

1. **Discover migration skills.** It walks `skills/forge/migration/v*-to-*`
   in the embedded skill registry and surfaces any whose `from` prefix
   matches the project's current `forge_version` major/minor family.
   Each skill prints with a `forge skill load <path>` command.
2. **Apply template drift.** Frozen Tier-2 files (Taskfile, Dockerfile,
   middleware scaffolds) are diffed against the latest templates; the
   user sees a unified diff for any file they've modified, and unmodified
   files are auto-updated.
3. **Bump `forge_version`** in `forge.yaml` to the target version.

## Reading per-version migration skills

A migration skill at `migration/v<from>-to-<feature>` is the playbook
for crossing one version boundary. Every skill follows the same
six-section shape (see `migration/v0.x-to-contractkit` as the canonical
example):

1. **What changed.** A one-paragraph technical description.
2. **Detection.** How to identify which shape your code currently has.
3. **Migration (deterministic part).** Commands that `forge upgrade`
   already runs for you (regen, build).
4. **Migration (manual part).** What user-edited code might need to
   change. This is where the LLM does its real work.
5. **Verification.** `go build && go test && forge lint` plus any
   shape-specific checks.
6. **Rollback.** How to back out if something breaks.

When `forge upgrade` surfaces a skill, the deterministic steps run
automatically. Load the skill yourself and apply the manual steps —
forge intentionally doesn't try to automate them, because they touch
hand-written code that the LLM is better placed to reason about than
a regex-based rewrite.

## Deprecation cycle policy

When forge changes the shape of a generated artifact:

- **Old shape works for N versions with warnings.** N is at least 2
  minor versions (e.g. an old shape introduced before 1.4 stays
  buildable, with deprecation warnings, through 1.5 and 1.6).
- **Old shape removed in next major.** A 2.0 release is allowed to
  delete the old shape entirely. The migration skill stays in the
  registry as an archived reference for projects upgrading directly
  from a pre-1.x version.
- **Behavioural fingerprints preserved across the cycle.** Mock
  not-set error strings, slog attribute keys, span names, and metric
  names are locked by fingerprint tests. A migration that breaks one
  of those gets called out explicitly in the skill's "What changed"
  section.

## When to write a new migration skill

Forge core authors should add a new `migration/v<from>-to-<feature>`
skill whenever a release changes the *shape* of a generated artifact
in a way that user code or downstream tooling can observe. Pure
internal refactors (e.g. swapping the regex engine that parses
proto annotations) don't need a skill. A new annotation, a renamed
helper, a changed file layout — those do.

## See also

- `migration` — the top-level skill for porting a non-forge project
  *into* forge in the first place. This skill is for upgrading an
  already-forge project.
- `migration/v0.x-to-contractkit` — the canonical per-version
  migration example (mock/middleware/tracing/metrics → contractkit).
- `migration/v0.x-to-observe-libs` — per-package wrapper codegen →
  `forge/pkg/observe` Connect interceptors.
- `migration/v0.x-to-crud-lib` — `handlers_crud_gen.go` inline
  lifecycle → `forge/pkg/crud` delegation shims.
- `migration/v0.x-to-authz-lib` — `handlers/<svc>/authorizer_gen.go`
  inline matching logic → `forge/pkg/authz` interface-driven shim.
- `migration/v0.x-to-tdd-rpccases` — `handlers_crud_gen_test.go`
  per-RPC inline test boilerplate → `tdd.RunRPCCases` row-driven shims.
- `migration/v0.x-to-pack-starter-split` — stripe / twilio /
  clerk-webhook demoted from packs to one-time-copy starters.
- `migration/v0.x-to-env-config` — hand-curated KCL env-var groups →
  `forge.yaml environments[].config` + sensitive-field projection.
- `migration/v0.x-to-testkit` — bootstrap_testing.go inlined sub-helpers
  (discard logger, in-memory SQLite, httptest harness, permissive
  authorizer, WithTestTenant) → `forge/pkg/testkit` library.
- `migration/v0.x-to-strict-contract-names` — internal-package
  `contract.go` files must declare `type Service interface`, `type Deps
  struct`, and `func New(Deps) Service` exactly. Lint-enforced via
  `forgeconv-internal-package-contract-names`; non-canonical names
  previously produced silently-broken bootstrap codegen.
- `migration/v0.x-to-checksum-history` — `.forge/checksums.json` flat
  shape (path -> hex string) -> structured shape (path -> {hash, history[]})
  so `forge upgrade` distinguishes stale codegen from real user edits.
  Transparent migration — most users will not notice.
