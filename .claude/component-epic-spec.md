# Forge epic: component model + feature graph + deploy-as-data

Branch: **feat/gateways**. **No backwards compat** (forge is unreleased) — delete old
shapes, don't deprecate. Binary: build `cmd/forge` → `/tmp/forge-bin/forge`.

Three changes, hard ordering:
- **A. Component model** — foundation; everything depends on it.
- **B. Feature dependency graph** — on top of A.
- **C. Deploy-as-data → KCL normalization** — on top of A; may run parallel with B.
- **D. Downstream fallout** (cp-forge, kalshi) — after A+B+C land + binary rebuilt.

Process rules (EVERY worktree agent): first `git reset --hard $(git rev-parse feat/gateways)`
and confirm HEAD; `cd` explicitly in every Bash (cwd resets); COMMIT EARLY per coherent step;
test tiers = `go test -short ./...` (~8s) inner loop, package-targeted before commit, plain
e2e gate `go test -tags e2e -count=1 ./internal/cli/` once at end AFTER committing; commits end
`Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.

---

## A. Component model

Replace BOTH `ProjectConfig.Services []ServiceConfig` AND `ProjectConfig.Binaries
[]BinaryConfig` with ONE `ProjectConfig.Components []ComponentConfig`, yaml key `components:`.

`ComponentConfig`:
- `Name string`
- `Kind string` — THE discriminator. **Repurpose the existing `kind` field; DELETE `type`.**
  Values: `server` (was type=go_service) · `worker` · `cron` (was worker+kind:cron, now
  first-class) · `operator` · `binary` (was the binaries: block).
- `Ports map[string]PortSpec` — named map; REPLACES scalar `Port`. `PortSpec` unmarshals from
  EITHER a YAML scalar int OR a struct `{port int, protocol string, expose bool}` (custom
  UnmarshalYAML). Consumers reference ports BY NAME (http/grpc/metrics/proxy/…).
- `Path string` — kind-derived default: server→`handlers/<pkg>`, worker/cron→`workers/<pkg>`,
  operator→`operators/<pkg>`, binary→`cmd/<pkg>.go`.
- `Schedule string` (cron) · `ProtoPackages`,`Webhooks` (server) · `Group`,`Version`,`CRDs` (operator).

Kind → behavior (preserve today's dispatch, keyed on `Kind`):
- **server**: Connect-RPC handlers + authorizer + client + frontend hooks + bootstrap row + cmd subcommand.
- **worker**: in-process ContextWorker goroutine; bootstrap Workers row.
- **cron**: scheduled job. Promote today's cron-worker; `Schedule` drives it. In-process scheduled
  goroutine for dev; KCL renders a CronJob (Phase C). Bootstrap row like worker, carrying Schedule.
- **operator**: controller-runtime manager + CRDs.
- **binary**: separate cobra SUBCOMMAND `cmd/<name>.go` (forge's existing binary model — ONE image,
  run `./app <name>`), `internal/<name>/` lifecycle, NO bootstrap wiring, NOT a separate main package.

`forge add service|worker|cron|operator|binary` all append a ComponentConfig with the right kind
(keep the convenient verbs — sugar over one model). `forge add binary` already exists; the others
move from the services/type path to components/kind.

Rename throughout (consumers mapped in the Explore survey — config validate, add.go, audit.go,
graph.go, generate_config_check.go, lint_bootstrap_deps_coverage.go, lint_config_deps.go,
project_bootstrap.go, binary_gen.go, cmd_services_gen.go, project_deploy.go, docs/generator.go):
`ServiceConfig`→`ComponentConfig`, `cfg.Services`→`cfg.Components`, `.Type`→`.Kind`,
`"go_service"`→`"server"`; fold `cfg.Binaries` into Components-where-kind==binary.

Templates: `services.go.tmpl` → `components.go.tmpl`. NAMING CARE: the registration-in-code file
lists what THIS binary SERVES = the **server-kind** components (only servers are "served" Connect
surfaces). Keep that semantic; `RegisteredServices` may stay (it returns the served server set) or
become `RegisteredServers` — pick the clearer one, update the generated `services_gen.go`→
`components_gen.go` row constructors and `cmd/services_gen.go` subcommand projection to match.
Update the scaffolded `forge.yaml` template: `services:`→`components:`, `type:`→`kind:`, scalar
`port:`→`ports:` map. Update audit/map/graph summaries to count by kind
(server/worker/cron/operator/binary).

Ports consumers: the `forge run` dev loop + CORS + DATABASE_URL composition + Dockerfile EXPOSE +
readiness probe read ports BY NAME now — update them (server's primary HTTP port is `ports.http`).

DONE = `go test -short ./...` green, package tests green, plain e2e gate green, a fresh
`forge new` scaffolds a `components:`/`kind:`/`ports:` forge.yaml that builds + boots.

---

## B. Feature dependency graph

Explicit dependency registry — each feature → required features / shape preconditions:
`frontend→codegen` · `orm→codegen + database-driver` · `migrations→codegen + database-driver` ·
`deploy→build` · `ingress→deploy` · `external_builds→build` · operator-kind component →
`features.operators` (experimental) on.

Validation at LoadStrict: a feature ENABLED (explicit or derived) with a dependency OFF is a load
error naming the fix — "feature 'frontend' requires 'codegen', which is disabled. Fix: enable
codegen, or disable frontend." Derivation (derive.go) still defaults from kind/shape but MUST
produce a dependency-consistent set; run graph validation after derivation + explicit overrides.

À LA CARTE litmus (must be a clean supported config): kind:service with ONLY orm+codegen+migrations
on (frontend/deploy/observability/hot_reload/packs/starters off) loads + generates with NO
contradiction — "forge as pure postgres schema-truth ORM + codegen." Add an e2e/unit pinning it.
`forge features` prints the resolved graph: each feature on/off + why (derived vs explicit) + deps.

---

## C. Deploy-as-data → KCL normalization

DELETE the Go projection layer: `project_deploy.go` field-by-field mapping, and the
`{{range .Services}}`/`{{range .Binaries}}` blocks in `deploy/kcl/*/main.k.tmpl` that hand-write
`forge.Service` entries as KCL TEXT.

Instead forge serializes component BASE shape to a generated JSON: `deploy/kcl/components_gen.json`
(Tier-1; self-cert via `.forge/hashes.json` since JSON carries no inline marker). Contents per
component: `{name, kind, ports, env (base defaults), command/args, health}` — DENORMALIZED, zero
k8s knowledge.

KCL owns ALL normalization via a typed inheritance hierarchy (forge ships it as an inherited KCL
library under `deploy/kcl/forge/`, like the pkg libraries — inherited, not copied):
`schema Component` (base: name, kind, ports, env) → `Server(Component)`, `Worker(Component)`,
`Cron(Component)`, `Operator(Component)`, `Binary(Component)`. `kind` selects the subtype; each
subtype expands to its k8s resources (Server→Deployment+Service with named ports; Worker→Deployment;
Cron→CronJob using Schedule; Binary→Deployment running `["/app/<proj>", "<name>"]`; Operator→its
manager Deployment+RBAC). forge JSON and KCL schemas are NOT 1:1 — KCL inheritance/defaults do the
expansion.

Per-env overlays stay in `deploy/kcl/<env>/main.k`: replicas, resource limits, secrets/env per env,
ingress/routes, cluster placement — they import `components_gen.json` + the base schemas and overlay.
Base (forge.yaml→JSON): name, kind, ports, command/args, base env, health.

Non-deploy consumers (Dockerfile, cmd-gen, audit, map) keep reading Components from forge.yaml —
unaffected. Verify: `kcl run deploy/kcl/dev` on a fresh scaffold yields valid manifests from
components_gen.json + schemas; multi-port server → Service with named ports; binary → Deployment
running the subcommand; cron → CronJob.

---

## D. Downstream fallout (after A+B+C merged + `/tmp/forge-bin/forge` rebuilt)

- **cp-forge**: forge.yaml `services:`→`components:`/`kind:`, `port:`→`ports:`; move workspace-proxy
  from the phantom service to `kind: binary` and DELETE the phantom `proto/services/workspace_proxy`
  + `handlers/workspace_proxy` shell; regen; verify build/test/audit/idempotent; fix any feature-graph
  contradictions surfaced.
- **kalshi**: same forge.yaml migration + ports map; regen; verify (`task test`).
- Both on `forge-canonical` branches (already exist); re-vendor happens via generate.
