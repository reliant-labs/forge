#!/usr/bin/env bash
# Run one shard of the pull-request test tier: -short, no -race.
#
#   scripts/ci-test-shard.sh pkgs I N          package shard I of N (0-based)
#   scripts/ci-test-shard.sh split PKG I N     chunk I of N of PKG's tests
#
# ci.yml's `test-short` matrix calls this once per shard. The partition itself
# lives in scripts/ci-test-packages.sh (`short I N`, `split`); this script only
# runs a shard, reports it, and proves it ran what it was assigned.
#
# PKGS. `go test -short` over the shard's packages: the packages run side by
# side (-p = the runner's cores), vet runs, and an unchanged package whose
# inputs main's build cache already holds replays its result. The -short tier
# is held cache-sound (reliant.md, "Testing tiers"), so that replay is correct.
#
# SPLIT. One test binary, built once with `go test -c`, listed with
# -test.list, and run under test2json with the chunk's tests as an anchored
# -test.run alternation: the same mechanics e2e-suite.yml's shards use. The
# chunk is every N-th name of the sorted list, so the chunks partition the
# list by construction; the run must then REPORT exactly as many top-level
# tests as it was assigned, or a regex that silently matched nothing (or a
# test that vanished between list and run) would shrink coverage behind a
# green check.
#
# OUTPUT. The `go test -json` stream goes to $CI_TEST_JSON (default
# test-shard.json) so it can be uploaded; the console gets the package lines
# (ok/FAIL), build errors, and the full output of every failed test — the
# same picture plain `go test` prints, without the -v noise -json implies.
# $GITHUB_STEP_SUMMARY gets the counts and the shard's slowest tests, which is
# where a test that crept past the ~2s -short budget shows up first.
set -euo pipefail

json=${CI_TEST_JSON:-test-shard.json}
summary=${GITHUB_STEP_SUMMARY:-/dev/null}
# Per test BINARY. A hang detector, not a speed bound: the job's own
# timeout-minutes is the budget.
timeout=${CI_TEST_TIMEOUT:-10m}

usage() {
  echo "usage: $0 pkgs I N | split PKG I N" >&2
  exit 2
}

# report FILE TITLE — the console picture and the step summary.
report() {
  local f=$1 title=$2
  # Build failures (Go 1.24+ reports them as build-output), then package-level
  # lines: ok/FAIL/panics outside any test.
  jq -j 'select(.Action == "build-output" or (.Action == "output" and .Test == null)) | .Output' "$f"
  local failed
  failed=$(jq -c 'select(.Action == "fail" and .Test != null) | [.Package, .Test]' "$f" | jq -sc 'unique')
  if [[ "$failed" != "[]" ]]; then
    echo
    echo "::group::output of failed tests"
    jq -j --argjson failed "$failed" \
      'select(.Action == "output" and .Test != null) | select([.Package, .Test] as $k | $failed | index([$k])) | .Output' "$f"
    echo "::endgroup::"
    jq -r '.[] | "::error::FAIL \(.[0] | sub("^github.com/reliant-labs/forge/"; "")) \(.[1])"' <<<"$failed"
  fi
  local top='select((.Action == "pass" or .Action == "fail" or .Action == "skip") and .Test != null and (.Test | contains("/") | not))'
  {
    echo "### $title"
    echo
    echo "| passed | failed | skipped |"
    echo "| --- | --- | --- |"
    printf '| %s | %s | %s |\n' \
      "$(jq -r "$top | select(.Action == \"pass\") | .Test" "$f" | grep -c . || true)" \
      "$(jq -r "$top | select(.Action == \"fail\") | .Test" "$f" | grep -c . || true)" \
      "$(jq -r "$top | select(.Action == \"skip\") | .Test" "$f" | grep -c . || true)"
    echo
    echo "<details><summary>slowest tests</summary>"
    echo
    echo "| seconds | package | test |"
    echo "| ---: | --- | --- |"
    jq -r "$top | select(.Action != \"skip\") | [.Elapsed, (.Package | sub(\"^github.com/reliant-labs/forge/\"; \"\")), .Test] | @tsv" "$f" |
      sort -rn | awk -F'\t' 'NR <= 15 {printf "| %.2f | %s | `%s` |\n", $1, $2, $3}'
    echo
    echo "</details>"
    echo
  } >>"$summary"
}

