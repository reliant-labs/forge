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

**Release train.** `rc/v1.4.0-rc.1` (`9caded4a3`) was cut before this PR's merge
base, so the RC does not contain the mobile work (it does contain the session-replay
changes `migrations/v1.4.0` section 1 describes). If v1.4.0 is finalized without
this PR, the mobile sections of `migrations/v1.4.0/*` (sections 2 to 5 and the
`native-dev-loop`, `native-auth` and `native-sdk` sub-skills) move to the next
minor's playbook, and section 1 stays.

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

State at integration head `027bb4237` plus the uncommitted gates pass in this
worktree (`feat/mobile-gates`). "Met" cites its evidence; every unmet or partial
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
- [ ] **Relevant build/lint/test checks against refreshed main, and a full `task
test`.** The merge base is `bce0a4738`; origin/main is 38 commits ahead of it
      at the time of writing, so a refresh is still owed. The earlier full-suite
      attempt failed on macOS shell and Git-fixture portability, and no full
      `task test` has been run on the integrated head. **Pending owner decision:**
      a Linux CI run of the full suite, or a scope statement for the known host
      failures. No failed or unavailable gate is represented as passing.
- [ ] **Independent integration review.** Reviews of the slices and of this
      integration found no unresolved blockers at their heads; a review of the exact
      final head is still owed after the owner decisions below.

**Remaining, each pending an owner decision** (none has been waived; "pending owner
decision" is the status, not a claim that it is acceptable):

- **Android.** No emulator or device run: Gradle builds, Hermes streaming and
  cancellation, lifecycle, and sign-in (an emulator needs `adb reverse` for the
  issuer). JS export for Android passes; that is not Android evidence.
- **Hosted (control-plane) registration of native clients.** The runs used the
  host-run dev IdP; a hosted identity refuses `kind = "native"` today.
- **Full `task test`** on the integrated head (above): a Linux CI run, or a scope
  statement.
- **Universal links** (https callbacks). The callback path is identical and the
  validators accept https, so adding one is a redirect-URI change; it has not been
  exercised.

Not claimed by this milestone and not gates of it: iPhone hardware (an iPad was
used), Release-mode sign-in, store publication, signed OTA and device crash and
recovery evidence.

Once acceptance is complete, mark the integration PR ready for review and let the
normal reviewer/merge queue process handle it. Do not manually merge/admin-bypass
or label it `merge:queue`. Store publication and OTA are later integration
milestones, not hidden prerequisites or claims of this foundation milestone.

## Source plans

- [Foundation audit and roadmap](mobile-foundation-audit.md)
- [Independent implementation-readiness verdict](mobile-implementation-readiness-review.md)
- [Production-readiness contract](mobile-production-readiness.md)
- [Release proposal](mobile-release-target.md)
