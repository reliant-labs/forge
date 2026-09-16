#!/usr/bin/env bash
# release-forge.sh — cut a forge release in ONE command.
#
# forge is ONE Go module, github.com/reliant-labs/forge, tagged vX.Y.Z. That
# is a recent simplification and it is most of why this script is now short.
#
# WHAT USED TO BE HERE. forge shipped a second module, forge/pkg, tagged
# pkg/vX.Y.Z, and the root module REQUIRED it. Releasing therefore meant
# tagging pkg, bumping the require, and tagging root — and that require could
# not resolve until the pkg tag was pushed, so this script carried three
# elaborate mechanisms to close the circle:
#
#   1. a two-tag atomic push, so a pkg tag could never ship without its root
#      release (or vice versa);
#   2. the go.sum trap — an in-repo `go build ./...` passed WITHOUT forge/pkg
#      hashes in go.sum because go.work resolved pkg from disk, so the script
#      made a temporary bare clone, tagged it locally, and resolved the
#      not-yet-public version through GOPROXY=direct to record real hashes;
#   3. a bump of `defaultPublishedForgePkgVersion`, the constant a dev build
#      wrote into scaffolds as the forge/pkg pin.
#
# All three are gone with the submodule. A single module cannot require
# itself, so there is no unpushed version to resolve, no second tag to order,
# and no hand-maintained pin to drift. What remains is what a release
# genuinely needs: validate, sync the version files, tag once, push atomically.
#
# Usage:
#   scripts/release-forge.sh [--dry-run] [--repo <dir>] [--branch <name>]
#                            [--skip-proxy-check] vX.Y.Z
#
#   --dry-run     run every validation and every file edit, print the plan,
#                 then RESTORE the working tree and create no commit or tag.
#   --repo DIR    operate on DIR instead of the enclosing git repo (tests).
#   --branch NAME the branch to push (default: main).
#   --skip-proxy-check
#                 skip the immutable-version gate (step 5). Only for an
#                 offline release where you have verified BY HAND that the
#                 version has never been published. See that step's comment.
#
# See docs/releasing.md.
set -euo pipefail

DRY_RUN=0
REPO_ROOT=""
BRANCH="main"
VERSION=""
SKIP_PROXY_CHECK=0
# Overridable so the tests can point the check at a local fixture server
# instead of the public proxy; nothing else should set it.
PROXY_BASE="${FORGE_RELEASE_PROXY_BASE:-https://proxy.golang.org}"

usage() {
  echo "usage: $0 [--dry-run] [--repo <dir>] [--branch <name>] [--skip-proxy-check] vX.Y.Z" >&2
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    --skip-proxy-check) SKIP_PROXY_CHECK=1; shift ;;
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
  echo "hint: pass the bare version, with the leading v and no build metadata." >&2
  exit 1
fi
RELEASE_TAG="$VERSION"

# ── 2. Repo / module layout ─────────────────────────────────────────
if [ -z "$REPO_ROOT" ]; then
  REPO_ROOT="$(git rev-parse --show-toplevel)"
fi
cd "$REPO_ROOT"

if [ ! -f go.mod ]; then
  echo "error: $REPO_ROOT/go.mod not found — not a forge repo checkout?" >&2
  exit 1
fi

# Derive the module path rather than hardcoding it. forge is ONE module: pkg/
# is a directory inside it, so there is no submodule relationship to assert
# and no directory-prefixed tag to derive.
ROOT_MODULE="$(awk '/^module /{print $2; exit}' go.mod)"

VERSION_FILES=("VERSION" "internal/buildinfo/VERSION")
for f in "${VERSION_FILES[@]}"; do
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

# ── 4. The tag may not already exist ────────────────────────────────
if git rev-parse -q --verify "refs/tags/$RELEASE_TAG" >/dev/null; then
  echo "error: tag $RELEASE_TAG already exists (versions are immutable; bump instead)" >&2
  exit 1
fi

