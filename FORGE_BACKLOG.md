# Forge backlog (overnight session, 2026-04-30)

## Open

- **(2026-05-06 cpnext continuation 8) `contractlint` flags `_test`
  packages with `Test*` exported symbols.** When a package has a
  `contract.go` declaring a `Service` interface, declaring tests in the
  conventional `package <name>_test` form trips the contract linter:
  `internal/entitlement/service_test.go:1:1: package entitlement_test
  has exported methods but no contract.go`. The test functions are seen
  as exported symbols in a "different" package (`entitlement_test`),
  and the linter expects every package with exported symbols to declare
  a contract. Workaround: use the internal-test form (`package
  entitlement`, not `package entitlement_test`) — loses the
  API-boundary discipline. Suggested fixes: (a) treat `*_test` packages
  as exempt from the exported-method-needs-contract rule; (b) skip
  files matching `*_test.go` entirely from the contractlint pass — Go
  test funcs by convention start with `Test*` / `Benchmark*` /
  `Example*` and are never part of the package's API surface. Likely
  one- or two-liner in
  `forge/internal/linter/contract/exported_methods.go`.

- **(2026-05-07 polish-round, surfaced multiple sessions) Internal
  package scaffolds emit single-result `New(Deps) Service` but the
  canonical/polished form is two-result `New(Deps) (Service, error)`.**
  This polish round (2026-05-08) updated `service.go.tmpl` and
  `contract_test.go.tmpl` to ship the two-result form, but the
  flavored variants (interactor, adapter, eventbus, client) and their
  matching `*_test.go.tmpl` still emit single-result. Drift creates
  a cross-flavor inconsistency that surfaces when users polish New in
  one package and expect the convention from another. Followup: walk
  `internal/templates/internal-package/{interactor,adapter,eventbus,client}/`
  and align them to two-result, plus adjust their tests.

