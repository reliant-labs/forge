# Independent mobile implementation-readiness review

**Verdict: START BOUNDED PHASES NOW.**

Start repairs and executable foundation fixtures now. Do not start the entire F0–F5 roadmap in parallel, freeze the release API, or advertise production readiness. Further broad planning is not the critical path; executable consumer evidence is. F1 durable publication and later production capabilities have specific gates below.

## Review basis and confidence

Reviewed all seven requested documents in their working-tree form: `mobile-release-target.md`, `mobile-release-target-audit.md`, `mobile-foundation-audit.md`, its dependencies/lifecycle/boundaries audits, and `mobile-production-readiness.md`. Source HEAD is `6c04734393bf58a20adfe169f1904b29009206f2`; the documents include uncommitted revisions. Existing author dispositions are requirements added to the proposal, **not independent approvals or demonstrated fixes**.

Independently spot-checked the workspace transport emitter and generated hooks, RN transport/manifest, plan approval basis and promotion history contracts; inspected launcher/build command references and cut-journal scope. Desktop §5.3 was read as related architectural direction only. No installs, registry revalidation, suite, native build, device test, signing, upload or deployment was performed. This review certifies neither the candidate dependency matrix nor provider behavior. Previously filed defect IDs are recorded in the foundation audit; this review does not discover a new reproduced Forge defect requiring a duplicate report.

## Why this verdict

The plan has enough architectural direction to begin: retain Expo/Connect/Query/pnpm, fix the existing RN Frontend, share API and portable runtime contracts, keep native adapters and UI platform-owned, and prove a local iOS slice before remote breadth. The alternatives are realistic. A rewrite or universal publisher would increase uncertainty; implementing the complete release design immediately would freeze assumptions about stores, credentials and long-lived clients before evidence exists.

The chosen shared approach also has a cost: generator/runtime changes can break web and owned older apps. This is why mixed consumers and retained upgrades belong in the first work, not after mobile implementation. Additive subpaths are preferable to a package split only while graph and real consumer tests enforce isolation. Revisit that choice if measured isolation fails; do not assume tree-shaking supplies the boundary.

### Scope and order

- **Begin:** existing install/bootstrap/discovery/dev/config/validation repairs, one supported SDK selection experiment, and narrow native transport/lifecycle adapters.
- **Then:** production native auth with account isolation; complete F0 evidence. F0a is a usable development foundation, not an authenticated production app.
- **Next gate:** F1 contract fixture and local, OTA-disabled iOS/TestFlight acceptance. Minimal builder primitives need not wait for desktop implementation, every remote adapter, hosted grants, Android or native extras.
- **Later:** F2 store production lifecycle and independent binary health; F3 OTA only as a signed, recoverable, measured capability; F4 breadth; F5 Android native distribution. Keep Android schema and JS-export coverage early without claiming Android device support.
- Provider feasibility experiments may run ahead on isolated test resources once authorized. Their results inform contracts; they do not bypass foundation acceptance or authorize shipping.

## Priority findings and dispositions

### P1 — F0's combined acceptance is not fully represented by its phase exits

**New synthesis of acknowledged requirements; not a newly discovered code defect.** `mobile-production-readiness.md` §10 extends F0 with lifecycle, account isolation and migration fixtures. Release target §12 F0a primarily exits on stream/config/log/device behavior; F0b lists auth cases but does not explicitly close lost-response mutation handling, account-switch in-flight isolation, or upgrade preservation. Foundation audit §4 is broader still. Read literally, the short phase exits could pass while linked mandatory requirements remain unmet.

**Impact:** blocks declaring F0 complete, not starting its repairs. Use the work-package exits below as the combined checklist. Assign lifecycle/network behavior to F0a, auth/account isolation to F0b, retained migration fixtures to each affected change. Product queues remain app-owned; absence of a queue is a valid explicit default, not a reason to build a sync engine. Do not defer basic account isolation until after store release.

### P1 — Approval and execution reuse is feasible but not implementation-ready by assertion

