# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-08-17

First minor release. The root CLI (`v0.1.0`) and the runtime library
(`pkg/v0.1.0`) are tagged together, as always.

### Added

- **The generated scaffold test row is falsifiable.** Every scaffolded
  handler ships a self-destructing test row that asserts the RPC is not
  implemented yet, so it goes red the moment you write the handler.
  Keyed on a bare `connect.CodeUnimplemented`, that row could never
  fail: a FINISHED handler can answer the same code for its own reasons
  — a forwarder, a feature-flagged path, or most commonly a nil-guard on
  an optional dep the test harness leaves unset — so the row passed
  forever against implemented RPCs. Observed in one project: 78 of 78
  integration rows green, none of them asserting anything.

  `svcerr.ErrScaffoldStub` / `svcerr.ScaffoldStub(rpc)` is a sentinel
  only forge's own untouched stub can produce, and `tdd.Case` gains
  `WantScaffoldStub` (backed by the exported `tdd.AssertScaffoldStub`)
  to match it. Replace the stub and the row fails, whatever the
  replacement returns. It is deliberately NOT matched by
  `svcerr.Unimplemented`, which remains the right answer for an RPC that
  is unimplemented on purpose.

  Identification is dual because the tiers differ: in process the error
  chain is intact and `errors.Is` matches, while through a real Connect
  client the error is marshalled and rebuilt, so only the
  `svcerr.ReasonScaffoldStub` metadata survives. Both are accepted, or
  unit rows and integration rows would mean different things under one
  field name.

  **Existing projects need no migration.** Stub excision still
  recognises the older `Unimplemented` and `CodeUnimplemented`
  spellings, so stubs already on disk stay excisable. Generated stubs no
  longer import `fmt` (the message is composed from the RPC name), and
  the AST import-fixer no longer adds it.

- **npm license gate for the published web-runtime.**
  `scripts/check-npm-licenses.sh` is the npm twin of `check-licenses.sh`
  — same allowlist-not-blocklist bar, same "an unrecognized license
  fails rather than passing quietly", same per-package exceptions that
  must state a reason. `web-runtime` is published to npm, so a
  non-permissive dependency would propagate into every project that
  installs it. The strong-copyleft family, LGPL included, is not
  exemptable through the allowlist. web-runtime passes with zero
  production dependencies.
- **Entity protos are dead: SQL is the schema language.** `forge scaffold
entity bookmark url:string title:string tags:[]string done:bool`
  emits the create-table migration (`db/migrations/NNNNN_create_*.sql`
  — `TEXT PRIMARY KEY CHECK (id <> '')`, NOT NULL + defaults, native
  arrays, `created_at`/`updated_at TIMESTAMPTZ DEFAULT (now())`;
  `--soft-delete`, `--no-timestamps`, `--no-rpcs`) and scaffolds the
  CRUD wire contract into the service proto once. `forge generate`
  shadow-applies `db/migrations` to a real ephemeral Postgres, introspects, and
  projects entity structs + ORM (`internal/db/<entity>_orm.go`, plain
  Go types: `time.Time`, pointers for nullable, native slices), CRUD
  wiring with generated `<entity>ToProto`/`<entity>FromProto`
  conversions, and frontend pages/nav/mocks — all from the APPLIED
  schema. Behavior is read off real columns: `deleted_at` ⇒ soft
  delete, `created_at`+`updated_at` ⇒ managed timestamps, text columns
  ⇒ search filter span.

### Removed

