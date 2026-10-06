---
name: ci
description: GitHub Actions workflows forge generates — what ships out of the box, the Tier-1 vs Tier-2 boundary, how to add custom workflows alongside the generated ones without losing them on regeneration, and how `forge ci` helper subcommands fit in.
---

# CI

Forge generates a standard set of GitHub Actions workflows under
`.github/workflows/` plus a `.github/dependabot.yml` and (when the
module path is on github.com) a `.github/CODEOWNERS`. The generated
files are Tier-1 codegen — regenerated on every `forge generate` and
banner-protected — but you can add custom workflows alongside them
without forge ever touching your additions.

## What ships out of the box

| File | Tier | What it gates |
|------|------|---------------|
| `.github/workflows/ci.yml` | Tier-1 | Lint (golangci-lint, buf lint, frontend lint+typecheck, migration safety), test (`go test -race -count=1 ./...`, frontend vitest), build (Go binaries with `-trimpath -buildvcs=true`, frontend `next build`), `forge ci verify-generated`, KCL validation, vuln scan (govulncheck, npm audit, Trivy), license check (go-licenses), Docker build, optional E2E |
| `.github/workflows/proto-breaking.yml` | Tier-1 | `buf breaking` (and nothing else — no lint, format, push or PR comment) against the PR's base branch on PRs that touch `proto/**`, `buf.yaml`, or `buf.gen.yaml`. Passes with a notice when the base has no protos yet (the PR introducing the first ones). See the `proto-breaking` skill for the full deprecation flow. |
| `.github/workflows/build-images.yml` | Tier-1 | The project image: build + push, cosign signature, SBOM, and SLSA provenance (skipped on private repositories, which GitHub's attestation store refuses outside Enterprise Cloud). Frontend images are per-env and built by `deploy.yml` |
| `.github/workflows/deploy.yml` | Tier-1 | Per-environment `forge env build <env> --push` + `forge env deploy <env> --yes`, one matrix entry per declared `deploy/kcl/<env>/main.k` (dev excluded) |
| `.github/workflows/e2e.yml` | Tier-1 | E2E suite, label-gated on PRs (`run-e2e`) — emitted when there is a suite to run: the generated `e2e/` harness exists, or `ci.e2e.enabled: true`. docker-compose or k3d runtime |
| `.github/workflows/pre-commit.yml` | Tier-2 | Runs the `.pre-commit-config.yaml` hook set so contributors who skipped the local install are still gated (written once at scaffold; yours to edit after) |
| `.github/dependabot.yml` | Tier-1 | Weekly bumps for `gomod` (root + `/gen` + each frontend), `npm` (frontend), `docker`, and `github-actions` |
| `.github/CODEOWNERS` | Tier-2 | Starter ownership rules (one-shot scaffold; yours to edit after) |
| `.github/pull_request_template.md` | Tier-1 | Standard PR template |

What's enabled is driven by `forge.yaml`:

```yaml
ci:
  provider: github            # only "github" today
  # No go_version key: every setup-go step pins `go-version-file: go.mod`,
  # so CI's Go version follows go.mod. Set it there.
  lint:                       # zero-value = "all enabled"
    golangci: true
    buf: true
    buf_breaking: true        # generates proto-breaking.yml
    frontend: true
    migration_safety: true
  test:
    race: true
    coverage: false
  vuln_scan:                  # zero-value = "all enabled"
    go: true
    docker: true
    npm: true
  e2e:
    enabled: false
    runtime: docker-compose   # or "k3d"
  permissions:
    contents: read            # default
  extra_jobs: []              # see "Extending ci.yml" below
```

### Which forge CI installs

Every job that runs forge installs it from the PROJECT, at run time — no
workflow carries a version literal. All of them run one shared script
(`installForgeScript` in forge), whose core is:

```sh
mod=$(GOWORK=off go mod edit -json)          # reads the go.mod FILE: no network, no cache
if <go.mod requires github.com/reliant-labs/forge>; then
  v=$(GOWORK=off go list -m -f '{{.Version}}' github.com/reliant-labs/forge)
else
  v=<forge_version from forge.yaml>
fi
go install "github.com/reliant-labs/forge/cmd/forge@${v}"
```

go.mod's forge requirement is the forge the code compiles against and the
one that generated it, so **bumping forge in go.mod is the whole upgrade** — the
workflows follow on their own. A version stamped into a workflow at scaffold
time froze there while go.mod moved on: that is how a project ended up
verifying its generated code with an older forge than the one that wrote it
("refusing to overwrite .forge-kcl/ with an OLDER forge's KCL module").

- WHETHER go.mod requires forge is read from the file, never inferred from a
  lookup failing. Only a module that does not require forge at all (a
  `--kind cli` / `library` project) uses forge.yaml's `forge_version`. (For a
  module that does, `forge generate` keeps `forge_version` equal to go.mod's
  require, so the two never disagree in a generated tree.)
