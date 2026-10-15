#!/usr/bin/env bash
# Run `go test <flags>` over the packages a change can reach.
#
#   scripts/go-test-affected.sh <go test flags...>
#
# Package selection is `forge ci affected` (cmd/affected: the import graph plus
# forge.yaml ci.gate.affected_rules); the flags are the caller's — the Taskfile
# owns -race / -short / -count / -timeout. An empty selection runs nothing and
# says so; an undecidable one selects ./... (the whole module).
#
# The repo-wide guards (the set scripts/ci-test-packages.sh calls `guards`)
# run in their own `go test -p 1` WITHOUT -race, exactly as ci.yml's test-guards
# job does: they scan every file or load the whole program, have no concurrency
# worth detecting, and -race costs removalguard ~100x (10s -> ~980s).
set -euo pipefail

pkgs=$(go run ./cmd/affected --packages)
if [[ -z "$pkgs" ]]; then
  echo "affected: no test can observe this change — nothing to run (task test:full runs everything)"
  exit 0
fi

if [[ "$pkgs" == "./..." ]]; then
  all=$(go list ./...)
else
  all=$(printf '%s\n' $pkgs)
fi
guards=$(scripts/ci-test-packages.sh guards)
guard_sel=$(grep -xF -f <(printf '%s\n' "$guards") <<<"$all" || true)
other_sel=$(grep -vxF -f <(printf '%s\n' "$guards") <<<"$all" || true)

flags=("$@")
noracefl=()
for f in "${flags[@]}"; do [[ "$f" == "-race" ]] || noracefl+=("$f"); done

status=0
if [[ -n "$other_sel" ]]; then
  go test "${flags[@]}" $other_sel || status=$?
fi
if [[ -n "$guard_sel" ]]; then
  go test -p 1 "${noracefl[@]}" $guard_sel || status=$?
fi
exit $status
