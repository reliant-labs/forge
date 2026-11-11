---
name: migration-v1.5.0-native-sdk
description: Upgrade an existing Expo app from SDK 52 to 58 one SDK at a time — the exact per-step commands (npx expo install expo@~N.0.0 --fix), the pnpm allowBuilds entry SDK 58 needs first, the SDK 56 expo-doctor Hermes V1 regression that is fixed by continuing to 57, what each SDK changes for the forge starter, the owned files to adapt by hand (package.json, app.json, tsconfig, metro, pnpm-workspace, jest) and the checks that prove each step.
relevance: migration
---

# v1.5.0 — Expo SDK 52 to 58

**Use when** an Expo frontend's `package.json` declares `expo` below `~58`. New
scaffolds are generated on SDK 58 (`expo ~58.0.24`, React and React DOM 19.3.0,
React Native 0.88.1, Router `~58.0.31`); forge never rewrites an existing app's
`package.json`, `app.json`, `metro.config.js`, `tsconfig.json`, Jest or ESLint files.
The SDK versions below are Expo's `bundledNativeModules.json` (sdk-58 branch), not
`npm latest`.

## Upgrade one SDK at a time

Expo recommends upgrading **one SDK at a time** so a breakage has one cause. Do not
jump 52 to 58 blindly: the validated path is 53, 54, 55, 56, 57, 58, one at a time,
each ending green (the executed run is recorded in
`docs/proposals/mobile-conformance-evidence.md`). Do section 2 of
`forge skill load migrations/v1.5.0` first: from SDK 53 on, `expo export` and the
Jest suites need its Metro `.js`→`.ts` resolver and `tests/setup/expo-fetch.ts`.
For each SDK N, read that release's changelog (expo.dev/changelog/sdk-N), then:

```bash
npx expo install expo@~N.0.0 --fix   # moves expo AND every Expo-managed dependency to SDK N
npx expo-doctor                      # must be clean before moving on (see SDK 56 below)
npx expo install --check             # "Dependencies are up to date"
```

Use `expo install expo@~N.0.0 --fix` rather than `pnpm add expo@^N.0.0` followed by
`expo install --fix`: it is one command, and it leaves expo on the tilde range the
scaffold uses (`~58.0.24`). (`expo install --fix` also adds the `expo-asset` (SDK 53) and `expo-status-bar`
(SDK 56) plugins to `app.json`; a fresh scaffold has neither, so they are optional:
keep both or remove both.) Then run `typecheck`, `lint`, `test` and an `expo export` for each platform. Delete
generated `ios/` and `android/` directories (or run `npx pod-install` and apply the
native upgrade helper if you keep them) and rebuild any development build.

**Before SDK 58 (pnpm only):** add `"@parcel/watcher": false` to `allowBuilds` in
`pnpm-workspace.yaml` (create the key if the file has `allowBuilds: {}`), *then* run
step 58. Moving to SDK 58 makes `expo install --fix` add Jest 30, whose file watcher
`@parcel/watcher` has an install script; pnpm fails the add with
`ERR_PNPM_IGNORED_BUILDS` and the upgrade stops half applied (package.json and the
lockfile changed, the rest not). `false` skips the script, which is right: it only
builds from source when asked, and the prebuilt binary arrives as an optional
dependency. If you hit it, add the line and rerun the same command.

**SDK 56 is a stepping stone: its `expo-doctor` fails by design.** Doctor 56 reports
"Check for Expo SDK versions affected by Hermes V1 regressions" (a memory regression
in `expo@56.0.x` with React Native 0.85) and cannot be cleared on 56: its advice is
to upgrade to SDK 57. Do not look for a fix on 56. Take every other check (typecheck,
lint, export, `expo install --check`) green on 56, commit, and continue to 57, where
doctor is 21/21 again. Do not stop on 56.

`pnpm run test` is green on every SDK once section 2 of the main playbook is in
place (the scheme case of `tests/app-config.test.ts` is added by section 5, not
before; 293 tests on SDK 58 with the auth suites in place).

What each step changes for the forge starter:

