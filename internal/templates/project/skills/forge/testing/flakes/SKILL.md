---
name: testing/flakes
description: "Flaky tests from the recorded test runs — the states (suspected, flaky, quarantined, fixed, removed), the verdicts and why `host` and `correlated` failures never score, the flake rate as a ranking with its n, `forge test flakes` / `flakes gate` / `flakes confirm` / `quarantine add|remove|list|skip-regex`, the one-file-per-test quarantine format under ci/quarantine/, `tests.flakes.issues` and `quarantine: pr`, and the advisory `flake-check` gate step that `lint.rules: {flake-check: error}` makes blocking. Use when a test fails intermittently, when deciding what to do about it, or when wiring the flake gate."
applies: any
---

# Flaky tests

Every captured lane records each test's outcome per run (`testing/coverage`
covers capture). From those records forge works out which tests flake, what
state each is in, and what to do about it. It reports and suggests; it never
changes the verdict of a run, and it blocks nothing until a project opts in.

## What counts as a flake

A **confirmed** flake event is a failure the same tree also passed:

- **Same-tree split:** the test passed in one run and failed in another on the
  same tree hash (sharded jobs, a re-run, a local run plus CI).
- **Retry pass:** it failed, then passed within one run (`go test -count=N`
  attempts in the stream, or a `forge test flakes confirm` rerun).

A commit where the test failed in **every** run is a real break, not a flake,
and does not count.

**Verdicts.** Each failure gets one (`pkg/testreport.Verdict`):

| Verdict | Meaning | Scores? |
|---|---|---|
| `confirmed` | failed, then passed on the same tree | yes, the only one |
| `real` | failed again on rerun, or nothing shows it would pass: a break | no |
| `raced` | the output reports a data race: a bug, never retried | no |
| `inconclusive` | the rerun could not tell (skipped, its package failed, over the `--max` budget) | no |
| `unattributed` | no test owns the failure (a build failure, `TestMain`) | no |
| `host` | the output matches a host signature | no |
| `correlated` | one of a correlated group | no |

**`host` and `correlated` never score.** Before any scoring, a failure's
output is matched against host signatures: network and download errors,
`toomanyrequests`, ENOSPC and quota errors, `address already in use`, docker
CLI timeouts, and `panic: test timed out` on a loaded host. A match is
reported as host health (per signature), not as a flaky test. Separately,
three or more failures in one run that share a package, a harness (testsrv,
ClickHouse, pgtest) or a failure signature are grouped into **one infra
event**, each carrying the group's `infra_group`; the same co-failing set on a
later commit is the same event. A broken shared fixture therefore does not
turn forty tests into forty flakes.

## The states

A state counts confirmed events on **distinct commits**: several events on one
commit (a burst of re-runs) count once.

| State | Enters when | Leaves when |
|---|---|---|
| `suspected` | one confirmed event | 14 days with no event: `healthy` |
| `flaky` | confirmed events on 2 or more distinct commits within 30 days | the test's file changes, then 14 days pass with no event: `fixed` |
| `quarantined` | a quarantine entry for it exists on the default branch | the entry is removed |
| `removed` | a suspected or flaky test no longer appears in its lane's runs | n/a |

`suspected` is the actionable state for a project whose policy is to delete a
flaky test on its first flake: the issue and the quarantine follow from it,
not from `flaky`. That is a policy a project can choose, **not a rule forge
imposes**; a project that waits for `flaky` before acting uses the same data.

**The flake rate is a ranking, not a trigger.** It is the lower bound (Wilson,
95%) of commits-with-a-confirmed-flake over commits-with-runs in the last 30
days, shown **with its n**, and it orders the list; it never moves a test
between states. The n is the completed default-branch runs of the test's lane
in which the test ran (not skipped), excluding cancelled runs. At normal
volumes a threshold on it would never decide anything (the worst flake can be
5 events in 1,000 runs), which is why the states count distinct commits
instead.

## Reading it

```bash
forge test flakes                              # every non-healthy test, with the command that quarantines it
forge test flakes --state suspected --json     # forge.test.flakes/v1
forge test flakes --all                        # include healthy tests that have flake history
forge test flakes --issues --dry-run           # what it would file, comment on and close
```

It reads the local record, `.forge/tests/runs/`: the project's own lanes plus
the `flake-gate` and `confirm` lanes below. `--state` takes `suspected`,
`flaky`, `quarantined`, `removed`, `fixed` or `healthy`. Each test is
`go:<package>:<Test>`, and a subtest is recorded under its top-level test,
which is what `-run` and `-skip` address. The JSON also carries `host`: the
host-health signatures with their events and runs.

## The flake gate

```bash
forge test flakes gate --base origin/main                  # the changed tests, three times
forge test flakes gate --base HEAD^1 --full -- scripts/ci-go-test.sh
forge test flakes gate --cpu 1,2,4
```

