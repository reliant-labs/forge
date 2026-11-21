---
name: migration-v1.9.0-tests
description: Bring a project scaffolded before forge v1.9.0 onto recorded test results and coverage — the Taskfile's test lanes through `forge test capture`, task test:report, a Report tests CI step, the release's tests gate from `forge test report --gate-json`, instrumented (cover) builds for dev and previews, and the Vitest lane block. `forge fix tests-capture` (run by `forge project upgrade`) does every edit whose file still has the scaffolded shape; this skill covers the rest. Use when `forge lint` reports tests-capture, or a Taskfile test lane runs go test without forge test capture.
relevance: migration
---

# v1.9.0 — recorded test results and coverage

**Use when** `forge lint` reports `tests-capture` (a Taskfile `test*` lane
runs `go test` without `forge test capture`), or `forge test report` says
there is nothing to report after `task test`.

## What changed

A project scaffolded since v1.9.0 records every test lane it runs. Each lane
goes through `forge test capture`, which runs the command unchanged, stores
its results and coverage under `.forge/tests/runs/<id>/<lane>/`, prints
ordinary `go test` output and exits with the command's own status. From that
record:

- `task test:report` (`forge test report`) summarises results, coverage merged
  across lanes and diff coverage, and `--upload` sends it to the control
  plane;
- `forge test flakes` keeps flake history, and `forge test quarantine` skips
  quarantined tests in every lane;
- the release's `tests` gate comes from the record
  (`forge test report --gate-json`), the document `forge ci verify-test-run`
  wrote;
- the dev env and previews of a project with `tests.coverage` on build their
  Go workloads instrumented, so `task test:e2e` collects the servers'
  coverage.

Recording never decides a verdict, and never breaks a lane:

- With `FORGE_TESTS=off`, every lane runs exactly the command it ran before.
- With a forge older than `forge test` (an offline `go run`, an old pin), the
  same is true.
- Coverage never decides an exit code.
- An upload that fails is a warning.

Upgrading forge changes none of your files: the Taskfile, Dockerfile,
`.air.toml`, CI workflows and Vitest configs are yours. Without this upgrade
nothing changes, and `forge test ingest --lane <name> --format go-json <file>`
(or junit, lcov, cobertura…) still records what the project already produces,
for `forge test report` to read.

**The project must pin forge v1.9.0 or later.** The scaffolded lanes degrade
to the plain command under an older forge, but the release workflow's
`forge test report --gate-json` step needs the command to exist.
`forge project upgrade` moves the pin before it runs this playbook. Run on its
own under an older pin, `forge fix tests-capture` leaves `release.yml` alone
and says so.

## What to do

1. **Run the codemod.** `forge project upgrade` runs it for you as this
   playbook's automation step. On its own, run `forge fix tests-capture`
   (`--dry-run` prints the diff first). It needs a committed tree, so that
   `git diff` is the review. It makes each edit below only where the file
   still has the scaffolded shape, keeping your own `go test` flags on a
   lane. It lists everything else as a step, under "Next".

   | File | Edit |
   |---|---|
   | `forge.yaml` | A `tests:` block with `coverage: true`, the other knobs as comments. Not added when the project has a `tests:` block, or sets the legacy `ci.test.coverage`, which the lanes keep reading. |
   | `Taskfile.yml` | The Test records vars (`TEST_BEGIN`, `TEST_SKIP`, `COVERAGE`, `COVERPKG`). `test`, `test:integration` and `test:e2e` go through capture; `test:e2e` also zeroes and collects the env's server coverage (`ENV`, default `dev`). Each frontend records as lane `frontend-<dir>`. Adds `test:report`. `coverage` renders the record (`forge test report --format html`). Nothing is written unless a lane can be recorded. |
   | `.github/workflows/ci.yml`, `e2e.yml` | A `Report tests` step after each test lane: `task test:report -- --upload`, `continue-on-error`, authenticated by the `FORGE_CONTROL_PLANE_TOKEN` secret. The k3d e2e lane runs `task test:e2e ENV=e2e`. The coverage artifact is `coverage.html`. |
   | `.github/workflows/release.yml` | The `Test` step runs `task test`, then `forge test report --gate-json gates/test.json`, then exits with the test's status. Only when `task test` records. |
   | `.air.toml` | `go build $FORGE_GO_BUILD_FLAGS` (unset, nothing changes). On Windows, the flags are splatted. |
   | `Dockerfile` | A `cover-builder` stage, which builds each binary your builder stage builds again with `-cover -covermode=atomic`, and a `cover` stage: your production stage with those binaries and `GOCOVERDIR=/tmp`. The production image is unchanged. |
   | `frontends/*` | The Vitest lane block, active only when `FORGE_TEST_LANE_DIR` is set, so a bare `npm test` is unchanged. Adds `@vitest/coverage-v8` at vitest's own range. |

