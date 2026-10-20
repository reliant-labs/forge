<!--
  forge repo notes. Reliant injects forge's live framework guide
  (internal/templates/project/reliant.md.tmpl — architecture, critical rules,
  package-boundary idioms, testing basics) ahead of this file because this repo
  has a forge.yaml, so none of that belongs here. Keep this file to what is true
  of working ON forge itself.
-->

## ⛔ You are running INSIDE forge — do not kill it

The agent session reading this is itself hosted by a forge process. Commands
that stop forge, or that match processes by name, therefore terminate **your
own session** and every agent working alongside you. All in-flight work stops,
and nothing gets reported back.

Never run any of these:

- `pkill forge`, `killall forge`, `pkill -f forge`, or any pattern-matched kill
- `kill` against a PID you did not personally start

`forge env down <env>` / `--all` will not stop the process running it or any
of its ancestors — it walks its own parent chain first and reports what it
skipped — so it cannot end your session directly. It still stops every OTHER
stack it reaches, including ones other agents depend on: run `--all` only when
you mean every stack on the machine, and prefer `forge env down <env>` from
inside the project you started.

**To clean up, be surgical.** Remove only containers you created, by explicit
name (`docker rm -fv <name>` — `-v` also removes the container's anonymous
volumes; without it every image that declares a `VOLUME`, postgres included,
leaves one behind). Stop only a PID you started yourself and recorded.
Leave every pre-existing container, process and stack alone — a shared dev box
runs many of them, and another agent almost certainly depends on one.

**Leaving a stack running is the correct outcome.** It costs a little memory. A
pattern-kill costs the session, the other agents, and the work.

## Philosophy

> Forge is your LLM's best friend. It aims to scaffold a production app from day
> 0, with hooks and seams that allow the LLM to introspect, and connect your app
> to best practices like middlewares, deployments, env promotion, monitoring and
> more. It must support the happy-path 80/20 rule: 80% of users get out of the
> box happy path, but the last 20% of users are never disempowered. We provide
> escape hatches and primitives for users to build upon.

Read this before proposing a design. It rules out whole categories of answer:

- **Production from day 0, not a toy that graduates.** A scaffold that works
  only in dev, and must be replaced before shipping, has not saved anyone work
  — it has deferred it to the moment they can least afford it. What forge
  scaffolds on day 0 is the same thing that runs in prod, configured
  differently.
- **Declarative over imperative.** The state of the world is DECLARED in files
  under version control, and something converges reality to it. A setup step
  that must be _run_, in order, against a live system, is a step that can be
  skipped, half-completed, or lost when a teammate clones the repo. If a
  provisioning story requires a CLI invocation, that is a smell to be designed
  out, not documented around.
- **Primitives, not modes.** An enum of three blessed configurations is not a
  primitive; it is three products, none of which is the one a given user needs,
  and it leaves dead branches in every project that picked one. Ship the real
  thing wired up, and expose the seams that let it be rewired.
- **Working defaults, not empty ones.** A scaffolded field left blank is not
  neutral — it is a broken app plus a scavenger hunt. Scaffold values that make
  the thing RUN, and make replacing them a one-line edit in a file that already
  exists.
- **The 20% are never disempowered.** Every default is overridable, every
  generated file has an owned seam beside it, and no convenience is implemented
  as a wall.

## Package boundaries: the worked example in this repo

The framework guide's package-boundary idioms apply here first. Concretely:
`templates.Render` serves eleven unrelated template categories. It knows only
the one-method `selfDefaulting` interface, declared at the consumer — not any
payload's shape. An earlier version type-asserted one concrete struct and
reached into another domain's package, which made the shared renderer the
obvious place for every domain to special-case. See
`internal/templates/self_defaulting_test.go`, which pins the boundary rather
than the behaviour. A reach for a third interface method is the signal to ask
whether you are modelling the consumer's need or the implementation.

## Testing tiers

Run the cheapest tier that answers your question. Wall-clock budgets are
enforced conventions — if you add a test that breaks a budget, gate it. Most of
an agent's test time was measured as compile/link plus defeated caching, not
test bodies.

1. **Inner loop — every edit:** `task test:short -- ./internal/<pkg>/...`, naming only the packages you touched (`go test -short`, cached, no `-race`).
2. **Package-targeted — before committing:** `task test -- ./internal/<pkg>/...`. Full mode (no `-short`, with `-race`) for the packages you touched, so the gated slow tests run too.
3. **Full gate — once, at the end / main's CI:** `task test` — the whole module in main's three CI lanes from `scripts/ci-test-packages.sh` (unit and `internal/cli` under `-race -count=1`, the guards at `-p 1` without `-race`), each with its own hang budget (15m / 45m / 30m) — plus the e2e corpus: `task test:e2e` (`go test -tags e2e -count=1 -timeout 60m -run '^TestE2E' ./internal/cli/`; narrow it with `task test:e2e -- -run <regex>`). The e2e tests are `t.Parallel()` (independent projects in separate temp dirs, forge binary built once via `sync.Once`), so wall-clock is roughly the slowest fixture. Run it in the background: the full lane, `internal/cli` / `internal/tierguard` in full mode, and the e2e corpus all exceed ~2 minutes.

Rules that keep the tiers honest:

