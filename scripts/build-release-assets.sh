#!/usr/bin/env bash
# build-release-assets.sh — cross-build the forge CLI for every release target
# and write the archives plus checksums.txt that install.sh, install.ps1, the npm
# shim and the Homebrew formula download.
#
#   scripts/build-release-assets.sh vX.Y.Z <outdir>
#
# Output, in <outdir>:
#   forge_<os>_<arch>.tar.gz   one per target; holds `forge` (`forge.exe` on
#                              windows), LICENSE and NOTICE at the archive root
#   checksums.txt              `<sha256>  <file>` for every archive (sha256sum format)
#
# The names carry no version on purpose: GitHub's /releases/latest/download/<name>
# then resolves without an API call, and every installer derives a name from the
# OS and CPU alone. install.sh, install.ps1, npm/ and the Homebrew formula all
# depend on this naming — change them together.
#
# Windows ships a .tar.gz as well (not a .zip): one archive format means one
# extractor in the npm shim, and tar.exe has shipped with Windows 10 since 1803.
#
# forge is cgo-free (README, "Install"), so every target cross-compiles from any
# host with a plain Go toolchain.
#
# Environment:
#   FORGE_RELEASE_TARGETS  space-separated GOOS/GOARCH list to build
#                          (default: the six below). Tests use it to build one.
#   FORGE_RELEASE_COMMIT   commit to stamp (default: git rev-parse HEAD)
#   FORGE_RELEASE_DATE     build date to stamp (default: today, UTC)
set -euo pipefail

usage() {
  echo "usage: $0 vX.Y.Z <outdir>" >&2
  exit 2
}

[ $# -eq 2 ] || usage
VERSION="$1"
OUT="$2"
if ! echo "$VERSION" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
  echo "error: version must look like vX.Y.Z (got: $VERSION)" >&2
  exit 1
fi

TARGETS="${FORGE_RELEASE_TARGETS:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64}"
COMMIT="${FORGE_RELEASE_COMMIT:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}"
DATE="${FORGE_RELEASE_DATE:-$(date -u +%Y-%m-%d)}"

# Build the checkout the caller is in (release-forge.sh --repo cd's into its
# target first), not the checkout this script lives in.
cd "$(git rev-parse --show-toplevel)"

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"
rm -f "$OUT"/forge_*.tar.gz "$OUT/checksums.txt"
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

# macOS tar would add AppleDouble ._* members for files with extended attributes.
export COPYFILE_DISABLE=1

: >"$OUT/checksums.txt"
for target in $TARGETS; do
  goos="${target%/*}"
  goarch="${target#*/}"
  bin="forge"
  [ "$goos" = "windows" ] && bin="forge.exe"
  name="forge_${goos}_${goarch}.tar.gz"

  echo "→ building $name"
  dir="$STAGE/$goos-$goarch"
  mkdir -p "$dir"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath \
    -ldflags "-s -w -X main.Version=$VERSION -X main.GitCommit=$COMMIT -X main.BuildDate=$DATE" \
    -o "$dir/$bin" ./cmd/forge
  cp LICENSE NOTICE "$dir/"
  chmod 755 "$dir/$bin"
  tar -C "$dir" -czf "$OUT/$name" "$bin" LICENSE NOTICE
  printf '%s  %s\n' "$(sha256_of "$OUT/$name")" "$name" >>"$OUT/checksums.txt"
done

echo "→ wrote $(wc -l <"$OUT/checksums.txt" | tr -d ' ') archives and checksums.txt to $OUT"
