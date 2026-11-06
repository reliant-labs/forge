# Mobile apps as a Forge release target

**Status:** proposal; no functionality described here is implied to have landed.
**Date:** 2026-11-06
**Scope:** Forge mobile development, builders, release artifacts, distribution,
observability and the control-plane contracts they require. Expo/React Native
first; iOS first, followed by Android.
**Related:** [Desktop release target](desktop-release-target.md),
[mobile foundation audit](mobile-foundation-audit.md), and
[mobile production-readiness contract](mobile-production-readiness.md).

Production readiness also requires the linked contract's old-client compatibility,
application lifecycle, device data isolation, upgrade fixtures, native supply-chain,
store-policy and release-mode device evidence. Successful publication alone does
not satisfy those gates.

## Summary and decision

Make mobile a first-class Forge release target, with the same guarantees as web:
declared inputs, useful local development, immutable releases, reviewed deployment
plans, promotion by digest, observable outcomes and a recovery path. Do not pretend
that a store submission behaves like replacing a web deployment. Phones retain
old binaries, stores process and review asynchronously, and OTA changes a second
software layer independently of store distribution.

The core model is **an immutable release plus reconciled distribution state across
multiple installed runtimes**. Forge owns intent, identity, approval and evidence;
Expo, Xcode, Gradle and store services remain the actual tools and authorities for
their operations.

Recommended decisions:

1. Introduce `MobileApp`, preserving existing React Native frontend behavior.
2. Share builder transport, immutable inputs and artifact assembly with desktop;
   keep mobile packaging and publication state machines separate.
3. Record binary identity, runtime compatibility and OTA identity separately.
4. Model a channel environment as an audience's desired store publication and
   **runtime-indexed** update assignments, not one currently installed binary.
5. Use a production-configured TestFlight candidate for byte-preserving App Store
   promotion. A staging-backend build is a different variant, not that candidate.
6. Persist deployment operations and reconcile partial results. Submitted,
   available and proven good are different states.
7. Ship a narrow end-to-end iOS path before adding the full builder fleet. Enable
   OTA only with signing, cohort routing, recovery and minimum health evidence.

## 1. Baseline and intended parity

The initial review identified Expo scaffolding and generated Connect hooks, but
no integrated mobile release target. Its reported baseline included Expo SDK 52,
React 18, non-streaming global fetch, a mock auth provider, missing RN runtime
config/log capture and scenario exclusions. These are **baseline observations to
revalidate against the implementation branch**, not permanent claims about main.
Choose SDK/RN/React versions from the supported Expo compatibility matrix when
implementing; do not encode a guessed future SDK number into this proposal.

Mobile parity means parity of guarantees, not identical rollout mechanics:

| Capability     | Required mobile behavior                                                      |
| -------------- | ----------------------------------------------------------------------------- |
| Scaffold/adopt | Discover an Expo app without replacing its owned configuration                |
| Dev            | Device-reachable API, streaming transport, typed config, logs and native auth |
| Test           | Unit/typecheck/lint and affected-package coverage; explicit native e2e lane   |
| Build          | Pinned source/config/toolchain; verified signed output; build provenance      |
| Release        | Immutable OCI artifact, retained dependencies, verifiable identities          |
| Plan           | Audience/config/signing/runtime/permission changes; exact publication scope   |
| Deploy         | Approval-bound durable execution, idempotent retries and reconciliation       |
| Promote        | Same bytes only when target identity, configuration and routing agree         |
| Status         | Desired state, external state, pending review, rollout and uncertainty        |
| Observe        | Release attribution, sample-aware health evidence and symbolication           |
| Recover        | Stop rollout, restore compatible update assignments, incident evidence        |

## 2. Alternatives and trade-offs

### Pipeline ownership

- **EAS as the system of record:** operationally convenient, but separates Forge
  release approval and provenance from what users receive. Not the target model.
- **Generated vendor workflow:** useful as a thin execution shim, not as the only
  place build or distribution semantics live.
- **Forge orchestration with vendor adapters (chosen):** more lifecycle work in
  Forge, but one release history, plan and recovery model. EAS Build/Update remain
  valid adapters; using them is not a failure of the architecture.

### Release representation

- **One mandatory fresh binary plus update per release:** simple-looking, but
  incorrectly couples JS delivery to store build numbers.
- **Unrelated binary/update ledgers:** flexible, but loses explicit compatibility
  and provenance links.
- **One release envelope with explicit binary references and update records
  (chosen):** retains one reviewable artifact while representing update-only cuts.
  Costs: reference retention and richer compatibility checks.

### Audience routing

- **Staging TestFlight binary promoted to prod:** rejected when endpoint, identity
  or embedded routing differs; no digest-preserving transformation exists.