# ── 5. The version must not already be PUBLISHED at another commit ──
# proxy.golang.org is IMMUTABLE. Once it has served a version, that content is
# permanent: deleting the tag and re-cutting it at a different commit does
# NOT change what the proxy serves, and there is no way to correct it. The
# version is burned forever.
#
# This really happened. pkg/v0.1.12 was tagged, published, deleted, and
# re-cut elsewhere; the proxy still serves the first tree, which does not
# compile. The symptom is maddening from inside the repo — `git show <tag>`
# plainly contains your code while every consumer gets the old bytes — so the
# gate belongs HERE, before a tag exists, not in the debugging afterwards.
#
# Step 4 only sees LOCAL tags, and the poisoning case is precisely the one
# where the local tag was deleted. Only the proxy knows.
#
# A network failure must NOT pass this check. "The proxy said 404" and "curl
# could not reach the proxy" are the same empty output and must never be
# conflated, so curl's exit status is inspected before its body: unreachable
# is a hard error, and --skip-proxy-check is the only way past it.
proxy_published_commit() {
  # Echoes the published commit hash, or nothing if the version is unpublished.
  # Exits 1 (with a diagnosis on stderr) if the proxy could not be consulted.
  local module="$1" version="$2" body http_code curl_status hash
  # -sS keeps the progress meter off but lets curl's own diagnosis reach
  # stderr, which is worth more than the exit code alone when this fails.
  body="$(curl -sS -w '\n%{http_code}' --max-time 30 \
    "$PROXY_BASE/$module/@v/$version.info")" && curl_status=0 || curl_status=$?

  if [ "$curl_status" -ne 0 ]; then
    echo "error: could not reach the Go module proxy to check whether $version is already published" >&2
    echo "       (curl exited $curl_status for $PROXY_BASE/$module/@v/$version.info)" >&2
    echo "hint: this check is NOT optional — publishing over an existing version is" >&2
    echo "      unrecoverable, so a proxy it cannot read is a hard stop. Fix the" >&2
    echo "      network, or pass --skip-proxy-check if you have verified BY HAND" >&2
    echo "      that $version has never been published." >&2
    return 1
  fi

  http_code="$(printf '%s' "$body" | tail -n 1)"
  body="$(printf '%s' "$body" | sed '$d')"

  case "$http_code" in
    404|410)
      # The proxy is authoritative that this version does not exist. Free.
      return 0 ;;
    200)
      ;;
    *)
      echo "error: the Go module proxy returned HTTP $http_code for $module@$version" >&2
      echo "       $body" >&2
      echo "hint: this check cannot be answered, and publishing over an existing" >&2
      echo "      version is unrecoverable. Retry, or pass --skip-proxy-check if" >&2
      echo "      you have verified BY HAND that $version has never been published." >&2
      return 1 ;;
  esac

  # Published. Pull Origin.Hash out of the .info JSON without a jq dependency.
  hash="$(printf '%s' "$body" | grep -o '"Hash":"[0-9a-fA-F]*"' | head -n 1 | cut -d'"' -f4)"
  if [ -z "$hash" ]; then
    # Published but the origin commit is unknown, so equality cannot be
    # proven. Refusing is the only safe reading: an unprovable match is a
    # possible overwrite of immutable bytes.
    echo "error: $module@$version is ALREADY PUBLISHED on the module proxy, and the" >&2
    echo "       response carries no origin commit to compare against:" >&2
    echo "       $body" >&2
    echo "hint: proxy.golang.org is immutable — this version can never be corrected." >&2
    echo "      Bump to the next version." >&2
    return 1
  fi
  printf '%s\n' "$hash"
}

if [ "$SKIP_PROXY_CHECK" = "1" ]; then
  echo "→ SKIPPING the immutable-version proxy check (--skip-proxy-check)"
  echo "  you are asserting that $VERSION has NEVER been published. If it has,"
  echo "  this release is permanently broken and cannot be fixed."
