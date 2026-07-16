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
git tag vX.Y.Z                      # root module tag, same HEAD
git push origin vX.Y.Z pkg/vX.Y.Z  # publish both
```

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

## 4. Pin forge's own CI

`forge/.github/workflows/ci.yml` installs forge to verify generated code — pin it
to the new tag: `go install github.com/reliant-labs/forge/cmd/forge@vX.Y.Z`.

## 5. Rebuild the daemon image

Once reliant's `go.mod` is on the new forge, the next daemon-image build (the
reliant binary → `Dockerfile.reliant.dev`) picks it up automatically. No manual
version edit; just rebuild/deploy per the normal flow.

## Note on history rewrites

If forge history is ever rewritten (e.g. redaction via `git filter-repo`), the
existing version tags move to new commit hashes. Force-push the moved tags, and
bump consumers to a **fresh** tag on the rewritten history — anything pinning the
moved tag will otherwise hit a go.sum/module-hash mismatch.