- **Production candidate tested in TestFlight (chosen default):** same binary can
  reach the App Store, but TestFlight alone does not isolate OTA candidates.
- **Runtime channel switching:** possible for supported Expo versions, but requires
  an explicit authenticated cohort-routing policy. Not an implicit first-cut fix.

### Shared core

Build common immutable job/result and transport primitives with desktop. Do not
require every remote adapter, or a universal app publisher abstraction, before a
local Mac can produce a ledger-backed TestFlight release. Conversely, do not ship
an opaque shell escape hatch as the permanent release model.

## 3. Identities and invariants

### 3.1 App and variant identity

App identity includes platform, bundle/application ID, Apple team and applicable
installed signing identity. Android upload keys and installed-app signing keys
are different concepts; upload-key rotation must not look like changing installed
app identity. Supported signing-key rotation/lineage requires platform-specific
validation rather than a blanket certificate-equality stop.

A **variant** identifies effective public embedded configuration: backend URLs,
public client keys, native identifiers, embedded OTA endpoint/channel and trust
configuration. Hash canonical resolved configuration, not merely the user-written
`build_env` map. Reject missing or ambient undeclared inputs.

Secrets needed by build tools are not client secrets. Public DSNs/client keys go
in public configuration. Credentials for symbol upload, signing and dependency
fetching must not enter the binary. If an exceptional credential can influence
output, capture a non-secret immutable version reference in build provenance;
never publish a raw or guessable secret hash as configuration identity.

### 3.2 Three different identities

| Identity              | Meaning                                                                                                   | Does not establish                                  |
| --------------------- | --------------------------------------------------------------------------------------------------------- | --------------------------------------------------- |
| Runtime fingerprint   | Compatibility between native runtime and OTA code                                                         | Equality of store binaries                          |
| Binary input identity | Pinned source, resolved native config, embedded JS/assets, toolchain, version/build, signing/trust inputs | Bit-for-bit reproducibility of a fresh signed build |
| Output digest         | Exact bytes that Forge retained and published                                                             | Store-generated delivered bytes after processing    |

Use Expo fingerprinting, pinned with its tool version/configuration. Compute and
verify it against the resolved inputs used by both build and export. Do not claim
that fingerprinting proves arbitrary application semantic compatibility.

Reuse a previously verified binary only when the requested binary inputs match,
or an update-only intent explicitly references it and passes compatibility checks.
A new store version, embedded-JS refresh, changed signing/trust/configuration, or
explicit binary-build request can require a new binary without a changed runtime.
Rebuilding signed outputs may produce different bytes; retries reuse an accepted
result, not an assumption of deterministic signatures.

### 3.3 Version and build numbers

Release ID, store marketing version, per-platform binary build number and OTA
update ID are distinct. Read source-declared versions and platform-valid build
numbers by default. A package.json version alone does not supply an iOS build
number or Android versionCode; the packager declares their authoritative source.

- New binary allocation must obey platform/store uniqueness and ordering rules,
  across relevant tracks and Forge operations, not just one env's live pointer.
- Reserve the app/platform/store build identity durably before dispatch. Competing
  cuts cannot both claim it; failed/unknown allocations are not silently recycled.
- An accepted build identity maps to exactly one retained binary digest. A
  conflicting rebuild cannot replace it.
- Reusing a binary or promoting it to another track needs **no increment**.
- Update-only cuts need **no new store build number**.
- Ledger-derived allocation is a later option with the same reservation and
  external-store reconciliation rules, not a local counter flag.

## 4. Releases and installed runtimes

`mobile.v1` is an immutable OCI release envelope, carried through the existing OCI
artifact path. Its versioned config records:

- App/source pin, effective configuration/variant identity and Forge version.
- Per platform: runtime fingerprint and its computation provenance.
- Binary: newly built or immutable reference; digest, size, marketing version,
  build number, minimum OS, embedded update routing/trust, signing verdicts,
  permissions/entitlements, builder/toolchain provenance and symbol references.
- Updates: platform/runtime target, exported launch asset/assets, hashes, update
  signing certificate identity and source maps. Provider-created IDs/signatures
  whose final form depends on publication are recorded in deployment evidence.
- Explicit intent: store binary, updates, or both. Native changes cannot be
  represented as compatible update-only delivery.

A store-only release need not configure OTA. An update-only release references a
verified compatible binary/runtime; it does not fabricate a binary build. The
embedded JS already inside a binary remains part of its immutable bytes.

Layers hold binaries, exported bundles/assets and symbols. Immutable references
must remain resolvable: retention/GC traverses binary, symbol and historical
assignment dependencies. Cross-repository blob mounts are an optimization only;
copy verified blobs when mounting is unsupported. Production cuts fail if required
referenced content is missing.