2. **Install** each frontend's new devDependency with its package manager
   (`pnpm install`, or `npm install`) and commit the lockfile.
   `forge project upgrade`'s locks phase does it for you.
3. **Do the steps it listed.** The sections below cover each one.
4. **Add the secret.** For CI uploads, add a `FORGE_CONTROL_PLANE_TOKEN`
   repository secret: a token with `tests:write` or `deploy:write`. Without
   it the Report step warns and the job passes.
5. **Verify.** Run `task test`, then `task test:report`. The report names the
   lanes that ran, and coverage when `tests.coverage` is on. Then run
   `forge lint`: `tests-capture` reports nothing.

### Taskfile lanes

The codemod recognises a lane whose one command is the scaffolded
`go test … -timeout … <packages>`. A lane it cannot take, because it pipes,
redirects, already passes `-json`, `-cover*` or `-skip`, is not a single
`go test`, or declares its own `vars:`, is left as it is. To record such a
lane by hand:

```yaml
  test:
    vars:
      RUN: { sh: "{{.TEST_BEGIN}}" }    # the run directory, or "" when recording is off
      SKIP: { sh: "{{.TEST_SKIP}}" }    # the quarantine's -skip pattern, or ""
    cmds:
      - >-
        {{if .RUN}}{{.FORGE}} test capture --run '{{.RUN}}' --lane unit -- {{end}}go test -count=1
        {{- if .RUN}} -json{{if eq .COVERAGE "true"}} -covermode=atomic '-coverprofile={{.RUN}}/unit/cover.out'{{.COVERPKG}}{{end}}{{end}}
        {{- if .SKIP}} -skip '{{.SKIP}}'{{end}} ./...
```

- Copy `TEST_BEGIN`, `TEST_SKIP`, `COVERAGE` and `COVERPKG` from a new
  scaffold's `Taskfile.yml` (`forge project new` in a scratch directory), or
  from the codemod's `--dry-run` output.
- Name each lane once: `unit`, `integration`, `e2e`, or one of your own.
- Keep `-covermode=atomic`. Hit counts add up across lanes only when every
  lane uses it.
- A lane that is not `go test` can be captured too
  (`forge test capture --lane <name> -- <command>`). Its output is then
  recorded unparsed.

A lane you deliberately leave unrecorded can silence the rule:
`lint.rules: {tests-capture: off}` in `forge.yaml`, or a
`# forge:lint-disable-next-line tests-capture: <why>` above its command.

### Frontend lanes

A new scaffold's `test:frontend:lanes` runs each frontend's
`$PM test` as `forge test capture --lane frontend-<dir> --results <run>/<lane>/vitest.json -- …`.
The frontend's Vitest config writes `vitest.json` and `coverage-final.json`
into `$FORGE_TEST_LANE_DIR` when it is set:

```ts
const laneDir = process.env.FORGE_TEST_LANE_DIR;
// in test: { … }
...(laneDir
  ? {
      reporters: ["default", "json"],
      outputFile: { json: join(laneDir, "vitest.json") },
      coverage: {
        enabled: process.env.FORGE_TEST_COVERAGE !== "off",
        provider: "v8" as const,
        reporter: ["json"],          // istanbul JSON only: lcov beside it cannot merge
        reportsDirectory: laneDir,
        clean: false,
      },
    }
  : {}),
```

Add `@vitest/coverage-v8` at exactly vitest's range, because it peers on the
exact vitest release.

### Release gate

The scaffolded release workflow's `Test` step becomes:

```yaml
        run: |
          status=0
          task test || status=$?
          forge test report --gate-json gates/test.json
          exit "$status"
```

The gate is undetermined, never a pass, when the record is empty. Swap the
step only once `task test` records.

### Instrumented builds

`forge build` builds a Dockerfile's `cover` stage for an env that
instruments its Go workloads, and builds the plain image when there is no
such stage. `forge env up` passes the flags to air through
`$FORGE_GO_BUILD_FLAGS`.

- **Dockerfile.** The codemod reads the root Dockerfile's `builder` and
  `production` stages. For any other Dockerfile, add the two stages by hand:
  1. `FROM builder AS cover-builder` re-runs each binary's `go build` with
     `-cover -covermode=atomic ${FORGE_COVERPKG:+-coverpkg=${FORGE_COVERPKG}}`
     into `/app/cover/`.
  2. `FROM <production base> AS cover` copies those binaries and sets
     `ENV GOCOVERDIR=/tmp`.
- **`.air.toml`.** Put `$FORGE_GO_BUILD_FLAGS` after `go build`.

Persistent envs never run a cover build.

## Opting out

- `tests.coverage: false` keeps the lanes recording results only, and builds
  nothing instrumented.
- `FORGE_TESTS=off` turns recording off for one run.
- Recording never gates unless the project opts in. One way is
  `lint.rules: {tests-capture: error}` (the forge, control-plane and reliant
  repos set it).
