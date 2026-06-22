# Registry / DI Redesign — Execution Plan

Date: 2026-06-21

## OVERNIGHT EXECUTION LOG (2026-06-21, autonomous)

NOTE: branches `feat/up-generate-and-logs` (forge) and `chore/bump-forge-93067142`
(control-plane) have CONCURRENT work from others (otel/observe typed-config rework,
frontend mock-transport). My commits interleaved cleanly; theirs left untouched. Nothing
deployed — all branch work, every committed step passed build + boot + parity gates.

LANDED & VERIFIED (I re-ran build/boot/parity myself, not trusting agent reports):
- forge `b6abf86` — forgepb relocated to public `pkg/forgepb`.
- forge `ef7732c` — `pkg/mountkit.RegisterService`.
- forge `3d32fb5` — `pkg/authz.FromDescriptors` (descriptor-driven authz).
- forge `a956bad` — generator: Path A scaffolds forge.proto at shared forgepb.
- forge `6baf327` — `pkg/config` descriptor-driven loader LIBRARY (consumer migration NOT done).
- cp `699eb95` — keystone: daemon on descriptor-auth.
- cp `2a0bf81` — Path A proto unification: cp consumes forgepb, local gen/forge/v1 deleted,
  boot panic fixed. VERIFIED: `go run ./cmd version` boots clean.
- cp `b7efd50` — 9 services migrated to descriptor-auth + mountkit, ~965 lines of per-service
  authz deleted. VERIFIED: build clean, boots, escalation parity green (17 admin procedures
  reject non-admin authenticated callers).

DEFERRED (need your judgment / review — bailed per the agreed rule):
- audit_log, daemon_token auth migration — their protos LACK `auth_required` annotations;
  descriptor-auth would fail-closed DENY them. Need proto annotation + regen first;
  daemon_token also has internal-service-only Managed RPCs to review.
- proxy_authz — subject-identity gate (`sub == "internal-service"`), not a role; stays
  hand-mounted (already separate). Left as-is.
- config-loader CONSUMER migration — library is built, but swapping it into control-plane
  needs proto changes (string duration fields → google.protobuf.Duration) + carrying Port
  validation, cross-field Validate(), Mode()/DevAuthBypass(). Correctness-sensitive across
  envs — left for review.
- tenant_gen / audit interceptor_gen library-ization — generic shims, but audit-log shape is
  compliance-sensitive and verification is weaker than auth had (no clean parity test);
  deferred rather than edited blind, esp. with concurrent otel/observe work on the branch.
- Design-heavy (always needed you): command-owned wiring + delete the injector (E),
  scenario interceptor + --mock/provider + config chaining (F), kalshi migration incl.
  MarketAPI seam (G), generator deletions of inject/inventory/build_topo (H).

Original status: draft (Phase 0 → keystone+rollout executed; see log above)
Owner: Sean
Companion to: FORGE_SHAPE_REDESIGN.md (this is the execution of its registry/DI half)

## Goal

Stop generating wiring and policy into user projects. Codegen retreats to *schema
projection*; everything cross-cutting becomes an imported forge library; per-project
wiring becomes hand-owned, command-scoped Go you read top to bottom.

## The litmus test (applies to every artifact)

- **Generate** only what projects a typed schema (proto, SQL, a Go interface) into code.
- **Library** anything whose logic is identical across projects and varies only by data
  it reads at runtime (descriptors, config).
- **Scaffold-once, owned** the per-project structure a human/LLM should be able to bend.

## Target end-state (per user repo)

- **Generated (mechanical):** proto→pb/connect (buf), DB entities/CRUD from migrations,
  frontend hooks, **mocks**. Nothing else.
- **Owned (you bend it):** `config.proto`, migrations, handler `service.go`/`contract.go`,
  **per-command wiring + à-la-carte provider constructors**, `cmd/*.go`, rare custom
  authorizers, scenario files.
- **Imported from forge (not in the repo):** `RegisterService`, descriptor auth/authz
  interceptor, config loader, observability/audit/rate-limit/tenant interceptors,
  worker/operator lifecycle, crud/testkit/tdd, serverkit, scenario interceptor.

### Key structural moves

1. **Delete the generated injector.** `inject_gen.go` → hand-written, owned wiring.
   Delete generator machinery: `build_topo.go`, `inject_gen.go`, `infra_assignability.go`.