- If go.mod requires forge and the version cannot be resolved, the step FAILS
  with go's own error. It never falls back to forge.yaml — installing a
  different forge than the code compiles against is the drift this exists to
  prevent.
- A `replace` of forge in go.mod fails the step with `::error` naming it — CI
  cannot install a local checkout. Bridge one with an uncommitted `go.work`.
- A pin no module proxy can serve (`+dirty`, `dev`, `0.0.0`) fails the same way.
- It needs `jq` (preinstalled on GitHub-hosted runners). No C toolchain is
  needed.

The verify-generated job also installs the codegen toolchain at go.mod's
versions — `forge tools install --force` (protoc-gen-go, protoc-gen-connect-go,
goimports) — and runs `npm ci` in each frontend, because it regenerates the
tree and demands identical bytes. `npm ci` is what installs
`@bufbuild/protoc-gen-es`, from the frontend's own devDependencies and
lockfile: `forge tools install` never runs npm and never writes a
frontend's `package.json` or `package-lock.json` (`--force` reinstalls the
Go tools only). It fails, naming the edit, when a frontend's buf.gen.yaml
runs the plugin but its package.json does not declare it. `forge ci
verify-generated` refuses to run when a frontend's protoc-gen-es is missing
rather than certify a tree whose TypeScript stubs it skipped, and refuses
to regenerate over a tree an earlier step already modified — naming those
paths as changed before `forge generate` ran, not as generated-code drift.

### No workflow names a registry

An image registry is part of a WORKLOAD's `image` in `deploy/kcl/workloads.k`
(`image = "ghcr.io/<owner>/<name>"`) and nowhere else. No env declares one, and
no workflow names one — no `REGISTRY` env or repository variable, no value after
`--push`, no `docker/login-action` or `docker/metadata-action` input. The
workflows reach the registries through forge, which derives them from the
images:

```bash
printf '%s' "$TOKEN" | forge registry login <env> --username <user> --password-stdin
forge env build <env> --push                    # each image → its own declared reference
forge registry ref <env> --github-output    # ref= image= digest= for later steps
```

`login` logs in to every distinct host the env's images name, so an env whose
workloads push to two registries needs no extra step.

`build-images.yml` builds once per commit on main for the first deploy env
(the one `deploy.yml` auto-deploys), then signs, SBOMs, attests and scans the
digest-pinned ref `forge registry ref` read back — never a tag and never a
YAML literal. The login credential is `GITHUB_TOKEN` (GitHub's container
registry); for another registry, pipe its credential from a secret (a GAR
key with `--username _json_key`, an ECR token with `--username AWS`).

### A hosted env needs NO registry credential — one token

Where the env pushes to the PLATFORM registry, there is no registry login step
and no registry secret. forge authenticates it with the same
`FORGE_CONTROL_PLANE_TOKEN` the job already holds for the release ledger, and
`forge env build --push` / `forge env deploy` log themselves in before their
first push. So the scaffolded workflows differ by env, not by project:

| Workflow | Login step | Credential |
|---|---|---|
| `release.yml` (hosted by construction) | none | `FORGE_CONTROL_PLANE_TOKEN` |
| `build-images.yml`, hosted build env | none for the build; the trivy job runs `forge registry login <env>` with no flags, because it PULLS rather than pushing | `FORGE_CONTROL_PLANE_TOKEN` |
| `build-images.yml`, cluster build env | `forge registry login <env> --username … --password-stdin` | `GITHUB_TOKEN` or your own |
| `deploy.yml` (non-hosted envs only) | same as above | `GITHUB_TOKEN` or your own |

Two things follow, and both are worth knowing before editing a workflow:

- **Do not add `--username`/`--password-*` for the platform registry.** forge
  refuses them for our host, so the job fails on its first run. The flags are
  for a registry you chose.
- **One secret, not two.** A hosted pipeline needs `FORGE_CONTROL_PLANE_TOKEN`
  and nothing else for images. Adding a registry secret gives you two things to
  rotate and only one of them in anybody's memory.

An env that pushes to the platform registry AND one of your own keeps the login
step for yours alone; forge does its half in the same run.

A job that gets a realm **401 / DENIED** on push is almost never missing a
credential. forge composes the push address from the org the token acts for
(there is no `organization` to declare), so check whether the token expired and
whether it carries `deploy:write` — a push needs both the org's subtree and
write access. forge prints a hint saying so. Do not add a `docker login`.

### Deploys go through forge

`deploy.yml` runs, per env, `forge registry login <env>`, `forge build <env>
--push` (each image to the reference its workload declares), then
`forge env deploy <env> --yes` — never `kcl run | kubectl apply`, which cannot
resolve `kcl_plugin.forge` and skips the declared-context binding, the
per-env frontend `config.js` render, digest pinning and the live preflight.

**`--yes` is not optional in CI, and it is not a bypass.** `forge env deploy`
prints the deploy plan and will not write a promotion until somebody approves
it. A GitHub runner has no TTY to prompt on, so without `--yes` the command
exits **5** (`plan_unconfirmed`) having built, pushed and cut but deployed
nothing. `--yes` means "I read the plan" — the plan is still computed and
printed into the job log immediately above the write, so the record of what
shipped is in the run. For a pipeline that wants a human in the loop, run
`--plan-only` in one job and put the approving job behind a GitHub Environment
with required reviewers; `release.yml` already has that shape.
The env list is the project's `deploy/kcl/<env>/main.k` set (dev excluded),
read when the workflow is scaffolded; with several, the first auto-deploys
after a green image build on main and the last is protected. A lone env is
never auto-deployed.

Credentials: an env with `forge.OnCluster` workloads needs `secrets.KUBECONFIG` holding a
context named exactly the ClusterTarget's `cluster`; `forge.OnHosted`
workloads need `secrets.FORGE_CONTROL_PLANE_TOKEN` (for the env's
`forge.ControlPlane`) and no kubeconfig; an env that binds both needs both.
forge picks each workload's path from its own binding.

### Pre-commit and generated files

`.pre-commit-config.yaml` excludes every forge-generated path (`gen/`,
`*_gen.*`, `.forge-kcl/`, the grafana dashboards, `deploy/alloy-config.alloy`,
`public/config.js`, the hooks barrel) from the MUTATING hooks — whitespace
fixers, gofmt/goimports, prettier. `forge ci verify-generated` demands
generated files regenerate byte-identically, so a formatter that rewrote one
would fail it on every commit. forge also emits those files already clean
(single trailing newline, no trailing whitespace, `buf format`-clean protos).

## Tier-1 vs Tier-2 boundary

The forge-generated workflows are Tier-1: they carry the
`# Code generated by forge. DO NOT EDIT.` banner, certify themselves
via an embedded `forge:hash` marker, and are regenerated every
`forge generate`.
Hand-edits get surfaced by `forge project audit` and `forge project map`
(`forge-space, hand-edited (drift from regen)`) and overwritten on the
next regenerate unless you pass `--force` to clobber explicitly or
opt-in to local edits via the workflow comment.

User-owned workflows are Tier-2: forge never reads them, never writes
them, and `forge generate` leaves them alone forever. The boundary is
mechanical:

- **Tier-1** — file matches a name forge knows about (`ci.yml`,
  `proto-breaking.yml`, `build-images.yml`, `deploy.yml`, `e2e.yml`,
  `dependabot.yml`, `pull_request_template.md`) AND carries the
  generated banner. `forge project map` reports `forge-space, regenerated`.
- **Tier-2** — any other workflow file. `forge project map` reports
  `user-owned`. Add as many as you want.

When you need to evolve a Tier-1 workflow beyond what forge.yaml
exposes, the right answer is almost always one of:

1. Add a knob to forge.yaml (and a forge issue / PR if forge can't
   express it yet). This is a forge improvement; surface it.
2. Use `extra_jobs:` in forge.yaml — forge appends them at the bottom
   of the generated `ci.yml` with their own steps and `needs:`.
3. Add a parallel Tier-2 workflow file. Keep the file name distinct
   from any Tier-1 name so forge never tries to regenerate it.

## Adding a custom workflow

The Tier-2 path. Drop a new file under `.github/workflows/` with any
name forge doesn't already own (`docs.yml`, `tag-release.yml`,
`slack-notify.yml`, …). Convention: open with the canonical
"yours" banner so future readers see at a glance that the file is
user-owned and forge will not touch it:

```yaml
# yours: scaffolded once, never touched again — forge will not overwrite this file
name: Release
on:
  push:
    tags: ['v*']
permissions:
  contents: write
jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - name: GoReleaser
        uses: goreleaser/goreleaser-action@v6
        with:
          version: latest
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

`forge project audit` and `forge project map` will report the file as `user-owned`.
`forge lint --banners` ignores user-owned files (the banner check only
fires on forge templates).

## Extending `ci.yml` from forge.yaml

Use `extra_jobs:` when the addition belongs in the same workflow as
the generated jobs (so it shares the `concurrency:` group, runs on the
same `pull_request` trigger, etc.):

```yaml
# forge.yaml
ci:
  extra_jobs:
    - name: docs
      runs_on: ubuntu-latest
      needs: [build]
      steps:
        - name: Checkout
          uses: actions/checkout@v4
        - name: Build docs
          run: make docs