- **`deploy/kcl/components_gen.json`. KCL is the sole source of truth for
  deploy.** The generated component inventory is gone — the file, the
  `codegen.GenerateComponentsJSON` / `ComponentsToJSON` /
  `ComponentsJSONRelPath` Go API that wrote it, and the
  `fc.load_components` / `fc.load_migrate` / `fc.COMPONENTS_GEN` KCL
  loaders that read it. No alias, no shim, no fallback.

  What replaces it is `deploy/kcl/components.k`: one typed literal per
  component (`fc.Server {name = "billing", build = {...}}`), **scaffolded
  once and owned by the project from then on.** `forge scaffold
service|worker|binary|operator` APPENDS an entry; nothing forge does
  ever rewrites what is already there.

  Why: a file forge regenerated every run could not hold the per-env
  customization KCL exists for — bringing in components forge never
  generated (NATS, a cache, a sidecar), running a containerized database
  in dev against a hosted one in prod, changing a port per environment.
  It was also gitignored, so a FRESH CLONE rendered **zero manifests,
  silently**, until someone ran `forge generate`. Now every deploy file
  is tracked and hand-editable, and `kcl run deploy/kcl/dev` works on a
  clean checkout with nothing generated first.

  Two axes, each owned by one place: `components.k` says WHAT the system
  is made of (env-neutral — no ports, no replicas); `deploy/kcl/<env>/main.k`
  says HOW it runs in that environment (ports, replicas, resources,
  registry, secrets, env-only components).

  The deploy-time migration step moved with it: `migrate` is now stated
  literally per env (`migrate = ["/app/<project>", "db", "migrate", "up"]`)
  instead of being derived, so an environment that migrates out of band
  sets `migrate = []` and one that migrates differently says so.

  Forge still NOTICES drift — a component in the code with no entry in
  `components.k` — but it REPORTS it (`forge lint`, with the exact stanza
  to paste) rather than rewriting the file. Both directions have
  legitimate deliberate cases, so neither fails a build.

  **Migration:** add a `deploy/kcl/components.k` declaring your
  components, replace `for c in fc.load_components(fc.COMPONENTS_GEN)`
  with `for c in comps.COMPONENTS` (plus `import ..components as comps`)
  in each env's `main.k`, replace `fc.load_migrate(fc.COMPONENTS_GEN)`
  with the literal argv, and delete the gitignore entry. `forge generate`
  scaffolds `components.k` for you if it is absent.

- **The per-component port.** `config.ComponentConfig.Ports`,
  `config.PortSpec`, `config.HTTPPortName`, `ComponentConfig.PrimaryPort()`
  and the `ports` key in `deploy/kcl/components_gen.json` are gone — no
  alias, no shim. A component never had a port to carry: every service in
  a binary mounts onto the SAME Connect mux and the process listens once,
  on `AppConfig.port` (env `PORT`, default 8080 — now
  `config.DefaultServePort`). Any other port is a DEPLOY fact, declared
  per environment on `forge.components.Component.ports` in
  `deploy/kcl/<env>/main.k`, which is unchanged and still the place to
  state one. The rendered manifests and the `forge build` / `forge env`
  JSON contract are byte-identical across the change: nothing ever
  populated the field, so `components_gen.json` shipped `"ports": []` for
  every component and every reader hit its fallback.
- The `Port` column in the generated `docs/generated/architecture.md`
  component table. It printed `0` for every component in every project.
- The `(forge.v1.entity)` / `(forge.v1.field)` annotation handling,
  `protoc-gen-forge mode=orm` (`*.pb.orm.go`), `proto/db` scaffolding,
  proto-derived migrations (`--from-proto`, the boilerplate entity
  migration), `forge db proto`/`forge db codegen`, the
  `proto_migration_alignment` audit category, the
  `proto-orm-out-of-sync` lint, and the `internal/db/types.go` proto
  alias file. The annotation definitions remain in
  `forge/v1/forge.proto` as deprecated tombstones (ignored, with a
  generate-time notice) so existing projects keep compiling; their
  migrations were already the de-facto schema truth — see the
  `proto-entities-to-schema-truth` migration skill.
- Initial project scaffold generated by `forge project new`.
- `forge/pkg/serverkit`: extracted the uniform `cmd/server.go` lifecycle
  (HTTP listener, observability chain, healthz/readyz, worker supervisor,
  operator manager, graceful shutdown) into a reusable library. The
  generated `cmd/server.go` is now a ~50-line shim that projects the
  project's typed `*config.Config` onto `serverkit.Config` and wires
  per-project `Hooks` (Bootstrap, PostBootstrap, AutoMigrate, SetupOTel,
  ProjectInterceptors, CORS/SecurityHeaders/RequestID middleware factories).
  Cuts ~470 lines from every project's generated output.

### Changed