**Already acknowledged:** release audit H2/M1; release target §7 “External resource ownership and approval basis” and “Promotion history and execution projection.” The revised desired/available/partial trace is coherent and addresses the original conceptual gap. It does not supply persistence, fencing or mixed-version support.

Independent source check: `pkg/release/plan.go:157–182` has promotion/config IDs and a drift boolean; finding Detail is not digested, while Fingerprint is. A new external snapshot needs deliberate digest and validation integration, not prose in a finding. `pkg/release/promotion.go:72–106,138–151` combines current binding/component selection with promotion-time approval and explicitly distinguishes promotion from deployment. Preserve those existing contracts; add operation/availability projections rather than change legacy meaning. `internal/cli/release_cut_journal.go` recovers a release cut, not store mutations.

**Gate before F1 durable publication implementation:** approve a small executable contract fixture covering approval → reservation → desired promotion → ambiguous upload → resume → partial availability → supersession, with named storage authority and fencing model. Demonstrate old readers and an older control plane cannot authorize unsupported mobile side effects. A local journal cannot acquire cross-machine guarantees. Single-writer standalone is a legitimate first restriction only if competing writers are actually refused within its declared deployment model.

### P1 — Concrete foundation failures justify implementation, not another inventory

**Already acknowledged:** foundation audit §1 and R1–R5; boundaries F1; lifecycle F1–F3. Independently confirmed `frontend_workspaces.go:407–430` reads transport during client construction and `frontend/hooks.ts.tmpl:85` constructs the client at module evaluation. Native registration occurs only after its root-package import (`react-native/src/lib/connect.ts.tmpl:55–62`). A transport-only import path does not alone guarantee later registration or replacement works.

The native manifest still has neither dev nor build (`react-native/package.json.tmpl:7–15`); launcher/build references execute those scripts (`up.go:3381`, `build.go:2386`). The runtime range remains `^0.2.0`. That range is source evidence; its current registry satisfiability was **not** independently queried here. Prior registry observations are enough to prioritize a fresh install fixture, not certify a replacement version.

**Disposition:** implement regression-backed narrow fixes now. Preserve start/custom scripts and any supported Expo web/static behavior. An explicit unsupported native artifact outcome is better than inventing signed-build semantics for generic frontend build.

### P2 — Dependency matrix is a candidate, not a compatibility decision

**Already acknowledged:** foundation audit §2 and dependencies audit uncertainty ledger. They correctly distinguish an internally coordinated older Expo family from a tested current matrix. Registry latest, an SDK tarball's bundled list, published runtime metadata, TypeScript success, Metro export and Hermes/native execution establish different things.

**Gate:** choose one supported SDK from fresh official metadata, retain exact resolved versions/lockfile/integrities and toolchain requirements, then execute consumer checks. Do not approve the table's Expo 58/React 19.3/RN 0.88 numbers merely because they are written down. Preserve React 18/19 support where publicly promised; do not impose React-major equality across all apps. No owner decision on every dependency patch is needed.

### P2 — OTA recovery terminology still overstates an unproved control

**Residual inconsistency in an acknowledged issue:** release target §8 says not to advertise a generic pause primitive and selects per-update rollouts, but F3's exit says “canary failure pauses rollout.” Release audit M2 already explains why abort, replacement, selection stop and cached-client recovery differ.

**Disposition:** F3 acceptance must name the demonstrated provider action and resulting reach, not require a fictional pause endpoint. This does not block F0 or store-only F1. Failure to prove retained-export publication/signing/recovery is a stop for the proposed OTA adapter, not permission to rebuild silently or weaken identity.

### P2 — Production evidence has a sound boundary, but owners must instantiate it

**Already acknowledged:** release target §§6,9,13 and F2; production readiness §§2–9. Source authorization, result authentication, credential isolation, external approval snapshots, old-client compatibility, pre-JS failure evidence and honest health uncertainty are substantive requirements. They are not supplied by shared interceptors, codesigning or a TestFlight upload.

F2's distribution-only fallback is acceptable only as an explicit limited capability. Full symbolication may follow minimum binary health; native failed-launch coverage may not be deferred behind OTA. F4's broad crash-reporting language must not undo F2's minimum gate. Keep `obs check` outcomes 0/1/2/8 intact; zero exposure, missing signals or waivers cannot produce 0. A provider may not furnish a complete failed-launch denominator: measure coverage and limitations, and remain unable to prove health when required coverage is absent.

