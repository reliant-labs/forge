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

## 0. Who releases: the release train

**Maintainers and agents do not cut forge releases by hand.** The release train
(`control-plane/scripts/release-train.sh`, run by the Reliant workflow
`release-train` weekdays at 18:00 UTC, when a PR labelled `release:asap` merges, and
on demand) cuts forge first, then the consumers:

- **web-runtime**, when `web-runtime/package.json` is ahead of its newest
  `web-runtime/v*` tag: `scripts/release-web-runtime.sh vX.Y.Z`, then the tag push
  that publishes it. The version bump itself is a reviewed PR; the train never
  invents one, and reports commits under `web-runtime/` that no bump has released.
- **forge**, when `main` has commits since the last `v*` tag and main's tip has a
  green full `ci.yml` run on push (the proof `scripts/check-release-tests.sh` reads):
  `scripts/release-forge.sh vX.Y.Z`, the next patch version. It waits up to 30
  minutes (60 for `release:asap`) for a run in progress on the tip; the release
  commit must land on main's tip, so only the tip will do.
- **the consumer bumps** (step 2 below), as one pull request per downstream repo
  labelled `merge:queue` + `merge:priority`, when a consumer does not already pin a
  forge that contains the release. pin-bot normally has: it pins forge `main` every
  two hours.

Need a forge change released sooner: label its PR **`release:asap`**. Both release
scripts take the machine-wide release lock (`scripts/lib/release-lock.sh`, a
`flock` on `$RELEASE_LOCK_DIR/forge.lock`, default `~/fm-logs/release-locks`), so a
second release of forge or web-runtime fails at once and names who holds it. The
steps below are what the train runs, for whoever fixes it.

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
   **different** commit (see below), then requires the latest full `ci.yml` run
   at the exact source `HEAD` to have succeeded. PR/merge-queue runs do not
   count: `Test`, `Test (internal/cli)`, and
   `Test (-short whole tier, then guards)` must each succeed, even if `CI gate`
   waived an existing red. Missing, queued, failed, or unreadable proof blocks
   the release before files change; wait for main or dispatch full CI at that
   commit. `gh` authentication is required for GitHub publication;
3. writes the release's section of `CHANGELOG.md` (see below) and formats it
   with the prettier the pre-commit hook pins;
4. syncs `VERSION` and `internal/buildinfo/VERSION` — which must stay
   byte-identical, since `buildinfo` embeds a copy;
5. builds the module;
6. commits, tags `vX.Y.Z` and pushes the branch and tag in a single atomic
   `git push`;
7. creates the GitHub release (`gh release create`) with that section as its notes.

`--dry-run` runs every validation and every file edit, prints the plan, then
restores the tree — no commit, no tag, no push.

### CHANGELOG.md and the GitHub release notes

`CHANGELOG.md` is written **only here**, never in a pull request —
`scripts/check-changelog-untouched.sh` fails a PR that edits it. The script
builds the new `## [vX.Y.Z] - <date>` section (`scripts/changelog-release.sh`)
from the squash commits `(#N)` between the previous `v*` tag and `HEAD` — the
merged PR titles, read from git so it works offline — and groups them by
conventional-commit type: `feat` → Added, `fix` → Fixed, everything else →
Changed. A commit with `!` after its type, a `BREAKING CHANGE:` footer, or a PR
labelled `breaking` (looked up with `gh pr list` when `gh` is available) goes
under **Breaking changes** instead. Whatever `## [Unreleased]` held is folded
into the section once and `[Unreleased]` is left empty, so **the PR title is the
changelog entry: write a good one.**

After the push, `gh release create vX.Y.Z --verify-tag --notes-file <section>`
publishes the same section. If that step fails the tag is already pushed and
immutable; the script says so, prints the retry command, and exits non-zero. A
section over GitHub's 125000-character body limit (the first release after the
old per-PR habit folds in a very long `[Unreleased]`) is published as the
generated bullets plus a link to `CHANGELOG.md`.

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

### Installers (what the release script ships beyond the tag)

