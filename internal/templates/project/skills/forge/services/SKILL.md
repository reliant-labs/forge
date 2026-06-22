---
name: services
description: Scaffold and wire new services, packages, and frontends in a Forge project.
---

# Adding New Components to a Forge Project

Use this skill whenever you need to introduce a new network-facing service, internal package, or frontend into a Forge mono-repo. Every scaffolded service comes with production middleware baked in — structured logging, distributed tracing, auth, rate limiting, and graceful shutdown — so you focus on business logic from day one.

## Choosing the Right Command

| I need…                                      | Command                        | What it creates                          |
| -------------------------------------------- | ------------------------------ | ---------------------------------------- |
| A new network-facing API (Connect RPC)       | `forge add service <name>`     | Proto definition, generated stubs, Go service skeleton |
| A background worker                          | `forge add worker <name>`      | Worker with Start/Stop lifecycle |
| A cron-scheduled worker                      | `forge add worker <name> --kind cron --schedule "..."` | Worker with cron scheduler |
| An internal Go package with interface contract | `forge package new <name>`   | Package directory with contract interface and default implementation |
| A Next.js web frontend                       | `forge add frontend <name>`    | Next.js app wired into the project |
| A React Native mobile frontend               | `forge add frontend <name> --kind mobile` | Expo app with Connect-web transport |

## Wiring Cycle

Follow this sequence every time you scaffold a new component:

1. **Scaffold** — run the appropriate `forge add` or `forge package new` command.
2. **Define the contract** — edit the `.proto` file (services) or the interface (packages).
3. **Generate** — run `forge generate` to produce Go code from protos and contracts (handler stubs, mocks, Connect clients).
4. **Implement** — write the business logic behind the `Service` interface in `internal/handlers/<svc>/contract.go`.
5. **Compose it into a binary** — a binary serves a service because `NewComponents` constructs it and the serve path mounts it. Add the constructor call to the composition (see below); selection is code, not a string table.

## Port Assignment

Ports are assigned automatically via `forge.yaml`. Do not hard-code port numbers; let Forge manage them.

## Rules

- **Always use `forge add` or `forge package new`** — never copy-paste an existing service or package directory.
- **One service per proto package** — keep proto definitions focused on a single domain.
- **Run `forge generate` after any proto or contract change** — generated code must stay in sync.
- **Service names canonicalize** the same way worker names do: lowercase snake_case (hyphens → underscores, PascalCase boundaries split). `forge add service admin-server` keeps `admin-server` as the `forge.yaml` `name:` display key, but the on-disk leaf, Go package decl, and `forge.yaml` `path:` leaf are all `admin_server` (`internal/handlers/admin_server/`, `package admin_server`, `path: internal/handlers/admin_server`). See the `workers` skill Naming section for the full rule and the migration gotcha; see `architecture` for the cross-ecosystem naming-conventions table.
- **Service code lives under `internal/handlers/<svc>/`** — contract.go, impl, and generated handlers co-located in ONE directory. The `handlers/` role subtree is under `internal/`, not top-level; a service is app-internal, imported by nobody external.

## Serving a service = composing it (the composition root)

**What a binary serves is the set of constructors it calls — not a string row in a registry.** The explicit composition is split across two files under `internal/app/`: the owned `providers.go` (`Infra` + `OpenInfra`) and the generated `compose.go` (`Components` + `NewComponents(infra *Infra) (*Components, error)`). A binary serves a service because `NewComponents` constructs it and the serve path mounts its handler:

```go
// internal/app/compose.go (forge-owned, regenerated — disown to hand-own)
func NewComponents(infra *Infra) (*Components, error) {
    c := &Components{}
    c.Users = user.New(user.Deps{Repo: infra.Repo})
    c.Bill  = billing.New(billing.Deps{Users: c.Users})  // collaborator by INTERFACE, in-process default
    // two-phase (bill.WithReliantAPIKeyIssuer(infra.LLM)) → disown compose.go and edit here
    return c, nil
}
```

The serve path (`cmd/<bin>/cmd/serve.go`) then applies the typed `Mount<Svc>` method values onto a `serverkit.Server` and calls `srv.RequireMounted(...)`.

- **Selection is code.** To stop serving a service, stop constructing it in `NewComponents` and stop mounting it. To serve it elsewhere, construct it in that binary's composition. No `forge.yaml` edit, no string match.
- **The collaborator interface is the seam.** A consumer depends on `Users user.Service`, never the concrete type — it can't tell whether it got the in-process instance, a Connect client, or a mock. Splitting a service into its own Deployment later is a one-line swap in `NewComponents` (`billing.New(billing.Deps{Users: userclient.New(conn)})`) with the consumer untouched.
- **Per-binary singletons are natural.** A collaborator that must be one instance within each process (e.g. `enforcement` in both the server and the workspace-proxy) is one field on the owned `Infra`, opened once in `OpenInfra` and shared across the components.
- **`serverkit.Run` takes a composed server with typed mounts, not service-name strings.** It runs handlers + workers + operators + lifecycle; *which* of each is decided by what `NewComponents` constructed and the serve path mounted. There is no `args`/`names` variadic, no name-matched wiring.

## Subcommands are real, owned compositions

Each cobra subcommand is real cobra (own flags, own help, a jumpable Go symbol) — **not a projection of a string table.** `cmd/billing.go` is typically one line via a helper:

```go
serverCmd("billing", app.MountBilling)
```

The `server` multi-mount subcommand composes all `MountX`. There is no generated `cmd/services_gen.go` and no `server [services...]` string-matching — both are gone. Custom non-service subcommands go in `cmd/` as their own owned subcommand (see the `binaries` skill).

## Inventory (introspection only)

A data-only inventory (`{Name, ConnectPath, Mount}`) survives for `forge map`/`forge audit` and CLI listing. Names there are for **display only** — never a lookup key for construction. `forge audit` flags orphan stubs (a generated `Unimplemented` service nobody composes) and `forge lint` flags unresolved non-optional Deps and narrow-interface silent drops.

## When This Skill Is Not Enough

- **Simple utility packages** — create a directory under `internal/` and write plain Go. No scaffold needed. (`pkg/` is reserved for code with real external importers.)
- **CLI-only projects** — use `forge new <name> --mod <module>` without `--service` to create a Cobra CLI binary with no server bootstrap.
- **One-off scripts or CLI tools within existing projects** — add a subcommand under `cmd/` (same image, opt-in config/OTel via the cmdkit paved path), or `forge add binary <name>` if it needs its own deploy lifecycle. A parallel `cmd/<name>/main.go` outside the cobra root is invisible to forge build/deploy — avoid it.