**Gate:** assign operational evidence owners and concrete sample/window/baseline, consent, ingestion-abuse and supported-device policies before production claims. App performance, retention and old-client support windows are adopter decisions, not universal Forge constants.

## Safe first work packages and measurable exits

These are ordered implementation slices, not a demand for another omnibus design. Each owns only its relevant contracts and tests; run scoped fast tests while iterating and appropriate slow consumer lanes before claiming its exit.

| Package                                         | Work allowed now                                                                                                                                                                                                        | Measurable exit                                                                                                                                                                                                                                                                                                                                                                                        |
| ----------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| A — Pin failing contracts and protect v1        | Add executable bootstrap/transport tests, manifest-to-launch argv checks, config non-default fixture and raw rn/react-native consumer cases. Retain previous standalone/workspace/adopted fixtures with owned edits.    | Tests fail for the specific current mismatch; existing web exports/setters/scripts remain covered. Tests distinguish source emission from execution. Owned-file classification is explicit.                                                                                                                                                                                                            |
| B — Repair shared bootstrap and import boundary | Make transport resolution call-time, preserve public exports/setters, use existing portable error/interceptor seams. No universal runtime manager.                                                                      | Import before registration succeeds; an actual uninitialized call errors clearly; unary/mutation/server-stream calls use registered transport; swap behavior is documented and tested. Native value graph excludes browser-only modules; web consumer behavior still passes.                                                                                                                           |
| C — Repair native lifecycle/config delivery     | Normalize aliases at boundaries; fix dev argv/port contract, explicit build capabilities, discovery/adoption seam, one documented native raw-config source and precedence. Preserve owned app.config/native projects.   | Fake runner captures correct commands/ports; non-default API/auth/feature config agrees; missing/sensitive config refuses; existing custom scripts and static output survive. Native-only selected unsupported build fails before launching a missing script. Two regenerations converge without overwriting owned edits.                                                                              |
| D — Select and exercise one dependency matrix   | Isolated registry-only and packed-runtime consumers; standalone, mixed pnpm web/native, adopted app. Keep checkout bridge testing separate. Supply real native Jest/ESLint configuration and shared-package validation. | Fresh registry metadata and lockfile recorded; install/Doctor/typecheck/lint and at least one native unit test pass; every shared package compiles, including unused exports. Deliberately broken unused primitive/test fails the lane. iOS/Android Metro exports and advertised Expo web output pass without accidental sibling dependencies. This is JS/tooling evidence only.                       |
| E — Close F0a native behavior                   | Expo-compatible fetch, explicit advertised device origin, bounded redacted dev log forwarding, native focus/online adapters; define stream restart and ambiguous mutation defaults.                                     | Hermes unary JSON/binary, incremental UTF-8 stream and cancellation pass; simulator and physical iOS reach API; background/resume/network-change cases have bounded behavior. Lost-response write is reconciled or retried with server-supported identity, never blindly duplicated. JS log/rejection coverage is distinguished from native crashes. Missing device capacity is unavailable, not PASS. |
| F — Close F0b security and upgrade evidence     | Native system-browser PKCE, secure storage, refresh/revocation/logout, cancellation and identity-scoped cache/in-flight work. Migration tooling/diagnostics accompany relevant owned-code changes.                      | Real-device sign-in/restore/refresh/restart/logout, cancelled/replayed/wrong-state callback and concurrent refresh tests pass. Two accounts cannot read or replay each other's protected state; revoked/offline/storage-unavailable cases are explicit. Retained standalone/workspace/adopted/custom-native fixtures preserve edits and converge, then pass relevant bundle/behavior checks.           |

A and the isolated D feasibility fixture can begin immediately; B/C then feed the full D matrix and E. F builds on the stable config/transport/lifecycle contracts. Cheap Android schema/export evidence is not a substitute for later Android Hermes/storage/signing acceptance. E's mutation fixture proves the seam and default behavior, not idempotency of every application's RPC.