Cut invariants: declared-platform completeness for the requested intent; valid
binary references; reserved build identities for new binaries; runtime agreement;
verified signing; exact variant identity; complete export assets. Unsigned builds
are simulator/dev artifacts and cannot enter store publication.

### Signing-trust partition invariant

Every binary sharing an external OTA selection key must have compatible embedded
verification policy. Changing or removing the update certificate requires a new
runtime/routing partition unless the adapter proves compatible delivery to every
retained population. Record and inspect the certificate actually embedded in
adopted native projects, not just the Expo config. Preserve old-key runtime
assignments during rotation; lost, compromised or expired trust may require a
corrective store binary rather than an OTA fix. Fingerprint equality never waives
this invariant.

### Runtime-indexed desired state

An audience environment contains:

- Desired store build/version and publication settings per platform.
- Update assignments keyed by platform and runtime, with rollout policy.
- Retained supported runtime inventory and policy for retiring it.
- Observed store and update-provider state, plus timestamp/uncertainty.
- Observed install/adoption cohorts, explicitly incomplete.

Publishing an update for runtime B does not delete runtime A's assignment.
Removal/retirement is an explicit planned change. Installed cohorts are not
inferred solely from the store's latest build. No telemetry is not proof that no
installs exist. TestFlight expiry, OS eligibility and minimum-supported policy
also affect reach.

## 5. Environments and promotion

Default audience layout:

| Env                | Backend/config                       | Distribution                | Promotion relationship                    |
| ------------------ | ------------------------------------ | --------------------------- | ----------------------------------------- |
| `mobile-preview`   | Staging, optionally separate app ID  | Internal/TestFlight preview | Not byte-promotable to prod configuration |
| `mobile-candidate` | Production configuration and routing | Production app's TestFlight | Same binary may be promoted to App Store  |
| `mobile-prod`      | Same production variant              | App Store                   | References candidate binary digest        |

An environment is the audience's deployment binding, not a guarantee of exclusive
OTA delivery. **The default production candidate and prod share embedded update
routing. Do not publish a candidate OTA update there expecting TestFlight-only
reach.** Store candidate testing exercises its embedded bundle; if it fetches the
shared OTA channel, status/tests must disclose that fact.

For OTA canaries choose explicitly:

1. A separate preview binary with equivalent native runtime and different embedded
   channel, exporting the exact production-configured update payload for testing;
   only re-publish identical assets when provider semantics support it.
2. A provider-supported production rollout with stable cohorts and health gates.
3. Later, authenticated installed-client cohort routing, separately designed.

Different channels need not make runtime fingerprints different, but their routing
is still part of binary identity. Never weaken fingerprint checks just to force a
promotion. Promotion refuses variant/trust incompatibility and names the differing
inputs. A release containing several variants selects an already-built target
variant; it does not rebuild during deploy or claim cross-variant byte equality.

## 6. Build execution and security

Use a shared versioned builder protocol: secret-free immutable spec, verified
inputs, platform capability requirements, output digests/provenance and a durable
job identity. Mobile payloads add Expo export/native config, expected runtime,
platform, distribution method and signing requirements.

- Local macOS first for iOS; then the selected macOS remote adapter.
- Pin Node/package manager/lockfile, Expo/fingerprint tooling, Xcode/macOS SDK or
  Android SDK/NDK/JDK/Gradle as appropriate. Record effective versions.
- Run Expo prebuild/Xcode/Gradle; Forge never interprets their input formats as a
  substitute for invoking them. Respect owned native projects/config plugins;
  `prebuild --clean` is valid only in an isolated generated-native build workspace.
- Export OTA under the same resolved environment and verify compatibility against
  its binary. Exercise Hermes export/source-map composition on real devices.
- Verify nested extensions, profiles, entitlements, IDs, minimum OS and signing
  verdicts, not just the outer IPA filename.

### Source authorization and result provenance

Production signing authorizes a specific source pin and job digest, not merely a
trusted runner. Protect source approval and dependency/config-plugin inputs.
Authenticate builder results with a runner-bound signed attestation or equivalently
verified authenticated result channel, covering job/spec digest, input/output
digests and toolchain. Reject cross-job substitution, stale credentials and replay
outside the accepted job result. Valid codesigning alone is not source provenance.

Separate dependency installation/compilation from protected signing and publication
where supported, with a verified immutable unsigned-input handoff. App build code
that necessarily runs with signing access is inside the trusted boundary and must
receive corresponding approval. Publisher and OTA private keys never enter install
or compilation subprocesses. Test subprocess/cache exfiltration boundaries, not
only redaction of serialized job specs. These are shared builder requirements to
resolve with desktop before either exposes production credentials.

### Credentials

