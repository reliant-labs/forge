# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- **A contract mock imports every package its foreign interfaces use.**
  `forge generate` rewrote control-plane's `internal/svcdaemon/mock_gen.go`
  without `github.com/nats-io/nats.go`, so the package stopped building on every
  run. The mock generator named an unaliased import by guessing its path's last
  element (`nats.go`), so the `*nats.Msg` a foreign Deps interface returned never
  matched it. It also read foreign interfaces from syntax alone, which skipped
  embedded interfaces, and matched an alias by substring (`v1` inside
  `controlplanev1.Plan`), which left an unused import. A new
  `contract.Resolver` asks the toolchain instead: unaliased imports get their
  declared package name, and each foreign interface is rendered from `go/types`,
  embedded methods included, with every package its signatures reference
  imported under its real path. Declared methods keep their source order, so
  existing mocks are not reshuffled. One resolver serves the whole run (two
  `go/packages` loads, not two per contract). A package the toolchain cannot
  load falls back to the syntax path, so mock generation never blocks codegen.

- **`FORGE_LEDGER=machine` keeps a run off a declared control plane.**
  control-plane's hermetic `scripts/test-kata-prepull.sh` set
  `FORGE_LEDGER_HOME` to a temp dir and ran `forge ledger import --apply`.
  prod declares `forge.ControlPlane`, so the import planned against, and tried
  to write to, the production ledger. Only a plan conflict stopped it.
  `FORGE_LEDGER_HOME` is a location, not a selector, and it stays one: users
  set it to move the ledger off a network filesystem, and making it select
  would send their hosted envs to an empty machine ledger. The new
  `FORGE_LEDGER=machine` sends every env to this machine's ledger. No
  endpoint, credential or client for the declared control plane is resolved.
  Every forge process that inherits the variable is covered, so a script's
  render, import and status calls are all covered. An unrecognised value is
  refused, never read as the default. The override prints one stderr line per
  env, and `forge ledger where <env> --json` reports
  `"override": "FORGE_LEDGER=machine"` so a script can assert it.
  `forge ledger import` now labels each target HOSTED or machine. Before a
  hosted write it prints `Writing to the HOSTED ledger at <url> …`.

- **A failed `forge generate` restores external-tool output too, and claims a
  clean tree only after checking it.** A refused generate in control-plane
  printed "your tree is back to its pre-run state" over 32 files buf had left
  modified: the rollback journal recorded only forge's own writers, on the
  theory that tool output is deterministic from the inputs. It is not. A local
  plugin rendered a different version header, and buf regenerated stubs from
  another agent's uncommitted proto. `GenStep.Writes` now declares what each
  external tool may write (buf's `out:` dirs, the descriptor, `openapi/`,
  frontend TS stubs, sqlc's outputs, `gen/go.mod`, every `go mod tidy`, the KCL
  image-registry migration). Those paths join the journal before the tool runs.
  Directories are captured recursively, and a file or directory the tool
  creates is removed on restore. Before the first step the pipeline also
  fingerprints the work tree (`git ls-files`, or a walk outside git; size,
  mtime, mode and sha256). After the revert the report makes one of three
  claims: back to its pre-run state (checked), NOT fully back (followed by
  every file that still differs), or could not be checked (run
  `git status`). Unjournaled changes are reported, never reverted: in a shared
  checkout they may be another agent's. The check found forge writers that
  wrote raw and are journaled now: the scaffold-once `contract_test.go` (a
  refused run left it behind and kept its birth record, so it was never
  scaffolded again), the `observe_chain.go` seam, and the frontend import
  repoint, tsconfig/vite edits, nav and hooks writes.

