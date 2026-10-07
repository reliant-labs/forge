<!-- forge:version=1 -->

# forge

This is a **Forge** project managed by the `forge` CLI.

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

## Skills

Run `forge skill list` to discover available playbooks, and `forge skill load <name>` to read one. Available skills:

- **forge** — start here: greenfield sequence, project conventions
- **services** — adding and editing services
- **api** — Connect RPC API patterns
- **db** — database, ORM, and migrations
- **frontend** — Next.js frontend overview
- **frontend/state** — state management (React Query, Zustand, URL)
- **frontend/patterns** — UI component patterns
- **proto** — protobuf schema conventions
- **architecture** — system architecture and layout
- **workers** — background job workers
- **auth** — authentication
- **testing** — testing overview
- **testing/unit** — unit test patterns
- **testing/integration** — integration test patterns
- **testing/e2e** — end-to-end test patterns
- **debug** — debugging overview
- **debug/investigate** — investigation techniques
- **debug/isolate** — isolating failures
- **debug/reproduce** — reproducing bugs
- **deploy** — deployment and releases
- **observability** — logging, tracing, and metrics

## Critical Rules

1. **Never edit generated code** — `gen/` and `*_gen.go` files are overwritten by `forge generate`. Make changes in proto files instead.
2. **Proto is the canonical input** — all API contracts, ORM models, and frontend hooks derive from proto definitions.
3. **`forge generate` is safe** — it never overwrites hand-written business logic (handler files, `pkg/app/setup.go`, etc.).
4. **Migrations are the DB source of truth** — the database schema comes from migrations, not proto. Proto drives the ORM layer above them.
5. **Use `task test`** — not raw `go test`. The project's `Taskfile.yml` sets the correct build tags, timeouts, and frontend lane, and it is what CI runs. (There is no `forge test`.)

## Package boundaries and interfaces

**Package A must be ignorant of package B's internals.** A concept that leaks
across a package boundary — through a func, a param, a type or a return value —
is the defect. This is the rule most often broken by a well-intentioned
"let's share this".

Three idioms that keep it true:

1. **Declare interfaces where they are CONSUMED, not where they are
   implemented.** Do not export an interface from the implementing package and
   make callers depend on it. If package X needs "something that can list Y",
   X declares that one-method interface locally. **WET over DRY is fine here** —
   two small local declarations beat one shared exported one.
2. **The larger the interface, the weaker the abstraction.** Prefer thin. A
   1–2 method interface is strong; a six-method one is a concrete type in
   disguise and will leak. Reaching for a third method is the signal to ask
   whether you are modelling the consumer's need or the implementation.
3. **Accept interfaces, return structs.** Take the narrow interface you need
   (flexible); return concrete types (explicit). Returning an interface hides
   what the caller actually got.

**An interface with ONE implementation and no substitution point is
indirection, not abstraction.** Prefer a concrete type with methods. Go is not
Java: `Kind() string` that every caller switches on has relocated a switch, not
removed it.

Worked example in this repo: `templates.Render` serves eleven unrelated
template categories. It knows only the one-method `selfDefaulting` interface,
declared at the consumer — not any payload's shape. An earlier version
type-asserted one concrete struct and reached into another domain's package,
which made the shared renderer the obvious place for every domain to
special-case. See `internal/templates/self_defaulting_test.go`, which pins the
boundary rather than the behaviour.

## Testing tiers

Run the cheapest tier that answers your question. Wall-clock budgets are enforced conventions, not aspirations — if you add a test that breaks a budget, gate it.

**Testing while iterating: run the fast tier, scoped to what you touched.**
Most of an agent's test time was measured as compile/link plus defeated
caching, not test bodies — so the inner loop is scoped and cached, and the
full lane runs once, at the end.

1. **Inner loop — every edit:** `task test:short -- ./internal/<pkg>/...`, naming only the packages you touched. It is `go test -short`, cached, with no `-race`. A re-run after an edit recompiles and re-runs only what the edit invalidated; everything else is a cache hit. Unscoped (`task test:short`) it is the whole repo.
2. **Package-targeted — before committing:** `task test -- ./internal/<pkg>/...`. Full mode (no `-short`, with `-race`) for the packages you touched, so the gated slow tests run too.
3. **Full gate — once, at the end / CI:** `task test` (`go test -race -count=1 ./...`) plus the e2e corpus: `go test -tags e2e -count=1 -timeout 60m -run TestE2E ./internal/cli/`. The e2e tests are `t.Parallel()` (independent projects in separate temp dirs, forge binary built once via `sync.Once`), so the gate's wall-clock is roughly the slowest fixture, not the sum. Do not run this after every edit.

How to run them:

- **Do not add `-count=1` or `-race` to inner-loop runs.** Both defeat Go's test cache, which keys on the test binary, flags, env vars and the files a test reads — it is correct for hermetic tests. `-count=1` belongs to CI and the full lane.
- **Never set a private `GOCACHE`** (or `GOMODCACHE`). A cold private cache turns every run into a full rebuild.
- **Anything expected to take more than ~2 minutes goes `run_in_background` + `shell_wait`**, never a long foreground call: the full lane, `internal/cli` or `internal/tierguard` in full mode, the e2e corpus.

Rules that keep the tiers honest:

- Any test that takes **>2s** (subprocess spawns, git repos, network, real scaffolds, KCL renders, `go build`/`go list`/`go mod tidy`, `npm install`) must be skipped or have its slow side-effect bypassed under `testing.Short()` — `if testing.Short() { t.Skip("<why it is slow>; runs in task test") }` — with the slow path still exercised in full mode and CI. Never weaken an assertion to get under the budget — gate, don't gut. A test that is slow because its fixture waits on something it never needed to (a fake CLI that sleeps, a real `gcloud` call) is a fixture bug: fix the fixture instead of gating.
- **A test that runs under `-short` must be cache-sound.** Go's test cache keys only on what the test PROCESS opens or stats inside the module. Repo files read by a CHILD process (`go build`/`go list` over repo packages, `git` against the checkout, `kcl`/`node`/`bash` over files on disk) or by the native KCL runtime are invisible to it, so the cached run replays a stale PASS after an edit that breaks the test. Such a test either skips under `testing.Short()` or opens/stats those inputs itself (a `filepath.WalkDir` + `os.Stat` over the tree is enough — verified). Inputs that are `go:embed`ded into the test binary, or written by the test into `t.TempDir()`, are already covered.
- A package that exceeds `task test:short`'s 60s timeout has grown a slow test. Find it with `go test -short -count=1 -json ./internal/<pkg>/` and gate it; do not raise the timeout.
- e2e tests that boot servers must allocate ports with `freePortE2E(t)` (`internal/cli/scaffold_e2e_test.go`) — never hard-code a port; the corpus runs in parallel.
- e2e tests must keep all state inside their own `t.TempDir()` project; no `t.Setenv`/`t.Chdir` in parallel tests (Go panics on the combo).
- CI runs the full non-short suite with `-race`; `-short` is a local/agent convention only.

See the comment block at the top of `internal/cli/fixture_corpus_e2e_test.go` for the same tiers from the e2e corpus's point of view.

## Project Notes

<!-- Add project-specific context, conventions, and open questions here. -->
