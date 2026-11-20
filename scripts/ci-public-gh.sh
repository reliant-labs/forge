#!/usr/bin/env bash
# A deliberately narrow, anonymous `gh api` adapter for the public Forge PR
# compatibility checks. Local callers can still use gh with their own auth.
set -euo pipefail

[[ ${1:-} == api ]] || exit 2
shift
paginate=false
if [[ ${1:-} == --paginate ]]; then paginate=true; shift; fi
endpoint=${1:-}
shift || exit 2
filter='.'
if [[ $# == 2 && $1 == --jq ]]; then filter=$2; shift 2; fi
[[ $# == 0 ]] || exit 2
case "$endpoint" in
  repos/reliant-labs/forge/*) ;;
  *) echo 'ci-public-gh: public Forge PR metadata only' >&2; exit 2 ;;
esac
[[ $endpoint =~ ^repos/reliant-labs/forge/(pulls/[1-9][0-9]*|issues/[1-9][0-9]*/(labels|events))$ ]] || exit 2
if $paginate; then [[ $endpoint == */events ]] || exit 2; fi

# No token lookup, user curl configuration, or redirects. API/parse failures
# must propagate, including a later page: partial history cannot approve a PR.
for ((page=1; page<=30; page++)); do
  response=$(curl -q --fail --silent --show-error --connect-timeout 10 --max-time 25 \
    -H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2022-11-28' \
    "https://api.github.com/$endpoint?per_page=100&page=$page")
  if [[ $endpoint == */labels || $endpoint == */events ]]; then
    jq -e 'type == "array"' <<<"$response" >/dev/null
  else
    jq -e 'type == "object" and (.labels | type == "array") and (.body == null or (.body | type == "string"))' <<<"$response" >/dev/null
  fi
  jq -cr "$filter" <<<"$response"
  if ! $paginate || [[ $(jq length <<<"$response") -lt 100 ]]; then exit 0; fi
done
echo 'ci-public-gh: too many label-event pages; cannot establish approval' >&2
exit 1
