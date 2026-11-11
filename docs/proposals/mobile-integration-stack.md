# Mobile foundation integration stack

**Status:** integration milestone, code-complete at this head and awaiting the
owner decisions listed under the checklist. The integration PR stays draft until
they are recorded. Evidence, with its limits, is in
[mobile-conformance-evidence.md](mobile-conformance-evidence.md).

## Merge boundary

Only the integration branch targets `main`. Its PR stays draft until the
acceptance gates below are satisfied or each remaining one has a recorded owner
decision. Every slice branch (`feat/mobile-foundation-conformance`, `-sdk`,
`-dev-loop`, `-native-auth`, and `feat/mobile-device-issuer`) is an ancestor of the
integration head, so there is nothing left on a descendant PR: slice PRs were
development/review units and none ships to main on its own. This is a statement
about where merges land, not about readiness: the integration PR is the one unit
that goes to main, and only when its checklist is resolved.

**Release train.** Main released v1.4.0 without this mobile work. The unreleased
mobile sections and `native-dev-loop`, `native-auth` and `native-sdk` sub-skills
now live in `migrations/v1.5.0`. The shipped `migrations/v1.4.0/SKILL.md` stays
exactly as on main, including its session-replay and Next.js proxy notes.

**Owner boundary:** #1887 must NEVER be marked ready by this integration work.
All integration here is local; slice reviews and control-plane pin coordination
remain separate. HTTPS device acceptance has not been run.

Initial checkpoint: `81cdce828` on `mobile-release`, containing the proposals,
audits and first bounded foundation repairs. Preserve this checkpoint unchanged.
The integration branch starts there; no history rewrite or split is required.

## Branches and pull requests

| Branch                             | PR base                                              | Purpose                                                   |
| ---------------------------------- | ---------------------------------------------------- | --------------------------------------------------------- |
| feat/mobile-foundation-integration | main                                                 | Complete validated foundation milestone                   |
| feat/mobile-foundation-conformance | feat/mobile-foundation-integration                   | Executable standalone/workspace/adopted consumer fixtures |
| feat/mobile-foundation-sdk         | feat/mobile-foundation-conformance, until integrated | Coordinated validated Expo SDK upgrade                    |
| feat/mobile-foundation-dev-loop    | feat/mobile-foundation-sdk, until integrated         | Native networking, reachability, lifecycle and logs       |
| feat/mobile-foundation-auth        | feat/mobile-foundation-dev-loop, until integrated    | Native PKCE, secure storage and account isolation         |

Only materialize later branches when work starts. Empty branches/PRs are not proof
of implementation or acceptance. Review slice PRs against their predecessor; keep
them draft until their own work and dependency handling are clear. No slice targets
main or receives merge-queue labels as an author action.

Completed slice changes may be incorporated into the integration branch after
review. This changes no main code. Retarget/reconcile remaining descendants before
continuing; avoid rewriting other contributors' pushed history. All slices must be
present on the integration branch before final validation. Do not leave work only
on a descendant PR and assume the integration PR includes it automatically.

Use Reliant-managed worktrees for isolation. Refresh integration against current
main with a reviewed merge (or agreed history update), preserve slice work and
rerun affected checks. GitHub's merge queue subsequently checks the actual merge
combination; it does not substitute for native/device acceptance.

## Publication dependency

Resolved without a runtime release. An earlier plan switched the shared templates'
`userMessage` value imports to a new additive runtime export and gated that on its
publication. The final templates use `normalizeError` and the `ConnectClientError`
type from `@reliantlabs/forge-web-runtime/interceptors`, which every published
0.13.x already exports, so no template depends on an unpublished version and the
consumer floor (`^0.13.0`) is unchanged. If a template ever imports something new
from the runtime, confirm the published package contains it and raise the floor
first.

## Integration acceptance checklist

State at integration head `dc5976a19` (branch `feat/mobile-gates`, pushed as
`feat/mobile-foundation-integration`); main was merged in at `3b69e5be7`. "Met" cites its evidence; every unmet or partial
item says what remains. Nothing below is a waiver unless it says the owner made one.

- [x] **Existing web behavior and v1 consumer contracts.** The `pkg/` API gate
      reports every change compatible; short suites for `internal/templates`,
      `internal/generator`, `internal/codegen`, `internal/config`, `pkg/release`
      and the web frontends pass.
- [x] **Registry-only and packed-runtime installs, no local bridge.** Cases 1 to 4
      of `TestE2EMobileConsumerConformance`.
- [x] **Standalone, mixed pnpm workspace and adopted (customized) fixtures** pass
      typecheck, lint, native Jest and shared-package checks: the five-case
      `TestE2EMobileConsumerConformance`. A customized scaffold is preservation
      coverage, not an independently authored Expo app (still listed below).
