## V2 of `docs/adr/env-verbs.md`: publishing moves to the env noun

### `forge env build <env> [--push] [--release vX]`

Pushing an image and recording a release are **environment acts**, and both
always *needed* an env to resolve:

| flag | what it needs the env for |
|---|---|
| `--push` | each image's destination is declared on **its own workload** in the env's render (`resolvePushPlan`) |
| `--release` | the artifact **set** — project images *plus* the per-env external `build_cmd` images that exist nowhere else — is discovered from `deploy/kcl/<env>/main.k` |

On `forge build [environment]`, whose env argument is **optional**, that made
them a combination which could only be rejected at runtime — which is precisely
what `errPushNeedsEnv` and `validateReleaseFlags`' env check were written to do.
With the env as a required positional, the bad invocation stops being a runbook
and becomes **unrepresentable**: cobra refuses the arity before any flag
handling. `TestBuildCmd_PushWithoutEnvFails` now records that shift.

**`--release vX` implies `--push`.** Not a convenience: a release pins registry
digests, and a digest exists only once a registry holds the bytes, so a release
that did not push would fail its own ledger write with *"no image digest was
captured"*.

### Top-level `forge build` is compile-only

```
$ forge build prod --push
Error: `forge build` is compile-only and cannot push images — use `forge env build prod --push`.
  --push needs an environment to resolve: each image's destination is declared on its own workload in the env's render
  Top-level `forge build` compiles the tree as a local check, which is why its environment argument is optional
```

The refusal names the env the user actually typed, and the message is specific
to the flag they passed — "cannot push or release" would make the reader work
out which half applies to them.

Both flags stay **registered but hidden**. Unregistering them would produce
cobra's bare `unknown flag: --push`, which tells someone with a working command
in their shell history nothing about where it went. Hidden keeps them out of
`--help` as things this command can do. `TestBuildHelpSaysCompileOnly` pins
both properties.

The refusal also fires **outside a project** — it is a usage decision, so it
costs no render and no config load (`TestBuildRefusalFiresBeforeAnyWork`).

### `forge release cut` is deleted

It was the cut **without** the build, for the pipeline shape whose build and
release are separate jobs. That is the new command with the build phase off:

```
forge env build prod --push                        # job 1: build and push
forge env build prod --release v1.4.0 --no-build   # job 2: record the release
forge env deploy prod v1.4.0                       # bind prod to it
```

One code path for "record a release", differing only in whether it builds
first. Two spellings meant two places for the release's **completeness gate**
to drift — and that gate is the only thing standing between a release with a
hole in it and a promotion that ships one.

**`forge release verify` and `forge release where` are untouched.** They act on
a ledger itself, independent of any environment, which is what the release noun
is for. `TestReleaseGroupKeepsVerifyAndWhere` asserts both survive and that
`cut` does not.

### One behavioral correction

Folding the cut surfaced a real bug in my first version: the cut-only path must
**not** be gated on `features.build`. A cut records a release over digests an
earlier build already produced, so gating it there makes the *release* step of
a pipeline depend on a flag about the *build* step, in a job that does not
build. `requireFeature` also loads project config, which is how this showed up
— `TestHostedLedger_CutPromoteListEndToEnd` failed on `module_path is required`
from a fixture that never needed one. The gate now applies only when a build
actually runs.

### Tests moved, not duplicated

- `runBuildCommand` (the harness behind the whole push-plan suite — declared
  references, bare-image refusal, hosted static sites, flag ordering, release
  tags) now drives `newEnvBuildCmd`. Those tests follow the flags, because the
  compile-only command can no longer resolve a push plan at all.
- Three end-to-end tests that *invoked* `release cut` now invoke
  `env build --release --no-build`, so the folded path keeps the exact coverage
  the deleted command had, through the real root command.
- `TestReleaseCutCmd_AcceptsTheRunFlags` became
  `TestEnvBuildCmd_AcceptsTheRunFlags` — the run identity has to travel with
  whichever verb records a release.
- `TestBuildCmd_PushRefusesARegistryValue` was rewritten rather than deleted:
  a detached value is now a second positional, so the refusal is cobra's arity
  check. That is *stronger* than before, when an optional env meant a registry
  could be mistaken for the env name.

### The sweep

The removalguard entry forced ~130 references across Go, the generated CI
workflows, skills that ship downstream, KCL, docs, the README and the test
goldens. The patterns are anchored on `forge build` so the live
`forge env build --push` is untouched — `forge env build` does not match
`forge build` because the word `env` sits between, which is what lets the
pattern be this narrow instead of needing a pile of allowances.

Three allowances, each narrow: the ADR's own Decision table; the **English
noun** "release cut" (forge still cuts releases — `pkg/release`'s `StageCut` is
literally that stage, and the ledger code describes provenance in those words,
so only the *command* went away); and the handful of comments that name the
deleted command in order to say where it went.

### Gates

- `go test -short ./...` — 108 packages, exit 0
- `go test -count=1 ./internal/cli/` — exit 0 (186s)
- `forge lint --no-fix` — exit 0, 12 gating linters
- Manually verified against the built binary: `env build` registered and
  documented, `build --push`/`--release` refused with the pointer, `release cut`
  gone, `release verify`/`where` present.

Tests fail before the change and pass after: the refusal tests, run against
`main`'s `build.go`, fail on *"refusal should say `forge build` is
compile-only"* for all four flag/env combinations, plus the stale help example.

### Scope

Siblings V3 (`env deploy`) and V4 (`env status`) touch disjoint files. The only
shared file is `internal/cli/env.go`, where this PR adds **one line**
(`cmd.AddCommand(newEnvBuildCmd())`); rebase before merging.

This PR does not touch the generated CI workflow *content* beyond the mechanical
command rename the guard required — V6 owns putting `release.yml` on the new
verbs.
