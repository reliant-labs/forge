---
name: v0.1.47
description: Upgrade a project to forge v0.1.47. One section per change — (1) the interceptor chain's Deadline slot, ahead of Auth: every unary RPC is now bounded by a 30s server deadline by default and watched for slow calls, and `(forge.v1.method).timeout` is enforced through `middleware.ProcedureDeadlines`, which an owned chain builder passes as `Deadline: serverkit.Deadline{Procedures: middleware.ProcedureDeadlines}`. Use when cmd/<bin>/cmd/serve.go or internal/app/providers.go calls serverkit.NewChain and names no Deadline, or when a project hand-wraps a timeout or watchdog interceptor around its authenticator. (2) The composition graph upgrade path: `forge fix composition-graph` converts the legacy Infra / OpenInfra / NewComponents / lifecycle.go / ServeSpec composition and the per-service, per-worker and per-operator subcommands to providers, compose.go constructor methods and roots, and lists — writing nothing — every shape it cannot convert without deciding for you; ServeSpec knobs become reachability (control-plane's OperatorListener, NoKubernetesAPI and WorkerLeases are the worked example); deploy binds workloads to roots. Use when internal/app declares `type Infra struct` or `func NewComponents`, or a graph project moves to binary_config.
version: v0.1.47
detection: grep -qsE '^type Infra struct|^func NewComponents\(' internal/app/*.go && exit 0; for f in cmd/*/cmd/serve.go internal/app/providers.go; do test -f "$f" && grep -qs 'serverkit.NewChain' "$f" && ! grep -qsE 'Deadline:|ProcedureDeadlines' "$f" && exit 0; done; exit 1
---

# Upgrading to v0.1.47

Each section below is independent: apply the ones whose "Use when" matches.

## 1. The Deadline slot: a per-RPC deadline that bounds authentication

Use when `cmd/<bin>/cmd/serve.go` or `internal/app/providers.go` calls
`serverkit.NewChain` and its `ChainDeps` sets no `Deadline:`.

### What changed

`serverkit.ChainDeps` has a `Deadline` slot, between metrics and auth:

    trace → recovery → request-id → logging → tracing → metrics →
    deadline → auth → audit → rate-limit → extras → activity → stream-drain →
    protovalidate

It runs **before** Auth, so authenticating — a token-store query, an
introspection call — runs inside the RPC's budget, and a stalled credential
lookup ends as `DeadlineExceeded` instead of holding the request until the
client gives up. `Extras` run after Auth and cannot do that, which is why it
is a slot.

**The default applies without any edit.** A nil `Deadline` is
`serverkit.Deadline{}`:

- every unary RPC is bounded by `serverkit.DefaultUnaryDeadline` (30s);
- streams are unbounded;
- a unary RPC still running after `DefaultSlowAfter` (10s) is logged at WARN
  (`rpc still in flight`), again every `DefaultSlowRepeat` (30s), and once
  more when it completes (`rpc completed slowly`).

The server budget is a ceiling: a client's shorter deadline still applies; a
longer one no longer lifts it.

`(forge.v1.method).timeout` used to be documented and read by nothing. It is
now projected into `pkg/middleware/procedures_gen.go` as
`middleware.ProcedureDeadlines` on every `forge generate`.

### What to do

1. **Find unary RPCs that legitimately run longer than 30s** — exports,
   long provisioning calls, anything waiting on a person (an OAuth sign-in).
   Declare each one's budget in its proto, next to the rpc:

   ```protobuf
   rpc Export(ExportRequest) returns (ExportResponse) {
     option (forge.v1.method) = { timeout: { seconds: 120 } };
   }
   ```

   A negative timeout (`{ seconds: -1 }`) is no server deadline.

2. **Pass the projection to your chain.** `serve.go` / `providers.go` are
   yours, so forge will not make this edit:

   ```go
   chain, err := serverkit.NewChain(serverkit.ChainDeps{
       Auth:     ...,
       Logger:   logger,
       Deadline: serverkit.Deadline{Procedures: middleware.ProcedureDeadlines},
       ...
   })
   ```