- Any test that takes **>2s** (subprocess spawns, git repos, network, real scaffolds, KCL renders, `go build`/`go list`/`go mod tidy`, `npm install`) must be skipped or have its slow side-effect bypassed under `testing.Short()` — `if testing.Short() { t.Skip("<why it is slow>; runs in task test") }` — with the slow path still exercised in full mode and CI. Never weaken an assertion — gate, don't gut. A test that is slow because its fixture waits on something it never needed to (a fake CLI that sleeps, a real `gcloud` call) is a fixture bug: fix the fixture instead of gating.
- **A test that runs under `-short` must be cache-sound.** Go's test cache keys only on what the test PROCESS opens or stats inside the module. Repo files read by a CHILD process (`go build`/`go list` over repo packages, `git` against the checkout, `kcl`/`node`/`bash` over files on disk) or by the native KCL runtime are invisible to it, so the cached run replays a stale PASS after an edit that breaks the test. Such a test either skips under `testing.Short()` or opens/stats those inputs itself (a `filepath.WalkDir` + `os.Stat` over the tree is enough — verified). Inputs that are `go:embed`ded into the test binary, or written by the test into `t.TempDir()`, are already covered.
- A package that exceeds `task test:short`'s 300s timeout has grown a slow test. Find it with `go test -short -count=1 -json ./internal/<pkg>/` and gate it; do not raise the timeout.
- e2e tests that boot servers must allocate ports with `freePortE2E(t)` (`internal/cli/scaffold_e2e_test.go`) — never hard-code a port; the corpus runs in parallel.
- e2e tests must keep all state inside their own `t.TempDir()` project; no `t.Setenv`/`t.Chdir` in parallel tests (Go panics on the combo).
- Never set a private `GOMODCACHE` either. A pull request's CI runs the `-short` tier (no `-race`); a push to main runs the full non-short suite with `-race`. So tier 2 before committing is how a gated test of yours gets exercised before it merges.

See the comment block at the top of `internal/cli/fixture_corpus_e2e_test.go` for the same tiers from the e2e corpus's point of view.

### CI: what runs where

Two tiers. A **pull request** gets the fast tier, budgeted at about five minutes of wall
time; a **push to main** gets the slow tier as well. Nothing left CI when the tiers split:
every check that is off the pull request runs on every push to main. Each workflow's header
says why its lanes are shaped the way they are; read it before moving a check between
lanes. `ci.yml`'s `CI gate` job prints every job's wall time and the critical path on each
run — watch it when you add a test.

**Pull request** (`ci.yml`, plus `pre-commit.yml`, `library-lint.yml`, `license-gate.yml`):

- **Test (-short)** — the `-short` tier, no `-race`, as 2 package shards
  (`scripts/ci-test-shard.sh` over `scripts/ci-test-packages.sh short I N`).
  `internal/ciguard` fails if the matrix misses an index. No `-count=1`: unchanged packages
  replay main's cached results.
- **Flake gate** — each test the change adds or edits runs three times under the race
  detector (`scripts/ci-changed-tests.sh`). A test that fails one run in three never merges.
- **Windows (cross-compile + vet)** — `GOOS=windows` build and vet (tests included) from
  Linux, and the windows/arm64 build of `cmd/forge`.
- **compile (-tags e2e)** — `go vet -tags e2e` over the e2e-tagged packages.
- Lint, Verify Generated Code + Validate KCL, Vulnerability Scan.
- A pull request that touches only `docs/**`, root `*.md` or skills skips `ci.yml` and
  `library-lint.yml`; `docs-skills.yml` runs the `-short` tests that read those files.

**Push to main** (and `workflow_dispatch` on any branch) adds:

- **Unit** — `ci.yml` Test: `go test -race -count=1` over `scripts/ci-test-packages.sh unit`
  (every package except `internal/cli` and the guards), full mode.
- **internal/cli** — `ci.yml` Test (internal/cli): the package alone, `-race -count=1`. Its
  subpackages stay in Unit.
- **Guards** — `ci.yml` Test (guards): tierguard, removalguard, deadcodeguard; `-p 1`, no
  `-race`. Under `-short` they skip their whole-repo work, so pull requests get their fast
  half only.
- **Test (-short, whole tier)** — the pull-request tier in one job; it saves the
  `go-build-short` cache the shards restore.
- **Build** (`-trimpath -buildvcs`) and **License Compliance** (go-licenses);
  `license-gate.yml` checks dependency licenses on every pull request.
- **Windows** — native: build, vet, the `-short` tier of Unit + internal/cli, and a
  scaffold+render smoke.
- **e2e** — `e2e-suite.yml`: `prepare` builds the e2e test binary, forge and the codegen
  plugins once and plans 4 shards by recorded duration (`.github/e2e-timings.json`,
  `internal/e2eshard`); the shards run only `TestE2E*` tests, minus `TestE2ENightly*`; a
  coverage ledger proves they ran every selected test. **E2E Scaffold** (`e2e-scaffold.yml`)
  likewise. Label a pull request `ci:e2e` to run both on it before merging.
- **Nightly** — `nightly.yml` (schedule + dispatch): the full windows/arm64 `go vet`, the
  whole e2e suite (`select: all`, so `TestE2ENightly*` too) and E2E Scaffold. A red nightly
  on main opens, or comments on, the "Nightly CI is red" issue; the next green one closes it.

So a test gated with `if testing.Short()` runs on main, not on pull requests: mark the slow
ones, and a red main is bisected from the merge that caused it.

e2e naming is load-bearing: a test in an e2e-tagged file must be named `TestE2E*` and no
untagged test may be — `internal/e2eshard`'s guard fails the test shards otherwise, because
the e2e shards select by that prefix. A test with no recorded duration is planned at the
median; `scripts/e2e-timings.sh` refreshes the file from a CI run.

A check moves off the pull request only when it is slow AND a failure is rare and cheap to
bisect from main. Say why in its job comment.

Build caches: a PR job restores main's cache and never saves one; only pushes to main
save (`ci.yml`'s "Build-cache note"). A PR's cache is visible to that PR alone, and PR
saves were evicting main's entries from the repository's 10 GB budget.
