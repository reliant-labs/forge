#!/usr/bin/env bash
# release-web-runtime.sh — validate, tag and PUBLISH a release of the
# @reliantlabs/forge-web-runtime npm package.
#
# THIS IS THE ONLY PUBLISH PATH. .github/workflows/release-web-runtime.yml,
# which published on a web-runtime/v* tag push, is deleted: releases are
# local-only and CI runs checks only. There is nothing to trigger and nothing
# to watch.
#
# The npm twin of scripts/release-pkg.sh, and it exists for the same reason:
# minting a release by hand is easy to get subtly wrong, and the failure is
# only visible downstream, after a scaffolded project can no longer install.
#
# What it validates, in order:
#   1. the version shape (vX.Y.Z, optional prerelease);
#   2. a CLEAN web-runtime/ tree — a dirty tree means the tarball would not
#      match the tag;
#   3. that the tag does not already exist;
#   4. that package.json's `version` agrees with the requested version;
#   5. that the package BUILDS, TYPECHECKS and its tests PASS;
#   6. that `npm pack` contains exactly the declared `files` surface — the
#      check that catches a build which silently emitted nothing, which is
#      indistinguishable from a healthy publish until an install fails;
#   7. that forge's `webRuntimePublishedRange` tracks this version, so a
#      released forge cannot scaffold a range that does not exist yet.
#
# Usage:
#   scripts/release-web-runtime.sh [--dry-run] vX.Y.Z
#   task release:web-runtime -- vX.Y.Z
#
# A real (non-dry-run) invocation runs every validation, tags, publishes to
# npm, confirms the registry actually SERVES the version, and only then pushes
# the tag. Nothing further is required.
#
# ORDER IS LOAD-BEARING: publish, verify, THEN push the tag. A pushed tag whose
# version was never published is the drift this script exists to prevent —
# web-runtime/v0.3.1 sat in exactly that state, invisible until a scaffolded
# project failed to install days later, and v0.3.0 shipped a devlog module
# that existed in the source and the templates but never reached the registry.
# Pushing the tag last means the tag can only exist once the artifact does.
#
# WHAT WAS LOST WITH THE WORKFLOW, stated plainly. It authenticated by npm
# TRUSTED PUBLISHING (OIDC): no stored credential, a token worthless off the
# runner, and a --provenance attestation binding the tarball to the commit and
# the CI run. A local publish cannot reproduce any of that — OIDC has no
# meaning off a runner — so this authenticates with your own `npm login` and
# the publish is attributed to a person. That is a genuine downgrade, and it
# is the deliberate cost of the local-only rule.
#
# It also ran on a clean checkout with a fresh install, which is the only place
# "the tarball a stranger downloads is correct" can really be established. A
# maintainer's node_modules cannot establish that. Mitigation: the pack-contents
# check below asserts the tarball's actual surface, which is the specific
# failure that clean-checkout property was there to catch.
set -euo pipefail

DRY_RUN=0
VERSION=""

usage() {
  echo "usage: $0 [--dry-run] vX.Y.Z" >&2
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage ;;
    -*)        echo "error: unknown flag $1" >&2; usage ;;
    *)
      [ -z "$VERSION" ] || usage
      VERSION="$1"; shift ;;
  esac
done

[ -n "$VERSION" ] || usage

# ── 1. Version shape ────────────────────────────────────────────────
# Canonical semver with optional prerelease. Reject the tag-prefixed form
# early — users habitually paste `web-runtime/v1.2.3` back in.
if ! echo "$VERSION" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
  echo "error: version must look like vX.Y.Z (got: $VERSION)" >&2
  echo "hint: pass the bare version; the script adds the web-runtime/ tag prefix itself." >&2
  exit 1
fi
TAG="web-runtime/$VERSION"
BARE="${VERSION#v}"

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

PKG_DIR="web-runtime"
if [ ! -f "$PKG_DIR/package.json" ]; then
  echo "error: $PKG_DIR/package.json not found (run from the forge repo)" >&2
  exit 1
fi

# ── 2. Clean tree ───────────────────────────────────────────────────
# Scoped to web-runtime/: unrelated work elsewhere in the repo must not block
# a release, but an uncommitted change to the package itself means the tag
# would not describe the bytes that get published.
if [ -n "$(git status --porcelain -- "$PKG_DIR")" ]; then
  echo "error: $PKG_DIR/ has uncommitted changes — commit or stash them first." >&2
  git status --short -- "$PKG_DIR" >&2
  exit 1
fi

# ── 3. Tag must not exist ───────────────────────────────────────────
if git rev-parse -q --verify "refs/tags/$TAG" >/dev/null; then
  echo "error: tag $TAG already exists." >&2
  exit 1
fi

# ── 4. package.json agrees ──────────────────────────────────────────
# The tag and the manifest must name the same version, or `npm publish` ships
# something the tag does not describe.
PKG_VERSION="$(node -p "require('./$PKG_DIR/package.json').version")"
if [ "$PKG_VERSION" != "$BARE" ]; then
  echo "error: $PKG_DIR/package.json says version $PKG_VERSION, but you asked for $BARE." >&2
  echo "hint: bump the version in package.json (and commit) before tagging." >&2
  exit 1
fi

PKG_NAME="$(node -p "require('./$PKG_DIR/package.json').name")"

# ── 5. Build, typecheck, test ───────────────────────────────────────
echo "==> building $PKG_NAME@$BARE"
( cd "$PKG_DIR" && npm run build )
echo "==> typechecking"
( cd "$PKG_DIR" && npm run typecheck )
echo "==> testing"
( cd "$PKG_DIR" && npm test )