- [x] **Expo SDK 58 passes Doctor and `expo install --check`, and iOS/Android JS
      export.** Each of the five cases runs `expo-doctor` (20/20) and `expo install
--check` (up to date), pinned to expo-doctor 1.21.6, plus both exports. Native
      build capability is a separate item (below).
- [x] **Native networking, streaming/cancellation, config, lifecycle and dev logs
      on iOS.** Simulator run (iPhone 18 Pro, Debug) and one physical iPad over
      Wi-Fi (dev loop 7 of 7, Release build with explicit API URL); details and
      limits in the evidence doc.
- [x] **Native auth, secure storage, refresh, logout and account isolation (F0b)
      on iOS.** Simulator (XCUITest, local Zitadel) and iPad: sign-in, cancel,
      restore, refresh, revoke, logout, isolation between two users; 294 Jest
      tests in a fresh scaffold, including every auth suite.
- [x] **Retained upgrade fixtures preserve owned/custom configuration and converge
      across generation.** The customized case; and the executed v1.3.0 to v1.4.0
      upgrade of a real app (`forge project upgrade`, then the playbooks): every
      mobile gate green on SDK 58 (typecheck, lint, Jest, iOS/Android export,
      `expo-doctor` 20/20, `expo install --check`) and `forge generate` at a fixed
      point twice. The one red gate at the end was the web `no-control-regex` lint
      on the dev-log receiver, a template defect fixed in this pass and now in the
      playbook. The run also found defects in the playbooks themselves; they are
      fixed in this pass and listed in the evidence doc. Simulator sign-in on the
      upgraded app was skipped.
- [x] **Runtime publication dependency.** No template depends on an unpublished
      runtime: they import `normalizeError` and the `ConnectClientError` type from
      `@reliantlabs/forge-web-runtime/interceptors`, which published 0.13.1 and
      0.13.9 both export, and nothing imports `userMessage`. The runtime-source
      `userMessage` re-export is additive and unused.
- [x] **Build, lint and test checks on this head, which includes refreshed main.**
      On `dc5976a19`: `go build ./...` and `go vet` pass; the `pkg/` API gate reports
      16 changes, all compatible; `task test:short ./pkg/... ./internal/...` passes;
      the full-mode (non-`-short`) `internal/templates`, scaffold and `kclrender`
      suites pass; the real-Zitadel tests pass; the five-case
      `TestE2EMobileConsumerConformance` passes with Doctor 20/20 and `expo install
--check` up to date (the fixture has since turned off Doctor's React Native
      Directory check, so it now runs 19 checks; see the evidence doc). A whole-module `task test` (every package in full mode)
      has not been run on this head; the lanes above are what was. No failed or
      unavailable gate is represented as passing.
- [x] **Independent integration review.** An integrated re-review of the exact head
      `dc5976a19` approved it.

**Remaining, each pending an owner decision** (none has been waived; "pending owner
decision" is the status, not a claim that it is acceptable):

- **Android.** No emulator or device run: Gradle builds, Hermes streaming and
  cancellation, lifecycle, and sign-in (an emulator needs `adb reverse` for the
  issuer). JS export for Android passes; that is not Android evidence.
- **Hosted (control-plane) registration of native clients, end to end.** The runs
  used the host-run dev IdP. forge now derives a native application for an `rn`
  frontend on a hosted identity, resolves its callbacks from `app.json`
  (`expo.scheme`) when it projects the shape, and sends it to a control plane that
  advertises `shape-identity-native-app`; a control plane that does not is sent
  none and a note says so. Registration at a real hosted IdP, and a mobile build
  consuming the registered client, are not exercised here (design:
  control-plane `docs/proposals/hosted-native-identity.md`).
- **Whole-module `task test`** in full mode: the lanes listed above passed on this
  head; the rest runs in CI on the merge queue.
- **Universal links** (https callbacks). The callback path is identical and the
  validators accept https, so adding one is a redirect-URI change; it has not been
  exercised.

Not claimed by this milestone and not gates of it: iPhone hardware (an iPad was
used), Release-mode sign-in, store publication, signed OTA and device crash and
recovery evidence.

#1887 remains draft by explicit owner instruction, even after local validation.
Do not mark it ready, manually merge/admin-bypass or label it `merge:queue`.
Store publication and OTA are later integration milestones, not hidden
prerequisites or claims of this foundation milestone.

## Source plans

- [Foundation audit and roadmap](mobile-foundation-audit.md)
- [Independent implementation-readiness verdict](mobile-implementation-readiness-review.md)
- [Production-readiness contract](mobile-production-readiness.md)
- [Release proposal](mobile-release-target.md)