else
  echo "→ checking $VERSION is not already published on the module proxy"
  # The commit that will carry the tags. The release commit is created on top
  # of HEAD, so an already-published version can only legitimately match when
  # HEAD *is* that release commit (a re-run after a successful push) — every
  # other match means the tag is about to move, which is the poisoning case.
  TAG_COMMIT="$(git rev-parse HEAD)"
  module="$ROOT_MODULE"
  if ! PUBLISHED_COMMIT="$(proxy_published_commit "$module" "$VERSION")"; then
    exit 1
  fi
  if [ -n "$PUBLISHED_COMMIT" ] && [ "$PUBLISHED_COMMIT" = "$TAG_COMMIT" ]; then
    echo "  $module@$VERSION: already published at $PUBLISHED_COMMIT (this commit) — idempotent"
  elif [ -z "$PUBLISHED_COMMIT" ]; then
    echo "  $module@$VERSION: not published — free to use"
  else
    echo "error: REFUSING TO RELEASE — $VERSION IS ALREADY PUBLISHED AT A DIFFERENT COMMIT." >&2
    echo "" >&2
    echo "  module:            $module" >&2
    echo "  published commit:  $PUBLISHED_COMMIT   (what proxy.golang.org serves today)" >&2
    echo "  commit to be used: $TAG_COMMIT   (what you are about to tag)" >&2
    echo "" >&2
    echo "This is NOT a network problem and NOT a transient error. The proxy answered" >&2
    echo "successfully: $module@$VERSION already exists, built from a different commit." >&2
    echo "" >&2
    echo "proxy.golang.org is IMMUTABLE. The content it has already served for" >&2
    echo "$VERSION is permanent. Deleting the tag and re-cutting it here will NOT" >&2
    echo "change what consumers download — they will keep getting $PUBLISHED_COMMIT" >&2
    echo "forever, and $VERSION can never be corrected by any action you take." >&2
    echo "" >&2
    echo "Release a NEW version instead. Treat $VERSION as burned and skip it." >&2
    exit 1
  fi
fi

# ── 7. Edit the release files ───────────────────────────────────────
# Everything below mutates the working tree. Back the files up first and
# restore them on ANY exit that is not a completed real release, so a failed
# or dry run leaves the checkout exactly as it was found. This matters more
# than usual here: the tree is shared with other agents, and the alternative
# cleanup (`git checkout -- <path>`) is exactly the destructive verb that is
# forbidden in this repo.
BACKUP_DIR="$(mktemp -d)"
RESTORE_ON_EXIT=1
TOUCHED=("${VERSION_FILES[@]}")

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

echo "→ syncing version files to $VERSION"
for f in "${VERSION_FILES[@]}"; do
  # internal/buildinfo/VERSION is a build-time copy of the root VERSION and
  # must stay BYTE-identical (TestEmbeddedVersionFileMatchesSource enforces
  # it) — an embed directive cannot reach outside its own package directory.
  printf '%s\n' "$VERSION" > "$f"
done

# ── 8. The module still builds ──────────────────────────────────────
# There is no submodule require to resolve any more, and therefore no go.sum
# trap: forge is one module, so a consumer's `go mod download` needs hashes
# for forge's own DEPENDENCIES, which are already in go.sum from ordinary
# development. The elaborate local-bare-clone dance this script used to
# perform existed solely to record hashes for a forge/pkg tag that was not
# pushed yet.
echo "→ building the module"
go build ./...

# ── 9. Commit, tag, push (or describe the plan) ─────────────────────
if [ "$DRY_RUN" = "1" ]; then
  echo ""
  echo "DRY RUN: all validations passed."
  echo "DRY RUN: would stage: ${TOUCHED[*]}"
  echo "DRY RUN: would commit: chore: release $VERSION"
  echo "DRY RUN: would tag $RELEASE_TAG at that commit"
  echo "DRY RUN: would push:   git push --atomic origin $BRANCH $RELEASE_TAG"
  echo ""
  echo "DRY RUN: restoring the working tree; no commit, tag or push was created."
  exit 0
fi

# Stage ONLY the files this script edited, by explicit path. Never `git add -A`
# — this checkout is routinely shared with other agents whose in-flight work
# would otherwise be swept into a release commit.
git add -- "${TOUCHED[@]}"
git commit -q -m "chore: release $VERSION

Sync VERSION and internal/buildinfo/VERSION.

Tagged $RELEASE_TAG at this commit."

RELEASE_SHA="$(git rev-parse HEAD)"
# From here the tree is intentionally changed; do not restore it on exit.
RESTORE_ON_EXIT=0

git tag -a "$RELEASE_TAG" -m "forge $VERSION"

echo ""
echo "✅ committed $(git rev-parse --short HEAD) and tagged $RELEASE_TAG at it"
echo ""
echo "→ pushing atomically (branch + tag, all-or-nothing)"
# --atomic keeps the branch and the tag from landing separately: a tag without
# its commit on the branch is a release nobody can reproduce.
git push --atomic origin "$BRANCH" "$RELEASE_TAG"

echo ""
echo "✅ released $VERSION at $RELEASE_SHA"
echo ""
echo "Verify the proxy serves what was tagged:"
echo "  GOPROXY=proxy.golang.org GOFLAGS=-mod=mod go mod download -x $ROOT_MODULE@$VERSION"
echo ""
echo "Then propagate the bump to consumers — see docs/releasing.md steps 2-3."
