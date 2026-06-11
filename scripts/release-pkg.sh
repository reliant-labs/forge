#!/usr/bin/env bash
# release-pkg.sh — tag a release of the github.com/reliant-labs/forge/pkg
# Go submodule.
#
# Go resolves submodule versions from tags of the form `pkg/vX.Y.Z`
# (directory-prefixed tags — the standard multi-module repo convention).
# This script is the only sanctioned way to mint one: it validates the
# version shape, the working tree, and — most importantly — that the pkg
# module builds STANDALONE (GOWORK=off, no sibling replace, no go.work),
# which is exactly how downstream `go mod download` will consume it.
#
# Usage:
#   scripts/release-pkg.sh [--dry-run] [--repo <dir>] vX.Y.Z
#
#   --dry-run   run every validation, print the actions, create no tag.
#   --repo DIR  operate on DIR instead of the enclosing git repo
#               (used by the test suite against fixture repos).
#
# After a real (non-dry-run) invocation ONE MANUAL STEP remains:
#
#   git push origin pkg/vX.Y.Z
#
# and then release forge binaries stamped against it:
#
#   go build -trimpath -ldflags "-X main.PkgVersion=vX.Y.Z" -o bin/forge ./cmd/forge
#
# (see docs/pkg-versioning.md for the full dev-vs-release model).
set -euo pipefail

DRY_RUN=0
REPO_ROOT=""
VERSION=""

usage() {
  echo "usage: $0 [--dry-run] [--repo <dir>] vX.Y.Z" >&2
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    --repo)    [ $# -ge 2 ] || usage; REPO_ROOT="$2"; shift 2 ;;
    -h|--help) usage ;;
    -*)        echo "error: unknown flag $1" >&2; usage ;;
    *)
      [ -z "$VERSION" ] || usage
      VERSION="$1"; shift ;;
  esac
done

[ -n "$VERSION" ] || usage

# ── 1. Version shape ────────────────────────────────────────────────
# Canonical semver with optional prerelease (v1.2.3, v1.2.3-rc.1).
# Reject the tag-prefixed form early — users habitually paste
# `pkg/v1.2.3` back in.
if ! echo "$VERSION" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
  echo "error: version must look like vX.Y.Z (got: $VERSION)" >&2
  echo "hint: pass the bare version; the script adds the pkg/ tag prefix itself." >&2
  exit 1
fi
TAG="pkg/$VERSION"

# ── 2. Repo / module layout ─────────────────────────────────────────
if [ -z "$REPO_ROOT" ]; then
  REPO_ROOT="$(git rev-parse --show-toplevel)"
fi
cd "$REPO_ROOT"

if [ ! -f pkg/go.mod ]; then
  echo "error: $REPO_ROOT/pkg/go.mod not found — not a forge repo checkout?" >&2
  exit 1
fi
if ! grep -q '^module github.com/reliant-labs/forge/pkg$' pkg/go.mod; then
  echo "error: pkg/go.mod does not declare module github.com/reliant-labs/forge/pkg" >&2
  exit 1
fi

# ── 3. Working tree must be clean (tags must point at committed code) ─
if [ -n "$(git status --porcelain -- pkg/)" ]; then
  echo "error: pkg/ has uncommitted changes; commit or stash before tagging" >&2
  exit 1
fi

# ── 4. Tag must not already exist ───────────────────────────────────
if git rev-parse -q --verify "refs/tags/$TAG" >/dev/null; then
  echo "error: tag $TAG already exists (versions are immutable; bump instead)" >&2
  exit 1
fi

# ── 5. Standalone build validation ──────────────────────────────────
# GOWORK=off detaches the repo's go.work (which stitches pkg to the
# main module). -mod=readonly is load-bearing: the validation must
# FAIL when pkg/go.mod is incomplete standalone, never silently edit
# it (a -mod=mod build here once reclassified a dependency and dirtied
# the tree AFTER the clean-tree check). This is the consumer's view of
# the module: if it doesn't compile here, the tag would publish a
# broken version.
echo "→ validating pkg module builds standalone (GOWORK=off go build ./...)"
( cd pkg && GOWORK=off GOFLAGS=-mod=readonly go build ./... )
echo "→ validating pkg module vets standalone (GOWORK=off go vet ./...)"
( cd pkg && GOWORK=off GOFLAGS=-mod=readonly go vet ./... )

# ── 6. Tag (or describe what would happen) ──────────────────────────
HEAD_SHA="$(git rev-parse --short HEAD)"
if [ "$DRY_RUN" = "1" ]; then
  echo "DRY RUN: all validations passed."
  echo "DRY RUN: would create annotated tag $TAG at $HEAD_SHA"
  echo "DRY RUN: then push with: git push origin $TAG"
  exit 0
fi

git tag -a "$TAG" -m "forge/pkg $VERSION"
echo "✅ created tag $TAG at $HEAD_SHA"
echo ""
echo "MANUAL STEP REMAINING — publish the tag:"
echo "  git push origin $TAG"
echo ""
echo "Then build release forge binaries stamped against it:"
echo "  go build -trimpath -ldflags \"-X main.PkgVersion=$VERSION\" -o bin/forge ./cmd/forge"