Every credential declaration is a secret reference. Builders and publishers get
only their required names, with provider-specific authentication and least
privilege. No untrusted PR job can redeem production signing credentials. Jobs
cannot select arbitrary protected environments or runner labels outside the
allowlisted declaration. Registry access is scoped to the job's inputs/outputs.

App Store Connect API authorization is separate from distribution signing:
manual signing uses a distribution private key/certificate plus provisioning
profiles; automatic signing needs an explicit supported lifecycle for creating,
fetching and renewing those assets. Use isolated temporary keychains/workspaces,
redacted logs, cleanup and no signing keys in artifacts/cache.

For remote `ExternalSecrets`, values exist on the trusted runner/publisher; a
missing name fails preflight. Hosted secrets require scoped, expiring grants bound
to job digest, environment and verified runner identity before that provider is
supported. Do not promise hosted credential delivery based on the desktop proposal
alone. EAS-managed credentials are an explicit external custody choice, recorded
in provenance; downloadable artifact verification is not build attestation.

OTA requires an embedded public trust certificate, publisher-only signing key,
expiry monitoring and a rotation plan. Distributing new trust typically requires a
new binary. Lost/expired keys and compromised keys have documented store-release
recovery paths; changing servers cannot replace installed trust.

## 7. Durable deployment and publication

Reuse release approval, CAS and append-only history. Add durable **execution
operations**; external side effects cannot be made atomic by a ledger append.

A deployment stores the exact approved plan digest, release/artifact references,
prior and desired assignment revisions, idempotency identities, actor, external
IDs, checkpoints, observations and errors. Reserve operation intent before a side
effect. Progress is separate from the last successfully available binding.

Representative states:

`planned → approved → executing → submitted → available → evaluating → proven_good`

Additional states include `awaiting_review`, `partial`, `blocked`, `failed`,
`superseded`, and health `insufficient_data`/`not_proven_good`. Components progress
independently; these are not one mandatory linear store-plus-OTA state enum.

### Reconciliation rules

1. Resume by operation identity. Reobserve external state before retrying an
   ambiguous upload/submission; never blindly submit a second version.
2. Only one active mutation owns a particular app/store publication or
   audience/platform/runtime assignment revision. Conflicting work waits or is
   superseded explicitly. Store API transaction/etag facilities are used where
   available; Forge CAS alone does not serialize outside actors.
3. Persist provider IDs and verified upload provenance. Stores may not expose
   the original upload digest and may transform/re-sign binaries. Unknown external
   builds are `unverified`, not falsely declared byte-identical. Adoption needs
   explicit evidence/approval or a new known build.
4. Store success plus OTA failure is `partial`. Retry the unfinished component;
   do not roll back an already released store binary as compensation.
5. A newer approval can supersede queued work. Recheck ownership before each
   mutating step; already submitted review cannot be erased from history. Cancel
   where supported, and prevent stale automatic release where possible. Report
   externally unavoidable completion as drift, never current desired success.
6. Reconciliation continues via a supported durable controller/job runner, or an
   explicit resumable CLI in standalone mode. The first slice may use CLI resume;
   it must not imply background progress without a configured runner.
7. `env status` distinguishes freshness, external drift, unknown state, review
   blockers, required human store actions and the last verified release.

### External resource ownership and approval basis

Mutation ownership follows canonical **provider resources**, not env names:
account/project/app, store version/track, update channel/branch and platform/runtime
as appropriate. Reject unacknowledged aliases across apps/envs. Candidate and prod
sharing one branch cannot independently own that assignment. A channel remap or
branch rollout locks and plans every affected runtime; a runtime-scoped approval
cannot authorize a channel-wide mutation.

Approval includes a content-addressed snapshot/revision of observable external
build attachments, metadata/review scope, release controls and channel mappings,
with timestamps and a bounded freshness policy. Reobserve before mutation; relevant
changes require reapproval. A drift boolean or an undigested finding description
is insufficient. Where APIs cannot fence console edits or expose complete privacy
state, require a named human attestation/manual-release checkpoint, disclose the
remaining race and preserve submitted-state evidence. Do not claim atomic control
over an externally mutable console.

### Promotion history and execution projection

Worked trace: approval at desired revision D0 reserves operation O1 and app/build
identity through the shared authority; a CAS promotion records desired D1 and its
approval before upload. Upload/processing checkpoints attach to O1, not a fictional
available binding. If iOS becomes available but Android fails, desired stays D1,
per-platform availability shows iOS D1/Android D0 and operation state is partial.
A new approved D2 supersedes O1's remaining mutations; O1's already available or
submitted side effects remain observed history and may require reconciliation.
Health evidence attaches to the actual available component/cohort, never merely D1.

