#!/usr/bin/env bash
# ci-tools.sh — the pinned kcl and pre-commit the merge tier runs, linked into
# a checkout-local bin that scripts/ci-premerge.sh (`task ci`) puts first on
# PATH. A machine needs neither installed: on a host without them the gate's
# validate-kcl and pre-commit steps used to SKIP, and a SKIP fails the gate.
#
#   scripts/ci-tools.sh [BIN]      BIN defaults to .forge/tools/bin (gitignored)
#
# THE PINS, each read from the one place forge already declares it:
#   kcl         .github/actions/install-release-tool/action.yml's kcl line (tag,
#               asset and sha256), which TestReleaseToolPinsMatchForge holds to
#               templates.KCLVersion, the KCL forge embeds. It pins linux-amd64
#               only; elsewhere a kcl already on PATH at the pinned version is
#               used, and none means validate-kcl SKIPs, as before.
#   pre-commit  .pre-commit-config.yaml's minimum_pre_commit_version, which
#               pre-commit itself enforces on every run, CI's included.
#
# THE CACHE. Each tool is installed once per machine into a shared,
# content-addressed root, ${XDG_CACHE_HOME:-~/.cache}/reliant-tools-v2
# (FORGE_CI_TOOLS_CACHE moves it), the same root and entry layout as
# control-plane's scripts/lib/tool-cache.sh: an entry <tool>@<version>-<os>-<arch>
# is ready exactly when its .complete marker exists, and is never written
# again. So a kcl control-plane installed is linked, not fetched, and a
# control-plane python-tools entry at the same pre-commit and Python version is
# reused too. A cached run only relinks: well under a second.
#
# Installs of one entry hold flock(1) on <root>/.locks/<key>.lock, so concurrent
# gates install it once; kcl builds in <root>/.staging and is renamed into
# place, and the pre-commit venv (whose scripts embed their absolute path) is
# built at its final path with .complete written last. Without flock the root
# is .forge/tools/cache, this checkout's own.
#
# Exit 0 when every tool this platform pins is linked; non-zero when an install
# failed (a download, a checksum, pip), with the reason on stderr.
set -uo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd) || exit 2
bin=${1:-$repo/.forge/tools/bin}
mkdir -p "$bin" || exit 1

if [[ -n "${FORGE_CI_TOOLS_CACHE:-}" ]]; then
  cache=$FORGE_CI_TOOLS_CACHE
