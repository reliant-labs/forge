---
name: services
description: Scaffold and wire new services, packages, and frontends in a Forge project.
---

# Adding New Components to a Forge Project

Use this skill whenever you need to introduce a new network-facing service, internal package, or frontend into a Forge mono-repo. Every scaffolded service comes with production middleware baked in — structured logging, distributed tracing, auth, rate limiting, and graceful shutdown — so you focus on business logic from day one.

## Choosing the Right Command

| I need…                                      | Command                        | What it creates                          |
| -------------------------------------------- | ------------------------------ | ---------------------------------------- |
| A new network-facing API (Connect RPC)       | `forge scaffold service <name>`     | Proto definition, generated stubs, Go service skeleton |
| A background worker                          | `forge scaffold worker <name>`      | Worker with Start/Stop lifecycle |
| A cron-scheduled worker                      | `forge scaffold worker <name> --kind cron --schedule "..."` | Worker with cron scheduler |
| A one-shot step that must finish before something else starts | declare a `kind = "job"` workload in `deploy/kcl/workloads.k` and bind it per env (see below) | Ordering primitive — no scaffold needed |
| An internal Go package with interface contract | `forge package new <name>`   | Package directory with contract interface and default implementation |
| A Next.js web frontend                       | `forge scaffold frontend <name>`    | Next.js app wired into the project |
| A React Native mobile frontend               | `forge scaffold frontend <name> --kind mobile` | Expo app with Connect-web transport |

## The fast path: scaffold everything the protos imply

The per-component commands above are the granular path. When you author the proto directly, there is a one-command batch alternative: mark each entity message with a leading `// forge:entity` comment, add your custom RPCs, then run **`forge scaffold`**. It births every marked entity (missing CRUD quintet injected + owned create-table migration) and runs generate, which emits a pb-through handler stub for every custom RPC — in one visible, phased run (`--dry-run` plans; a re-run with nothing missing is a clean no-op). In dev the app boots alive: `forge env up` auto-seeds a fresh DB from the applied schema. The entity/seed halves live in the `db` skill, the pb-through RPC handlers in `api`, and the standalone domain package (when you extract one) in `service-layer` / `contracts`.

## One-shot steps: the `job` workload kind

When something must **run once to completion before something else starts** —
register an OAuth client against the IdP, provision a bucket, load a fixture —
that is a `job`. Every other workload kind is long-running: `service` /
`worker` / `operator` run forever, `cron` runs on a schedule forever, and a
`tool` is never scheduled at all. `job` is the one that ends.

Declare it ONCE in `deploy/kcl/workloads.k`, like every workload, and bind
it in each env that runs it. Ordering (`before`) is part of the declaration;
WHERE it runs is the env's binding:

```kcl
# deploy/kcl/workloads.k
provision_idp = fw.Workload {
    name = "provision-idp"
    kind = "job"
    build = forge.GoBuild {cmd = "./cmd/myproj", output_name = "myproj"}
    args = ["provision-idp"]  # a subcommand of the project binary
    before = ["api"]          # `api` does not start until this exits 0
}

# deploy/kcl/dev/main.k — one binding line
_workloads = [
    _on_host_job(wl.provision_idp)
    _on_host(wl.api)
]
```

`before` is the ordering declaration and it is the point — a one-shot with no
ordering is just a cron that fires once. Each runtime honours it:

| runtime | lowering |
| ------- | -------- |
| cluster (`forge.OnCluster`) | an **init container** on each workload named in `before`, rendered by `pkg/deploy.RenderWorkloads`. It is the only ordering Kubernetes enforces itself, so it holds under any applier. A gating job renders **no** standalone `Job` — that would run the command a second time, unordered. |
| cluster, `before` empty | a standalone `batch/v1` Job, pre-rollout by default (`forge env deploy` waits for it before applying Deployments); `deployPhase = "post-rollout"` defers it. |
| hosted (`forge.OnHosted`) | the control plane runs it the same way, before the workloads it gates. |
| host (`forge.OnHost`) | `forge env up` runs `go run ./cmd/<p> <args>`, waits for exit 0, and only then launches the dependents. Fail-closed: a job that exits non-zero stops the up. |
| compose (`forge.OnCompose`) | the compose file owns it: `restart: "no"` plus `depends_on: {<job>: {condition: service_completed_successfully}}` on each dependent. |