case "${1:-}" in
  pkgs)
    [[ $# -eq 3 ]] || usage
    pkgs=()
    while IFS= read -r p; do pkgs+=("$p"); done < <(scripts/ci-test-packages.sh short "$2" "$3")
    if [[ ${#pkgs[@]} -eq 0 ]]; then
      echo "::error::package shard $2 of $3 is empty — a shard with nothing to run passes vacuously; lower the shard count"
      exit 1
    fi
    echo "::group::package shard $2 of $3 — ${#pkgs[@]} packages"
    printf '%s\n' "${pkgs[@]}"
    echo "::endgroup::"
    status=0
    go test -short -json -timeout "$timeout" "${pkgs[@]}" >"$json" || status=$?
    report "$json" "packages $(($2 + 1))/$3 — ${#pkgs[@]} packages"
    exit $status
    ;;
  split)
    [[ $# -eq 4 ]] || usage
    pkg=$2 i=$3 n=$4
    if ! scripts/ci-test-packages.sh split | grep -qxF "$pkg"; then
      echo "::error::$pkg is not a split package (scripts/ci-test-packages.sh split): its tests would run in a package shard too"
      exit 1
    fi
    work=$(mktemp -d)
    bin="$work/split.test"
    go test -c -o "$bin" "$pkg"
    dir=$(go list -f '{{.Dir}}' "$pkg")
    # Benchmarks are listed but never run without -test.bench, so they could
    # never be reported; everything else -test.list prints runs under -run.
    (cd "$dir" && "$bin" -test.list '.*') | { grep -E '^(Test|Example|Fuzz)' || true; } | sort -u >"$work/all.txt"
    total=$(wc -l <"$work/all.txt" | tr -d ' ')
    awk -v i="$i" -v n="$n" '(NR - 1) % n == i' "$work/all.txt" >"$work/mine.txt"
    mine=$(wc -l <"$work/mine.txt" | tr -d ' ')
    if [[ "$mine" -eq 0 ]]; then
      echo "::error::chunk $i of $n of $pkg was assigned 0 of $total tests"
      exit 1
    fi
    run="^($(paste -sd'|' "$work/mine.txt"))$"
    # Linux caps ONE argument at 128 KiB (MAX_ARG_STRLEN). Fail with the fix
    # rather than with an exec error nobody can read.
    if [[ ${#run} -gt 120000 ]]; then
      echo "::error::the -test.run pattern for chunk $i of $n of $pkg is ${#run} bytes, near Linux's 128 KiB argument limit: raise its chunk count in ci.yml"
      exit 1
    fi
    echo "::group::$pkg chunk $i of $n — $mine of $total tests"
    cat "$work/mine.txt"
    echo "::endgroup::"
    status=0
    (cd "$dir" && go tool test2json -t -p "$pkg" "$bin" -test.v=test2json -test.paniconexit0 \
      -test.short -test.timeout "$timeout" -test.run "$run") >"$json" || status=$?
    report "$json" "${pkg#github.com/reliant-labs/forge/} $((i + 1))/$n — $mine of $total tests"
    ran=$(jq -r 'select((.Action == "pass" or .Action == "fail" or .Action == "skip") and .Test != null and (.Test | contains("/") | not)) | .Test' "$json" | sort -u | wc -l | tr -d ' ')
    echo "assigned=$mine ran=$ran"
    if [[ "$ran" -ne "$mine" ]]; then
      echo "::error::chunk $i of $n of $pkg was assigned $mine tests but reported $ran: coverage silently shrank"
      comm -23 "$work/mine.txt" <(jq -r 'select((.Action == "pass" or .Action == "fail" or .Action == "skip") and .Test != null and (.Test | contains("/") | not)) | .Test' "$json" | sort -u) |
        sed 's/^/  never reported: /'
      [[ $status -ne 0 ]] || status=1
    fi
    exit $status
    ;;
  *)
    usage
    ;;
esac
