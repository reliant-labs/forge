# Independent audit: Mobile apps as a Forge release target

**Reviewed:** `mobile-release-target.md`, dated 2026-11-06, complete through §14.
**Verdict:** **Approve the architectural direction, not yet the production contract.** Resolve the high-priority items below before freezing adapter contracts or claiming production parity. F0 and a clearly experimental, OTA-disabled F1 can proceed with explicit limitations. No implementation, deployment or test suite was run for this audit.

The proposal correctly separates runtime compatibility, binary inputs and output digests; production candidates from staging variants; desired assignments from installed cohorts; and durable partial execution from publication success. Those are substantive strengths, not missing requirements. Provisional KCL names are not a defect. The remaining risks concern how these principles compose with real provider resources and existing Forge semantics.

## Findings

### H1 — Mutation ownership must follow provider resources, not audience environments

**References:** §§5, 7 reconciliation rule 2, 8, 10.

Candidate and production explicitly share embedded routing, yet assignment ownership is expressed as `audience/platform/runtime`. Two environments can therefore own different revisions of the **same external assignment**. Different apps or declarations can also alias one EAS project/channel/branch. Both operations could pass Forge CAS while changing the same provider resource. Store ownership is already app-scoped; OTA ownership needs the equivalent rule.

**Verified:** Expo documents one per-update rollout per branch/platform/runtime and one branch rollout per channel. A channel remap affects multiple runtimes, not only the runtime being deployed. The proposal correctly warns about shared reach, but does not define shared ownership or lock scope.

**Change:** Canonicalize external mutation identities (provider account/project, channel, branch, platform/runtime as applicable), reject conflicting declarations or make their shared ownership explicit, and serialize at the actual side-effect scope. Include channel-map revisions and all affected assignments in plans. A branch rollout cannot masquerade as a single-runtime operation.

**Acceptance:** Concurrent candidate/prod operations targeting one branch cannot independently acquire mutation ownership; a channel remap lists and protects all affected runtimes; aliases across declarations cannot evade this rule.

### H2 — Approval does not yet bind the external store content being approved

**References:** §§7, 9, 13; particularly §7's console-owned metadata and §§7/9's exact approved plan.

A release digest does not pin mutable store metadata, build attachments, release settings, privacy answers or review submissions. Console ownership is a reasonable initial scope decision, but someone can change those between approval and submission. A boolean “drift observed” is insufficient to distinguish two different external states.

**Verified repository constraint:** `pkg/release/plan.go:139–183` binds bundle/release and a `PlanBasis` containing promotion/config identities plus a drift boolean. Finding detail is explicitly not digested. `Fingerprint` exists for content outside a bundle, but mobile needs a deliberate observation/binding contract. Existing approval reuse is not automatically enough.

**Change:** Specify an immutable external observation/snapshot or revision included in the approval digest, its freshness policy, and pre-mutation comparison. Freeze the selected store build, metadata/review scope and release controls. Changes require reapproval unless explicitly classified as irrelevant. Where APIs cannot fence concurrent console edits, disclose the residual race, use manual release where practical, and preserve submitted-state evidence rather than claiming atomic approval enforcement.

**Acceptance:** Mutating the selected build, privacy declarations or release mode after approval either refuses execution or produces an explicit, bounded uncertainty path—not silent success under the old approval.

### H3 — Health arrives too late for the advertised production binary guarantee

**References:** §§1, 9, F2, F3, F4.

F2 delivers production App Store promotion, but minimum release-attributed health is in F3 and native crash reporting/symbolication in F4. A store-only adopter need never enable OTA. Meanwhile §9 requires failed-launch-aware health and the summary promises observable outcomes and recovery. JS instrumentation cannot observe a native crash before JS starts.

**Change:** Make minimum binary health a prerequisite of production parity independent of OTA. Either bring native failed-launch/crash evidence into F2, or explicitly label F2 as distribution-only and make `obs check` unable to prove native health. Distinguish TestFlight evidence from App Store exposure and separate “approved to release” from post-release “proven good.” Define the minimum denominator, window, baseline and signal coverage; do not imply TestFlight proves store-channel health.

**Acceptance:** A store-only fixture that crashes before JS initialization cannot return health success. Zero exposure and unavailable native evidence have named non-success outcomes. Production binary and OTA health policies can be exercised independently.

### H4 — Runtime-indexed routing needs a mandatory signing-trust partition invariant