3. **Delete any hand-rolled timeout or slow-call interceptor**, in particular
   one merged into the `Auth` slot so that it would wrap authentication. Its
   budgets go in the proto (step 1) or in `Deadline.Procedures`; its default
   in `Deadline.Unary`; its thresholds in `SlowAfter` / `SlowRepeat`; a
   metric it bumped in `Deadline.OnSlow`, which replaces the WARN log (log
   and count in the one func).

4. Run `forge generate`, then `go build ./...`.

To keep the old behaviour — no server deadline at all — set
`Deadline: serverkit.NoDeadline`. That leaves a wedged handler holding its
request until the client gives up, with nothing reporting it.

## 2. The composition graph: off Infra, NewComponents and ServeSpec

Use when `internal/app` declares `type Infra struct` or `func NewComponents`
(the legacy composition), or when a graph project moves to `binary_config`.

### What changed

A project composes through the composition graph
(`forge skill load architecture`, "The composition graph"): every part of a
process is a component built from its Deps, providers in `providers.go` replace
the eager `OpenInfra`, `compose.go` holds one constructor method per node, and a
**root** (`//forge:root <name>`) selects what a process builds — one command per
root, run by `runRoot` in `serve.go`. Each root's config is its reach
(`config.LoadReach`, KCL `ROOTS`), `forge lint --graph` proves reach and
audience, and the least-knowledge rules judge every component's Deps.

A project still on the legacy composition generates exactly as before, and
none of that applies to it. Nothing migrates it implicitly: run the codemod.

### Step 1 — the codemod

Commit first; the codemod replaces files you own.

```bash
forge fix composition-graph --dry-run   # what it would do, or what blocks it
forge fix composition-graph
forge generate && go build ./... && forge lint --graph
```

It is all or nothing. On the composition forge scaffolded and reconciled it:

- re-scaffolds `internal/app/providers.go` (Infra's stock fields become the
  graph's inputs and providers: `infra.Log` → `g.Log`, `infra.Cfg` → `g.Cfg`,
  `infra.DB` → `g.DB().DB`, `infra.ORM` → `g.ORM()`, `infra.DefaultClient()` →
  `g.HTTPClient()`) and `cmd/<bin>/cmd/serve.go` (as `runRoot`);
- turns each `NewComponents` construction into a `func (g *Graph) new<X>()`
  method in `compose.go`, keeping every Deps value you set (`c.Tasks` and a
  producer local become `g.Tasks()`); a compose.go holding only the
  conventional Logger/Config values is re-scaffolded instead;
- deletes `lifecycle.go`, `server.go`, `mounts_services_gen.go` and
  `cmd/<bin>/cmd/{services,workers,operators}/`; rewrites `root.go`
  (`root.AddCommand(rootCommands(deps)...)`, no group factories) and
  `main.go` (`cmd.Execute()`); retypes `APIKeyValidator(*Infra)` to `*DB`; and
  points the e2e harness at `server`.

The one root afterwards is the generated default, `server`: every service,
worker and operator — what `<bin> server` ran. The retired per-component
subcommands are listed; a workload in `deploy/kcl` that runs one is named
too.

Anything it cannot map without deciding for you is listed with its file:line,
and **nothing is written**. A conversion that compiles but changes what a
process serves — collapsing two deployment groups into one `server`, say — is
worse than none. Clear each item, then run it again.

### Step 2 — clearing what it lists