elif [[ ${XDG_CACHE_HOME:-} == /* ]]; then
  cache=$XDG_CACHE_HOME/reliant-tools-v2
else
  cache=$HOME/.cache/reliant-tools-v2
fi
locking=1
if ! command -v flock >/dev/null 2>&1; then
  echo "ci-tools: no flock(1): installing into this checkout's own $repo/.forge/tools/cache, not the shared $cache" >&2
  cache=$repo/.forge/tools/cache
  locking=""
fi

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) arch=$(uname -m) ;;
esac
platform=$os-$arch

# link TARGET NAME: point $bin/NAME at TARGET in one rename, so a step already
# running never finds it missing.
link() {
  ln -sfn "$1" "$bin/$2.link.$$" && mv -f "$bin/$2.link.$$" "$bin/$2"
}

# ensure KEY INSTALLER [in-place]: make $cache/KEY ready, running
# `INSTALLER <dir>` once across every concurrent caller.
ensure() {
  local key=$1 installer=$2 in_place=${3:-}
  [[ -f $cache/$key/.complete ]] && return 0
  mkdir -p "$cache/.locks" "$cache/.staging" || return 1
  if [[ -n $locking ]]; then
    (
      flock -x 9 || exit 1
      install_entry "$key" "$installer" "$in_place"
    ) 9>"$cache/.locks/$key.lock"
  else
    install_entry "$key" "$installer" "$in_place"
  fi
}

install_entry() {
  local key=$1 installer=$2 in_place=$3 dir=$cache/$1 stage
  # Another gate installed it while this one waited for the lock.
  [[ -f $dir/.complete ]] && return 0
  # No marker: a crashed install's remnant. We hold the lock; readers wait for
  # the marker.
  if [[ -e $dir ]]; then rm -rf "$dir" || return 1; fi
  echo "ci-tools: installing $key into $cache"
  if [[ -n $in_place ]]; then
    mkdir -p "$dir" || return 1
    if ! "$installer" "$dir"; then
      rm -rf "$dir"
      return 1
    fi
    : >"$dir/.complete"
    return
  fi
  stage=$(mktemp -d "$cache/.staging/$key.XXXXXX") || return 1
  if ! "$installer" "$stage"; then
    rm -rf "$stage"
    return 1
  fi
  # mktemp -d makes 0700; every user of a shared cache reads the entry.
  chmod 0755 "$stage" && : >"$stage/.complete" && mv "$stage" "$dir" && return 0
  rm -rf "$stage"
  return 1
}

failed=""

# ── kcl ─────────────────────────────────────────────────────────────────────
action=$repo/.github/actions/install-release-tool
# repo tag asset sha256 member, from the action's `kcl) set -- ...` line.
read -r kcl_repo kcl_tag kcl_asset kcl_sha kcl_member < <(
  sed -n 's|^ *kcl) *set -- \([^ ]*\) \([^ ]*\) \([^ ]*\) \([^ ]*\) \([^ ;]*\) *;;.*|\1 \2 \3 \4 \5|p' "$action/action.yml"
)
build_kcl() {
  local dl
  dl=$(mktemp "${TMPDIR:-/tmp}/ci-tools-kcl.XXXXXX") || return 1
  if ! "$action/fetch-release-asset.sh" "$kcl_repo" "$kcl_tag" "$kcl_asset" "$dl" "$kcl_sha" ||
    ! tar -xzf "$dl" -C "$1" "$kcl_member"; then
    rm -f "$dl"
    return 1
  fi
  rm -f "$dl"
  [[ -x $1/kcl ]] || { echo "ci-tools: $kcl_asset holds no executable kcl" >&2; return 1; }
}
if [[ -z ${kcl_tag:-} ]]; then
  echo "ci-tools: no kcl pin in $action/action.yml" >&2
  failed+=" kcl"
elif [[ $kcl_asset != *-"$platform".tar.gz ]]; then
  if command -v kcl >/dev/null 2>&1 && kcl --version 2>/dev/null | grep -q "${kcl_tag#v}"; then
    echo "ci-tools: kcl ${kcl_tag} is pinned for ${kcl_asset}; using the kcl ${kcl_tag} on PATH ($(command -v kcl))"
  else
    echo "ci-tools: kcl ${kcl_tag} is pinned for ${kcl_asset} only, and no kcl ${kcl_tag} is on PATH for $platform: validate-kcl will SKIP" >&2
  fi
else
  key=kcl@${kcl_tag#v}-$platform
  if ensure "$key" build_kcl && link "$cache/$key/kcl" kcl; then
    echo "ci-tools: kcl ${kcl_tag} -> $cache/$key/kcl"
  else
    echo "ci-tools: could not install kcl ${kcl_tag}" >&2
    failed+=" kcl"
  fi
fi

# ── pre-commit ──────────────────────────────────────────────────────────────
pc_version=$(sed -n "s/^minimum_pre_commit_version:[[:space:]]*[\"']\{0,1\}\([0-9][0-9.]*\)[\"']\{0,1\}[[:space:]]*$/\1/p" "$repo/.pre-commit-config.yaml" | head -1)
build_pre_commit() {
  python3 -m venv "$1/venv" &&
    "$1/venv/bin/pip" install --quiet --disable-pip-version-check "pre-commit==$pc_version"
}
if [[ -z $pc_version ]]; then
  echo "ci-tools: .pre-commit-config.yaml declares no minimum_pre_commit_version" >&2
  failed+=" pre-commit"
elif ! py=$(python3 -c 'import sys; print("%d.%d" % sys.version_info[:2])' 2>/dev/null); then
  echo "ci-tools: no python3 to install pre-commit $pc_version into: pre-commit will SKIP unless uvx is on PATH" >&2
else
  # A venv is bound to the interpreter that made it, hence the +py in the key.
  key=pre-commit@$pc_version+py$py-$platform
  entry=""
  if [[ -f $cache/$key/.complete ]]; then
    entry=$cache/$key
  else
    # control-plane's bundle (pre-commit + shellcheck-py) at the same pin.
    for e in "$cache/python-tools@$pc_version+"*"+py$py-$platform"; do
      if [[ -f $e/.complete && -x $e/venv/bin/pre-commit ]]; then
        entry=$e
        break
      fi
    done
  fi
  if [[ -z $entry ]] && ensure "$key" build_pre_commit in-place; then
    entry=$cache/$key
  fi
  if [[ -n $entry && -x $entry/venv/bin/pre-commit ]] && link "$entry/venv/bin/pre-commit" pre-commit; then
    echo "ci-tools: pre-commit $pc_version -> $entry/venv/bin/pre-commit"
  else
    echo "ci-tools: could not install pre-commit $pc_version" >&2
    failed+=" pre-commit"
  fi
fi

if [[ -n $failed ]]; then
  echo "ci-tools: NOT installed:$failed" >&2
  exit 1
fi
