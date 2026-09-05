#!/usr/bin/env bash
# release-forge.sh — cut a complete forge release in ONE command.
#
# forge ships two Go modules that must be released together:
#
#   github.com/reliant-labs/forge      → tag vX.Y.Z
#   github.com/reliant-labs/forge/pkg  → tag pkg/vX.Y.Z   (Go requires the
#                                        directory-prefixed form for submodules)
#
# WHY THIS EXISTS. The previous flow (`task release:pkg` → push → `go mod edit`
# → a SECOND commit → tag) put the two tags on two DIFFERENT commits and left
# three ways to ship a broken release:
#
#   1. Push ordering. The require bump cannot resolve until pkg/vX.Y.Z is
#      pushed, so the steps had to happen in a specific order across two
#      pushes. Stopping halfway published a pkg tag with no root release, or a
#      root release requiring a pkg version nobody could download.
#   2. The go.sum trap. `go build ./...` inside this repo passes WITHOUT the
#      forge/pkg hashes in go.sum, because go.work resolves pkg from the local
#      directory. The missing hashes only surface for a consumer running
#      `go mod download` outside the workspace — i.e. after release.
#   3. Three version files drifting. VERSION, internal/buildinfo/VERSION and
#      defaultPublishedForgePkgVersion each had to be bumped by hand.
#
# This script does all of it against ONE commit, and pushes that commit and
# both tags with a single atomic `git push`. Two tags remain — Go leaves no
# choice — but they point at the same SHA, which removes the ordering hazard
# entirely: either the whole release lands or none of it does.
#
# Usage:
#   scripts/release-forge.sh [--dry-run] [--repo <dir>] [--branch <name>] vX.Y.Z
#
#   --dry-run     run every validation and every file edit, print the plan,
#                 then RESTORE the working tree and create no commit or tag.
#   --repo DIR    operate on DIR instead of the enclosing git repo (tests).
#   --branch NAME the branch to push (default: main).
#
# scripts/release-pkg.sh still works and still tags pkg/ alone. It is the
# narrow tool; this is the one to reach for. See docs/releasing.md.
set -euo pipefail

DRY_RUN=0
REPO_ROOT=""
BRANCH="main"
VERSION=""

usage() {
  echo "usage: $0 [--dry-run] [--repo <dir>] [--branch <name>] vX.Y.Z" >&2
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    --repo)    [ $# -ge 2 ] || usage; REPO_ROOT="$2"; shift 2 ;;
    --branch)  [ $# -ge 2 ] || usage; BRANCH="$2"; shift 2 ;;
    -h|--help) usage ;;
    -*)        echo "error: unknown flag $1" >&2; usage ;;
    *)
      [ -z "$VERSION" ] || usage
      VERSION="$1"; shift ;;
  esac
done

[ -n "$VERSION" ] || usage

# ── 1. Version shape ────────────────────────────────────────────────
# Canonical semver with optional prerelease. Reject the tag-prefixed forms
# early — maintainers habitually paste `pkg/v1.2.3` or `v1.2.3+meta` back in.
if ! echo "$VERSION" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
  echo "error: version must look like vX.Y.Z (got: $VERSION)" >&2
  echo "hint: pass the bare version; the script adds the pkg/ tag prefix itself." >&2
  exit 1
fi
PKG_TAG="pkg/$VERSION"
ROOT_TAG="$VERSION"

# ── 2. Repo / module layout ─────────────────────────────────────────
if [ -z "$REPO_ROOT" ]; then
  REPO_ROOT="$(git rev-parse --show-toplevel)"
fi
cd "$REPO_ROOT"

for required in go.mod pkg/go.mod; do
  if [ ! -f "$required" ]; then
    echo "error: $REPO_ROOT/$required not found — not a forge repo checkout?" >&2
    exit 1
  fi
done

# Derive the module paths rather than hardcoding them, and assert the
# submodule relationship that makes the directory-prefixed tag correct. A repo
# where pkg/ is not "<root>/pkg" would need different tags entirely.
ROOT_MODULE="$(awk '/^module /{print $2; exit}' go.mod)"
PKG_MODULE="$(awk '/^module /{print $2; exit}' pkg/go.mod)"
if [ "$PKG_MODULE" != "$ROOT_MODULE/pkg" ]; then
  echo "error: pkg/go.mod declares '$PKG_MODULE', expected '$ROOT_MODULE/pkg'" >&2
  echo "hint: the pkg/vX.Y.Z tag convention only applies to a submodule at ./pkg." >&2
  exit 1