- **One custom RPC, one handler file.** The generate pipeline now
  scaffolds each unimplemented custom RPC into its own
  `internal/handlers/<svc>/rpc_<snake_name>.go` — package clause,
  imports, one `*Service` method, its `// forge:gen unwired-stub
symbol=<pkg>.<Method>` marker — instead of piling every RPC into one
  shared `handlers.go`. This is the same filename `forge scaffold rpc`
  has always written for an RPC that is not in the proto yet, so the two
  paths agree; previously the layout depended on whether you scaffolded
  the RPC before or after declaring it in the proto. A handler package's
  file layout is invisible to Go and to every forge reader (all of them
  walk the directory), so it is chosen for the one thing it does decide:
  who has to merge with whom. Two authors implementing two RPCs of the
  same service now have nothing to split. This matches the scaffold
  TESTS, which have been one file per RPC
  (`handlers_scaffold_<rpc>_test.go`) since the same reasoning was
  applied to them. When CRUD gen excises an RPC's stub because the RPC
  became entity-backed, the emptied `rpc_<name>.go` is removed with it
  rather than left as a bare `package x` husk. **Existing projects need
  no migration:** forge only ever writes a stub for an RPC it finds
  unimplemented, so a `handlers.go` already holding your handlers is
  never read, rewritten, or re-stubbed — it keeps working exactly as it
  is, and only NEW RPCs land in per-RPC files. The `forgeconv-handler-
file-size` lint now names `rpc_<name>.go` in its remediation instead of
  a third spelling.
- **Scaffolding is one verb: `forge scaffold`.** Arity picks the
  granularity — bare `forge scaffold` births every `// forge:entity`-marked
  message the protos imply and then projects them; `forge scaffold <noun>
…` scaffolds exactly one thing (`entity`, `service`, `worker`,
  `operator`, `crd`, `binary`, `frontend`, `scenario`, `webhook`,
  `package`, `adapter`, `library`, `handler-file`, `rpc`). This replaces
  `forge project add <noun>` and `forge project scaffold`, which were the
  same operation spelled as unrelated commands. There is no alias and no
  deprecation shim: the old spellings exit non-zero with `unknown
command`. `forge project` keeps the commands that act on the project as
  a whole — `new`, `delete`, `disown`, `migrate`, `upgrade`, `map`,
  `graph`, `introspect`, `features`, `annotations`, `audit`.
- `pkg/app/app_gen.go`: `RESTHandler` is now a method (`func (a *App)
RESTHandler() http.Handler`) backed by an unexported `restHandler`
  field. Required by the `serverkit.Application` interface contract;
  the renamed accessor preserves the public read path
  (`app.RESTHandler()` instead of `app.RESTHandler`).
- `pkg/app/bootstrap.go`: `WorkerList()` returns `[]serverkit.Worker`
  (was `[]*WorkerInstance`); `OperatorList()` returns
  `[]serverkit.Operator` (was `[]*OperatorInstance`). The wrapper types
  unchanged — they satisfy the interfaces directly.
- `pkg/app/bootstrap.go`: `RunOperators` signature gains
  `healthProbeAddr string` so projects that bind a controller-runtime
  probe listener can forward `serverkit.Config.OperatorHealthProbeAddr`.

### Migration

- See the new `v0.x-to-serverkit` migration skill for the upgrade path,
  including the manual edits required for projects whose
  `pkg/app/bootstrap.go` is forge-forked (e.g. cp-forge).

### Deprecated

### Removed

### Fixed

- `forge env up --target <name>` did cluster work for targets that have
  no cluster deployment edge. Target selection filtered the manifest
  stream, which is too late: cluster creation and cross-cluster
  kubeconfig minting run before render/apply, so targeting a host
  service still created a k3d cluster and the deploy pipeline reached
  its empty-manifest fallback. Phase requirements are now derived from
  the rendered placement graph — host, build-only and dev-served
  frontend targets skip both phases; compose and external targets deploy
  without a cluster; cluster services, operators, platform charts and
  cluster frontends require both. Infra pre-warm remains independent, so
  host processes can still depend on compose services when nothing
  selected runs in Kubernetes. The build-plan summary now reflects the
  filtered set rather than announcing work that will not happen.
- `forge scaffold frontend` baked `DEV_API_URL = "http://localhost:0"` into
  the new frontend's `src/lib/apiurl_gen.ts` whenever the project already
  had a server component — it read the always-zero per-component port and
  overwrote its own 8080 default with it. Non-mock dev then pointed
  `connect.ts` at port 0 until the next `forge generate` happened to
  rewrite the file.
- `forge project audit`'s ingress category could never emit its
  "declared but no route" finding: it gated on a port that was always 0,
  so it reported `0 service(s) without route` for every project. It now
  reports every SERVER with no route (workers/crons/operators/binaries get
  no k8s Service, so nothing can route to them).

### Security

[Unreleased]: https://github.com/reliant-labs/forge/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/reliant-labs/forge/compare/v0.0.8...v0.1.0