Emitter extraction (foundation R6) is optional enabling cleanup, not a prerequisite to all these repairs. Do it only where it reduces actual duplication under preservation tests. Do not let a template reorganization delay the regression fixes.

## Later-phase design gates versus real-provider experiments

| Boundary                            | Design/owner gate                                                                                                                                                                                                                                                                        | Executable acceptance or experiment                                                                                                                                                                                                                                                                                   |
| ----------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Before F1 contract freeze           | Minimal versioned job/result and mobile artifact schema; promotion/operation projection; reservation authority; v1 old-reader refusal; source authorization and first signing provider. Include dual-platform/store-only/update-only schema fixtures without implementing all providers. | Local pinned signed IPA, inspected native inputs/entitlements/trust and retained OCI dependencies. Isolated ASC upload interrupted at ambiguous boundaries resumes without duplicate submission; concurrent reservations conflict. Unknown external build is not accepted by version number alone.                    |
| Before remote protected credentials | Select trust/custody boundary and allowlisted runner/source policy; result authentication and credential grant scope. Coordinate shared primitives with desktop, not its entire roadmap.                                                                                                 | Cross-job/replayed result rejected; unauthorized source denied; install/plugin subprocesses and caches cannot access publisher/OTA keys. Where signing access is unavoidable, approved build code is explicitly trusted.                                                                                              |
| Before F2 production claims         | Candidate/prod identity, store metadata snapshot freshness/manual attestation, controller or honest CLI resume mode; binary health and incident policy.                                                                                                                                  | Same retained production-configured IPA promotes; staging variant refuses; console edits/supersession/partial state are visible. Native pre-JS crash cannot pass health; store-only recovery drill and release-mode/device/store checklist evidence recorded.                                                         |
| Before F3 public OTA contract       | Choose supported per-update rollout, trust partition, canonical resource locks, ingestion owner and health policy.                                                                                                                                                                       | Retained export published without changed assets; pin SDK/EAS CLI/account entitlement. Test signed active-rollout recovery on control/new/cached/offline clients, preserve provider evidence before deletion, prove stable cohort/health attribution and old-runtime retention. Failure narrows or stops OTA support. |

ASC correlation, review races, signing renewal, retained-export EAS semantics and failed-launch coverage are real experiments, not questions another prose audit can settle. Use named isolated apps/accounts; credentials or paid entitlements must be authorized before experiments. Do not test against unrelated production resources.

## Truly blocking owner decisions

**None blocks packages A–D or deterministic tests for E/F.** Implementation can use explicit unsupported/unavailable states instead of guessing production policy.

Before credentialed F0b/device auth acceptance: name the broker/native public-client registration, allowed callback identities and test environment; authorize device/test-account access. Before F1 signing/upload: name the first adopter, isolated ASC app/team, Mac capacity, credential custody/provider, approved source authority and standalone single-writer versus shared durable authority. These are access, ownership and trust decisions, not details an implementer should invent.

Before production: name source/deployment owners, candidate/preview topology, supported app/API/OS windows, device budgets, binary incident and health owners. Before OTA: approve provider/entitlement, canary strategy, trust/key custody and public ingestion ownership. These later decisions are not reasons to block the foundation repairs today.

## Planning to stop doing

- Stop another full mobile audit, speculative provider enumeration or package-version spreadsheet without a runnable fixture.
- Stop treating author responses, desktop plans, generated string tests or local runtime links as independent acceptance.
- Stop debating final CLI/KCL names before the narrow contract fixture; do review public names, unions, JSON/exit behavior and mixed-version refusal before publishing them.
- Stop designing a universal UI/lifecycle/offline framework or requiring remote fleet/Android/push/widgets/hosted updates before iOS foundation and store-only learning.
- Stop calling all unknowns implementation-start blockers. Keep explicit gates for the capability they constrain; do not turn a production checklist into prerequisites for writing the first regression test.

**Bottom line:** authorize bounded F0 implementation now. Require evidence-backed contract review before F1 durable publication, and provider/device/security/health acceptance before each later capability is advertised. No broad replanning round is warranted.