Existing promotion readers continue to mean desired binding, as documented; mobile
status/health need new availability and operation projections. Do not reinterpret
legacy JSON fields as proof of availability. App/build reservations and resource
leases live in a shared durable ledger authority with fencing, not only a local
cut journal. Standalone operation uses an explicitly configured shared authority
or a documented single-writer mode that refuses concurrent writers; it must not
promise multi-machine concurrency safety from a local lock.

### Store adapters

App Store Connect: upload with a supported Apple transport; observe processing;
assign internal/external TestFlight groups; track Beta App Review where required;
attach a known build to a store version; submit; observe review; release manually
or automatically and track phased availability. Validate required pre-existing
metadata, agreements/export compliance and access before upload where possible.
A submitted review is not an available release, and phased release is not a
precisely controlled percentage of all installed users.

Google Play: edit/commit lifecycle, track identifiers, versionCode validation,
rollout/halt controls and policy/review state. Separate upload signing identity
from Play's app-signing identity/rotation. Do not label Play production submission
immediately available.

Store metadata stays in store consoles initially. Plans link missing requirements;
permissions/privacy changes cannot be silently waved through by a successful upload.
Later metadata automation should use versioned files/adapters rather than large
marketing blobs in KCL.

## 8. OTA publication and recovery

The first OTA adapter may use EAS Update. It must prove it can publish a retained
export without silently rebuilding different JS or substituting ambient EAS env
values. Persist provider update/group IDs, runtime, asset digests, signing
identity, rollout and channel/branch mapping. Provider-specific final manifests
and signatures are immutable deployment evidence linked to the release.

Changes to rollout percentages, channel mappings or embedded-fallback directives
are new approved deployment operations. Status shows desired assignments and
observed served groups for each supported runtime, including partial publication.

Pre-seeding a runtime before a compatible binary is available requires a scoped
plan acknowledgement; never reuse a generic destructive-action flag for every
mobile exception. Update-only publication of native-incompatible changes is a
hard refusal. Runtime targeting is not a way to bypass store policy: OTA may not
ship changes prohibited by platform rules or substitute for required review.

Recovery is a new append-only operation that restores a previous compatible
update or issues the provider's supported embedded-fallback directive. Merely
deleting an update does not ensure cached devices stop running it. Record scope,
provider support and limitations for offline clients, cached updates and restarts.
Embedded fallback may differ for binaries sharing a runtime; the plan shows which
embedded versions can result. Never guarantee recovery from every launch crash or
assume Expo's error recovery protects every failure mode.

The first EAS adapter uses per-update rollouts, not branch-based rollouts. Probe
and pin CLI capabilities and account entitlements, including paid signing support
where required. Do not advertise a generic pause primitive: specify whether a
control stops selection, aborts a rollout or republishes control. Replacing an
active rollout may end it and expose the replacement to all matching clients;
reverting may delete provider update groups. Snapshot IDs/manifests/signatures and
observations before destructive provider actions. Approval states the resulting
reach. Test control, newly selected and already cached/launched clients on real
devices, including recovery interrupted by another CLI failure.

The OTA milestone requires tested stable-cohort rollout, supported abort controls,
signed republication, embedded recovery and minimum release-attributed health signals.
Hosted updates is a later independent service: protocol conformance, immutable
asset serving, trust/signatures, caching, rollout assignment, availability,
retention and incident ownership all need a design and operational tests.

## 9. Plans, approval and health

Plans include exact platform/store/runtime scope, identities/variant differences,
new versus reused binaries, expected reach with uncertainty, retained assignments,
review requirements, rollout, signing expiry, permissions, minimum OS and recovery.

| Finding                                           | Default action                                    |
| ------------------------------------------------- | ------------------------------------------------- |
| Variant/app identity mismatch                     | Stop; build/select matching artifact              |
| Build number conflict for a new binary            | Stop before expensive dispatch                    |
| Native-incompatible update-only publication       | Stop                                              |
| Missing/invalid signing or untrusted update key   | Stop                                              |
| No known compatible distributed binary            | Stop; scoped pre-seed acknowledgement possible    |
| Removing a supported runtime assignment           | Stop pending explicit scoped acknowledgement      |
| New permissions/entitlements or raised minimum OS | Warn, show review/privacy impact                  |
| External build with insufficient provenance       | Stop promotion pending explicit adoption evidence |
| Insufficient mobile health sample                 | Not proven good; never silently pass              |

Finding/public JSON additions must be coordinated with control-plane consumers;
version payloads and test mixed-version refusal/negotiation. Do not assume adding
closed-set values is automatically backward compatible in v1. New lint rules use
the normal warning-before-gating rollout.

### Binary incident recovery

