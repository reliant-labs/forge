#!/usr/bin/env bash
# Checks that `brew install reliant-labs/reliant/forge` hands out forge's latest
# release, and keeps one forge issue in step with the answer
# (.github/workflows/homebrew-watch.yml runs it; that file has the why).
#
#   tap current             close the open issue, if any; exit 0
#   behind < GRACE_HOURS    nothing (the tap is still publishing it); exit 0
#   behind >= GRACE_HOURS   open the issue, or comment on it once per release
#                           it falls further behind; exit 1
#
# A tap with no Formula/forge.rb (HTTP 404) installs nothing: it is behind like
# any other, so it raises the same issue. Any other API failure exits non-zero
# before touching an issue. scripts/test-homebrew-watch.sh is its hermetic test.
set -euo pipefail
repo=${GITHUB_REPOSITORY:?}
tap=${HOMEBREW_TAP_REPO:-reliant-labs/homebrew-reliant}
grace=${GRACE_HOURS:-4}
title=${TITLE:-Homebrew formula is behind the latest forge release}
now=${HOMEBREW_WATCH_NOW:-$(date -u +%s)}
run_url="${GITHUB_SERVER_URL:-https://github.com}/$repo/actions/runs/${GITHUB_RUN_ID:-local}"

release="$(gh api "repos/$repo/releases/latest" --jq '"\(.tag_name) \(.published_at)"')"
read -r tag published <<< "$release"
[ -n "${tag:-}" ] && [ -n "${published:-}" ] || {
  echo "::error::cannot read the latest release of $repo: '$release'" >&2
  exit 2
}
latest="${tag#v}"

# The formula has no version stanza: Homebrew scans it from the release URLs,
# which all name one release.
err="$(mktemp)"
trap 'rm -f "$err"' EXIT
if content="$(gh api "repos/$tap/contents/Formula/forge.rb" --jq .content 2> "$err")"; then
  have="$(printf '%s\n' "$content" | base64 -d |
    sed -n 's|.*/releases/download/v\([0-9][0-9.]*\)/forge_.*|\1|p' | sort -u | paste -sd, -)"
elif grep -q 'HTTP 404' "$err"; then
  echo "::warning::$tap has no Formula/forge.rb"
  have=""
else
  cat "$err" >&2
  echo "::error::cannot read $tap's Formula/forge.rb" >&2
  exit 2
fi
echo "tap installs forge ${have:-nothing}; latest release $latest ($published)"

issue="$(gh issue list --repo "$repo" --state open --search "in:title \"$title\"" \
  --json number,title --jq ".[] | select(.title == \"$title\") | .number" | head -n1)"

if [ "$have" = "$latest" ]; then
  if [ -n "$issue" ]; then
    gh issue close "$issue" --repo "$repo" --comment "Caught up: the tap installs forge $have ($run_url)."
  fi
  exit 0
fi

age_h=$(((now - $(date -u -d "$published" +%s)) / 3600))
if [ "$age_h" -lt "$grace" ]; then
  echo "::notice::$tag was released ${age_h}h ago; within the ${grace}h the tap gets to publish it"
  exit 0
fi

msg="\`brew install reliant-labs/reliant/forge\` installs ${have:-nothing}, but $tag was released ${age_h}h ago.

The tap publishes every release itself ($tap, workflow \"Update forge formula\"; README, \"Automation\"). Its runs say why it has not: https://github.com/$tap/actions/workflows/update-forge-formula.yml

Retry: \`gh workflow run update-forge-formula.yml --repo $tap -f version=$latest\`. Found by $run_url."
if [ -z "$issue" ]; then
  gh issue create --repo "$repo" --title "$title" --body "$msg"
elif ! gh issue view "$issue" --repo "$repo" --json body,comments \
  --jq '([.body] + [.comments[].body]) | join("\n")' | grep -qF "but $tag was released"; then
  gh issue comment "$issue" --repo "$repo" --body "$msg"
fi
echo "::error::the Homebrew tap installs forge ${have:-nothing}; $tag was released ${age_h}h ago"
exit 1
