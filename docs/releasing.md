# Releasing forge and propagating the bump

forge ships **one** Go module, `github.com/reliant-labs/forge`, tagged `vX.Y.Z`.
It carries both the CLI (reliant embeds it) and the `pkg/*` runtime libraries
generated projects import.

Consumers: **reliant** and **control-plane** each require that one module. The
managed workspace **daemon image** is the reliant binary cross-compiled
(`control-plane/docker/Dockerfile.reliant.dev` COPYs it) — so its forge version
flows transitively from reliant's `go.mod`; there is **no forge pin in any
Dockerfile** to bump.

See `docs/versioning.md` for the dev-vs-release dependency model. This file is
the operational checklist for cutting a version.

## 1. Tag forge (from a clean `main`) — ONE command

```sh
cd forge
task release:forge -- vX.Y.Z --dry-run   # optional: every validation, no side effects
task release:forge -- vX.Y.Z
```

What it does, in order:

1. validates the version shape, a **clean tree**, and that the tag does not
   already exist;
2. confirms against the module proxy that the version was never published at a
   **different** commit (see below);
3. syncs `VERSION` and `internal/buildinfo/VERSION` — which must stay
   byte-identical, since `buildinfo` embeds a copy;
4. builds the module;
5. tags `vX.Y.Z` and pushes the branch and tag in a single atomic `git push`.

`--dry-run` runs every validation and every file edit, prints the plan, then
restores the tree — no commit, no tag, no push.

### The immutable-version gate

`proxy.golang.org` is immutable. Once it has served a version, that content is
permanent: deleting the tag and re-cutting it elsewhere does **not** change what
consumers download, and the version is burned forever. `pkg/v0.1.12` was burned
exactly this way — tagged, published, deleted, re-cut — and the symptom is
maddening from inside the repo, because `git show <tag>` plainly contains your
code while every consumer gets the old bytes.

So the script asks the proxy before a tag exists. A network failure is a hard
stop, not a pass; `--skip-proxy-check` is the only way past it, and only when
you have verified by hand that the version was never published.

### What this step used to involve

forge shipped a second module — `github.com/reliant-labs/forge/pkg`, tagged
`pkg/vX.Y.Z` — which the root module **required**. That require could not
resolve until the pkg tag was pushed, so releasing carried three extra
mechanisms, all of which are now gone:

- **a two-tag atomic push**, so a pkg tag could never ship without its root
  release (v0.0.4 shipped requiring `pkg v0.0.3` while `pkg/v0.0.4` existed, and
  no in-repo build could have noticed);
- **the `go.sum` trap** — an in-repo `go build ./...` passed with no `forge/pkg`
  hashes in `go.sum`, because `go.work` resolved `pkg` from disk, so the script
  made a temporary bare clone, tagged it locally, and resolved the not-yet-public
  version through `GOPROXY=direct` to record real hashes;
- **a bump of `defaultPublishedForgePkgVersion`**, the constant a dev build wrote
  into scaffolds as the forge/pkg pin.

A single module cannot require itself, so there is no unpushed version to
resolve, no second tag to order, and no hand-maintained pin to drift.
`scripts/release-pkg.sh` and `task release:pkg` are deleted.

## 2. Bump reliant — PR

```sh
cd reliant
git checkout -b chore/forge-vX.Y.Z
go get github.com/reliant-labs/forge@vX.Y.Z
go mod edit -droprequire=github.com/reliant-labs/forge/pkg   # first bump only
go mod tidy        # if it errors on the //go:build manual dev/fork_context_test.go
                   # (a known debug artifact with a broken import), use: go mod tidy -e
go build ./...
```

The `-droprequire` matters on the FIRST bump past the merge, and only then.
Leaving the retired module in the graph does not degrade gracefully: both it and
the merged module serve `github.com/reliant-labs/forge/pkg/*`, so every such
import becomes `ambiguous import: found package ... in multiple modules`.
Confirm it is gone with `go list -m github.com/reliant-labs/forge/pkg` — it
should report nothing.

## 3. Bump control-plane + pin its CI — PR

Do this AFTER reliant, which control-plane depends on: while reliant still
requires the retired `forge/pkg`, control-plane inherits it transitively and
nothing it does locally can resolve the resulting ambiguity.

```sh
cd control-plane
git checkout -b chore/forge-vX.Y.Z
go get github.com/reliant-labs/forge@vX.Y.Z
go mod edit -droprequire=github.com/reliant-labs/forge/pkg   # first bump only
go mod tidy && go build ./...
# and the same two edits in gen/, which has its own go.mod
(cd gen && go get github.com/reliant-labs/forge@vX.Y.Z && \
  go mod edit -droprequire=github.com/reliant-labs/forge/pkg && go mod tidy)
```

Also bump the forge-CLI install pins in `.github/workflows/ci.yml`
(`go install github.com/reliant-labs/forge/cmd/forge@vX.Y.Z`, two occurrences)
and `forge_version` in `forge.yaml`.

`forge_version` and the `go.mod` require are now necessarily the **same
number** — one module, one version — so they should be asserted equal in CI
rather than kept in step by hand.

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