2. **Wiring lives in the commands, not one global `Build()`.** Each cobra command is its
   own composition root, constructing only what it needs from shared provider constructors.
   `forge.yaml` + proto stay the topology source of truth for `forge map`/deploy.
   Deletes `inventory_gen.go` + `services_gen.go` — and dissolves control-plane's
   proto-layout blocker (hand-registration doesn't care where the proto lives).
3. **Descriptor-driven auth.** One interceptor reads `(forge.v1.method)` options off the
   registered FileDescriptors at startup (RolesDecider already exists in `forge/pkg/authz`).
   Deletes every per-service `authorizer_gen.go`; only real ownership policy keeps a small
   hand-authorizer.
4. **`RegisterService` library.** Generic mount: interface-assert optional capabilities
   (webhooks/REST/custom HTTP) + apply the shared interceptor chain. Deletes Mount closures.
5. **Config loader library.** Descriptor-driven `forge/pkg/config.Load(cmd, &cfg)` walks the
   config proto's fields + `(forge.v1.config)` options. Deletes the generated `pkg/config`
   loader; only the buf proto struct + tiny owned derived helpers remain.
6. **Substitution by config, two grains:** coarse provider/`--mock` swaps in the provider
   constructors; fine per-RPC overlays via a config-selected **scenario interceptor**
   (backend analog of frontend ADR-0002, "production code path must run"). Both dev-gated,
   both audited, both driven by a layered/chained config (defaults→env-file→env→flags→--mock).

## Repos in scope

1. **forge** — `forge/pkg/*` libraries (upstream target) + generator changes.
   NOTE (Phase 0 D1): forge's own app is a **CLI** (`kind: cli`, no services) — it has none
   of the server-shape DI/auth/inventory/config-loader machinery, so there is **no
   forge-self server migration and no server dogfood**. forge is current on CLI-applicable
   migrations. Drop the former "Phase 4 forge self-upgrade" workstream.
2. **control-plane** — consumer, mid-migration; proto-layout blocker; messy real case.
3. **kalshi-trader** — consumer, stable sandbox; proves the `Markets` interface seam +
   `--mock` provider + a backend scenario.

## Strategy: vertical slices (feature + migration together)

Each capability is built in forge **and** consumed in one real service in the same slice,
proven end-to-end, before the next capability builds on it. Rollout to remaining
components/repos comes after the keystone slice validates the library APIs.

## Subagent / parallelization guardrails (READ BEFORE SPAWNING)

- **Commit incrementally.** Never `git checkout .`, never tree-wide reverts, never
  `forge generate` that wipes uncommitted work. (An agent's `git checkout .` once wiped a
  tree — do not repeat.)
- **Scope builds to your packages.** Don't panic on momentary cross-repo breakage; never
  revert another agent's work.
- **Coexistence, not big-bang.** Old (generated) and new (owned) wiring must compile
  side-by-side at every step. Migrate one component/command at a time.
- **Validate locally.** CI is broken (billing). For forge tests on macOS use
  `FORGE_TEST_POSTGRES_URL` (embedded PG flakes at shmmni=32).
- **Read-only agents for discovery.** No edits in Phase 0.

## Phases

### Phase 0 — Discovery — DONE (findings below)

**Key findings (2026-06-21):**
- **D1:** forge-the-project is a CLI; no server migration/dogfood. (See Repos-in-scope note.)
- **GATING BLOCKER (D4):** `E_Method`/`E_Config` proto-option extension symbols live in
  `forge/internal/gen/forge/v1` — not importable by `forge/pkg/*` or user projects. **Must
  relocate to a public path (e.g. `forge/pkg/forgepb`) before ANY descriptor-driven library
  compiles.** This is Phase 1 step 0.
- **D2/D4 classification confirmed:** `tenant_gen` (generic, params claim_field+column_name)
  and audit `interceptor_gen` (generic, params logger+store) → **libraries**, not gen.
- **D2:** control-plane `cmd/server.go` is already an owned composition root reading
  `app.Inventory`; the inject/inventory/services gens are hash-stamped-but-hand-maintained.
  Migration = formalize + swap to libs + delete redundant scaffolding (lower risk).
  6 global `Set*` seams to make explicit: `log.SetLogger`, `SetTokenValidator`,
  `SetAuditStore` (+local setter), `SetIdentityEnricher`, `SetClaimsLookup` (authorizer init).
  Delete/rewrite path ≈ 2,160 lines (incl. 13 `authorizer_gen.go` ≈ 965). `cmd/workspace-proxy`
  is an independent root — leave it alone.
- **D3:** kalshi `MarketAPI` seam = 6 methods (`GetMarketsContext`, `GetMarketContext`,
  `GetBalanceContext`, `GetSettlementsContext`, `GetFillsContext`, `CreateOrder`);
  `*kalshi.Client` satisfies as-is; single swap point at `providers.go:202-207`; settlement/
  shadow adapters already take narrow interfaces (precedent). Only blocker: field type
  `*kalshi.Client` → `kalshi.MarketAPI`.
- **D4 pkg homes:** extend `pkg/authz` (descriptor decider+interceptor), `pkg/config`
  (`LoadFromDescriptor`); move `Authorizer`/`AuthzInterceptor`/`DevAuthorizer` from templates
  into `pkg/middleware`; `RegisterService` + scenario interceptor are NEW surface
  (serverkit/new mountkit). Generator files to shrink/delete: `inject_gen.go` (730),
  `build_topo.go` (432), `infra_assignability.go` (295), `inventory_gen.go` (197),
  `deps_parser.go` (528), `config_gen.go` (662), `authz_gen.go` (266), `cmd_services_gen.go` (101).
- **D1 forge self-state:** does forge's own app use the legacy injector/inventory/auth/
  config-loader shape? Which published migration skills does it still need? Catch-up list.
- **D2 control-plane change inventory:** every file on the injector/inventory/authorizer/
  config-loader/services_gen path; the `cmd/*` command topology; proto-layout blocker
  specifics; classify `tenant_gen` + audit `interceptor_gen` (lib vs gen).
- **D3 kalshi change inventory + Markets seam:** same inventory; map every `*kalshi.Client`
  use site; design the `MarketAPI` interface + a `mock` provider (the `--mock` proving case).
- **D4 forge generator + pkg surface:** exact files/templates generating inject/inventory/
  authorizer/config-loader/services_gen (to delete/modify); existing `forge/pkg` surface
  (authz, observe, middleware, appkit, crud, testkit) to extend rather than duplicate.

### Phase 1 — Keystone slice (GATE) — forge F1+F2 + control-plane/daemon

**GATE FINDING (2026-06-21): proto double-registration boot panic.** Publicizing `forgepb`
(Step 0) + `pkg/authz` importing it means consumer binaries link forge's copy of
`forge/v1/forge.proto` AND the project's own generated `gen/forge/v1` → `proto: file
"forge/v1/forge.proto" is already registered` panic at init (confirmed on control-plane's
server binary; `go build` + isolated tests do NOT catch it). The daemon parity LOGIC is
proven (isolated test `699eb95`) but the integrated binary can't boot until this is fixed.

**Decision: Path A (durable) — forge.proto becomes the shared `forgepb` library; projects
stop generating their own `gen/forge/v1`.** Mechanism (per discovery, buf option (i)):
point `forge/v1/forge.proto`'s `go_package` at `forge/pkg/forgepb`, exclude `forge/v1` from
Go generation, delete the local copy; no hand-written importers exist so regeneration
re-points the blank-imports. Lands in (a) forge generator (embedded.go + buf.gen.yaml
template — all future projects) and (b) control-plane/kalshi one-time migration. This is the
TRUE Phase-1 gate. (Path B, dynamic extension read with no forgepb import, was the smaller
tactical fix but was rejected in favor of the principled single-shared-schema end-state.)

- **Step 0 (done):** relocate `forge/internal/gen/forge/v1` → public path
  (`forge/pkg/forgepb`). Update go_package (proto + rawDesc), importer
  `internal/cli/forge_descriptor.go`, `internal/assets/embedded.go` rewrite target. Verify
  `go build ./...` + descriptor tests. Nothing descriptor-driven proceeds until this lands.
- forge: `pkg` `RegisterService` (F1) + descriptor-auth interceptor in `pkg/authz` (F2),
  with unit tests. (F1 + middleware-move are independent of Step 0 → can parallelize.)
- control-plane: hand-wire `daemon` in a command root; register via the lib; delete
  `daemon/authorizer_gen.go`; **prove identical allow/deny + build + tests green locally.**
- Other 21 components stay on the generated path (coexistence).
- **Gate:** parity proven → library APIs validated → proceed.

### Phase 2 — Generator changes (F5) + config loader (F3)
- F3 `forge/pkg/config` descriptor loader + tests (independent → parallel with F5).
- F5: stop generating inject/inventory/services_gen; `forge add service` appends to a
  command manifest; emit the owned composition-root scaffold; keep generator machinery
  until consumers are off it.

### Phase 3 — Scenario + `--mock`/provider config (F4)
- Backend scenario interceptor + layered/chained config + `--mock`/provider convention +
  `forge add scenario` backend path.
- Prove in kalshi: `MarketAPI` mock provider + one backend scenario (one RPC canned,
  everything else real).

### Phase 4 — Rollout (parallel per repo / component group)
- control-plane: remaining components → command-owned wiring; config-loader swap; delete
  generated authorizers; unblock proto-layout. (parallel agents per command/component group)
- kalshi: full migration + Markets seam.
- forge self-hosted app: catch up + adopt new libs (dogfood).
- **Gate per repo:** build + tests green locally.

### Phase 5 — Delete machinery + docs
- Remove `build_topo.go`, `inject_gen.go`, `infra_assignability.go`, the generated config
  loader template, inventory/services_gen generators — once no consumer references them.
- Update forge skills/docs + add a migration skill for the new shape.

## Open decisions / risks
- `tenant_gen` + audit `interceptor_gen` classification — settle in Phase 0 (D2).
- forge self-app staleness depth — may widen Phase 4 (D1).
- Per-command wiring duplication of shared infra — absorbed by provider constructors;
  confirm the seam holds for control-plane's shared per-binary enforcement instance.
