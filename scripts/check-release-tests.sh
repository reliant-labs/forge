#!/usr/bin/env bash
# Fail closed before publishing: only the newest full CI run for this exact
# source commit counts. A PR/merge-queue green did not run the full tests.
set -euo pipefail
[[ $# == 2 ]] || { echo "usage: $0 OWNER/REPO SHA" >&2; exit 2; }
repo=$1 sha=$2
gh=${FORGE_RELEASE_GH:-gh}
refusal() {
  echo "error: release blocked: full tests for $sha are not proven green ($*)" >&2
  echo "hint: wait for main's CI, or dispatch ci.yml at this commit; full Test, Test (internal/cli), and Test (-short whole tier, then guards) must all succeed." >&2
  exit 1
}
command -v "$gh" >/dev/null 2>&1 || refusal "gh is required"
# gh's jq filter is bundled: no extra jq dependency for release contributors.
run=$("$gh" run list --repo "$repo" --workflow ci.yml --commit "$sha" --limit 100 \
  --json databaseId,event,status,conclusion,headSha \
  --jq '[.[] | select(.event == "push" or .event == "workflow_dispatch")] | sort_by(.databaseId) | last | if . == null then "missing" else [.databaseId,.status,.conclusion,.headSha] | @tsv end') || refusal "cannot read CI runs"
read -r id status conclusion head <<<"$run"
# The run must have finished (not cancelled); its verdict is the three jobs
# below, so a red lint or Windows job elsewhere in the run does not block it.
[[ "$status" == completed && "$conclusion" != cancelled && "$head" == "$sha" ]] || refusal "$run"
# CI gate may waive known main failures; the actual full test jobs cannot be
# skipped, cancelled, failed, or waived for a release, even if the gate is green.
verdict=$("$gh" run view "$id" --repo "$repo" --json jobs \
  --jq '[.jobs[] | select(.name == "Test" or .name == "Test (internal/cli)" or .name == "Test (-short whole tier, then guards)")] | if length == 3 and all(.[]; .status == "completed" and .conclusion == "success") then "success" else "failure" end') || refusal "cannot read full test jobs"
[[ "$verdict" == success ]] || refusal "full test jobs missing or not successful"
echo "Full release tests passed at $sha (CI run $id)."
