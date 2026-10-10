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
name (`docker rm -f <name>`). Stop only a PID you started yourself and recorded.
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
3. **Full gate — once, at the end / CI:** `task test` (`go test -race -count=1 ./...`) plus the e2e corpus: `go test -tags e2e -count=1 -timeout 60m -run TestE2E ./internal/cli/`. The e2e tests are `t.Parallel()` (independent projects in separate temp dirs, forge binary built once via `sync.Once`), so wall-clock is roughly the slowest fixture. Run it in the background: the full lane, `internal/cli` / `internal/tierguard` in full mode, and the e2e corpus all exceed ~2 minutes.

Rules that keep the tiers honest:

- Any test that takes **>2s** (subprocess spawns, git repos, network, real scaffolds, KCL renders, `go build`/`go list`/`go mod tidy`, `npm install`) must be skipped or have its slow side-effect bypassed under `testing.Short()` — `if testing.Short() { t.Skip("<why it is slow>; runs in task test") }` — with the slow path still exercised in full mode and CI. Never weaken an assertion — gate, don't gut. A test that is slow because its fixture waits on something it never needed to (a fake CLI that sleeps, a real `gcloud` call) is a fixture bug: fix the fixture instead of gating.
- **A test that runs under `-short` must be cache-sound.** Go's test cache keys only on what the test PROCESS opens or stats inside the module. Repo files read by a CHILD process (`go build`/`go list` over repo packages, `git` against the checkout, `kcl`/`node`/`bash` over files on disk) or by the native KCL runtime are invisible to it, so the cached run replays a stale PASS after an edit that breaks the test. Such a test either skips under `testing.Short()` or opens/stats those inputs itself (a `filepath.WalkDir` + `os.Stat` over the tree is enough — verified). Inputs that are `go:embed`ded into the test binary, or written by the test into `t.TempDir()`, are already covered.
- A package that exceeds `task test:short`'s 60s timeout has grown a slow test. Find it with `go test -short -count=1 -json ./internal/<pkg>/` and gate it; do not raise the timeout.
- e2e tests that boot servers must allocate ports with `freePortE2E(t)` (`internal/cli/scaffold_e2e_test.go`) — never hard-code a port; the corpus runs in parallel.
- e2e tests must keep all state inside their own `t.TempDir()` project; no `t.Setenv`/`t.Chdir` in parallel tests (Go panics on the combo).
- Never set a private `GOMODCACHE` either; CI runs the full non-short suite with `-race`, and `-short` is a local/agent convention only.

See the comment block at the top of `internal/cli/fixture_corpus_e2e_test.go` for the same tiers from the e2e corpus's point of view.
