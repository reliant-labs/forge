# Releasing forge and propagating the bump

forge ships **two** Go modules, tagged at the **same commit**:

- `github.com/reliant-labs/forge` — the CLI (reliant embeds it) → tag `vX.Y.Z`
- `github.com/reliant-labs/forge/pkg` — the runtime lib generated projects import → tag `pkg/vX.Y.Z`

Consumers today: **reliant** pins both; **control-plane** pins only `forge/pkg`. The
managed workspace **daemon image** is the reliant binary cross-compiled
(`control-plane/docker/Dockerfile.reliant.dev` COPYs it) — so its forge version
flows transitively from reliant's `go.mod`; there is **no forge pin in any
Dockerfile** to bump.

See `docs/pkg-versioning.md` for the dev-vs-release dependency model behind the
`pkg` module. This file is the operational checklist for cutting a version.

## 1. Tag forge (from a clean `main`) — ONE command

```sh
cd forge
task release:forge -- vX.Y.Z --dry-run   # optional: every validation, no side effects
task release:forge -- vX.Y.Z
```

That is the whole step. It commits once, tags `pkg/vX.Y.Z` **and** `vX.Y.Z` at
that single commit, and pushes the branch plus both tags atomically.

What it does, in order:

1. validates the version shape, a **clean tree**, that **neither** tag already
   exists, and that `pkg/` builds and vets **standalone** (`GOWORK=off`, the
   consumer's view);
2. bumps `require github.com/reliant-labs/forge/pkg@vX.Y.Z`;
3. syncs all three version files — `VERSION`, `internal/buildinfo/VERSION`
   (which must stay byte-identical to the root one; `buildinfo` embeds a copy)
   and `defaultPublishedForgePkgVersion` in
   `internal/generator/project_pkgdep.go`;
4. resolves the `forge/pkg` hashes into `go.sum` **before the tag is public**,
   then asserts they are really there;
5. tags both refs at the one commit, then pushes the branch and both tags in a
   single atomic `git push`.

`--dry-run` runs every validation and every file edit, prints the plan, then
restores the tree — no commit, no tag, no push.

### Why one commit, and what it fixes

The old flow was `task release:pkg` → push → `go mod edit` → a **second**
commit → tag → push again, which left three ways to ship a broken release:

- **Push ordering.** The require bump could not resolve until `pkg/vX.Y.Z` was
  pushed, so the steps spanned two pushes. Stopping halfway published a pkg tag
  with no root release, or a root release requiring a pkg version nobody could
  download.
- **The `go.sum` trap.** `go build ./...` in this repo passes with **no**
  `forge/pkg` hashes in `go.sum`, because `go.work` resolves `pkg` from the
  local directory. A consumer has no `go.work`, so their `go mod download`
  needs those hashes — and their absence is invisible here until after the
  release is public.
- **Three version files drifting**, each bumped by hand.

Two tags still exist — Go requires the directory-prefixed form for submodules —
but they now land on the **same commit**, which removes the ordering hazard
entirely: either the whole release lands or none of it does.

How step 4 escapes the circularity (hashes normally come from the proxy, which
cannot serve an unpushed tag): the script makes a temporary **bare clone** of
the repo, tags it locally, and resolves with `GOPROXY=direct`. That is sound
because a module's `h1:` hash digests the module's **file tree**, not the commit
carrying the tag — and the release commit touches only root-module files, never
`pkg/`. The script asserts that precondition rather than assuming it.

**The require bump is not optional.** The root module has no
`replace ... => ./pkg`, so the require IS how a consumer resolves forge/pkg.
Skipping it ships a root module pointing at a stale pkg — v0.0.4 shipped
requiring `pkg v0.0.3` while `pkg/v0.0.4` existed, and no in-repo build could
have noticed. `internal/modguard` fails the suite if the require is a
pseudo-version or a placeholder.

If the standalone build fails, `pkg/`'s go.mod isn't tidied for the consumer's
view — run `cd pkg && GOWORK=off go mod tidy`, commit, and retry. (Normal
in-workspace CI never exercises this, so the gap only shows at release time.)

### `task release:pkg` — the narrow tool

`scripts/release-pkg.sh` still works and still tags `pkg/vX.Y.Z` alone. Reach
for it only when the submodule genuinely needs a tag by itself: it does **not**
bump the root require, sync the version files, or populate `go.sum`, so a
release driven from it is only half done. `release:forge` is the documented
path.

## 2. Bump reliant (both modules) — PR

```sh
cd reliant
git checkout -b chore/forge-vX.Y.Z
go get github.com/reliant-labs/forge@vX.Y.Z github.com/reliant-labs/forge/pkg@vX.Y.Z
go mod tidy        # if it errors on the //go:build manual dev/fork_context_test.go
                   # (a known debug artifact with a broken import), use: go mod tidy -e
go build ./...
```

## 3. Bump control-plane (`forge/pkg` only) + pin its CI — PR

```sh
cd control-plane
git checkout -b chore/forge-vX.Y.Z
go get github.com/reliant-labs/forge/pkg@vX.Y.Z && go mod tidy && go build ./...
```

Also bump the forge-CLI install pins in `.github/workflows/ci.yml`
(`go install github.com/reliant-labs/forge/cmd/forge@vX.Y.Z`, two occurrences).

### The KCL module needs no tag

There is deliberately no `kcl-vX.Y.Z` step here. The forge KCL module is
embedded in the binary and vendored into each project's `.forge-kcl/` by
`forge generate`, so a release publishes it automatically by shipping the
binary. Forge once scaffolded a published KCL git tag on release builds; the
tag was never pushed, and every project a released forge created could not
resolve its deploy manifests. See `docs/adr/0001-always-vendor-forge-kcl.md`.

## 4. Forge's own CI needs no pin

Nothing to do here — this step is listed only because it used to exist.

`forge/.github/workflows/ci.yml` runs `go install ./cmd/forge`, building the
forge under test from the working tree. That is deliberate: installing a tag
would validate every PR against the LAST release, so no change to a template or
an emitter could ever go green until after it shipped.

## 5. Rebuild the daemon image

Once reliant's `go.mod` is on the new forge, the next daemon-image build (the
reliant binary → `Dockerfile.reliant.dev`) picks it up automatically. No manual
version edit; just rebuild/deploy per the normal flow.

## Note on history rewrites

If forge history is ever rewritten (e.g. redaction via `git filter-repo`), the
existing version tags move to new commit hashes. Force-push the moved tags, and
bump consumers to a **fresh** tag on the rewritten history — anything pinning the
moved tag will otherwise hit a go.sum/module-hash mismatch.
