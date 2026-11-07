#!/usr/bin/env bash
# Hermetic self-test of scripts/homebrew-watch.sh: the decision (current,
# within grace, overdue, missing formula, API failure) and the issue lifecycle
# (opened once, one comment per further release, closed on recovery). A fake gh
# serves the release, the tap's formula and one issue from files and logs every
# write; the clock is HOMEBREW_WATCH_NOW. No network.
set -euo pipefail
src="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/homebrew-watch.sh"
fx="$(mktemp -d)"
trap 'rm -rf "$fx"' EXIT
mkdir -p "$fx/bin"
cat > "$fx/bin/gh" << 'GH'
#!/usr/bin/env bash
set -euo pipefail
jqexpr="."; body=""; comment=""; prev=""
for a in "$@"; do
  case "$prev" in --jq) jqexpr="$a" ;; --body) body="$a" ;; --comment) comment="$a" ;; esac
  prev="$a"
done
case "$1 $2" in
  "api repos/acme/forge/releases/latest")
    jq -n --arg t "$TAG" --arg p "$PUBLISHED" '{tag_name: $t, published_at: $p}' | jq -r "$jqexpr" ;;
  "api repos/acme/tap/contents/Formula/forge.rb")
    case "${FORMULA_STATUS:-200}" in
      200) jq -n --arg c "$(printf '%s' "$FORMULA" | base64)" '{content: $c}' | jq -r "$jqexpr" ;;
      *) echo "gh: Request failed (HTTP $FORMULA_STATUS)" >&2; exit 1 ;;
    esac ;;
  "issue list")
    # A decoy whose title only contains ours must never be picked.
    { echo '{"number": 7, "title": "Re: Homebrew formula is behind the latest forge release"}'
      if [ -e "$FX/open" ]; then jq -n --argjson n "$(cat "$FX/open")" --arg t "$TITLE" '{number: $n, title: $t}'; fi
    } | jq -s . | jq -r "$jqexpr" ;;
  "issue create")
    echo 42 > "$FX/open"; printf '%s' "$body" > "$FX/body"; : > "$FX/comments"
    echo "create" >> "$FX/log" ;;
  "issue view")
    [ "$3" = "$(cat "$FX/open")" ] || { echo "view of unknown issue $3" >&2; exit 91; }
    jq -n --rawfile b "$FX/body" --rawfile c "$FX/comments" \
      '{body: $b, comments: ($c | split("\u0000") | map(select(. != "")) | map({body: .}))}' | jq -r "$jqexpr" ;;
  "issue comment")
    printf '%s\0' "$body" >> "$FX/comments"; echo "comment $3" >> "$FX/log" ;;
  "issue close")
    rm -f "$FX/open"; echo "close $3 $comment" >> "$FX/log" ;;
  *) echo "unexpected gh: $*" >&2; exit 90 ;;
esac
GH
chmod +x "$fx/bin/gh"
export PATH="$fx/bin:$PATH" FX="$fx" GITHUB_REPOSITORY=acme/forge HOMEBREW_TAP_REPO=acme/tap \
  GRACE_HOURS=4 TITLE="Homebrew formula is behind the latest forge release"
fail() { echo "FAIL: $*" >&2; exit 1; }

formula() { # formula <version>: the tap's Formula/forge.rb for that release
  local v
  for v in darwin_arm64 linux_amd64; do
    printf '      url "https://github.com/acme/forge/releases/download/v%s/forge_%s.tar.gz"\n      sha256 "00"\n' "$1" "$v"
  done
}
reset() { rm -f "$fx/open" "$fx/body" "$fx/comments" "$fx/log"; touch "$fx/log"; unset FORMULA_STATUS; }
release() { export TAG="$1" PUBLISHED="$2"; }
installs() { FORMULA="$(formula "$1")"; export FORMULA; }
at() { HOMEBREW_WATCH_NOW="$(date -u -d "$1" +%s)"; export HOMEBREW_WATCH_NOW; }

# watch <want exit>: runs the script; its output is in $fx/out.
watch() {
  local rc=0
  bash "$src" > "$fx/out" 2>&1 || rc=$?
  [ "$rc" = "$1" ] || { cat "$fx/out" >&2; fail "$label: exit $rc, want $1"; }
}
writes() { # writes <want log>: the issue writes since the last reset
  local got
  got="$(cut -d' ' -f1,2 "$fx/log" | paste -sd'|' -)"
  [ "$got" = "$1" ] || fail "$label: issue writes '$got', want '$1'"
}

label="current, no issue"
reset; release v1.2.0 2026-01-01T00:00:00Z; installs 1.2.0; at 2026-01-02T00:00:00Z
watch 0; writes ""

label="within grace"
reset; release v1.3.0 2026-01-02T00:00:00Z; installs 1.2.0; at 2026-01-02T03:59:00Z
watch 0; writes ""
grep -q 'within the 4h' "$fx/out" || fail "$label: no notice"

label="overdue, no issue: opens one"
at 2026-01-02T05:00:00Z
watch 1; writes "create"
grep -qF 'installs 1.2.0, but v1.3.0 was released 5h ago' "$fx/body" || fail "$label: body $(cat "$fx/body")"
grep -qF -- '-f version=1.3.0' "$fx/body" || fail "$label: body names no retry"

label="overdue, release already reported: no duplicate"
at 2026-01-02T07:00:00Z
watch 1; writes "create"

label="further release: one comment"
release v1.3.1 2026-01-02T08:00:00Z; at 2026-01-02T13:00:00Z
watch 1; writes "create|comment 42"
tr '\0' '\n' < "$fx/comments" | grep -qF 'but v1.3.1 was released 5h ago' || fail "$label: comment"

label="further release, re-run: still one comment"
at 2026-01-02T15:00:00Z
watch 1; writes "create|comment 42"

label="recovered: closes the issue"
installs 1.3.1
watch 0; writes "create|comment 42|close 42"
grep -qF 'Caught up: the tap installs forge 1.3.1' "$fx/log" || fail "$label: close comment"

label="recovered, re-run: nothing left to close"
watch 0; writes "create|comment 42|close 42"

label="missing formula, overdue: opens the issue"
reset; release v1.3.1 2026-01-02T08:00:00Z; export FORMULA_STATUS=404; at 2026-01-02T13:00:00Z
watch 1; writes "create"
grep -qF 'installs nothing, but v1.3.1 was released' "$fx/body" || fail "$label: body $(cat "$fx/body")"

label="missing formula, within grace"
reset; export FORMULA_STATUS=404; at 2026-01-02T09:00:00Z
watch 0; writes ""

label="other API failure: touches no issue"
reset; export FORMULA_STATUS=500; at 2026-01-02T13:00:00Z
watch 2; writes ""

echo "ok: homebrew-watch.sh"
