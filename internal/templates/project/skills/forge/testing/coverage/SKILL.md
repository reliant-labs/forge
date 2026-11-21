---
name: testing/coverage
description: "Recorded test results and coverage — how the Taskfile lanes capture through `forge test begin` and `forge test capture` (forge never runs a suite of its own), atomic mode, `tests.coverage` and `packages: module`, generated code left out of the percentages, `forge test report` / `coverage --diff` / lcov, cobertura, html and `--gate-json`, instrumented `cover` builds for e2e coverage (KCL `forge.Tests`; local, ephemeral and preview envs only), `forge test collect`, and the exit codes. Use when wiring a lane, reading coverage, or asking why a figure is what it is."
applies: any
---

# Recorded results and coverage

A forge project records what its own test lanes did: one row per test, the
statements each lane reached, and the commit, tree and CI run they came from.
It records them under `.forge/tests/runs/<id>/`, merges them across lanes, and
reads them back as one report. Nothing here changes what runs or whether it
passes.

**forge never decides how the tests run.** `forge test` was once a second
spelling of the suite, and it reported on a different suite than the one CI
ran. The flags that make a lane are in the project's Taskfile, where anyone can
read them; forge wraps those commands and records the result. `forge test run`
only runs the Taskfile's own lanes (`task test`, then `task test:integration`
when the project has it) as one recorded run, and refuses with exit 4 when the
Taskfile has no such lane.

Skip all of this with `FORGE_TESTS=off` for one invocation (capture becomes a
pure pass-through) or `tests.coverage: false` in forge.yaml (lanes record
results only). Nothing blocks unless the project opts in.

## How a lane captures

Two plumbing commands bracket a lane. The scaffolded Taskfile already has them;
this is what they do, for when you wire a lane of your own.

```bash
RUN=$(forge test begin)          # prints .forge/tests/runs/<ulid>/ and writes its run.json
forge test capture --run "$RUN" --lane unit -- \
  go test -json -count=1 -covermode=atomic -coverprofile="$RUN/unit/cover.out" ./...
```

- **`forge test begin`** allocates the run directory and writes `run.json`
  (`forge.test.run/v1`: commit, tree hash, dirty flag, branch, base, CI
  provider and run URL, forge version). When `FORGE_TEST_RUN` is already set it
  prints that directory instead, which is how several lanes join one run: `forge
  test run` sets it. `--base <rev>` overrides the revision diff coverage is
  measured against.
- **`forge test capture`** runs the command exactly as given (no shell, stdin
  and environment passed through) and **exits with the command's own exit
  code, always**: 128+n when signal n killed it, 127 when it was not found. A
  record it cannot write is a warning on stderr, never a different verdict.
  Stdout is read as a `go test -json` stream, stored scrubbed of secret shapes
  in `<lane>/results.json` (outputs truncated to 8 KiB a case), and printed as
  the ordinary text `go test` prints, so `-json` costs the reader nothing. A
  stream it cannot parse is stored as it is and recorded `results: unparsed`.
  `--render raw` prints the stream unchanged.
- A runner that writes a report file instead (Vitest, JUnit) names it:
  `--results web/junit.xml` (`--format go-json|junit|vitest` when detection is
  wrong). The file is copied, scrubbed, into the lane after the command exits.
- The command sees `FORGE_TEST_RUN`, `FORGE_TEST_LANE` and
  `FORGE_TEST_LANE_DIR`, the directory its coverage goes in (a
  `-coverprofile`, a `GOCOVERDIR`, a Vitest `reportsDirectory`). After it
  exits, `<lane>/capture.json` (`forge.test.lane/v1`) holds the command, its
  start and end, its exit code, the results and the coverage files found.

`task test`, `task test:integration`, `task test:e2e` and the frontend lanes
capture. `task test:short` records nothing: it is the inner loop and stays
exactly what it is. `forge test runs [--commit <sha>|--branch <b>] [--limit N]
[--json]` lists what is recorded, newest first.