- **`forge env render` no longer asks the control plane for an organization
  that nothing uses.** The off-base check for hosted images composed the push
  base first, which costs an organization lookup with the stored credential,
  and only then looked for hosted images. An env that declares a control plane but
  hosts nothing (control-plane's prod) called its control plane on every
  render. The base is now resolved only when there is a hosted image to judge.

- **A no-version `forge env deploy` reuses the release already cut for the
  checkout, and builds nothing.** On 2026-10-09
  `forge env build prod --release 20261009.205925-d29da50b` cut prod's release
  from a clean checkout. A
  `forge env deploy prod` in the same unchanged checkout then rebuilt every
  image (the reliant image came out with a different digest) and named a new
  release. The control plane had stored the cut's tree hash, but the hosted
  ledger dropped `provenance` when it read releases back, so no release ever
  matched. Reads now decode it. A release is reused when it records this
  checkout's clean tree and this forge version, covers every artifact the env
  declares (each source-pinned frontend at today's resolved commit included),
  and every image it pins still resolves in its registry. Its name does not
  matter. A reused release is deployed as `forge env deploy <env> <version>`
  would deploy it, with no build. The deploy first prints
  `reusing release <v> (cut at …)` or `cutting new release <v> because …`. A
  dirty checkout, another forge version, a `-D` option, a missing artifact, or
  an expired image each cut a new release. The cut and the deploy capture
  provenance through one function.

- **Storage maintenance unlinks are capped, paced, and back off on filesystem
  distress.** On 2026-10-09 one `forge storage gc --apply` unlinked 175,324 Go
  build cache entries in ~30s on a virtiofs-backed workspace volume while
  builds ran; the host's virtiofsd ran out of descriptors, the guest saw
  EMFILE, and the volume was detached seconds later. The trim skipped each
  failed unlink and tried the next. Every unlink a pass makes (Go build cache,
  golangci-lint, goimports, orphaned private caches, temp sweep, source cache,
  rotated logs) now goes through one governor: at most `max_deletes_per_pass`
  (default 50,000) per pass, paced to `delete_rate_per_sec` (default 1,000),
  and the first EMFILE/ENFILE/ENOTCONN/EIO stops the pass, retaining the rest.
  A backed-off pass prints `BACKED OFF`, records `backed_off_layers`, and exits
  0 so nothing retries it at once. A source clone is renamed out of its cache
  key before deletion, so a stopped eviction never leaves a half-deleted pin.

- **No automatic storage pass removes a worktree.** The background auto-gc
  (`forge env up`, the hourly job) ran the worktree layer, so a policy with
  `worktree_reap` let it remove any worktree that looked idle, clean and
  pushed, which is what a worktree an agent is between commands in looks like.
  Only an explicit `forge storage gc --apply` removes worktrees now; the daily
  job runs `gc --apply --scheduled` and previews. Explicit removal also refuses
  the main checkout, anything containing it or the home directory, a path whose
  `.git` is not a gitdir file of the repository, worktrees under Reliant's
  `~/.reliant/worktrees` (unless `worktree_reap_reliant_managed`), and any tree
  with an entry modified in the last 24h; it prints each canonical target
  before removing it, and `git worktree prune` only expires registrations idle
  for the same 24h.

- **`internal/pkgguard` catches every reference to a forbidden environment
  read, not only calls.** It inspected call expressions alone, so
  `getenv: os.Getenv` — a function value stored for a later call — passed it
  while forbidigo failed `Lint (forge/pkg)` on the same line. It now resolves
  each file's imports and reports any `os.Getenv` / `os.LookupEnv` /
  `os.Environ` reference, including through an aliased or dot import.

- **A release refuses a sibling checkout that is not at the commit the
  project pins.** A `ShellBuild` with `cwd = "../reliant"` built whatever that
  checkout had on disk, so a release could record a days-old sibling image
  while `go.mod` pinned a newer commit (prod release 20261007.165315). Every
  `ShellBuild` now records the checkout it ran in — HEAD, dirty flag, origin,
  module — in its build state, and a release carries it per artifact as
  `built_from`. `forge env build <env> --release` (also `--plan`, `--no-build`
  and a deploy that re-uses a release) refuses when an external checkout is
  dirty or not at the commit its `go.mod` require or `forge.GitSource` ref
  pins, before anything is built, and prints the fix:
  `git -C <dir> checkout --detach <commit>`.

- **Request factories seed parents in non-`public` schemas, and say why when they
  cannot.** `forge generate` warned `relation "users" does not exist` for every
  create-request factory in a project whose tables live in a named schema
  (`controlplane.users`): the baked parent `INSERT`s named only the bare table,
  so the shadow connection never found it, and the factory became a `t.Fatalf`.
  `schemadef.Table` now carries its `Schema`, and the seed planner renders
  `"controlplane"."users"` for any table outside `public`. A one-member
  vocabulary (`CHECK (provider = 'byo')`, which postgres stores as a plain
  equality rather than an `= ANY` list) is also read as a pool, so such a column
  no longer gets a value its CHECK rejects. When a parent row still cannot be
  built, the warning now names the parent table's multi-column constraints forge
  could not place and the two remedies that work (restate the constraint, or
  `forge project disown` the factory) instead of echoing a bare SQLSTATE.

- **A narrow stored credential elevates on demand instead of dead-ending.**
  `forge cluster connect` and `forge domain ls` answered a signed-in org owner
  with 403 `does not carry the cluster:manage / domain:read scope`, and the
  hint ("re-run `forge login`") minted the same narrow token again. When a
  call made with a `forge login` credential or a credential-helper token is
  refused for a missing scope, forge now calls the control plane's
  `AccessTokenService/ExchangeToken` for exactly that scope and retries once.
  The control plane re-reads the user's role at that moment, so the stored
  credential never has to carry `cluster:manage`. The elevated token lives in
  memory for the process and is never written to the credentials file; `--token`
  and the token env var are never elevated. A refusal is shown with the
  original error plus why elevation did not help (an older control plane, or a
  role without the permission).

- **Dependabot no longer bumps code generators it cannot regenerate for.**
  protoc-gen-es writes its version into every `*_pb.ts`, and protoc-gen-go
  (installed by `forge tools install` at go.mod's `google.golang.org/protobuf`)
  into every `*.pb.go`. Dependabot edits the lockfile or go.mod but never runs
  `forge generate`, so each such bump failed Verify Generated Code by
  construction; control-plane #645 was one, merged red. The scaffolded
  `.github/dependabot.yml` now ignores `@bufbuild/protoc-gen-es` and its exact
  peer `@bufbuild/protobuf` in the npm entry, and `google.golang.org/protobuf`
  in the gomod entry when the project runs protoc-gen-go. The file is yours
  once scaffolded, so existing projects get it from the v0.1.44 migration
  (`forge project upgrade list`). A migration detection script that ended in a
  quoted argument (`"$f"`) also had its closing quote stripped by the
  frontmatter parser and silently matched nothing; only a matched pair of
  quotes is stripped now.

- **`forge generate` no longer rewrites go.sum under a caller's
  `GOFLAGS=-mod=mod`.** Its `go` subprocesses inherited the caller's GOFLAGS,
  and the goimports pass over generated code runs `go list -m -e -json ...`,
  which under `-mod=mod` records a checksum for every module in the graph: 356
  go.sum lines on control-plane that a CI regenerate never produces. Every `go`
  subprocess forge runs on its own behalf — and goimports — now gets its
  environment from `goexec.Env`, which drops `-mod=mod` from the inherited
  GOFLAGS and keeps everything else (`-tags`, `-trimpath`, `-mod=vendor`, …).
  Env that forge or a project's declared build config sets explicitly is still
  honoured. A test pins that no new call site inherits the environment
  unscrubbed.

- **An unknown key in the per-machine `storage.json` no longer fails unrelated
  commands.** `storage.Load` decoded with `DisallowUnknownFields`, so a file
  written by another forge version (e.g. `go_cache_unused`, a live key added in
  #537 and unknown to older binaries) made `forge build` and friends abort with
  `storage policy: json: unknown field`. Unknown keys are now ignored with a
  one-line warning naming them; strictness stays with forge.yaml.

- **A fresh scaffold signs in.** The README's first sixty seconds ended at a
  sign-in page nobody could pass, for two reasons.

  - `auth idp-provision` printed the login broker's token to the terminal
    ("shown ONCE") instead of storing it, minted a new one on every
    `forge env up`, and the dev `_api` named only `DATABASE_URL` in
    `config_secrets` — so even a token stored by hand never reached the server,
    and `/auth/login` was a 404. The job now keeps the token in the env's secret
    store (`secrets/dev.yaml`, as `IDP_BROKER_TOKEN`; the dev env hands it the
    path as `IDP_BROKER_TOKEN_STORE`), reuses it while the issuer still accepts
    it (`devidp.ProvisionLoginBroker`, `devidp.SecretFile`), and never prints
    it. The dev `_api` names it, and `forge env up` re-reads the store after its
    jobs, so the first run's API already has it.
  - After sign-in every RPC answered 401 "missing Authorization header". The
    session is an HttpOnly cookie, but the Next.js transport did not send
    credentials cross-origin, and the scaffolded auth interceptor read only the
    `Authorization` header. The transport is built on `fetchWithSession`, as the
    Vite one already was, and `pkg/middleware/middleware.go`'s `credentialFrom`
    reads a Bearer header, else the session cookie. `SessionCookieName` is
    declared there and `internal/app/login_broker.go` sets the cookie under it.

  Existing projects own every file involved, so apply it by hand: add
  `"IDP_BROKER_TOKEN"` to `_api`'s `config_secrets` and
  `IDP_BROKER_TOKEN_STORE = "secrets/dev.yaml"` to the idp-provision env in
  `deploy/kcl/dev/main.k`; re-scaffold `cmd/<bin>/cmd/auth.go` and
  `pkg/middleware/middleware.go` (delete, then `forge project rescaffold <path>`)
  or port the changes; and give each real transport in
  `frontends/<name>/src/lib/connect.ts` a fetch that sets
  `credentials: "include"`.

- **The observability opt-in delivers data.** Following the comment in
  `deploy/kcl/dev/main.k` ran Grafana with nothing reaching it: the host-run API
  had no OTLP endpoint and compose published no OTLP port. The forge dashboards
  never loaded either (`grafana/otel-lgtm` reads providers from
  `/otel-lgtm/grafana/conf/provisioning`, not `/etc/grafana/provisioning`), and
  their panels queried `rpc_server_duration_milliseconds`, which otelconnect
  v0.10 no longer emits. Now one switch, `_observability = True`, runs `lgtm`,
  publishes OTLP on a loopback port the env declares, and points every host
  process at it. The overview dashboard queries
  `rpc_server_call_duration_seconds` (by `rpc_method` and
  `rpc_response_status_code`) and the database pool, and its test derives the
  series names from otelconnect itself. `forge env up` and `forge env status`
  list what compose services publish, Grafana included.
- **`forge env status` no longer passes on what it cannot see.** Prometheus
  reported "✓ 1 targets up" when the only target was the lgtm image scraping
  itself; it now passes only on series carrying the app's `job`. pprof read
  whichever process held the machine-wide 127.0.0.1:6060 and reported its
  profiles as the app's; each dev host process now gets its own `PPROF_ADDR`,
  and the check warns when `/debug/pprof/cmdline` names another process.
- **`forge env up` output.** initdb's and pg_ctl's transcript (absolute paths,
  a `pg_ctl … start` hint) goes to `.forge/hostinfra/postgres/postgres.log`.
  Host jobs write the `.forge/logs/<env>/<job>.log` the summary lists. A compose
  service is converged once per run, not three times. Entity birth no longer
  announces a "(+down)" migration it does not write, and prints the migration
  project-relative. The compose check's hint is `forge env up <env>`, not
  `docker compose up -d`.
- **One unusable currency no longer takes a generated page down.** The list and
  detail pages format money in each row's own `currency`, and
  `Intl.NumberFormat` throws a `RangeError` for anything that is not an ISO 4217
  code — an empty string, a typo, or the dev seed's `sample_currency_<n>` — so
  the whole Orders page fell to "Something went wrong" on the first
  `forge env up`. `formatMoneyCents`, `formatMoneyWhole` and
  `formatMoneyInterval` now render such a value as the plain amount beside the
  raw code. Existing projects: copy `formatMinorUnits` and the three helpers
  from a fresh scaffold's `src/lib/format-utils.ts`.
- **Migration-safety findings point at the statement.** Each statement's line
  was counted from the previous semicolon, so the first statement under the
  header `forge db migration new` writes was reported at `:1`, and every later
  one at the line before it. The verdict also put the finding count beside the
  number of files scanned ("2 findings across 2 migration files" for two
  findings in one file); it now says "2 findings in 1 migration file (2
  migration files checked)".

### Changed

- **Every successful call is logged again by default; sampling is a
  per-environment setting in typed config.** `observe.LoggingInterceptor`
  (`rpc completed`) and `observe.LogMiddleware` (component calls) sampled
  successes out of the box — one per procedure per minute. They now write
  every success unless their caller passes `observe.WithSuccessSampling(d)`.
  Failures and successes over 1s are always logged. The scaffolded
  `proto/config/v1/config.proto` declares `log_success_sample_window`
  (default `0s`; env `LOG_SUCCESS_SAMPLE_WINDOW`, flag
  `--log-success-sample-window`), so a deployment sets it per env in
  `deploy/kcl/<env>/config.k`, and the scaffolded `serve.go` passes the
  loaded value to `observe.Chain` as `LogOptions`. `forge/pkg/observe` reads
  no environment: an existing project adds the field to its AppConfig and
  `LogOptions: []observe.LogOption{observe.WithSuccessSampling(cfg.LogSuccessSampleWindow.AsDuration())}`
  to the `observe.Deps` in its owned `serve.go`.
  `observe.DefaultSuccessSampleWindow` is removed (there is no default
  window).
- **One prettier release formats a scaffolded project, everywhere.** Frontends
  now pin `prettier` exactly (`3.5.3`, was the range `^3.5.0`), and the
  scaffolded `.pre-commit-config.yaml` runs that same release as a local hook
  instead of `pre-commit/mirrors-prettier` (archived at `v3.1.0`). The two
  disagreed about the component library forge installs, so a fresh project's
  hook rewrote files its own `npm run format` had just written, and the range
  drifted to whatever `npm install` resolved. Existing projects: replace the
  mirrors-prettier repo in `.pre-commit-config.yaml` with a `repo: local`
  hook (`language: node`, `entry: prettier --write --ignore-unknown`,
  `additional_dependencies: ["prettier@3.5.3"]`), and pin
  `"prettier": "3.5.3"` in each frontend's `package.json`.
- **Skills and project memory now lead with shipping.** The start-here `forge`
  skill gains a "Ship it" section (hosting is the default, `forge env deploy`,
  the free static tier, queued-on-billing exit 7, no `forge login` under
  Reliant); `deploy/hosting` holds the gates table and the provides/refuses
  table; `frontend/serving`, `deploy/static-site`, `db/deploy-migrations` and
  `secrets` state what hosting accepts and gates on. The project memory template
  gets a one-line Shipping note.

### Removed

- **The host-application credential DEPOSIT is gone (`pkg/cloudcred.Save`,
  `Delete`, `HostClientID`, `Location`, `Credential`).** A host used to copy its
  own session token into `credentials.json` under client `host-app`, and forge
  presented it to the control plane's deploy API. That token was a permanent,
  multi-purpose session credential (Reliant's daemon or CLI login) which mostly
  did NOT hold deploy authority, so deploys 403'd and users ran `forge login`
  anyway — and where it did work, the deploy API, the registry login and every
  subprocess saw a credential that could also connect as the user's daemon.
  forge no longer reads `host-app` entries at all; `cloudcred.RemoveLegacyHostDeposits`
  lets a host purge what it wrote. Replaced by the credential helper below.
- **forge no longer requires cgo.** `kcl_plugin.forge` is bridged through a
  purego callback, so `go install` works with any `CGO_ENABLED` setting,
  including Windows without a C toolchain. `env diff`'s `unsupported` status is
  gone, as are the `CGO_ENABLED=1` prefixes and "CGO is required" notes in the
  Taskfile, Makefile, scaffolded CI install step, version-skew install hints
  and skills.

### Changed

- **The dev loop runs the API as ONE process — `_api`, the binary's
  `server`, under air (hot reload) — so the frontend reaches every service.**
  Dev ran each service as its own `go run ./cmd/<p> <service>` host process on
  its own port, and the frontend's dev config (`api_url`, the
  `<project>-dev-api` port key) named the first: a 3-service project's
  frontend could call one service in dev — the defect #527 fixed for hosted
  envs, and it strands the sign-in cookie the same way. `deploy/kcl/dev/main.k`
  now declares the same `_api` the hosted envs do, `_port_of` gives it the
  `-api` key, and it binds `_on_host(_api)` instead of the services and
  workers (`server` mounts every service and supervises every worker). This
  is what the docs already said — "one binary serves every service on one
  mux", "Go services (hot reload)" — and what `.air.toml` (`entrypoint =
["./tmp/<p>", "server"]`) and `task dev` already ran; `forge env up dev`
  did neither. `_on_host` now runs `server` under air (anything else still
  `go run`), so a .go edit rebuilds and restarts the API; air must be
  installed (`go install github.com/air-verse/air@latest`, which `forge
doctor` already required), and `forge env up` refuses to start without it,
  naming the workload and the `runner = "go-run"` way out, instead of failing
  in the host phase on `exec: "air": executable file not found`. It no longer
  prints "the workload's args [server] are not passed" when the air config's
  entrypoint passes exactly those args. `forge scaffold service|worker` adds
  no dev line (`_on_host(_api)` is bound with the first service);
  `forge env up dev --target api` restarts the API. **Existing projects:**
  copy `_api` and the `_on_host` binder from a fresh `forge project new`'s
  `deploy/kcl/dev/main.k`, change `_port_of`'s `-api` owner to `"api"`, and
  replace the service/worker lines with `_on_host(_api)`.
- **New projects deploy to Reliant hosting by default.** `forge project new`
  scaffolds `staging` and `prod` hosted on the forge control plane: the API
  (every service and worker, as one workload — below) and the migrate job
  bound `_hosted` (its image's registry host dropped,
  since the platform pulls only from its own registry), a
  `forge.ManagedDatabase` each workload reads through `forge.DatabaseRef`,
  `forge.HostedSecrets`, the frontend on platform static hosting with
  `API_URL` and `CORS_ORIGINS` wired by reference, and `control_plane =
forge.ControlPlane {}`. A fresh scaffold renders every env with no
  placeholder. Before, staging and prod bound everything to a cluster the
  author had to name (`forge env deploy prod --explain`: `REFUSE (declared
context not in kubeconfig)`) and the frontend to `REPLACE_ME_BUCKET`. The
  scaffolded CI is the hosted pipeline (`release.yml`, no registry login)
  instead of `deploy.yml`. `dev` is unchanged. Hosted workloads and the
  managed database need billing; a static site alone is free.
  - **The hosted API is ONE workload, `_api`: the binary's `server`.** A
    browser reaches the API at one origin — the frontend's Connect transport
    has one base URL (`/<package>.<Service>/<Method>`, so one origin serves
    every service), and sign-in answers with an HttpOnly session cookie the
    browser returns to that origin only. The platform gives each workload its
    own hostname and routes no paths between them, so hosting each service as
    its own workload (as first shipped) left a 3-service project's frontend
    able to call one service: `API_URL` named the first. Per-service URLs in
    the browser cannot fix that — the session cookie would still be stranded
    on one host — so each hosted env declares `_api` (`args = ["server"]`:
    every service on one Connect mux, every worker supervised beside it) and
    binds it instead of its services and workers; the frontend's `API_URL`
    names it. One workload is also one bill, not one per service. It is bound
    once there is something for it to run (a project born with no service
    pays for no idle workload; `forge scaffold service` binds it with the
    first). `forge scaffold service|worker` then adds no hosted line — `server`
    already runs it (dev too, below); a job,
    an operator (which `server` skips without a Kubernetes API) and a tool
    bind as before. `forge env new X --from prod --bind api=cluster` rebinds
    the API; `--bind <service>=…` is refused, naming `_api`. Splitting a
    service or worker out is its own `_hosted(wl.<name>)` line plus taking it
    out of `server` (cmd/<bin>/cmd/server.go). The binary mode
    (`--binary per-service|shared`) never reached the deploy files and does
    not now: both modes ship one binary with a subcommand per component and
    the all-in-one `server`.
  - **Hosting elsewhere stays a one-line rebind.** Each env declares
    `_on_cluster` and `_on_bucket` beside the hosted binders, unused, with
    the `forge.ClusterTarget` / `forge.OnBucket` to fill shown in a comment
    (`_cluster = None`, `_bucket = None`). A binding to either before it is
    declared fails the render, naming the workload and the fix.
  - **Kinds hosting refuses.** `forge scaffold operator` binds the operator
    `_on_cluster` in every deployed env — the one binding that can run it —
    and warns that those envs refuse to render until `_cluster` is declared
    (or the line is dropped); a hand-declared `kind = "cron"` binds the same
    way. forge's cron component (`forge scaffold worker --kind cron`) is a
    worker with its own scheduler and is hosted like any worker. Every other
    new workload binds where the env's `migrate` job runs, so an env
    scaffolded on a cluster keeps binding there.
    `forge scaffold frontend` binds a new frontend through a hosted env's
    `_hosted_frontend`, and still to a bucket in an env without one.
  - **Existing projects are not rewritten** (env files are scaffolded once).
    To adopt hosting, add `control_plane`, `secret_provider =
forge.HostedSecrets {}` and a hosted `forge.ManagedDatabase` to the env's
    Bundle, give `_hosted` the bare image and `DATABASE_URL =
forge.DatabaseRef {...}`, and rebind each line `_hosted(...)` — or try it
    beside prod with `forge env new cloud --from prod --bind <name>=hosted`
    first. The `deploy` skill has the exact edits. A project scaffolded
    between the hosting default and the one-workload API binds each service
    `_hosted(wl.<name>)` and its frontend calls only the first: replace those
    service and worker lines with `_api` (copy the declaration and the
    `_api_config` / `_hosted_frontend` lines from a fresh `forge project new`'s
    `deploy/kcl/prod/main.k`; the `deploy/hosting` skill shows them).
- **Next.js frontends are scaffolded as static exports, and the generated CRUD
  pages are static routes.** Before, the default was `output: standalone`, a
  Node server that hosted static hosting (`forge.OnHosted {}`) cannot run. The
  generated detail and edit pages were dynamic `src/app/<slug>/[id]/` routes,
  so switching to `output: static` failed `npm run build` on the first entity
  with `Page "/<slug>/[id]" is missing "generateStaticParams()"`. Now:
  - `forge project new --frontend` and `forge scaffold frontend` write
    `output: static` into `forge.yaml`. `npm run build` exports into `out/`,
    and the Dockerfile serves `out/` from nginx. `--output standalone` is the
    explicit opt-in for a Node server. `next dev` is unchanged. A `forge.yaml`
    entry with no `output:` still means standalone, because every frontend
    scaffolded before this change has a standalone `next.config.ts`.
  - Detail and edit are `src/app/<slug>/view/page.tsx` and
    `src/app/<slug>/edit/page.tsx`, served at `/<slug>/view?id=…` and
    `/<slug>/edit?id=…`. Each reads the id with `useSearchParams` under a
    Suspense boundary, and renders a "which row?" state when `?id=` is
    missing. List row clicks, create-then-redirect, the Edit action, edit's
    Cancel, breadcrumb and save redirect all build the new URLs through
    `src/lib/entity-routes.ts` (`entityViewHref`, `entityEditHref`,
    `useEntityIdParam`). That file is a new scaffold-once helper with its
    own vitest suite. `forge generate` backfills it into an older frontend
    the first time it writes a page that imports it.
  - The id is in the query string, not the path, because an export can only
    hold pages it enumerates at build time. `trailingSlash` stays off, so
    the export writes `out/<slug>/view.html`. Hosted static hosting and the
    scaffolded nginx image both resolve `/<slug>/view` to that file by
    trying `.html`. No CDN rewrite is involved.
  - The static `next.config.ts` sets `images: { unoptimized: true }`, since
    next/image's optimizer is a server route. The dev-only browser-log
    route (`app/%5F_forge/log`) answers POST only, so the export leaves it
    out.
  - The sign-in guard keeps the query string in `returnTo`, so a visitor
    bounced off `/<slug>/view?id=…` comes back to the same row.
  - **Existing projects are not rewritten.** Their pages are scaffold-once.
    While `src/app/<slug>/[id]/` exists, `forge generate` keeps it as that
    entity's detail/edit route and writes no view/edit pair beside it. If
    the frontend is `output: static`, generate warns that `next build` will
    refuse those routes. `forge project upgrade list` offers the new
    `migrations/v0.1.44` playbook to any project with a `[id]` route. The
    playbook covers rescaffolding untouched pages, moving edited ones, and
    switching `output:` with `forge project upgrade --force next.config.ts
Dockerfile`.
  - `forge lint --guarded-fields` reads the new `<slug>/edit/page.tsx` as
    well as the old `[id]/edit` path. The stale-route report recognizes both
    page shapes.
- **The render refuses a Next.js server build bound to a static runtime.**
  `forge.OnHosted` for a frontend, `forge.OnBucket` and `forge.OnFirebase`
  serve files and run nothing. A frontend whose forge.yaml `output` is
  `standalone` or `server` used to render, deploy and build green, then fail
  at publish on an `out/` nothing wrote. The roofers scaffold bound one this
  way. `forge env render` / `deploy` / `up` and `forge build` now refuse it
  at render. The refusal names the frontend, the env and both fixes: set
  `output: static` and migrate (`forge lint --static-export` lists what), or
  bind it elsewhere, since a Next.js server ships as a workload with a
  `forge.DockerBuild`. forge.yaml stays the one declaration of the build
  shape. forge binds it into every render it drives as the reserved
  `-D frontend_outputs` option, and the KCL never restates it. An env with no
  forge.yaml frontend of that name, or a plain `kcl run`, is not judged.
  **Adoption:** a project whose forge.yaml frontend has no `output:` reads as
  `standalone` (what its scaffold-once next.config says). If such a frontend
  is bound to a static runtime, set `output: static` and follow
  `forge skill load migrations/v0.1.44`.
- **Concurrent `forge generate` / `forge scaffold` runs in one project now
  queue instead of failing.** Every run takes the project's lock
  (`.forge/forge.lock`). A second run prints one line,
  `⏳ forge: waiting for pid N (<command>) …`, and starts when the first
  finishes. Previously the lock was an O_EXCL marker file: every concurrent
  run failed at once with "another forge process is running … remove it with:
  rm .forge/forge.lock". Agents that followed that advice ran two pipelines
  in one tree, and their revert-on-failure rollbacks restored each other's
  writes. The lock is now an OS file lock (flock; LockFileEx on Windows) that
  dies with its process, so a killed run cannot leave a stale lock and there
  is no 10-minute reclaim window. `forge scaffold` and all its nouns hold it
  for the whole command, not only for the generate pass at the end, so another
  run never sees a half-scaffolded tree: protos with the CRUD quintet injected
  and a migration written, but no ORM or handlers. Build's auto-generate,
  `generate --watch`, `--check`, `project rescaffold` and `project new` take
  the same lock. The file stays on disk between runs and is already
  gitignored (`.forge/*`); do not delete it.
- **`orm.Context` gains `RunTx`, `RunTxReadOnly` and `RunTxWithOptions`.**
  They are on the interface so `s.deps.DB.RunTx(…)` compiles against the
  injected `DB orm.Context`. A hand-written `orm.Context` implementation, such
  as a test fake, must add the three methods. `orm.Client.Bun()` now returns a
  thin wrapper around the `*bun.DB`, so `ExecContext`/`QueryContext`/
  `QueryRowContext` can join a ctx transaction. Use `BunDB()` when you need the
  concrete type. `RunTransaction` keeps its semantics: database-default
  isolation, one attempt, handle-passing. It now joins a transaction already
  carried in ctx, and `Tx.Commit` runs after-commit callbacks registered
  through `tx.RunTx`.
- **Scaffolded CRUD lifecycle tests carry no fixtures; their rows come from
  regenerated factories.** `handlers_crud_test.go` (scaffold-once) used to
  embed a literal `INSERT` seed block and literal create-request values,
  frozen against the birth schema. Editing the birth migration afterwards —
  the db skill's own advice: a `GENERATED` column, a one-way status `CHECK` —
  broke it (`cannot insert a non-DEFAULT value into column …`,
  `violates check constraint …`). It now calls
  `<svc>.NewCreate<Entity>Request(t, db, 0|1)`,
  rendered into the forge-owned `factories_gen_test.go` from the applied schema
  on every `forge generate`: it seeds the FK parents and returns a request the
  current constraints accept (enums the column DEFAULT supplies, nullable
  timestamps and GENERATED columns are left unset). A factory forge cannot
  derive a valid request or row for is still emitted, with a body that fails
  the calling test naming postgres's verdict, and `forge generate` prints it
  as a warning — it no longer fails the generate.
- **`New<Entity>` factories insert minimal, lifecycle-consistent rows.** Every
  NOT NULL column without a DEFAULT and every FK parent is filled; every other
  column is left to its DEFAULT or NULL; GENERATED columns are skipped; CHECKs
  are satisfied, including length/range/vocabulary checks, a DEFAULT the
  column's own CHECK rejects, and one-way status implications (the
  union/guard branch closest to the DEFAULTs). A jobs factory that used to
  insert a LEAD job with a crew, a schedule, `completed_at` and `lost_reason`
  now inserts a bare LEAD. Each factory is executed against the shadow schema
  at generate time; if the minimal row is rejected the full row is tried.
- **`forge lint --fixture-drift` executes literal fixtures instead of
  pattern-matching them.** Every fixture statement in a scaffolded
  `handlers_crud_test.go` runs against the shadow schema in a rolled-back
  transaction (grouped per test function, one savepoint per statement), and
  postgres's own error is reported per statement — GENERATED columns, UNIQUE
  collisions, dangling foreign keys and the CHECK violations the old matchers
  missed. A project with no literal fixtures opens no database; an unreachable
  shadow is reported as `forge-fixture-unverified`, never as clean. The rule id
  is now `forge-fixture-rejected` (was `forge-fixture-generated-column` /
  `forge-fixture-duplicate-unique`). `--crud-fixtures` (the dangling-FK
  matcher, rule `forge-crud-fixtures`) is folded in and kept as a hidden,
  deprecated alias.
  **Existing projects:** run `forge generate` (factories gain the
  `New<CreateRequest>` functions), then `forge lint --fixture-drift`. Your
  `handlers_crud_test.go` is yours and is not rewritten; to adopt the new shape,
  delete its seed block and replace each literal `&pb.Create<Entity>Request{…}`
  with `<svc>.NewCreate<Entity>Request(t, db, 0)` / `(t, db, 1)`. Tests that
  relied on `New<Entity>` populating optional columns now get DEFAULT/NULL
  there — set them with an override.
- **`forge doctor`'s `forge: kcl-plugin` check is now a real probe.** It
  evaluates a one-line KCL program through `kcl_plugin.forge` instead of
  reading a build flag, so it fails for what can actually break now: KCL's
  native library failing to extract or load (antivirus, read-only temp dir,
  bad `KCL_LIB_HOME`). Same name and pass/fail/skip meaning as before.
- **A KCL runtime that cannot load is an error, not a crash.** kcl-lang.io/lib
  panics when it cannot extract libkcl; forge used to crash with a stack trace
  (`forge doctor` exited 2). It now reports `load the KCL runtime: …` from
  every render.
- **Scaffolded `.air.toml` sets no env inline.** `ENVIRONMENT=development` is
  set by `forge env up` on the air process (and by `task dev`), so the config
  uses `entrypoint` and works under air's PowerShell on Windows.
- **`task db:restore` shows pg_restore's warnings.** The `grep -v` filter that
  hid them is gone, because `grep` does not exist on Windows.
- **An unwritten `forge:computed` field fails `forge lint`, like an unwritten
  `forge:read-only` one.** It used to only warn while read-only failed —
  backwards, since computed is the stronger promise ("my app derives this").
  `computed-fields lint` now gates; `--computed-fields --json` reports
  `ok: false`.
- **Pending-stub mode for both unwritten-column rules.** Right after
  `forge scaffold`, the rpc that will write a read-only/computed column is
  still forge's own `// forge:gen unwired-stub` placeholder, and failing the
  gate on forge's fresh output taught agents to ignore the rule. While the
  declaring service's handler package still holds those stubs, the finding is
  a warning naming them — `pending: implement ChangeJobStatus, ScheduleJob` —
  and once none remain it is an error again. Deterministic: the service comes
  from the entity's proto directory and the handler package from the same
  resolver `forge generate` uses; forge does not guess which rpc writes which
  column.
- **A failing `forge lint` names the failed linters on its last line**
  (`forge lint: 2 gating linter(s) failed: computed-fields lint, read-only-fields lint`),
  instead of "one or more linters reported errors".
  The verdict is the last line in every mode.
- **`frontend lint` no longer "passes" on a project with no frontend.** With no
  declared frontend and no `frontends/` directory it is a silent no-op, as the
  frontend typecheck lane already was, instead of counting as a gating linter
  that ran.
- **Bridging a project to a local forge checkout is opt-in: `forge project new
--link-forge`.** It used to happen on its own for every dev build of forge —
  including a host binary that embeds forge through a workspace (`reliant
forge project new`), which bridged every project it created to whatever
  checkout that host compiled from. On a shared machine that is the main
  checkout everyone pulls into, so the library under those projects moved with
  each merge while the binary generating their code did not. Now nothing is
  written unless asked for; a service scaffold from an unreleased build is
  refused with the flag named, and `--link-forge` on a binary that cannot
  locate its checkout is refused before anything is written.
  `FORGE_LINK_FORGE=1` is the same opt-in for harnesses (forge's e2e corpus
  sets it). The web-runtime twin (`.forge-link/` + root `package.json`) now
  follows the project's go.work bridge and links that same checkout, instead
  of being laid down by any dev build for its own checkout. **Adopting:**
  existing bridged projects are untouched; a contributor scaffolding with a
  dev forge adds `--link-forge`.
- **The scaffolded `handlers_crud.go` says each thing once.** Every delegating
  method used to carry the same eight-to-ten-line comment (what pkg/crud does,
  where the wiring lives, and the full AUTHENTICATED / PUBLIC / SCOPED
  explanation). Now each method has a summary line and one `Auth:` line that
  still names `middleware.GetUser` (an author editing `DeleteOrder` reads
  `DeleteOrder`'s comment, not the header). The file header explains each tag
  once, and only the tags the file contains. Update methods keep a line on mask
  handling, which now mentions that `op.ReadOnly` columns are refused and kept.
  The file is yours, so existing projects keep their comments; methods appended
  later get the short form.

### Added

- **`forgeconv-list-filter-optional`: `forge lint --conventions` now rejects a
  List-request filter field without `optional`.** The rule ("List request
  filter fields must be optional") was documented in every project's
  CLAUDE.md and the proto skill and checked by nothing. A scalar or enum field
  on a `List<X>Request` (or the request of a `List<X>` rpc) with no presence
  is an error: unset and the zero value are the same on the wire, so the
  generated List op either can never filter on the zero value or — for a
  bool/enum — always applies it and returns only the `false`/UNSPECIFIED rows.
  Pagination/ordering controls and message-typed fields are exempt, enums
  resolve across the proto tree, and a required parameter can say so with
  `[(buf.validate.field).required = true]` instead.

- **Signed in to Reliant means signed in to the control plane — no `forge
login`.** forge has a credential-helper protocol (`pkg/cloudcred`, the model
  of kubectl exec plugins and git credential helpers): when
  `$FORGE_CREDENTIAL_HELPER` names a command, forge writes
  `{"version":1,"endpoint":…,"scopes":[…]}` to its stdin and reads a token (or a
  structured refusal: `no_session`, `denied`, `unavailable`) from its stdout.
  The value is a JSON argv array, or a single executable path. Reliant sets it
  for `reliant forge …` and for every shell its agents run, and answers with a
  short-lived token minted from the user's Reliant session for exactly the
  endpoint the env declares. Hosted commands (`env deploy`, `secret`,
  `domain`, `cloud`, `release`, `env promote|verify|start|stop`, `registry
login`, …) resolve a credential in this order: `--token`, the env's declared
  token variable (CI), the stored `forge login`, then the helper — and an
  EXPIRED `forge login` falls through to the helper instead of failing. A
  helper's token is reused within one process until a minute before it
  expires; caching across processes is the helper's job. When the helper
  refuses, forge prints the host's own advice (Reliant: "sign in to Reliant
  (`reliant auth login` / the app)") and the CI variable — never `forge login`
  — and a 401/403 on a helper-minted token says to sign in again there. Helper
  stdout never reaches an error message or a log. `forge cloud status <env>`
  shows the source and expiry; `forge login` notes when a helper is already
  set. `forge login` and the token variable are unchanged for standalone forge
  and CI.

- **`forge lint --static-export`, and a `static-export lint` lane in every
  `forge lint`.** It judges each Next.js frontend that must build to a static
  export: forge.yaml declares `output: static` for it, or any env binds it to
  `forge.OnHosted`, `forge.OnBucket` or `forge.OnFirebase` (learned from each
  env's real render, not a text scan). It reports with file:line and a fix:

  - **errors**, which `next build` refuses for an export: a dynamic segment
    (`[x]`, `[...x]`, `[[...x]]`) with no `generateStaticParams`, a GET route
    handler not declared `force-static`, `'use server'`, `next/headers`,
    `dynamic = "force-dynamic"` / `revalidate = 0`, and next/image without
    `images.unoptimized` or a custom loader. Also a frontend whose build is not
    an export at all, and forge.yaml and next.config disagreeing about it.
  - **warnings**, for what the export accepts and then drops: `middleware.ts` /
    `proxy.ts`, non-GET route handlers that are not dev-only, next.config
    `rewrites` / `redirects` / `headers` not gated to development,
    `revalidate = N`, and an export bound to `forge.OnBucket` without
    `trailingSlash: true` (a bucket resolves no `.html`, so every route but
    `/` 404s; the hosted origin tries `<path>.html` itself).

  CI's `NODE_ENV=production npm run build`, `forge build` and
  `forge env deploy` run the real export, which stays authoritative. This lane
  reports the same failures earlier and offline. A `[id]` page born from
  forge's CRUD templates names the `migrations/v0.1.44` route migration. It is
  a text scan with no TypeScript AST, so it stays silent rather than guess: a
  re-exported `generateStaticParams` or a next.config built by a function is
  not followed. Honours `--quiet`, `--scope` (by file) and `--json`, and the
  shared `// forge:lint-disable-next-line <rule>: <reason>` directive.

- **Queued hosted deploys: exit 7, the block, and `--wait`.** A control plane
  that ACCEPTS a deploy but holds it on a person — today, billing for the
  hosted workloads or managed database it runs, or for static sites past the
  free tier — no longer makes `forge env deploy` fail or sit out a
  fifteen-minute wait on a promotion nothing is moving. The release, bundle
  and promotion are recorded as usual; forge reads the hold off the Promote
  response (rollout phase `HELD` as the fallback) and prints ONE block — what
  it waits on, why, what to do, and the action URL — then exits **7
  (queued)**, a code nothing else uses.
  The deploy goes live by itself once billing is set up; nothing is re-run, and
  a newer deploy replaces a queued one. `--json` carries the same facts as
  `.queued` (`waiting_on`, `holds[].action_url`). `forge env deploy --wait`
  blocks through the queue until the release is live (or `--timeout`; still
  queued then is 7), and is mutually exclusive with `--no-wait`.
  `forge env status <env>` on a queued env exits 7 with the block instead of
  reporting drift, and `forge env status <env> --wait` waits through it. The
  capacity pre-flight now continues past a "would be queued" answer
  (`CheckDeployCapacity` holds) instead of refusing before the build; an older
  control plane that sends no holds is refused exactly as before. See the
  `deploy/hosted-capacity` skill.
- **Money and basis-point helpers in `@reliantlabs/forge-web-runtime`.** The
  barrel now exports `formatMinorUnits`, `parseMinorUnits`,
  `minorUnitsToInput`, `currencyMinorDigits`, `formatBasisPoints`,
  `parseBasisPoints` and `basisPointsToInput`. They are the web counterpart of
  `pkg/money`: an amount is an integer count of minor units, as a `bigint` or
  an integer `number`, plus an ISO 4217 code. Currency defaults to USD and
  locale to en-US. Each currency's minor digits come from Intl (JPY 0, KWD 3),
  and formatting stays exact past 2^53 because Intl receives a decimal string,
  never a float. Parsing returns `null` instead of rounding or guessing. That
  covers too many decimals, a negative without `allowNegative`, a misplaced
  group separator (`"12,50"` in en-US), another currency's symbol, and values
  outside int64. The input helpers round-trip through the parsers. Rate parsing
  caps at 100% unless you pass `max`. Plain library functions: codegen does not
  infer money from field names. A forge app (roofers) hand-wrote these, along
  with a per-page `bpsToPercentInput`.
- **`// forge:generated <expr>` births a GENERATED column.** The db skill says a
  value derived from the same row should be `GENERATED ALWAYS AS (…) STORED`,
  but birth could only emit `BIGINT NOT NULL DEFAULT 0`, so the owned
  migration was hand-edited straight after every birth (`line_total_cents`,
  `tax_cents`, `balance_cents` in the roofers run). Write the expression on the
  field instead: `int64 line_total_cents = 9; // forge:generated
round(quantity * unit_price_cents)::BIGINT`. The rest of the comment line is
  copied verbatim into `<col> <type> NOT NULL GENERATED ALWAYS AS (<expr>)
STORED`. NOT NULL follows the presence rule every born column follows:
  dropped for an `optional` field and for a Timestamp, so an expression that
  yields NULL for a plain field fails the write rather than storing a value
  the wire cannot carry. There is no DEFAULT; an enum keeps its CHECK and
  protovalidate rules still project. The marker implies `forge:read-only`: the
  field leaves the born Create request, and the generated Update refuses an
  `update_mask` naming it. Before writing anything, birth applies the rendered
  migration to the shadow database inside a rolled-back transaction. An
  expression postgres rejects refuses the birth, naming the marker's file and
  line with postgres's message, and leaves the proto untouched: an unknown
  column, a non-`IMMUTABLE` function such as `now()`, or one generated column
  reading another (both are named). So does a marker with no expression, two
  markers on one field, or a repeated/map/oneof/JSONB field. Supported fields
  are single-valued scalars, enums and Timestamps. The descriptor carries the
  expression too (`SchemaFieldDef.Generated`), so `forge scaffold entity
--from-proto <svc>.<Message>` births the same column as bare `forge
scaffold`. `forge project annotations` lists the marker and the mapping row.
  `pkg/schemadef` gains `OpenShadowAt` and `(*Shadow).TryApply` for asking
  postgres whether SQL would apply without changing the shadow.
- **Version-skew warning for a bridged forge checkout.** When a project's
  go.work (or a directory `replace`) bridges a local forge checkout,
  `forge generate` and `forge lint` print one line — and `forge doctor`
  reports a `Forge Bridge` warning — whenever the running binary was not built
  from that checkout as it is now: a different commit, uncommitted changes on
  either side, or (forge embedded in a host through a workspace, which records
  no forge commit) a checkout that moved after the binary was written. The
  line names both sides and the fix:
  `⚠️  forge skew: reliant (built 2026-10-01 01:31) records no forge commit, and go.work bridges /src/forge, which changed after it was built (now 750833054b78) — generated code and library differ. Fix: rebuild reliant from that checkout, or unbridge: go work edit -dropuse=/src/forge`.
- **A decision table for column write markers** in `db/write-policy`. It picks
  `forge:read-only`, `forge:computed`, `forge:generated`, `forge:guards` or
  `forge:immutable` by who writes the value. It reflects the Update enforcement
  below: guards adds no protection to a read-only column, only the disabled edit
  row naming the rpc that owns it. `forge`, `proto` and the project memory point
  to it.
- **The rule for CRUD vs custom rpcs, stated once and the same everywhere.** An
  rpc is CRUD only when it is unary and named exactly
  `Create|Get|Update|Delete<E>` or `List<Es>`, where `<E>` has a table and a
  same-name proto message. `UpdateJobStatus`, `CreateInvoiceFromEstimate` and
  `ListJobsByCrew` are custom, so there is no need to rename them defensively
  (an agent did in the roofers run). This is in the project memory and in the
  `forge`, `proto` and `api` skills.
- **A retired-claims registry over all shipped guidance**
  (`internal/templates/retired_claims_test.go`). It covers every project and
  service template (skills, `reliant.md.tmpl` and `reliant-reliant.md.tmpl`,
  which render as reliant.md / CLAUDE.md / AGENTS.md / .cursorrules /
  copilot-instructions.md), plus the README and `docs/`. Each row is a claim
  forge retired, with the truth that replaced it, the skill that carries that
  truth (checked to exist), and a bad and a good sample the pattern must catch
  and spare. Against the previous main it reports 16 stale lines.
- **`db/seeds/vocab.yaml` describes timestamps, booleans and NULL.** A time
  column takes instants relative to the day the seed runs:
  `{from: -90d, to: +30d}` (optional `step: 1d`), or a list of `now` / `-3d` /
  `+2w` offsets and absolute ISO dates. A boolean column takes
  `[true, true, false]`, and `null` is a list value for a nullable column
  (`[null, "x"]`). Repeating an entry weights the draw for every kind. A `null`
  on a NOT NULL column refuses the seed and names the column. Before this,
  YAML dropped a null from the list (`[null]` failed with "has no values"), and
  boolean and time columns were ignored with a warning. `seedplan.Config.Now`
  is the anchor: the seed CLI passes the start of the current UTC day. The zero
  value is a fixed instant, so generated mocks, factories and fixtures stay
  byte-stable.
- **`forge db seed apply|reset --dsn` accepts any loopback database**
  (localhost, 127.0.0.0/8, ::1, a unix socket; any port, any name), so a
  throwaway postgres can be seeded. A non-loopback `--dsn` must still be the
  env's declared database unless `--allow-remote-dsn` is passed. The env must
  be dev either way. An ambient `$DATABASE_URL` gets no relaxation, and
  `forge db reset` (DROP DATABASE) keeps the strict check.
- **Context-carried transactions in `pkg/orm`: `RunTx`, `RunTxReadOnly`,
  `RunTxWithOptions` and `AfterCommit`.**
  `s.deps.DB.RunTx(ctx, func(ctx context.Context) error)` runs fn in a
  transaction carried by the ctx it receives. Every query made with that ctx
  against the same database joins it:
  generated delegates (`db.GetJobByID(ctx, s.deps.DB, id)`), stores,
  `pkg/crud.Repo`, `db.Bun()` builders and raw `Exec`/`Query`/`QueryRow`, with
  no handle threaded through signatures. It is SERIALIZABLE by default and
  retries serialization failures and deadlocks (SQLSTATE 40001/40P01) with
  jittered backoff, up to `orm.DefaultTxMaxAttempts` (7). Still conflicting
  after that, it returns `svcerr.Aborted`. A ctx that already carries a
  transaction on the same database is joined, and the inner call's options are
  ignored. `orm.AfterCommit(ctx, fn)` runs fn once, after the outermost commit;
  rolled-back and retried attempts discard it. fn may run more than once, so it
  must have no side effects outside the database. Read-check-write state
  transitions therefore need no `SELECT … FOR UPDATE` lock helpers. The new
  `service-layer/transactions` skill covers RunTx, AfterCommit, a lock-free
  state-transition recipe and migrating lock helpers, and `service-layer`,
  `db`, `db/crud-overrides` and `interactor` point to it. Executor resolution is
  guarded by identity: a transaction carried for one `*sql.DB` never runs a
  statement sent to another.
- **`crud.Preserve(columns...)`, a per-call write policy for a full-replace
  Update.** The generated `db.Update<Entity>` delegate takes
  `opts ...crud.UpdateOption`; existing calls compile unchanged. `Preserve`
  keeps the named columns out of that one write and reads their stored values
  back. Use it when the caller, not the column, decides what may be written.
  `forge:immutable` (`,skipupdate`) remains the answer when no full replace
  should ever rewrite the column. Naming a column that does not exist is an
  error, not a no-op, so a typo cannot silently disable the protection.
- **`crud.UpdateOp.ReadOnly`**: the columns `HandleUpdate` refuses in a client
  `update_mask`. Generated from the entity's `forge:read-only` /
  `forge:computed` fields.
- **`forge lint --quiet` (`-q`).** Prints only what failed — a one-line summary
  per failing linter, its error findings with fix hints, and the verdict as
  the last line; a clean run is one line. Auto-fix still applies, so the
  verdict matches the default run. Combines with any targeted flag
  (`forge lint --read-only-fields --quiet`), with `--scope` and with
  `--gate-json`. Agents were piping full lint through `head` and losing the
  result.
- **`forge lint --scope <path>`** (repeatable or comma-separated). Reports only
  findings in files under the given project-relative paths, so an agent
  sharing a checkout can lint its slice. golangci-lint, the typed-config
  guardrail and the contract linter run on the scope's Go packages only;
  file-anchored linters run project-wide and report what is under the scope
  (a finding with no file is never hidden); the whole-project linters —
  frontend lint, component-drift and `--config-reach` — cannot be scoped, are
  skipped, and are named in the verdict ("run an unscoped `forge lint` before
  merging"). Auto-fix touches only files under the scope. Works with `--json`
  and `--quiet`; refused with positional package paths and with `--gate-json`
  (a gate records whole-project evidence). Every pipeline lane must declare
  its scope mode, pinned by a test.
- **`seedplan.Config.Minimal`** — plan the smallest row the schema accepts
  instead of the fullest: only keys, NOT NULL columns without a usable
  DEFAULT, NOT NULL references, UNIQUE/ordered NOT NULL columns and columns a
  child references are written; discriminated-union and status-guard CHECKs
  take the branch closest to the column DEFAULTs, the same branch on every
  row. `Plan.Writes(table, column)`, `Plan.NaturalValue(...)` and
  `MinimalUnionPlacement` expose the decision; `UnionCell.Present` reports a
  branch's IS NOT NULL requirement. `forge db seed` is unchanged (full plans).
- **Windows support.** Native windows/amd64 and windows/arm64, no C toolchain:
  - process lifecycle (tree kill, liveness, parent/port/start-time lookup,
    reading another process's argv and environment for stale-stack reclaim);
  - an in-process POSIX shell for ShellBuild and migration detection;
  - a delve JSON-RPC client that builds on windows/arm64;
  - `[build.windows]` sections in the scaffolded air configs, `.exe` host
    binaries from `forge build` and the `binary`/`delve` runners;
  - zitadel's Windows binary for the host IdP;
  - in-use file detection via the Restart Manager, so reclaimers work;
  - a directory-junction fallback when symlinks need privileges;
  - portable scaffolded Taskfiles (guarded by a test), and a `.gitattributes`
    keeping generated files LF;
  - `forge doctor` floors for Windows: air >= 1.65.3, Task >= 3.45.5;
  - `forge storage schedule` registers a daily Task Scheduler task;
  - a `windows-latest` CI job that builds, tests and renders a scaffold.
- **`forge doctor` checks air** (any OS, >= 1.63.2) for projects with an
  `.air.toml`: the scaffolded config names its binary with `entrypoint`, which
  older air does not read, so it would have nothing to run.
- **`cli.RenderSkill` — a skill exactly as `skill load` prints it, for
  harnesses.** `cli.LoadSkill` returns the raw body, so a harness injecting a
  skill into an agent's context handed it `forge generate` while
  `reliant forge skill load` (and the project memory) said
  `reliant forge generate` — a command the agent may not have, or a different
  forge build than the one that generated its project. `RenderSkill` applies
  the same resolution, version-skew advisory, audience filter and command-name
  rewrite as the CLI; `skill load` and `forge start` now call the same
  renderer, so the two cannot drift. `RenderSkillOptions.CLIName` defaults to
  the name this process reports, the same one `cli.RenderProjectMemory`
  renders.

### Fixed

- **`schemadef.OpenShadowAt` returns no shadow when it fails.** It used to
  return the live scratch database beside a migration-replay error, so the
  only correct call shape was `defer shadow.Close()` BEFORE checking `err` —
  and a caller writing the ordinary err-check-first form leaked the database.
  It now drops what it opened and returns `nil, err`: check `err`, then defer
  `Close`. (`Close` stays nil-safe, so existing callers keep working.)
- **A pre-rollout Job wait no longer leaves a `kubectl wait` running after it
  returns.** The wait races a `condition=complete` watcher against a
  `condition=failed` one and answers with the first; the loser was only
  signalled, so it could outlive the deploy step that started it. It is now
  killed and reaped before the verdict is returned.
- **A service, entity, field or RPC whose name has a digit in it now builds —
  forge spells every identifier it shares with buf's generators by THEIR
  casing rules.** protoc-gen-go camel-cases a proto name word by word and
  treats a digit as a word, so the letter after it is capitalised; forge
  title-cased only after `_`. A service named `alphav1connect` made forge emit
  `UnimplementedAlphav1connectServiceHandler` where connect-go declares
  `UnimplementedAlphav1ConnectServiceHandler`; an entity `Base64Item`
  (field `base64item`) made the Update op read `req.Base64item` where the
  field is `Base64Item`; and an entity scaffolded as `Oauth2token` broke every
  pb type, RPC method and response field (`pb.CreateOauth2tokenRequest` vs
  `CreateOauth2TokenRequest`). `internal/naming` now carries ports of the
  generators' own rules — `GoCamelCase` (protoc-gen-go's `strs.GoCamelCase`),
  `GoFieldNames` (its per-message rename of a field that collides with a
  generated method or getter: `descriptor` → `Descriptor_`), the connect-go
  names (whose `<Service>Name` constant, alone, is spelled from the RAW proto
  name), and protobuf-es's `protoCamelCase`, method `localName` and `$`
  escapes — and every emitter that names a generated symbol goes through them:
  the CRUD ops, shims and born tests, handler stubs and the stub/shim
  dedupe scans, the mocks, `service.go`, the test clients, the public
  procedure list, `forge scaffold rpc`, the read-only/computed-field lints,
  the `unscoped_auth` audit (which looked a handler up by the proto rpc name,
  so a digit-named authenticated RPC was never inspected), and on the TS side the hooks' `client.<method>` (an RPC `LLMChat` is
  `client.lLMChat`, not `llmChat`) and the pages' and mock transport's entity
  fields (`data?.base64item`, not `base64Item`). The pb message and forge's
  `db.<Entity>` row name a column differently (`sha256sum`: pb `Sha256Sum`,
  db `Sha256sum`; `address_line_2`: pb `AddressLine_2`, db `AddressLine2`), so
  the conversions now spell each side with its own rule; the ORM's rule is
  unchanged (`naming.ColumnGoName`, formerly `ToProtoPascalCase`, which claimed
  to be protoc-gen-go's and was not), so no existing struct field is renamed.
  The same derivation fixes entities with a leading acronym, whose response
  field forge spelled from the entity name (`LLMKey` → field `llm_key` →
  `LlmKey`, not `LLMKey`). `TestGeneratedNames_MatchRealPlugins` builds
  protoc-gen-go and protoc-gen-connect-go at the go.mod versions, runs them on
  a fixture of such names and checks every derived identifier is declared;
  `TestE2EDigitNamedServicesAndEntitiesBuild` scaffolds them end to end
  (build, vet, idempotent regenerate, the born CRUD tests on postgres, `tsc`).
  Not covered: a message named after a TS-reserved identifier (`Object`,
  `Partial`), which protoc-gen-es exports as `Object$`.

- **Two services may declare the same message name without breaking the
  frontend's `tsc`.** Proto keeps message names per package, so alpha and beta
  each declaring `PingRequest` is ordinary — but the generated TS files that
  import from several proto modules into one scope imported each name bare, and
  a name from two modules is TS2300 `Duplicate identifier 'PingRequest'`. The
  project-wide scenario handler map (`src/mocks/scenario-rpcs_gen.ts`) hit this
  as soon as two services shared an RPC shape; `mock-transport_gen.ts` did for
  two entities whose services name a response alike (`GetResponse`); and a
  service's hooks file did when an RPC took a request type from another proto
  file that shared a name with one of its own. A name imported from more than
  one module is now imported `as <module>_<Name>`
  (`services_alpha_v1_alpha_pb_PingRequest`) from each, and the file refers to
  that; a name only one module supplies keeps its own, so files without a
  clash are byte-identical. `mock-transport_gen.ts` also imported an entity's
  fixture module once per service that lists the entity (`import * as
thingsMocks` three times for a `ListThings` in three services); it is now
  imported once and every service's dispatch row is seeded from it.

- **A component named like an identifier `compose.go` itself uses no longer
  breaks the first build.** A service, worker or operator was imported into
  `internal/app` under its package name, beside the names the generated files
  already use — `NewComponents`' locals (`c`, `infra`, `err`), its imports
  (`fmt`, `slog`, `time`, `db`, `ctrl`, …) and package `app`'s own
  declarations — so `forge project new x --service c` failed its first
  generate with `c.New undefined (type *Components has no field or method
New)`, and `fmt`, `slog`, `err`, `infra` failed the same way (`fmt
redeclared in this block`). Such a component is now imported as
  `<role><Name>` (`svcC`, `wkrFmt`), the form two components that share a
  package name already get; every other keeps its package name, so no
  `compose.go` that compiled before changes. The set of names in use is not a
  list forge maintains: it is read off the `compose.go`/`lifecycle.go`
  templates themselves and the project's own `internal/app` files, so a local
  added to a template later is covered the day it is written. A
  `TestGenerateCompose_EveryTemplateIdentifierAsAComponentName` sweep builds a
  project with one service — and one worker — per such name. `main` is
  refused up front by `forge project new --service`, `forge scaffold
service|worker|operator|library`: Go cannot import package main, so the
  project could never build.

- **`forge project rescaffold` re-creates the hosted CI pipeline again.**
  Since new projects are born hosted, the scaffold writes `release.yml` and
  `.github/actions/forge-deploy/action.yml` for the staging and prod it just
  wrote — but rescaffold decides which CI files a project has from a render of
  every env, and those envs import config modules (`config_gen.k`, each env's
  `config.k`) only `forge generate` writes. On a project generate had not
  completed on (a fresh scaffold whose bootstrap generate failed), the envs
  did not render, rescaffold took them for envs that are not hosted, and
  refused both files ("this project declares none"). It also memoized that
  answer and handed it to the generate pipeline it ran next, so the
  pipeline's CI step — after writing the very modules that made the envs
  render — still wrote a `deploy.yml` beside `release.yml` and a
  `build-images.yml` for cluster envs. Rescaffold now writes the env config
  modules first when an env does not render (the new `forge generate --steps
env-config` preset), never hands its answer to the pipeline, and names an
  env that still fails to render (`forge env render <env>`) instead of
  calling it unhosted. `TestRescaffold_EveryScaffoldedFileIsReemittable` had
  been failing on main since #518.

- **`forge project new` no longer warns `frontend config: could not read
deploy/kcl/dev/config.k` — and no longer writes the dev `config.js` from
  proto defaults.** It scaffolds into `<--path>/<name>`, a relative path, and
  generates from there; the frontend-config probe at
  `shop/deploy/kcl/dev/zz_forge_frontend_config_probe_<pid>.k` was handed to
  kpm, which resolves a relative source against the work dir (`shop`) and
  looked for `shop/shop/deploy/...`. The fallback's `API_URL` was the proto
  default, `http://localhost:8080` — the dev IdP's port. `kclrender` now
  resolves the work dir and the source against the process cwd before kpm
  sees either, so every render means one thing by a relative path — the
  backend config probe, `env new --check` and `RunInWorkDir` (which entered a
  relative work dir and resolved it again from inside) had the same defect;
  #520 anchored the frontend probe alone. The two doctor renders that passed
  a work-dir-relative source pass an absolute one.
- **A freshly scaffolded operator renders.** `forge scaffold operator`
  declares the workload before its first CRD (`crds = []`; `forge scaffold
crd` is the next step), and the workload schema required at least one CRD
  of every operator — so every env, dev included, failed to render with a
  schema error naming neither the operator nor the fix. An operator may now
  list none: one that owns no CRD yet, or reconciles built-in kinds only
  (granted by `clusterRBAC`); `crds` is what its derived ClusterRole covers,
  and an empty list derives nothing. Dev renders it on k3d; a hosted env,
  where it is bound `_on_cluster`, refuses only for the undeclared `_cluster`,
  naming the operator. `forge scaffold crd <Kind>` now adds the kind to the
  operator's `crds` in `deploy/kcl/workloads.k` (the derived RBAC reads only
  that list; before, the user had to know to), and prints the edit when the
  list cannot be found unambiguously. Relaxed in `pkg/deploy/v1alpha1`'s
  `WorkloadSpec.Validate` too, so the control plane's admission agrees.
- **A born list page no longer links its rows to a detail page that does not
  exist.** Every list row called `router.push('/<slug>/<id>')`, but the detail
  page is only generated when the service has a Get RPC — so a List-only
  entity's rows were all 404s. Rows link only when the detail page is
  generated (`PageTemplateData.EmitsDetailPage`, which also gates the detail
  route itself and the create page's landing); otherwise they are inert and
  the list keeps the id/created/updated columns it would have left to the
  detail page.

- **`next dev` no longer rewrites the scaffolded `tsconfig.json`.** Next 16
  checks the file on every `next dev` and `next build`. When it finds a value
  missing it rewrites the whole committed file, reformatted. The Next.js
  scaffold shipped `"jsx": "preserve"`, which Next 16 forces to `react-jsx`, and
  lacked the route-type globs Next appends to `include`. So every dev run left
  a diff. The template now ships `"jsx": "react-jsx"` and includes
  `.next/types/**/*.ts`, `.next/dev/types/**/*.ts` and their `.next-prod`
  twins, which `next build` asks for under forge's production `distDir`. A
  dev or build run now leaves the file byte-identical. `tsc` reads the same
  files as before, because `exclude` still drops `.next` and `.next-prod`. The
  Vite SPA template already used `react-jsx`, and Vite never writes its
  tsconfig. Expo only adds `extends`, which the React Native template already
  has. Existing projects can commit the tsconfig Next already rewrote; after
  their next `npm run build`, they also commit the two `.next-prod` globs.
- **`<Resource>` and `DefaultErrorFallback` show `userMessage(error)`.** Both
  printed the raw `error.message` in monospace. A `ConnectError` reached the
  user with its transport framing intact (`[not_found] no such job`). This
  broke the runtime's own rule that errors are displayed through
  `userMessage`. Both now render the framing-free message as body text.
  `<Resource>` falls back to generic copy when the error has no message.
- **Mock mode signs the UI in.** With `MOCK_API` set to `"true"` or
  `"hybrid"`, the scaffolded web auth context (`src/lib/auth/context.tsx`)
  now takes its identity from the fixture session in `session-provider.ts`
  (new export `isMockMode()`) and never sends `GET /auth/session`, so the
  route guard lets every page through. Previously the context asked the
  server in every mode: in pure mock the request failed, the app read
  signed-out, and every page bounced to `/auth/sign-in`. In mock mode, logout
  ends the fixture session locally, and a reload restores it. Hybrid renders
  the same fixture user, but the fixture is presentation only. Its token is
  still handed out in pure mock alone, so a forwarded hybrid RPC carries only
  the real session cookie the browser already holds, and `/auth/sign-in`
  still posts to the real server. With mock mode off, nothing changes. New
  frontends ship `src/lib/auth/context.test.tsx` (Vitest), which pins all
  three modes. `context.tsx` is scaffold-once, so existing projects see the
  change through `forge project upgrade`'s advisory lane.
- **Fixture Lists answer like the backend's generated List.**
  `@reliantlabs/forge-web-runtime/mock-transport` now:

  - filters by equality on every `optional` List request field that is set
    and names an entity field (enums compare by value; implicit-presence
    fields never filter, matching the generated `Filters` closure);
  - applies `search` as a case-insensitive substring match over string fields
    and enum value names;
  - sorts by `order_by` / `descending`, rejecting an unknown column with
    `InvalidArgument`;
  - applies `page_size` (default 50, max 100) with an offset `page_token`;
  - returns `total_count` as the filtered count before paging.

  Previously every List returned every fixture row with `total_count` 0, so
  filtered lists showed everything and dashboard counts read 0. Like the
  backend, an ordered list mints no `next_page_token`. The request schema
  comes from the Connect method descriptor, so `mock-transport_gen.ts` is
  unchanged. Custom (non-CRUD) RPCs are still answered only by scenario
  handlers.

- **Born List requests filter by `optional` fields too.** Quintet completion
  gave a List facet to every bool, enum and `<stem>_id` reference except an
  `optional` one, so the nullable reference was the one FK a born list could
  not filter by (`optional string crew_id` on the roofers `Job`, unassigned
  until dispatch, while `customer_id` and `property_id` got facets). The
  `forge:read-only` marker on it played no part. The facet gate predated FK and
  enum facets and skipped every field with presence, though a facet is
  `optional` on the request either way. An optional reference, enum or bool now
  gets its facet, and a nullable text column counts toward `search`. The
  generated filter is unchanged: `col = $1`, which rows holding NULL never
  match. Matching the NULL rows ("jobs with no crew") is not a facet; add a
  request field and filter in the op. Born once: an existing List request
  gains nothing, so add the field by hand.
- **Generated create/edit forms and list filters follow the field's declared
  rules.** Four defects from the roofers run, in both the Next.js and Vite
  page templates:

  - A field is required only when a rule says so: protovalidate
    `required = true`, `string.min_len >= 1`, or a non-optional foreign key
    (born `NOT NULL REFERENCES` with no DEFAULT). Every other `NOT NULL`
    column used to be born `.min(1, "Required")`, so a `string notes` column
    (`NOT NULL DEFAULT ''`) was mandatory. `max_len`, `email`, `pattern` and
    numeric bounds are still projected as before.
  - An empty input for a proto3 `optional` field submits as unset on both
    forms. Its zod schema maps `""` to `undefined` before validating, so
    `optional int32 year` stores NULL instead of 0, and an empty optional
    email is not rejected by `.email()`. The edit prefill no longer seeds
    the zero, and the update mask still names the field, so clearing a
    stored value writes NULL.
  - The enum zero value (`*_UNSPECIFIED`, found by wire number 0) is never
    offered in a form select or a list filter. The born CHECK rejects it.
    Both forms refuse it in zod. A `NOT NULL` enum's create select starts on
    the first real member, which is the column DEFAULT and what Create stores
    when the field is omitted. An `optional` enum gets a None option.
  - Create navigates to the new record's detail page (`/<entity>/<id>`),
    read from the Create response's entity field. It falls back to the list
    when the response does not carry the entity.

  Scaffolded pages are yours, so existing projects keep their pages. Delete
  a page and run `forge project rescaffold <path>` to pick up the new form.

- **A CRUD op no longer disappears when its shim moves out of
  `handlers_crud.go`.** Generate decided which `crud<Rpc>Op`s to emit by
  file: a method declared anywhere except `handlers_crud.go` counted as
  "implemented by hand" and lost its op. Moving a shim verbatim into a
  sibling file (roofers: `CreatePayment` into `invoice_ops.go`) therefore
  made the next generate drop `crudCreatePaymentOp`, fail its own build
  validation and revert 70 files. Emission now follows what the package's
  code calls, in any file: an op is emitted when no method declares the rpc
  (forge appends a shim) or when anything in the package calls
  `s.crud<Rpc>Op`; a method that calls no op is hand-written wherever it
  lives, so it gets no op and the op's own checks do not apply. Shims are
  appended only for rpcs no file of the package declares, so a moved shim is
  never declared twice. `<entity>ToProto` / `<entity>FromProto` stay emitted
  while any code in the package calls them, even when every CRUD rpc of the
  entity is hand-written. Behavior change: a real implementation written
  inside `handlers_crud.go` (one that no longer calls its op) is now treated
  like one in any other file — no op, no op validation.
- **A failed validate build names the error on its ROOT CAUSE line.** The
  line read `go build failed: exit status 1. Fix: ensure all referenced types
are imported` whatever the error was. It now quotes the first compiler
  error (`file:line:col: message`, plus a count of the rest) and says whether
  that file is hand-written (fix it) or generated (fix its inputs, or report
  a forge bug). A failing generated `_test.go` typecheck quotes its first
  error the same way. The revert is unchanged.
- **`forge env down` never stops the process running it.** forge's ownership
  markers are environment variables, inherited by everything a forge-started
  process runs — an agent server's shells included — so `forge env down <env>`
  or `--all` typed inside one selected the very server hosting it and ended
  the session along with every sibling session. Every stop path (`env down`,
  `env down --all`, the `env up` pre-flight, host-infra shutdown, `debug stop`)
  now walks the command's own parent chain (darwin, linux, the BSDs, Windows)
  and never signals an ancestor or the tree under it, reporting
  `skipped pid 1234 (reliant serve …): it is an ancestor of this command —
stopping it would end the session running you`. Everything else is stopped;
  the per-env form leaves that env's host infrastructure up, records stay so
  `forge env ps` still lists the stack, and `forge env up` refuses (stopping
  nothing) when the stack it would replace hosts it. The tree-kill primitive
  refuses an ancestor too, and skips a group-wide signal to a process group
  the command belongs to.
- **A custom rpc whose name starts with a CRUD verb is custom on the frontend
  too.** The server has always required a CRUD rpc's entity to have a table and
  a wire message. The hook and page generators split the name on the verb alone.
  So `useUpdateJobStatus` invalidated a `jobStatus` query scope that no query is
  keyed under, and the Job list and detail it had just changed stayed cached
  until a hard refresh. `CreateInvoiceFromEstimate` left the invoice list stale
  in the same way, and `ListJobsByCrew` produced a `jobs-by-crews` CRUD page,
  nav entry and mock fixture. Both generators now apply the server's message
  check (`crudEntity`). A custom mutation invalidates every query on its service,
  and a custom List births no page. A descriptor written before the deep message
  inventory existed keeps the old name-only answer. Regenerate to pick it up.
- **`forge:guards` on a `forge:read-only` column renders its disabled row.** The
  edit page dropped read-only fields before the guard pass looked, so with both
  markers the column vanished from the page and nothing named the rpc that owns
  it. Now read-only alone still hides the field, and read-only plus guards
  renders the disabled "changed through `<Rpc>`" row. That combination is right
  for a lifecycle column: the API refuses the write, and the page says who can
  make it.
- **The project memory no longer contradicts the code.** `reliant.md.tmpl` is
  loaded into every session, and it described thin handlers in front of an
  `internal/<svc>/contract.go` business layer, with an `errors.go`
  `connect.Code` switch, that `forge scaffold service` has never written and
  `forgeconv-no-handler-error-mapping` flags. It also said `auth_required`
  "gates nothing at runtime", although the interceptor enforces it fail-closed.
  It now shows what scaffold writes: logic on the `*Service` rpc methods in
  `internal/handlers/<svc>/`, with `forge scaffold package` for logic that
  needs isolation, `svcerr.Wrap` and `RunTx`. The same corrections apply to the
  `api`, `db`, `services`, `proto-split`, `migration`, `migration-service`,
  `service-layer`, `architecture` and `api/role-interface` skills (stale
  `contract.go`-beside-handlers and `<svc>_mock.go` paths), to
  `docs/getting-started.md`, and to the scaffolded `service.go` comment, which
  said Deps were resolved by name (they are resolved by type).
  `internal/app/compose.go` ownership was already consistent on main (yours,
  reconciled). A harness with an older embedded forge can still show "forge-owned
  and regenerated", and the retired-claims registry keeps that phrasing out.
- **`forge db` commands read the `-C` project, not the CWD.** `--dir` defaulted
  to a relative `db/migrations` computed before `-C` was parsed, so
  `forge db migrate up -C <proj>` from `/tmp` failed with
  `open /tmp/db/migrations/.`. `forge db seed apply -C <proj>` read migrations,
  `vocab.yaml` and `db/seeds/custom/` from the CWD, then printed
  `Seeded 0 row(s) across 0 table(s)` and exited 0. Every db subcommand
  (migrate, migration new/rebase, seed, reset, squash) now resolves the
  migrations directory, a relative `--dir` and the seed overlays against the
  project root. A missing migrations directory is an error naming the path.
- **Seeding nothing is an error when there was something to seed.** `seed
apply`/`seed reset`/`db reset` fail when the database has tables the
  migrations read do not define (the wrong `-C`/`--dir`), when
  `database.seed.tables` names no real table, or when the plan inserted nothing
  into tables that are still empty. `database.seed.tables: []`, an
  already-seeded database and a schema with no tables still succeed.
- **Undescribed seeded timestamps sit in the four weeks before today**, not
  January 2024.
- **The mock-fixture freshness test names both causes.** Editing only
  `db/seeds/vocab.yaml` fails `fixture-freshness_gen.test.ts`, which is
  correct, but the message said the schema had changed. It now lists the schema
  (`db/migrations`) and the seed vocabulary (`db/seeds/vocab.yaml`), and says to
  run `forge generate` (`reliant forge generate` inside reliant) from the
  project root.
- **ORM writes return the values the database computed.** `pkg/crud.Repo`'s
  Create, Upsert, Update and UpdateMasked now `RETURNING` every
  `GENERATED ALWAYS AS (…) STORED` column into the entity. Create and Upsert
  also return each column the insert left to its DB `DEFAULT` (a nil pointer,
  or a zero `,nullzero` / `,default:` field), along with a server-allocated
  PK. The generated delegates and stores inherit this. Previously a generated
  column came back as the Go zero after Create and stale after an update, and
  on a serial-PK table the explicit `RETURNING id` also suppressed Bun's own
  read-back of defaulted columns. So every caller re-read the row
  (`line_total_cents`, `total_cents`, `balance_cents` in the roofers run).
  The RETURNING list names only the struct's own columns, never `*`, so
  inserts keep working while a migration that adds a column has been applied
  ahead of the code. A write that matches no row is still `orm.ErrNoRows`.
- **A client Update no longer writes `forge:read-only` / `forge:computed`
  columns.** Both markers promise "readable, not client-writable", and the born
  Create request keeps that promise. The AIP-134 `Update<Entity>Request` wraps
  the whole entity, though, and the generated CRUD Update wrote it all back. Any
  authenticated caller could set a server-owned column directly (an invoice
  straight to PAID, past the RPC that owns the state machine), and a maskless
  Update from a client that simply didn't echo the field reset it: an unset enum
  became the column DEFAULT, a lifecycle timestamp became NULL, a computed total
  became 0. Nothing errored and nothing was logged. An `update_mask` naming the
  column was accepted as well. The generated Update op now carries the entity's
  read-only columns as `op.ReadOnly`. `crud.HandleUpdate` refuses a mask path
  naming one with `InvalidArgument`, reason `unknown_field`, and the full
  replace passes `crud.Preserve(...)`, so the stored value survives whatever the
  request carried. Your own code is unaffected: `db.Update<Entity>Masked`
  naming the column (what a custom RPC calls), `Create`, and a plain
  `db.Update<Entity>` all still write it. The policy is read from the proto on
  every `forge generate`, the same reading that already shapes the Create
  request and the edit form, so existing projects are fixed by regenerating, with
  no migration. Hand-written overrides that reloaded the stored row and copied
  the editable fields onto it, or filtered lifecycle paths out of the mask, can
  go. AIP-203 asks servers to ignore output-only mask paths; forge refuses them
  instead, because a 200 that changed nothing is a silent non-write (an override
  wanting AIP behaviour sets `op.ReadOnly = nil`). A value derived in an Update
  `op.Entity` hook is no longer persisted by the generated Update. Derive it
  where its inputs change, with `db.Update<Entity>Masked` naming it, or as a
  GENERATED column.
- **Update responses report the stored row, not the request.** Beyond the
  generated columns above, `Repo.Update` and `Repo.UpdateMasked` now read back
  (`RETURNING`) every column the write did not set, in the same round trip. The
  entity a caller holds afterwards, and so the CRUD response, carries the stored
  value of a held-back (`forge:immutable`, read-only, secret) column and of every
  column a mask did not name, rather than whatever the request carried. Before,
  a full replace that correctly left `status` alone still answered with the
  reset `status` the client sent.
- **A freshly scaffolded project no longer warns `gen-missing-source` on
  forge's own files.** Three emitters hand-wrote a header that stopped at the
  forge-owned banner: the ORM projection (`internal/db/<entity>_orm_gen.go`),
  the service mocks (`internal/handlers/mocks/<svc>_mock_gen.go`) and
  `db/embed_gen.go`. Each now stamps a `// Source:` line naming what it is
  derived from. Nothing held emitters to the rule `forge lint` holds every
  `_gen` file to, so `internal/tierguard` now runs that exact rule
  (`scaffolds.LintGeneratedHeader`) over every Tier-1 file in its rendered
  fixtures. Existing projects pick the lines up on the next `forge generate`;
  the finding on a forge-generated file now says that, instead of telling the
  user to edit a file marked DO NOT EDIT.
- **`forge lint --read-only-fields --json` and `--guarded-fields --json` run
  their one lane.** Both were missing from the targeted JSON table and fell
  through to the whole suite.
- **`forge lint --conventions` reports proto paths from the project root**
  (`proto/services/x/v1/x.proto`, not `services/x/v1/x.proto`), so they are
  openable and `--scope` can match them.
- **forge only kills processes it can prove are its own.** On every OS, a pid
  read from a pidfile (embedded postgres, the host zitadel, a recorded dlv)
  is killed only if its executable matches and it started no later than the
  pidfile was written — a recycled pid is left alone. On Windows, where pids
  recycle fast and a parent pid is never updated, process-tree ownership and
  teardown also require the parent to be alive and older than the child, and
  a graceful stop (CTRL_BREAK) is sent only to a verified process-group
  leader, never broadcast to the whole console.
- **Background `forge env up` stacks on Windows survive closing the
  terminal** (DETACHED_PROCESS), as they do on Unix.
- **`deploy/static-site` skill: forge is for static sites too.** Agents
  reaching for forge only when there is a backend routed every landing page,
  marketing site and docs site around it, even though forge already ships one
  end to end: a static export bound to `forge.OnHosted {}` is published to the
  control plane's static hosting (bucket, CDN and hostname owned by the
  platform), with envs, build-once releases and promotion. The skill covers when
  forge is the right call for a site, the static frontend scaffold, a complete
  frontends-only hosted `prod`, `runtime_config`, and the release flow, all
  verified against a real project. The root `forge` skill, `deploy`,
  `frontend` and the README now route to it. The scaffold gaps it works around
  are tracked in #401.

### Changed

- **`forge project new` with frontends and no services leads with the
  frontend.** The next-steps block told a frontend-only project to
  `forge scaffold service item`, which is how a static site ends up with a CRUD
  backend nobody asked for. It now points at the frontend and
  `deploy/static-site`, and keeps `scaffold service` as the line for a project
  that does need an API. It does not print a bare `forge env up dev`, because a
  frontend-only scaffold's dev env still fails that way until #401 lands.
- **The README stopped advertising `forge.External`.** It was removed with the
  one workload model (#284), but the README still offered it for Fly.io, Cloud
  Run, ECS, Vercel and Railway, and described deploy as `forge.Service` plus a
  `host` block. Both now describe the per-workload runtimes, and a
  platform-CLI runtime is tracked in #400.

- **`forge env status` is the ONE read view of an environment.** Reading an env
  used to take six verbs — `env status`, `env verify`, `env wait`,
  `env rollout`, `env topology`, `env history` — and they were views of a
  single question, split by which half of the answer each happened to own. A
  reader had to know, before they could ask, that the bound release lived in
  `verify`, the rollout phase in `rollout`, the runtime ports in `status`, and
  the promotion that caused all of it in `history`. Two were literal
  duplicates: `env rollout` WAS `env wait --timeout 0`.

  They are modes of one command now:

  ```bash
  forge env status                  # every environment, and how far behind each is
  forge env status prod             # prod right now: runtime AND release
  forge env status prod --wait      # block until the rollout settles
  forge env status prod --history   # prod's promotion ledger, newest first
  ```

  One env's report carries the bound release, verify (running digests vs the
  binding, in all five states, through the control plane's observer for a
  hosted env), the rollout phase per workload, runtime health, gates, and
  ledger freshness. The two halves are deliberately asymmetric about failure:
  runtime health REPORTS (a down stack is a state this command must be able to
  print), while the release half makes a claim and so owns the exit code.

  **Every exit code is unchanged**, because every mode reaches the same code
  the old verb called: 0/1/2 for the read, plus 5 (timed out, still
  progressing) and 6 (superseded) under `--wait`. `--timeout` serves two modes
  with different budgets — 60s for a cluster read, 15m for a wait — so its
  declared default is an unset sentinel resolved per mode; `--wait --timeout 0`
  still means one read, never blocking.

  `--json` emits one document carrying the standard envelope. Its release
  fields stay FLAT (`bound`, `images`, `release`) because `forge gate record
--from` recognises the document by them.

### Removed

- **BREAKING: `forge env verify|wait|rollout|topology|history` are deleted**,
  with no alias and no hidden name — they are modes of `forge env status` (see
  Changed, above). `forge release where` and `forge release verify` are
  untouched: both answer a question about a RELEASE rather than an
  environment. A removalguard entry pins all five spellings on every surface.

- **BREAKING: `forge env promote` is gone — `forge env deploy <env> [vX | --from
<src-env>]` does the whole job.** No alias and no hidden name; the old
  spelling dies on "unknown command".

  ```bash
  forge env deploy prod v1.4.0          # was: forge env promote v1.4.0 --to prod && forge env deploy prod
  forge env deploy prod --from staging  # exactly what staging runs
  forge env deploy prod                 # unchanged: re-apply prod's current binding
  ```

  **The verb now means record + apply + wait, and the wait is not a flag.**
  Recording a binding ships nothing, so a pipeline step that only promoted
  reported success before any byte had moved and the release's real failure
  surfaced minutes later with nothing connecting the two. Every pipeline
  therefore spelled it `promote --deploy --wait`, and the spellings that
  omitted either half were bugs waiting for an incident. So the health gate is
  ON by default; `--no-wait` opts out and names
  `forge env status <env> --wait` as the way to gate later.

  **Who applies it is read off the environment's ledger, not a flag** — the
  same declarative fact as where the promotion is recorded, so the two cannot
  disagree. An env whose KCL declares `forge.ControlPlane` is converged by
  that control plane and forge waits on the rollout it computes, pinned to the
  promotion just written. Every other env is self-managed: the same command
  renders the new binding and applies it from this machine, and that apply's
  per-resource rollout wait IS the health gate.

  `promote --wait` and `promote --deploy` are removed with the verb rather than
  renamed: both are now what the command does.

  Everything else is unchanged and moved with the code — the compare-and-set
  (`--expect-current` / `--expect-unbound` / `--supersede`), `--gate`,
  `--from` / `--from-promotion`, `--plan`, the run flags, every exit code and
  the JSON envelope. A deploy that names no version keeps today's spec-change
  behaviour, and a release-only flag passed without a version is now refused
  rather than silently ignored.

- **BREAKING: an environment no longer has an image registry — a WORKLOAD has
  one, as part of its image.** `forge.ClusterTarget.registry`,
  `forge.ControlPlane.registry` and `forge.DockerBuild.registry` are all gone.
  The author writes the full reference on the workload and forge contributes
  only what it alone knows:

  ```kcl
  # deploy/kcl/workloads.k
  api = fw.Workload {
      name = "api"
      image = "ghcr.io/acme/api"        # the registry IS the image
      build = forge.GoBuild {cmd = "./cmd/acme", output_name = "acme"}
  }
  ```

  forge appends the resolved tag and, after a push, the digest. It never
  invents, prefixes or rewrites the host. So the reference you read in the KCL
  is the reference the build pushes and the cluster pulls — they cannot
  disagree, which is the whole point: a build aimed at one registry while the
  spec named another was an `ErrImagePull` that surfaced only at rollout.

  Two workloads in one env may now name two different registries, which an
  env-wide field could not express at all.

  **Migration is automatic.** `forge generate` removes `registry = …` from every
  ClusterTarget / ControlPlane and prefixes the removed value onto a workload's
  bare `image`, judging ambiguity ONLY over the envs where that workload is
  bound to a runtime that pulls an image (cluster or hosted). A registry on an
  env where the workload runs `OnHost`, or is not bound at all, is simply
  dropped — it was never that workload's. If the same workload genuinely pulls
  from two different registries, forge REFUSES and prints the exact per-env KCL
  rather than picking one. Your binder expressions are never edited.

  **A bare image is refused at render**, naming the workload: it would render as
  `api:<tag>`, which the kubelet resolves against Docker Hub and fails to pull.
  A third-party image must name its host too (`docker.io/library/nats:2.10`).

  A hosted env is no different, and gets its own runbook: a hosted env may one
  day inherit a default from the control plane's advertised image push base, but
  that base is not pushable yet (control-plane ADR 0003 / #334), so forge does
  not invent one. Hosted FRONTENDS follow the same rule with the same field
  name (`forge.Frontend.image`), and forge appends the platform's own
  `static.v1` layout segment itself, so that segment never appears in your KCL.

  A local env states the image IT runs, because a k3d node can only pull from a
  host-local registry. That is a plain KCL string on the workload — the
  scaffolded dev env declares one `_LOCAL_IMAGE` constant and its `_on_k3d`
  binder sets it. forge ships no helper for composing an image reference and
  prescribes no pattern: a project that wants one source of truth writes its
  own constant, which is what KCL is for.

  **`forge registry login <env>` logs in to every distinct host the env's images
  name** — no host argument and no flag that takes one. The credential is the
  only input: `--username` with `--password-stdin`, or `--password-env` naming
  the variable that holds it (the `forge.ControlPlane.token_env` convention, so
  CI states a NAME in git and never the value). `forge registry ref <env>` now
  prints one `<image>@<digest>` per built image, labelled by the workload that
  declared it.

  **RELEASES MUST BE RE-CUT.** The release ledger now keys an OCI artifact by
  its declared repository, host included, so the key IS the address and
  `release.Artifact.URI` is no longer recorded for images. Ledgers cut before
  this change are keyed by bare name, which no longer resolves to an address:
  `forge release verify` reports such an entry as UNVERIFIABLE rather than
  guessing Docker Hub. Re-cut and re-promote any release you still rely on.
  Keying by bare name could not survive per-workload registries — two envs
  building the same app to two registries collapsed onto one entry, so a prod
  promotion could read a digest that only ever existed in the dev registry.

  Also removed: `BuildState.Registry` and `buildtarget.State.Registry` (Image
  now holds the full repository), `forge env new`'s `REPLACE_ME_REGISTRY` knob
  (an env has no registry to get wrong), and the `registry` field the render
  projected onto each cluster runtime.

- **BREAKING: a `forge.ShellBuild` `cmd` is plain KCL, run verbatim — the
  `${TOKEN}` substitution is gone.** Forge used to rewrite a fixed vocabulary
  into the command before running it — `${IMAGE}`, `${TAG}`, `${CODE_VERSION}`,
  `${SERVICE}`, `${TARGETARCH}`, `${REGISTRY}`, `${PROJECT_DIR}`, `${ENV}` and
  `${BUILD_CWD}`. It no
  longer substitutes anything and exports no variables of its own: the rendered
  string is handed to `sh -c` byte-for-byte, from the declared `cwd`, with the
  declared `env` merged onto the process environment. Every `$VAR` in a command
  is now the shell's, so `$PWD` and `$HOME` work.

  Write the values in KCL, where the command is composed and the value already
  lives: `forge.target_arch()` (new, and the reason `target_arch` is now a
  reserved render input), `forge.image_tag("<env>")`, `forge.env()`,
  `file.workdir()`, the image reference itself, or a literal. To keep a
  `${NAME}` spelling, declare `NAME` in that build's `env` map and the shell
  resolves it.

  `forge lint` (`shellbuild-tokens`) and `forge generate` both REFUSE a leftover
  token. That is a gate rather than a warning because one of them fails
  silently: `GOARCH=${TARGETARCH}` reaching the shell unset becomes `GOARCH=`,
  which Go reads as "unset" and builds for the host — so an amd64 cluster gets
  an arm64 binary whose only symptom is an `exec format error` crash-loop. The
  rest (`docker push /:`) fail loudly.

  Forge now resolves the build's tag BEFORE the render and binds it as the
  existing `image_tag` input, so a rendered `cmd` already names the tag the build
  records and pushes (`--release` > `--tag` > the image's own pin > the env's
  `image_tag` > `git describe`, unchanged). Two casualties of the old pass are
  also gone: `forge project audit`'s `conflict_tokens` / `conflict_count` (with no
  built-in tokens there is nothing for a declared env key to collide with) and
  `forge doctor`'s substituted-command preview, which now prints the exact string
  forge will run instead of one built from `<registry>`/`<tag>` stand-ins.

  Escaping, worth knowing before you write one: braced-dollar is KCL's own
  interpolation, so backslash-escape a shell `${HOME}` or use a raw `r"..."`
  string. A DOUBLED dollar is not an escape — KCL reads it as an undefined name
  and refuses to compile.

- **BREAKING: every rollback surface is gone — recovery is roll forward.**
  `forge env deploy --rollback`, `forge env promote --rollback`, the KCL
  `forge.External.rollback_cmd` field, and every provider's rollback path
  (`kubectl rollout undo`, compose override pinning, the static-site
  re-sync, the hosted `DeployService/Rollback` call) are removed, along with
  the deploy's skip-the-migration-on-a-rollback machinery. A rollback claims to
  undo a release it cannot undo: the release already ran its migrations and
  wrote data. Fix forward instead — cut a release and promote it. Binding an
  env to an OLDER release is still an ordinary `forge env promote`; the plan
  labels it `direction BEHIND` and warns that it undoes nothing. Existing
  ledgers keep working: a `"kind":"rollback"` entry reads as the promote of
  its release. Delete `rollback_cmd` from any `forge.External` block.

### Added

- **Scaffolded CI for hosted environments** (ADR `docs/adr/env-verbs.md`,
  task V6). A project with at least one hosted env (an env whose KCL declares
  `forge.ControlPlane` and something the platform runs) now gets two new
  files. The first is `.github/workflows/release.yml`. On a `v*` tag it runs
  checks once (`forge lint --gate-json`, plus `task test -- -json` fed to
  `forge ci verify-test-run --gate-json`). It then builds, pushes and records
  the release in ONE command (`forge env build <env> --release "$VERSION"`;
  `--release` implies `--push`). Finally it deploys the release through the
  hosted envs in promotion order, one job per env, each under its GitHub
  Environment. Each stage after the first deploys `--from` the stage before
  it, pinned with `--from-promotion`. It asserts `--expect-current`, which the
  previous job captured before this stage's approval wait. A hotfix that lands
  while an approval is pending therefore turns the run red (exit 3) instead of
  being overwritten. The second file is
  `.github/actions/forge-deploy/action.yml`, a composite action vendored into
  the repo so that it versions with the project's forge pin. It is one
  `forge env deploy` — which records, applies and waits for health itself — and
  then records that outcome as rollout evidence on the promotion the deploy
  wrote and summarizes it into the job summary. Because the verb decides who
  applies from what the env DECLARES, the action carries no promote step, no
  "does this env converge its own promotions" probe, no separate wait and no
  `deploy:` input.
  Existing projects get both via
  `forge project rescaffold .github/workflows/release.yml`.
  Neither file uses curl or environment ids, and no token appears in a URL.
  The only secret is `FORGE_CONTROL_PLANE_TOKEN`.
- **`forge cloud token create|list|revoke`** mints and manages the org
  automation token that CI deploys with. An org token has no acting user, so
  it outlives any one person. Scopes are validated before the round trip.
  `--json` carries the one-time secret, so
  `… --json | jq -r .secret | gh secret set FORGE_CONTROL_PLANE_TOKEN` never
  echoes it to a terminal.
- **`forge ci summarize <doc.json>...`** renders forge `--json` documents as
  Markdown for `$GITHUB_STEP_SUMMARY`. It renders the verdict from the
  envelope, a field table and row tables. It decides nothing: it exits 0
  whatever the documents say, and 1 only when an input cannot be read.
- **`forge env deploy` applies a MIXED env's cluster half.** An env that keeps
  its ledger on a control plane AND declares workloads that control plane does
  not run — a cluster Deployment, a compose service, host infra, a shipped
  frontend — now gets both halves of the deploy: the client-side apply for what
  forge owns, then the wait on the rollout the control plane computes. Before,
  the follow-through branched on "is this env hosted" alone, so a mixed env
  recorded its promotion and waited on the hosted rollout while its cluster
  workloads silently stayed on the previous release — a green deploy that
  shipped half a release. The apply runs first and a failure short-circuits the
  wait. Nothing is configured: the shape comes from the env's own render.
- **Status verbs for hosted deploys** (hosted-deploy-primitives §3.5):
  - `forge env rollout <env>`: where the current (or `--promotion`)
    rollout has got to, read once. It is `forge env wait --timeout 0`, with
    the same exit codes (0 succeeded, 1 degraded, 2 undetermined, 5 still
    rolling out, 6 superseded).
  - `forge env history <env> [--limit] [--before] [--release]`: the
    promotion ledger, newest first, with from-env, actor, note, evidence
    counts and the CI run. It pages with a keyset cursor (`next_before`;
    empty means the last page), and both ledger backends page identically.
  - `forge release where <version>`: which environments are currently bound
    to a release. Exit 1 when none are.
  - **`forge env verify` works for hosted envs.** forge cannot read a hosted
    env's cluster, so it reads the control plane's observer through
    GetRollout and reports the same five states with
    `source: control-plane observer`. A stale observation is UNREACHABLE
    (exit 2), never MATCH. Cluster envs are unchanged.
  - `forge env topology --json` gains `promotion_id`, `from_env`,
    `gates_summary` and, for hosted envs, `rollout_phase`.
- **`forge ci run <run-id> [--env <env>] [--json]`**: one CI run's timeline
  from the hosted control plane. It shows the release cut, the recorded
  checks, every promotion and each promotion's rollout, with a verdict.
  Exit codes: 0 every stage passed, 1 a stage failed or errored, 5 still
  running, 2 could not read the control plane. A run id nothing has written
  under exits 5, never 0. `--env` picks which control plane to ask;
  without it, the project's single declared control plane is used. The
  design spec named this `forge run show`; that would have collided with the
  dev-server `forge run`, which takes its own positional arguments.
- **`forge env verify` refuses to judge against a stale file ledger.** A
  self-managed env's promotions live in `.forge/promotions/<env>.jsonl`,
  committed to git, so a checkout that has not pulled the latest release
  record compares the cluster against an OLDER promotion. A fine deploy then
  reads as DRIFT, and a deploy that never happened can read as MATCH. verify
  now compares the env's log with origin's default branch (as of the last
  fetch; it never fetches). When this copy is `behind` or `diverged`, every
  image is still reported, but the command exits **2** (could not determine)
  with the fix. `ahead` (recorded here, not yet merged) is shown, not failed.
  `--json` carries `ledger: {state, ref, local_entries, upstream_entries,
detail}`. A control-plane ledger has no copy to be behind and is unaffected.
- **`forge env promote` always compare-and-sets.** The write asserts that the
  env is still on the promotion the plan read, and is refused if someone else
  moved it since. A hotfix that lands while a pipeline waits for approval now
  turns that pipeline red (exit 3, `promotion_conflict`) instead of being
  overwritten. No flag is needed. `--expect-current <id>` replaces the planned
  value with one captured earlier; `--expect-current unbound` /
  `--expect-unbound` asserts the env was never promoted. Re-promoting the
  release an env already runs is still a no-op whatever the expectation says,
  so a retried success is never a conflict. `--supersede` admits a promote
  while the current rollout is in flight and is recorded on the new entry.
  Exit 4 is `rollout_in_flight` / `environment_pinned`. `--plan --json` now
  shows `current.promotion_id` (the value to capture) and `expected`; a
  refused `--json` document carries `applied: false` and
  `refusal: {reason, expected…, actual_current, actual_phase}`, and its
  `ok`/`exit_code` match the process status. The file ledger applies the same
  check against the history it reads. Hosted CAS needs a control plane with
  the P0 contract — an older one ignores the fields.
- `forge env promote --run-id / --run-url / --no-run`: the promotion records
  the CI run, defaulted from `GITHUB_*` / `CI_PIPELINE_*`. `--wait`,
  `--deploy`, `--timeout`, `--fail-fast`, `--gate`, `--from` and
  `--from-promotion` are declared and refuse with "not supported by this
  forge build" until they are wired.
- `service_account_annotations: {str:str}` on `forge.K8sOverrides`,
  `forge.RenderedWorkload` and `forge.workloads.Workload`: annotations stamped
  on the ServiceAccount forge GENERATES for the workload, and on no other
  object. It is how a workload gets cloud workload identity (GKE
  `iam.gke.io/gcp-service-account`, EKS `eks.amazonaws.com/role-arn`) without
  renaming its identity — the SA keeps the workload's name, so Roles, bindings
  and policy that name it keep working. Keys are validated as Kubernetes
  annotation keys at render time, and combining it with `service_account`
  (where forge generates no SA) is refused rather than silently dropped.
- `forge env render` now includes every declared `forge.HelmChart`'s objects.
  They used to be missing entirely (flux, cert-manager and envoy-gateway each
  rendered 0 objects while `env deploy` applied them), so render was not a
  preview of deploy. Charts are templated through the deploy's own chart
  render (`helm template` with the declared values, the pinned CRD bundle,
  the chart's own CRDs, the post-hook drop), each document is marked
  `# source: helm chart <name>` and attributed to the cluster deploy applies it
  to, and `--target` selects charts the same way deploy does. Templating needs
  `helm` and registry access; `--no-charts` skips it, and the summary names the
  charts it left out.
- `cluster:manage` access-token scope (`accesstoken.ScopeClusterManage`):
  registers, rotates and removes an org's BYO clusters. Its own authority —
  `deploy:write` neither implies it nor is implied by it.

### Changed

- **Scaffolded CI: the build-once release pipeline owns hosted envs, and the
  broken pieces are gone.**
  - `deploy.yml` no longer rebuilds per env for a hosted env. If it did,
    staging and prod would run different digests, and `release.yml` builds
    once instead. A mixed env, one with both hosted and cluster parts, also
    moves wholly to `release.yml`. Its ledger is the control plane, so once
    it has been promoted, `forge env deploy` pins both halves to the
    release. A `deploy.yml` rebuild of its cluster half would then deploy
    the old digests and still report success. A mixed stage writes the
    kubeconfig, because `forge env deploy` applies that half from CI itself.
    When every env is hosted, `deploy.yml` is not emitted at all.
  - `build-images.yml`'s opt-in `cut-release` job is deleted. It cut and
    promoted by raw curl, read `DEPLOY_TOKEN` instead of
    `FORGE_CONTROL_PLANE_TOKEN`, and was broken against the server because
    it sent no artifact kind or mode.
  - `reconcile.yml` runs `forge env status <env> --json`. It used to call
    `forge reconcile`, which was never a verb, and its matrix expressions
    were missing their `$`.
  - Every rendered workflow is now checked by `actionlint`, when it is on
    PATH.
- **BREAKING (one require line): `github.com/reliant-labs/forge/pkg` is no
  longer a module.** forge is now a single module,
  `github.com/reliant-labs/forge`, carrying both the CLI and the `pkg/*`
  runtime libraries. **Import paths did not change** — a module at
  `github.com/reliant-labs/forge` with a `pkg/testkit` directory serves
  `github.com/reliant-labs/forge/pkg/testkit`, byte-identical to what the
  submodule served. Consumers change one require line and no source:

  ```sh
  go get github.com/reliant-labs/forge@vX.Y.Z
  go mod edit -droprequire=github.com/reliant-labs/forge/pkg
  go mod tidy
  ```

  Dropping the retired requirement is not optional, and it does not degrade
  gracefully: both modules can serve `forge/pkg/*` import paths, so a graph
  holding both answers every such import with `ambiguous import: found package
... in multiple modules`. `forge generate` now detects this before codegen
  and distinguishes a requirement you own from one a dependency drags in.
  A dependency's must be fixed in that dependency — a `replace` only hides it.

  The two modules were always released in lockstep, at the same commit, with
  the same version. Keeping that true took three hand-maintained syncs, and
  two failed in production: the root module's `require forge/pkg` went stale
  and made `go install .../cmd/forge@main` uninstallable, and a hand-listed
  registry of "forge/pkg symbols the generator emits" went stale when
  `testkit.StubNotConfigured` was added, so `forge generate` rewrote a project
  tree and then failed its own validate. One module makes both
  unrepresentable.

- **A dev build no longer pins a version it is not.** The scaffold's forge
  requirement comes from the binary's own build info: a release tag, or the
  pseudo-version `go install ...@main` records. A local or dirty build — whose
  bytes no module proxy can serve — now pins **nothing** and requires a
  `go.work` source bridge, which `forge generate` names in its refusal. It
  used to fall back to a hand-maintained "last published tag" constant,
  telling projects to require a release that could not satisfy the code being
  generated.

- **`forge generate`'s compatibility gate is a version comparison.** The
  project's forge must be `>=` the generating binary's. This replaces a probe
  that compiled a throwaway program against a hand-listed symbol set; the
  inequality covers every symbol ever added, cannot rot, and costs one
  `go list` instead of a `go build`. A project resolving a NEWER forge still
  passes — that is the ordinary upgrade order.

- **`task release:forge` is one tag, one commit.** The two-tag atomic push, the
  `go.sum` bare-clone resolution dance, and the `defaultPublishedForgePkgVersion`
  bump all existed only because the root module required an unpushed submodule
  version. `scripts/release-pkg.sh` and `task release:pkg` are removed.

### Fixed

- **The host disk reserve no longer refuses builds on a CI runner.** v0.1.43's
  admission check refused every build lane below a fixed 20 GiB of free disk,
  and a stock GitHub runner starts with ~14 GiB — so `forge env build` failed
  on a runner before compiling anything, with a remedy (`forge storage gc`)
  that reclaims caches a fresh runner does not have:

  ```
  host disk /tmp/... has 2.3 GiB free, below the 20 GiB reserve; run
  'forge storage status' and 'forge storage gc --apply' before building
  ```

  On a CI runner (`CI` / `GITHUB_ACTIONS`) a measured shortfall is now a
  warning and the build proceeds; an unreadable disk or unloadable policy
  still refuses. `FORGE_STORAGE_ENFORCE_RESERVE=1` keeps the refusal for a
  persistent self-hosted runner. Developer machines are unchanged.

- **A store-backed Secret now reaches EVERY cluster that consumes it.** A
  `FileSecrets` (or pulled local hosted) provider projects the Secrets its
  cluster workloads declare by `secret_ref` — but forge applied that
  projection once, into the env's primary kubectl context. A multi-cluster
  dev env whose second cluster also reads the Secret got nothing there, so the
  pod stalled on CreateContainerConfigError. control-plane worked around it by
  declaring the Secret a `forge.ExternalSecret` (which then blocked every
  fresh worktree's deploy preflight) and creating + mirroring it with a shell
  script, whose randomly generated HMACs then disagreed with the store values
  forge projected into the first cluster. The projection is now placed per
  consuming (cluster, namespace), with exactly the keys that cluster's
  workloads declare — the same trust boundary `RenderedSecrets` already keeps.
  A single-cluster env renders and applies exactly what it did before.

- **The dev databases exist before the first cluster workload is applied.**
  `ensureDevDatabase` already creates every declared dev DSN — including one
  an in-cluster workload reaches through `host.k3d.internal` — but it ran in
  `forge env up`'s host phase, which starts only after the cluster rollout
  converges. A cluster workload dialing a per-worktree database therefore
  crash-looped on `database "…" does not exist`, the rollout never converged,
  and the step that would have created the database never ran. The deploy
  dispatch now ensures the dev databases at the infra→cluster boundary — after
  compose / host-infra have brought the server up, before any cluster group is
  applied — so `forge env up dev` and an applying `forge env deploy dev` both
  start a fresh stack with no manual `CREATE DATABASE`. Dry runs create
  nothing; non-dev envs are untouched (the same fail-closed dev classifier).

- **`forge env up` from a linked git worktree no longer takes over and
  recreates compose infrastructure another checkout is running.** Compose names
  a project after its directory's basename and resolves relative bind mounts
  against that directory. Two worktrees laid out as `<container>/<repo>` share
  one basename, so they selected the SAME project with DIFFERENT mount sources:
  compose saw a changed config and recreated every shared container, which
  dropped every other stack's connections and left the containers mounting
  files inside that worktree.

  - New `forge.OnCompose {shared = True}`: the stack is machine infrastructure
    every worktree uses, so forge drives it from the repo's PRIMARY checkout
    (`--project-directory`, with `file` and `env_file` resolved there) from
    whichever worktree runs. An `up` from any checkout is then a no-op once the
    stack is running. A stack a worktree had already taken over is converged
    back to the primary on the next run, with one recreate.
  - New `fp.write_file(path, content, shared=True)`: writes into the primary
    checkout, so every stack's render updates the ONE copy of a file the
    shared stack mounts, instead of N diverging per-checkout copies.
  - Every compose deploy now checks container ownership first. A non-shared
    deploy whose containers carry another LIVE checkout's
    `com.docker.compose.project.working_dir` is refused before pull/up, with
    both fixes named. Containers from a checkout that no longer exists are
    adopted.

- **A cert-manager `Certificate` in the bundle now SUPPLIES the Secret it
  materialises, so the deploy preflight stops false-failing on it.** The
  render-time secret back-propagation gate recognised only `kind: Secret`
  documents as in-stream supply, so a workload mounting a Secret that a
  `Certificate` in the SAME bundle creates was reported as an undeclared
  mount. `forge env deploy e2e` refused to run on the vendored
  cloudnative-pg barman-cloud plugin, whose Certificates and the Deployment
  mounting `barman-cloud-{client,server}-tls` render in one stream, and the
  only way past it was `--skip-preflight` — which disables the image, CRD and
  live-Secret checks too.

  cert-manager writes the Secret when it reconciles the Certificate, in the
  same apply pass that creates the pods mounting it, exactly as
  `kubectl apply` orders a rendered `kind: Secret` ahead of its consumers.
  Demanding it exist beforehand made a first deploy of any cert-manager-backed workload
  unsatisfiable by construction: the thing that provisions the Secret was the
  very deploy being blocked. The advice the gate printed — declare a
  `KubeconfigSecret` or an `ExternalSecret` — was actively wrong for a
  cert-manager Secret, whose bytes cert-manager owns and would overwrite.

  Matching is on the `cert-manager.io` API group (any version), so a bundle
  on `v1alpha2` or a future `v2` is recognised without a forge change, and an
  unrelated CRD that happens to be named `Certificate` is not. A Certificate
  supplies only the one name in its `spec.secretName`; every other undeclared
  mount in the same bundle still fails.

- **The frontend bundle and image tests run in the e2e lane, on a minimal
  fixture.** The `next build` peer-pin test scaffolded a whole frontend
  (OpenTelemetry web instrumentation included) inside the unit lane's
  `go test -race ./...`, where the 7GB runner OOM-killed it (`signal: killed`
  after up to 1029s) and failed the Test job on unrelated PRs. It and the
  static-image `docker build` test now live in `internal/cli` under
  `-tags e2e`, on the e2e shards' own runners, where a missing tool is a
  failure rather than a skip. The pin test installs only what its claim
  needs (33 packages; ~35s locally for both of its builds) and still proves
  both halves: directory pins fail with `No QueryClient set`, and forge's
  post-install reconcile makes the build prerender. Each node process is
  capped at `--max-old-space-size=2048`. The e2e shard now verifies a docker
  daemon answers before the suite runs.
- **`forge tools install` never mutates a frontend's `package.json` or
  lockfile.** It used to run `npm install --save-dev @bufbuild/protoc-gen-es`
  in every frontend under `--force` — which the scaffolded verify-generated
  job passes — and in any frontend without `node_modules` yet. On a Linux
  runner npm re-saved a macOS-written `package-lock.json` without its `libc`
  entries, and `forge ci verify-generated` reported that user-owned file as
  generated-code drift; committing either OS's lockfile moved the failure to
  the other OS. The plugin is now installed only by the frontend's own
  `npm ci`, from its declared devDependency. `forge tools install` checks
  that every frontend whose buf.gen.yaml runs the local plugin declares it,
  and fails with the edit to make when one does not. `--force` reinstalls the
  Go tools only.
- **`forge ci verify-generated` no longer blames `forge generate` for a file
  an earlier step changed.** It refuses to regenerate over an
  already-modified tree and lists those paths as changed before
  `forge generate` ran, instead of reporting them as out-of-date generated
  code.
- **A Next.js frontend's tsconfig peer pins no longer split packages in the
  bundle.** The pins named package DIRECTORIES, and Next's webpack resolver
  applies tsconfig `paths` to app code: wherever a pin resolved (an ordinary
  `npm ci` in the frontend, or a Docker build at `/app`), app code bundled
  `@tanstack/react-query`'s `build/legacy` while
  `@reliantlabs/forge-web-runtime` bundled `build/modern`. Two module instances
  are two React contexts, and prerender failed with `No QueryClient set, use
QueryClientProvider to set one`. Every pin now names the package's
  declaration file (e.g. `…/@tanstack/react-query/build/modern/index.d.ts`,
  `react` → `@types/react/index.d.ts`), derived from the INSTALLED manifest,
  never hardcoded, because the entry moves inside the declared ranges
  (`@opentelemetry/sdk-trace-base` 2.0 → 2.11). Next's resolver skips a `.d.ts`
  target and Vite ignores `paths`, so bundles resolve through `exports`, while
  tsc still binds one copy of the types — which the pins remain necessary for.
  `forge project new` and `forge scaffold frontend` write them after their
  install; `forge generate` heals an existing project's directory pins
  wherever they resolve, once, and never rewrites a declaration pin.
- **The Next.js frontend Dockerfile follows `frontends[].output`.** It always
  copied `.next-prod/standalone`, which only `output: standalone` produces, so
  a `static` frontend's image (and a `server` one's) failed with
  `COPY .next-prod/standalone: not found` — and CI builds an image for every
  frontend. `static` now serves `out/` from `nginxinc/nginx-unprivileged`
  (uid 101, port 8080, mounted under `base_path`), `server` runs `next start`,
  and `standalone` keeps the node server. The build runs at the frontend's
  repository path (`/src/frontends/<name>`) instead of `/app`, installs with
  `npm ci`, and the node runners drop the bundled npm/corepack trees, which
  carried every HIGH CVE in `node:22-alpine`. The scaffold ships a
  frontend-level `.dockerignore`, since the build context is the frontend
  directory. The Dockerfile is scaffold-once: `forge project upgrade --check`
  reports the new one to existing projects.

### Removed

- `database.migration_safety.down_files_allowed_until` and the
  `forge project upgrade` step that stamped it. Every down migration is now a
  `no-down-migration` lint error, with no grandfathered history: forge never
  runs one, so deleting it is always safe. A forge.yaml still carrying the key
  loads with a warning naming the fix.
- `go.work` from forge's own repo. It existed to stitch `pkg` to the root
  module, and it was also what hid the stale-require bug: every in-repo build
  resolved `./pkg` locally and stayed green while `@main` was uninstallable.
- `buildinfo.PkgVersion`/`SetPkgVersion`, `PkgModuleVersion` (already dead),
  `Build.PkgPath` and the `forge/pkg` line in `forge version` — a second
  version axis with nothing left to describe.
- `docs/pkg-versioning.md`, replaced by `docs/versioning.md`.

## [0.1.11] - 2026-09-01

Patch release. The root CLI (`v0.1.11`) and the runtime library
(`pkg/v0.1.11`) are tagged as always — on adjacent commits, `pkg` on the
release commit and the root tag one commit later on the require bump.

This release bounds the parallel-dev-stack port allocator, teaches the
block registry what kind of key it is holding, and repairs two k3d
failures that presented as something other than what they were.

### Added

- **`dev_stack:` in `forge.yaml`** — `max_stacks` (default 8: the primary
  checkout plus seven worktrees) and `block_size` (default 100, the
  historical hardcoded quantum). The parallel-stack ceiling was
  previously stated twice and tied together nowhere: the allocator was
  unbounded while a project pre-maps host ports at cluster-CREATE time,
  so a worktree past the last mapped block allocated cleanly and then
  failed later as "gateway unreachable". `AllocateBlock` now refuses a
  NEW block past the ceiling with the remedy in the error. An EXISTING
  entry always resolves regardless — an issued block is baked into k3d
  mappings and the dev IdP's `iss` claim and redirect URIs, so lowering
  the ceiling must never move a port already handed out.
- **`forge env devstack prune`** reclaims blocks whose git worktree no
  longer exists, which is what keeps the now-finite block range from
  filling with leaked entries. Dry-run by default. It never touches a
  plain port-block key or the default key, and if enumerating live
  worktrees fails for any reason it reclaims nothing.
- **`fp.dev_stacks()` KCL builtin** — the roster of registered dev-stack
  keys, so a module generating one config block per running stack never
  parses `.forge/blocks.json` itself. It returns EMPTY on a read-only
  render, which keeps `forge generate` a pure function of committed
  inputs; otherwise a generated tracked file would capture whichever
  worktrees happened to exist on that machine.
- **`forgeconv-allocate-port-spacing`** — two `allocate_port` base
  literals congruent modulo `block_size` collide at the block equal to
  their separation. The rule fires only when that block is reachable at
  the project's configured `max_stacks`, which keeps it actionable
  rather than flagging every latent pair.

### Fixed

- **Rendering or deploying a cloud env no longer claims a local port
  block.** `forge env render` / `forge env deploy` armed the persistent
  `fp.allocate_port` block registry for every env, so rendering prod from a
  linked worktree registered a new `prod-<worktree>` block in the primary
  checkout's `.forge/blocks.json`. That leaked one permanent block per
  throwaway worktree, went unseen by `--fail-on-write` because the registry
  sits outside the worktree, and failed the render outright once the registry
  reached `dev_stack.max_stacks`. These commands now claim a block only when
  the env's declaration runs on this machine: a k3d cluster, a host, compose
  or host-infra service, or a workload on a local context. Otherwise
  `allocate_port` returns its base port and nothing is written. `forge env up`
  is unchanged, including `forge env up prod --target <frontend>`.
- **A stale k3d serverlb upstream no longer reads as a flaky cluster.**
  k3d's serverlb is nginx configured with the server node's NAME and no
  `resolver` directive, so it resolves each upstream once at startup and
  caches it for the process lifetime. When the node later gets a
  different Docker IP, nginx keeps proxying to the cached address; with
  `max_fails=1 fail_timeout=10s` the symptom is not a clean outage but
  ~10-second "connection refused" windows that recover on their own.
  Forge now compares container start times and refreshes the load
  balancer before the API probe and after `k3d cluster start`.
- **A declared `api_port` is no longer dropped** for clusters that also
  name a k3d config file. `k3d cluster create --config` accepts no
  per-flag settings, so the value parsed, validated, and then did
  nothing while k3d picked a random host port. Where the config file
  already sets `kubeAPI.hostPort`, the file wins if they agree and forge
  refuses if they disagree.
- **The cluster health probe reports why it failed.** It previously
  collapsed every distinguishable failure into a bare false, after which
  forge printed "the API is still starting" — a diagnosis it had never
  established.
- **A node sampled moments after restart is no longer misreported as
  dead.** k3d's entrypoint runs its own setup before it execs k3s, so
  "no k3s process" must now persist for a grace period before counting
  as exited.
- **The block registry distinguishes a dev-stack key from a plain
  port-block key.** A bare `{key: block}` map could not, so a generator
  reading it raw emitted a dev NATS account for a prod web port into a
  tracked config file. The legacy bare-int form is still accepted.
- **A frontend `npm install` is retried once during
  `forge project new`.** A single transient registry failure previously
  degraded into a scaffold with no `node_modules`, discovered much later
  as some other tool reporting a package it could not find.

## [0.1.10] - 2026-08-28

Patch release. The root CLI (`v0.1.10`) and the runtime library
(`pkg/v0.1.10`) are tagged as always — on adjacent commits, `pkg` on the
release commit and the root tag one commit later on the require bump.

v0.1.9 made a deploy fail when a workload never became ready. This
release fixes the two ways that new strictness, and `forge generate`,
could report a failure that had not happened.

### Fixed

- **A successful deploy is no longer failed by waiting on one-shot Jobs
  that were never applied.** forge names a one-shot Job by its spec hash,
  but the rollout wait unioned the manifest-derived (hashed) names with a
  caller-supplied list that derived the UNHASHED entity names. Those names
  matched no object in the namespace, `kubectl wait` errored on each, and
  v0.1.9's policy — correctly, for a Job that genuinely did not run —
  turned that into a failed deploy. A real prod deploy applied 201
  objects, rolled all 13 Deployments, completed both one-shot Jobs, and
  was still reported as failed. The wait set is now every `kind: Job` in
  the stream the apply just sent and nothing else: authoritative by
  construction, already filtered by `--target` and scoped to the right
  cluster. A Job that was applied and then FAILED still fails the deploy,
  and a Job that failed to APPLY is still caught by the kind-agnostic
  apply-completeness check.

- **`forge generate` no longer leaks machine-local state into tracked
  files, and `forge lint` no longer fights the generator over `gen/`.**
  Evaluating a KCL module fires every `file.write` in it, bypassing
  forge's write chokepoint, so a generate on a clean checkout could emit
  bytes derived from a gitignored registry — two developers on the same
  commit produced different output. Generate-path renders now revert any
  tracked file that went clean → dirty across them.

## [0.1.9] - 2026-08-28

Patch release. The root CLI (`v0.1.9`) and the runtime library
(`pkg/v0.1.9`) are tagged as always — on adjacent commits, `pkg` on the
release commit and the root tag one commit later on the require bump.

The theme is deploys that report the truth. `forge env deploy` could
previously exit 0 over an environment that never came up, in two
independent ways: a workload that never became ready was a warning, and
a kubectl apply that confirmed fewer objects than were rendered went
unnoticed entirely.

### Added

- **`RolloutPolicy`: a deploy now fails when a workload never becomes
  ready.** `forge env deploy` printed a warning and exited 0 when a
  Deployment or a one-shot Job failed to roll out, so a prod deploy
  reported success over a broken environment — the failure surfaced
  later, somewhere else, looking like a different bug. Readiness is now
  part of what "deployed" means.

  Four flags govern it: `--rollout wait|warn|skip` (the previous
  exit-0-on-failure behavior is still reachable as `warn`, and `skip`
  opts out), `--rollout-timeout` (default raised from 60s to 5m, since
  60s failed honest-but-slow rollouts), `--rollout-fail-fast`, and
  `--rollout-order`.

  A failed Job now fails the deploy rather than being ignored, and it
  fails as soon as Kubernetes says so: both the `complete` and the
  `failed` conditions are watched, so a Job that has already failed no
  longer burns the full timeout before reporting it.

- **`forge env up` resolves GitSource frontends.** Only build and deploy
  did, so a frontend declared with a cross-repo `source:` dev-served by
  running npm in the project root — against the wrong tree, or against
  no frontend at all. `up` now resolves the source like its siblings.

### Fixed

- **A deploy fails when kubectl confirms fewer objects than were
  rendered.** The apply step trusted a zero exit code and never compared
  what came back against what went in. In the v1.5.6 incident a prod
  deploy applied 103 of 105 rendered objects and reported success; the
  two missing objects were found by hand, much later. The count is now
  checked, and a short apply is a failed deploy.

### Changed

- The toolchain moves to go 1.27, and the govulncheck pin rises to
  v1.7.0. These are one change, not two: v1.1.4 vendors an `x/tools`
  that panics under 1.27, so the old pin cannot scan the new toolchain.
- CI caches each Go module separately, rather than treating the
  multi-module repo as one cache key.
- Scaffold guidance now tells the reader to FIX forge when forge is what
  stands in the way, instead of working around it. A workaround plus a
  TODO is the outcome that rule exists to prevent — it buries the signal
  that tells us what to fix.

## [0.1.8] - 2026-08-27

Patch release. The root CLI (`v0.1.8`) and the runtime library
(`pkg/v0.1.8`) are tagged together, as always.

Everything under [Unreleased] below ships in this release: `forge env
render`, the cluster-health and object-collision doctor checks, the
`forge.dev/env` ownership stamp on every rendered object, seedplan tuple
and union support, and the testreport package.

## [Unreleased]

### Added

- **`forge env render <env>` — read what an environment renders, without
  deploying it.** There was no supported way to ask "what objects does this
  environment own". `kcl run` fails on a real project (it needs forge's
  `kcl_plugin.forge` harness), `forge doctor` renders every environment and
  reports a count, and `forge env deploy --dry-run` is the deploy command:
  it resolves a kubectl context, runs the declared-cluster guard, and refuses
  outright when the recorded build is behind HEAD. So the answer got
  reconstructed by hand. One audit read 1,886 lines of KCL and inferred each
  workload's target lexically — HOST from the presence of a `host =` block,
  cluster from a `k8s = K8sOverrides{cluster = ...}` override a thousand
  lines further down. One misread there deletes the wrong object out of the
  wrong cluster.

  The command prints the manifests a deploy would apply as a `---`-separated
  YAML stream, each document preceded by a `# cluster:` comment naming where
  it lands — a comment, so the stream still pipes into `kubectl diff -f -`.
  Attribution matters because an environment renders ONE stream and may
  deploy it to several clusters (control-plane's dev: most workloads on
  k3d-control-plane, workspace-proxy on k3d-cp-daemon), and it is not
  modelled a second time here: each document is handed to
  `internal/cluster.ScopeManifestsToGroup`, the function that performs the
  routing at deploy time, once per cluster. A document replicated to every
  cluster (an unattributed Namespace) is printed once with both named, so
  the object count equals the render's own — `forge doctor`'s 341 across
  control-plane's five environments is 30 + 71 + 58 + 76 + 106. `--cluster`
  narrows to exactly one cluster's stream; `--list` gives the inventory view;
  `--kind` / `--name` / `--target` narrow further; a non-zero exit carries
  the KCL error, so it works as a CI gate.

  **It does not claim to be pure, because it cannot be.** KCL evaluates
  `file.write` during rendering, so a project whose deploy KCL generates a
  file writes it every time anything renders — control-plane's dev main.k
  calls `nats.write_conf`, and `forge doctor` has been rewriting
  `deploy/nats/nats.conf` on every run all along. forge has no hook to
  suppress a project's own writes: the write happens inside the embedded KCL
  runtime, which has no read-only mode, and imposing one from outside
  (rendering from a copy of the tree, sandboxing the process) is neither
  portable nor cheap. So the command reports instead of promising: it stats
  the project tree before and after and names every file that changed, and
  `--fail-on-write` turns that into a non-zero exit for callers that need
  the guarantee. What forge CAN suppress it does — its own resolve_port
  store write is reverted, content and mtime, so it never shows up in the
  report as forge blaming the render for forge.

- **`forge ci verify-test-run` — a green suite that ran almost nothing is now
  visible.** A Go suite that skips its way through exits 0 and reads as green.
  Measured on one real package, one environment variable apart: 9 pass / 124
  skip without `DATABASE_URL`, 103 pass / 1 skip with it — same exit code, 7%
  of the tests, and nothing in the pipeline had a word to say about it.

  The new command reads the run's own `go test -json` output — from a pipe or
  `--from a-file` — and reports the packages whose pass therefore proves
  nothing. It runs no tests: `forge test` was removed precisely because a
  second spelling of the project's suite reports on a different suite than the
  one the project runs, so this reads the record of the run that already
  happened. On a 176-package, 482 MB stream it takes 2.5 seconds.

  Two rules, because skips are legitimate and a gate that fires on every skip
  gets switched off: `zero-evidence` (every test in the package skipped — no
  sample-size floor, "none of them ran" is unambiguous at any size) and
  `mass-skip` (more than `--max-skip-ratio`, default 0.5, of at least
  `--min-tests`, default 5). Healthy packages are never listed. Across the 112
  tested packages of the reference project in its broken environment the pair
  reports five, and the ratios above the line are 1.00 / 0.95 / 0.80 / 0.60 /
  0.56 against a next-highest of 0.20 — the threshold sits in an empty band,
  not through the middle of a crowd.

  Genuinely-expected heavy skipping is declared once in forge.yaml under
  `ci.test_skips.allow`, with a REQUIRED `reason` — the same contract as
  `forge project disown <path> --reason`. A declaration that stops suppressing
  anything is reported as no longer needed rather than left to rot; one that
  matches no package in the run is silent, so a scoped `go test ./internal/x/`
  does not drown in irrelevant notices.

  Three states, not two: input carrying no `go test -json` events, or a stream
  that ends mid-run, is UNDETERMINED and exits non-zero — the command never
  reports a clean run it could not read, and `--warn-only` does not downgrade
  that. Failures in the stream also fail it, because
  `go test -json ./... | forge ci verify-test-run` in a shell without
  `set -o pipefail` reports only the last command's status.

### Changed

- **Scaffolded services run pprof by default.** `pprof_addr` shipped with
  no default, so a scaffolded binary declared `PPROF_ADDR` on the deploy
  side and started no listener — the env var was projected onto every
  workload and read by nobody, and `forge env status` reported
  `? pprof — could not resolve a pprof address` about it. A gateway then
  sat at ~1 GB of anonymous memory being OOMKilled with no way to ask the
  process WHAT it was holding: a cgroup's `memory.stat` says how much and
  never what, and the answer only exists while the process is still alive.

  The default is now `127.0.0.1:6060`, and loopback is what makes always-on
  safe: the listener exists in every environment and is routable from none
  of them. It stays on its own serverkit listener (never the public port,
  whose endpoints sit behind a k8s Service), and the scaffolded manifests
  give it no Service, route, or container port — reach it with
  `kubectl port-forward <pod> 6060:6060` and then
  `go tool pprof http://localhost:6060/debug/pprof/heap`. The scaffolded
  `docker-compose.yml` overrides it to `0.0.0.0:6060`, the one place a
  profile has to cross a container boundary (Alloy scrapes it for
  Pyroscope). `PPROF_ADDR` / `--pprof-addr` moves it; `""` switches it off.

- **A pprof bind failure can no longer take a service down.** `serverkit.Run`
  routed it through `FailurePolicy`, so with the default `FailProcess` a busy
  debug port terminated the process. That was defensible while pprof was
  opt-in; with it on by default the ordinary cause is banal — a second copy
  of the binary on the same dev box — and a debug surface that can kill the
  service is worse than no debug surface. Run now logs it and serves on,
  under either policy. `FailurePolicy` continues to govern supervised
  components (workers, operators) unchanged.

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