- **(2026-05-07 cpnext T2-A) `forge generate` gated by sibling-lane
  convention violations.** During parallel-lane work, an unrelated
  `internal/<other-lane>/contract.go` violation (e.g. wrongly-named
  type / func in a vendor adapter where the contract.go declares a real
  `type Service interface` but the linter incorrectly flags type names
  in a SIBLING file as living "in contract.go") blocks `forge generate`
  project-wide — no escape hatch (`--force` does not help; the
  pre-codegen contract check runs unconditionally). This breaks the
  canonical "drop stub when real handler lands" flow: parallel agents
  can't regenerate `handlers_gen.go`, so they have to overwrite it by
  hand. Suggested fixes: (a) scope the pre-codegen contract check to
  packages actually being regenerated; (b) add a `--skip-pre-checks`
  flag for parallel-lane workflows; (c) fix the contractlint to
  accurately attribute violations to the file they live in (today
  `client.go`'s `type APIError` is reported under `contract.go:25`).

  *2026-05-07 T3 update*: project-wide regenerate worked once the email/
  litellm `Deps` were widened to accept `Logger / Config` (forge auto-
  discovered them as packages and emitted the canonical bootstrap call
  shape; the package-side Deps need to either match or carry a
  `// forge:not-a-package` opt-out). Backlog item still open.

- **(2026-05-07 cpnext T2-A) `handlers_scaffold_test.go` factory
  rejects realistic handlers that have required Deps fields.** The
  scaffold goes through `app.NewTest<Svc>(t)` which calls `New(deps)`
  with only the bare-Deps trio (Logger/Config/Authorizer). When the
  handler grows a required Repo field tied via `validateDeps`, the
  scaffold tests fail at construction with "Deps.<Repo> is required"
  before any RPC is dispatched. The scaffold convention assumes
  bare-Deps is enough, which it never is for a real handler. We worked
  around by deleting the scaffold and hand-rolling `handlers_test.go`
  using `tdd.RunRPCCases` directly. Suggested fixes: (a) auto-emit
  `t.Skip("scaffold: <field> required, swap to handlers_test.go")` when
  the handler's `validateDeps` rejects bare-Deps; (b) have
  `app.NewTest<Svc>` thread an optional in-memory test fake for any
  named-Repository field via reflection.

  *2026-05-07 T3 update*: confirmed the gap at scale — patched
  `pkg/app/testing.go::NewTestDaemon` by hand to inject `stubDaemonRepo{}`
  from `pkg/app/testing_extras.go` when `deps.Repo` is nil, then used
  `forge generate --accept` to record the hand-edit. The stubs let all
  12 daemon scaffold tests pass without integration-DB. Cleaner
  long-term fix: extend the testing.go template to insert a
  user-overridable stub-injection block per service-owned interface
  field. See `pkg/app/testing_extras.go` for the pattern.

- ~~**(2026-05-07 cpnext-v2 upgrade dogfood) `forge upgrade --to v0.2` rejected
  pseudoversion baselines AND missed the if-err init-form ApplyDeps shape AND
  scattered comments onto synthesized assignments.** ✅ Fixed in-session.
  Three forge bugs surfaced while running `forge upgrade --to v0.2` against
  cpnext-v2 (a real 9-service v0.1-shape project; codemod had only been tested
  against a synthetic single-service setup.go):
  1. **Minor-hop guard rejected pseudoversions.** `cfg.EffectiveForgeVersion()`
     returned `v0.0.0-20260430002332-...` for projects pinned to a pre-v0.2
     pseudoversion; `splitMinor` parsed it as `0.0` and the guard refused
     `--to v0.2` as a 2-minor jump. Added `isPreV01Baseline` helper in
     `forge/internal/cli/upgrade.go` that recognizes the `0.0.0-` pseudoversion
     prefix and the unset-pin sentinel, normalizing both to `v0.1` (the
     implicit pre-codemod baseline). The error-message guide then correctly
     suggests `forge upgrade --to v0.2` rather than the bogus `--to v0.1`.
  2. **Codemod only matched bare-ExprStmt ApplyDeps.** v0.1 cpnext-v2 wraps
     `app.Services.X.ApplyDeps(...)` in `if err := ...; err != nil { return ... }`
     (init-form IfStmt) and an outer `if app.Services != nil { ... }` guard.
     Refactored `codemodSetupGo` in `upgrade_v0_1_to_v0_2.go` to recurse via
     `rewriteApplyDepsBlock`, matching three shapes: bare ExprStmt, IfStmt
     with init, and either nested in an outer guard. Auto-rewrites went from
     1 (the standing "run forge generate" instructional line) to 20 real AST
     rewrites for cpnext-v2.
  3. **go/format scattered comments onto synthesized AssignStmts.** Comments
     attached to deleted CompositeLit fields ("// Stripe nil — ...") and
     between consecutive ApplyDeps calls were floating onto the new
     `app.<Field> = ...` assignments, producing syntactically broken output
     (e.g. `app.\n\t\t// comment\n\t\tStore = ...`). Added `removedRanges`
     tracking + a `commentInAnyRange` filter on `file.Comments` before
     `format.Node`. Records both the per-call range AND the entire enclosing
     block range when a block had any rewrite, so inter-call gap comments
     don't leak. Block-preamble comments outside the brace stay preserved.

- ~~**(2026-05-07 cpnext re-port D1) `pkg/app/testing.go` `NewTest<Svc>` factory
  drops bare-Deps when `With<Svc>Deps` is supplied.** ✅ Fixed in-session.
  The generated factory was `if cfg.<svc>Deps != nil { return New(*cfg.<svc>Deps) }`,
  which means tests that wanted to inject ONE service-owned dep (e.g. just
  `Store` on audit_log) had to re-supply Logger/Config/Authorizer themselves
  or get a `validateDeps` failure. Patched
  `forge/internal/templates/project/bootstrap_testing.go.tmpl`: factory now
  starts from override (so explicit-nil values for service-owned fields
  still flow through to exercise "not configured" paths) and fills in
  zero-valued bare-Deps from the test defaults. Surfaced while porting
  audit_log handler tests in continuation D1.

- ~~**(2026-05-07 cpnext re-port D1) `.forge-pkg/controller/reconciler.go`
  drifts from `forge/pkg/controller/reconciler.go` after partial sync,
  leaving an unused `handler` import that fails the `forge generate`
  validation step.** ✅ Worked around in-session by re-syncing the file from
  upstream. Root cause likely lives in the `.forge-pkg` sync pipeline:
  it copied a newer reconciler.go that stripped out the `WatchSpec` block
  (which used `handler.EventHandler`) without also deleting the now-unused
  import. Either the partial-write got committed or the diff-trim drops
  trailing-block content but doesn't re-run goimports. Worth a real fix in
  whatever syncs `.forge-pkg/`.

- ~~**(2026-05-07 cpnext re-port
  after a no-op `forge generate`.** ✅ Fixed in-session. `protoc-gen-go` always
  rewrites `*.pb.go` (mtime bumps even when content is unchanged), but
  protogen's `*.pb.orm.go` writer skips byte-identical rewrites — so a
  no-op regen leaves orm-side mtimes lagging the pb.go ones, which the
  staleness check then flags as drift. Patched in
  `forge/internal/cli/generate_orm.go`: added `touchORMOutputs` and a new
  pipeline step `stepTouchORMOutputs` (registered after `stepRehashTracked`
  so it runs after goimports) that bumps every `gen/db/**/**.pb.orm.go`
  mtime to `time.Now()`. The lint now stays quiet for no-op regens while
  still catching real "ran buf without forge generate" drift.


- ~~**(2026-04-30 LLM-port) `requestIDInterceptor` panics on every error
  response — typed-nil bug masquerading as a 500 panic. ✅ Fixed
  in-session.** `forge/pkg/observe/interceptors.go::WrapUnary` did
  `if resp != nil { resp.Header().Set(...) }`. When the inner handler
  returns an error, connect returns a typed `*Response[T](nil)` boxed in
  the `connect.AnyResponse` interface. The interface is non-nil but the
  underlying pointer is nil, so `resp.Header()` nil-derefs in
  `connect@v1.19.0/connect.go:265`. Result: every error response (every
  unimplemented RPC, every Unauthenticated, every NotFound, etc.) was
  caught by the recoveryInterceptor and converted into HTTP 500 +
  `{"code":"internal","message":"panic: runtime error: invalid memory
  address or nil pointer dereference"}` — totally hiding the real error
  code from clients. Reproduced against control-plane-next's
  `AdminServerService.List`/`Get` (both unimplemented stubs). Patched in
  both `forge/pkg/observe/interceptors.go` and the vendored
  `control-plane-next/.forge-pkg/observe/interceptors.go`: gate the
  header-write on `err == nil && resp != nil` so we never reach for
  Header() on a typed-nil response. After the fix, the same RPCs return
  HTTP 501 + `{"code":"unimplemented","message":"..."}` — the real
  Connect error envelope.

- ~~**(2026-04-30 LLM-port) `task dev-up` local-dev flow broken by 3
  forge issues — operator panic, service-filter nil-deref, and a
  missing dev-mode NATS gracefully-degrade story. Validated against
  control-plane-next, which uses the canonical scaffold.**
  `task dev-up` brings up postgres + nats-less + lgtm + alloy + the
  app container cleanly. Air builds and runs `./tmp/control-plane-next
  server`. The DB connects, migrations report `current_version=2
  dirty=false`, NATS gracefully degrades. Then the binary exits 1
  before serving traffic. Three layered bugs:
  1. ~~**`bootstrap.go.tmpl` `RunOperators` uses
     `ctrl.GetConfigOrDie()`...**~~ **Fixed in-session.** Template now
     calls `ctrl.GetConfig()` (non-Die) and returns nil after a
     logged WARN when kubeconfig isn't reachable. Operators gracefully
     degrade in dev exactly the way NATS already does. The warning
     surfaces clearly in stderr so the user knows operators are off,
     not silently broken. Verified end-to-end: `task dev-up` →
     `forge run` → `/healthz` returns 200 without a cluster present.
  2. ~~**`server <svc>` filtered-mode panics in `Setup`.**~~ **Fixed
     in-session — root cause was `BinaryShared` shared-mode bootstrap
     gating CONSTRUCTION as well as registration on `nameSet`.** The
     fix unifies the two binary modes' construction behavior: services
     are always constructed (cheap struct allocation), only mux
     registration is gated by the name filter. So `app.Services.X` is
     non-nil for every service regardless of which one cobra
     subcommand picked, and `ApplyDeps` is always safe in setup.go.
     Where: `internal/templates/project/bootstrap.go.tmpl` shared-mode
     branch — moved the `if runAll || nameSet[...]` gate to wrap only
     the Register/RegisterHTTP/Opts plumbing.
  3. **Generated migration content collides with baseline schema.**
     `db/migrations/00002_audit_log.up.sql` was generated against the
     proto entities with no schema qualifier, so it runs `CREATE TABLE
     audit_log (...)` against postgres's default `search_path`
     (`"$user", public`). The `00001_baseline.up.sql` (a pg_dump of the
     v0 cluster) creates schema `controlplane` and `controlplane.audit_logs`
     (plural). Result: SQLSTATE 3F000 ("no schema has been selected to
     create in") with `auto-migrate` enabled, leaving `schema_migrations
     dirty=true`. Out-of-scope for forge directly (the migration is a
     hand-written .sql), but forge's migration scaffolder should warn
     when proto-generated entities collide with a baseline pg_dump that
     uses a non-`public` schema. The forge `db` skill should also call
     out the multi-schema gotcha (`SET search_path` at top of every
     non-baseline migration when baseline is a pg_dump).
- ~~**(2026-04-30 LLM-port) Real k3d deploy surfaced 5 forge bugs that
  bricked end-to-end pods.** The deploy story compiles cleanly per
  `forge deploy --dry-run`, but actually applying to a cluster
  surfaces a chain of issues:
  1. **KCL output is `manifests:` wrapper, not document stream.**
     `kcl run main.k` emits `manifests:\n- apiVersion: ...\n- ...` which
     `kubectl apply -f -` rejects with "apiVersion not set, kind not
     set". **Fixed in-session**: forge's deploy.go now parses KCL
     output as YAML, extracts the `manifests` list, and re-emits as a
     `---`-separated document stream. Required marking ALL other
     top-level KCL vars as private (underscore prefix) so they don't
     pollute the wrapper. Templates updated for dev/staging/prod and
     for both per-service + shared-binary modes.
  2. **Binary path mismatch. ✅ Fixed in-session.** All 3 envs of
     `main-shared.k.tmpl` now emit `/app/{{.ProjectName}}` matching the
     Dockerfile production stage. control-plane-next mirror updated.
  3. **Cobra service name kebab vs snake mismatch. ✅ Fixed in-session.**
     `internal/codegen/generator.go::GenerateBootstrap` was setting
     `BootstrapServiceData.Name = pkg` (snake-case package name). Changed
     to derive the kebab form from the package: `runtimeName :=
     strings.ReplaceAll(pkg, "_", "-")`. Bootstrap's `known` map and
     `nameSet` lookups now match what cobra subcommands pass to
     `runServer`. Verified end-to-end: control-plane-next pods boot,
     register handlers on mux, and only fail at the
     postgres-not-deployed step.
  4. **Local registry hostname mismatch between docker push (`localhost:5050`)
     and k3d cluster pull. ✅ Fixed in-session (2026-05-06).** Docker
     treats `localhost`/`127.x` as insecure-by-default but
     `registry.localhost` (the hostname k3d uses internally) as secure
     → HTTPS. The image pushes to `localhost:5050` cleanly but cluster
     pulls fail because the mirror config in `registries.yaml` only
     catches `registry.localhost:5000` and `registry.localhost:5050`.
     Forge-managed k3d clusters now get the canonical mirrors
     (`localhost:5050 → registry.localhost:5000`,
     `registry.localhost:5050 → registry.localhost:5000`,
     `registry.localhost:5000 → registry.localhost:5000`) baked in at
     create time via two paths:
       a. Project-templated `deploy/k3d.yaml` carries a
          `registries.config: |` block with the mirror YAML inline
          (k3d Simple-config feature). See
          `internal/templates/deploy/k3d.yaml.tmpl`.
       b. The fallback `forge deploy dev` cluster-create (no
          `deploy/k3d.yaml` on disk) writes a temp `registries.yaml`
          and passes `--registry-config` to `k3d cluster create`. See
          `internal/cli/deploy.go::ensureDevCluster` +
          `writeFallbackRegistriesYAML`.
     Pre-existing clusters (forge wasn't there at create time) need a
     manual containerd-config fix; the deploy skill SKILL.md now
     documents the `docker exec ... /etc/rancher/k3s/registries.yaml`
     hot-patch and the simpler `k3d cluster delete dev && forge deploy
     dev` recreate path.
  5. **`forge deploy` rollout watcher uses unprefixed deployment names. ✅
     Fixed in-session.** Replaced the `cfg.Services` loop with a
     `kubectl get deployments -l app.kubernetes.io/managed-by=forge -o
     jsonpath` discovery so the watcher follows the actually-applied
     Deployment names regardless of scaffold mode (per-service `<svc>`,
     shared-binary `<project>-<svc>`, operator/worker variants, or pack
     additions).

- ~~**(2026-04-30 LLM-port) Descriptor + frontend TS gen hard-code the
  proto subdir list, hiding pack-emitted services.** `runDescriptorGenerate`
  (forge/internal/cli/generate_orm.go) and `runBufGenerate` (generate_buf.go)
  iterated only `proto/{services,api,db,config}`, so any pack that emits
  protos under a different namespace (e.g. audit-log → proto/audit/, api-key
  → proto/api_key/) silently disappeared from forge_descriptor.json AND from
  the TypeScript stubs. Downstream this manifested as "no useListAuditEvents
  hook generated despite the proto compiling and the Go service shape
  existing." Replaced both with a `discoverProtoSubdirs(projectDir)` walk
  in this session. Worth confirming there are no other call sites still
  using the hard-coded list (generate_buf.go for backend Go gen has the
  full proto/ tree already; the leak was in the frontend / descriptor
  paths only).

- ~~**(2026-05-06 LLM-port) Codegen mock drift is recurring and silent
  until tests run.**~~ **Closed (2026-05-06, this session — see "CI
  verify-generate gate now ships across all project kinds + Taskfile
  parity" under Fixed in-session.)** The path-of-least-resistance
  fix landed: `task verify-generate` is now wired into the generated
  CI workflow for every project kind (service, CLI, library), and the
  CLI/library Taskfiles now carry the matching `verify-generate` task
  so local-dev parity is preserved. Drift surfaces at PR-CI time
  instead of in unrelated test failures. The deeper "regenerate
  mock_gen.go from contract on every forge generate" half is still
  worth doing as a follow-up but is no longer load-bearing for
  catching the recurring symptom.

- ~~**(2026-04-30 LLM-port) Pack install is not idempotent for migrations
  even when the project lists the pack.**~~ **Closed (2026-05-06, this
  session — see "Pack install is now idempotent + collision-aware" under
  Fixed in-session.)**

- ~~**(2026-04-30 LLM-port) Pack handlers and forge service scaffolds
  collide when the pack proto declares a service.**~~ **Closed (2026-05-06,
  this session — see "Pack install is now idempotent + collision-aware"
  under Fixed in-session.)**

- ~~**(2026-04-30 LLM-port) Frontend mock transport is a guaranteed
  build-time prerender failure when `.env.local` ships
  `NEXT_PUBLIC_MOCK_API=true` for projects with no entity-CRUD RPCs.**~~
  **Closed (2026-05-06, this session — see "Frontend mock-transport stub
  no longer crashes prerender; stub-emit path always runs" under Fixed
  in-session.)**


- ~~**(2026-04-30 LLM-port) `forge deploy --dry-run` KCL output missing
  ConfigMap and Secret resources.**~~ **Closed (2026-04-30, this session
  — see "deploy/kcl ConfigMap codegen + projection" under Fixed
  in-session.)**

- ~~**(2026-04-30 LLM-port) `task lint` hard-codes `golangci-lint` instead
  of delegating to `forge lint`.**~~ **Closed (2026-05-06, this session
  — see "Webhook codegen log line + RegisterWebhookRoutes auto-wire +
  Taskfile lint delegation" under Fixed in-session.)**

- ~~**(2026-04-30 LLM-port) Dev-mode docker container can't see local
  forge/pkg `replace` target.**~~ **Closed (2026-05-06, this session —
  see "Local forge/pkg replace auto-vendored on `forge generate`" under
  Fixed in-session.)**

- **(2026-04-30 reliant-discovery) Bidirectional gRPC service template
  missing.** Forge `services` skill has unary and server-streaming
  shapes; no template for `stream In returns stream Out` services, and
  none for the "direction-reversed" shape where the server-side of the
  RPC is the daemon listening (reliant's `ConnectGateway` is a
  server-streaming RPC where the gateway-side server is actually the
  daemon process). When forge is asked to scaffold tools_daemon-style
  services, it can't.

- **(2026-04-30 reliant-discovery) No "stream → React Query cache
  bridge" pattern in `frontend/state` skill.** Real apps that ingest
  primary server data via long-lived server-streams (reliant's
  `StreamUserUpdates` fans 21 distinct event types out to N domain
  caches) have no canonical place to put the dispatcher. Suggested
  addition: a `createStreamCacheBridge(client, dispatcherMap)` helper
  that maps stream events to `queryClient.setQueryData(...)` /
  `queryClient.invalidateQueries(...)` invocations, plus a
  frontend/state skill section explaining when to reach for it. Without
  this, every project re-invents the pattern (reliant's
  `globalUpdatesStore` is exhibit A).

- **(2026-04-30 reliant-discovery) `forge.yaml` lacks an "entities
  disabled" / "ORM-off" switch.** Projects using sqlc + hand-written
  migrations (reliant) don't want forge looking at proto entity
  annotations (`(forge.v1.field) = { pk: true }` etc.). Today the
  workaround is to not write the annotations and rely on lint staying
  quiet, but there's no affirmative declaration. Suggested
  `forge.yaml`: `entity_codegen: disabled` (or similar).

- **(2026-04-30 reliant-discovery) CommandRegistry / dynamic
  command-envelope pattern undocumented.** Reliant's
  `DaemonCommandRequest { string command_type; bytes payload; }` lets
  the daemon add new commands without proto/oneof/router changes —
  handlers register at runtime keyed by `command_type`. Useful
  extensibility pattern; forge has no skill page or template for it.

- **(2026-04-30 reliant-discovery) NATS pack missing.** Both reliant
  (`internal/natsutil/`) and control-plane-next (`internal/natsio/`)
  hand-roll a NATS client wrapper. `forge pack add nats` could emit
  client + JetStream config + OTel-wrapped publisher + contract.go
  and serve both projects plus future event-driven multi-service work.

- **(2026-04-30 reliant-discovery) Temporal pack missing.** Reliant
  uses Temporal externally (cloud) AND embedded (monolith). Any
  project with a workflow engine re-implements activity registration
  / worker boot. External-Temporal pack is high-value; embedded-
  Temporal is more exotic (skill-page material).

- **(2026-04-30 reliant-discovery) Multi-binary Cobra-subcommand mode
  for `kind: service`.** Reliant ships ONE Go binary that role-switches
  via Cobra subcommand (`reliant server api`, `reliant server worker`,
  etc.). Forge templates assume one `cmd/<svc>/main.go` per service.
  Suggested `forge.yaml` knob: `binary_mode: subcommand` emits a single
  `cmd/<project>/main.go` with Cobra dispatch into per-service `Run`
  functions, while still letting forge generate per-service
  `pkg/app/setup.go` wiring.

- **(2026-04-30 reliant-discovery) Electron desktop shell template
  (out-of-scope candidate).** Reliant's monolith mode = Electron +
  embedded Go server + embedded daemon. Not worth templating today,
  but worth flagging as a recurring shape if AI-tooling projects keep
  landing on forge.

- **(2026-04-30 reliant-discovery) `migration_safety` doesn't extend
  to dual-driver (SQLite + Postgres) parity.** Reliant has 181 SQLite
  migrations + 36 Postgres migrations and a custom
  `scripts/db-driver-audit.sh` that detects drift between the two
  trees. Suggested: `migration_safety.dual_driver: error` mode that
  runs the parity check at `forge lint` time.

- **(2026-04-30 reliant-discovery) Per-handler-file size guidance +
  `forge add handler-file`.** Reliant's `internal/grpc/services/chat.go`
  is **4,331 LOC**, worktree.go 1,767, settings.go 1,457, workflow.go
  1,413. Forge could (a) lint-warn at >1000 LOC per file inside
  `handlers/<svc>/`, (b) provide `forge add handler-file <svc> <name>`
  to scaffold a new method file in an existing handler dir.

- **(2026-04-30 reliant-discovery) `forge lint --frontend-stores`
  rule for "server data in Zustand."** The `frontend/state` skill has
  the rule; lint doesn't enforce it. ~20 of reliant's 40 stores would
  flag. Heuristic: any `web/src/store/*.ts` that imports from
  `gen/.*-grpc` AND defines a `create<…>` Zustand store is suspect.

- **(2026-04-30 reliant-discovery) `forge migrate-service` command
  for flat-file → `handlers/<svc>/` porting.** Reliant has 26 flat
  handler files in `internal/grpc/services/<name>.go`, several over
  1,000 LOC. A `forge migrate-service <name>` that splits the file
  into the canonical `handlers/<svc>/{service.go, handlers_gen.go,
  <rpc>.go, ...}` shape (one method file per RPC group) would
  massively reduce migration risk for any project in this flat-file
  shape.

- ~~**(2026-04-30 LLM-port) `forge generate` REWRITES hand-written
  `handlers/<svc>/service.go` files back to scaffold...**~~
  **Misdiagnosis. Closed (2026-04-30, this session).** Empirical re-test
  with a snapshot diff: `forge generate` does NOT touch existing
  `handlers/<svc>/service.go`. `generateServiceStubs` only calls
  `GenerateServiceStub` (full-file emit) when the service directory
  doesn't exist; for existing dirs it calls
  `GenerateMissingHandlerStubs` which only adds new RPC stubs to
  `handlers_gen.go`. The actual cause of the destruction the
  setup.go-wiring agent saw was the parallel-running codemod agent
  executing `git checkout -- handlers/` mid-task, which reverted the
  setup.go agent's uncommitted handler ports. **Root cause:
  inter-agent file collision, not a forge bug.** Lesson for orchestration:
  agents working on overlapping file paths must commit-and-rebase
  rather than run destructive git operations on a shared tree. The
  setup.go-style agents should commit each service's port before the
  codemod-style agents run.

- ~~**(2026-04-30 LLM-port) `forge generate` is non-idempotent for
  service-name acronym capitalization.**~~ **Closed (2026-04-30, this
  session) — the polish-agent fix that extended the acronym table
  (LLM/JWT/IO) made forge's casing consistent across all codegen
  paths. Empirical re-verification: two consecutive `forge generate`
  runs on control-plane-next produce byte-identical `pkg/app/{bootstrap,
  testing}.go`. The hand-written `setup.go` and tests now reference
  `LLMGateway` (the proto-canonical form) and stay valid across
  regen.**

- ~~**(2026-04-30 LLM-port) `pkg/app/setup.go` runs AFTER mux
  registration, so reassigning `app.Services.X` cannot swap
  dispatch.**~~ **Closed (2026-04-30, this session). Picked option (b)
  from the original sketch:** every generated `handlers/<svc>/service.go`
  now ships an exported `ApplyDeps(deps Deps)` method that mutates the
  *Service pointer's `deps` field in-place. Connect's bound-method
  capture at Register time is fine — the mux holds the same pointer
  whose deps now point at the rich production values. The setup.go
  scaffold was rewritten to model the canonical pattern:
  ```go
  app.Services.Foo.ApplyDeps(foo.Deps{
      Logger:     ...,
      Config:     ...,
      Authorizer: ...,
      DB:         db,
      StripeClient: stripeClient,
  })
  ```
  Verified end-to-end: control-plane-next's setup.go now uses ApplyDeps
  for all 7 rich-Deps services (admin_server and workspace_proxy stay
  bare-Deps); triple-gate clean; fresh `forge new` smoke-tested.
  **Where:**
  `internal/templates/service/service.go.tmpl` (added ApplyDeps method);
  `internal/templates/project/setup.go.tmpl` (rewrote pattern docs).
  Existing forge projects need a one-time addition of the ApplyDeps
  method to each handler service.go (forge upgrade should pick this up
  via the existing template-drift mechanism on next regen).

- ~~**(2026-04-30 LLM-port) `forge generate` log line for webhook routes uses kebab-case service name in path, but file lands at snake_case.**~~
  **Closed (2026-05-06, this session — see "Webhook codegen log line + RegisterWebhookRoutes auto-wire + Taskfile lint delegation" under Fixed in-session.)**

- ~~**(2026-04-30 LLM-port) Webhook codegen emits `RegisterWebhookRoutes`, but the user-owned `RegisterHTTP` stub doesn't call it.**~~
  **Closed (2026-05-06, this session — see "Webhook codegen log line + RegisterWebhookRoutes auto-wire + Taskfile lint delegation" under Fixed in-session.)**

- ~~**(2026-04-30 LLM-port) Frontend page templates still use hand-rolled `<table>` markup despite the `Table` primitive shipping in scaffolds.**~~
  **Closed (2026-05-06, this session — see "List-page template now uses
  Table primitive + URL-backed search" under Fixed in-session.)**

- ~~**(2026-04-30 LLM-port) Scaffolded pages put filter/search state in Zustand instead of URL search params.**~~
  **Closed (2026-05-06, this session — see "List-page template now uses
  Table primitive + URL-backed search" under Fixed in-session.)**

- ~~**(2026-04-30 LLM-port) Scaffolded `handlers_test.go` calls wrong helper name when service name collides with internal package.**~~
  **Closed (2026-05-06, this session — see "Test scaffolds now consult
  the same alias-collision rule as bootstrap testing" under Fixed
  in-session.)**

- ~~**(2026-04-30 LLM-port) `forge add operator` scaffold collides with ported user code on a 2-file shape.**~~
  **Closed (2026-05-06, this session — see "`forge add operator`
  detects existing controller-shape and skips operator.go" under
  Fixed in-session.)**

- ~~**(2026-04-30 LLM-port) Bulk-port hits "exclude" config gap.**~~
  **Closed (2026-05-06, this session — see "`forge lint
  --suggest-excludes` emits a YAML snippet of contracts.exclude
  candidates" under Fixed in-session.)**

- ~~**(2026-04-30 LLM-port) `proto/db/v1/` files don't trigger ORM regeneration without a sister `forge generate`.**~~
  **Closed (2026-05-06, this session — see "proto skill clarifies
  forge generate is canonical; lint warns on .pb.go without .pb.orm.go
  sibling" under Fixed in-session.)**

- ~~**(2026-04-30 LLM-port) Default values aren't projected to KCL config_gen.k.**~~
  **Closed wontfix (2026-05-06).** Confirmed intentional: defaults
  are the binary's contract (applied by `pkg/config.Load()` at
  startup), not the manifest's. The right operator-visibility fix is
  documentation, not always-emit codegen. Two doc updates landed:
  - `deploy` SKILL.md gained a "Per-env config rendering
    (`config_gen.k`)" section explaining (a) defaults don't appear in
    the rendered manifest by design, (b) empty categories are elided,
    (c) sensitive fields always emit. Operators who want a default
    visible in the rendered Deployment add the value to
    `environments[<env>].config` — the explicit override IS the
    "I want this in the manifest" signal.
  - The `v0.x-to-env-config` migration skill was already accurate
    about the projection rules; no change needed there.
  Considered an opt-in `(forge.v1.config) = { always_emit: true }`
  annotation — rejected as overkill for a doc-shaped concern.

- ~~**(2026-04-30 LLM-port) Per-env empty-category lists land empty-but-present.**~~
  **Fixed in-session (2026-05-06).** `renderDeployConfigKCL` now
  buffers each category's entries before emitting the
  `<CAT>_ENV: [schema.EnvVar] = [...]` list and skips categories whose
  buffer ends up empty. Categories that DO emit (sensitive fields are
  always emitted; non-sensitive fields with per-env values are
  emitted) appear unchanged. Test:
  `TestGenerateDeployConfig_OmitsEmptyCategoryLists`. Note for
  `main.k` authors: any reference to a `cfg.<CAT>_ENV` that has
  evaporated will KCL-error with "no attribute" — the deploy SKILL.md
  flags this. (The orthogonal "always emit secret_ref for sensitive
  fields" point in the original report was already true; that part of
  the entry was a misread, not a bug.)

## Older items below.



- ~~**LLM-pain-points surfaced during control-plane-next port (2026-04-30).**~~
  **Closed (2026-05-06, this session — all 8 sub-items now resolved.)**
  Each was a real friction point an LLM porting the project hit trying
  to use forge as documented. Compound entry kept for history; per-item
  status:
  1. ~~**`forge pack add` vs `forge pack install` inconsistency.**~~
     **Closed (2026-04-30, this session — see "`forge pack add`/`remove`
     aliases" under Fixed in-session.)**
  2. ~~**Pack install fails on tidy when proto isn't yet generated.**~~
     **Closed (2026-04-30, this session — see "Pack install skips tidy
     when proto files added" under Fixed in-session.)**
  3. ~~**Local forge/pkg replace is manual.**~~ **Closed (2026-05-06,
     this session — see "Local forge/pkg replace auto-vendored on
     `forge generate`" under Fixed in-session.)**
  4. ~~**No `forge db squash`.**~~ **Closed (2026-05-06, this session
     — see "`forge db squash` ships as a first-class subcommand" under
     Fixed in-session.)**
  5. ~~**Scaffold's placeholder `0001_init.up.sql` clashes with brought-in
     baseline.**~~ **Closed (2026-04-30, this session — see "Scaffold
     migration init now 5-digit + skipped on existing baseline" under
     Fixed in-session.)**
  6. ~~**No `forge pack info <name>`.**~~ **Closed (2026-05-06, this
     session — see "`forge pack info` summarizes manifest + flags
     project conflicts" under Fixed in-session.)**
  7. ~~**`forge starter add` doesn't `tidy` after copy.**~~ **Closed
     (2026-05-06, this session — see "`forge starter add` runs `go mod
     tidy` after copy" under Fixed in-session.)**
  8. ~~**No `forge config set --env <env> <key> <value>`.**~~ **Closed
     (2026-05-06, this session — see "`forge config set` edits
     environments[<env>].config programmatically" under Fixed
     in-session.)**

- ~~**Service.go scaffolder camel-cases ignoring proto acronym
  conventions.**~~ **Closed (2026-04-30, this session — see "Scaffolder
  acronym list extended (LLM, JWT, IO)" under Fixed in-session.)**

- **Convention proposal: `internal/service/<name>/` interactor pattern.**
  control-plane-next-v0 used this organically (`internal/service/{audit,
  authutil, freetier, plansync, proxysession}`) for multi-step business
  flows (create user → assign org → audit → notify). Today handlers can
  inline that logic OR call internal packages — both work, neither is
  the "convention." Formalizing this layer as the canonical home for
  multi-step orchestration would: (a) keep handlers thin (auth +
  validate + delegate to interactor), (b) give a clear seam for
  transactional boundaries, (c) make tests easier (mock the interactor
  in handler tests, mock repos in interactor tests). Could ship as
  `forge add interactor <name>` with a contract.go + tests scaffold,
  paralleling the existing `forge package new`.

- **Convention proposal: `internal/adapters/<external>/` adapter
  pattern.** Today external clients (stripe, clerk, firebase, litellm,
  twilio) get scaffolded directly in `pkg/clients/<name>/` (via packs
  or starters). Adding a project-side adapter layer — `internal/
  adapters/<name>/` with a `contract.go` defining a domain-shaped
  interface, and the pack/starter providing the concrete implementation
  — gives test seams that don't require mocking the third-party SDK.
  Example: `internal/adapters/billing/` defines
  `Billing interface { CreateCheckout(ctx, customer) (URL, error) }`;
  the stripe starter provides `*StripeBilling`; tests use a `MockBilling`.
  This is what `pkg/contractkit` enables today; codifying it as
  `forge add adapter <name> --client <pack-or-starter-name>` makes the
  pattern discoverable.

- ~~**goimports reformats Tier-2 Go files after checksum is recorded.**~~
  **Closed (2026-04-30, this session — see "Re-record frozen checksums
  after `bootstrapGeneratedCode`" + "Codegen pipeline now checksums its
  own outputs" under Fixed in-session.)**

- ~~**Issue:** Multi-service-per-proto-file template gap.~~ **Closed
  (2026-04-30, this session) as wontfix-by-convention.** The canonical
  forge convention is one-proto-service-per-file, and the convention
  is lint-enforced via `forgeconv-one-service-per-file`. Migrations
  with inherited multi-service protos run a proto split as part of
  the port (per `MIGRATION_TIPS.md`). The remaining sketch fix
  (ProtoServices schema + multi-`Unimplemented*Handler` embed) would
  re-introduce a configuration knob that the convention removes. The
  user accepted this direction in past sessions ("per-pack subpackages
  prevent collision by construction; no matrix system"). Same
  reasoning applies here: the lint enforces the canonical shape; users
  who can't split inherit a one-time refactor cost rather than a
  forever-configuration cost.
  - **Status (continuation 4):** Dedupe of Connect imports in
    bootstrap_testing.go landed (`BootstrapTestServiceData` now carries
    `ProtoConnectImportPath` + `ProtoConnectPkg` derived from
    descriptor instead of guessed from service name). The remaining
    work — supporting one Go service struct that registers N proto
    services from one go_package — would require service.go template
    changes (multi-`Unimplemented*Handler` embed, multi-`Register`
    call), `forge.yaml` schema for `proto_services: []string`, and
    handler-stub generator updates. See
    `/home/sean/src/reliant-labs/DESIGN_DECISIONS_PENDING.md`
    decision 4 for the design tradeoffs.
  - **Severity:** moderate — for in-the-wild forge usage (where the
    user controls proto layout) the per-file-per-service convention
    works. For migration-of-existing-projects (where the proto layout
    is inherited), this either forces a manual proto split or a
    forge-side multi-binding refactor.
  - **Where:** `internal/codegen/generator.go:GenerateBootstrap`,
    `GenerateBootstrapTesting`, `GenerateServiceStub`; templates
    `internal/templates/project/bootstrap.go.tmpl`,
    `bootstrap_testing.go.tmpl`,
    `internal/templates/service/service.go.tmpl`.
  - **Sketch fix (remaining):** add `ServiceConfig.ProtoServices
    []string` (already declared as `proto_packages: []string` but
    unused). When set, `service.go.tmpl` embeds N
    `Unimplemented<X>Handler`s and `Register()` calls
    `New<X>Handler(s, opts...)` N times. Bootstrap dedupes services
    by handler-binding name (forge.yaml service `Name`) rather than
    proto service name.

- ~~**Issue:** Internal-package contracts must use the names `Service`,
  `Deps`, and `New(Deps) Service` exactly~~ **Closed (2026-04-30,
  this session — see "Fixed in-session" below for the closing entry.)**

- ~~**Issue:** `binary: shared` codegen mode (Layer B of the
  MultiServiceApplication dispatch) is **deferred**.~~ **Closed
  (2026-04-30, this session — see "Fixed in-session" below for the
  closing entry.)**

- ~~**`forge upgrade` does not branch by project kind for Taskfile.yml**~~
  **Closed (2026-04-30, this session — see "`forge upgrade` Taskfile.yml
  + managed-file plan now kind-aware" under Fixed in-session.)**

- ~~**`forge generate`'s `go mod tidy` step fails on workspace-mode
  projects.**~~ **Closed (2026-04-30, this session) as not-actually-broken.**
  Empirical re-test in workspace mode (forge's own go.work + in-tree
  `./pkg/`) showed `go mod tidy` succeeds — it adds the missing
  `require` directive automatically when given a `replace` target.
  Attempted "skip tidy in workspace mode" + "use `go work sync`
  instead" alternatives both broke
  `TestBootstrapGeneratedCodeRunsGeneratePipelineInProjectDirectory`
  because `go work sync` does not add require directives; the build
  then fails with "module ... is replaced but not required." The
  original report appears to have described a different, transient
  condition. Reverted; tidy stays as-is.

## Fixed in-session

- **(2026-05-06 cpnext continuation 8) Auto-installed Go toolchain
  missing `covdata` tool surfaces as a confusing `task coverage`
  failure with no breadcrumb.** ✅ Fixed by adding `CheckCovdata` to
  the standard doctor check set (`internal/doctor/toolchain.go`,
  wired into both `RunStandard` and `RunFiltered`'s default branch).
  Probes `go tool covdata help` and warns (not fails) with an install
  hint (`go install golang.org/x/tools/cmd/covdata@latest`) when the
  active toolchain ships without the tool. Warn-not-fail rationale:
  most projects don't trip it because the scaffolded `task coverage`
  recipe drops `-covermode=atomic` for exactly this reason; the warn
  fires for projects that opt into atomic-mode coverage and would
  otherwise hit the confusing failure mode.

- **(2026-05-08 polish-round) `[banner-unclassified]` warning on
  `internal/templates/project/app_extras.go.tmpl`.** ✅ Fixed in
  `forge/internal/linter/scaffolds/banners.go`: bumped the
  content-first head window from 30 → 60 lines (so a long doc-comment
  preamble doesn't push the marker out of scope), added `//forge:allow`
  as a content-first Tier-3 override (it's the canonical user-owned
  marker and was already documented as such), and added `app_extras.go`
  to the Tier-3 known-name list as defense-in-depth.

- **(2026-05-08 polish-round) `forge add frontend` did not flip
  `features.frontend: true` in forge.yaml.** ✅ Fixed in
  `forge/internal/cli/add.go::runAddFrontend`: after appending the
  frontend to `cfg.Frontends`, set `cfg.Features.Frontend = &true`
  before persisting. Projects scaffolded with `forge new --kind
  service` (no `--frontend`) can now run `forge add frontend admin`
  and have subsequent `forge generate` runs pick up frontend codegen.

- **(2026-05-08 polish-round) `contract_test.go.tmpl` rendered
  single-result `pkg.New(pkg.Deps{})` even after users polished `New`
  to the canonical two-result `(Service, error)` shape.** ✅ Updated
  both `internal/templates/internal-package/service.go.tmpl` (now
  emits `func New(deps Deps) (Service, error)`) and
  `contract_test.go.tmpl` (now emits `svc, err := pkg.New(pkg.Deps{});
  if err != nil { t.Fatalf(...) }`). Updated the
  `forge generate` auto-scaffold gate in
  `internal/cli/generate_middleware.go` to skip when the existing
  package's `New` is still single-result (with breadcrumb pointing at
  the polish), preventing a non-compiling auto-scaffold for legacy
  packages. Test assertion in `package_test.go` updated.

- **(2026-05-08 polish-round) `forgeconv-interactor-deps-are-interfaces`
  fired on primitive-shaped config DATA (`[]string`,
  `map[string]int`, etc.).** ✅ Refined the rule in
  `forge/internal/linter/forgeconv/interactor_deps_are_interfaces.go`:
  added `isPrimitiveConfigShape` helper that skips slice/map types
  whose element (and key, for maps) is a Go built-in primitive.
  Rationale: these have no meaningful interface equivalent — hiding
  `[]string` behind an interface adds friction without unlocking test
  power. The rule still fires on concrete struct pointers / concrete
  selector types (real foot-guns).

- **(2026-05-07 cpnext D2) wire_gen emits `<stringField>: nil` for
  string-typed Deps fields it cannot resolve.** ✅ Fixed by
  `zeroValueLiteral()` in `forge/internal/codegen/wire_gen.go` —
  scalar Deps types now emit their typed zero (`""`, `0`, `false`,
  `0` for `time.Duration`); pointer/interface/slice/map/chan/func
  fall through to `nil`.

- **(2026-05-07 cpnext D2) wire_gen `wireSvcBillingDeps` (bootstrap)
  vs `wireBillingDeps` (wire_gen.go) name-collision mismatch.** ✅
  Fixed by `ResolveCollisionNaming` in
  `forge/internal/codegen/generator.go` and the shared
  `CollisionCounts` call now used by both
  `GenerateWireGen` (`internal/codegen/wire_gen.go`) and
  `GenerateBootstrap` (`internal/codegen/generator.go`) — both
  emitters derive identical `FieldName`/`alias` tuples.

- **(2026-05-07 cpnext D2) wire_gen auto-resolves Deps.Repo to
  app.Repo (typed db.Repository) but each service has its own narrow
  Repository interface.** ✅ Resolved by the AppExtras-holds-concrete
  convention: per-service narrow Repository fields land on
  `pkg/app/AppExtras` and wire_gen resolves by exact-name match.
  Documented in cpnext's `DESIGN_DECISIONS_PENDING.md` (Option A
  shipped) and in `pkg/app/CONVENTIONS.md`.

- **(2026-05-07 cpnext integration) `forge generate` overwrites
  hand-curated `frontends/admin-web/src/components/nav.tsx` and
  `frontends/admin-web/src/app/page.tsx`.** ✅ Fixed by the round-2
  `nav_gen.tsx` + user-owned `nav.tsx` split + checksum guard. The
  generated component lives next to the user-owned wrapper and the
  scaffold writer skips overwrite when checksums show user edits.

- **(2026-05-07 cpnext integration) Internal-package
  `validateDeps()` enforcing setup-time deps breaks Bootstrap.** ✅
  Resolved by the wire_gen pattern unifying construction. Internal
  packages now follow the same wire_gen flow as handlers — Setup
  populates `*App` fields, wire_gen assembles the full Deps before
  `New()` runs, and validateDeps gates the complete set at startup.

- **(2026-05-07 cpnext integration) Bootstrap two-phase init prevents
  Day-5 `validateDeps` eliminating per-RPC nil-checks.** ✅ Resolved
  by the wire_gen pattern (`forge/internal/codegen/wire_gen.go` +
  `forge/internal/templates/project/wire_gen.go.tmpl`). Bootstrap
  calls `wireXxxDeps(app, cfg)` to assemble the full Deps and passes
  it into `X.New(deps)`. The cpnext re-cut validated that wire_gen
  eliminates 70 of 79 per-RPC nil-checks (88%); the remaining 9 are
  legitimate guards on optional deps and now have a first-class
  `// forge:optional-dep` marker (this session) to communicate intent.

- **Pack-install half-state recovery + tidy-deferral on stale gen/ (2026-05-06, cpnext fresh-cut dogfood).**
  Repro: `forge pack add audit-log` (emits .proto, defers tidy) then
  `forge pack add api-key` (no .proto, runs tidy) — tidy fails because
  audit-log's Go templates import `gen/audit/v1` which only exists after
  `forge generate`. The api-key install errored after writing files but
  before recording the pack in `forge.yaml`, leaving the project in a
  half-installed state where retry tripped the fresh-install collision
  check on the very files the pack itself had just written. Same shape
  trips firebase-auth and nats. Two structural fixes:
  - **`internal/packs/pack.go` reorders `cfg.Packs` mutation BEFORE
    `go get`/`go mod tidy`**, and **`internal/cli/pack.go` persists
    `forge.yaml` even when `InstallWithConfig` returns an error**. After
    files+migrations land on disk the pack is morally installed; tidy
    failure is a post-action recoverable by `forge generate`. Disk and
    config now stay in sync, so retry runs as resync (not fresh).
  - **New `installedPacksWithUnrenderedProto` guard** scans installed
    packs for `.proto` outputs whose `gen/<ns>/<version>/` directory is
    absent. When any are pending, the current pack install skips tidy
    with a one-line "run forge generate after this pack-cluster install"
    hint. Prevents the cascade entirely on a fresh-cut project.
- **`forge add webhook` test scaffold updated for `New(deps Deps) (*Service, error)` signature (2026-05-06, cpnext dogfood).**
  `templates/webhook/webhooks_test.go.tmpl` was still using
  `svc := New(Deps{...})` — the single-return form from before the
  Day 3 svcerr changes. Now uses `svc, err := New(Deps{...})` and
  wires `Authorizer: NewAuthorizer()` (required by `validateDeps`).

- **`forge add package --type={service|adapter|interactor}` + two new architecture skills + two new lint rules (2026-05-06 polish-phase, Day 8).**
  Hexagonal-architecture vocabulary is now first-class in the scaffolder.
  `forge add package <name> --type=adapter` emits a four-file outbound-
  boundary scaffold (`contract.go` with `// forge:adapter` marker,
  `adapter.go` with Service/Deps/New + httptest-friendly HTTPClient
  injection, `adapter_test.go` with httptest stubs against a controlled
  downstream). `--type=interactor` emits a use-case orchestrator
  scaffold (`contract.go` with `// forge:interactor` marker plus
  example dependency interfaces `Source`/`Sink`, `interactor.go`
  composing both deps in a `Run` method, `interactor_test.go` with
  hand-rolled fakes asserting composition order and short-circuit
  behavior). `--type=service` (default) preserves today's behavior.
  - **Skills.** `internal/templates/project/skills/forge/adapter/SKILL.md`
    and `.../interactor/SKILL.md` ship as peers of the existing
    `service-layer`, `contracts`, and `api/handlers` skills. Cross-
    linked. Auto-discovered by `forge skill list`.
  - **Lint rules.** `forgeconv-adapter-no-rpc` warns when a
    `// forge:adapter`-marked package registers a Connect RPC handler
    (`connect.NewXxxHandler`) — adapters are outbound-only by
    convention, so RPC means it's actually a service.
    `forgeconv-interactor-deps-are-interfaces` warns when an interactor's
    `Deps` struct holds a concrete struct pointer instead of an
    interface — concrete deps defeat the all-mock test surface that
    interactors are designed for. Both wired into
    `runConventionLint` alongside the Day 2-3 agent's
    no-handler-error-mapping rule. Severity warning (false-positive
    risk is real for projects opting out of the marker convention).
    Two fixtures each (firing + clean) under
    `internal/linter/forgeconv/testdata/{adapter_with_rpc,adapter_clean,interactor_concrete_deps,interactor_clean}`.
  - **Marker convention.** `// forge:adapter` and `// forge:interactor`
    are the package-doc directives both rules look for, mirroring the
    existing `// forge:entity` / `// forge:operator-scheme` style
    already used by codegen. Documented identically in both skills so
    the convention is one decision, not two.
  - **Inline forge bug fixed.** `forgeconv-internal-package-contract-names`
    used to require Service+Deps+New all in `contract.go`. The existing
    `--kind=client` scaffold (and the new adapter/interactor types)
    intentionally split Service in contract.go from Deps+New in
    `client.go` / `adapter.go` / `interactor.go`. The lint now scans
    the whole package directory; findings still point at contract.go
    (the canonical anchor). Existing tests pass unchanged because the
    pre-existing bad fixture only had a single contract.go file.
  - **Files.**
    `internal/cli/package.go` (route on `--type`),
    `internal/cli/add.go` (alias picks up `--type` + better long help),
    `internal/config/config.go` (`PackageConfig.Type`),
    `internal/templates/internal-package/{adapter,interactor}/*.tmpl`
    (six new templates),
    `internal/templates/project/skills/forge/{adapter,interactor}/SKILL.md`,
    `internal/linter/forgeconv/{adapter_no_rpc.go,interactor_deps_are_interfaces.go}`
    (+ matching `_test.go` files + four fixture trees),
    `internal/linter/forgeconv/internal_pkg_contract.go` (package-scope
    fix), `internal/cli/lint.go::runConventionLint` (wire two new rules).
  - **Smoke recipe.** `forge new tmp --kind service --service api --mod
    example.com/tmp && cd tmp && forge add package adaptstripe --type=adapter
    && forge add package billingflow --type=interactor && go build
    ./internal/... && forge lint --conventions` — all four steps green.

- **Lifecycle-banner discipline + `forge lint --banners` (2026-05-06 polish-phase, Day 6).**
  Every emitted file now declares its ownership tier via a header banner,
  and a new analyzer catches templates that drop the banner.
  Three tiers, three shapes:
  1. **Tier 1** (regenerated every run, gitignored) — `// Code generated
     by forge generate. DO NOT EDIT.` + `// Source: <relative path>` in
     the first ~30 lines. Comment-syntax-appropriate for `.alloy` (`//`),
     YAML/KCL (`#`), SQL (`--`).
  2. **Tier 2** (one-shot scaffold; user owns after first emit) —
     `// forge:scaffold one-shot — <one-line>. <pointer-to-canonical>`.
     Markdown form: `> **forge:scaffold one-shot** — …`.
  3. **Tier 3** (user-owned skeleton like `setup.go.tmpl`,
     `forge.yaml.tmpl`, `service.go.tmpl`) — banner-less by design;
     existing `//forge:allow` marker is the user-owned signal and is
     enforced by other linters.
  - **Tier-1 banners normalized** (mostly already present, but stragglers
    fixed): `frontend/hooks.ts.tmpl`, `frontend/mocks/mock-data.ts.tmpl`,
    `frontend/mocks/mock-transport.ts.tmpl` (were `AUTO-GENERATED — DO
    NOT EDIT`), `ci/github/proto-breaking.yml.tmpl`,
    `ci/github/dependabot.yml.tmpl`, `project/alloy-config.alloy.tmpl`,
    `project/cmd-server.go.tmpl`, `project/cmd-root.go.tmpl`,
    `project/cmd-version.go.tmpl`, `project/cmd-db.go.tmpl`. Pack
    `_gen`-style files that lacked the `// Source:` line filled in:
    `clerk/clerk_auth_gen.go.tmpl`, `clerk/clerk_dev_auth.go.tmpl`,
    `firebase-auth/firebase_dev_auth.go.tmpl`, `jwt-auth/dev_auth.go.tmpl`.
  - **Tier-2 banners added** to every one-shot scaffold that was
    silently lacking one: all six `internal-package/{service,client,
    eventbus}/*.go.tmpl` files; the four `internal-package/{adapter,
    interactor}/*.go.tmpl` files; all four `frontend/pages/*-page.tsx.tmpl`;
    every pack scaffold (`api-key`, `audit-log`, `clerk`, `firebase-auth`,
    `jwt-auth`, `nats`, `auth-ui`, `data-table`); pack SQL migration up/down
    files (`-- forge:scaffold one-shot —` form); pack barrel exports
    (`auth-ui/index.ts`, `data-table/index.ts`); pack READMEs
    (Markdown italicized form); `audit-log/audit_log.proto`; `ci/github/CODEOWNERS.tmpl`.
  - **New analyzer.** `internal/linter/scaffolds/banners.go` walks every
    `.tmpl` under `internal/templates/` and `internal/packs/*/templates/`,
    classifies it (content-first: an existing canonical banner overrides
    name-based classification; falls back to filename suffix and an
    explicit known-name map otherwise), and warns when the required
    banner is missing. Three rules: `banner-tier1-missing-generated-header`,
    `banner-tier2-missing-scaffold-header`, `banner-unclassified`. All
    severity=warning so the analyzer never gates the build (tier
    classification is heuristic — false positives shouldn't block
    `forge generate`).
  - **CLI wiring.** `--banners` flag added to `forge lint` (mirrors the
    Day-4 `--tests` shape). Plain `forge lint` runs the banner check
    alongside the rest when `internal/templates/` or `internal/packs/`
    exist; outside the forge repo the helper short-circuits to a no-op
    so `control-plane-next` and friends see no behavior change. Verified:
    `forge lint --banners` from `forge/` returns clean; same command from
    `control-plane-next/` returns silently with exit 0.
  - **Three test fixtures** under `internal/linter/scaffolds/testdata/banners/`:
    `missing_tier1/` (fires `banner-tier1-missing-generated-header`),
    `missing_tier2/` (fires `banner-tier2-missing-scaffold-header`),
    `correct/` (silent).
  - **Files:** new `internal/linter/scaffolds/banners.go`,
    `internal/linter/scaffolds/banners_test.go`, three fixture roots,
    `internal/cli/lint.go` (flag + helper + runAllLinters wiring),
    plus banner edits across ~40 templates.

- **`runGeneratePipeline` → typed `[]GenStep` plan (2026-05-06 polish-phase).**
  Closes FORGE_REVIEW_CODEBASE.md Tier 1.1. The pre-refactor pipeline
  was a 584-line procedural function with 25 numbered ordered steps
  (`Step 0a, 0b, 0b.1, ..., 8d-iii, 8f.1, 8g, 9`) gated by 93
  `Features.*Enabled()` checks — every new step was 30 lines of boiler-
  plate appended to the comment numbering. Now it's a 30-line loop over
  `generateSteps()` returning `[]GenStep{Name, Gate, Run, Tag}` over a
  shared `pipelineContext` (services, modulePath, entityDefs,
  configFields, has* flags parsed once). 17 steps extracted to
  dedicated `stepXxx` functions (load config, load checksums, dev-pkg
  sync, pre-codegen contract, proto detect, buf Go, descriptor, ORM,
  sqlc, tidy gen, CI workflows, pack hooks, regen infra, per-env
  deploy, Grafana, seeds, frontend mocks, tidy root, goimports,
  rehash, post-gen validate, go build). Remaining ~8 mid-pipeline
  blocks (2b, 2c, 3, 3b–3z, 4, 4a–c, 5, 5b–e, 6, 6b, 6c) live in
  `runMidPipelineLegacy` until per-block golden tests land — the
  riskiest extraction is step 6 (bootstrap.go ormEnabled derivation
  reads from three sources in a specific order). Unblocks Tier 2.3
  (`--plan` flag emits the step plan as JSON without writing), Tier
  2.5 (one-time entity proto parse vs current 3×), and Tier 3.1
  (`forge dev` watch loop dispatching by step Tag).
  - **Files:** `internal/cli/generate_pipeline.go` (new),
    `internal/cli/generate_pipeline_test.go` (new), and
    `internal/cli/generate.go` rewritten — `runGeneratePipeline` now a
    loop, `runMidPipelineLegacy` holds the un-extracted middle, and
    `runGoBuildValidate` peeled out so the build-validate body is
    callable from tests without spinning up the full pipeline.
  - **Test guard:** `TestGenerateStepsPlanStable` pins the step order
    (re-ordering breaks tests), `TestGenerateStepsPlanDeterministic`
    catches map-iteration-style nondeterminism, `TestGenerateStepsHave-
    RunAndGate` catches copy-pasted-empty step entries, and
    `TestGenerateStepsGatesAreSideEffectFree` asserts every gate is
    idempotent + non-mutating against four representative contexts
    (dir-scan fallback, cfg-only, services+db, frontends+packs) so
    future watch-mode/--plan callers can invoke gates safely without
    running the body.
  - **Smoke recipe:** `forge new smoke --kind service --service api
    --mod example.com/smoke && cd smoke && forge generate` — produces
    byte-identical output before/after the refactor (only
    `.forge/checksums.json` re-saves with fresh hashes, expected).
    Error wrapping now reports `step "<name>": <err>` so the failing
    step is in the error chain.

- **`forge/pkg/svcerr` carve-out + `api/handlers` skill rewrite + lint nudge (2026-05-06 polish-phase).**
  Closes the strongest LLM-friction signal from the dogfood review:
  the `api/handlers` skill prescribed a per-service `mapServiceError` /
  `toConnectError` helper, and the LLM faithfully produced 4 byte-
  identical copies of the resulting switch in cpnext
  (`handlers/{billing,daemon,llm_gateway,org}/handlers.go`). The fix
  ships in three pieces:
  1. **`forge/pkg/svcerr` package.** Canonical service-error → Connect-
     error mapping. Sentinel-per-Connect-code (`svcerr.ErrNotFound`,
     `ErrAlreadyExists`, `ErrPermissionDenied`, …) plus matching
     constructors (`svcerr.NotFound("user")`) that wrap with
     `fmt.Errorf("...: %w", sentinel)` so `errors.Is` keeps working.
     `ToConnect(err)` / `Wrap(err)` map to `*connect.Error`; already-
     Connect errors pass through unchanged; context.Canceled /
     DeadlineExceeded mapped; everything else falls through to
     CodeInternal so domain leaks don't escape. `WithDetail(err, msg)`
     attaches a structured proto detail when needed (rare). 13-test
     suite covers nil-passthrough, sentinel mapping, cause preservation,
     already-Connect identity passthrough (incl. wrapped-via-`%w`), the
     IsX predicates, and the WithDetail round-trip.
  2. **`api/handlers` skill rewritten.** The "write your own
     `mapServiceError`" section is gone. The canonical pattern is now
     `return nil, svcerr.Wrap(err)` after a domain-layer call, with
     domain failures expressed as `svcerr.NotFound("user")` /
     `svcerr.PermissionDenied("admin only")` / etc. in
     `internal/<svc>/`. The reference table maps every sentinel to its
     Connect code. `service-layer/SKILL.md` updated alongside: drop the
     per-package `errors.go` boilerplate, use `svcerr.Err*` directly, and
     have custom typed errors `Unwrap() error` to a `svcerr` sentinel so
     `svcerr.Wrap` keeps working uniformly.
  3. **`forgeconv-no-handler-error-mapping` lint rule.** New analyzer
     under `internal/linter/forgeconv/no_handler_error_mapping.go` walks
     `handlers/` for files declaring a function that BOTH constructs
     `connect.NewError` AND switches on `errors.Is`/`errors.As` against
     ≥2 sentinels — and/or carries one of the canonical mapper names
     (`mapServiceError`, `toConnectError`, `errToConnect`, …). Severity
     is warning (false-positive risk is real); message points at
     `svcerr.Wrap(err)`. Files already importing `forge/pkg/svcerr` are
     suppressed (mid-migration files don't double-warn). Wired into
     `forge lint --conventions` alongside the existing proto +
     internal-package-contract checks. Three fixtures
     (`handlers_bad`, `handlers_clean`, `handlers_uses_svcerr`) cover
     the firing/non-firing/suppressed cases; in-tempdir tests cover
     test-file skipping and non-handler-tree ignoring.
  - **Files:** `forge/pkg/svcerr/{svcerr.go,svcerr_test.go}` (new
    package); `internal/templates/project/skills/forge/api/handlers/SKILL.md`
    (rewritten); `internal/templates/project/skills/forge/service-layer/SKILL.md`
    (errors-section + rules update); `internal/linter/forgeconv/no_handler_error_mapping.go`
    + `_test.go` + `testdata/handlers_{bad,clean,uses_svcerr}/` (new
    rule); `internal/cli/lint.go::runConventionLint` (handler-tree
    branch); `internal/linter/forgeconv/test_helpers_test.go` (added
    `mustParseGo` / `walkAssigns` / `anyAssign` helpers needed by the
    pre-existing `handler_tests_use_tdd_test.go` predicate test).
  - **Migration for cpnext:** delete `internal/svcerr/errors.go`,
    replace each handler's `toConnectError(err)` call with
    `svcerr.Wrap(err)`, and delete the per-package `toConnectError`
    function. `forge lint --conventions` will flag any leftovers.

- **Skill catalog drift on removed `*_gen.go` filenames swept (2026-05-06 polish-phase).**
  Pre-1.7 forge emitted `middleware_gen.go` / `tracing_gen.go` /
  `metrics_gen.go` per package; those were removed when observability
  moved to Connect interceptors in `forge/pkg/observe`. The live skills
  (`SKILL.md`, `architecture/SKILL.md`, `service-layer/SKILL.md`,
  `migration/cli/SKILL.md`) still listed them as currently-emitted —
  rewritten to point at the interceptor layer (`observe.DefaultMiddlewares`
  in `cmd/server.go`) and the per-method opt-ins
  (`observe.LogCall` / `observe.TraceCall` / `observe.NewCallMetrics`).
  Migration skills (`v0.x-to-*`) are correctly preserved as historical
  artefacts. Backed by a meta-test
  (`internal/templates/skills_drift_test.go`) that scans every shipped
  SKILL.md outside `migration/v0.x-to-*` for `*_gen.go` filename
  references and asserts each is in an explicit allow-list of files
  forge actually emits today (`mock_gen.go`, `handlers_gen.go`,
  `handlers_crud_gen.go`, `authorizer_gen.go`, etc.). Mentions of the
  removed shapes are tolerated only in paragraphs containing
  "removed" / "pre-1." / "no longer" / "have been removed".

- **`handlers_test.go` → `handlers_scaffold_test.go` rename (2026-05-06 polish-phase).**
  The 2026-05-06 dogfood review counted 7 of 9 cpnext services that had
  invented a sibling `handlers_unit_test.go` to escape forge stomping the
  canonical `_test.go` slot every `forge generate`. The qualified name
  frees the canonical filename for user-owned tests. Templates affected:
  `service/unit_test.go.tmpl` now opens with a `// forge:scaffold one-shot`
  banner explaining the lifecycle and pointing at `handlers_test.go` as
  the user-owned slot. Generators updated:
  `internal/codegen/generator.go` (`GenerateServiceStub`,
  `GenerateMissingHandlerStubs` placeholder regen path, doc comments),
  `internal/generator/service_gen.go` (`forge new` path), plus golden
  test references under `internal/codegen/generator_test.go`. Migration
  users with existing `handlers_unit_test.go` files keep them; they can
  delete or rename the obsolete sibling at their convenience. Smoke
  test verifies `forge new svc-test --kind service --service api` lands
  the file at `handlers/<svc>/handlers_scaffold_test.go` with the
  scaffold header.

- **`methodAuthRequired` defaults to fail-closed (2026-05-06 polish-phase).**
  Pre-fix: every RPC without an explicit `(forge.v1.method).auth_required`
  annotation rendered `false` into `methodAuthRequired`, contradicting
  cpnext's stated fail-closed posture. Real auth still ran through
  `requireMembership` per-handler, but the generated layer was misleading
  to anyone reading the authorizer. Post-fix: proto field flipped to
  `optional bool auth_required = 1` in both
  `proto/forge/v1/forge.proto` and the in-tree assets copy, so descriptor
  extraction can distinguish unset from explicit-false. Defaults
  `AuthRequired = true` at descriptor extraction
  (`internal/cli/forge_descriptor.go`) when the annotation is absent.
  Methods explicitly opting out via `auth_required = false` keep that
  setting. Template comment in `service/authorizer_gen.go.tmpl`
  rewritten to document the new polarity. Smoke test confirms
  `handlers/api/authorizer_gen.go` renders `true` for all five
  unannotated CRUD methods.

- **`.golangci.yml.tmpl` and `eslint.config.mjs` lint-preset expansion (2026-05-06 polish-phase).**
  Open-source linter rules over hand-rolled. `.golangci.yml.tmpl` already
  enabled cyclop / funlen / gocognit / nestif / interfacebloat / gocritic
  / revive; this pass added `godot` (sentence punctuation on top-level
  doc comments, `scope: declarations`), brought thresholds in line with
  the conservative-but-not-stingy profile (cyclop 15, funlen 100/50,
  gocognit 20), and added per-rule rationale comments so users
  understand why each linter is on. Skipped: wsl (too aggressive),
  gomnd (FP-heavy), exhaustruct (forces struct field listing).
  `frontend/nextjs/eslint.config.mjs` gained `eslint-plugin-import`
  (`no-cycle: error`, `no-default-export: warn` with a Next.js
  app-router carve-out for `page.tsx` / `layout.tsx` / `route.ts` /
  config files, `order: warn` with grouped + alphabetised imports) and
  selective `eslint-plugin-unicorn` rules (`prefer-string-trim-start-end`,
  `prefer-set-has`, `prefer-includes` — the rest of unicorn is too noisy).
  `package.json.tmpl` updated with the new devDeps.

- **`forge db squash` ships as a first-class subcommand (2026-05-06 LLM-port).**
  Closes the "no canonical N-migrations → 1-baseline collapse" papercut.
  `forge db squash --from-dir <dir> --to <stem>` spins up an ephemeral
  `postgres:16-alpine` docker container, applies every migration via
  `migrate up`, runs `pg_dump --no-owner --no-privileges --inserts
  --exclude-table=public.schema_migrations` *inside the container* (so
  callers don't need a host-side postgresql-client at the matching
  major version), strips `\connect`/`\restrict` psql meta-commands, and
  writes `<stem>.up.sql` + a paired `<stem>.down.sql` (`DROP SCHEMA
  IF EXISTS public CASCADE; CREATE SCHEMA public;`). Container is torn
  down on every exit path — failure leaves no orphan containers in
  `docker ps`. Smoke-tested against the 60-migration
  control-plane-next-v0-backup tree: produced a 1.5k-line baseline
  matching the manual workflow that originally surfaced the gap.
  - **Files:** `internal/cli/db.go` (new `newDBSquashCommand`,
    `runDBSquash`, `dockerPort`, `waitForPostgres`, `stripPSQLMeta`).
  - **Smoke recipe:** `forge db squash --from-dir <migrations>
    --to test_baseline` from any directory; verifies docker availability
    and migrate CLI presence up front.

- **`forge pack info` summarizes manifest + flags project conflicts (2026-05-06 LLM-port).**
  Closes the "I have to read pack source to know what it emits" papercut.
  `forge pack info <name>` prints the pack description, kind/version/
  subpath, proto files emitted, Go files + their package directories,
  npm dependencies (incl. provider-keyed extras), Go module deps,
  migrations, generate-hook outputs, forge.yaml additions
  (`cfg.Packs += <name>`, `pack_config.<section>` defaults), and the
  post-install hooks the install will run (tidy / npm install / proto
  awareness). When invoked inside a project, the tool stat()s every
  declared output path against the project tree and surfaces conflicts
  pre-install — same shape as the install-time fresh-collision check
  but read-only. `--json` flag emits a stable snake-case shape for
  scripting. The summary is derived from the manifest plus the
  `EffectiveKind()` rules (Go vs frontend), so it stays in sync with
  installer behaviour by construction.
  - **Files:** `internal/cli/pack.go` (new `newPackInfoCmd`,
    `runPackInfo`, `buildPackInfoSummary`, `collectPackConflicts`,
    `printPackInfoText`, `packInfoSummary` JSON shape).
  - **Smoke recipe:** `forge pack info audit-log` (text);
    `forge pack info audit-log --json`.

- **`forge starter add` runs `go mod tidy` after copy (2026-05-06 LLM-port).**
  Closes the "starter add asymmetry with pack install" papercut. Pack
  install runs tidy after the file copy; starter add did not, leaving
  cold-build state where `goimports` had to resolve the new external
  imports on first build. Now `forge starter add` runs tidy
  best-effort: a tidy failure is logged but does not fail the scaffold
  (starters intentionally echo-not-install the dep list — see
  `StarterDeps.Go` — so a transient "package not found" tidy failure
  is plausible and recoverable). When `go.mod` is absent (frontend-
  only project, corrupted state) tidy is skipped silently.
  - **Files:** `internal/cli/starter.go`.
  - **Smoke recipe:** `forge starter add stripe --service billing` in
    a fresh service project; tidy runs after the copy logs.

- **`forge config set` edits environments[<env>].config programmatically (2026-05-06 LLM-port).**
  Closes the "hand-edit YAML to wire a per-env value" papercut. New
  `forge config set --env <env> <key> <value>` (and matching
  `--unset <key>`) edits forge.yaml's
  `environments[<env>].config[<key>]` map via yaml.Node round-trip,
  so unrelated keys / comments / ordering are preserved verbatim.
  Type-checks <value> against `proto/config/v1/config.proto`'s field
  annotation when the project ships one — int32/bool/float fields
  reject malformed values up front (`config key "port" is int32 in
  proto; value "abc" is not a valid integer`). Sensitive values can
  be passed as `${secret-name#secret-key}` references; the reference
  *shape* is validated but the secret is never resolved by the CLI.
  Auto-creates the environment block if --env names a missing entry.
  Empty config maps are pruned on unset so the file doesn't accumulate
  stub `config: {}` after a sequence of removes.
  - **Files:** `internal/cli/config_cmd.go` (new package surface;
    wired into `root.go`).
  - **Smoke recipe:** `forge config set --env dev log_level debug`,
    `forge config set --env prod database_url '${prod-db#dsn}'`,
    `forge config set --env dev port 9090`, `--env dev port not-a-num`
    (rejected up-front), `--env dev --unset port` (removes key,
    cleans up empty config map).

- **CI verify-generate gate now ships across all project kinds + Taskfile parity (2026-05-06 LLM-port).**
  Closes the recurring "codegen mock drift surfaces only at test time"
  symptom. Three load-bearing fixes:
  1. **Bug fix** in `internal/cli/generate_ci.go`'s
     `buildCIWorkflowData`: the `VerifyGenerated` field was never set,
     so every `forge generate` overwrote the scaffold-time
     `VerifyGenerated:true` (from `internal/generator/project_ci.go`)
     with the zero value, silently stripping the verify-generated job
     from CI. With this fix, the rendered job is stable across
     scaffold and regenerate.
  2. **CLI/library kinds now opt in to verify-generated** (previously
     disabled because "no proto codegen"). Even without proto, every
     contract.go-bearing package emits a `mock_gen.go`, and that's
     exactly the artifact that drifts when a contract method gains a
     parameter. Forge itself is a CLI kind and was hit by this in
     past sessions — eating dogfood here.
  3. **Taskfile.cli.yml.tmpl** and **Taskfile.library.yml.tmpl** gain
     `generate` and `verify-generate` tasks so local-dev parity
     matches the service-kind Taskfile (which already shipped them in
     the round-5 Make→Task migration).
  - **Files:** `internal/cli/generate_ci.go`,
    `internal/generator/project_ci.go`,
    `internal/templates/project/Taskfile.cli.yml.tmpl`,
    `internal/templates/project/Taskfile.library.yml.tmpl`.
  - **Smoke recipe:** `forge new svctest --kind service --service api`
    and `forge new clitest --kind cli` both emit a
    `verify-generated:` job in `.github/workflows/ci.yml`; CLI kind's
    `Taskfile.yml` carries `task verify-generate`.


- **Test scaffolds now consult the same alias-collision rule as bootstrap testing (2026-05-06 LLM-port).**
  Closes the "freshly-scaffolded billing test fails to compile because
  `app.NewTestBilling` doesn't exist; the actual factory is
  `app.NewTestSvcBilling`" papercut. Root cause: `unit_test.go.tmpl`,
  `integration_test.go.tmpl`, `handlers_crud_test_gen.go.tmpl`, and
  `handlers_crud_integration_test.go.tmpl` all rendered
  `app.NewTest{{$.ServiceName | pascalCase}}(...)` — but
  `pkg/app/testing.go` emits a `Svc`-prefixed factory whenever a
  service's Go package collides with an internal package's leaf name
  (`internal/billing/` + service `billing`). The bootstrap testing
  generator has had this disambiguation since the initial AssignBootstrapAliases
  pass; the test scaffolds just never picked it up.
  Fix: added `ServiceTemplateData.TestHelperName` (and
  `CRUDTestTemplateData.TestHelperName`), populated by a new
  `codegen.ComputeTestHelperName(servicePkg, projectDir)` helper that
  mirrors `GenerateBootstrapTesting`'s pkgCount logic — returns
  `Svc<Pascal>` when `<projectDir>/internal/<servicePkg>` exists,
  else `<Pascal>`. All four templates now reference
  `{{$.TestHelperName}}` directly. `GenerateServiceStub` derives
  projectDir from targetDir's `<projectDir>/handlers/<svc>` shape;
  `GenerateMissingHandlerStubs` and `GenerateCRUDTests` already
  carried projectDir.
  Verified end-to-end via `TestGenerateServiceStub_HandlersTestMatchesBootstrapTestingHelper`:
  scaffolding a `billing` service against a project with
  `internal/billing/` produces `handlers_test.go` referencing
  `app.NewTestSvcBilling` (and falls back to `app.NewTestEcho` when
  there's no collision).
  - **Files:** `internal/codegen/generator.go`, `internal/codegen/crud_gen.go`,
    `internal/generator/service_gen.go`,
    `internal/templates/service/{unit_test,integration_test,handlers_crud_test_gen,handlers_crud_integration_test}.go.tmpl`,
    `internal/codegen/generator_test.go`, `internal/codegen/crud_gen_test.go`.

- **`forge add operator` detects existing controller-shape and skips operator.go (2026-05-06 LLM-port).**
  Closes the "ported v0 controller-IS-the-reconciler shape collides
  with the new operator scaffold" papercut. Root cause:
  `GenerateOperatorBinaryOnly` unconditionally wrote `operator.go`
  (declaring `Deps`, `Controller`, `New`, `SetupWithManager`,
  `AddToScheme`) even when the operator dir already contained a ported
  `controller.go` carrying those exact symbols — duplicate-declaration
  compile errors immediately, every time.
  Fix (option (a) from the original sketch): added
  `detectExistingOperatorShape(operatorDir)`, an AST-based scan that
  walks every non-test `.go` file in the dir and returns true when any
  of `type Controller`, top-level `func New(...)`, top-level `func
  AddToScheme(...)`, or method `SetupWithManager(...)` is present.
  When the shape is detected, both `operator.go` AND `doc.go` are
  skipped (doc.go is purely cosmetic and would inject conflicting
  package documentation onto the user's ported code). The skip is
  printed with the conflicting filename so the user sees
  immediately why the scaffold backed off. Per-CRD reconciler files
  (e.g. `<crd>_controller.go` from `forge add crd`) don't trigger the
  skip — they declare per-CRD types like `WorkspaceController`, not
  bare `Controller`, and don't carry top-level `New`/`AddToScheme`.
  - **Files:** `internal/generator/operator_binary_gen.go`,
    `internal/generator/operator_gen_test.go`.

- **`forge lint --suggest-excludes` emits a YAML snippet of contracts.exclude candidates (2026-05-06 LLM-port).**
  Closes the bulk-port discovery loop ("run lint, see 30 missing-contract
  errors, hand-add 30 lines to forge.yaml"). Root cause: there was no
  way to ask forge "which packages SHOULD be in contracts.exclude?"
  short of running the contract linter and grepping the failures.
  Fix: new `forge lint --suggest-excludes` walks `internal/` for
  packages with no `contract.go` AND at least one exported method on
  a struct (the same trigger the require-contract analyzer uses), then
  applies four heuristics to filter to legitimate exclude candidates:
  filename pattern (`analyzer.go`, `*_analyzer.go`, `*lint*.go`),
  third-party-embed only (every exported-method type embeds a
  third-party struct, e.g. cobra.Command), generated-only (every .go
  file is `_gen.go` or carries the canonical `// Code generated by`
  header), and convention-prefix (`internal/db`, `internal/auth`,
  `internal/metrics`, `internal/middleware`, `internal/planlimits`,
  …). Output is a copy-paste-ready YAML snippet with one comment per
  entry naming the heuristic that fired. Existing
  `contracts.exclude:` entries are filtered out so successive runs
  don't re-suggest already-excluded paths. Nothing is mutated — the
  user pastes selectively.
  - **Files:** `internal/cli/lint.go` (flag + dispatch),
    `internal/cli/lint_suggest_excludes.go` (heuristics + walk),
    `internal/cli/lint_suggest_excludes_test.go`.

- **proto skill clarifies forge generate is canonical; lint warns on .pb.go without .pb.orm.go sibling (2026-05-06 LLM-port).**
  Closes the "ran `buf generate` after adding a proto/db entity, then
  hit confusing 'ORM out of sync' lint warnings" papercut. The
  underlying expectation gap is real: forge layers ORM, descriptor,
  mock, frontend hook, and bootstrap codegen *on top* of `buf
  generate`'s output. Running buf alone produces only the proto stubs
  and leaves everything downstream stale. Fix is documentation +
  warning, not behavior change (per scope guidance):
    1. **proto SKILL.md update.** New section "`forge generate` is
       the canonical entry — not `buf generate` alone" lists exactly
       what's missing when buf runs solo (ORM, descriptor, mocks,
       hooks, bootstrap), and points at the new lint hint as the
       reactive companion.
    2. **`proto-orm-out-of-sync` lint.** New `runORMSyncLint(projectDir)`
       walks `gen/db/v1/`, groups files by basename, and warns when
       any `<base>.pb.go` has no matching `<base>_*.pb.orm.go`
       sibling OR when the `.pb.go` is newer (mtime) than every ORM
       sibling. Wired into `runAllLinters` step 11. Warnings only;
       never gates the build.
  - **Files:** `internal/templates/project/skills/forge/proto/SKILL.md`,
    `internal/cli/lint.go` (wiring), `internal/cli/lint_orm_sync.go`
    (heuristic), `internal/cli/lint_orm_sync_test.go`.

- **Webhook codegen log line + RegisterWebhookRoutes auto-wire + Taskfile lint delegation (2026-05-06 LLM-port).**
  Closes three webhook/dx backlog items in one pass.
  - **(1) Log line kebab/snake mismatch.** `internal/cli/generate_middleware.go::generateWebhookRoutes`
    was printing `handlers/<svc.Name>/webhook_routes_gen.go` using the
    forge.yaml `name:` field verbatim — kebab-case for services like
    `admin-server`, while the file actually lands at the snake-case
    package path (`handlers/admin_server/`). Now the log uses `relPath`
    directly (the same path the writer used), so copy-paste from the
    log always works.
  - **(2) `RegisterWebhookRoutes` auto-wire.** Picked option (b) from
    the original sketch: `pkg/app/bootstrap.go` now emits
    `svcs.<Field>.RegisterWebhookRoutes(mux, middleware.HTTPStack(logger))`
    directly after `svcs.<Field>.RegisterHTTP(...)` for any service
    that has webhooks declared in forge.yaml. The user-owned
    `service.go` stays untouched — no template overwrite, no manual
    edit required, no double-registration risk (the user-owned
    `RegisterHTTP` no longer needs to delegate). Plumbing: added
    `BootstrapServiceData.HasWebhooks bool`, threaded a
    `webhookServices map[string]bool` parameter through
    `codegen.GenerateBootstrap` (keyed by snake-case package name to
    match `toServicePackage`), and added a
    `discoverWebhookServices(projectDir)` helper in
    `internal/cli/generate_bootstrap.go` that scans
    `cfg.Services[i].Webhooks`. Updated comments in
    `service/service.go.tmpl` and `webhook/webhooks.go.tmpl` to make
    the auto-wire explicit and to warn against manual delegation
    (which would double-register on the same mux path and panic at
    boot). New unit test
    `TestGenerateBootstrap_AutoWiresWebhookRoutes` verifies the
    wire-on / wire-off behavior. Smoke-test on control-plane-next:
    `admin_server` (webhooks declared) gets the auto-wire; nine other
    services don't; `handlers/admin_server/service.go::RegisterHTTP`
    cleaned up to the no-op shape with the new template's "do NOT
    call from here — would double-register" comment.
  - **(3) `task lint` delegates to `forge lint`.** Replaced the
    hard-coded `golangci-lint run` + `buf lint` + `task: lint:frontend`
    chain in `internal/templates/project/Taskfile.yml.tmpl` with a
    single `forge lint`, which already runs the full suite
    (golangci-lint with skip-on-missing, buf, frontend lint &
    typecheck, plus contract / db / migration-safety / convention
    sub-linters as configured). `lint:frontend` survives as a
    standalone subtask for fast iteration on frontend-only runs.
    Mirrored the change into `control-plane-next/Taskfile.yml`
    (Tier-2 user-owned, so the template change doesn't auto-propagate).
    Re-blessed `internal/templates/testdata/golden/Taskfile.yml.golden`.
    Verified: `task lint` in cpn delegates cleanly through to forge
    lint and exits 0.
  - **Files:** `internal/cli/generate_middleware.go`,
    `internal/cli/generate_bootstrap.go`,
    `internal/codegen/contract.go`,
    `internal/codegen/contract_methods.go`,
    `internal/codegen/generator.go`,
    `internal/codegen/generator_test.go`,
    `internal/codegen/mock_gen.go`,
    `internal/templates/project/bootstrap.go.tmpl`,
    `internal/templates/project/Taskfile.yml.tmpl`,
    `internal/templates/service/service.go.tmpl`,
    `internal/templates/webhook/webhook_routes_gen.go.tmpl`,
    `internal/templates/webhook/webhooks.go.tmpl`,
    `internal/templates/testdata/golden/Taskfile.yml.golden`,
    `control-plane-next/Taskfile.yml`,
    `control-plane-next/handlers/admin_server/service.go`.

- **List-page template now uses Table primitive + URL-backed search (2026-05-06 LLM-port).**
  Closes two scaffolding-convention backlog items in one pass:
  hand-rolled `<table>` markup AND `useState`-only filter state.
  `internal/templates/frontend/pages/list-page.tsx.tmpl` now imports
  `Table, TableBody, TableCell, TableHead, TableHeader, TableRow` from
  `@/components/ui/table` and composes them — no more inline `<table
  className="...">` / `<thead>` / `<tbody>` markup with bespoke Tailwind.
  The `striped`/`clickable` props on `<TableRow>` carry the previously
  hand-classed alternation + cursor styling. Search state moved off
  `useState` only and into URL search params via `useSearchParams()` +
  `router.replace(?...)`, with a 200ms-debounced local draft so typing
  doesn't thrash history. Verified end-to-end: scaffolded a fresh
  `--kind service --frontend admin` project off the lifecycle proto
  fixture, confirmed `forge generate` emits pages that import the
  `Table` primitives and read `q` from `searchParams`. No Zustand
  store is generated for list-page filters (matching the
  `frontend/state` skill's decision table).
  - **Files:** `internal/templates/frontend/pages/list-page.tsx.tmpl`.

- **Frontend mock-transport stub no longer crashes prerender; stub-emit path always runs (2026-05-06 LLM-port).**
  Two interlocking bugs that together made `npm run build` fail
  whenever `.env.local` shipped `NEXT_PUBLIC_MOCK_API=true` for a
  non-CRUD project (control-plane-next was hitting this on every
  build):
  1. `internal/cli/generate_frontend_mocks.go::mockTransportStubContent`
     used to throw at `createMockTransport()` invocation time. Next.js
     prerender of `/_not-found` evaluates `connect.ts` which calls
     `createMockTransport()` synchronously when `MOCK_API=true` —
     module-eval throw → opaque `digest: ...` build failure. New stub
     returns a valid `Transport` whose `unary` and `stream` reject with
     `Code.Unavailable` + a clear message, so pages render their
     existing 3-state error UI and prerender treats that as success.
  2. `generateFrontendMocks` was guarded behind
     `len(entityDefs) > 0 && len(services) > 0`, making the early-return
     stub-emit path **dead code**. The stub was never written for
     no-entity projects, leaving connect.ts's
     `require('@/lib/mock-transport')` unresolvable at build time.
     Guard relaxed to `cfg.Features.FrontendEnabled() && len(cfg.Frontends) > 0`
     so the stub emits whenever there's a Next.js frontend.
  Verified end-to-end: scaffolded a fresh forge project, ran
  `npm run build` with `NEXT_PUBLIC_MOCK_API=true`, all 10 static pages
  prerendered successfully under both the rich-mock and stub-mock
  variants.
  - **Files:** `internal/cli/generate_frontend_mocks.go`,
    `internal/cli/generate.go` (Step 8d-iii guard).

- **Pack install is now idempotent + collision-aware (2026-05-06 LLM-port).**
  Closes both pack-install backlog items in one pass.
  `internal/packs/pack.go::InstallWithConfig` no longer errors with "pack
  already installed" when the pack is already in `cfg.Packs`. Instead it
  enters resync mode: `overwrite: once` files that exist are skipped (as
  before), and migrations are de-duplicated by slug — a new
  `findMigrationIDBySlug` helper checks `db/migrations/` for any
  `<digits>_<slug>.{up,down}.sql` and skips the emit with
  `Skipping migration audit_log (already at 00002, slug match)` rather
  than allocating a fresh sequential ID. Re-running `forge pack install
  audit-log` against control-plane-next is now a no-op for the
  audit_log table (no more 00004/00005 dupes).
  For the collision case: a fresh install (pack NOT yet in `cfg.Packs`)
  scans every pack file's rendered target plus migration slugs up-front
  via `Pack.detectFreshInstallCollisions`. If any `overwrite: once`
  target already exists OR a migration slug is on disk, install fails
  fast with the full list of conflicting paths and a rename recipe
  (rename/delete the file, OR add the pack name to forge.yaml's
  `packs:` for resync-mode re-install). Files with `overwrite: always`
  are exempt — the pack author opted into clobbering. Smoke-tested on
  control-plane-next (re-install audit-log produces zero new files,
  zero new migrations) and on a fresh `forge new` project with a
  hand-written `pkg/middleware/audit/auditlog/handler.go` (install
  fails fast, user file untouched, `cfg.Packs` not mutated). New unit
  tests: `TestPackInstallIdempotentMigrations`,
  `TestPackInstallCollisionDetected`, `TestFindMigrationIDBySlug`.
  Also nudged the CLI to print "Re-installing pack 'X' (resync — existing
  files preserved)..." instead of "Installing" when the pack is already
  listed, so users can see at a glance which path they're on.

- **Codegen contract drift fixed: `GenerateBootstrap` mock + bootstrap
  scaffolds now carry `webhookServices` / `HasWebhooks` (2026-05-06
  LLM-port).** Surfaced as a `go test ./...` failure with the pack-install
  changes above: the canonical `codegen.Service` interface had been
  extended with `webhookServices map[string]bool` (for the auto-wire fix
  on `RegisterWebhookRoutes`) but `mock_gen.go`'s `GenerateBootstrapFunc`
  field, the `internal/cli/generate_bootstrap.go` caller, and the
  initial-scaffold `bootstrapService` struct in
  `internal/generator/project_bootstrap.go` had not been refreshed. Three
  call sites were skewed; tests (`TestProjectGeneratorGenerateWritesScaffoldThatBuildsCleanlyByDefault`,
  `TestBootstrapTemplate_WithAllComponentTypes`, all `TestFeatureFlag_*`)
  were failing on `<.HasWebhooks>: can't evaluate field` template errors.
  Refreshed all three: added the `webhookServices` argument to
  `GenerateBootstrapFunc` + the mock method, added a
  `discoverWebhookServices(projectDir) map[string]bool` helper that scans
  forge.yaml's `services[].webhooks` and keys by snake-case package, and
  added `HasWebhooks bool` to `internal/generator.bootstrapService` (and
  the matching template-test fixture). Initial scaffold leaves it false;
  the codegen-pass regenerator overwrites bootstrap.go with the real
  value once forge.yaml + descriptor are read. **Forge-improvement note:**
  this is the second time mock_gen.go drifted this session — the codegen
  contract probably wants a regen step in `forge generate` that's at
  least lint-checked, or a `forge audit` rule that diffs the contract
  vs. the mock signatures. Backlogging.

- **Operator-scaffold collision detector implemented (2026-05-06 LLM-port).**
  `internal/generator/operator_binary_gen.go::GenerateOperatorBinaryOnly`
  was calling an undefined `detectExistingOperatorShape` helper — added
  the implementation (AST-scans the operator dir for top-level `Controller`
  / `New` / `AddToScheme` decls or a method named `SetupWithManager`,
  ignoring `_test.go` files). When a sibling .go file is already shaped
  like a v0 controller-IS-the-reconciler scaffold, `forge add operator`
  now skips emitting `operator.go`/`doc.go` rather than clobbering ported
  user code. Closes the build break that was masking the rest of the test
  suite.

- **Universal Taskfile verbs upstreamed: `verify-generate`, `coverage`, `db-snapshot-restore` (2026-05-06 LLM-port).**
  Surfaced by the control-plane → control-plane-next Make→Task migration:
  the legacy `make verify-generate`, `make coverage`, and `make
  db-snapshot-restore` recipes belong in every forge-managed project, not
  per-project. Added all three to
  `internal/templates/project/Taskfile.yml.tmpl` (service kind);
  `verify-generate` runs `task generate` + `git diff --exit-code` so CI
  catches stale codegen; `coverage` produces `coverage.html` (no -race —
  race+atomic requires `covdata` which is unreliable in auto-installed
  toolchains); `db-snapshot-restore` is the local-side half of the
  snapshot loop, parameterized by `DUMP=` / `PGHOST=` / `SNAPSHOT_DB=`.
  The matching `db-snapshot` (download from a managed store) is
  intentionally NOT upstream — the URL scheme (gs://, s3://, https://)
  is project-specific. Projects that need it (like
  control-plane-next) ship a project-local `db-snapshot` task whose
  output feeds the upstream `db-snapshot-restore`. Also added `coverage`
  to the CLI/library Taskfile templates (lighter set, no service-only
  features). Updated `clean` to remove `coverage.html` alongside
  `coverage.out`. Re-blessed `internal/templates/testdata/golden/Taskfile.yml.golden`.
  Rationale: forge's contract is "production-ready Go projects"; CI
  guard + coverage are table stakes at that bar.
  - **Files:** `internal/templates/project/Taskfile.yml.tmpl`,
    `internal/templates/project/Taskfile.cli.yml.tmpl`,
    `internal/templates/project/Taskfile.library.yml.tmpl`,
    `internal/templates/testdata/golden/Taskfile.yml.golden`.

- **Local forge/pkg replace auto-vendored on `forge generate` (2026-05-06).**
  Closes the dev-mode docker pain that's blocked sibling-checkout users
  for the entire control-plane-next port: when `go.mod` has
  `replace github.com/reliant-labs/forge/pkg => /absolute/host/path`,
  `forge generate` now auto-vendors that path into
  `<project>/.forge-pkg/`, rewrites the replace to `./.forge-pkg`, and
  the `Dockerfile.tmpl` emits the corresponding
  `COPY .forge-pkg/ ./.forge-pkg/` line whenever `.forge-pkg/go.mod`
  exists on disk. **One `forge generate` and the host build, the docker
  build, and air-loop dev mode all see the same forge/pkg source.**
  - Picked Option D (auto-vendor at generate time) over Option A
    (`forge new --dev` flag) because the trigger is implicit (presence
    of the host-absolute replace), so no flag, no UX surface area, and
    existing projects with the manual workaround pick it up on next
    `forge generate` with zero opt-in. Tradeoff: ~700 KB of extra
    files inside the project tree while in dev mode; offset by gitignore.
  - Sync is content-aware (byte-equal short-circuits a re-write),
    purges stale files (so a renamed/deleted `forge/pkg` file doesn't
    linger as a dangling symbol in `.forge-pkg/`), and refuses to
    vendor a target whose go.mod doesn't declare the canonical
    `module github.com/reliant-labs/forge/pkg` (defence-in-depth
    against pointing at the wrong directory).
  - Also picks up the "already vendored" case: when go.mod points at
    `./.forge-pkg` and a sibling `../forge/pkg` exists, every
    `forge generate` cycle refreshes the vendor from the sibling — so
    the failure mode where the user hand-edits forge/pkg, runs forge
    generate from the project, and gets stale binding evaporates.
  - **Files:** new `internal/cli/dev_pkg_replace.go` +
    `dev_pkg_replace_test.go` +
    `dev_pkg_replace_integration_test.go` (integration test runs
    against control-plane-next when present); generate.go calls
    `syncDevForgePkgReplace` early in the pipeline (Step 0b.1);
    `Dockerfile.tmpl` adds a `{{- if .LocalForgePkgVendored }}` block;
    `upgrade.go::upgradeTemplateData` + `buildTemplateData` learn the
    flag (auto-detected from `.forge-pkg/go.mod` presence);
    `project.go::templateData` adds the field with `false` default
    (initial scaffold is never in vendored state);
    `templates/templates_test.go` covers the on/off rendering of the
    COPY line.
  - **Migration for existing dev-mode projects:** zero. Re-run
    `forge generate`. Optional: add `.forge-pkg/` to `.gitignore` if
    you want the vendor uncommitted (recommended for clarity — it's
    fully reconstructible from forge/pkg). control-plane-next was
    used as the test bed; the integration test asserts idempotency
    on the real codebase.

- **deploy/kcl ConfigMap codegen + projection (2026-04-30 LLM-port).**
  `forge deploy <env> --dry-run` previously rendered Deployments whose
  non-sensitive env vars baked literal `value:` strings into the
  Deployment manifest and rendered ZERO ConfigMap resources, even when
  `forge.yaml -> environments[*].config` had 30+ entries (control-plane-next
  was hitting the gap on every dry-run). Fix synthesises a per-env
  ConfigMap (`<project>-<env>-config`) from non-sensitive value-bearing
  fields and switches their EnvVar projection from inline `value=` to
  `config_map_ref` + `config_map_key` so the rendered Deployment env
  entry becomes `valueFrom.configMapKeyRef`. Sensitive fields stay on
  externally-managed Secrets via `secret_ref` — Secret resources are
  intentionally NOT generated (the security boundary lives outside the
  deploy artefact). Schema changes:
    - `EnvVar` gained `config_map_ref` + `config_map_key` (validity
      check now requires one of value, secret_ref, config_map_ref).
    - New `ConfigMap` schema (name, namespace, data, labels, annotations).
    - `Environment` gained `config_maps: [ConfigMap]`.
  Render changes: `_env_source` emits `valueFrom.configMapKeyRef` when
  `config_map_ref` is set; new `_render_config_map` lambda; the master
  `render_environment` lambda now appends `[_render_config_map(cm, env)
  for cm in env.config_maps]` between the namespace and the apps. Codegen
  changes: `internal/codegen/deploy_config_gen.go` now emits a `CONFIG_MAPS`
  list at the bottom of `config_gen.k` (always emitted, possibly empty)
  carrying a single `schema.ConfigMap` populated from the non-sensitive
  fields. Per-env `main.k` templates (single + binary=shared) wire
  `config_maps = cfg.CONFIG_MAPS` on `Environment`. Result: dry-run on
  control-plane-next now emits 1 ConfigMap (10 keys) alongside 9
  Deployments + 9 Services + Namespace + RBAC; pods get correctly
  projected env vars on apply. control-plane-next's hand-materialised
  `schema.k`, `render.k`, and per-env `main.k` were updated in lockstep
  (since those files are project-owned, not forge-managed). Tests:
  `TestGenerateDeployConfig_BasicValuesAndSecrets` and `_SkipsNonSensitiveWithoutValue`
  updated to assert the new ConfigMap shape; triple-gate green.

  **Design choices made in-session** (per the operational notes):
    - **Per-env ConfigMap, not per-service.** Simpler — one resource
      per env shared by all services in the multi-service app. Per-service
      ConfigMaps would let workspace-controller drop AUTH_DEV_MODE
      visibility, but the security boundary is the Secret, not the
      ConfigMap. Revisit only if a project surfaces a real need to
      narrow ConfigMap visibility.
    - **Always emit `CONFIG_MAPS: [schema.ConfigMap] = [ ... ]` (possibly
      empty list).** The env's `main.k` references it unconditionally;
      keeping it unconditional avoids a "is this a multi-service project
      with a config proto" branch in the template wiring.
    - **Generated ConfigMap data values are stringified.** k8s `ConfigMap.data`
      is `map[string]string`; we use the same `stringifyConfigValue`
      helper as the inline-value path, so int/bool fields project to
      `"100"` / `"true"` and the in-process config loader (which
      `strconv.ParseInt` / `ParseBool`s the env-var) round-trips them.

- **Config-loader codegen no longer registers empty-name pflags for secret-only fields (2026-04-30).**
  `internal/templates/project/config.go.tmpl` previously emitted
  `cmd.Flags().String("{{.Flag}}", ...)` unconditionally, so config
  fields annotated with `sensitive: true` and no `flag:` annotation
  rendered as `cmd.Flags().String("", "", "...")`. pflag panics on the
  second such registration with `migrate flag redefined: `, taking down
  every CLI subcommand that touches the config loader (`task migrate`,
  `db migrate up`, `--help` on subcommands that called `RegisterFlags`,
  etc). Reproduced in `control-plane-next` where 30+ secret-backed
  fields hit this. Fix: gated all three template branches that touch
  `.Flag` (the `cmd.Flags().XYZ` registration in `RegisterFlags`, the
  `} else if cmd.Flags().Changed(...)` arm in `Load`, and the
  `flag: --` text in the required-field error message) on
  `{{- if ne .Flag "" -}}`. The field is still in the `Config` struct,
  still loaded from its env var, and still projected to a Secret in
  the deploy gen — only the CLI flag plumbing is skipped. This is
  intentional defense-in-depth: the user explicitly does NOT want
  secrets configurable via `--db-password=hunter2` (shell history,
  `ps`-leak surface). New unit test
  `TestGenerateConfigLoader_NoFlagSkipsRegistration` pins the
  invariant. Triple-gate green on forge; control-plane-next now
  starts with `--help` instead of panicking.

- **Generated Dockerfile no longer COPYs a non-existent `config/` directory (2026-04-30).**
  `internal/templates/project/Dockerfile.tmpl` had `COPY config/ ./config/`
  in both the `debug` and `production` stages (lines 71 and 87), but
  service-shaped forge projects don't have a top-level `config/` dir —
  the config system reads env vars and uses a generated
  `pkg/config/config.go` (compiled in) sourced from `proto/config/v1/
  config.proto`. The dead COPY lines made `docker compose build`
  fail with `failed to compute cache key: "/config": not found` on
  every freshly generated project, blocking `task dev-up` and any
  `forge deploy <env>` that needs an image. Fix: dropped the two
  `COPY config/ ./config/` lines from the template and updated the
  `Dockerfile.golden` snapshot. The same fix was applied to
  `control-plane-next/Dockerfile` to unblock infra verification.
  `forge generate` regenerates the corrected file. Triple-gate green.

- **`forge test migrate-tdd` codemod for hand-rolled handler tests (2026-04-30).**
  New `forge test migrate-tdd` subcommand walks `handlers/<svc>/*_test.go`
  under the project root, parses each file with `go/parser`, and rewrites
  the canonical hand-rolled shape — `tests := []struct{name; call}{...}`
  + `for _, tt := range tests { t.Run(tt.name, ...) }` — into per-RPC
  `TestXxx_Generated` functions that delegate to
  `tdd.RunRPCCases[Req, Resp]`. Two input shapes are supported, picked by
  the `call` field's signature: service-receiver (`call func() error`)
  emits `svc.Method` as the handler arg, client-receiver
  (`call func(client X) error`) emits `client.Method`. Imports are
  patched: `forge/pkg/tdd` is added, unused imports (e.g. the no-longer-
  referenced `*v1connect` client type, or `context` if no longer used)
  are dropped. Files that don't match the recognised shape are skipped
  with a printed reason and never partially rewritten. `--dry-run` prints
  the actions without writing.

  Also extended `tdd.Case` with `AnyOutcome bool`. The hand-rolled tests
  the codemod replaces tolerated either a successful response or any
  Connect error (the `t.Logf` on err pattern) — necessary because the
  RPCs they exercised were a mix of stub (`CodeUnimplemented`) and
  partially wired (`CodeFailedPrecondition`) handlers. Without
  `AnyOutcome`, every codemod-emitted scaffold row would have failed on
  the wired-but-deps-less services. `AnyOutcome` is documented as the
  scaffold-stage knob: replace with `WantErr` or `Check` once the
  handler is real. `Check` still runs on successful responses even in
  `AnyOutcome` mode so happy-path assertions can be pinned without
  giving up the lenient error path.

  Validation: codemod golden tests cover service-receiver, client-receiver,
  and no-match shapes (`internal/cli/testdata/test_migrate_tdd/`); all
  three assert the file parses, the hand-rolled shape is gone, the new
  shape is present, sibling tests survive verbatim, and no-match files
  are byte-identical post-run. `--dry-run` is checked separately.
  `pkg/tdd/rpc_test.go` gains two cases for `AnyOutcome`: lenient
  acceptance of success + every error code, plus `Check` running on
  successful responses. Triple-gate green: `go build ./... && go test
  -count=1 ./internal/cli/... ./pkg/tdd/...`.

  Closes the "tdd.RunRPCCases migration codemod missing" backlog item.

  **Where:**
  - New: `internal/cli/test_migrate_tdd.go` (the codemod), `internal/cli/test_migrate_tdd_test.go` (golden tests),
    `internal/cli/testdata/test_migrate_tdd/{svc_basic,client_integration,no_match}/input.go.txt`.
  - Modified: `internal/cli/test.go` (wired `migrate-tdd` subcommand under `test`),
    `pkg/tdd/rpc.go` (`AnyOutcome` field + `TableRPC` honors it),
    `pkg/tdd/rpc_test.go` (two new cases),
    `internal/templates/project/skills/forge/migration/v0.x-to-tdd-rpccases/SKILL.md`
    (mentions the new automated path).

- **`forge pack add`/`remove` aliases (2026-04-30).** Added cobra
  aliases so `forge pack add <name>` (and `forge pack uninstall`) work
  alongside the canonical `install`/`remove` verbs. Closes the
  "I tried `add` first because that's what `forge add operator` uses"
  papercut from the LLM-port pain points. Help text under
  `forge pack` now lists the alias-preferred form. Triple-gate green.
  **Where:** `internal/cli/pack.go` (cobra `Aliases` slice on the
  install + remove subcommands; updated parent `Long` help text).

- **Scaffolder acronym list extended (LLM, JWT, IO) (2026-04-30).**
  Closed the LlmGatewayService vs LLMGatewayService scaffolder bug
  surfaced during the control-plane-next port. The naming.GoInitialisms
  list now includes `LLM`, `JWT`, and `IO` so kebab/snake-cased service
  names like `llm-gateway` PascalCase to `LLMGateway` (matching what
  protoc-gen-go emits via the proto's explicit `service LLMGatewayService`
  declaration), keeping the Connect handler embed type aligned with the
  proto-derived Go type. Verified by smoke-test: `forge new --service
  llm-gateway` produces `service.go` referencing
  `llm_gatewayv1connect.UnimplementedLLMGatewayServiceHandler`.
  **Where:** `internal/naming/naming.go` (extended `GoInitialisms` slice
  + new tests in `naming_test.go`).

- **Frontend scaffold ignores `next-env.d.ts` in eslint (2026-04-30).**
  Next.js auto-generates `next-env.d.ts` with a triple-slash directive
  that `@typescript-eslint/triple-slash-reference` flags. Standard
  Next.js convention is to ignore the file (Next regenerates it as
  needed); the scaffold's `eslint.config.mjs` template now lists it
  alongside `node_modules/**` and `src/gen/**`. **Where:**
  `internal/templates/frontend/nextjs/eslint.config.mjs`.

- **Pack install skips tidy when proto files added (2026-04-30).**
  Pack-emitted Go files often `import` paths under `gen/<x>/v1` that
  don't exist until `forge generate` runs (e.g. audit-log emits
  `proto/audit/v1/audit_log.proto` + Go that imports the gen output
  in the same install). Running `go mod tidy` post-install fails with
  "no required module provides package …/gen/<x>/v1". Pack install
  now detects new `.proto` outputs and prints
  `Skipping go mod tidy: pack added .proto files; run 'forge generate'
  to produce gen/ output and tidy.` Tidy still runs as part of the
  next `forge generate`. **Where:** `internal/packs/pack.go`
  (`hasNewProtoFile` helper + early-return in `InstallWithConfig`).

- **Scaffold migration init now 5-digit + skipped on existing baseline (2026-04-30).**
  Two related fixes for the brought-in-baseline collision: (1) the
  `forge new` example migration now writes `00001_init.{up,down}.sql`
  (was `0001_init.*`) so width matches the rest of the forge ecosystem
  (pack migrations, ORM migrations, all use 5-digit) and any collision
  is exact-name and obvious. (2) `generateExampleMigration` now skips
  emit entirely when `db/migrations/` already contains any `.sql`
  file — which is the case when a user `forge new`'d into a project
  that brought its own `00001_baseline.up.sql`. db/README.md updated
  to document the 5-digit convention. **Where:**
  `internal/generator/dx_files.go` (`generateExampleMigration` +
  `hasExistingMigration` helper; README content).

- **`forge generate` honours user edits to `app/page.tsx` and `nav.tsx` (2026-04-30).**
  Both files are documented as "yours to extend" in the frontend skill
  but the generator was unconditionally overwriting them on every
  `forge generate`, reverting hand-edits to the dashboard listing and
  the nav items. The nav generator now uses
  `checksums.WriteGeneratedFile` (the same path-aware-of-history
  helper that gates other Tier-2 emits) so a render whose hash
  matches the on-disk content's *or any prior render in
  `.forge/checksums.json`* sails through, but a user-edited file is
  left untouched. New entities entering the proto descriptor still
  auto-grow nav.tsx until the user hand-edits — at which point they
  own the file. `forge generate --force` re-clobbers explicitly.
  **Where:** `internal/cli/generate_frontend_nav.go` (signature now
  takes `*checksums.FileChecksums` + `force`; call site in
  `generate.go` updated).

- **`mock-transport.ts` always emitted, even with no CRUD entities (2026-04-30).**
  `connect.ts` unconditionally `require()`s `@/lib/mock-transport`, but
  `generateFrontendMocks` was skipping the file when no entity-CRUD
  RPCs existed (early return on `len(entities)==0 || len(services)==0`,
  plus the per-frontend `if len(transportEntities) > 0` guard). Result:
  every frontend-with-bespoke-RPCs failed `next build` with
  `Module not found: Can't resolve '@/lib/mock-transport'`. The
  generator now always writes `mock-transport.ts` for each Next.js
  frontend: the rich CRUD-dispatch template when transportEntities are
  available, or a no-op stub (throws on call) otherwise. The stub
  satisfies webpack's static analysis and gives a clear runtime error
  if NEXT_PUBLIC_MOCK_API=true is set on a project with nothing to
  mock. **Where:** `internal/cli/generate_frontend_mocks.go` (added
  `mockTransportStubContent` const + `emitMockTransportStubs` helper;
  per-frontend block now always writes the file).

- **Codegen pipeline now checksums its own outputs (2026-04-30).**
  Closed the long-standing orphan-flagging gap by extracting
  `FileChecksums` + `WriteGeneratedFile` into a new
  `internal/checksums` subpackage (broke the `codegen → generator`
  import cycle that previously forced raw `os.WriteFile` calls in
  `internal/codegen/*.go`). The `internal/generator` package re-exports
  the symbols as type aliases so existing call sites compile unchanged.
  Threaded `*checksums.FileChecksums` through every codegen entry
  point: `GenerateAuthMiddleware`, `GenerateTenantMiddleware`,
  `GenerateAuthorizer`, `GenerateConfigLoader`, `GenerateCmdServer*`,
  `GenerateDeployConfig`, `GenerateCRUDHandlers`, `GenerateCRUDTests`,
  `GenerateMissingHandlerStubs`, `GenerateBootstrap`,
  `GenerateBootstrapTesting`, `GenerateMigrate`, plus
  `generateWebhookRoutes` in the cli layer. ~17 production emit sites
  converted (the remaining `os.WriteFile` calls in
  `internal/codegen/*.go` are user-owned scaffold files that
  intentionally aren't tracked: `service.go`, `handlers.go`,
  `authorizer.go`, `setup.go`, the `*_test.go` placeholders that
  become user-owned after first edit, and the `ensureDepsDBField`
  in-place mutation of the user's service.go).

  Added a `rehashTrackedFiles` post-pass after `runGoimportsOnGenerated`
  in `internal/cli/generate.go` so goimports' import-group rewrite
  doesn't show up as user-edit drift in `forge audit` (the
  pre-formatter render is preserved in the History array; only the
  current Hash is refreshed). Without this, every Go file the codegen
  pipeline writes would show as "user-edited" on the next audit.

  Validation: `forge new audit-test --kind service --service api ...`
  followed by `forge audit` reports `56 tracked, 0 modified, 0 orphans`
  (pre-fix: many orphans across `handlers/`, `pkg/middleware/`,
  `pkg/app/`).

  **Where:**
  - New: `internal/checksums/checksums.go` (canonical home for
    `FileChecksums`, `WriteGeneratedFile`, history-bound `RecordFile`,
    legacy-shape unmarshal).
  - Modified: `internal/generator/checksums.go` (now a thin shim of
    type aliases + re-exports), `internal/codegen/{auth,tenant,authz,
    config,deploy_config,crud}_gen.go`, `internal/codegen/generator.go`
    (most emit-site updates), `internal/cli/{generate,
    generate_bootstrap,generate_services,generate_middleware,
    generate_tools}.go` (caller threading + the rehash pass).

- **Strict internal-package contract names — lint-and-fail-fast
  (2026-04-30).** Closed the standing "non-canonical contract names
  produce silently-broken bootstrap codegen" item with a new
  `forgeconv-internal-package-contract-names` analyzer. The analyzer
  walks `internal/<pkg>/contract.go` files and asserts:
  - `type Service interface { ... }` exists,
  - `type Deps struct { ... }` exists,
  - `func New(Deps) Service` exists with that exact signature
    (pointer-Deps shapes are intentionally rejected — the bootstrap
    template emits `<pkg>.New(<pkg>.Deps{})`, a value).

  Wired in two places: `forge lint` (and `forge lint --conventions`)
  surfaces findings the same way the existing forgeconv proto rules
  do; `forge generate` runs the same pass as a Step 0c pre-codegen
  validation BEFORE any generators emit files, so the user sees the
  error at their `contract.go` rather than chasing a confusing build
  break in `pkg/app/bootstrap.go`. Findings carry the same actionable
  sentinel — `forge convention: internal-package contracts must
  declare 'type Service interface', 'type Deps struct', and 'func
  New(Deps) Service'` — and the offending name (`Sender`/`Config`/
  `NewSender`) so the rename is mechanical.

  Honors `contracts.exclude` from `forge.yaml` so analyzer-style
  packages, embed wrappers, and `internal/cli` itself can keep
  alternate shapes without false-positive findings.

  Five forgeconv tests cover the rule (canonical-clean, three-findings
  on a Sender/Config/NewSender contract, exclude-honored, no-internal
  no-op, pointer-Deps rejected). Three more in
  `internal/cli/precodegen_contract_test.go` exercise the
  pre-codegen integration. Migration skill at
  `migration/v0.x-to-strict-contract-names`; cross-referenced from
  `migration/upgrade`.

  **Where:**
  - New: `internal/linter/forgeconv/internal_pkg_contract.go`,
    `internal/linter/forgeconv/testdata/contracts_{good,bad,excluded}/`,
    `internal/cli/precodegen_contract_test.go`,
    `internal/templates/project/skills/forge/migration/v0.x-to-strict-contract-names/SKILL.md`.
  - Modified: `internal/cli/lint.go` (runConventionLint runs both
    proto and contract analyzers), `internal/cli/generate.go`
    (`preCodegenContractCheck` Step 0c),
    `internal/linter/forgeconv/forgeconv_test.go` (5 new tests).

- **Drift checksum gap: stale-codegen vs user-modified (2026-04-30).**
  `.forge/checksums.json` per-file entries now carry a deduplicated,
  bounded (20-entry) list of every checksum forge has ever rendered for
  that path, in addition to the current `Hash`. `forge upgrade`'s
  Tier-2 comparison promotes "matches any prior render" to the
  auto-update path, not just "matches the current Hash". The result:
  template upgrades that produce stale codegen on disk (e.g. a
  doc-comment normalization that drifted `pkg/middleware/requestid.go`
  in the wild) are recognised as forge-rendered content and updated
  cleanly without `--force`. JSON wire format is forward-compatible:
  legacy flat-string entries (`path -> hex`) are accepted on load and
  promoted to structured entries; structured entries are always emitted
  on save. `internal/checksums.MatchesAnyKnownRender` is the new helper
  that the upgrade path consults. Migration skill at
  `migration/v0.x-to-checksum-history` documents the (transparent)
  migration; `migration/upgrade` "See also" cross-references it. New
  unit tests cover the history dedupe / bound, IsFileModified accepting
  prior renders, legacy-shape load promotion, and the end-to-end
  stale-codegen Upgrade scenario. **Where:**
  `internal/checksums/checksums.go`,
  `internal/generator/upgrade.go:Upgrade`,
  `internal/generator/checksums_test.go`,
  `internal/generator/upgrade_test.go`,
  `internal/templates/project/skills/forge/migration/v0.x-to-checksum-history/SKILL.md`.

- **Re-record frozen checksums after `bootstrapGeneratedCode` (2026-04-30).**
  `runNew` now calls a new `generator.RecordFrozenChecksums` after
  `bootstrapGeneratedCode` runs `goimports -w pkg/...`. Without this,
  Go 1.19+ doc-comment normalization drifted `pkg/middleware/*.go` from
  the embedded-template bytes, and `forge upgrade --dry-run` flagged
  `pkg/middleware/requestid.go` as user-modified on a fresh service
  scaffold. Validated: fresh `forge new svctest` now reports 0
  user-modified files (was 1).

- **`forge upgrade` Taskfile.yml + managed-file plan now kind-aware
  (2026-04-30).** Three related fixes from one validation run:
  1. `managedFilesForCfg` reads `cfg.EffectiveKind()` and picks the
     right Taskfile template (`Taskfile.yml.tmpl` for service,
     `Taskfile.cli.yml.tmpl` for CLI, `Taskfile.library.yml.tmpl` for
     library). Without this, CLI/library projects' upgrade dry-run
     produced a 100+ line bogus diff that would have replaced the
     kind-correct Taskfile with the service one.
  2. `fileEnabledByFeatures` gates `cmd/*`, `pkg/middleware/*`,
     `Dockerfile`, `docker-compose.yml`, and `deploy/alloy-config.alloy`
     to service-kind only. Previously the upgrade plan included those
     paths for all kinds, surfacing additional bogus diffs on CLI
     projects.
  3. `recordFrozenChecksums` moved to the end of `Generate()` so every
     Tier-2 file (notably `.golangci.yml` written by
     `generateGolangciLint` later in the flow) is on disk by the time
     its checksum is taken. Pre-fix the checksum file omitted
     `.golangci.yml` and 4 other late-written files, surfacing them as
     "user-modified" on a fresh scaffold.

  Validation: fresh `forge new clitest --kind cli` now upgrade-clean
  (0 user-modified, 2 auto-update for trailing-newline drift, 1
  up-to-date). Fresh `forge new svctest --kind service` drops from
  many false-positives to 1 residual (requestid.go via goimports — see
  Open).

- **`handlers/<svc>/authorizer_gen.go` → `forge/pkg/authz` interface
  shim (Wave 3 hybrid migration, 2026-04-30).** Slimmed the generated
  per-service authorizer from ~110 lines (inline matching logic +
  proto-derived data maps) to ~35 fixed lines + one row per RPC, with
  the matching logic moved into `forge/pkg/authz`. The library is
  interface-driven: callers implement
  `Decider.Decide(ctx, method, claims) error` and pass it to
  `authz.New(d)` to get an `*authz.Authorizer` that implements the
  project's `middleware.Authorizer` interface (`Can` + `CanAccess`).
  The library handles panic recovery, connect.Error normalisation
  (decider plain errors → `CodePermissionDenied`; nil-claims rewrap on
  `Can` → `CodeUnauthenticated`), and claims-from-context dispatch via
  a `SetClaimsLookup` hook the generated `init()` wires.

  **Library shape.** Three top-level types in `forge/pkg/authz/authz.go`:
  - `Decider` (interface) + `DeciderFunc` adapter — the user-extension
    point.
  - `Authorizer` + `New(d Decider)` — the boundary type implementing
    `middleware.Authorizer`. New panics on nil decider so misconstruction
    surfaces at boot.
  - `RolesDecider` — a built-in implementation that reads
    `MethodRoles` (proto `required_roles`) and `MethodAuthRequired`
    (proto `auth_required`); the generated shim uses this. `DenyAll`
    and `AllowAll` cover stub/test paths.

  Behavioural fingerprints preserved exactly: empty procedure denies,
  unknown procedure denies (fail-closed), `auth_required: false`
  passes through, `required_roles` allow-list matches against
  `claims.Role` and `claims.Roles[]`. `TestAuthorizerDenyByDefault`
  in the unit_test.go scaffold passes verbatim against the new shim.

  **Shim shape.** `handlers/<svc>/authorizer_gen.go` now emits:
  ```go
  type GeneratedAuthorizer = authz.Authorizer
  func NewGeneratedAuthorizer() *GeneratedAuthorizer {
      return authz.New(authz.RolesDecider{
          MethodRoles:        methodRoles,
          MethodAuthRequired: methodAuthRequired,
      })
  }
  ```
  Plus the two data maps populated from proto annotations and an
  `init()` that wires `middleware.ClaimsFromContext` into the library
  (sidesteps the `middleware → authz → middleware` import cycle).

  **Smoke verified.** Fresh `forge new authztest --kind service
  --service api --mod github.com/example/authztest` produces a
  65-line `authorizer_gen.go` (down from ~110), `go build ./...`
  is clean, and the unit-test scaffold's `TestAuthorizerDenyByDefault`
  still passes. Library covered by 16 tests in
  `forge/pkg/authz/authz_test.go` including deny-by-default,
  allow-rule matched, allow-rule fallthrough, panic recovery,
  nil-claims rewrap, and `RolesDecider.MethodAuthRequired` opt-out.

  **Where:**
  - New: `forge/pkg/authz/authz.go`, `forge/pkg/authz/authz_test.go`.
  - Modified: `internal/templates/service/authorizer_gen.go.tmpl`
    (109 → 63 lines).
  - New skill:
    `internal/templates/project/skills/forge/migration/v0.x-to-authz-lib/SKILL.md`.
  - Updated: `migration/upgrade/SKILL.md` "See also" to reference it,
    `CODEGEN_AUDIT.md` row + status banner.

- **`handlers_crud_gen_test.go` → `tdd.RunRPCCases` shim (Wave 3
  hybrid migration, 2026-04-30).** Slimmed the generated CRUD test
  scaffold from ~15 lines/RPC of inline `_, err := svc.<Method>(...);
  _ = err` boilerplate to ~9 lines/RPC of `tdd.RunRPCCases` delegation.
  Per-RPC test body now constructs a `[]tdd.RPCCase[Req, Resp]` slice
  and hands it to the runner, which carries the harness construction
  (subtest fanout, `connect.CodeOf` error matching, default-context
  fallback, ordered per-row `Setup`). The runner, case type, and
  `AssertConnectError` helper already lived in `forge/pkg/tdd`; this
  session adds `RunRPCCases` / `RPCCase` codegen-facing aliases (alias
  of `TableRPC` / `Case`) so the generated identifier matches the
  migration skill, then refactors
  `handlers_crud_test_gen.go.tmpl` to emit row-driven tests.
  - **Where:** `pkg/tdd/rpc.go` (alias additions), `pkg/tdd/rpc_test.go`
    (alias + ordered-setup fingerprint tests),
    `internal/templates/service/handlers_crud_test_gen.go.tmpl`
    (template rewrite), `internal/codegen/crud_gen_test.go`
    (rendered-shape fingerprint test plus `go/parser`-based syntactic
    validity check).
  - **Migration:** `migration/v0.x-to-tdd-rpccases` skill at
    `internal/templates/project/skills/forge/migration/v0.x-to-tdd-rpccases/SKILL.md`.
    Cross-referenced from `migration/upgrade`.
  - **Audit row:** `CODEGEN_AUDIT.md` row for
    `handlers/<svc>/handlers_crud_test_gen.go` flipped to "shipped"
    with new line-count column (~15/RPC → ~9/RPC).

- **`forge/pkg/testkit` shipped (Wave 3 hybrid extraction, 2026-04-30).**
  `pkg/app/bootstrap_testing.go.tmpl` no longer inlines the `testAuthorizer`
  struct, the `newTestDB` SQLite helper, the `slog.New(io.Discard, …)`
  literal, the `httptest.NewServer + t.Cleanup(srv.Close)` pair, or the
  `WithTestTenant` body. Each is now a one-line call into
  `forge/pkg/testkit`:
  - `testkit.DiscardLogger()` returns the discard slog.
  - `testkit.NewSQLiteMemDB(t)` returns an `orm.Context` with the SQLite
    driver + dialect blank-imported by the library so projects no longer
    need either import.
  - `testkit.NewTestServer(t, registerFn)` runs an httptest.Server and
    invokes `registerFn(mux)` so the per-service Connect registration
    can stay project-local.
  - `testkit.PermissiveAuthorizer{}` satisfies the project's
    `middleware.Authorizer` (made possible by the `Claims = auth.Claims`
    alias from the `auth` migration).
  - `testkit.WithTestTenant(ctx, id)` writes to the same context key
    `pkg/tenant`'s interceptor reads from.
  Per-project net shrink: ~30% of the boilerplate region of
  `pkg/app/testing.go` (97 lines on a fresh single-service scaffold,
  down from ~120). Wiring stays codegen because each project's per-service
  `Deps` shape is unique. Audit row updated to `shipped (hybrid)`;
  migration skill at `migration/v0.x-to-testkit`.

- **Drift detector trailing-newline normalization (2026-04-30).**
  `forge upgrade` was reporting 11 of 36 managed files as user-modified
  on `control-plane-next`, but the diffs were all `+` of a single
  trailing `\n` — gofmt / editor-on-save adds one, the embedded
  templates don't carry one. `renderManagedFile` now passes its output
  through `ensureTrailingNewline` so the comparison and on-disk write
  both end with exactly one `\n`. Validated on control-plane-next:
  false-modified count dropped 12 → 3. The remaining 3 are legitimate
  divergences (project-customized `docker-compose.yml`, `.golangci.yml`
  + stale codegen in `pkg/middleware/requestid.go`).

- **`binary: shared` codegen mode (Layer B, 2026-04-30).** Added
  `binary: shared` first-class support to forge. New projects can
  scaffold with `forge new ... --binary shared --service A --service
  B`; existing projects opt in by setting `binary: shared` in
  `forge.yaml` and running `forge generate`. Branches:
  - `internal/config/config.go` — `Binary` field, `EffectiveBinary()`,
    `IsBinaryShared()`, `ProjectBinaryShared` / `ProjectBinaryPerService`
    constants.
  - `internal/cli/new.go` — `--binary` flag, validated /
    normalized in `validateNewArgs`, plumbed to `runNew` →
    `gen.Binary` + `gen.AdditionalServices` (so all services scaffold
    in one shot for shared mode).
  - `internal/generator/project.go` — `Binary` + `AdditionalServices`
    fields on `ProjectGenerator`; `isBinaryShared()` gates the cmd/
    template choice and `generateSharedSubcommands()` emits one
    `cmd/<svc>.go` cobra subcommand per service.
  - `internal/generator/upgrade.go` — `managedFilesForCfg(cfg)` swaps
    `cmd-root.go.tmpl` for `cmd-shared-main.go.tmpl` based on
    `cfg.EffectiveBinary()`. Tier-1 regeneration via
    `RegenerateInfraFiles` honors this so `forge generate` cycles
    don't clobber the shared-binary main.
  - `internal/generator/project_deploy.go` — `binary: shared`
    selects the `kcl/<env>/main-shared.k.tmpl` variants and renders
    the full project's services as `SubCommandService` entries via
    `render.multi_service_apps(...)`.
  - `internal/templates/project/bootstrap.go.tmpl` — `BinaryShared`
    branch in `BootstrapOnly` constructs services lazily inside their
    name-gated blocks. Per-service mode keeps the prior shape.
  - `internal/templates/project/cmd-shared-main.go.tmpl`,
    `cmd-shared-service.go.tmpl` — new templates for the cobra root
    + per-service subcommand stubs.
  - `internal/templates/deploy/kcl/{dev,staging,prod}/main-shared.k.tmpl`
    — `MultiServiceApplication` per-env variants.

  KCL emits one `MultiServiceApplication` block (one image: shared,
  N `SubCommandService` entries) instead of N `Application` blocks.
  Cuts image build/push from N to 1; lazy bootstrap construction
  means `./<bin> api` boots only the api service's deps. See the
  new migration skill at
  `internal/templates/project/skills/forge/migration/v0.x-to-binary-shared/SKILL.md`
  for migration steps and trade-offs. Verified end-to-end (fresh
  `forge new --binary shared` builds a working binary; KCL produces
  one image with N Deployments). The `architecture` skill's "Binary
  modes" section was expanded to describe both modes side-by-side;
  the `deploy` skill cross-references the migration skill.

- **P4 — Skills consolidation: skills proactive, lint reactive
  (2026-04-30).** Deleted `proto/conventions` (a near-1:1 restatement
  of the four `forgeconv-*` lint rules). Merged its unique content
  (canonical entity shape, multi-tenant patterns, cross-service shared
  types) into `proto`, which now also carries explicit "Enforced by:
  forgeconv-*" markers per rule. Cross-references in `proto-split`,
  `api/handlers`, `frontend/pages`, and `reliant.md.tmpl` were
  redirected. Skill count: 38 → 37. The audit (in `SKILLS_AUDIT.md`)
  confirmed every other skill is genuine methodology/cookbook content
  that lint cannot capture, so no other collapses were warranted.
  See `MIGRATION_TIPS.md` "Skills consolidation" for the full writeup.

- **P5 — Proto-first context: greenfield vs migrated (2026-04-30).**
  Made the proto-vs-migration authoritativeness boundary explicit in
  the `proto`, `db`, and `migration` skills. The `forge audit` agent's
  `proto_migration_alignment` check already detects the divergence;
  its hint now points at the relevant skills + enumerates the three
  resolutions (drop proto entities, `forge db proto sync-from-db`,
  roll migration forward). No code changes beyond the audit hint —
  this was a docs alignment task. See `MIGRATION_TIPS.md` "Proto-first
  context" for the full writeup.

- **Per-version migration skills: upgrade path complete (2026-04-30).**
  Authored the two missing `migration/v0.x-to-*` skills so
  `forge upgrade` from a 0.0.0-pinned project surfaces every shape
  change shipped this session:
  - `migration/v0.x-to-pack-starter-split` — stripe / twilio /
    clerk-webhook demoted from packs to one-time-copy starters.
  - `migration/v0.x-to-env-config` — hand-curated KCL `<NAME>_ENV`
    groups → `forge.yaml environments[].config` + sensitive-field
    projection via proto `(forge.v1.config) = { sensitive, category }`.

  Both follow the canonical six-section shape of
  `migration/v0.x-to-contractkit` (what changed / detection /
  deterministic / manual / verification / rollback). The
  `migration/upgrade` skill's "See also" now lists all five per-version
  skills (contractkit, observe-libs, crud-lib, pack-starter-split,
  env-config). Closes the implicit "two skills pending" gap from the
  original upgrade-story entry.

- **MultiServiceApplication KCL schema (Layer A, 2026-04-30).** Added
  `MultiServiceApplication` and `SubCommandService` schemas to
  `internal/templates/deploy/kcl/schema.k` plus the
  `render.multi_service_apps(mapp)` helper that expands the
  multi-service struct into a `{name: Application}` map suitable for
  `Environment.applications`. Lets a project that ships one Go binary
  with N cobra subcommands describe its deploy as one image + N
  Deployments instead of N copies of `Application`. Cuts image
  build/push from N to 1 — control-plane-next sized this at 11
  services → 1 image.

  Compose pattern (smoke-tested end-to-end with kubectl
  `--dry-run=client`):

  ```kcl
  multi = schema.MultiServiceApplication { ... }
  env = schema.Environment {
      applications = render.multi_service_apps(multi)
  }
  manifests = render.render_environment(env)
  ```

  Each child Application inherits the parent's `image`, `command`,
  `shared_env_vars`, `labels`, `annotations` and merges per-service
  overrides; per-service `args` becomes the cobra subcommand. `deploy`
  skill SKILL.md gained a "MultiServiceApplication" section.

  Layer B (`binary: shared` codegen mode — one Go binary, cobra
  subcommand per service, bootstrap-per-service, deploy auto-defaulting
  to MultiServiceApplication) is **deferred** — see Open above for the
  scope notes. Layer A alone is sufficient to express the deploy shape
  declaratively; Layer B is a polish on top.

- **Pack count reduction — business integrations split into starters
  (2026-04-30).** The `stripe`, `twilio`, and `clerk-webhook` slices of
  the pack roster moved out of `forge pack` and into a new `forge
  starter` command. Per-project divergence on those three was 100%
  (every control-plane and migration we touched rewrote the
  forge-emitted code), so central maintenance was creating more bugs
  than it prevented (stripe proto pkg, twilio template-escape, clerk
  svix import all bit us this cycle).

  The split:

  - `stripe` → `forge starter add stripe --service <svc>` (no proto
    entities — projects wire their own data model).
  - `twilio` → `forge starter add twilio --service <svc>` (no proto
    entities).
  - `clerk` pack split: JWKS validator + Connect interceptor + dev-mode
    bypass **stay as pack** (pure infrastructure). Webhook user-sync
    moved to `forge starter add clerk-webhook` (every project's user
    table differs).

  New surface: `internal/starters/` with a one-page `starter.yaml`
  manifest (name, description, deps, files, notes — no migrations, no
  install lifecycle). `forge starter add` copies files, echoes deps for
  the user to install on their schedule, prints post-install notes,
  exits. No `forge.yaml` mutation, no `go mod tidy`, no
  re-rendering on `forge generate` — the user owns every line.

  Final pack roster: `jwt-auth`, `clerk`, `firebase-auth`, `api-key`,
  `audit-log`, `nats`, `auth-ui`, `data-table` (8 packs, down from 10).

  Closes the implicit "every Stripe/Twilio integration diverges" item
  this session uncovered. Pack-vs-starter framing landed in the `packs`
  skill (and a new `starters` skill); migration tips appended in
  `MIGRATION_TIPS.md`. Existing projects with `stripe:` / `twilio:`
  under `packs:` warn-and-skip on the next `forge generate` — no
  destructive cleanup, the user's already-customized handler code is
  untouched.

- **Six-pack of small forge improvements (2026-04-30, post-cutover).** All
  six items from a single batch landed.

  - `forge db migrate {up,down,status,version,force}` now read the
    `DATABASE_URL` env var as a fallback when `--dsn` is omitted, removing
    the friction of having to paste the dsn flag for every db command. The
    flag still wins when both are set. New helper `resolveDSN` in
    `internal/cli/db.go` plus a unit test in `db_test.go`.

  - `forge deploy <env> --dry-run` now skips the dev-only k3d
    bootstrap + `docker build`/`docker push` step; dry-run only renders
    + prints manifests so the slow image step was dead weight. Fix in
    `internal/cli/deploy.go::runDeploy` (gated `if envName == "dev"
    && !dryRun`). Smoke-tested at ~74ms vs. tens of seconds previously.

  - `pack_overrides.<name>.skip_migrations: true` in `forge.yaml`
    declines a pack's shipped migrations at install time, for projects
    whose own migrations supersede the pack's (the audit-log + api-key
    case in control-plane-next). New `PackOverride` block in
    `internal/config/config.go`; honoured in
    `internal/packs/pack.go::InstallWithConfig`. Pack files,
    dependencies, and generate hooks still install — only migrations
    are skipped. Smoke-tested with audit-log on a fresh project.

  - `forge add operator <name> --with-placeholder-crd --api-package
    <pkg> --crd-type <Type>` now scaffolds the CRD type into a separate
    `api/<version>/<pkg>/types.go` (`package <pkg>`), with the
    controller importing it via `apipkg`. Lets the operator binary and
    the CRD type diverge — `workspace-controller` reconciling
    `Workspace` instead of `WorkspaceController`. New
    `GenerateOperatorFilesWithAPI` in `internal/generator/operator_gen.go`
    plus template updates in `internal/templates/operator/*.tmpl` (an
    `if .SplitAPI` switch flips controller imports to the apipkg
    namespace). Coexists with the parallel `forge add crd` agent's
    `--with-placeholder-crd` legacy gating.

  - Three new base UI primitives: `RowActionsMenu` (kebab trigger +
    suspend/resume/delete dropdown for admin tables), `ProgressBar`
    (value/max with auto-tint at >80%, for billing/quota), and
    `StatusDot` (colored dot + label, for dense status cells). Files in
    `forge/components/components/ui/{row_actions_menu,progress_bar,status_dot}.tsx`,
    registered in `library.go` and added to `coreComponents` in
    `internal/generator/frontend_gen.go`. UI category 40 → 43, total
    69 → 72. `library_test.go` counts updated.

  - `audit-log` pack now ships a read-side `ListAuditEvents` Connect
    RPC alongside the existing write-side interceptor. New
    `proto/audit/v1/audit_log.proto` + `pkg/middleware/audit/auditlog/handler.go`
    backed by the existing `pkg/audit.AuditStore`. Pack version bumped
    to 1.1.0. Lets every project that installs audit-log surface a free
    admin view (and unblocks the control-plane-next audit page from its
    stub state). Pack manifest in
    `internal/packs/audit-log/pack.yaml` updated; new templates beside it.

- **`forge lint --contract` honours `go.work` for self-hosted modules
  (2026-04-30, post-cutover).** When forge is its own forge project,
  the contract analyzer subprocess was hard-coding `GOWORK=off` and
  `GOFLAGS=-mod=mod`, which causes Go to fall back to the published
  `forge/pkg @ v0.0.0-...` (no contractkit/auth/tenant/tdd) instead of
  the local `pkg/` checkout the workspace points at. Fixed in
  `internal/cli/lint.go` by adding a `hasWorkspaceGoMod()` walk-up
  helper; when a `go.work` is found, the GOWORK/GOFLAGS overrides
  are skipped (workspace mode is incompatible with `-mod=mod`
  anyway). The GOPROXY default still applies.

- **`contract_test.go` scaffold respects non-canonical interface
  names (2026-04-30, post-cutover).** The scaffold previously emitted
  `pkg.New(pkg.Deps{})` for any single-interface package, regardless
  of the interface's name. Forge's own `internal/packs` declares a
  `Manager` interface, not `Service`, which fails to compile against
  the canonical scaffold. Fixed in `generate_middleware.go`: when a
  single-interface contract names something other than `Service`,
  the scaffold is skipped with an `ℹ️` log line ("interface %q is
  not the canonical Service shape; write tests manually"). The
  multi-interface skip path is unchanged.

- **`contracts.exclude` widened for the two new analyzer linters
  (2026-04-30, post-cutover).** Forge's own `forge.yaml` was missing
  exclusions for `internal/linter/forgeconv` and
  `internal/linter/frontendpacklint`, both of which use the
  Go analysis framework's exported-`Analyzer` package-var idiom (same
  shape as the three already-excluded analyzer sub-packages). Both
  added with one-line rationale comments.

- **Base UI primitive library + frontend pack refactor (2026-04-30).**
  Closed both backlog items about the missing base UI primitives in one
  pass.

  **What landed.** Eight new low-level primitives were added under
  `forge/components/components/ui/`: `button`, `input`, `label`, `form`
  (with `FormField` / `FormError` / `FormActions` subcomponents), `card`
  (with `CardHeader` / `CardBody` / `CardFooter`), `table` (with
  `TableHeader` / `TableBody` / `TableRow` / `TableHead` / `TableCell`),
  `select`, `chip`. Three primitives that already existed in the library
  but were not part of the scaffold's `coreComponents` (`avatar`,
  `tabs`, `toast_notification` — the last was already there) were
  promoted into the scaffold so every forge frontend ships the full
  primitive set without further configuration. All eleven primitives
  install as `overwrite: once`, so they are user-owned after the first
  scaffold.

  **Pack refactor.** Both shipped frontend packs were rewritten to
  compose only the base library:

  - `data-table`: `DataTable.tsx.tmpl` now imports `Table` /
    `TableHeader` / `TableBody` / `TableRow` / `TableHead` / `TableCell`
    from `@/components/ui/table` instead of inlining `<table>` markup.
    `filters.tsx.tmpl` uses `Chip` (filter pills), `Button` (clear-all),
    `SearchInput` (search box). `pagination.tsx.tmpl` uses `Select`
    (page-size picker) and `Button` (Prev/Next fallback) on top of the
    existing `BasePagination`. `@tanstack/react-table` remains
    allowlisted as the headless sort/filter engine.
  - `auth-ui`: `LoginForm.tsx.tmpl`, `SignupForm.tsx.tmpl`,
    `SessionNav.tsx.tmpl` all rewritten on top of `Card`, `Form` +
    `FormField` + `FormError`, `Input`, `Label`, `Button`,
    `AlertBanner`, and (for SessionNav) `Avatar`. `react-hook-form`,
    `zod`, and `zustand` remain as utility (non-UI) deps.

  **Lint result.** `forge lint --frontend-packs` reports zero warnings
  against either pack. `frontendpacklint` is now backed by real
  enforcement — the convention "frontend packs reuse base library" is
  no longer aspirational.

  **Smoke.** Fresh `forge new` + scaffold yields
  `frontends/web/src/components/ui/{button,input,label,form,card,avatar,toast_notification,tabs,table,select,chip}.tsx`
  plus the existing higher-level components. `tsc --noEmit` clean,
  `npm run lint` clean (only two pre-existing warnings in
  `avatar.tsx` / `skeleton_loader.tsx` from the prior baseline,
  unrelated to the new primitives). `forge pack install auth-ui` and
  `forge pack install data-table` install cleanly into the scaffold;
  every import in the rendered files points at `@/components/ui/*`.

  **Where:**
  - New primitives:
    `forge/components/components/ui/{button,input,label,form,card,table,select,chip}.tsx`.
  - Registry: `forge/components/library.go` (registry entries) +
    `forge/components/library_test.go` (UI category count
    32 → 40, total 61 → 69).
  - Scaffold list: `forge/internal/generator/frontend_gen.go`
    `coreComponents` split into a "primitives" group + "domain
    components" group.
  - Pack refactors:
    `forge/internal/packs/data-table/templates/{DataTable,filters,pagination}.tsx.tmpl`,
    `forge/internal/packs/auth-ui/templates/{LoginForm,SignupForm,SessionNav}.tsx.tmpl`.
  - Docs: `frontend` skill (lists primitives), `pack-development` skill
    ("import from base library" rule promoted from aspirational to
    enforced), `MIGRATION_TIPS.md` (reference-refactor section
    expanded).



- **`contract_test.go` scaffold for nested-path packages and
  multi-interface skip (2026-04-30).** Two correctness issues in
  `generateInternalPackageContracts` (`internal/cli/generate_middleware.go`):
  1. Nested package paths (e.g. `internal/mcp/database`) were importing
     `<module>/internal/database` (leaf only), producing a non-existent
     import. Fixed by computing `ImportPath` from the module-relative
     directory (strip the leading `internal/` segment) and threading it
     into the template (`{{$.Module}}/internal/{{.ImportPath}}`).
  2. Multi-interface packages (more than one `type ... interface` in
     `contract.go`) were getting a scaffold whose `pkg.New(pkg.Deps{})`
     constructor call was ambiguous and frequently wrong. Fixed by
     re-parsing the contract via `contract.ParseContract` and skipping
     the scaffold when `len(cf.Interfaces) > 1`, with a `ℹ️` log line so
     the user knows to write tests by hand. The `forge package new`
     code path was also updated to pass `ImportPath` (mirrors `Name` for
     now since flat-only names are accepted today).

  Verified: `forge new ctest && forge generate` with a hand-created
  `internal/mcp/database/contract.go` now produces a `contract_test.go`
  importing `<module>/internal/mcp/database`. A second project with two
  interfaces in `contract.go` skips the scaffold and prints the
  expected `ℹ️ Skipped contract_test.go scaffold for internal/multi/`.

- **Webhook `webhook_routes_gen.go` `//go:build ignore` was a
  false-positive bug (2026-04-30).** The audit flagged the template
  header as making generated webhook routes inert. Investigation:
  `templates.stripBuildIgnore` runs inside `RenderFromFS` and
  unconditionally drops the directive at write time, so user projects'
  `webhook_routes_gen.go` files do compile and routes do wire up.
  Confirmed by `TestRenderTemplate_StripsBuildIgnoreFromRenderedOutput`
  and an end-to-end `forge new` + `forge add webhook` + `forge generate`
  smoke (the rendered file starts at `// Code generated …`).
  CODEGEN_AUDIT.md updated to record the resolution.

- **Annotation-only ORM + forge convention lint suite (continuation,
  2026-04-30).** Two related cleanups landed together:

  **Codegen.** The protoc-gen-forge plugin no longer applies name-based
  heuristics on top of the `(forge.v1.entity)` / `(forge.v1.field)`
  annotation system. Removed:
  - `looksLikeEntity()` — entities now require an explicit
    `option (forge.v1.entity) = { ... }` (no more "looks like an entity
    by shape" inference for `proto/db/v1/` messages with an `id` field).
  - Default-PK-by-name — `id` is no longer auto-marked PK; every entity
    must declare its PK field with `[(forge.v1.field) = { pk: true }]`.
    Missing PK now produces a precise actionable error from `forge
    generate` (`entity "User" (in proto/...): missing primary key
    annotation: mark the PK field with [(forge.v1.field) = { pk: true }]`).
  - `applyFieldInferences()` — `_id` no longer auto-creates an FK
    reference, `email` no longer auto-implies unique, the
    `name/title/status/role/...` not-null heuristic is gone.
  - `inferEntityOptions()` — `timestamps: true` and `soft_delete: true`
    are no longer auto-toggled by detecting `created_at`/`deleted_at`/
    `updated_at` fields.
  - `entity_convert.go` `if f.Name == "id"` PK fallback — replaced with
    `entity.PkField`-driven logic so the actual annotated PK drives
    plan-entity construction regardless of name.

  **Lint.** New analyzer package `internal/linter/forgeconv/` wired into
  `forge lint` (and the standalone `--conventions` flag). Catches each
  failure mode with copy-pasteable remediation text:
  - `forgeconv-one-service-per-file` — one service per .proto, error.
  - `forgeconv-pk-annotation` — entity without `pk: true`, error.
  - `forgeconv-timestamps` — `*_at` Timestamp without `timestamps: true`
    on entity or field-level annotation, error.
  - `forgeconv-tenant-annotation` — entity has one `tenant: true` field
    AND another tenant-shaped name without the annotation, warning.

  Verified: scaffolded `lintok` project lints clean; injecting `service
  Foo {} service Bar {}` produces two findings; `proto/db/v1/example.proto`
  with `string id = 1;` (no `pk: true`) gets caught at lint AND at
  generate-time. control-plane-next still passes `forge generate` and
  `forge lint --conventions` clean. Covered by 8 tests in
  `internal/linter/forgeconv/forgeconv_test.go` plus the `protoc-gen-forge`
  e2e suite.

- **`nats` pack and frontend pack support (continuation 6).** Two packs
  shipped that recurred during the overnight migration:
  - `nats` — JetStream client + JSON-envelope publisher + durable
    pull-consumer with backoff/retry/DLQ. Distilled from
    `control-plane-next/internal/natsio/`. Lives under
    `pkg/clients/nats/`. Closes the standing
    "natsio-publisher should be a pack" item.
  - `data-table` — first **frontend** pack. Generic TanStack-Table-based
    component (sort/filter/paginate/search) wired to forge's
    auto-generated `useEntities` hooks. Installed into
    `frontends/<name>/src/components/data-table/`.

  To support frontend packs, the pack manifest schema gained:
  - `kind: frontend` (default still `go`, so existing manifests are
    untouched);
  - `npm_dependencies: []` — `npm install --save` runs once per
    frontend dir;
  - templated `output:` paths — output now passes through `text/template`
    so `{{.FrontendPath}}` / `{{.FrontendName}}` resolve per-frontend
    (no-op for plain paths).

  The installer iterates `cfg.Frontends` for frontend packs and skips
  `go get` / `go mod tidy`. control-plane-next's `internal/natsio/`
  predates the pack and may either migrate or stay bespoke — the
  recommendation is documented in `MIGRATION_TIPS.md`.

- **Cross-role import-alias collision in bootstrap.go / testing.go
  (continuation 5).** When a service `Package` collides with an
  internal package `Package` (e.g. `forge add service billing` in a
  project that already has `internal/billing/`), the codegen emitted
  two `import "<module>/handlers/billing"` and
  `"<module>/internal/billing"` lines aliased as the same identifier
  `billing`, plus duplicate `NewTestBilling` / `c.billingDeps` symbols
  in `pkg/app/testing.go`. Added an `Alias` field to
  `BootstrapComponentData` (and `VarName` to `BootstrapTestServiceData`)
  with a new `AssignBootstrapAliases` helper that detects cross-role
  collisions (services / packages / workers / operators) and assigns
  role-prefixed aliases — `svcBilling` for the service, `pkgBilling`
  for the package, etc. When there's no collision, `Alias = Package`
  and the rendered output is byte-identical to pre-fix. Templates
  rewritten to use `{{.Alias}}.X` for symbol references and
  `{{.Alias}} "<path>"` on import lines (Go accepts redundant aliases
  matching the leaf name). FieldName / VarName get the same
  disambiguation so `NewTest<FieldName>` / `c.<varName>Deps` stay
  unique. End-to-end verified: control-plane-next adds 7 services
  including `billing` (which collides with `internal/billing`) and
  emits clean compiling code.

- **Entity ORM auto-inference path-restricted to db.* proto packages
  (continuation 5).** `looksLikeEntity()` previously returned true
  for any non-Request/Response message with an `id` field, regardless
  of which proto file it lived in — despite a code comment that
  promised path-restriction to `proto/db/`. Adding a multi-service
  proto with messages like `Daemon`, `Plan`, `LLMKey`, `Subscription`
  caused forge to generate an `internal/db/<name>_orm.go` for each,
  colliding with hand-written postgres.go declarations. Added the
  guard: inference now only fires when the proto package starts
  with `db.` or equals `db`. Explicit `forge.v1.entity` annotations
  are still honored regardless of path —
  `TestE2EScaffoldEntityInServiceProto` covers that path.

- **types.go entity-alias file skipped when all entities are in
  proto/db (continuation 5).** The `if svcName != ""` guard in
  `generate.go` short-circuited the call to
  `GeneratePlanDBTypesFromEntities` whenever none of the descriptor's
  entities came from `proto/services/<name>/v1/`. Pack-contributed
  entities (stripe customer / invoice / payment intent /
  subscription) all live in `proto/db/v1/`, so projects whose only
  entities are pack-contributed got no `internal/db/types.go` and
  the pack's emitted `*_orm.go` files failed to compile with
  `undefined: StripeCustomer`. Removed the guard — `svcName` is now
  a hint-only fallback because every entity carries its real
  `ProtoFile` path through which `importPathForProtoFile` derives the
  correct `gen/<...>` import.

- **Frontend page generator gated on entity descriptor presence
  (continuation 5).** The CRUD-page generator emits a
  `frontends/<fe>/src/app/<slug>/page.tsx` (and detail/create/edit
  pages) for every service RPC that matches a `List<X>` /
  `Get<X>` / `Create<X>` / `Update<X>` / `Delete<X>` pattern. The
  templates assume specific field naming on responses
  (`ListXResponse.x`) that doesn't hold for non-entity-style
  list responses (`ListAvailableModelsResponse.models` not
  `availableModels`; `GetLLMKeyResponse.key` not `lLMKey`). Without a
  matching entity, the emitted pages are guaranteed broken.
  `generateFrontendPages` now takes an `entities []EntityDef`
  parameter and skips RPC-derived entity names that don't appear in
  the descriptor. Net effect: pages are produced only for genuine
  entities (typically pack-contributed or `proto/db/v1/`-defined
  entries), never for service RPCs that happen to match a CRUD prefix.

- **Stale codegen artifact cleanup (continuation 4).** `forge generate`
  now sweeps gen/services/<svc>/, handlers/<svc>/, handlers/mocks/<svc>_mock.go,
  and frontends/<fe>/src/hooks/<svc>-hooks.ts for proto services that no
  longer appear in the descriptor. Cleanup is conservative: only runs
  when `len(services) > 0` (avoids deleting fresh scaffolds before the
  first descriptor exists), only removes a handler dir wholesale when
  every file in it is a generated artifact (banner check or `_gen.go`
  suffix), and preserves user-owned files with a printed warning when
  a stale handler dir mixes user code with generated stubs. Six tests:
  `TestCleanupStaleArtifacts_RemovesStaleGenServices`,
  `TestCleanupStaleArtifacts_RemovesStaleHandlerDir_OnlyGenerated`,
  `TestCleanupStaleArtifacts_PreservesUserOwnedHandlerFiles`,
  `TestCleanupStaleArtifacts_RemovesStaleMocks`,
  `TestCleanupStaleArtifacts_RemovesStaleFrontendHooks`,
  `TestCleanupStaleArtifacts_NoServices_DoesNothing`. End-to-end smoke:
  control-plane-next regenerates clean with no spurious removals.

- **bootstrap_testing.go Connect imports derived from descriptor
  (continuation 4).** The bootstrap_testing template previously hard-coded
  the gen-package import as `<module>/gen/services/<pkg>/v1/<pkg>v1connect`,
  which broke whenever the proto's `go_package` option didn't follow the
  convention (multi-service-per-file, custom aliases). Enriched
  `BootstrapTestServiceData` with `ProtoConnectImportPath` (full Go
  import path) and `ProtoConnectPkg` (Go identifier) derived from
  `ServiceDef.GoPackage` + `PkgName`. Added a separate `ConnectImports`
  []string in the template data that is deduped before render, so two
  proto services sharing one go_package emit one import line. The
  per-service NewTest<X>Server helper now uses `ProtoConnectPkg` to
  reference the Connect client type. Verified end-to-end:
  control-plane-next testing.go compiles clean with the convention-
  aligned per-service paths preserved, and would correctly render
  with non-convention go_package values too.

- **Service-stub placeholder proto syntax was already correct
  (continuation 4 — false alarm).** The backlog item from continuation
  session 2 reported `(forge.v1.field) = { required: true }` syntax in
  service stub generation. Reproduction in continuation 4 with `forge
  new` and `forge add service` produced no `(forge.v1.field)` annotations
  at all in the placeholder — and where annotations DO exist (e.g.
  control-plane-next's hand-edited admin_server.proto), they correctly
  use `validate: { required: true }`. Marking as resolved with no code
  change required; possibly fixed by an in-flight template change before
  this session.

- **Descriptor concurrency: per-invocation fragments + parent-process
  merge.** Replaced the in-process `syscall.Flock` (which silently raced
  across separate buf-plugin processes) with per-invocation JSON
  fragments under `gen/.descriptor.d/<sha1>.json`, atomically written via
  `os.CreateTemp` + `os.Rename`. The parent process
  (`runDescriptorGenerate`) merges fragments into
  `gen/forge_descriptor.json` after `buf generate` returns, then removes
  the staging dir. Stable, content-addressable filenames make repeat
  invocations idempotent. Regression tests:
  `TestMergeDescriptorFragments_CombinesAllFragments`,
  `TestMergeDescriptorFragments_NoStageDir_NoError`. End-to-end verified
  by running `forge generate` against control-plane-next twice in
  sequence (formerly failed with `services: null`; now produces a
  complete descriptor on every run with no duplicates).

- **Kubebuilder controller-runtime SchemeBuilder exception.**
  `contractlint`'s exported-vars analyzer recognized
  `runtime.NewSchemeBuilder(...)` (classic kubebuilder) but flagged
  `&scheme.Builder{...}` (newer kubebuilder using
  `sigs.k8s.io/controller-runtime/pkg/scheme`). control-plane's
  `api/v1alpha1/groupversion_info.go` uses the latter, so the lint
  failed immediately on copying CRD types. Extended `isKubebuilderAPIVar`
  to accept the `&scheme.Builder{...}` shape via AST UnaryExpr +
  CompositeLit pattern. Regression test:
  `TestExportedVars_KubebuilderControllerRuntimeAPIExempt` plus dedicated
  testdata under `testdata/src/varsoperatorcr/` with stub `scheme` and
  `k8sschema` packages.

- **Pack subpath nesting (Phase 1).** Each pack's output now nests under a
  category subtree: auth providers → `pkg/middleware/auth/<provider>/`,
  audit pack → `pkg/middleware/audit/auditlog/`, external-service clients
  (stripe, twilio) → `pkg/clients/<service>/`. Added optional `subpath:`
  field to the pack manifest schema; surfaced in `forge pack list` as a
  SUBPATH column. Six-pack gauntlet (jwt-auth, api-key, stripe, audit-log,
  clerk, twilio) installs + builds + lints clean on a fresh scaffold.

- **contractlint: kubebuilder API vars exempt.** `contractlint` previously
  flagged `GroupVersion`, `SchemeBuilder`, and `AddToScheme` in operator
  packages — but k8s API machinery discovers these by name and wrapping
  them in a getter would silently break operator scheme registration.
  Added an exception in `internal/linter/contract/exported_vars.go` that
  matches by initializer (`schema.GroupVersion{...}`,
  `runtime.NewSchemeBuilder(...)`, `*.AddToScheme`). Regression test:
  `TestExportedVars_KubebuilderAPIExempt` in the contract analyzer suite.

- **Frontend stylelint: don't lint .tsx; allow Tailwind v4 import.** Two
  papercuts in the scaffolded Next.js frontend:
  1. `lint:styles` ran `stylelint "src/**/*.{css,tsx}"` but stylelint
     can't parse JSX — every `.tsx` file errored. Restricted to
     `src/**/*.css` in `internal/templates/frontend/nextjs/package.json.tmpl`.
  2. `stylelint-config-standard` enforces `import-notation: url(...)` but
     Tailwind v4's documented entry point is `@import "tailwindcss";`
     (string). Disabled `import-notation` in
     `internal/templates/frontend/nextjs/stylelint.config.mjs`.
  Verified: scaffolded `admin-web` now passes `npm run lint:styles` clean.