It finds the top-level `Test` and `Fuzz` functions the diff from `--base` to
`HEAD` adds or edits and runs them in one invocation of the project's own `go
test`: `go test -json -short -count=3 -timeout 10m -run '^(TestA|TestB)$'
<packages>`. `--full` adds `-race`. `--cpu 1,2,4` runs each test once per
`GOMAXPROCS` value instead of `-count=3`, which Go's test cache keeps. A
command after `--` replaces `go test` (a wrapper such as a repo script); the
gate adds only its flags, the `-run` pattern and the packages. Tests under the
`e2e` or `integration` build tags are skipped. `--base` defaults to `$CI_BASE`,
else the merge base with `origin/<default branch>`.

The run is recorded as lane `flake-gate`, so its retry passes feed `forge test
flakes`.

| Exit | Meaning |
|---|---|
| 0 | every changed test passed every run |
| 1 | a changed test failed at least once |
| 2 | no changed test could be identified (none in the diff, or the diff could not be read) |

**As a CI step it is `flake-check`**, one of `forge ci gate`'s steps in the
`test` lane. For a project that has not opted in it **reports and never
fails**: the step passes with a warning. A project makes it blocking in
forge.yaml:

```yaml
lint:
  rules:
    flake-check: error
```

`forge test flakes gate --graded` exits as the gate step does: 1 only when
`lint.rules` grades `flake-check` an error, 0 when there were no changed tests.
At any pin it stays a warning unless `lint.rules` says error (the rollout rule,
`ci/lint-rollout`). A branch that changed no test passes it. Details of the
gate: `ci/gate`.

## Confirming a red run

```bash
forge test flakes confirm --run 1234567890           # a GitHub Actions run (its failed-job logs, read with gh)
forge test flakes confirm --logs .git/ci-gate/runs/test.abc123 --sha "$(git rev-parse HEAD)"
forge test flakes confirm --from .forge/tests/runs/<id>/unit/results.json
forge test flakes confirm --run 1234567890 --list    # only list the failing tests found
```

It reads the failing Go tests of a red run and reruns each **once, alone, under
`-race`**, at the run's commit (run it from the module root of a checkout of
that commit), then classifies every failure with the verdicts above. At most
`--max` (15) distinct tests are rerun. The reruns are recorded as lane
`confirm` of a new run, with every verdict in `confirm/confirm.json`
(`forge.test.confirm/v1`), so `forge test flakes` sees a confirmed flake. **It
always exits 0 and never changes the verdict of the run it read.**

## Quarantine

A quarantine is the gap between detecting a flake and deleting or rewriting the
test. It is one file per test:

```
ci/quarantine/<lang>/<package dir>/<Test>.yaml        # `_root` for the module's root package
```

```yaml
lang: go
package: example.com/shop/internal/billing
test: TestRefund
issue: https://github.com/acme/shop/issues/12
reason: times out under -race
signature: ...
quarantined_at: 2026-11-21T05:45:53Z
expires_at: 2026-11-23T05:45:53Z
commit: ...
run: ...
```

The file is flat `key: value` lines, so it is YAML and a line-oriented shell
reader alike. One file per test means two quarantine pull requests never
conflict: each adds its own file, and the path is the key.

```bash
forge test quarantine add go:example.com/shop/internal/billing:TestRefund --issue https://github.com/acme/shop/issues/12
forge test quarantine add ./internal/billing:TestRefund --reason "times out under -race" --expires 48h
forge test quarantine list [--overdue] [--json]
forge test quarantine remove <test-id>              # exit 1 when the test is not quarantined
forge test quarantine skip-regex go                 # ^(TestA|TestB)$, or nothing
```

- `add` takes the id forge prints or `<package>:<Test>`, where the package is
  the import path or the directory relative to the module root. An existing
  entry is left as it is unless `--force`. `--expires` defaults to 48h; `0`
  writes an entry that is never overdue. `--commit`, `--run` and `--signature`
  record where the flake was seen.
- **The lanes skip quarantined tests:** the scaffolded Taskfile passes `-skip
  "$(forge test quarantine skip-regex go)"` (asking forge only when
  `ci/quarantine/go/` exists or `$CI_QUARANTINE_EXTRA` is set). The names come
  from the file names alone. `$CI_QUARANTINE_EXTRA` names an overlay file of
  `<package><TAB><Test>` lines that adds to them. For `go` it always exits 0,
  and an unreadable list is an empty one, so the lanes run every test, the safe
  direction.
- **An expired entry stays applied.** The test is still skipped, and `forge
  test flakes` and `quarantine list` report it **overdue**. Remove the entry in
  the same change that deletes the test or rewrites it as a deterministic one.

## Issues and quarantine pull requests

For a customer, forge **suggests** a quarantine and never makes one: the flakes
list prints the command. Two forge.yaml keys opt in to more, both off by
default:

```yaml
tests:
  flakes:
    quarantine: suggest     # suggest (default) | pr
    issues: false           # true: file flake:suspected / flake:removed issues
```

- **`issues: true`** makes the product file or update one `flake:suspected`
  issue per suspected test, carrying the test body, the failure signature and
  the runs, and a `flake:removed` issue on quarantine; it closes a
  `flake:suspected` issue when its test leaves that state.
  `forge test flakes --issues` does the same locally with `gh` (`--repo` names
  the repository, default `$GITHUB_REPOSITORY` or the current one), for a
  project with no control plane.
- **`quarantine: pr`** makes the hosted product open the quarantine pull
  request itself, through the project's GitHub connection. Both server-side
  actions need a control plane that records Tests (`deploy/record`).

forge's own repository, control-plane and reliant turn both on, and set
`flake-check: error`.

---
Related skills (use skill tool to load):
- testing/coverage: capture and the record the flake states are computed from.
- testing: the tiers; `testkit.Slow` and the `test-waits` lint rules that stop the common flakes at the source.
- ci/gate: the gate's lanes, where `flake-check` runs.
- ci/lint-rollout: why a new rule warns before it gates.
