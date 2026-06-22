---
name: v0.x-to-internal-layout
description: Migrate top-level handlers/, workers/, operators/ into internal/handlers/, internal/workers/, internal/operators/. The packages are app-internal (no external importers), so they belong under internal/ where the compiler enforces privacy. This is a mechanical move + import-path rewrite; the pkg/ → internal/ fold is a SEPARATE later migration. Use when bumping across the layout-collapse release.
relevance: migration
---

# Migrating to the `internal/`-nested project layout

Use this skill when `forge upgrade` reports a jump across the release that
moves the generated component trees under `internal/`. forge used to scaffold
`handlers/`, `workers/`, and `operators/` at the repo root; it now nests them
under `internal/`.

## 1. What changed

**Before.** Three component trees lived at the repo root:

```
handlers/<svc>/{handlers.go, service.go, authorizer.go, handlers_crud_gen.go, ...}
workers/<name>/{worker.go, worker_gen.go, ...}
operators/<op>/{operator.go, operator_gen.go, ...}
```

Top-level placement *implies a package is public API* (idiomatic Go reserves
the root for genuine public surface). These packages have no external
importers, so the placement was actively misleading — especially in a repo
about to be open-sourced.

**After.** The same trees nest under `internal/`, where the Go compiler
enforces that nothing outside the module imports them:

```
internal/handlers/<svc>/...
internal/workers/<name>/...
internal/operators/<op>/...
```

This is a **nest**, not a co-location. `internal/<svc>/` (contract.go, the
service implementation) is unchanged and stays where it is; the handler tree
lands beside it as `internal/handlers/<svc>/`. (Co-locating `handlers/<svc>`
*into* `internal/<svc>` is a separate, later refinement — do not attempt it
here.)

`api/` (CRD types — kubebuilder convention, genuinely imported by clients) and
`cmd/` stay top-level. `pkg/` is **out of scope** for this migration: folding
`pkg/` into `internal/` is its own breaking change with its own skill. Do not
move `pkg/` here.

## 2. Detection

```bash
# Old shape — component trees at the repo root.
test -d handlers && echo "OLD SHAPE — handlers/ at root"
test -d workers  && echo "OLD SHAPE — workers/ at root"
test -d operators && echo "OLD SHAPE — operators/ at root"

# New shape — nested under internal/.
test -d internal/handlers && echo "already on internal/ layout"
```

## 3. Migration (deterministic part)

`forge generate` on the new version scaffolds NEW components under
`internal/`, but it will **not** relocate existing hand-written/forked files —
git history and your edits live in the root trees. The move itself is a
mechanical `git mv` + import-path rewrite. Do it in one commit so the diff is
reviewable as a pure rename.

```bash
# 1. Move the trees, preserving history.
mkdir -p internal
git mv handlers   internal/handlers
git mv workers    internal/workers
git mv operators  internal/operators      # only if the project has operators

# 2. Rewrite import paths across the whole module.
#    Replace <module>/handlers -> <module>/internal/handlers (and workers,
#    operators). Get the module path from go.mod.
MOD=$(head -1 go.mod | awk '{print $2}')
grep -rl "\"$MOD/handlers"   --include=*.go . | xargs sed -i '' "s#\"$MOD/handlers#\"$MOD/internal/handlers#g"
grep -rl "\"$MOD/workers"    --include=*.go . | xargs sed -i '' "s#\"$MOD/workers#\"$MOD/internal/workers#g"
grep -rl "\"$MOD/operators"  --include=*.go . | xargs sed -i '' "s#\"$MOD/operators#\"$MOD/internal/operators#g"

# 3. Regenerate so forge-owned wiring (the injector / inventory / registries)
#    points at the new paths.
forge generate

# 4. Build.
go build ./...
```

(On Linux drop the `''` after `sed -i`.)

## 4. Migration (manual part)

What the rewrite can't fully cover:

- **Import aliases.** Code that aliased a component package
  (`svcbilling "<module>/handlers/billing"`) keeps its alias — only the path
  string changes. The grep/sed in step 2 handles the path; confirm the alias
  still reads sensibly.
- **Disowned wiring / registries.** Any disowned file `forge generate` won't
  touch (a forked composition root, a hand-edited registry) needs its component
  imports rewritten by hand with the same `<module>/handlers` →
  `<module>/internal/handlers` rewrite.
- **`.air.*.toml`, Taskfile, Dockerfile, CI globs.** Any path glob that
  referenced `handlers/`, `workers/`, or `operators/` (build watch lists,
  lint excludes, codecov paths) needs the `internal/` prefix added.
- **`forge.yaml` path assumptions.** If your project pins component paths
  anywhere in `forge.yaml`, update them. Stock projects don't.
- **Contract-exclusion lists.** Any tooling that excludes `handlers/**` from a
  check (e.g. a lint allowlist) needs `internal/handlers/**`.
- **Generated comment / doc references** in hand-written docs that name the
  old paths — cosmetic, fix opportunistically.

## 5. Verification

```bash
go build ./... && go test ./... && forge lint
```

Plus shape checks:

```bash
# No component trees left at root.
! test -d handlers && ! test -d workers && ! test -d operators && echo "root is clean"

# No stale import paths survived the rewrite.
MOD=$(head -1 go.mod | awk '{print $2}')
! grep -rq "\"$MOD/handlers/"  --include=*.go . && echo "no stale handler imports"
! grep -rq "\"$MOD/workers/"   --include=*.go . && echo "no stale worker imports"
! grep -rq "\"$MOD/operators/" --include=*.go . && echo "no stale operator imports"
```

If all pass, `forge upgrade` will bump `forge_version` in `forge.yaml`.

## 6. Rollback

```bash
git revert <move-commit>                 # undo the git mv + import rewrite
git revert <forge-generate-commit>       # undo the regen
forge upgrade --to <prev-version>        # pin back to the prior version
```

`--to <prev-version>` requires the prior forge build on `PATH`
(`go install github.com/reliant-labs/forge/cmd/forge@vX.Y.Z`).

## See also

- `architecture` skill — the generated-vs-hand-written split and where each
  component lives (now under `internal/`).
- `migrations/v0.x-to-serverkit-composed` — sequence layout BEFORE the
  serverkit/DI changes; doing layout first keeps the later import churn small.
- The `pkg/` → `internal/` fold is a separate migration; this skill
  deliberately leaves `pkg/` alone.
