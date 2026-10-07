#!/usr/bin/env bash
# Partition the module's packages between ci.yml's two Go test jobs.
#
#   scripts/ci-test-packages.sh unit     every package EXCEPT the guards
#   scripts/ci-test-packages.sh guards   exactly the guards
#   scripts/ci-test-packages.sh check    prove the two sets partition `go list ./...`
#
# WHY TWO JOBS. internal/tierguard renders four whole forge projects,
# internal/removalguard regex-scans every file in the repository, and
# internal/deadcodeguard type-checks the whole program — every dependency
# included — once per target OS. They are the slowest and the largest packages
# in the suite. Run inside `go test -race ./...` they co-resided with a hundred
# other test binaries on one 16 GB runner, and the Test job was OOM-killed ("The
# runner has received a shutdown signal", exit 143): roughly one run in three
# for tierguard and removalguard until #304 moved them here, and most runs for
# deadcodeguard once its scan grew to three OS loads (#469; ~23 GB RSS under
# -race). In their own job their peak never meets the rest of the suite's —
# bounded by the job boundary, not by the luck of go test's package scheduling.
#
# WHY THIS SCRIPT. A package list typed into two workflow steps can drift: add
# a guard to one list and forget the other, and it runs twice or — worse — not
# at all while both jobs stay green. So the list lives HERE, once, and `check`
# fails the job unless unit ∪ guards == `go list ./...` and unit ∩ guards == ∅.
# A build tag was rejected for the same reason e2e-suite.yml documents: tagged
# tests are invisible to golangci-lint and to every plain `go test ./...`,
# which is how 47 e2e tests once went unexecuted while CI stayed green. The
# guards stay ordinary tests; locally `go test ./...` still runs them.
set -euo pipefail

# The guard packages. Import paths, one per line.
guards=(
  github.com/reliant-labs/forge/internal/deadcodeguard
  github.com/reliant-labs/forge/internal/removalguard
  github.com/reliant-labs/forge/internal/tierguard
)

all() { go list ./...; }
guard_set() { printf '%s\n' "${guards[@]}"; }
unit_set() { all | grep -vxF -f <(guard_set); }

case "${1:-}" in
  unit) unit_set ;;
  guards) guard_set ;;
  check)
    all_pkgs=$(all | sort)
    unit_pkgs=$(unit_set | sort)
    guard_pkgs=$(guard_set | sort)
    status=0
    # Every guard must be a real package: a renamed or deleted guard would
    # otherwise leave the guards job testing nothing and passing.
    missing=$(comm -13 <(printf '%s\n' "$all_pkgs") <(printf '%s\n' "$guard_pkgs"))
    if [[ -n "$missing" ]]; then
      echo "::error::guard package(s) not in \`go list ./...\` — renamed or deleted? Update scripts/ci-test-packages.sh:"
      printf '  %s\n' $missing
      status=1
    fi
    overlap=$(comm -12 <(printf '%s\n' "$unit_pkgs") <(printf '%s\n' "$guard_pkgs"))
    if [[ -n "$overlap" ]]; then
      echo "::error::package(s) in BOTH jobs:"
      printf '  %s\n' $overlap
      status=1
    fi
    union=$(printf '%s\n%s\n' "$unit_pkgs" "$guard_pkgs" | sort -u)
    if [[ "$union" != "$all_pkgs" ]]; then
      echo "::error::the two jobs do not cover \`go list ./...\` exactly:"
      diff <(printf '%s\n' "$all_pkgs") <(printf '%s\n' "$union") || true
      status=1
    fi
    if [[ $status -eq 0 ]]; then
      echo "partition ok: $(printf '%s\n' "$unit_pkgs" | wc -l | tr -d ' ') unit + $(printf '%s\n' "$guard_pkgs" | wc -l | tr -d ' ') guard = $(printf '%s\n' "$all_pkgs" | wc -l | tr -d ' ') packages"
    fi
    exit $status
    ;;
  *)
    echo "usage: $0 unit|guards|check" >&2
    exit 2
    ;;
esac