`release-forge.sh` also publishes what lets someone with no Go toolchain install
forge. All of it hangs off one naming contract, owned by
`scripts/build-release-assets.sh`: `forge_<os>_<arch>.tar.gz` for
linux/darwin/windows × amd64/arm64 (the binary, `LICENSE`, `NOTICE`) plus
`checksums.txt`, attached to the GitHub release. Change a name there and you must
change `install.sh`, `install.ps1`, `npm/lib/install.js` and
`scripts/render-homebrew-formula.sh` in the same pull request;
`internal/installers` fails if they drift.

| Channel                                                   | What the release does                                                                                                                                                                     | Needs a human                                                                                                                                                        |
| --------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `curl \| sh` (`install.sh`), `irm \| iex` (`install.ps1`) | Nothing: they read `main` and download the release's assets.                                                                                                                              | No                                                                                                                                                                   |
| GitHub release assets                                     | `build-release-assets.sh` runs before the release commit; `gh release create` attaches the archives and `checksums.txt`.                                                                  | No                                                                                                                                                                   |
| Homebrew `reliant-labs/reliant/forge`                     | Dispatches `forge-release-published` to `reliant-labs/homebrew-reliant`; its `update-forge-formula.yml` renders `Formula/forge.rb` (`scripts/render-homebrew-formula.sh`) and opens a PR. | Yes: the tap's ruleset requires an approving review, same as the cask. The workflow lives in the tap; see `docs/installers/update-forge-formula.yml` for the source. |
| npm `@reliantlabs/forge`                                  | `npm/package.json` is synced to the release version in the release commit. Publishing happens only with `--publish-npm` (then `verify-npm-published.sh` waits for the registry).          | Yes: `npm publish` needs an npm login with rights to the `@reliantlabs` scope (and its OTP), or a trusted-publisher registration like web-runtime's.                 |

Publish npm by hand after any release with `(cd npm && npm publish --access public)`.
A prerelease tag gets assets but is never sent to Homebrew or npm.

If the GitHub release step fails after the tag is pushed, the script prints the
retry command; build the archives again with
`scripts/build-release-assets.sh vX.Y.Z dist/` and attach `dist/*` to it.

## 2. Bump the consumers

Pre-launch, reliant and control-plane pin forge by **commit**, not by tag
(reliant `dev-docs/pinning.md` records why, and the switch back at launch). A
release tag is still a valid pin: both pin scripts take any ref.

```sh
cd reliant
make pin-forge REF=vX.Y.Z          # go.mod, go.sum, forge.yaml forge_version, regenerated output
go build ./...                     # then open the PR

cd ../control-plane                # AFTER reliant: control-plane depends on it
task pin:forge REF=vX.Y.Z          # go.mod, gen/go.mod and forge.yaml forge_version together
```

Each script resolves the ref against forge's remote rather than the local
`../forge` checkout, edits every pin together (a partial bump is the failure
they exist to prevent), and stops without committing. Commit and open the PR yourself.

`forge_version` in `forge.yaml` and the `go.mod` require are necessarily the
**same version** — one module, one version — and both repos' CI installs the
forge CLI from `forge_version` and fails when the two disagree, so there are no
per-workflow `go install ...@vX.Y.Z` pins to chase.

### The KCL module needs no tag

There is deliberately no `kcl-vX.Y.Z` step here. The forge KCL module is
embedded in the binary, and every render supplies it to KCL from the binary
doing the render, so a release publishes it automatically by shipping the
binary — a project pinned to the release renders against exactly its module.
Forge once scaffolded a published KCL git tag on release builds; the tag was
never pushed, and every project a released forge created could not resolve its
deploy manifests.

## 3. Forge's own CI needs no pin

Nothing to do here — this step is listed only because it used to exist.

`forge/.github/workflows/ci.yml` runs `go install ./cmd/forge`, building the
forge under test from the working tree. That is deliberate: installing a tag
would validate every PR against the LAST release, so no change to a template or
an emitter could ever go green until after it shipped.

## 4. Rebuild the daemon image

Once reliant's `go.mod` is on the new forge, the next daemon-image build (the
reliant binary → `Dockerfile.reliant.dev`) picks it up automatically. No manual
version edit; just rebuild/deploy per the normal flow.

## Note on history rewrites

If forge history is ever rewritten (e.g. redaction via `git filter-repo`), the
existing version tags move to new commit hashes. Force-push the moved tags, and
bump consumers to a **fresh** tag on the rewritten history — anything pinning the
moved tag will otherwise hit a go.sum/module-hash mismatch.
