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
4. **Implement** — write the business logic behind the `Service` interface in `internal/<svc>/contract.go`.
5. **Compose it into a binary** — a binary serves a service because its `Build` constructs and mounts it. Add the constructor call to the composition root (see below); selection is code, not a string table.

## Port Assignment

Ports are assigned automatically via `forge.yaml`. Do not hard-code port numbers; let Forge manage them.

## Rules

- **Always use `forge add` or `forge package new`** — never copy-paste an existing service or package directory.
- **One service per proto package** — keep proto definitions focused on a single domain.
- **Run `forge generate` after any proto or contract change** — generated code must stay in sync.
- **Service names canonicalize** the same way worker names do: lowercase snake_case (hyphens → underscores, PascalCase boundaries split). `forge add service admin-server` keeps `admin-server` as the `forge.yaml` `name:` display key, but the on-disk directory, Go package decl, and `forge.yaml` `path:` are all `admin_server` (`internal/admin_server/`, `package admin_server`, `path: internal/admin_server`). See the `workers` skill Naming section for the full rule and the migration gotcha; see `architecture` for the cross-ecosystem naming-conventions table.
- **Service code lives under `internal/<svc>/`** — contract.go, impl, and generated handlers co-located in ONE directory. There is no top-level `handlers/<svc>/`; a service is app-internal, imported by nobody external.

## Serving a service = composing it (the composition root)

**What a binary serves is the set of constructors it calls — not a string row in a registry.** Each binary owns a typed composition root (`Build(infra) (*Server, error)`, e.g. `BuildServer`) under `internal/app/`. A binary serves a service because `Build` constructs it and mounts its handler:

```go
func BuildServer(infra Infra) (*Server, error) {
    shared := buildShared(infra)                  // infra + services both roots reuse
    users := user.New(user.Deps{Repo: shared.Repo})
    bill  := billing.New(billing.Deps{Users: users})  // collaborator by INTERFACE, in-process default
    bill.WithReliantAPIKeyIssuer(shared.LLM)          // two-phase: construct, then inject

    mux := http.NewServeMux()
    MountUsers(mux, users)
    MountBilling(mux, bill)
    return &Server{Handler: mux, /* Workers, Operators, OnShutdown */}, nil
}
```

- **Selection is code.** To stop serving a service, stop calling its constructor and `MountX`. To serve it elsewhere, call its constructor in that binary's `Build`. No `forge.yaml` edit, no string match.
- **The collaborator interface is the seam.** A consumer depends on `Users user.Service`, never the concrete type — it can't tell whether it got the in-process instance, a Connect client, or a mock. Splitting a service into its own Deployment later is a one-line swap in `Build` (`billing.New(billing.Deps{Users: userclient.New(conn)})`) with the consumer untouched.
- **Per-binary singletons are natural.** A collaborator that must be one instance within each process (e.g. `enforcement` in both the server and the workspace-proxy) is one `var` per `Build`; factor shared infra/services into `buildShared(infra)`.
- **`serverkit.Run` takes a composed `Server`, not service-name strings.** It runs handlers + workers + operators + lifecycle; *which* of each is decided by what `Build` constructed. There is no `args`/`names` variadic, no name-matched wiring.

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