fi

VERSION_FILES=("VERSION" "internal/buildinfo/VERSION")
PKGDEP_FILE="internal/generator/project_pkgdep.go"
for f in "${VERSION_FILES[@]}" "$PKGDEP_FILE"; do
  if [ ! -f "$f" ]; then
    echo "error: expected version file $f not found" >&2
    exit 1
  fi
done

# ── 3. Clean tree ───────────────────────────────────────────────────
# The WHOLE tree, not just pkg/: this script commits root-module files, so any
# unrelated uncommitted change would be swept into the release commit. (The
# stage below names every path explicitly, but a dirty tree still means the
# tag would not describe a state anyone can reproduce.)
if [ -n "$(git status --porcelain)" ]; then
  echo "error: working tree is not clean; commit or set aside your changes before releasing" >&2
  git status --short >&2
  exit 1
fi

# ── 4. Neither tag may already exist ────────────────────────────────
for tag in "$PKG_TAG" "$ROOT_TAG"; do
  if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
    echo "error: tag $tag already exists (versions are immutable; bump instead)" >&2
    exit 1
  fi
done

# ── 5. Standalone build validation ──────────────────────────────────
# GOWORK=off detaches go.work (which stitches pkg to the main module).
# -mod=readonly is load-bearing: the validation must FAIL when pkg/go.mod is
# incomplete standalone, never silently edit it. This is the consumer's view:
# if it does not compile here, the tag would publish a broken version.
echo "→ validating pkg module builds standalone (GOWORK=off go build ./...)"
( cd pkg && GOWORK=off GOFLAGS=-mod=readonly go build ./... )
echo "→ validating pkg module vets standalone (GOWORK=off go vet ./...)"
( cd pkg && GOWORK=off GOFLAGS=-mod=readonly go vet ./... )

# ── 6. Edit the release files ───────────────────────────────────────
# Everything below mutates the working tree. Back the files up first and
# restore them on ANY exit that is not a completed real release, so a failed
# or dry run leaves the checkout exactly as it was found. This matters more
# than usual here: the tree is shared with other agents, and the alternative
# cleanup (`git checkout -- <path>`) is exactly the destructive verb that is
# forbidden in this repo.
BACKUP_DIR="$(mktemp -d)"
RESTORE_ON_EXIT=1
TOUCHED=("go.mod" "go.sum" "${VERSION_FILES[@]}" "$PKGDEP_FILE")

restore_tree() {
  if [ "$RESTORE_ON_EXIT" = "1" ]; then
    for f in "${TOUCHED[@]}"; do
      backup="$BACKUP_DIR/$(echo "$f" | tr '/' '_')"
      if [ -f "$backup" ]; then
        cp "$backup" "$f"
      else
        # The file did NOT exist before this run, so restoring means removing
        # it. A repo with no go.sum yet (or a fixture) would otherwise be left
        # with a stray untracked file that the next run's clean-tree gate
        # rejects — a dry run must leave NO trace.
        rm -f "$f"
      fi
    done
  fi
  rm -rf "$BACKUP_DIR" "${CLONE_DIR:-}"
}
trap restore_tree EXIT

for f in "${TOUCHED[@]}"; do
  [ -f "$f" ] && cp "$f" "$BACKUP_DIR/$(echo "$f" | tr '/' '_')"
done

echo "→ requiring $PKG_MODULE@$VERSION"
go mod edit -require="$PKG_MODULE@$VERSION"

echo "→ syncing version files to $VERSION"
for f in "${VERSION_FILES[@]}"; do
  # internal/buildinfo/VERSION is a build-time copy of the root VERSION and
  # must stay BYTE-identical (TestEmbeddedVersionFileMatchesSource enforces
  # it) — an embed directive cannot reach outside its own package directory.
  printf '%s\n' "$VERSION" > "$f"
done