A store-only release has its own recovery runbook: halt/phased-release controls
where supported, disclose that manual downloads and already installed binaries
may remain affected, allocate a corrective new build/version and submit it.
Expedited review is a request, not a latency guarantee. Backend compatibility and
preplanned feature-disable controls may mitigate impact; removing store listing
availability does not restore installed bytes. F2 acceptance includes a reviewed
incident drill plus adapter fixtures, with demonstrated controls separated from
uncontrollable production review timing. Plans/status expose these limits even
when OTA is disabled.

### Health semantics

Binary health is independent of OTA enablement. Before claiming production parity,
require native pre-JS crash/failed-launch evidence as well as JS session signals,
with an explicit policy for denominator, exposure window, baseline and coverage.
Missing native evidence cannot prove native health. TestFlight pre-release evidence
is labelled separately from post-release App Store exposure. A distribution-only
milestone may ship earlier, but `obs check` then reports incomplete coverage rather
than proving the whole binary good.

Minimum OTA instrumentation: sessions/launch attempts, JS fatal errors, update ID,
binary build, runtime, variant and channel/cohort attribution. Crash-free metrics
must account for failed launches; reporting only successful next launches hides
users who never reopen. Native crash reporter integration, native symbolication
and Hermes composed source maps are explicit adapter work, not consequences of
adding an OTel API dependency.

Treat device data as untrusted. A mobile binary cannot hold a secret ingestion
credential: use a scoped public ingestion path with validation, rate limits,
payload/PII controls and abuse budgets. No privileged backend OTLP credential is
baked into an app. Consent, retention and resource attributes must avoid tokens,
message bodies and user-identifying metadata by default.

`obs check` preserves the existing exit contract: 0 proven good, 1 regression,
2 unable to prove, 8 insufficient data. Compare equivalent cohorts and elapsed
exposure, with minimum sample/window and late-event handling. No installs, pending
review or missing telemetry cannot produce 0. App Store release availability and
health remain separate evidence. Gates freeze evidence and its limitations at
the approved production promotion; they cannot guarantee later mobile behavior.

## 10. Declarative surface and CLI direction

This is a **schema direction**, not a final exported KCL contract. Validate a
fixture end to end before freezing names and unions.

- `Bundle.mobiles`: named `MobileApp` declarations; exactly one local path or pinned
  Git source, Expo packager, per-platform identities/builders/signing, public
  embedded config and update trust policy.
- Runtime bindings: `OnHost`, `BuildOnly`, or `MobileChannel`.
- `MobileChannel`: **per-platform** store bindings (iOS ASC and Android Play can
  coexist), plus platform/runtime update assignments and supported-runtime policy.
  Not a single `ASC | GooglePlay` field that prevents both platforms in one env.
- Store intent: requested source-declared marketing/build identities and new-build
  or verified-reference policy. Distribution method is explicit; simulator/ad-hoc
  artifacts cannot be submitted as store binaries.
- Provider adapters: separate builder, store and update concerns; secret references
  and public identity identifiers are distinct fields.
- Links: explicit reviewed domains/path rules and web binding. Expo route discovery
  may suggest paths, but must not publish every internal route automatically.
- `min_supported`: an optional app/API policy recipe, not permission to break old
  backend contracts or assume every device has received an update.

Illustrative commands, **not existing CLI claims**:

```text
forge env up dev
forge env build mobile-candidate --release mobile-1.2.0 --plan
forge env build mobile-candidate --release mobile-1.2.0
forge release verify mobile-1.2.0
forge env deploy mobile-candidate mobile-1.2.0
forge env status mobile-candidate
forge env deploy mobile-prod --from mobile-candidate
forge obs check mobile-prod --wait --json
```

Resume/reconcile, update-only cut, rollout edits and embedded fallback need explicit
CLI designs tied to declarative intent. Avoid blessing one-off imperative flags
before their plan, persistence and retry semantics exist. `deploy --from` selects
an immutable release and checks target publication compatibility; it does not copy
preview endpoints into a prod binary or blindly copy source audience settings.

## 11. Dev loop and native plumbing

Foundation work independent of release infrastructure:

1. Supported Expo/RN/React alignment with migration guidance and workspace checks.
2. `expo/fetch` transport, verified streaming/cancellation on Hermes; generated
   clients retain auth/trace/error interceptors without web-only imports.
3. Owned `app.config.ts` seam plus generated Forge input data. No competing source
   for app identity; build resolves declarations once, without requiring a running
   control plane. RN runtime config reads the documented Expo extra mechanism.
4. `env up` runs the dev client/Metro with device-reachable LAN/tunnel API URL.
   Explain ATS/cleartext dev exceptions; do not weaken production networking.
