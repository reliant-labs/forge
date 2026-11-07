# Mobile production-readiness contract

**Status:** proposed requirements; not implementation or certification.
**Date:** 2026-11-06
**Scope:** production behavior beyond successful mobile builds and publication.
**Related:** [release target](mobile-release-target.md),
[foundation audit and upgrade matrix](mobile-foundation-audit.md).

## Purpose

A successful build, TestFlight upload or OTA publication does not establish that
an application is production-ready. Mobile retains old clients, executes across
suspension and unreliable networks, persists device data, and depends on native
supply chains and store policies that web validation does not exercise.

This contract supplements the existing proposals rather than creating a second
release lifecycle. Its items are requirements to validate, **not newly verified
Forge defects**. No device tests or compliance assessment were performed to write
this document. Each adopter records supported behavior and evidence; no unchecked
box is implied to pass because another platform works.

## 1. Ownership and boundaries

| Forge guarantees                                                         | Application owns                                  | Jointly validated                               |
| ------------------------------------------------------------------------ | ------------------------------------------------- | ----------------------------------------------- |
| Immutable build/release provenance and compatible configuration delivery | Supported offline product behavior                | Old-client/backend compatibility                |
| Shared transport/error/idempotency integration seams                     | Which operations can queue or retry               | Background/resume/network transitions           |
| Native auth/storage lifecycle seams and safe logging defaults            | Sensitive-data classification and retention       | Logout/account isolation and revoked sessions   |
| Test integration, upgrade tooling and capability diagnostics             | UX, accessibility and product performance budgets | Real-device and release-mode readiness          |
| Native artifact inspection and store adapter evidence                    | Truthful privacy/legal/store declarations         | Store requirements and third-party SDK behavior |

Avoid a universal app lifecycle manager or a general-purpose offline engine.
Share transport/config/release contracts; expose small platform adapters for
storage, focus/connectivity, authentication and native evidence. Product-specific
policy stays explicit and app-owned.

## 2. Long-lived clients and backend compatibility

Declare supported app versions, OS versions and native runtimes separately. An
OTA runtime fingerprint is not an API compatibility version. Retiring one runtime
assignment does not prove its installed clients have disappeared.

Requirements:

- Preserve API compatibility for the declared support window; test older generated
  clients against a new backend, including unknown enum values and optional fields.
- Define capability negotiation for features that old clients cannot execute.
- Define minimum-version enforcement, availability of the upgrade link and offline
  behavior. Never use a blocking screen as the only protection for breaking APIs.
- Keep backend/database rollout sequencing compatible with both current and
  supported older clients. Feature-disable controls must have documented defaults
  when remote configuration is unavailable.
- Record runtime/version retirement as a reviewed policy change with reach
  uncertainty, user communication and retained recovery artifacts.

**Acceptance:** a retained old-client fixture exercises critical auth/read/write
flows against the candidate backend; unsupported features fail explicitly; minimum
version and missing-network cases do not loop, crash or erase user data.

## 3. Suspension, connectivity and operation semantics

Specify behavior for backgrounding, suspension, process death, network handoff,
offline operation, resume and expired credentials. Distinguish cancellation of a
client request from cancellation of work already accepted by the server.

Requirements:

- Stream reconnect/resume defines replay, ordering and deduplication behavior; do
  not assume a restarted stream continues where it stopped.
- Mutations with ambiguous outcomes reconcile with the server or use the same
  server-supported idempotency key. Reconnect must not blindly duplicate writes.
- An intentional new user operation receives a new identity; retries preserve the
  previous operation identity and its server-defined scope/expiry.
- Query focus/online signals use native lifecycle adapters rather than DOM events.
- Credential refresh is single-flight; account changes cannot replay queued work
  under another user. Expired/revoked offline sessions have an explicit policy.
- Product-owned queues, if any, specify ordering, persistence, expiry, conflict
  resolution and user-visible failure. Forge need not supply a generic sync engine.

**Acceptance:** background/kill during a stream and mutation, then resume on a new
network; verify no duplicate writes, defined stream recovery, bounded retries and
correct identity. Test a server commit followed by a lost response.

## 4. Device data security and account isolation

Secure token storage alone does not establish secure application persistence.
Inventory tokens, query caches, attachments, queued operations, logs and backups.

Requirements:

- Choose storage/retention by data classification. Do not persist every query by
  default or treat AsyncStorage as encrypted secret storage.
- Namespace cached/persisted data by authenticated identity and environment;
  logout/account switching cancels or isolates in-flight work and clears or makes
  inaccessible the previous user's protected state.
- Test backup/restore, reinstall and secure-storage availability semantics on each
  supported OS. Document what survives logout, app deletion and device migration.
- Handle refresh-token revocation, device lock and unavailable secure storage
  without infinite login loops or silent use of another account's credentials.
- Redact credentials and sensitive content in console/crash/telemetry payloads.
  Review clipboard, screenshots/app-switcher previews and local attachments when
  the application's sensitivity requires controls.
- Define deletion for locally stored user data and communicate limitations for
  offline devices and OS-managed backups.

**Acceptance:** two accounts used consecutively on one device cannot observe each
other's protected cached data or execute each other's pending writes. Inspect
persisted files, logs and representative backup/restore behavior using test data.

## 5. Native supply chain and build inputs

The JavaScript lockfile is only part of a native build's dependency graph.

Requirements:

- Record CocoaPods/Swift package inputs, Gradle dependencies/plugins and relevant
  lock/verification metadata alongside npm/pnpm inputs and config plugins.
- Pin and record effective native toolchain versions. Identify unavoidable mutable
  remote inputs and refuse to call the result fully reproducible without evidence.
- Inspect generated native changes and SDK/plugin compatibility, including the
  chosen React Native architecture. Never assume a successful Metro bundle proves
  native linkage or extension compatibility.
- Include native dependencies in license/security inventory where supported;
  missing scanner coverage is named, not reported clean.
- Keep dependency installation and executable config/plugin code away from
  publisher/OTA credentials. Apply the release proposal's approved-source and
  authenticated builder-result boundaries.

**Acceptance:** artifact provenance names effective native inputs/toolchains; a
changed native dependency affects build identity; a missing lock or unsupported
scanner yields an explicit limitation. Test protected credential isolation across
subprocesses and caches.

## 6. Upgrading existing Forge applications

New templates do not repair existing apps. Each foundation change includes a
migration story and ownership classification.

Required fixtures:

1. Previous Forge standalone mobile scaffold.
2. Previous mixed web/mobile workspace scaffold.
3. Adopted Expo app with owned configuration/transport.
4. App with committed/custom native projects or config plugins.

For each, distinguish generated output, scaffold-once owned code and app-specific
configuration. Use additive contracts and migration playbooks/codemods where
appropriate. Preserve existing v1 paths and custom scripts; do not overwrite owned
files to obtain a passing upgrade. Detect manual conflicts with actionable output.

**Acceptance:** upgrade each retained fixture, regenerate twice, preserve owned
edits, verify convergence, then compile/bundle and run relevant behavior tests.
A fresh-scaffold PASS does not count as upgrade evidence. Existing app SDK upgrades
follow the supported migration sequence rather than an untested major-version jump.

## 7. Store policy and artifact suitability

Forge inspects evidence and required inputs; it does not certify legal compliance
or replace the application's declarations.

Readiness includes:

- Apple privacy manifests and required-reason API declarations from the app and
  included SDKs, permission descriptions and privacy disclosures.
- Export compliance, account agreements and review information as applicable.
- Current Android target SDK/store deadlines and supported native architectures.
- Third-party SDK and extension requirements for the chosen store/toolchain.
- Confirmation that an OTA change is permitted by applicable store rules and does
  not replace a required reviewed binary release.

**Acceptance:** the approved release links an artifact inspection report and
human-owned declaration/checklist evidence. Missing/inaccessible console state is
unknown or a named manual checkpoint, not automatically compliant. Preserve the
release proposal's external-state approval snapshots and residual-race disclosure.

## 8. Release-mode quality and supported devices

Declare a modest supported OS/device matrix, including minimum supported versions
and at least one lower-resource Android profile when Android is supported.

- Measure release-mode startup, responsiveness, memory behavior and large-list/
  streaming UI performance against app-owned budgets.
- Exercise VoiceOver/TalkBack, text scaling, focus order, reduced motion and
  meaningful loading/error/offline states.
- Validate deep links and authentication callbacks on real devices, including
  cold start and an already-running app. Domain association and callback
  validation must agree with the signed binary's identity.
- Test relevant permissions denied/revoked and supported OS upgrade behavior.

**Acceptance:** record device/OS/build mode and measured results; debug simulator
performance is not release-mode evidence. Accessibility checks include actual
platform assistive technology, not only static lint.

## 9. Evidence gates and scheduling

Use the existing Forge test tiers. Do not put every real-device scenario on the
merge tier. Required capabilities still need explicit evidence before claiming
production readiness.

| Gate                         | Evidence                                                      | When                                                   |
| ---------------------------- | ------------------------------------------------------------- | ------------------------------------------------------ |
| Foundation conformance       | Config/transport/lifecycle contracts and native test harness  | Foundation F0                                          |
| Upgrade compatibility        | Retained standalone/workspace/adopted/native-project fixtures | Each relevant upgrade                                  |
| API compatibility            | Supported old-client fixtures against candidate backend       | API/backend compatibility changes                      |
| Native runtime               | Lifecycle, auth, storage and networking device scenarios      | Before production readiness; relevant changes          |
| Artifact suitability         | Native build/provenance, signing, privacy/store checklist     | Binary release                                         |
| Release-mode quality         | Device matrix, accessibility and performance budgets          | Initial production readiness and risk-based regression |
| Distribution health/recovery | Store/OTA evidence and incident drills from release proposal  | Respective distribution capability                     |

Record each required capability as passed, failed, unavailable or explicitly
waived by an authorized owner, with scope, artifact/source identity, tool/device
versions, evidence timestamp and expiry/revalidation trigger. A waiver is not a
PASS and must not cause `obs check` to claim proven health. TestFlight observations
remain distinct from App Store exposure.

## 10. Adoption sequence

1. Extend foundation F0 with lifecycle, secure account isolation and migration
   fixtures while repairing the verified audit blockers.
2. Before initial production distribution, establish old-client support policy,
   native build inventory, store checklist and release-mode/device baseline.
3. Bind binary/OTA rollout claims to the corresponding release health and recovery
   gates; do not defer native failed-launch evidence behind optional OTA adoption.
4. Expand Android and native-feature coverage as supported capabilities ship.

The first implementation review should assign concrete owners and supported
version/device windows. This document does not invent universal retention periods,
performance thresholds or store-policy guarantees for every Forge application.