**References:** §§3.2, 4, 6, 8, F3.

The proposal recognizes that trust changes require a binary, but permits a changed signing/trust input without a changed runtime (§3.2). Two binaries sharing provider project/channel/platform/runtime but trusting different OTA keys cannot necessarily consume one signed assignment. An envelope can retain both binaries while the provider's runtime-indexed assignment can only serve one of their trust populations correctly.

**Verified provider guidance:** Expo's code-signing documentation treats the certificate as part of the runtime and instructs changing runtime version on rotation or removal. This is stronger than merely recording trust in binary identity. Fingerprint tooling may enforce this for standard configurations; custom/manual runtime policies and adopted native projects still need enforcement.

**Change:** Require all binaries sharing an OTA routing key to have compatible verification policy, or require a distinct runtime/routing partition for trust changes. Define handling for retained old-key runtimes, loss/compromise and expiry. Validate the certificate actually embedded in owned native projects, not only app configuration.

**Acceptance:** Same-runtime/different-trust binaries are refused unless an adapter demonstrates compatible delivery to both. A key-rotation drill preserves correct signed updates for old and new populations.

### H5 — Trusted-runner identity is not sufficient provenance for signed application bytes

**References:** §§3.1, 6, 13.

The proposal strongly constrains secret redemption but leaves source authorization, builder-result authentication and executable build isolation implicit. A trusted runner executes app scripts, config plugins and dependency hooks. Those can access credentials present in the same job. An IPA with valid signatures proves signing authorization, not that it came from the approved source/config. “Verified output” must not collapse these claims.

**Change:** Define the threat boundary: who may authorize source pins for production signing; how runner identity authenticates a result bound to the spec digest; how stale/replayed/substituted results are rejected; which build code can access each credential. Prefer separating compilation from protected signing/publication where feasible, with verified unsigned-input handoff. Where tool semantics require shared access, state that trusted app build code can use that credential and require corresponding source/dependency approval. Specify ephemeral runner/cache cleanup and symbol/artifact access controls as well as key cleanup.

**Acceptance:** A trusted runner cannot submit a result for another job; an unauthorized source pin cannot obtain signing rights; dependency/install phases cannot access publisher or OTA signing keys. Credential-exfiltration tests cover subprocesses and caches, not just serialized specs/logs.

## Medium-priority findings

### M1 — Define how operations map to existing promotion history before F1

**References:** §§4, 7, F1; desktop proposal §5.7.

The proposal correctly adds durable operations rather than pretending a ledger append makes side effects atomic. It still needs to say **when** an environment promotion is appended and how a partial operation affects readers of its current binding.

**Verified repository constraint:** `pkg/release/promotion.go:72–106` defines current binding through append-only promotions and component targeting; `:138–151` records approval and explicitly distinguishes promotion from deployment time. `internal/cli/release_cut_journal.go` describes recovery after artifact pushes, not a store-side operation controller. The desktop proposal's claim that promotion machinery is unchanged must not be inherited literally for mobile.

**Change:** Provide a worked trace for approval → upload → partial availability → supersession, showing desired promotion, last available component bindings and operation observations. Name which existing readers remain valid and which need new projections. Durable app/build reservations must work across machines, not merely a local journal. Standalone mode needs an explicit shared authority or a documented single-writer restriction.

**Acceptance:** Two machines, CLI restart and mixed-platform partial publication yield one coherent history without falsely saying the whole release is available.

### M2 — OTA recovery during an active rollout requires a specific provider contract

**References:** §§8, 12 F3, 13.

Signed restore and embedded fallback are already requirements. What remains unspecified is the transition **while a rollout is active**, including whether ending it deletes provider evidence, republishes control, or exposes a replacement to everyone.

**Verified:** Current Expo documentation says replacement publication ends the active rollout and exposes the replacement to all matching clients; reverting a per-update rollout deletes its update group and republishes control (or creates embedded fallback). Replacing an active rollout with another rollout is disallowed. Branch-based rollouts have different scope and controls.

**Change:** Choose a rollout mechanism for the first adapter and define pause versus abort versus replacement precisely. Retain manifests, signatures, IDs and observations before provider deletion. State which actions stop future selection versus recover already cached/launched code. Capability-probe CLI/version and account requirements; signed EAS Update currently requires a paid Production or Enterprise plan.