```

Forge appends each `extra_jobs:` entry to the generated `ci.yml`. The
job sees the same `actions/checkout@v4` ergonomics as forge's own
jobs. Use this for jobs that:

- Need to gate on `[lint, test, build]` from the generated workflow.
- Should run on every PR alongside the generated jobs.
- Don't need their own trigger, schedule, or concurrency rules.

For anything that wants its own `on:` trigger (cron, workflow_dispatch,
release tags), use a parallel Tier-2 workflow file instead.

## Common extensions

- **Release.** Tier-2 `tag-release.yml` triggered on `push: tags: ['v*']`
  using GoReleaser, ko, or a hand-rolled `gh release create`.
  Generated `build-images.yml` covers main-branch image pushes;
  release adds tag-driven artifact + image promotion.
- **Deploy beyond k3d/staging/prod.** The generated `deploy.yml`
  iterates the env directories discovered under `deploy/kcl/<env>/main.k`.
  Add a preview-environment Tier-2 workflow keyed on PR labels or a
  manual-promotion Tier-2 workflow keyed on `workflow_dispatch`.
- **Slack / Discord notifications.** Tier-2 `notify.yml` triggered on
  `workflow_run: completed` of `CI` — keeps notification logic out of
  the generated chain.
- **Codecov / coverage upload.** Set `ci.test.coverage: true` in
  `forge.yaml` to have the generated `test` job upload a coverage
  artifact, then add a Tier-2 workflow that downloads + uploads to
  Codecov on `workflow_run`.
- **Custom security scanners (Snyk, Semgrep).** Tier-2 workflow
  triggered on schedule + PR; the generated `vuln-scan` job covers
  govulncheck/npm audit/Trivy and is opinionated, so add scanners
  alongside rather than try to extend the generated job.

## `forge ci` helper subcommands

The generated `ci.yml` calls into a `forge ci` subcommand group rather
than inlining shell logic, so the same checks are runnable locally:

| Command | What it does |
|---------|--------------|
| `forge ci verify-generated` | Runs `forge generate` and asserts nothing changed (`git status --porcelain`, so a newly created file counts) — catches mock/codegen drift where a contract.go grew a parameter but the mock_gen.go wasn't refreshed. Refuses to run when a frontend's protoc-gen-es is missing |
| `forge ci validate-kcl` | Renders every environment under `deploy/kcl/<env>/` and asserts the result is APPLYABLE — a `manifests` root exists, every document carries apiVersion + kind, and no other top-level key hides objects no deploy would apply. Shares its implementation with `forge doctor --signal deploy`, so CI and the doctor cannot disagree |
| `forge doctor --signal deploy` | Deployability gate: probes, resource requests/limits, credentials sourced from Secrets rather than literal env values, ServiceAccounts bound to pods, and a way to apply pending SQL migrations. Reads the rendered manifests, so it needs no cluster and no image |
| `forge ci vuln-scan --go` | Runs govulncheck against `./...` |
| `forge ci vuln-scan --npm` | Runs `npm audit` in each frontend |
| `forge ci migration-safety` | Runs `forge lint --migration-safety` against `db/migrations/` |

Run any of them locally before pushing to reproduce the CI gate.

## Linting the workflows themselves

Forge has no dedicated CI-workflow analyzer. What covers the workflows today:

- **`forge lint --banners`** — verifies every forge template (including
  `.github/workflows/*.yml`) carries the right Tier-1 / Tier-2 banner.
  Catches a generated workflow that lost its `# Code generated by
  forge. DO NOT EDIT.` header or a Tier-2 scaffold missing the
  `# yours: scaffolded once, never touched again` banner. Runs only inside the forge repo
  (the templates live there).
- **`forge project audit`** — reports drift on Tier-1 workflows (forge-space
  files with hand-edits) under the `codegen` category.
- **`forge ci verify-generated`** — re-runs `forge generate` and fails
  if the diff is non-empty.

If you want stricter checks (no `@latest` action versions, required
secrets enumerated, permissions-locked-down rules), file a forge issue
or add a Tier-2 workflow that runs `actionlint` and friends against
`.github/workflows/`. Treat the gap as known.

## Rules

- forge's workflows (`ci.yml`, `proto-breaking.yml`, `build-images.yml`,
  `deploy.yml`, `release.yml`, `e2e.yml`, `reconcile.yml`, `pre-commit.yml`,
  `dependabot.yml`) and the vendored `.github/actions/forge-deploy/action.yml`
  are scaffold-once: written once, then yours to edit.
  Deleting one sticks — `.forge/scaffolded.json` records it, so `forge
  generate` does not bring it back. To get forge's current version of one,
  delete it and run `forge project rescaffold .github/workflows/<name>.yml`
  (only workflows this project has: e2e.yml needs an e2e suite,
  reconcile.yml the reconcile feature, release.yml and the forge-deploy
  action a hosted env). Which envs are hosted is read off a render of each
  env; when one does not render yet, rescaffold first writes the config
  modules envs import (`forge generate --steps env-config`), and an env that
  still fails is named — run `forge env render <env>` to see why.
- Tier-2 workflows live alongside Tier-1 in `.github/workflows/`. Use
  any name forge doesn't own; open with `# yours: scaffolded once,
  never touched again — forge will not overwrite this file` so the
  boundary is visible.
- The generated workflows install the forge go.mod resolves, at run
  time. Bumping forge in go.mod is the whole upgrade; there is no pin in
  a workflow to re-stamp.
- `extra_jobs:` in forge.yaml is the right escape hatch for jobs that
  belong in `ci.yml`. A standalone trigger (cron, tag, dispatch) means
  a Tier-2 file.
- `forge ci verify-generated` is the canonical drift gate. Run it
  locally before pushing if `forge generate` touched anything.

## When this skill is not enough

- **Proto breaking-change handling** — see `proto-breaking`.
- **Deploy and release pipelines beyond CI gates** — see `deploy`.
- **What `forge project audit` reports about CI files** — see `audit-json`.
- **Banner classification details** (Tier-1 vs Tier-2 vs skip-list) —
  see `architecture`.