## The coverage flags

**`-covermode=atomic`, always.** Every scaffolded lane, `GoBuild` and the
Dockerfile `cover` stage pass it explicitly. Hit counts add up across lanes
only because every input counts; without the flag a lane that does not run
`-race` gets `set` mode, which records 0 or 1. A `set` profile from a project's
own lane is still accepted, and is merged as covered or not covered, with no
count. Measured: no difference in test time between the two modes.

`-coverprofile` is what a unit lane uses, not `-test.gocoverdir`: a
`-coverprofile` run replays from Go's test cache with the same blocks and
counts, so capturing a cached `-short` lane is close to free. A `-count=1` lane
never replays, with or without coverage.

**`tests` in forge.yaml**, every key optional:

```yaml
tests:
  coverage: true            # default in a new scaffold; false: lanes record results only
  # coverage:               # or the mapping form (enabled defaults to true)
  #   packages: package     # package (default) | module
  #   exclude: ["internal/legacy/**"]
  #   include_generated: false
  upload: ci                # ci (default) | always | never
```

- **`packages: package`** (the default) means each package covers its own
  statements. **`packages: module`** adds `-coverpkg=<module>/...`, which also
  counts code reached from other packages' tests. It is opt-in because it
  instruments every package in every test binary (up to 2.7x the cold compile
  in the sample), and its raw profile repeats each block once per test binary
  that links it, 18 to 31 times over. Capture **deduplicates** it by block key
  before anything is stored.
- A project with no `tests:` block has coverage off until it opts in, so a
  forge bump alone never instruments anything. The old `ci.test.coverage` key
  keeps working: `true` means `tests.coverage: true`. The upgrade playbook
  `migrations/v1.9.0/tests` adds the block to an existing project.
- **Frontends.** The scaffolded Vitest config records only when
  `FORGE_TEST_LANE_DIR` is set, so a bare `npm test` is unchanged. Coverage is
  v8 with the **`json` (istanbul) reporter only**: `coverage-final.json` holds
  statement blocks, the granularity the Go lanes record. Do not add the `lcov`
  reporter beside it. LCOV has only lines, a second line-granular copy of the
  same files, and `forge test report` refuses to merge one file at two
  granularities (exit 2, "statement and line coverage cannot merge").

## What is merged, and what is left out

- **A block is the unit.** Blocks are keyed by `(file, start line.col, end
  line.col)`, the same key in a unit profile, an integration profile and e2e
  covdata, so the union is exact. Hit counts sum. The denominator is every
  instrumented statement, so a package with no tests counts at 0%.
- **Same source only.** Runs merge only when their tree hashes are equal. A
  run on a dirty tree never joins a commit's report. Lanes from different
  trees are refused (exit 2), never averaged.
- **Same build only.** Two blocks with one key but different statement counts
  prove the source differs, and are refused (exit 2), never averaged. e2e
  covdata is grouped by its `covmeta` hash, which fingerprints the build; Go's
  own `covdata merge` accepts two builds of one package without a word and
  reports nonsense, so forge never merges them.
- **Generated code is excluded** from every percentage by default: `gen/`,
  `*_gen.go`, `mock_gen.go`, `*.pb.go`, `*-hooks.ts` and files headed "Code
  generated … DO NOT EDIT." It is reported on its own line (in a server
  project it is often a fifth of the statements). `tests.coverage.exclude` adds
  globs; `include_generated: true` counts it.
- **Languages are kept apart.** Go and TypeScript percentages are shown
  separately; a combined project figure is weighted by statements and labelled
  as such. LCOV and Cobertura inputs carry lines only, so a line counts as one
  statement and the report says `granularity: line`.

## Reading it

All of these read the record and **run nothing**. `task test:report` and `task
coverage` wrap them in a scaffolded project.