5. Dev-only bounded log batching to the existing dev logging path, preserving
   `[browser:<level>]` searches. Redact secrets, avoid recursive logging/retry
   storms, and verify global-handler/rejection behavior for the selected SDK.
   Collect native build/Metro logs separately from JS logs.
6. Unit/typecheck/lint/affected-package discovery include mobile. Device/simulator
   e2e is an explicit lane: missing required capacity is a named unavailable/skip,
   never reported as a validated release. CI waiver policy remains explicit.
7. Native browser-based OIDC authorization-code/PKCE through the broker, secure
   refresh storage, single-flight refresh, logout/revocation and validated
   callback state/redirects. No embedded-webview login or client secret.

Native extras are separate follow-ons, not prerequisites for the first store cut:

- Maestro runner with an explicit mapping from scenario assertions to native
  actions; do not promise mechanical translation of arbitrary Playwright tests.
- APNs/FCM adapters, adopting suitable libraries first; thin consumer contracts,
  typed provider errors, device token lifecycle and environment separation.
- AASA and assetlinks from reviewed identity/link declarations; correct serving
  without redirects, ownership validation and device tests. Android uses installed
  app-signing certificate identity, including supported rotation fingerprints.
- Widgets/Live Activities: owned Expo/native configuration; verify extension IDs,
  signing profiles and App Groups in the builder.

## 12. Implementation phases and exit criteria

Sizes are intentionally not promised before the vertical slice validates store,
Expo publication and remote credential behavior.

### F0a — usable foundation

Prerequisite evidence and file-level work are in the
[mobile foundation audit](mobile-foundation-audit.md). Repair the runtime manifest
range, eager workspace transport bootstrap, missing dev/build task contracts and
native typed-config delivery before calling the foundation usable. Normalize kind
aliases, preserve portable runtime boundaries, and validate standalone, adopted
and mixed web/native workspace consumers without relying on a local runtime link.

SDK/workspace alignment, streaming, typed config, device reachability, log mirror,
unit/typecheck/lint lanes and migration guidance.

**Exit:** fresh and adopted Expo fixtures stream/cancel an RPC on an iOS simulator
and a physical-device smoke run, resolve env config, mirror redacted errors and
pass the affected test lanes. Also demonstrate bounded background/resume/network
behavior, ambiguous mutation outcomes without duplicate writes, and retained
upgrade fixtures preserving owned edits. No claim of release support yet.

### F0b — production native authentication

Broker PKCE flow, secure lifecycle and callback handling.

**Exit:** sign-in/refresh/restart/logout/cancel/replayed callback and concurrent
refresh cases pass, including a real-device integration fixture. Account switching
isolates cached data, in-flight work and any app-owned queue; revoked/offline and
unavailable secure-storage outcomes are explicit. Relevant upgrade fixtures pass.

### F1 — local iOS release vertical slice

Minimal shared builder job/result, MobileApp render, isolated Mac signing,
version/build reservation, OCI envelope/reference verification, durable operation
journal, ASC upload/processing/internal TestFlight, status and explicit resume.
Use existing execution/journal infrastructure where suitable; extend it rather
than create a second ledger. First secret provider is named and actually tested.

**Exit:** one command cuts a signed pinned IPA; another deploys the retained bytes
to internal TestFlight. Kill/restart the CLI after upload, resume without duplicate
submission, and retain verifiable provenance. Concurrency cannot claim the same
new build identity. OTA is disabled, not half-wired.

### F2 — production promotion and remote execution

Chosen trusted macOS remote builder; external TestFlight/review; ASC submission,
manual/automatic/phased lifecycle; controller integration where configured; drift,
supersession and candidate/prod variant checks.

**Exit:** production-configured candidate binary promotes without rebuild; staging
variant refuses; stale approvals cannot silently release over newer intent;
status accurately distinguishes submission, review and availability. Demonstrate
provider uncertainty and manual external modification handling. Production parity
also requires minimum binary health, including a pre-JS native-crash fixture that
cannot pass, zero-exposure/unavailable-signal non-success outcomes, and the binary
incident drill in §9. Bring the minimum native crash adapter into this phase;
full symbolication may follow in F4. If that adapter is not ready, label this phase
distribution-only and never advertise full production parity.

### F3 — signed OTA and minimum health (one milestone)

Runtime-indexed assignments, retained-export EAS publication, key lifecycle,
provider cohort rollout, recovery and release-attributed health ingestion/checks.
Validate the EAS adapter contract before committing its public schema.

**Exit:** JS-only release reuses binary without new build number; native change
refuses update-only intent; old runtime assignments survive a newer one; canary
failure triggers the demonstrated provider abort/recovery control with its resulting
reach recorded (no assumed pause endpoint); signed restore/embedded fallback is exercised; missing
samples return 8, telemetry failure 2 and measured regression 1.

