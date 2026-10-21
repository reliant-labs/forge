#!/usr/bin/env bash
# The main-red override: may `CI gate` pass a red run because every failure in
# it ALSO fails on main?
#
#   scripts/ci-main-red.sh       (from ci.yml's gate job, after a red result)
#
# Exits 0 when it accepts the run, 1 when it does not, saying why either way.
#
# WHY. `CI gate` is the main ruleset's required check and main merges through
# a merge queue, with no bypass for the account agents merge as. When main
# itself is red on a pull-request-tier check, every pull request inherits the
# red, the fix for one main red can still fail on another, and the queue
# cannot land either. This is the narrow, visible way through that: a
# maintainer labels the pull request `ci:main-red`, and the gate accepts the run
# only if the machine agrees that nothing in it is new.
#
# Accepted only when ALL of these hold:
#   - the event is a pull request or a merge-queue entry (never a push: a red
#     main is red);
#   - the pull request carries the `ci:main-red` label;
#   - it is not a Dependabot pull request: those have no author to come back
#     and fix what a red showed, so they are never merged over one;
#   - every failed job of this run is matched on main's latest completed push
#     run of ci.yml:
#       * a job whose log names failing tests (`--- FAIL: TestX`) is matched
#         when every one of those tests also failed in some job on main;
#       * a job whose log names none (lint, verify-generated, a build error)
#         is matched when main has a failed job of the same name that failed
#         at the same step(s);
#   - no job was cancelled (a cancelled job proves nothing either way).
#
# In a merge-queue entry the label is read from the pull request the entry's
# ref names (gh-readonly-queue/main/pr-<n>-<sha>), the last one in its group.
#
# Needs GH_TOKEN (actions: read, pull-requests: read), GITHUB_REPOSITORY,
# GITHUB_RUN_ID, GITHUB_EVENT_NAME and GITHUB_EVENT_PATH: all present in a
# workflow job.
set -euo pipefail
export LC_ALL=C # sort and comm must collate alike

: "${GITHUB_REPOSITORY:?}" "${GITHUB_RUN_ID:?}" "${GITHUB_EVENT_NAME:?}" "${GITHUB_EVENT_PATH:?}"
repo=$GITHUB_REPOSITORY
label=ci:main-red
summary=${GITHUB_STEP_SUMMARY:-/dev/null}

refuse() {
  echo "main-red override refused: $*"
  exit 1
}

case "$GITHUB_EVENT_NAME" in
  pull_request) pr=$(jq -r '.pull_request.number // empty' "$GITHUB_EVENT_PATH") ;;
  merge_group)
    ref=$(jq -r '.merge_group.head_ref // empty' "$GITHUB_EVENT_PATH")
    pr=$(sed -nE 's#.*/pr-([0-9]+)-[0-9a-f]+$#\1#p' <<<"$ref")
    ;;
  *) refuse "a $GITHUB_EVENT_NAME run is judged as it is" ;;
esac
[ -n "$pr" ] || refuse "could not tell which pull request this run is for"

prjson=$(gh api "repos/$repo/pulls/$pr")
if ! jq -e --arg l "$label" 'any(.labels[]?; .name == $l)' >/dev/null <<<"$prjson"; then
  refuse "#$pr has no $label label"
fi
if [ "$(jq -r .user.login <<<"$prjson")" = "dependabot[bot]" ]; then
  refuse "#$pr is a Dependabot pull request; fix what its checks show instead"
fi

# Top-level failing test names in a job's log. Timestamps and colour codes
# are stripped first; a subtest's line is indented, so only its parent counts.
# A log that cannot be read names no tests: on this run that sends the job to
# the stricter step match below, and on main it only shrinks what matches.
#
# go test colours nothing, but the runner's log does, and a gh new enough to
# refuse escape sequences needs --allow-escape-sequences to print them; an
# older gh has neither the refusal nor the flag, hence the fallback.
failed_tests() {
  local log path="repos/$repo/actions/jobs/$1/logs"
  log=$(gh api --allow-escape-sequences "$path" 2>/dev/null || gh api "$path" 2>/dev/null) || log=""
  sed -E 's/\x1b\[[0-9;]*m//g; s/^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:.]+Z //' <<<"$log" |
    sed -nE 's/^--- FAIL: ([^ ]+).*/\1/p' | sort -u
}

# The latest attempt of every job in a run.
run_jobs() {
  gh api --paginate "repos/$repo/actions/runs/$1/jobs?filter=latest&per_page=100" --jq '.jobs[]' | jq -s .
}

mine=$(run_jobs "$GITHUB_RUN_ID")
if jq -e 'any(.[]; .conclusion == "cancelled")' >/dev/null <<<"$mine"; then
  refuse "a job was cancelled: $(jq -r '[.[] | select(.conclusion == "cancelled") | .name] | join(", ")' <<<"$mine")"
fi
failed=$(jq -c '[.[] | select(.conclusion == "failure" and .name != "CI gate")]' <<<"$mine")
[ "$(jq length <<<"$failed")" -gt 0 ] || refuse "no failed job to match"

main_run=$(gh api "repos/$repo/actions/workflows/ci.yml/runs?branch=main&event=push&status=completed&per_page=1" --jq '.workflow_runs[0].id // empty')
[ -n "$main_run" ] || refuse "found no completed ci.yml run on main to compare with"
main_failed=$(run_jobs "$main_run" | jq -c '[.[] | select(.conclusion == "failure")]')
[ "$(jq length <<<"$main_failed")" -gt 0 ] || refuse "main's latest run ($main_run) is green: every failure here is this change's"

main_tests=$(for id in $(jq -r '.[].id' <<<"$main_failed"); do failed_tests "$id"; done | sort -u)

report="| job | matched on main run $main_run by |"$'\n'"| --- | --- |"
unmatched=""
while IFS= read -r job; do
  name=$(jq -r .name <<<"$job")
  tests=$(failed_tests "$(jq -r .id <<<"$job")")
  if [ -n "$tests" ]; then
    new=$(comm -23 <(echo "$tests") <(echo "$main_tests"))
    if [ -n "$new" ]; then
      unmatched="$unmatched"$'\n'"  $name: $(echo $new) do not fail on main"
      continue
    fi
    report="$report"$'\n'"| $name | tests: $(echo $tests) |"
    continue
  fi
  steps=$(jq -r '[.steps[]? | select(.conclusion == "failure") | .name] | sort | join(", ")' <<<"$job")
  main_steps=$(jq -r --arg n "$name" '[.[] | select(.name == $n) | .steps[]? | select(.conclusion == "failure") | .name] | sort | join(", ")' <<<"$main_failed")
  if [ -z "$main_steps" ]; then
    unmatched="$unmatched"$'\n'"  $name: failed here (no test names in its log), passed on main"
  elif [ "$steps" != "$main_steps" ]; then
    unmatched="$unmatched"$'\n'"  $name: failed at [$steps] here, at [$main_steps] on main"
  else
    report="$report"$'\n'"| $name | job and step: $steps |"
  fi
done < <(jq -c '.[]' <<<"$failed")

if [ -n "$unmatched" ]; then
  refuse "these failures are not main's:$unmatched"
fi

echo "main-red override accepted for #$pr: every failure also fails on main (run $main_run)."
echo "$report"
{
  echo "### Accepted over main's reds (\`$label\`)"
  echo
  echo "$report"
  echo
} >>"$summary"
