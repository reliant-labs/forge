# Loud-by-default: `server <name...>` subcommand filter

- **Status**: proposed
- **Author**: cp-forge agent (2026-06-08 prod KCL silent-filter postmortem)
- **Related**: commit `3425d9e` (loud-by-default audit pass), `pkg/diagnostics` proposal
- **Touches**: project template `internal/templates/project/cmd-server.go.tmpl`, generated `pkg/app/bootstrap.go` `BootstrapOnly`, optional new lint in `internal/cli/lint_server_filter.go`.

## Context

Forge's project template emits `cmd/server.go` with the signature `server [services...]`. Passing one or more service names filters which services get `.Register(mux, ...)`'d. The intent is per-pod scope-down for KCL deployments that run one binary per service (admin-server pod runs only AdminServerService routes, workspace-proxy pod runs only WorkspaceProxyService routes, etc.).

The trap: a filter name like `admin-server` matches **only** the `services.admin_server.v1.AdminServerService` (the CRUD stub forge scaffolded). Every other `controlplane.v1.*`, `reliant.v1.*`, `audit.v1.*` service that the same binary owns gets constructed but **not** mux-registered. Result: the admin-server pod returns 404 for `/controlplane.v1.UserService/...`, `/controlplane.v1.BillingService/...`, the DaemonRegistry compatibility adapter, etc. — even though Setup ran, NATS connected, the DB pool is healthy, and the binary boots green.

cp-forge hit this twice:

1. **dev (cloud-dev.sh)** — host-mode admin-server launched as `go run ./cmd server admin-server`. Every `/reliant.v1.DaemonRegistryService/ListDaemons` returned 404 from `:8090`. Fix: drop the third arg. Now `go run ./cmd server` (no filter → register all).
2. **prod + staging KCL** — `deploy/kcl/{prod,staging}/main.k` shipped `command = ["./cp-forge", "server", "admin-server"]`. Same 404 surface, deployed to live clusters. Fix: drop the third arg in both KCL files. Workspace-proxy keeps its filter (its filter is correct — the pod only needs WorkspaceProxyService routes).

The current `BootstrapOnly` partially helps: when a filter is active it logs a `server filter active — RPCs for excluded names will return 404` warn line naming registered vs excluded sets. That landed in some prior pass and saved several hours when re-debugging the dev case. But:

- The warning is a single `logger.Warn` line, easy to lose under boot noise.
- It fires uniformly for every filter — including the *correct* `workspace-proxy` filter. So the signal-to-noise is bad: operators see the warn line on every workspace-proxy boot and learn to ignore it.
- The KCL author who wrote `command = ["./cp-forge", "server", "admin-server"]` is the one who needs to see the signal, not the SRE reading boot logs three weeks later. There's no static-time check.

## The class of bugs

Same shape as the silent-skip class commit `3425d9e` already partially closed:

- `forge.yaml` declares a service with no on-disk dir → now a hard error (was a silent skip).
- On-disk service dir not in `forge.yaml` → now a hard error (was silent).
- Deprecated `forge.yaml` keys → now hint at the migration (was silent ignore).
- `go mod tidy` failure → now fatal (was a silent warn).
- 27 codegen pipeline `Warning: ... + return nil` sites → now `warnOrFail(--strict)`.

The server-subcommand filter is the runtime analog. Same anti-pattern: a silently-narrow selection that the author didn't mean to make narrow. Same fix shape: name what got excluded, fail loudly when the exclusion was almost certainly a mistake.

## Proposed change

Three layers, smallest to largest:

### 1. Boot-time error when the filter looks like a project-name typo

When `BootstrapOnly` is called with `names = ["<projectname>"]` (e.g. `["cp-forge"]`) or `names = ["server"]` (e.g. someone typed `./bin server server`), error out at boot:

```
./cp-forge server cp-forge
ERROR: 'cp-forge' is not a registered service name. Did you mean to run the binary with no filter?
       Pass no arguments to register all services, or pass one or more of:
         admin-server, workspace-proxy, daemon, ...
```

The current "unknown service" path just `logger.Warn`s and continues with an empty filter → registers nothing → 404s every RPC. That degrades to total outage with a warn no one will see.

### 2. Boot-time fatal for an "absurd" filter — opt-in, opt-out

A filter is "absurd" when:

- The named service has fewer than N% of the project's total RPC routes (e.g., the `admin_server` scaffold service has 5 stub RPCs; the catch-all admin-server binary has 80+).
- AND the filter is the only filter (single-arg).
- AND the project has more than one service with substantially more routes than the filtered one.

Heuristic: if the operator filtered to a service that owns < 10% of the project's RPC surface AND there exists another service that owns > 50%, the operator probably meant the bigger one. Default = warn loudly (boot prints a multi-line block, not a one-liner). Opt-in fatal via `forge.yaml: features.strict_server_filter: true`. Opt-out per-binary via env `FORGE_SUPPRESS_SERVER_FILTER_WARN=1`.

The route counts come from the same proto-walk that already populates `forge audit`'s `proto_integrity` shape. No new AST machinery.

### 3. Lint: KCL-vs-bootstrap cross-check

`forge lint` (and `forge audit`) walk every `deploy/kcl/<env>/main.k` (already discovered for `forge deploy`). For each `forge.Service` with a `command` ending in `["./...", "server", "<name>"]`, look up `<name>` in the bootstrap's service registry:

- Unknown name → error (typo).
- Known name, but the service owns < 25% of the project's RPC routes AND no sibling service in the same KCL bundle is launched with the catch-all binary → warn ("`admin-server` in prod KCL filters to 5 RPCs; the admin frontend imports 80+ from this binary. Did you mean `command = [..., 'server']`?").
- Same matrix as the boot-time check but caught at `forge lint` / `forge audit` / pre-commit, not at pod-boot.

Lint output mirrors the `forgeconv.Finding` shape so `forge audit --json` consumers pick it up additively.

## Why this matters

The cost is hours per incident, and it happens during the most stressful moments: prod deploys, post-deploy smoke, "the admin UI just stopped working." The boot log is green, the pod is `Running`, health checks pass, metrics scrape — and every API call is a 404. The fault is in the KCL `command` array, three directories away from where the operator is debugging.

The dev-time fix (the existing warn) helped but is not enough. Loud-by-default at boot AND lint AND project-shape heuristics is the same pattern `3425d9e` chose for the codegen pipeline. Adopt it for the runtime filter too.

## Out of scope (for this proposal)

- Renaming `server <name>` to something less footgun-shaped (e.g., `server --service=<name>`). Touches every project's KCL `command` arrays — separate breaking change, separate migration.
- Per-service binaries via `binary: per-service` in `forge.yaml`. That's the right long-term answer for projects that want hard process-level isolation, but most projects keep `binary: shared` for build-cycle reasons. The filter is the workhorse.
- Default-filter behavior change (today: no args → register all). Keep as-is; it's the right default. The proposal is about catching mistaken filters, not removing the filter mechanism.

## Migration

- Boot-time error for project-name typo: zero migration, ships in next minor.
- Boot-time fatal for "absurd" filter: opt-in via `features.strict_server_filter`; default warn-loud. No breaking change.
- Lint cross-check: ships disabled by default for one minor, then becomes a default warn, then a default error one minor later. Matches the deprecation cycle the `migration-upgrade` skill documents.