**The job's command must be idempotent.** The k8s lowering fans it out to one
init container per dependent, and init containers re-run on every pod start,
every replica and every restart. That is the same contract the deploy-time
migration step already keeps (golang-migrate takes a postgres advisory lock),
and a one-shot that is not safe to repeat is unsafe under any retrying
orchestrator.

Things forge rejects at load time rather than at 3am: a `before` naming a
component that does not exist, a job with no `command`, a job carrying a
`schedule` (use `kind = "cron"`), a workload an env lists without binding a
runtime, and — on the host path — a cycle in the job graph.

## Wiring Cycle

Follow this sequence every time you scaffold a new component:

1. **Scaffold** — run the appropriate `forge scaffold` or `forge package new` command.
2. **Define the contract** — edit the `.proto` file (services) or the interface (packages).
3. **Generate** — run `forge generate` to produce Go code from protos and contracts (handler stubs, mocks, Connect clients).
4. **Implement** — write the business logic on the `*Service` methods in `internal/handlers/<svc>/` using `s.deps` (a custom RPC is a pb-through method on `*Service`; CRUD delegates via `handlers_crud.go`). For a reusable, transport-free domain layer, scaffold a standalone package (`forge scaffold package <name>`) and call it from the handler.
5. **Compose it into a binary** — a binary serves a service because `NewComponents` constructs it and the serve path mounts it. Add the constructor call to the composition (see below); selection is code, not a string table.

## Deploying it: declared once, bound per env

`forge scaffold service|worker|operator|binary` appends the workload to
`deploy/kcl/workloads.k` — build, the component's subcommand in `args`, the
`http` port for a service — and a binding line to every env's `_workloads`
list (`_on_host(wl.<name>)` in dev, `_on_cluster(wl.<name>)` in staging and
prod). Probes need nothing: a service forge builds gets `/readyz` + `/healthz`
on its `http` port on every runtime. See the `deploy` skill for the runtimes
and how to rebind one workload (to the forge control plane, say).

## Port Assignment

In a container every service owns `:8080` (the `http` port). In the dev loop
each host process gets its own port from `plugin.resolve_port` in
`deploy/kcl/dev/main.k`. Do not hard-code port numbers elsewhere.

## Rules

- **Always use `forge scaffold` or `forge package new`** — never copy-paste an existing service or package directory.
- **One service per proto package** — keep proto definitions focused on a single domain.
- **Run `forge generate` after any proto or contract change** — generated code must stay in sync.
- **Service names canonicalize** the same way worker names do: lowercase snake_case (hyphens → underscores, PascalCase boundaries split). `forge scaffold service admin-server` keeps `admin-server` as the `forge.yaml` `name:` display key, but the on-disk leaf, Go package decl, and `forge.yaml` `path:` leaf are all `admin_server` (`internal/handlers/admin_server/`, `package admin_server`, `path: internal/handlers/admin_server`). See the `workers` skill Naming section for the full rule and the migration gotcha; see `architecture` for the cross-ecosystem naming-conventions table.
- **Service code lives under `internal/handlers/<svc>/`** — `service.go` (Deps/New), the rpc methods that hold its logic, and the generated CRUD ops co-located in ONE directory, with no `contract.go`. The `handlers/` role subtree is under `internal/`, not top-level; a service is app-internal, imported by nobody external.

## Serving a service = composing it (the composition root)