```bash
forge test report                                  # newest run, with every run on its source
forge test report --commit HEAD --base origin/main
forge test report --json                           # forge.test.report/v1
forge test report --format lcov > lcov.info        # or cobertura, html, json, text
forge test report --gate-json gates/test.json      # the deploy's `tests` gate document
forge test coverage                                # combined, per lane, per package
forge test coverage internal/billing --by file
forge test coverage --diff origin/main             # which changed lines no test reached
forge test coverage --lane e2e --json              # forge.test.coverage-view/v1
```

- **Which runs.** With no argument, the newest run's source: the clean runs of
  its commit, or the runs on its dirty tree. `<run>...` names runs by id, id
  prefix or directory; `--commit <sha>` takes every clean run of a commit.
  `coverage` takes `--run`, `--commit` and `--lane`.
- **Views.** Per lane (that lane's covered over instrumented statements);
  combined (covered if any lane covered it); only-e2e (reached by e2e and
  nothing else); per package or per file (`--by package|file`); diff coverage;
  delta.
- **Diff coverage** is the fraction of the executable lines `git diff -U0
  <base>...<commit>` adds or changes that a test reached. The base is `--base`,
  else the run's recorded base (a pull request's), else the merge base with the
  default branch. The **delta** is the combined percentage minus the base
  commit's own recorded report, and says "no report for base" when there is
  none, never comparing against an older commit.
- **`--gate-json FILE`** writes the same gate document `forge ci
  verify-test-run --gate-json` writes, with its zero-evidence and mass-skip
  checks, so `forge gate record` and `forge env deploy --gate` keep their
  meaning. The scaffolded release workflow uses it.
- **Source is never stored.** Each covered file carries its repo-relative path
  and its **git blob id**, which pins the exact bytes the counts describe
  (`forge project schema test-coverage`). A viewer fetches the source from
  GitHub by that blob id at view time and draws nothing over source that does
  not match it; without GitHub access it falls back to line numbers and hit
  counts.

## Exit codes

Coverage never decides one.