| It lists | What to do |
|---|---|
| `Infra.<F>` is infrastructure forge did not scaffold | Make it a provider in `providers.go`: `func New<F>(<F>Deps) (T, error)`, built from the `OpenInfra` lines that set it, with what those lines read as `<F>Deps` fields. A root that does not reach it never opens it. Give two values of one type distinct named types (`type ReadPool struct{ *sql.DB }`). |
| a declaration in `providers.go` / `serve.go` / `server.go` / a command group | Move it to a file of its own first (a provider if it opens something; a chain or edge option belongs in `NewUserChain` / `NewAPIListener`). |
| `OpenInfra` takes options; a `ServeSpec.<Knob>` | Reach — see the worked example below. |
| `NewComponents` wiring forge did not scaffold (sub-builders, locals, conditionals) | Write each component's `new<X>` method in `compose.go` by hand: one node per method, its Deps values naming other nodes (`g.Repo()`), the graph inputs (`g.Cfg`, `g.Log`, `g.Ctx`) or providers. A conditional value is the method's body (`if g.Cfg.X == "" { … }`). A true cycle is a `func() T` field (a lazy edge). |
| supervision forge did not scaffold (`lifecyclekit.Singleton`, a wrapper) | A component of its own with `Run` (and `Drain`/`Close`): `type LeasedReaper struct{ … }`, `func NewLeasedReaper(LeasedReaperDeps) (*LeasedReaper, error)`. The graph supervises it from its methods. |
| `RunOperators` sets `operatorkit.Options.<X>` | The generated controller manager does not take it from the project: run the operators under a root of their own with a hand-written manager node. |
| `<X> is retired by the graph` (wire files, tests, `cmd.Serve` callers) | Rewrite over the graph: a test builds the node it tests from its constructor, or a `Graph{…}` and its accessor; a command that called `Serve` with its own spec is a root. |
| a per-component subcommand with flags or setup | A root (`//forge:root <name> command=<name>`), with the setup in `runRoot` (branch on `root.Name`) or a provider. |

### Worked example: control-plane's ServeSpec knobs

control-plane's `cmd/control-plane/cmd/serve.go` adds three `ServeSpec`
fields, and four commands hand `Serve` a spec: `public-api`, `admin-api`,
`workers`, `workspace-controller`. No process serves both service groups.
Under the graph each command is a root, and each knob is whether that root
reaches the component it switched:

- **`WorkerLeases`** opened `Infra.Lease` only for `workers`. The lease
  connection becomes a provider (`func NewLease(LeaseDeps) (*leasekit.Locker,
  error)`); each singleton worker becomes a component taking it
  (`LeasedBillingEnforcement{Lease, Worker}`, `Run` = acquire, then run). Only
  the `workers` root reaches them, so only that process dials the lease.
- **`NoKubernetesAPI`** stopped `admin-api` constructing a Kubernetes client.
  The client becomes a provider (`NewK8sClient`); the components that read it
  take it in their Deps. The `admin-api` root reaches none of them, so it never
  builds one — and `TestServeProcessesProbeKubernetesOnlyWithAMountedToken`
  becomes a reach assertion over the generated roots.
- **`OperatorListener`** selected the fail-closed operator validator chain. That
  is a second trust domain: an `OperatorAuth` provider marked
  `//forge:audience operator`, an `OperatorChain`, and an `AdminListener` serving
  `OperatorRoutes` — every admin service declares
  `option (forge.v1.service) = {audience: "operator"}`. `forge lint --graph` and
  the listener's boot check then prove what `internal/app/deployments.go`
  asserts by hand today: no end-user listener reaches an admin rpc.

```go
// internal/app/roots.go — yours
//forge:root public-api
type PublicAPI struct{ API *APIListener }

//forge:root admin-api
type AdminAPI struct{ Admin *AdminListener }

//forge:root workers
type Workers struct {
	BillingEnforcement *LeasedBillingEnforcement
	// … one field per worker; field order is stop order
}

//forge:root workspace-controller
type WorkspaceController struct{ Operators *Operators }
```

The command names stay, so `deploy/kcl` keeps running the same args.

### Step 3 — deploy

`deploy/kcl` is untouched by the codemod and keeps running the same commands.
To give each workload exactly what its root reaches, bind it to the root and
project its env from the root's reach (this changes the rendered env, which is
the point — a worker no longer receives the API's secrets):

```kcl
api = fw.Workload {
    name = "api"
    build = forge.GoBuild {cmd = "./cmd/<bin>", output_name = "<bin>"}
} | config_gen.root_of("server")
# and in each env:
w | {env = config_gen.workloadEnvMap(appcfg.app_config, w) | w.env}
```

A graph project that moves to `binary_config` (per-command configs) needs three
edits by hand:

1. Each env file passes `appcfg.<bin>_process` where it passed
   `appcfg.app_config`.
2. `deploy/kcl/<env>/config.k` is re-emitted with
   `forge project rescaffold <path>` — deleting it records that you took
   ownership, so `forge generate` will not bring it back.
3. The scaffolded `_api` workload is rebound to a root that exists
   (`config_gen.root_of("<root>")`); rendering fails on an unknown root.