**What a binary serves is the set of constructors it calls — not a string row in a registry.** The explicit composition is split across two files under `internal/app/`, **both yours**: `providers.go` (`Infra` + `OpenInfra`) and `compose.go` (`Components` + `NewComponents(infra *Infra) (*Components, error)`). A binary serves a service because `NewComponents` constructs it and the serve path mounts its handler:

```go
// internal/app/compose.go (yours; reconciled — newly discovered components and
// Deps keys are injected, your value expressions are kept)
func NewComponents(infra *Infra) (*Components, error) {
    c := &Components{}
    c.Users = user.New(user.Deps{Repo: infra.Repo})
    c.Bill  = billing.New(billing.Deps{Users: c.Users})  // dep by INTERFACE, in-process default
    // two-phase (bill.WithReliantAPIKeyIssuer(infra.LLM)) → just add it here; it is kept
    return c, nil
}
```

The serve path (`cmd/<bin>/cmd/serve.go`) then applies the typed `Mount<Svc>` method values onto a `serverkit.Server` and calls `srv.RequireMounted(...)`.

- **Selection is code.** To stop serving a service, stop constructing it in `NewComponents` and stop mounting it. To serve it elsewhere, construct it in that binary's composition. No `forge.yaml` edit, no string match.
- **The `Deps` interface is the seam.** A consumer's field is `Users user.Service`, never the concrete type — it can't tell whether it got the in-process instance, a Connect client, or a mock. Splitting a service into its own Deployment later is a one-line swap in `NewComponents` (`billing.New(billing.Deps{Users: userclient.New(conn)})`) with the consumer untouched.
- **Per-binary singletons are natural.** A dep that must be one instance within each process (e.g. `enforcement` in both the server and the workspace-proxy) is one field on the owned `Infra`, opened once in `OpenInfra` and shared across the components.
- **`serverkit.Run` takes a composed server with typed mounts, not service-name strings.** It runs handlers + workers + operators + lifecycle; *which* of each is decided by what `NewComponents` constructed and the serve path mounted. There is no `args`/`names` variadic, no name-matched wiring.

## Subcommands are real cobra, not a projected string table

Each service gets its own file at `cmd/<bin>/cmd/services/<name>.go` — forge-owned (Tier-1, `forge project disown` to take it), exporting a real cobra command with its own flags, help, and a jumpable Go symbol:

```go
// cmd/<bin>/cmd/services/billing.go — generated
func NewBillingCmd(deps cmd.Deps) *cobra.Command {
    // ...RunE:
    return cmd.Serve(c.Context(), deps, cmd.ServeSpec{Mount: (*app.Components).MountBilling})
}
```

The mount is the typed METHOD EXPRESSION, named directly — no name→func lookup anywhere on the run path. `cmd/<bin>/cmd/server.go` is the all-services command and mounts `(*app.Components).MountAll`; there is no `server [services...]` string-matching and no generated `cmd/services_gen.go`. Custom NON-service subcommands go in the user-owned `cmd/<bin>/cmd/commands.go` (`userCommands(deps)`) — see the `binaries` skill.

## Inventory (introspection only)

A data-only inventory (`{Name, ConnectPath, Mount}`) survives for `forge project map`/`forge project audit` and CLI listing. Names there are for **display only** — never a lookup key for construction. `forge project audit` flags orphan stubs (a generated `Unimplemented` service nobody composes) and `forge lint` flags unresolved non-optional Deps and narrow-interface silent drops.

## When This Skill Is Not Enough

- **Simple utility packages** — create a directory under `internal/` and write plain Go. No scaffold needed. (`pkg/` is reserved for code with real external importers.)
- **CLI-only projects** — use `forge project new <name> --mod <module>` without `--service` to create a Cobra CLI binary with no server bootstrap.
- **One-off scripts or CLI tools within existing projects** — add a subcommand under `cmd/` (same image, opt-in config/OTel via the cmdkit paved path), or `forge scaffold binary <name>` if it needs its own deploy lifecycle. A parallel `cmd/<name>/main.go` outside the cobra root is invisible to forge build/deploy — avoid it.