| Command | 0 | 1 | 2 | 4 |
|---|---|---|---|---|
| `test run` | every lane exited 0 | a lane failed (each lane's code is in `--json`) | | refused, nothing run: no such Taskfile lane, or `task` not installed |
| `test capture` | the command's own exit code, always | | | |
| `test report` | every recorded test passed | a test failed | undetermined: no results, a lane that could not be read or exited non-zero with no failure recorded, a package whose every test skipped, or runs from different trees | `--upload=required` and it could not upload |
| `test coverage` | coverage found | | none recorded | |
| `test collect` | something collected, or nothing to collect | a bad argument | an instrumented workload could not be reached and nothing was collected | |

## Instrumented builds: coverage from a running stack

An e2e test drives a running stack, so the coverage that matters is the
servers'. For those, forge builds Go workloads with `-cover
-covermode=atomic` and collects what they reached.

**Which envs.** `coverage` is computed, never scaffolded: on for an env with
`lifecycle = "local"` or `"ephemeral"` and for a **preview** instance, off for
everything else, and only when forge.yaml opts the project in (`tests.coverage:
true`, which a new scaffold writes). So the scaffolded `dev` and every preview
are instrumented with no line in `main.k`. An env with neither a lifecycle nor
a preview is persistent, which forge treats as production, and is never
instrumented by default.

```
tests = forge.Tests {coverage = False}                       # never in this env
tests = forge.Tests {coverage_packages = ["./internal/..."]} # default "main": the binary's module minus gen/
tests = forge.Tests {coverage = True, coverage_in_persistent_env = "staging runs the nightly e2e"}
```

- **Persistent envs refuse it.** `coverage = True` on one fails to render
  unless `coverage_in_persistent_env` says why, so a staging deliberately used
  for e2e is a decision on the record.
- **One binary, one answer.** `GoBuild.coverage` and `DockerBuild.coverage`
  override the env per binary, and two workloads of an env that run the same
  binary must agree, or the env does not render.
- **A cover build is a variant of the release**, not a replacement: the
  Dockerfile gets a `cover` stage beside `debug`, built with `--target cover`,
  and the prod stage stays byte-identical, so its digest is unchanged. A deploy
  refuses to put a `cover` digest on a persistent env, so prod always runs the
  digest staging ran. Only a preview instance resolves a release's cover
  variant.
- **Host runtime.** forge appends the flags to its `go build`, and under air the
  scaffolded `air.toml` builds with `$FORGE_GO_BUILD_FLAGS`, which `forge env up`
  sets. The process gets `GOCOVERDIR=.forge/tests/covdata/<env>/<workload>/`.
- **Not instrumented:** a prebuilt image or any non-Go server reports
  `coverage: unavailable (not a forge Go build)`. A hosted preview's pods are
  not reachable from outside, so it records results only.

**Collecting.** Go writes counters to `GOCOVERDIR` when `main` returns and on
`os.Exit`; only SIGKILL, an unrecovered panic or a fatal signal lose them. A
long-lived pod may never exit cleanly, so the preferred path is a pull:

- serverkit serves `/debug/coverage` on its pprof listener, **loopback only**
  (`127.0.0.1:6060`, reachable from nowhere else). It is **read-only**: it
  answers GET with a tar of the coverage data and nothing else. A POST or any
  `?reset` gets 405; a request from a browser (an `Origin` header, a
  cross-site or same-site `Sec-Fetch-Site`, or a non-loopback `Host`) gets 403;
  a binary built without `-cover` answers 404. Nothing a request can do changes
  the running process's counters.
- `forge test collect <env>` reaches it per runtime: a host process over
  localhost, a cluster pod through a port-forward, and a compose container by
  `docker cp` of what its process flushed at exit (compose binds pprof on
  0.0.0.0, so the endpoint is withheld there).

```bash
forge test collect dev --reset --run "$RUN"            # baseline: count from here
forge test capture --run "$RUN" --lane e2e -- go test -tags=e2e -json ./e2e/...
forge test collect dev --run "$RUN" --lane e2e         # pull, subtract the baseline, record
```

**`--reset` does not zero anything.** The endpoint is read-only and the
running processes are left alone. `--reset` pulls each live process's counters
and records them as a **client-side baseline**, keyed by process (scope, build
hash, pid). Every later collect subtracts that baseline block by block, clamped
at 0, and writes `covdata/<workload>/since-reset.out` as the lane's coverage
input. What a process flushes at exit is baselined the same way. A new pid, a
new pod or build, or a restart is counted **whole**, since nothing of it
predates the reset. Counter files flushed before the reset, by any process
including one that has since exited, are recorded by (source, name, sha256) and
left out of every later collect; a file flushed after it is always collected.
Run `--reset` before the lane and collect after it.

The scaffolded `task test:e2e ENV=<env>` does exactly this, collecting even
when the suite fails. Coverage lands in `<run>/<lane>/covdata/<workload>/`
grouped by build: the newest build is the workload's coverage, and an older one
(an air rebuild, the last rollout's pod) is kept under `stale/<hash>/`,
labelled "stale build" and never merged. `--workload` narrows it, `--json`
prints `forge.test.collect/v1`. Collection is best effort per workload.
`forge env down --collect` and `forge env preview delete --collect` pull before
teardown (or with `FORGE_TEST_RUN` set), and never fail the teardown.

## Where the record goes

It stays in `.forge/tests/`. `forge test report --upload` sends it to a control
plane, `--upload=required` makes a failed upload exit 4, and `tests.upload`
(`ci` by default, `always`, `never`) says when. Hosted recording, the token it
needs and recording from CI you run yourself are in `deploy/record`. Flaky
tests are in `testing/flakes`.

---
Related skills (use skill tool to load):
- testing: the tiers, the harness and the Taskfile lanes this records.
- testing/flakes: flake states, the quarantine and the flake gate, fed by the same record.
- deploy/record: upload from external CI, and `forge test ingest` for runners forge does not wrap.
- deploy/preview: the preview workflow that collects e2e coverage.