# ── 6. The tarball actually contains the package ────────────────────
# `files` in package.json declares dist/ + interceptors/ + README. A build
# that emitted nothing still packs "successfully" — with no dist/ — and the
# breakage only surfaces when a consumer imports the barrel and gets a module
# resolution error. Assert the entry point is really in there.
echo "==> verifying pack contents"
PACK_LIST="$(cd "$PKG_DIR" && npm pack --dry-run --json)"
for required in "dist/index.js" "dist/index.d.ts"; do
  if ! echo "$PACK_LIST" | grep -q "\"$required\""; then
    echo "error: tarball is missing $required — did the build emit dist/?" >&2
    exit 1
  fi
done

# ── 7. forge's scaffold range tracks this version ───────────────────
# A released forge writes webRuntimePublishedRange into every scaffolded
# frontend's package.json. If it lags the version being released, forge
# scaffolds a range the registry cannot satisfy for the newest features —
# and the failure lands on a user, not here. (TestWebRuntimePublishedRange-
# TracksPackage enforces the same invariant in CI; this is the release-time
# copy so the check runs at the moment it matters.)
RANGE_FILE="internal/generator/frontend_webruntime.go"
DECLARED_RANGE="$(grep -oE 'webRuntimePublishedRange = "\^[0-9]+\.[0-9]+\.[0-9]+"' "$RANGE_FILE" | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' || true)"
RELEASE_MINOR="$(echo "$BARE" | cut -d. -f1-2)"
DECLARED_MINOR="$(echo "$DECLARED_RANGE" | cut -d. -f1-2)"
if [ "$RELEASE_MINOR" != "$DECLARED_MINOR" ]; then
  echo "error: $RANGE_FILE declares ^$DECLARED_RANGE but you are releasing $BARE." >&2
  echo "hint: update webRuntimePublishedRange to ^$BARE and commit before tagging." >&2
  exit 1
fi

# ── 8. provenance metadata ──────────────────────────────────────────
# `--provenance` makes npm compare package.json's `repository` against the
# repository in the signed provenance statement and reject the publish with a
# 422 when they disagree — an ABSENT field reads as "" and fails that
# comparison. This check exists because that failure happens at the WORST
# possible moment: the first 0.3.1 publish was authenticated, packed, signed
# and written to the sigstore transparency log before the registry refused it,
# and everything upstream reported success. Catching it here costs a second.
REPO_URL="$(node -p "require('./$PKG_DIR/package.json').repository?.url ?? ''")"
case "$REPO_URL" in
  *github.com/reliant-labs/forge*) ;;
  *)
    echo "error: $PKG_DIR/package.json repository.url is '${REPO_URL:-<absent>}'." >&2
    echo "hint: npm publish --provenance rejects this with a 422 AFTER signing." >&2
    echo "      Set it to git+https://github.com/reliant-labs/forge.git" >&2
    exit 1 ;;
esac

# ── 9. Tag and PUBLISH ──────────────────────────────────────────────
#
# THIS SCRIPT PUBLISHES. It used to stop at the tag and leave the publish to
# .github/workflows/release-web-runtime.yml, which triggered on the tag push.
# That workflow is deleted: releases are local-only, and a CI publish path is
# exactly what that rule forbids.
#
# AUTHENTICATION CHANGES WITH IT, and this is the real cost of the move. The
# workflow used npm TRUSTED PUBLISHING — GitHub minted a short-lived OIDC
# token, npm verified it came from this repo running that workflow file, and
# issued a credential that expired minutes later. No stored secret existed.
# A local publish cannot do that; OIDC has no meaning off a runner. So this
# uses your own `npm login` session, which means the publish is attributed to
# a PERSON rather than to a workflow. That is a real downgrade in provenance
# strength and it is the deliberate trade the local-only rule makes.
#
# --provenance is kept. Off a runner npm cannot produce the CI attestation,
# so it is passed only when npm reports it can; see below.
if [ "$DRY_RUN" -eq 1 ]; then
  echo
  echo "DRY RUN — every validation passed. Would create tag $TAG and publish $PKG_NAME@$BARE."
  exit 0
fi

# Fail BEFORE tagging if the session cannot publish. A tag with no artifact is
# the exact drift this whole script exists to prevent (web-runtime/v0.3.1).
if ! NPM_WHOAMI="$(cd "$PKG_DIR" && npm whoami 2>/dev/null)"; then
  echo "error: not logged in to npm — run 'npm login' first." >&2
  echo "hint: publishing @reliantlabs/* needs a member of the reliantlabs org." >&2
  exit 1
fi
echo "==> publishing as npm user: $NPM_WHOAMI"

git tag -a "$TAG" -m "$PKG_NAME $VERSION"
echo "==> created tag $TAG"

# --access public is required: npm defaults a SCOPED package to restricted,
# and a restricted package breaks `npm install` for everyone outside the org,
# including every machine that scaffolds a project with a released forge.
echo "==> npm publish"
if ! ( cd "$PKG_DIR" && npm publish --access public ); then
  echo >&2
  echo "error: publish FAILED. The tag $TAG exists locally and has NOT been pushed." >&2
  echo "hint: fix the cause, then 'git tag -d $TAG' and re-run. Do NOT push a tag" >&2
  echo "      whose version was never published — that is the drift this guards." >&2
  exit 1
fi

# `npm publish` exiting 0 means the registry ACCEPTED the upload, not that the
# version is fetchable. Those came apart for real (see the script's header),
# and a tag that exists while the registry serves nothing is invisible until a
# scaffolded project fails to install days later.
echo "==> confirming the registry serves it"
"$REPO_ROOT/scripts/verify-npm-published.sh" \
  --name "$PKG_NAME" --version "$BARE" --publish-reported-success

git push origin "$TAG"
echo
echo "Published $PKG_NAME@$BARE and pushed $TAG."