| SDK | Template-relevant change |
| --- | --- |
| 53 | React 19 and React Native 0.79. Metro enables package.json `exports` by default, so `unstable_enablePackageExports` is no longer needed. New Architecture on by default. `eslint-config-expo` flat config. Node 18 dropped. |
| 54 | React Native 0.81, React 19.1. Last SDK with the Legacy Architecture. Isolated pnpm installs and monorepos are supported, so `nodeLinker: hoisted` is no longer required. `SafeAreaView` from react-native deprecated (the starter uses `react-native-safe-area-context`). |
| 55 | Legacy Architecture removed: delete `newArchEnabled` from `app.json` (expo-doctor fails the schema otherwise). `edgeToEdgeEnabled` removed. Node 20.19.4+. |
| 56 | React Native 0.85, React 19.2. `expo/fetch` becomes `globalThis.fetch` (opt out: `EXPO_PUBLIC_USE_RN_FETCH=1`). TypeScript 6.0.3 is pulled in by `expo install --fix`; `baseUrl` then fails with TS5101, so use `paths` alone. Expo Router drops its React Navigation dependency. On-demand file system: `watchFolders` no longer load-bearing. |
| 57 | React Native 0.86, no breaking changes. |
| 58 | React Native 0.88, React 19.3. Node `^22.13 \|\| ^24.3 \|\| ^26 \|\| >=27`. Router defaults `web.output` to `server` (set `"static"` to keep a static export, which the starter does). `jest-expo` needs `@react-native/jest-preset` and Jest 30. Strict TypeScript API for React Native. Safe-area handling on web moves to CSS `env()`. |

## Owned files to adapt by hand

- `package.json`: let `expo install --fix` move Expo-managed packages. Add
  `@react-native/jest-preset` (`~0.88.1`) and move `jest`/`@jest/globals` to `^30` when
  you adopt `jest-expo ~58`. Raise `@types/react` to `~19.3.0` and `typescript` to
  `~6.0.3`; add `engines.node` if you want the Expo range enforced. Remove
  `query-string` if it was only there for Router's SDK 52 hoisting failure.
  Keep `@babel/runtime`: Expo declares it only as a dependency of its own packages,
  and under isolated pnpm it is not importable from the app.
- `app.json`: delete `newArchEnabled`; run `npx expo-doctor` to catch other removed
  fields. Router's `web.output` stays `"static"` if you export a static site.
- `tsconfig.json`: delete `baseUrl` (TypeScript 6) and, once `expo/tsconfig.base` is
  SDK 58's, the explicit `moduleResolution`; keep `paths`.
- `metro.config.js`: remove `unstable_enablePackageExports` and the workspace-root
  `nodeModulesPaths` / `watchFolders` additions only after a bundle passes without
  them. Then replace the top of the file, through the runtime bridge block, with the
  template's (`forge project upgrade --check frontends/<name>/metro.config.js`
  prints it): otherwise a stale header comment and a misplaced top-level
  `nodeModulesPaths` stay. The runtime bridge block (`watchFolders` for a symlinked
  `@reliantlabs/forge-web-runtime`) is a forge dev-build concern, not an SDK one: keep it.
- `pnpm-workspace.yaml`: `nodeLinker: hoisted` may be removed after SDK 54; in a
  workspace layout the project root file is shared with the web frontends, so
  validate the native bundle in that layout before changing it. Add
  `"@parcel/watcher": false` to `allowBuilds` **before** the SDK 58 step (above).
- `jest.config.cjs`: `jest-expo ~58` rewrites `transformIgnorePatterns`; re-check
  that the runtime/Connect/protobuf allowance still matches.
- Shared `ui-native` package: newly generated packages declare `@types/react ~19.3.0`.
  Existing owned packages are unchanged; align them with the native app's React
  type major when upgrading so one `@types/react` major is installed. A new
  ui-native package beside an SDK 52 app should use `~18.3.0` until that app is
  upgraded. Generated hooks/api packages declare no React types.
- `ui-native` Input now types its ref as `React.ComponentRef<typeof TextInput>`
  (`InputRef`), which works with React Native's legacy and SDK 58 strict APIs.

Verify with `expo install --check`, `expo-doctor`, the app and shared-package
typechecks, the Jest suite, and iOS and Android `expo export`. An executed
generated-hook test is worth adding only if your app has a generated service the
tests can drive; the scaffold ships no such test, and none is required for the
steps above. `tests/fetch-stub.test.ts` already exercises the app's real transport
end to end. These
checks are JS-level evidence only: a device build, Hermes streaming and native auth
are separate gates.