### F4 — full operational/native integration

Native crash reporting and symbolication, remote adapter breadth, Maestro,
links/push/widgets recipes, retention/rotation drills. Hosted updates has its own
reviewed service design and may proceed independently.

**Exit:** stack traces resolve from retained Hermes/native symbols, device deep
links and native scenarios pass, and signing/update-key expiry recovery is drilled.

### F5 — Android

Linux native builder, Gradle/AAB signing, Play edit/track/review lifecycle,
installed signing identity/rotation, assetlinks and FCM. Validate Android schema
fixtures from F1 onward even though implementation follows iOS.

**Exit:** internal-track → production promotion of the same AAB; upload-key rotation
is not misclassified as app identity change; both iOS and Android coexist in one
audience; update cohorts and health attribution remain platform-specific.

## 13. Validation and compatibility gates

Implement tests alongside each phase, not as a final hardening phase:

- Unit/table tests for fingerprint versus binary identity, new versus reused build
  numbers, all plan findings, platform-valid version parsing and variant mismatch.
- Schema fixtures for dual-platform apps, store-only/update-only audiences,
  retained runtimes, unsupported unsigned publication and migration compatibility.
- Contract tests for store/update adapters: pagination, eventual consistency,
  timeout, expired credentials, unknown uploads, partial success and duplicate
  requests. Synthetic adapters are not proof of real store behavior.
- Fault-injection operation tests at every side-effect/checkpoint boundary;
  concurrent cut/deploy, supersession during review and explicit drift adoption.
- Real Mac signing/TestFlight acceptance and retained-export OTA acceptance with
  isolated test app/accounts; never mutate an unrelated production app in tests.
- Device tests for offline/cached updates, launch failure, fallback, wrong runtime,
  wrong/expired signing trust, extension signing, and minimum OS/version handling.
- Privacy/security tests: no credentials in job specs/artifacts/logs, denied
  untrusted-runner access, public telemetry abuse controls and dev-only logging.
- Retention/GC tests prove referenced binary/assets/symbols survive; incompatible
  control-plane versions fail before side effects rather than misread new fields.

Use normal Forge test tiers and slow-test gates. New generated contracts/KCL/CLI
surfaces preserve v1 compatibility. Keep existing RN `Frontend` discovery, hooks,
workspaces and `OnHost` behavior until MobileApp parity and an upgrade playbook
exist; deprecate for at least the established window, never remove in v1.

## 14. Ownership, risks and approval points

**Recommended ownership:** source repo owns app/build identity and owned Expo
configuration; deployment repo owns audience bindings and secret-provider policy.
Bindings reference pinned app declarations/artifacts. Validate agreement rather
than maintain two mutable copies of bundle IDs, routing and signing settings.
Forge owns orchestration/adapters; control-plane owns hosted durable execution,
ledger persistence and hosted grants; Reliant adopts the resulting contracts.
Standalone mode retains a documented resumable path without hidden hosted needs.

| Risk                                                   | Required mitigation                                                     |
| ------------------------------------------------------ | ----------------------------------------------------------------------- |
| Shared candidate OTA channel accidentally targets prod | Explicit routing model; no candidate OTA isolation claim                |
| Native fingerprint mistaken for binary equality        | Separate identities and intent; exact retained output digest            |
| Days-long review/partial publication                   | Durable operations, independent status, reconciliation                  |
| Bad OTA or lost trust key                              | Signed staged rollout, health, tested recovery, store path              |
| Sparse/biased mobile telemetry                         | Minimum samples, failed-launch signals, honest uncertainty              |
| Toolchain/store policy churn                           | Pinned adapters, real acceptance fixtures, documented capability limits |
| Scope balloons before first TestFlight                 | Local iOS vertical slice; follow-on native plumbing separate            |

Owner approval is requested for: first adopter and source/deployment ownership;
production candidate versus preview audience setup; initial Mac capacity and secret
provider; OTA provider/canary strategy and ingestion ownership; supported-runtime
retirement and health policy. Exact KCL/JSON names remain implementation design
reviews, not implicitly approved public contracts.

## References

- [Desktop release proposal](desktop-release-target.md): shared builder/artifact
  direction; still a proposal, not an implemented dependency.
- [Expo deployment guidance](https://docs.expo.dev/eas-update/deployment/): embedded
  channel configuration and identical configuration for update promotion.
- [Expo runtime versions](https://docs.expo.dev/eas-update/runtime-versions/): native
  compatibility and fingerprint policy.
- [Expo updates protocol](https://docs.expo.dev/technical-specs/expo-updates-1/):
  protocol boundary for a future hosted adapter.

Provider capabilities and SDK-specific APIs must be revalidated at implementation;
these references do not replace the phase acceptance tests.