# defaultPublishedForgePkgVersion is the forge/pkg pin a DEV build writes into
# every scaffold (a release build uses its ldflags stamp instead). If it lags,
# dev-built scaffolds pin an old forge/pkg and generated code targeting newer
# APIs will not compile. resolveForgePkgVersion() is its only reader.
echo "→ bumping defaultPublishedForgePkgVersion in $PKGDEP_FILE"
PKGDEP_TMP="$(mktemp)"
sed -e "s|^const defaultPublishedForgePkgVersion = \".*\"$|const defaultPublishedForgePkgVersion = \"$VERSION\"|" \
  "$PKGDEP_FILE" > "$PKGDEP_TMP"
mv "$PKGDEP_TMP" "$PKGDEP_FILE"
if ! grep -q "^const defaultPublishedForgePkgVersion = \"$VERSION\"$" "$PKGDEP_FILE"; then
  echo "error: failed to bump defaultPublishedForgePkgVersion in $PKGDEP_FILE" >&2
  echo "hint: the declaration's shape changed; update the sed pattern in $0." >&2
  exit 1
fi

# ── 7. Populate go.sum for a version that is not pushed yet ─────────
# THE go.sum TRAP, and why this step is the heart of the script.
#
# An in-workspace `go build ./...` passes with NO forge/pkg hashes in go.sum,
# because go.work resolves pkg from ./pkg on disk. A consumer has no go.work,
# so their `go mod download` needs the hashes — and their absence is invisible
# here until after the release is public.
#
# The obstacle: hashes normally come from the module proxy, which cannot serve
# a tag that has not been pushed. We break that circularity with a temporary
# BARE CLONE of this repo, tagged locally, resolved with GOPROXY=direct.
#
# This is sound because a module's h1: hash is a digest of the module's FILE
# TREE, not of the commit that carries the tag. The release commit touches
# only root-module files (go.mod, go.sum, VERSION, buildinfo/VERSION,
# project_pkgdep.go) and never pkg/, so the pkg/ tree — and therefore the hash
# — is identical whether the tag sits at HEAD or at the release commit. The
# assertion below proves that precondition rather than assuming it, and CI
# re-resolves against the real proxy after the push.
#
# The clone is used instead of tagging this repo directly so the shared
# checkout's tag namespace is never touched by a run that might fail.
echo "→ resolving $PKG_MODULE@$VERSION hashes into go.sum (via a local clone)"

HEAD_PKG_TREE="$(git rev-parse "HEAD:pkg")"
if [ -n "$(git status --porcelain -- pkg/)" ]; then
  echo "error: pkg/ has uncommitted changes — the resolved hash would not match the tag" >&2
  exit 1
fi

CLONE_DIR="$(mktemp -d)/forge.git"
git clone --bare --quiet "$REPO_ROOT" "$CLONE_DIR"
git -C "$CLONE_DIR" tag -f "$PKG_TAG" HEAD >/dev/null

# GOPROXY=direct + url.insteadOf sends the fetch at the local clone.
# GOSUMDB=off is required and safe: the checksum database cannot yet know a
# version that is not public. The hash we record is computed from the module
# content by the same dirhash algorithm the proxy will use, so it matches the
# public one once the tag is pushed — VerifyReleaseHashes below re-checks that
# against the real proxy after the push, which is where a mismatch would
# actually be actionable.
if ! GIT_CONFIG_COUNT=1 \
     GIT_CONFIG_KEY_0="url.file://$CLONE_DIR.insteadOf" \
     GIT_CONFIG_VALUE_0="https://$ROOT_MODULE" \
     GOWORK=off GOFLAGS=-mod=mod GOPRIVATE="$ROOT_MODULE" GOPROXY=direct GOSUMDB=off GONOSUMDB="$ROOT_MODULE" \
     go mod download "$PKG_MODULE"; then
  echo "error: could not resolve $PKG_MODULE@$VERSION from the local clone" >&2
  exit 1
fi

# ── 8. Assert the hashes actually landed ────────────────────────────
# The check the old flow lacked entirely. Without it, a silently-skipped
# resolution ships a root module whose go.sum cannot verify forge/pkg, and the
# first person to find out is a consumer outside the workspace.
# A missing go.sum is itself the failure this asserts against, so probe for the
# file before grepping it: `grep -c` on a nonexistent path prints an error and
# yields an EMPTY string, which turns the numeric test below into a syntax
# error and lets the check fall through instead of failing cleanly.
if [ ! -f go.sum ]; then
  echo "error: go.sum does not exist after resolution — no hashes were recorded at all." >&2
  echo "hint: this is the go.sum trap; see the comment above this check." >&2
  exit 1