**Acceptance:** Fault-inject recovery during a signed active rollout on real devices; verify control/new/cached clients and retained evidence. “Pause” must not be asserted as a provider feature without a demonstrated primitive.

### M3 — Store-binary incident recovery needs its own acceptance criterion

**References:** §§1, 7, 8, F2.

The proposal correctly rejects compensating an available binary with a fictional rollback. Most recovery detail and acceptance coverage nevertheless concerns OTA. With OTA disabled in F1/F2, a bad production binary has a materially different recovery path.

**Change:** Require an explicit binary incident plan: halt/pause where supported, limitations on manual updates and existing installations, corrective new build/version allocation, expedited review as a request rather than a guarantee, and backend compatibility/feature-disable responsibilities. Keep recovery capability limits visible in plans/status. App unavailability is not restoration of old installed bytes.

**Acceptance:** Demonstrate a bad store-only release incident through provider test facilities or a reviewed runbook plus adapter fixtures. Clearly separate demonstrated controls from real production review timing that cannot be guaranteed.

## Evidence and real-provider acceptance boundary

### Verified from repository inspection

- The mobile proposal includes the identity separation, retained artifacts, durable partial operations and security/health requirements acknowledged above.
- Existing plan and promotion contracts have the constraints cited in H2/M1; a mobile design must extend them deliberately and preserve old readers' v1 behavior.
- Desktop §5.7 is architectural context, not proof of implemented mobile publication or credential delivery.

### Verified against provider documentation

Reviewed official Expo source documentation (mutable `main`; pin the applicable tool/docs revision during implementation):

- [Rollouts](https://github.com/expo/expo/blob/main/docs/pages/eas-update/rollouts.mdx): resource scopes, replacement/revert behavior, deletion and CLI-version restrictions.
- [End-to-end code signing](https://github.com/expo/expo/blob/main/docs/pages/eas-update/code-signing.mdx): embedded trust, new-runtime rotation guidance, expiry behavior and plan entitlement.

These establish documented semantics, not successful execution under Forge. Apple phased-release documentation was not extractable through the fetch tool; no Apple-specific operational guarantee in this audit rests on that fetch.

### Still assumptions requiring acceptance, not established facts

- Retained Expo exports can be published/re-published without a different JS/assets export, with correct Hermes maps, signature verification and recovery directives for the selected SDK/EAS CLI.
- Stable cohort identity, reinstall/offline behavior and health attribution remain suitable for the selected rollout mechanism.
- The chosen Apple account/transport can correlate an ambiguously completed upload with the retained IPA using sufficient evidence; matching version/build alone is not cryptographic proof.
- Real ASC review/cancellation/supersession, signing renewal and manual-console races behave within the adapter's declared limits.
- Native crash signals supply a useful failed-launch denominator within the chosen consent and reporting constraints. Missing events cannot be made trustworthy by a statistical gate alone.

## Author response after audit

The independent findings above are preserved as the review of the initial draft.
The proposal author subsequently added the following requirements; these are
**design dispositions, not proof of implementation or an independent reapproval**.

| Finding | Proposal revision                                                                                                        |
| ------- | ------------------------------------------------------------------------------------------------------------------------ |
| H1      | §7 canonical provider-resource ownership, alias checks and channel-wide mutation scope                                   |
| H2      | §7 immutable external approval basis, freshness, reapproval and console-race limitations                                 |
| H3      | §9 binary-specific health coverage; F2 production parity now requires pre-JS evidence or is explicitly distribution-only |
| H4      | §4 signing-trust partition invariant, actual embedded trust validation and retained old-key populations                  |
| H5      | §6 source authorization, authenticated job-bound results, signing isolation and exfiltration tests                       |
| M1      | §7 worked desired/available/partial/superseded trace and shared authority versus single-writer standalone restriction    |
| M2      | §8 first-adapter per-update rollout semantics, entitlement probes and evidence preservation before provider deletion     |
| M3      | §9 store-only incident runbook and F2 recovery drill                                                                     |

Real-provider acceptance items listed above remain mandatory phase gates. No test,
store upload, credential provisioning or production capability is implied by these
text changes.

## Recommended disposition

Resolve H1–H5 as design decisions and explicit phase gates; add M1's history trace before the first durable deployment implementation. Keep the narrow iOS slice, but advertise distribution support separately from complete production observability/recovery until demonstrated. Use real-provider acceptance as a prerequisite to claiming each capability—not as deferred validation after publishing its public contract.
