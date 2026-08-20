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

## 1. Tag forge (from a clean `main`)

```sh
cd forge
task release:pkg -- vX.Y.Z          # validates clean pkg/ tree + standalone
                                    # (GOWORK=off) build/vet, then tags pkg/vX.Y.Z
git push origin pkg/vX.Y.Z          # publish pkg FIRST — the require below needs it

go mod edit -require=github.com/reliant-labs/forge/pkg@vX.Y.Z
go build ./... && git commit -am "chore: require forge/pkg vX.Y.Z"

git tag vX.Y.Z                      # root module tag, on the require bump
git push origin main vX.Y.Z
```

**Also bump `defaultPublishedForgePkgVersion`** in
`internal/generator/project_pkgdep.go` to `vX.Y.Z` and include it in the same
commit as the require bump above. This is the fallback a *dev-build* forge
binary (no ldflags `PkgVersion` stamp) pins into every scaffold — if it lags,
dev builds keep pinning an old `forge/pkg` and generated code targeting newer
`forge/pkg` APIs won't compile. `resolveForgePkgVersion()` is the only reader;
there is no automated staleness check (a test that read git tags would be
non-hermetic), so this step is the guard.

**The require bump is not optional.** The root module has no
`replace ... => ./pkg`, so the require IS how a consumer resolves forge/pkg.
Skipping it ships a root module pointing at a stale pkg — v0.0.4 shipped
requiring `pkg v0.0.3` while `pkg/v0.0.4` existed, and no in-repo build could
have noticed. `internal/modguard` fails the suite if the require is a
pseudo-version or a placeholder.

The two tags therefore land on DIFFERENT commits (pkg on the release commit,
root one commit later on the bump). That is expected; `pkg/` content is
identical at both.

If `task release:pkg` fails on the standalone build, the `pkg/` go.mod isn't
tidied for the consumer's view — run `cd pkg && GOWORK=off go mod tidy`, commit,
and retry. (Normal in-workspace CI never exercises this, so the gap only shows at
release time.)

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
