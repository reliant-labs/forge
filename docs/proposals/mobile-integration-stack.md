# Mobile foundation integration stack

**Status:** active integration milestone; not ready for main.

## Merge boundary

Only `feat/mobile-foundation-integration` targets `main`. Its PR stays draft until
the verified mobile foundation acceptance gates below are satisfied. Slice PRs
are development/review units, not independently shippable changes to main.

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

The added portable `userMessage` export needs a runtime release before templates
switch value imports. An independently reviewed additive runtime PR/release can
ship first. It must not pull unfinished mobile changes into main. Confirm the
published package actually contains the export, raise the consumer floor and test
registry-only consumers before integrating the import switch. No template depends
on an unpublished version.

## Integration acceptance checklist

- [ ] Existing web behavior passes relevant regressions; shared API exports and
      v1 consumer contracts remain compatible.
- [ ] Registry-only and packed-runtime installs work without a local bridge.
- [ ] Standalone Expo, mixed pnpm web/mobile workspace and adopted Expo fixtures
      pass typecheck, lint, configured native unit tests and shared-package checks.
- [ ] Chosen Expo SDK matrix passes Doctor and iOS/Android JS export checks;
      native build capability is verified separately from Metro export.
- [ ] Native networking, streaming/cancellation, config, lifecycle and dev logs
      have the simulator/physical-device evidence defined in the foundation plan.
- [ ] Native auth/secure storage/refresh/logout and account isolation satisfy F0b.
- [ ] Retained upgrade fixtures preserve owned/custom native configuration and
      converge across generation.
- [ ] Relevant build/lint/test checks pass against refreshed main. Full-suite
      failures are diagnosed and resolved or receive an explicit authorized scope
      assessment; no failed/unavailable gate is represented as passing.
- [ ] Runtime publication dependency is resolved before any new import requires it.
- [ ] Independent integration review finds no blockers; evidence and limitations
      are recorded against the exact reviewed integration head.

Initial validation is not a full-suite PASS: bounded foundation checks passed,
but the attempted complete suite failed in other packages, including macOS shell
and Git fixture portability. Native SDK/device acceptance has not yet happened.

Once acceptance is complete, mark the integration PR ready for review and let the
normal reviewer/merge queue process handle it. Do not manually merge/admin-bypass
or label it `merge:queue`. Store publication and OTA are later integration
milestones, not hidden prerequisites or claims of this foundation milestone.

## Source plans

- [Foundation audit and roadmap](mobile-foundation-audit.md)
- [Independent implementation-readiness verdict](mobile-implementation-readiness-review.md)
- [Production-readiness contract](mobile-production-readiness.md)
- [Release proposal](mobile-release-target.md)
