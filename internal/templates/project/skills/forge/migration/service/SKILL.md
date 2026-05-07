---
name: service
description: Migrate a server-shaped project to forge — services, operators, workers, webhooks, packs, multi-binary cmd/, k8s manifests.
---

# Migrate a Server-Shaped Project

Use this skill when the existing project is network-facing: HTTP/gRPC servers, background workers, webhook receivers, k8s deployments. For CLI / library shapes see `migration/cli`. For prerequisites and the overall flow see `migration`.

## Scaffold

```bash
forge new <name>-next \
  --kind service \
  --mod github.com/<owner>/<name>-next \
  --service <main-service> \
  --frontend <main-fe>
```

`--kind service` is the default — emitting it explicitly documents intent. Pass `--service` and `--frontend` for the primary network-facing component(s); add the rest one at a time after scaffold so each can be verified in isolation.

## Add components, one at a time

```bash
forge add service <name>      # additional Connect-RPC services
forge add operator <name>     # k8s operator / controller loop
forge add worker <name>       # background worker (Start/Stop lifecycle)
forge add worker <name> --kind cron --schedule "..."   # cron worker
forge add webhook <name> --service <existing-service>  # webhook on an existing service
```

Run `forge generate && go build ./...` after each. Hyphens are OK in names; forge stores the hyphenated form as the display name and snake-cases the directory and Go package paths.

## Install packs after the components they extend

Order matters. A stripe webhook receiver needs the host service to exist before pack install can wire its handler:

```bash
forge add service billing
forge pack install stripe
forge generate
```

Available packs: `jwt-auth`, `clerk`, `api-key`, `stripe`, `twilio`, `audit-log`. See `packs` skill for details.

## Known pack landmines

These have all surfaced in real migrations. Spot the symptom, apply the fix immediately:

- **audit-log pack: nested subpackage layout.** The DB-backed interceptor is `auditlog.Interceptor` in `pkg/middleware/audit/auditlog/` (lives in its own Go subpackage so it cannot collide with the scaffold's slog-only `middleware.AuditInterceptor`). Wire one or the other in `pkg/app/setup.go`, not both — they record the same events.
- **jwt-auth pack on newer keyfunc.** Older revisions called the now-removed `keyfunc.Keyfunc.Cancel()` API. Fixed; if you see it again on a fresh fork, file the bug.
- **stripe pack proto package.** Generates as `db.v1`, NOT `<project>.db.v1`. Proto package names align with the buf module root, not the project name. Templates that try to prefix the project name will lint-fail.
- **Multiple webhooks on the same service** used to redeclare shared symbols (`webhookMaxBodySize`, `webhookEvent`, `extractEventID`, `verifyHMACSHA256`). Fixed; webhook templates now use unique names per webhook.

## Get to a green baseline before porting business logic

```bash
forge generate
go mod tidy
go build ./...
forge lint
```

All four must pass on the empty scaffold. Fix any failures here, not after porting — the failures will be much easier to read against zero ported code.

## Set the contracts floor

Before porting any internal package, edit `forge.yaml`:

```yaml
contracts:
  strict: true
  allow_exported_vars: false
  allow_exported_funcs: false
  exclude: []
```

Then `forge lint --contract` is part of the per-phase gate. See the `contracts` skill for the per-package design pattern (interface in `contract.go` first, `forge generate`, then implementation behind the interface).

## Porting order

1. **Internal utility packages first** (domain types, naming, validation helpers). These have the fewest deps on the rest of the codebase.
2. **Database layer** (`internal/db/` types + ORM functions, plus `db/migrations/`). Migrations are the schema source of truth — copy them as-is, do not regenerate from proto.
3. **Service handlers** (`handlers/<svc>/service.go`). The generated `*_gen.go` files (CRUD, authorizer) get rewritten on every `forge generate`; only your hand-written code moves over.
4. **Custom wiring** (`pkg/app/setup.go`). `bootstrap.go` is generated and re-emitted on every `forge generate` — do not port the source repo's bootstrap.
5. **Workers, operators, webhooks** — implement the lifecycle methods (`Start`, `Stop`, `Reconcile`, webhook event handlers).

## Multi-binary `cmd/` layouts

`forge add service` emits one `cmd/<service>/main.go` per service. If the source repo has additional binaries (CLI tools, background daemons, ops scripts) that aren't first-class forge components:

- For binaries that wrap forge-managed services or workers, prefer `forge add` and let forge own the wiring.
- For genuinely standalone binaries (one-off cron jobs, dev tooling), drop them under `cmd/<name>/main.go` directly. There is no `forge add binary` today; this gap is intentional — most "extra binary" cases are better modeled as workers or operators.

## k8s manifests

Forge emits `deploy/kcl/<env>/` (KCL-based manifests, one dir per environment: `dev`, `staging`, `prod`). Do NOT port hand-written YAML over the KCL output — KCL generates the YAML. Either:

- **Adopt KCL.** Translate hand-written manifest customizations into KCL overrides. Recommended.
- **Disable KCL.** Set `deploy: { mode: manual }` in `forge.yaml` and bring your own manifests under `deploy/k8s/`. You lose forge's per-env diff and validation.

`forge deploy <env>` runs against the active mode; verify which is configured before pushing.

## Final checks before declaring done

```bash
forge generate          # idempotent on a healthy project
forge lint              # contract + db + general lints
forge build             # binaries + frontends + Docker images
forge test              # unit + integration
forge test e2e          # full-stack (requires `forge run` in another shell)
forge deploy dev        # local k3d
```

## Rules

- One service per proto package. Hyphens in names are fine; forge handles the snake-case translation everywhere it needs to.
- Pack-after-component, then `forge generate`. Never the reverse.
- `bootstrap.go` is generated — all custom wiring goes in `setup.go`.
- KCL or hand-rolled manifests, not both.
- `forge generate` after every `forge add` and every `forge pack install`. It's idempotent and catches misconfigurations early.

## When this skill is not enough

- **CLI / library shape** — see `migration/cli`.
- **Designing the contract surface** for ported internal packages — see `contracts`.
- **Pack-specific config** (auth, billing, SMS) — see `packs`, `auth`, and the per-pack docs in `forge pack list`.