fi
GOSUM_HITS="$(grep -c "^$PKG_MODULE $VERSION" go.sum || true)"
if [ -z "$GOSUM_HITS" ] || [ "$GOSUM_HITS" -eq 0 ]; then
  echo "error: go.sum has no hashes for $PKG_MODULE $VERSION after resolution." >&2
  echo "hint: this is the go.sum trap — an in-workspace build would still pass," >&2
  echo "      but every consumer outside the workspace would fail to verify." >&2
  exit 1
fi
# Both the module zip (h1:) and the go.mod hash must be present; a consumer
# needs each, and a partial go.sum fails only at their end.
if ! grep -q "^$PKG_MODULE $VERSION h1:" go.sum; then
  echo "error: go.sum is missing the module (h1:) hash for $PKG_MODULE $VERSION" >&2
  exit 1
fi
if ! grep -q "^$PKG_MODULE $VERSION/go.mod h1:" go.sum; then
  echo "error: go.sum is missing the /go.mod hash for $PKG_MODULE $VERSION" >&2
  exit 1
fi
echo "  go.sum: $GOSUM_HITS entries for $PKG_MODULE $VERSION"

# ── 9. The root module still builds with the new require ────────────
echo "→ building the root module against the new require"
go build ./...

# ── 10. Commit, tag, push (or describe the plan) ────────────────────
# The pkg/ tree must be untouched by everything above, or the hash resolved in
# step 7 describes different bytes than the tag will point at.
if [ "$(git rev-parse "HEAD:pkg")" != "$HEAD_PKG_TREE" ]; then
  echo "error: pkg/ changed during the release edits — the resolved hash is stale" >&2
  exit 1
fi
if [ -n "$(git status --porcelain -- pkg/)" ]; then
  echo "error: pkg/ is dirty after the release edits; refusing to tag" >&2
  exit 1
fi

if [ "$DRY_RUN" = "1" ]; then
  echo ""
  echo "DRY RUN: all validations passed."
  echo "DRY RUN: would stage: ${TOUCHED[*]}"
  echo "DRY RUN: would commit: chore: release $VERSION"
  echo "DRY RUN: would tag BOTH $PKG_TAG and $ROOT_TAG at that one commit"
  echo "DRY RUN: would push:   git push --atomic origin $BRANCH $PKG_TAG $ROOT_TAG"
  echo ""
  echo "DRY RUN: restoring the working tree; no commit, tag or push was created."
  exit 0
fi

# Stage ONLY the files this script edited, by explicit path. Never `git add -A`
# — this checkout is routinely shared with other agents whose in-flight work
# would otherwise be swept into a release commit.
git add -- "${TOUCHED[@]}"
git commit -q -m "chore: release $VERSION

Require $PKG_MODULE@$VERSION, sync VERSION, internal/buildinfo/VERSION and
defaultPublishedForgePkgVersion, and record the forge/pkg hashes in go.sum.

Tagged $PKG_TAG and $ROOT_TAG at this commit."

RELEASE_SHA="$(git rev-parse HEAD)"
# From here the tree is intentionally changed; do not restore it on exit.
RESTORE_ON_EXIT=0

git tag -a "$PKG_TAG"  -m "$PKG_MODULE $VERSION"
git tag -a "$ROOT_TAG" -m "forge $VERSION"

echo ""
echo "✅ committed $(git rev-parse --short HEAD) and tagged BOTH $PKG_TAG and $ROOT_TAG at it"
echo ""
echo "→ pushing atomically (branch + both tags, all-or-nothing)"
# --atomic is the whole point: without it git pushes each ref separately and a
# partial failure recreates the split-release state this script exists to
# prevent.
git push --atomic origin "$BRANCH" "$PKG_TAG" "$ROOT_TAG"

echo ""
echo "✅ released $VERSION at $RELEASE_SHA"
echo ""
echo "Verify the published hashes match what was committed (uses the REAL proxy,"
echo "so it confirms the locally-computed go.sum entries were correct):"
echo "  GOWORK=off GOPROXY=proxy.golang.org GONOSUMDB= GOFLAGS=-mod=mod \\"
echo "    go mod download -x $PKG_MODULE@$VERSION"
echo ""
echo "Then propagate the bump to consumers — see docs/releasing.md steps 2-3."
