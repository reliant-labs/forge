# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- **`forge env status` is the ONE read view of an environment.** Reading an env
  used to take six verbs — `env status`, `env verify`, `env wait`,
  `env rollout`, `env topology`, `env history` — and they were views of a
  single question, split by which half of the answer each happened to own. A
  reader had to know, before they could ask, that the bound release lived in
  `verify`, the rollout phase in `rollout`, the runtime ports in `status`, and
  the promotion that caused all of it in `history`. Two were literal
  duplicates: `env rollout` WAS `env wait --timeout 0`.

  They are modes of one command now:

  ```bash
  forge env status                  # every environment, and how far behind each is
  forge env status prod             # prod right now: runtime AND release
  forge env status prod --wait      # block until the rollout settles
  forge env status prod --history   # prod's promotion ledger, newest first
  ```

  One env's report carries the bound release, verify (running digests vs the
  binding, in all five states, through the control plane's observer for a
  hosted env), the rollout phase per workload, runtime health, gates, and
  ledger freshness. The two halves are deliberately asymmetric about failure:
  runtime health REPORTS (a down stack is a state this command must be able to
  print), while the release half makes a claim and so owns the exit code.

  **Every exit code is unchanged**, because every mode reaches the same code
  the old verb called: 0/1/2 for the read, plus 5 (timed out, still
  progressing) and 6 (superseded) under `--wait`. `--timeout` serves two modes
  with different budgets — 60s for a cluster read, 15m for a wait — so its
  declared default is an unset sentinel resolved per mode; `--wait --timeout 0`
  still means one read, never blocking.

  `--json` emits one document carrying the standard envelope. Its release
  fields stay FLAT (`bound`, `images`, `release`) because `forge gate record
--from` recognises the document by them.

### Removed

- **BREAKING: `forge env verify|wait|rollout|topology|history` are deleted**,
  with no alias and no hidden name — they are modes of `forge env status` (see
  Changed, above). `forge release where` and `forge release verify` are
  untouched: both answer a question about a RELEASE rather than an
  environment. A removalguard entry pins all five spellings on every surface.

- **BREAKING: `forge env promote` is gone — `forge env deploy <env> [vX | --from
<src-env>]` does the whole job.** No alias and no hidden name; the old
  spelling dies on "unknown command".

  ```bash
  forge env deploy prod v1.4.0          # was: forge env promote v1.4.0 --to prod && forge env deploy prod
  forge env deploy prod --from staging  # exactly what staging runs
  forge env deploy prod                 # unchanged: re-apply prod's current binding
  ```

  **The verb now means record + apply + wait, and the wait is not a flag.**
  Recording a binding ships nothing, so a pipeline step that only promoted
  reported success before any byte had moved and the release's real failure
  surfaced minutes later with nothing connecting the two. Every pipeline
  therefore spelled it `promote --deploy --wait`, and the spellings that
  omitted either half were bugs waiting for an incident. So the health gate is
  ON by default; `--no-wait` opts out and names
  `forge env status <env> --wait` as the way to gate later.

  **Who applies it is read off the environment's ledger, not a flag** — the
  same declarative fact as where the promotion is recorded, so the two cannot
  disagree. An env whose KCL declares `forge.ControlPlane` is converged by
  that control plane and forge waits on the rollout it computes, pinned to the
  promotion just written. Every other env is self-managed: the same command
  renders the new binding and applies it from this machine, and that apply's
  per-resource rollout wait IS the health gate.

  `promote --wait` and `promote --deploy` are removed with the verb rather than
  renamed: both are now what the command does.

  Everything else is unchanged and moved with the code — the compare-and-set
  (`--expect-current` / `--expect-unbound` / `--supersede`), `--gate`,
  `--from` / `--from-promotion`, `--plan`, the run flags, every exit code and
  the JSON envelope. A deploy that names no version keeps today's spec-change
  behaviour, and a release-only flag passed without a version is now refused
  rather than silently ignored.

- **BREAKING: an environment no longer has an image registry — a WORKLOAD has
  one, as part of its image.** `forge.ClusterTarget.registry`,
  `forge.ControlPlane.registry` and `forge.DockerBuild.registry` are all gone.
  The author writes the full reference on the workload and forge contributes
  only what it alone knows:

  ```kcl
  # deploy/kcl/workloads.k
  api = fw.Workload {
      name = "api"
      image = "ghcr.io/acme/api"        # the registry IS the image
      build = forge.GoBuild {cmd = "./cmd/acme", output_name = "acme"}
  }
  ```

  forge appends the resolved tag and, after a push, the digest. It never
  invents, prefixes or rewrites the host. So the reference you read in the KCL
  is the reference the build pushes and the cluster pulls — they cannot
  disagree, which is the whole point: a build aimed at one registry while the
  spec named another was an `ErrImagePull` that surfaced only at rollout.

  Two workloads in one env may now name two different registries, which an
  env-wide field could not express at all.

  **Migration is automatic.** `forge generate` removes `registry = …` from every
  ClusterTarget / ControlPlane and prefixes the removed value onto a workload's
  bare `image`, judging ambiguity ONLY over the envs where that workload is
  bound to a runtime that pulls an image (cluster or hosted). A registry on an
  env where the workload runs `OnHost`, or is not bound at all, is simply
  dropped — it was never that workload's. If the same workload genuinely pulls
  from two different registries, forge REFUSES and prints the exact per-env KCL
  rather than picking one. Your binder expressions are never edited.

  **A bare image is refused at render**, naming the workload: it would render as
  `api:<tag>`, which the kubelet resolves against Docker Hub and fails to pull.
  A third-party image must name its host too (`docker.io/library/nats:2.10`).

  A hosted env is no different, and gets its own runbook: a hosted env may one
  day inherit a default from the control plane's advertised image push base, but
  that base is not pushable yet (control-plane ADR 0003 / #334), so forge does
  not invent one. Hosted FRONTENDS follow the same rule with the same field
  name (`forge.Frontend.image`), and forge appends the platform's own
  `static.v1` layout segment itself, so that segment never appears in your KCL.

  A local env states the image IT runs, because a k3d node can only pull from a
  host-local registry. That is a plain KCL string on the workload — the
  scaffolded dev env declares one `_LOCAL_IMAGE` constant and its `_on_k3d`
  binder sets it. forge ships no helper for composing an image reference and
  prescribes no pattern: a project that wants one source of truth writes its
  own constant, which is what KCL is for.

  **`forge registry login <env>` logs in to every distinct host the env's images
  name** — no host argument and no flag that takes one. The credential is the
  only input: `--username` with `--password-stdin`, or `--password-env` naming
  the variable that holds it (the `forge.ControlPlane.token_env` convention, so
  CI states a NAME in git and never the value). `forge registry ref <env>` now
  prints one `<image>@<digest>` per built image, labelled by the workload that
  declared it.

  **RELEASES MUST BE RE-CUT.** The release ledger now keys an OCI artifact by
  its declared repository, host included, so the key IS the address and
  `release.Artifact.URI` is no longer recorded for images. Ledgers cut before
  this change are keyed by bare name, which no longer resolves to an address:
  `forge release verify` reports such an entry as UNVERIFIABLE rather than
  guessing Docker Hub. Re-cut and re-promote any release you still rely on.
  Keying by bare name could not survive per-workload registries — two envs
  building the same app to two registries collapsed onto one entry, so a prod
  promotion could read a digest that only ever existed in the dev registry.

  Also removed: `BuildState.Registry` and `buildtarget.State.Registry` (Image
  now holds the full repository), `forge env new`'s `REPLACE_ME_REGISTRY` knob
  (an env has no registry to get wrong), and the `registry` field the render
  projected onto each cluster runtime.

- **BREAKING: a `forge.ShellBuild` `cmd` is plain KCL, run verbatim — the
  `${TOKEN}` substitution is gone.** Forge used to rewrite a fixed vocabulary
  into the command before running it — `${IMAGE}`, `${TAG}`, `${CODE_VERSION}`,
  `${SERVICE}`, `${TARGETARCH}`, `${REGISTRY}`, `${PROJECT_DIR}`, `${ENV}` and
  `${BUILD_CWD}`. It no
  longer substitutes anything and exports no variables of its own: the rendered
  string is handed to `sh -c` byte-for-byte, from the declared `cwd`, with the
  declared `env` merged onto the process environment. Every `$VAR` in a command
  is now the shell's, so `$PWD` and `$HOME` work.

  Write the values in KCL, where the command is composed and the value already
  lives: `forge.target_arch()` (new, and the reason `target_arch` is now a
  reserved render input), `forge.image_tag("<env>")`, `forge.env()`,
  `file.workdir()`, the image reference itself, or a literal. To keep a
  `${NAME}` spelling, declare `NAME` in that build's `env` map and the shell
  resolves it.

  `forge lint` (`shellbuild-tokens`) and `forge generate` both REFUSE a leftover
  token. That is a gate rather than a warning because one of them fails
  silently: `GOARCH=${TARGETARCH}` reaching the shell unset becomes `GOARCH=`,
  which Go reads as "unset" and builds for the host — so an amd64 cluster gets
  an arm64 binary whose only symptom is an `exec format error` crash-loop. The
  rest (`docker push /:`) fail loudly.

  Forge now resolves the build's tag BEFORE the render and binds it as the
  existing `image_tag` input, so a rendered `cmd` already names the tag the build
  records and pushes (`--release` > `--tag` > the image's own pin > the env's
  `image_tag` > `git describe`, unchanged). Two casualties of the old pass are
  also gone: `forge project audit`'s `conflict_tokens` / `conflict_count` (with no
  built-in tokens there is nothing for a declared env key to collide with) and
  `forge doctor`'s substituted-command preview, which now prints the exact string
  forge will run instead of one built from `<registry>`/`<tag>` stand-ins.

  Escaping, worth knowing before you write one: braced-dollar is KCL's own
  interpolation, so backslash-escape a shell `${HOME}` or use a raw `r"..."`
  string. A DOUBLED dollar is not an escape — KCL reads it as an undefined name
  and refuses to compile.

- **BREAKING: every rollback surface is gone — recovery is roll forward.**
  `forge env deploy --rollback`, `forge env promote --rollback`, the KCL
  `forge.External.rollback_cmd` field, and every provider's rollback path
  (`kubectl rollout undo`, compose override pinning, the static-site
  re-sync, the hosted `DeployService/Rollback` call) are removed, along with
  the deploy's skip-the-migration-on-a-rollback machinery. A rollback claims to
  undo a release it cannot undo: the release already ran its migrations and
  wrote data. Fix forward instead — cut a release and promote it. Binding an
  env to an OLDER release is still an ordinary `forge env promote`; the plan
  labels it `direction BEHIND` and warns that it undoes nothing. Existing
  ledgers keep working: a `"kind":"rollback"` entry reads as the promote of
  its release. Delete `rollback_cmd` from any `forge.External` block.

### Added

- **Scaffolded CI for hosted environments** (ADR `docs/adr/env-verbs.md`,
  task V6). A project with at least one hosted env (an env whose KCL declares
  `forge.ControlPlane` and something the platform runs) now gets two new
  files. The first is `.github/workflows/release.yml`. On a `v*` tag it runs
  checks once (`forge lint --gate-json`, plus `task test -- -json` fed to
  `forge ci verify-test-run --gate-json`). It then builds, pushes and records
  the release in ONE command (`forge env build <env> --release "$VERSION"`;
  `--release` implies `--push`). Finally it deploys the release through the
  hosted envs in promotion order, one job per env, each under its GitHub
  Environment. Each stage after the first deploys `--from` the stage before
  it, pinned with `--from-promotion`. It asserts `--expect-current`, which the
  previous job captured before this stage's approval wait. A hotfix that lands
  while an approval is pending therefore turns the run red (exit 3) instead of
  being overwritten. The second file is
  `.github/actions/forge-deploy/action.yml`, a composite action vendored into
  the repo so that it versions with the project's forge pin. It is one
  `forge env deploy` — which records, applies and waits for health itself — and
  then records that outcome as rollout evidence on the promotion the deploy
  wrote and summarizes it into the job summary. Because the verb decides who
  applies from what the env DECLARES, the action carries no promote step, no
  "does this env converge its own promotions" probe, no separate wait and no
  `deploy:` input.
  Existing projects get both via
  `forge project rescaffold .github/workflows/release.yml`.
  Neither file uses curl or environment ids, and no token appears in a URL.
  The only secret is `FORGE_CONTROL_PLANE_TOKEN`.
- **`forge cloud token create|list|revoke`** mints and manages the org
  automation token that CI deploys with. An org token has no acting user, so
  it outlives any one person. Scopes are validated before the round trip.
  `--json` carries the one-time secret, so
  `… --json | jq -r .secret | gh secret set FORGE_CONTROL_PLANE_TOKEN` never
  echoes it to a terminal.
- **`forge ci summarize <doc.json>...`** renders forge `--json` documents as
  Markdown for `$GITHUB_STEP_SUMMARY`. It renders the verdict from the
  envelope, a field table and row tables. It decides nothing: it exits 0
  whatever the documents say, and 1 only when an input cannot be read.
- **`forge env deploy` applies a MIXED env's cluster half.** An env that keeps
  its ledger on a control plane AND declares workloads that control plane does
  not run — a cluster Deployment, a compose service, host infra, a shipped
  frontend — now gets both halves of the deploy: the client-side apply for what
  forge owns, then the wait on the rollout the control plane computes. Before,
  the follow-through branched on "is this env hosted" alone, so a mixed env
  recorded its promotion and waited on the hosted rollout while its cluster
  workloads silently stayed on the previous release — a green deploy that
  shipped half a release. The apply runs first and a failure short-circuits the
  wait. Nothing is configured: the shape comes from the env's own render.
- **Status verbs for hosted deploys** (hosted-deploy-primitives §3.5):
  - `forge env rollout <env>`: where the current (or `--promotion`)
    rollout has got to, read once. It is `forge env wait --timeout 0`, with
    the same exit codes (0 succeeded, 1 degraded, 2 undetermined, 5 still
    rolling out, 6 superseded).
  - `forge env history <env> [--limit] [--before] [--release]`: the
    promotion ledger, newest first, with from-env, actor, note, evidence
    counts and the CI run. It pages with a keyset cursor (`next_before`;
    empty means the last page), and both ledger backends page identically.
  - `forge release where <version>`: which environments are currently bound
    to a release. Exit 1 when none are.
  - **`forge env verify` works for hosted envs.** forge cannot read a hosted
    env's cluster, so it reads the control plane's observer through
    GetRollout and reports the same five states with
    `source: control-plane observer`. A stale observation is UNREACHABLE
    (exit 2), never MATCH. Cluster envs are unchanged.
  - `forge env topology --json` gains `promotion_id`, `from_env`,
    `gates_summary` and, for hosted envs, `rollout_phase`.
- **`forge ci run <run-id> [--env <env>] [--json]`**: one CI run's timeline
  from the hosted control plane. It shows the release cut, the recorded
  checks, every promotion and each promotion's rollout, with a verdict.
  Exit codes: 0 every stage passed, 1 a stage failed or errored, 5 still
  running, 2 could not read the control plane. A run id nothing has written
  under exits 5, never 0. `--env` picks which control plane to ask;
  without it, the project's single declared control plane is used. The
  design spec named this `forge run show`; that would have collided with the
  dev-server `forge run`, which takes its own positional arguments.
- **`forge env verify` refuses to judge against a stale file ledger.** A
  self-managed env's promotions live in `.forge/promotions/<env>.jsonl`,
  committed to git, so a checkout that has not pulled the latest release
  record compares the cluster against an OLDER promotion. A fine deploy then
  reads as DRIFT, and a deploy that never happened can read as MATCH. verify
  now compares the env's log with origin's default branch (as of the last
  fetch; it never fetches). When this copy is `behind` or `diverged`, every
  image is still reported, but the command exits **2** (could not determine)
  with the fix. `ahead` (recorded here, not yet merged) is shown, not failed.
  `--json` carries `ledger: {state, ref, local_entries, upstream_entries,
detail}`. A control-plane ledger has no copy to be behind and is unaffected.
- **`forge env promote` always compare-and-sets.** The write asserts that the
  env is still on the promotion the plan read, and is refused if someone else
  moved it since. A hotfix that lands while a pipeline waits for approval now
  turns that pipeline red (exit 3, `promotion_conflict`) instead of being
  overwritten. No flag is needed. `--expect-current <id>` replaces the planned
  value with one captured earlier; `--expect-current unbound` /
  `--expect-unbound` asserts the env was never promoted. Re-promoting the
  release an env already runs is still a no-op whatever the expectation says,
  so a retried success is never a conflict. `--supersede` admits a promote
  while the current rollout is in flight and is recorded on the new entry.
  Exit 4 is `rollout_in_flight` / `environment_pinned`. `--plan --json` now
  shows `current.promotion_id` (the value to capture) and `expected`; a
  refused `--json` document carries `applied: false` and
  `refusal: {reason, expected…, actual_current, actual_phase}`, and its
  `ok`/`exit_code` match the process status. The file ledger applies the same
  check against the history it reads. Hosted CAS needs a control plane with
  the P0 contract — an older one ignores the fields.
- `forge env promote --run-id / --run-url / --no-run`: the promotion records
  the CI run, defaulted from `GITHUB_*` / `CI_PIPELINE_*`. `--wait`,
  `--deploy`, `--timeout`, `--fail-fast`, `--gate`, `--from` and
  `--from-promotion` are declared and refuse with "not supported by this
  forge build" until they are wired.
- `service_account_annotations: {str:str}` on `forge.K8sOverrides`,
  `forge.RenderedWorkload` and `forge.workloads.Workload`: annotations stamped
  on the ServiceAccount forge GENERATES for the workload, and on no other
  object. It is how a workload gets cloud workload identity (GKE
  `iam.gke.io/gcp-service-account`, EKS `eks.amazonaws.com/role-arn`) without
  renaming its identity — the SA keeps the workload's name, so Roles, bindings
  and policy that name it keep working. Keys are validated as Kubernetes
  annotation keys at render time, and combining it with `service_account`
  (where forge generates no SA) is refused rather than silently dropped.
- `forge env render` now includes every declared `forge.HelmChart`'s objects.
  They used to be missing entirely (flux, cert-manager and envoy-gateway each
  rendered 0 objects while `env deploy` applied them), so render was not a
  preview of deploy. Charts are templated through the deploy's own chart
  render (`helm template` with the declared values, the pinned CRD bundle,
  the chart's own CRDs, the post-hook drop), each document is marked
  `# source: helm chart <name>` and attributed to the cluster deploy applies it
  to, and `--target` selects charts the same way deploy does. Templating needs
  `helm` and registry access; `--no-charts` skips it, and the summary names the
  charts it left out.
- `cluster:manage` access-token scope (`accesstoken.ScopeClusterManage`):
  registers, rotates and removes an org's BYO clusters. Its own authority —
  `deploy:write` neither implies it nor is implied by it.

### Changed

- **Scaffolded CI: the build-once release pipeline owns hosted envs, and the
  broken pieces are gone.**
  - `deploy.yml` no longer rebuilds per env for a hosted env. If it did,
    staging and prod would run different digests, and `release.yml` builds
    once instead. A mixed env, one with both hosted and cluster parts, also
    moves wholly to `release.yml`. Its ledger is the control plane, so once
    it has been promoted, `forge env deploy` pins both halves to the
    release. A `deploy.yml` rebuild of its cluster half would then deploy
    the old digests and still report success. A mixed stage writes the
    kubeconfig, because `forge env deploy` applies that half from CI itself.
    When every env is hosted, `deploy.yml` is not emitted at all.
  - `build-images.yml`'s opt-in `cut-release` job is deleted. It cut and
    promoted by raw curl, read `DEPLOY_TOKEN` instead of
    `FORGE_CONTROL_PLANE_TOKEN`, and was broken against the server because
    it sent no artifact kind or mode.
  - `reconcile.yml` runs `forge env status <env> --json`. It used to call
    `forge reconcile`, which was never a verb, and its matrix expressions
    were missing their `$`.
  - Every rendered workflow is now checked by `actionlint`, when it is on
    PATH.
- **BREAKING (one require line): `github.com/reliant-labs/forge/pkg` is no
  longer a module.** forge is now a single module,
  `github.com/reliant-labs/forge`, carrying both the CLI and the `pkg/*`
  runtime libraries. **Import paths did not change** — a module at
  `github.com/reliant-labs/forge` with a `pkg/testkit` directory serves
  `github.com/reliant-labs/forge/pkg/testkit`, byte-identical to what the
  submodule served. Consumers change one require line and no source:

  ```sh
  go get github.com/reliant-labs/forge@vX.Y.Z
  go mod edit -droprequire=github.com/reliant-labs/forge/pkg
  go mod tidy
  ```

  Dropping the retired requirement is not optional, and it does not degrade
  gracefully: both modules can serve `forge/pkg/*` import paths, so a graph
  holding both answers every such import with `ambiguous import: found package
... in multiple modules`. `forge generate` now detects this before codegen
  and distinguishes a requirement you own from one a dependency drags in.
  A dependency's must be fixed in that dependency — a `replace` only hides it.

  The two modules were always released in lockstep, at the same commit, with
  the same version. Keeping that true took three hand-maintained syncs, and
  two failed in production: the root module's `require forge/pkg` went stale
  and made `go install .../cmd/forge@main` uninstallable, and a hand-listed
  registry of "forge/pkg symbols the generator emits" went stale when
  `testkit.StubNotConfigured` was added, so `forge generate` rewrote a project
  tree and then failed its own validate. One module makes both
  unrepresentable.

- **A dev build no longer pins a version it is not.** The scaffold's forge
  requirement comes from the binary's own build info: a release tag, or the
  pseudo-version `go install ...@main` records. A local or dirty build — whose
  bytes no module proxy can serve — now pins **nothing** and requires a
  `go.work` source bridge, which `forge generate` names in its refusal. It
  used to fall back to a hand-maintained "last published tag" constant,
  telling projects to require a release that could not satisfy the code being
  generated.

- **`forge generate`'s compatibility gate is a version comparison.** The
  project's forge must be `>=` the generating binary's. This replaces a probe
  that compiled a throwaway program against a hand-listed symbol set; the
  inequality covers every symbol ever added, cannot rot, and costs one
  `go list` instead of a `go build`. A project resolving a NEWER forge still
  passes — that is the ordinary upgrade order.

- **`task release:forge` is one tag, one commit.** The two-tag atomic push, the
  `go.sum` bare-clone resolution dance, and the `defaultPublishedForgePkgVersion`
  bump all existed only because the root module required an unpushed submodule
  version. `scripts/release-pkg.sh` and `task release:pkg` are removed.

### Fixed

- **A store-backed Secret now reaches EVERY cluster that consumes it.** A
  `FileSecrets` (or pulled local hosted) provider projects the Secrets its
  cluster workloads declare by `secret_ref` — but forge applied that
  projection once, into the env's primary kubectl context. A multi-cluster
  dev env whose second cluster also reads the Secret got nothing there, so the
  pod stalled on CreateContainerConfigError. control-plane worked around it by
  declaring the Secret a `forge.ExternalSecret` (which then blocked every
  fresh worktree's deploy preflight) and creating + mirroring it with a shell
  script, whose randomly generated HMACs then disagreed with the store values
  forge projected into the first cluster. The projection is now placed per
  consuming (cluster, namespace), with exactly the keys that cluster's
  workloads declare — the same trust boundary `RenderedSecrets` already keeps.
  A single-cluster env renders and applies exactly what it did before.

- **The dev databases exist before the first cluster workload is applied.**
  `ensureDevDatabase` already creates every declared dev DSN — including one
  an in-cluster workload reaches through `host.k3d.internal` — but it ran in
  `forge env up`'s host phase, which starts only after the cluster rollout
  converges. A cluster workload dialing a per-worktree database therefore
  crash-looped on `database "…" does not exist`, the rollout never converged,
  and the step that would have created the database never ran. The deploy
  dispatch now ensures the dev databases at the infra→cluster boundary — after
  compose / host-infra have brought the server up, before any cluster group is
  applied — so `forge env up dev` and an applying `forge env deploy dev` both
  start a fresh stack with no manual `CREATE DATABASE`. Dry runs create
  nothing; non-dev envs are untouched (the same fail-closed dev classifier).

- **`forge env up` from a linked git worktree no longer takes over and
  recreates compose infrastructure another checkout is running.** Compose names
  a project after its directory's basename and resolves relative bind mounts
  against that directory. Two worktrees laid out as `<container>/<repo>` share
  one basename, so they selected the SAME project with DIFFERENT mount sources:
  compose saw a changed config and recreated every shared container, which
  dropped every other stack's connections and left the containers mounting
  files inside that worktree.

  - New `forge.OnCompose {shared = True}`: the stack is machine infrastructure
    every worktree uses, so forge drives it from the repo's PRIMARY checkout
    (`--project-directory`, with `file` and `env_file` resolved there) from
    whichever worktree runs. An `up` from any checkout is then a no-op once the
    stack is running. A stack a worktree had already taken over is converged
    back to the primary on the next run, with one recreate.
  - New `fp.write_file(path, content, shared=True)`: writes into the primary
    checkout, so every stack's render updates the ONE copy of a file the
    shared stack mounts, instead of N diverging per-checkout copies.
  - Every compose deploy now checks container ownership first. A non-shared
    deploy whose containers carry another LIVE checkout's
    `com.docker.compose.project.working_dir` is refused before pull/up, with
    both fixes named. Containers from a checkout that no longer exists are
    adopted.

- **A cert-manager `Certificate` in the bundle now SUPPLIES the Secret it
  materialises, so the deploy preflight stops false-failing on it.** The
  render-time secret back-propagation gate recognised only `kind: Secret`
  documents as in-stream supply, so a workload mounting a Secret that a
  `Certificate` in the SAME bundle creates was reported as an undeclared
  mount. `forge env deploy e2e` refused to run on the vendored
  cloudnative-pg barman-cloud plugin, whose Certificates and the Deployment
  mounting `barman-cloud-{client,server}-tls` render in one stream, and the
  only way past it was `--skip-preflight` — which disables the image, CRD and
  live-Secret checks too.

  cert-manager writes the Secret when it reconciles the Certificate, in the
  same apply pass that creates the pods mounting it, exactly as
  `kubectl apply` orders a rendered `kind: Secret` ahead of its consumers.
  Demanding it exist beforehand made a first deploy of any cert-manager-backed workload
  unsatisfiable by construction: the thing that provisions the Secret was the
  very deploy being blocked. The advice the gate printed — declare a
  `KubeconfigSecret` or an `ExternalSecret` — was actively wrong for a
  cert-manager Secret, whose bytes cert-manager owns and would overwrite.

  Matching is on the `cert-manager.io` API group (any version), so a bundle
  on `v1alpha2` or a future `v2` is recognised without a forge change, and an
  unrelated CRD that happens to be named `Certificate` is not. A Certificate
  supplies only the one name in its `spec.secretName`; every other undeclared
  mount in the same bundle still fails.

- **The frontend bundle and image tests run in the e2e lane, on a minimal
  fixture.** The `next build` peer-pin test scaffolded a whole frontend
  (OpenTelemetry web instrumentation included) inside the unit lane's
  `go test -race ./...`, where the 7GB runner OOM-killed it (`signal: killed`
  after up to 1029s) and failed the Test job on unrelated PRs. It and the
  static-image `docker build` test now live in `internal/cli` under
  `-tags e2e`, on the e2e shards' own runners, where a missing tool is a
  failure rather than a skip. The pin test installs only what its claim
  needs (33 packages; ~35s locally for both of its builds) and still proves
  both halves: directory pins fail with `No QueryClient set`, and forge's
  post-install reconcile makes the build prerender. Each node process is
  capped at `--max-old-space-size=2048`. The e2e shard now verifies a docker
  daemon answers before the suite runs.
- **`forge tools install` never mutates a frontend's `package.json` or
  lockfile.** It used to run `npm install --save-dev @bufbuild/protoc-gen-es`
  in every frontend under `--force` — which the scaffolded verify-generated
  job passes — and in any frontend without `node_modules` yet. On a Linux
  runner npm re-saved a macOS-written `package-lock.json` without its `libc`
  entries, and `forge ci verify-generated` reported that user-owned file as
  generated-code drift; committing either OS's lockfile moved the failure to
  the other OS. The plugin is now installed only by the frontend's own
  `npm ci`, from its declared devDependency. `forge tools install` checks
  that every frontend whose buf.gen.yaml runs the local plugin declares it,
  and fails with the edit to make when one does not. `--force` reinstalls the
  Go tools only.
- **`forge ci verify-generated` no longer blames `forge generate` for a file
  an earlier step changed.** It refuses to regenerate over an
  already-modified tree and lists those paths as changed before
  `forge generate` ran, instead of reporting them as out-of-date generated
  code.
- **A Next.js frontend's tsconfig peer pins no longer split packages in the
  bundle.** The pins named package DIRECTORIES, and Next's webpack resolver
  applies tsconfig `paths` to app code: wherever a pin resolved (an ordinary
  `npm ci` in the frontend, or a Docker build at `/app`), app code bundled
  `@tanstack/react-query`'s `build/legacy` while
  `@reliantlabs/forge-web-runtime` bundled `build/modern`. Two module instances
  are two React contexts, and prerender failed with `No QueryClient set, use
QueryClientProvider to set one`. Every pin now names the package's
  declaration file (e.g. `…/@tanstack/react-query/build/modern/index.d.ts`,
  `react` → `@types/react/index.d.ts`), derived from the INSTALLED manifest,
  never hardcoded, because the entry moves inside the declared ranges
  (`@opentelemetry/sdk-trace-base` 2.0 → 2.11). Next's resolver skips a `.d.ts`
  target and Vite ignores `paths`, so bundles resolve through `exports`, while
  tsc still binds one copy of the types — which the pins remain necessary for.
  `forge project new` and `forge scaffold frontend` write them after their
  install; `forge generate` heals an existing project's directory pins
  wherever they resolve, once, and never rewrites a declaration pin.
- **The Next.js frontend Dockerfile follows `frontends[].output`.** It always
  copied `.next-prod/standalone`, which only `output: standalone` produces, so
  a `static` frontend's image (and a `server` one's) failed with
  `COPY .next-prod/standalone: not found` — and CI builds an image for every
  frontend. `static` now serves `out/` from `nginxinc/nginx-unprivileged`
  (uid 101, port 8080, mounted under `base_path`), `server` runs `next start`,
  and `standalone` keeps the node server. The build runs at the frontend's
  repository path (`/src/frontends/<name>`) instead of `/app`, installs with
  `npm ci`, and the node runners drop the bundled npm/corepack trees, which
  carried every HIGH CVE in `node:22-alpine`. The scaffold ships a
  frontend-level `.dockerignore`, since the build context is the frontend
  directory. The Dockerfile is scaffold-once: `forge project upgrade --check`
  reports the new one to existing projects.

### Removed

- `database.migration_safety.down_files_allowed_until` and the
  `forge project upgrade` step that stamped it. Every down migration is now a
  `no-down-migration` lint error, with no grandfathered history: forge never
  runs one, so deleting it is always safe. A forge.yaml still carrying the key
  loads with a warning naming the fix.
- `go.work` from forge's own repo. It existed to stitch `pkg` to the root
  module, and it was also what hid the stale-require bug: every in-repo build
  resolved `./pkg` locally and stayed green while `@main` was uninstallable.
- `buildinfo.PkgVersion`/`SetPkgVersion`, `PkgModuleVersion` (already dead),
  `Build.PkgPath` and the `forge/pkg` line in `forge version` — a second
  version axis with nothing left to describe.
- `docs/pkg-versioning.md`, replaced by `docs/versioning.md`.

## [0.1.11] - 2026-09-01

Patch release. The root CLI (`v0.1.11`) and the runtime library
(`pkg/v0.1.11`) are tagged as always — on adjacent commits, `pkg` on the
release commit and the root tag one commit later on the require bump.

This release bounds the parallel-dev-stack port allocator, teaches the
block registry what kind of key it is holding, and repairs two k3d
failures that presented as something other than what they were.

### Added

- **`dev_stack:` in `forge.yaml`** — `max_stacks` (default 8: the primary
  checkout plus seven worktrees) and `block_size` (default 100, the
  historical hardcoded quantum). The parallel-stack ceiling was
  previously stated twice and tied together nowhere: the allocator was
  unbounded while a project pre-maps host ports at cluster-CREATE time,
  so a worktree past the last mapped block allocated cleanly and then
  failed later as "gateway unreachable". `AllocateBlock` now refuses a
  NEW block past the ceiling with the remedy in the error. An EXISTING
  entry always resolves regardless — an issued block is baked into k3d
  mappings and the dev IdP's `iss` claim and redirect URIs, so lowering
  the ceiling must never move a port already handed out.
- **`forge env devstack prune`** reclaims blocks whose git worktree no
  longer exists, which is what keeps the now-finite block range from
  filling with leaked entries. Dry-run by default. It never touches a
  plain port-block key or the default key, and if enumerating live
  worktrees fails for any reason it reclaims nothing.
- **`fp.dev_stacks()` KCL builtin** — the roster of registered dev-stack
  keys, so a module generating one config block per running stack never
  parses `.forge/blocks.json` itself. It returns EMPTY on a read-only
  render, which keeps `forge generate` a pure function of committed
  inputs; otherwise a generated tracked file would capture whichever
  worktrees happened to exist on that machine.
- **`forgeconv-allocate-port-spacing`** — two `allocate_port` base
  literals congruent modulo `block_size` collide at the block equal to
  their separation. The rule fires only when that block is reachable at
  the project's configured `max_stacks`, which keeps it actionable
  rather than flagging every latent pair.

### Fixed

- **Rendering or deploying a cloud env no longer claims a local port
  block.** `forge env render` / `forge env deploy` armed the persistent
  `fp.allocate_port` block registry for every env, so rendering prod from a
  linked worktree registered a new `prod-<worktree>` block in the primary
  checkout's `.forge/blocks.json`. That leaked one permanent block per
  throwaway worktree, went unseen by `--fail-on-write` because the registry
  sits outside the worktree, and failed the render outright once the registry
  reached `dev_stack.max_stacks`. These commands now claim a block only when
  the env's declaration runs on this machine: a k3d cluster, a host, compose
  or host-infra service, or a workload on a local context. Otherwise
  `allocate_port` returns its base port and nothing is written. `forge env up`
  is unchanged, including `forge env up prod --target <frontend>`.
- **A stale k3d serverlb upstream no longer reads as a flaky cluster.**
  k3d's serverlb is nginx configured with the server node's NAME and no
  `resolver` directive, so it resolves each upstream once at startup and
  caches it for the process lifetime. When the node later gets a
  different Docker IP, nginx keeps proxying to the cached address; with
  `max_fails=1 fail_timeout=10s` the symptom is not a clean outage but
  ~10-second "connection refused" windows that recover on their own.
  Forge now compares container start times and refreshes the load
  balancer before the API probe and after `k3d cluster start`.
- **A declared `api_port` is no longer dropped** for clusters that also
  name a k3d config file. `k3d cluster create --config` accepts no
  per-flag settings, so the value parsed, validated, and then did
  nothing while k3d picked a random host port. Where the config file
  already sets `kubeAPI.hostPort`, the file wins if they agree and forge
  refuses if they disagree.
- **The cluster health probe reports why it failed.** It previously
  collapsed every distinguishable failure into a bare false, after which
  forge printed "the API is still starting" — a diagnosis it had never
  established.
- **A node sampled moments after restart is no longer misreported as
  dead.** k3d's entrypoint runs its own setup before it execs k3s, so
  "no k3s process" must now persist for a grace period before counting
  as exited.
- **The block registry distinguishes a dev-stack key from a plain
  port-block key.** A bare `{key: block}` map could not, so a generator
  reading it raw emitted a dev NATS account for a prod web port into a
  tracked config file. The legacy bare-int form is still accepted.
- **A frontend `npm install` is retried once during
  `forge project new`.** A single transient registry failure previously
  degraded into a scaffold with no `node_modules`, discovered much later
  as some other tool reporting a package it could not find.

## [0.1.10] - 2026-08-28

Patch release. The root CLI (`v0.1.10`) and the runtime library
(`pkg/v0.1.10`) are tagged as always — on adjacent commits, `pkg` on the
release commit and the root tag one commit later on the require bump.

v0.1.9 made a deploy fail when a workload never became ready. This
release fixes the two ways that new strictness, and `forge generate`,
could report a failure that had not happened.

### Fixed

- **A successful deploy is no longer failed by waiting on one-shot Jobs
  that were never applied.** forge names a one-shot Job by its spec hash,
  but the rollout wait unioned the manifest-derived (hashed) names with a
  caller-supplied list that derived the UNHASHED entity names. Those names
  matched no object in the namespace, `kubectl wait` errored on each, and
  v0.1.9's policy — correctly, for a Job that genuinely did not run —
  turned that into a failed deploy. A real prod deploy applied 201
  objects, rolled all 13 Deployments, completed both one-shot Jobs, and
  was still reported as failed. The wait set is now every `kind: Job` in
  the stream the apply just sent and nothing else: authoritative by
  construction, already filtered by `--target` and scoped to the right
  cluster. A Job that was applied and then FAILED still fails the deploy,
  and a Job that failed to APPLY is still caught by the kind-agnostic
  apply-completeness check.

- **`forge generate` no longer leaks machine-local state into tracked
  files, and `forge lint` no longer fights the generator over `gen/`.**
  Evaluating a KCL module fires every `file.write` in it, bypassing
  forge's write chokepoint, so a generate on a clean checkout could emit
  bytes derived from a gitignored registry — two developers on the same
  commit produced different output. Generate-path renders now revert any
  tracked file that went clean → dirty across them.

## [0.1.9] - 2026-08-28

Patch release. The root CLI (`v0.1.9`) and the runtime library
(`pkg/v0.1.9`) are tagged as always — on adjacent commits, `pkg` on the
release commit and the root tag one commit later on the require bump.

The theme is deploys that report the truth. `forge env deploy` could
previously exit 0 over an environment that never came up, in two
independent ways: a workload that never became ready was a warning, and
a kubectl apply that confirmed fewer objects than were rendered went
unnoticed entirely.

### Added

- **`RolloutPolicy`: a deploy now fails when a workload never becomes
  ready.** `forge env deploy` printed a warning and exited 0 when a
  Deployment or a one-shot Job failed to roll out, so a prod deploy
  reported success over a broken environment — the failure surfaced
  later, somewhere else, looking like a different bug. Readiness is now
  part of what "deployed" means.

  Four flags govern it: `--rollout wait|warn|skip` (the previous
  exit-0-on-failure behavior is still reachable as `warn`, and `skip`
  opts out), `--rollout-timeout` (default raised from 60s to 5m, since
  60s failed honest-but-slow rollouts), `--rollout-fail-fast`, and
  `--rollout-order`.

  A failed Job now fails the deploy rather than being ignored, and it
  fails as soon as Kubernetes says so: both the `complete` and the
  `failed` conditions are watched, so a Job that has already failed no
  longer burns the full timeout before reporting it.

- **`forge env up` resolves GitSource frontends.** Only build and deploy
  did, so a frontend declared with a cross-repo `source:` dev-served by
  running npm in the project root — against the wrong tree, or against
  no frontend at all. `up` now resolves the source like its siblings.

### Fixed

- **A deploy fails when kubectl confirms fewer objects than were
  rendered.** The apply step trusted a zero exit code and never compared
  what came back against what went in. In the v1.5.6 incident a prod
  deploy applied 103 of 105 rendered objects and reported success; the
  two missing objects were found by hand, much later. The count is now
  checked, and a short apply is a failed deploy.

### Changed

- The toolchain moves to go 1.27, and the govulncheck pin rises to
  v1.7.0. These are one change, not two: v1.1.4 vendors an `x/tools`
  that panics under 1.27, so the old pin cannot scan the new toolchain.
- CI caches each Go module separately, rather than treating the
  multi-module repo as one cache key.
- Scaffold guidance now tells the reader to FIX forge when forge is what
  stands in the way, instead of working around it. A workaround plus a
  TODO is the outcome that rule exists to prevent — it buries the signal
  that tells us what to fix.

## [0.1.8] - 2026-08-27

Patch release. The root CLI (`v0.1.8`) and the runtime library
(`pkg/v0.1.8`) are tagged together, as always.

Everything under [Unreleased] below ships in this release: `forge env
render`, the cluster-health and object-collision doctor checks, the
`forge.dev/env` ownership stamp on every rendered object, seedplan tuple
and union support, and the testreport package.

## [Unreleased]

### Added

- **`forge env render <env>` — read what an environment renders, without
  deploying it.** There was no supported way to ask "what objects does this
  environment own". `kcl run` fails on a real project (it needs forge's
  `kcl_plugin.forge` harness), `forge doctor` renders every environment and
  reports a count, and `forge env deploy --dry-run` is the deploy command:
  it resolves a kubectl context, runs the declared-cluster guard, and refuses
  outright when the recorded build is behind HEAD. So the answer got
  reconstructed by hand. One audit read 1,886 lines of KCL and inferred each
  workload's target lexically — HOST from the presence of a `host =` block,
  cluster from a `k8s = K8sOverrides{cluster = ...}` override a thousand
  lines further down. One misread there deletes the wrong object out of the
  wrong cluster.

  The command prints the manifests a deploy would apply as a `---`-separated
  YAML stream, each document preceded by a `# cluster:` comment naming where
  it lands — a comment, so the stream still pipes into `kubectl diff -f -`.
  Attribution matters because an environment renders ONE stream and may
  deploy it to several clusters (control-plane's dev: most workloads on
  k3d-control-plane, workspace-proxy on k3d-cp-daemon), and it is not
  modelled a second time here: each document is handed to
  `internal/cluster.ScopeManifestsToGroup`, the function that performs the
  routing at deploy time, once per cluster. A document replicated to every
  cluster (an unattributed Namespace) is printed once with both named, so
  the object count equals the render's own — `forge doctor`'s 341 across
  control-plane's five environments is 30 + 71 + 58 + 76 + 106. `--cluster`
  narrows to exactly one cluster's stream; `--list` gives the inventory view;
  `--kind` / `--name` / `--target` narrow further; a non-zero exit carries
  the KCL error, so it works as a CI gate.

  **It does not claim to be pure, because it cannot be.** KCL evaluates
  `file.write` during rendering, so a project whose deploy KCL generates a
  file writes it every time anything renders — control-plane's dev main.k
  calls `nats.write_conf`, and `forge doctor` has been rewriting
  `deploy/nats/nats.conf` on every run all along. forge has no hook to
  suppress a project's own writes: the write happens inside the embedded KCL
  runtime, which has no read-only mode, and imposing one from outside
  (rendering from a copy of the tree, sandboxing the process) is neither
  portable nor cheap. So the command reports instead of promising: it stats
  the project tree before and after and names every file that changed, and
  `--fail-on-write` turns that into a non-zero exit for callers that need
  the guarantee. What forge CAN suppress it does — its own resolve_port
  store write is reverted, content and mtime, so it never shows up in the
  report as forge blaming the render for forge.

- **`forge ci verify-test-run` — a green suite that ran almost nothing is now
  visible.** A Go suite that skips its way through exits 0 and reads as green.
  Measured on one real package, one environment variable apart: 9 pass / 124
  skip without `DATABASE_URL`, 103 pass / 1 skip with it — same exit code, 7%
  of the tests, and nothing in the pipeline had a word to say about it.

  The new command reads the run's own `go test -json` output — from a pipe or
  `--from a-file` — and reports the packages whose pass therefore proves
  nothing. It runs no tests: `forge test` was removed precisely because a
  second spelling of the project's suite reports on a different suite than the
  one the project runs, so this reads the record of the run that already
  happened. On a 176-package, 482 MB stream it takes 2.5 seconds.

  Two rules, because skips are legitimate and a gate that fires on every skip
  gets switched off: `zero-evidence` (every test in the package skipped — no
  sample-size floor, "none of them ran" is unambiguous at any size) and
  `mass-skip` (more than `--max-skip-ratio`, default 0.5, of at least
  `--min-tests`, default 5). Healthy packages are never listed. Across the 112
  tested packages of the reference project in its broken environment the pair
  reports five, and the ratios above the line are 1.00 / 0.95 / 0.80 / 0.60 /
  0.56 against a next-highest of 0.20 — the threshold sits in an empty band,
  not through the middle of a crowd.

  Genuinely-expected heavy skipping is declared once in forge.yaml under
  `ci.test_skips.allow`, with a REQUIRED `reason` — the same contract as
  `forge project disown <path> --reason`. A declaration that stops suppressing
  anything is reported as no longer needed rather than left to rot; one that
  matches no package in the run is silent, so a scoped `go test ./internal/x/`
  does not drown in irrelevant notices.

  Three states, not two: input carrying no `go test -json` events, or a stream
  that ends mid-run, is UNDETERMINED and exits non-zero — the command never
  reports a clean run it could not read, and `--warn-only` does not downgrade
  that. Failures in the stream also fail it, because
  `go test -json ./... | forge ci verify-test-run` in a shell without
  `set -o pipefail` reports only the last command's status.

### Changed

- **Scaffolded services run pprof by default.** `pprof_addr` shipped with
  no default, so a scaffolded binary declared `PPROF_ADDR` on the deploy
  side and started no listener — the env var was projected onto every
  workload and read by nobody, and `forge env status` reported
  `? pprof — could not resolve a pprof address` about it. A gateway then
  sat at ~1 GB of anonymous memory being OOMKilled with no way to ask the
  process WHAT it was holding: a cgroup's `memory.stat` says how much and
  never what, and the answer only exists while the process is still alive.

  The default is now `127.0.0.1:6060`, and loopback is what makes always-on
  safe: the listener exists in every environment and is routable from none
  of them. It stays on its own serverkit listener (never the public port,
  whose endpoints sit behind a k8s Service), and the scaffolded manifests
  give it no Service, route, or container port — reach it with
  `kubectl port-forward <pod> 6060:6060` and then
  `go tool pprof http://localhost:6060/debug/pprof/heap`. The scaffolded
  `docker-compose.yml` overrides it to `0.0.0.0:6060`, the one place a
  profile has to cross a container boundary (Alloy scrapes it for
  Pyroscope). `PPROF_ADDR` / `--pprof-addr` moves it; `""` switches it off.

- **A pprof bind failure can no longer take a service down.** `serverkit.Run`
  routed it through `FailurePolicy`, so with the default `FailProcess` a busy
  debug port terminated the process. That was defensible while pprof was
  opt-in; with it on by default the ordinary cause is banal — a second copy
  of the binary on the same dev box — and a debug surface that can kill the
  service is worse than no debug surface. Run now logs it and serves on,
  under either policy. `FailurePolicy` continues to govern supervised
  components (workers, operators) unchanged.

## [0.1.0] - 2026-08-17

First minor release. The root CLI (`v0.1.0`) and the runtime library
(`pkg/v0.1.0`) are tagged together, as always.

### Added

- **The generated scaffold test row is falsifiable.** Every scaffolded
  handler ships a self-destructing test row that asserts the RPC is not
  implemented yet, so it goes red the moment you write the handler.
  Keyed on a bare `connect.CodeUnimplemented`, that row could never
  fail: a FINISHED handler can answer the same code for its own reasons
  — a forwarder, a feature-flagged path, or most commonly a nil-guard on
  an optional dep the test harness leaves unset — so the row passed
  forever against implemented RPCs. Observed in one project: 78 of 78
  integration rows green, none of them asserting anything.

  `svcerr.ErrScaffoldStub` / `svcerr.ScaffoldStub(rpc)` is a sentinel
  only forge's own untouched stub can produce, and `tdd.Case` gains
  `WantScaffoldStub` (backed by the exported `tdd.AssertScaffoldStub`)
  to match it. Replace the stub and the row fails, whatever the
  replacement returns. It is deliberately NOT matched by
  `svcerr.Unimplemented`, which remains the right answer for an RPC that
  is unimplemented on purpose.

  Identification is dual because the tiers differ: in process the error
  chain is intact and `errors.Is` matches, while through a real Connect
  client the error is marshalled and rebuilt, so only the
  `svcerr.ReasonScaffoldStub` metadata survives. Both are accepted, or
  unit rows and integration rows would mean different things under one
  field name.

  **Existing projects need no migration.** Stub excision still
  recognises the older `Unimplemented` and `CodeUnimplemented`
  spellings, so stubs already on disk stay excisable. Generated stubs no
  longer import `fmt` (the message is composed from the RPC name), and
  the AST import-fixer no longer adds it.

- **npm license gate for the published web-runtime.**
  `scripts/check-npm-licenses.sh` is the npm twin of `check-licenses.sh`
  — same allowlist-not-blocklist bar, same "an unrecognized license
  fails rather than passing quietly", same per-package exceptions that
  must state a reason. `web-runtime` is published to npm, so a
  non-permissive dependency would propagate into every project that
  installs it. The strong-copyleft family, LGPL included, is not
  exemptable through the allowlist. web-runtime passes with zero
  production dependencies.
- **Entity protos are dead: SQL is the schema language.** `forge scaffold
entity bookmark url:string title:string tags:[]string done:bool`
  emits the create-table migration (`db/migrations/NNNNN_create_*.sql`
  — `TEXT PRIMARY KEY CHECK (id <> '')`, NOT NULL + defaults, native
  arrays, `created_at`/`updated_at TIMESTAMPTZ DEFAULT (now())`;
  `--soft-delete`, `--no-timestamps`, `--no-rpcs`) and scaffolds the
  CRUD wire contract into the service proto once. `forge generate`
  shadow-applies `db/migrations` to a real ephemeral Postgres, introspects, and
  projects entity structs + ORM (`internal/db/<entity>_orm.go`, plain
  Go types: `time.Time`, pointers for nullable, native slices), CRUD
  wiring with generated `<entity>ToProto`/`<entity>FromProto`
  conversions, and frontend pages/nav/mocks — all from the APPLIED
  schema. Behavior is read off real columns: `deleted_at` ⇒ soft
  delete, `created_at`+`updated_at` ⇒ managed timestamps, text columns
  ⇒ search filter span.

### Removed

- **`deploy/kcl/components_gen.json`. KCL is the sole source of truth for
  deploy.** The generated component inventory is gone — the file, the
  `codegen.GenerateComponentsJSON` / `ComponentsToJSON` /
  `ComponentsJSONRelPath` Go API that wrote it, and the
  `fc.load_components` / `fc.load_migrate` / `fc.COMPONENTS_GEN` KCL
  loaders that read it. No alias, no shim, no fallback.

  What replaces it is `deploy/kcl/components.k`: one typed literal per
  component (`fc.Server {name = "billing", build = {...}}`), **scaffolded
  once and owned by the project from then on.** `forge scaffold
service|worker|binary|operator` APPENDS an entry; nothing forge does
  ever rewrites what is already there.

  Why: a file forge regenerated every run could not hold the per-env
  customization KCL exists for — bringing in components forge never
  generated (NATS, a cache, a sidecar), running a containerized database
  in dev against a hosted one in prod, changing a port per environment.
  It was also gitignored, so a FRESH CLONE rendered **zero manifests,
  silently**, until someone ran `forge generate`. Now every deploy file
  is tracked and hand-editable, and `kcl run deploy/kcl/dev` works on a
  clean checkout with nothing generated first.

  Two axes, each owned by one place: `components.k` says WHAT the system
  is made of (env-neutral — no ports, no replicas); `deploy/kcl/<env>/main.k`
  says HOW it runs in that environment (ports, replicas, resources,
  registry, secrets, env-only components).

  The deploy-time migration step moved with it: `migrate` is now stated
  literally per env (`migrate = ["/app/<project>", "db", "migrate", "up"]`)
  instead of being derived, so an environment that migrates out of band
  sets `migrate = []` and one that migrates differently says so.

  Forge still NOTICES drift — a component in the code with no entry in
  `components.k` — but it REPORTS it (`forge lint`, with the exact stanza
  to paste) rather than rewriting the file. Both directions have
  legitimate deliberate cases, so neither fails a build.

  **Migration:** add a `deploy/kcl/components.k` declaring your
  components, replace `for c in fc.load_components(fc.COMPONENTS_GEN)`
  with `for c in comps.COMPONENTS` (plus `import ..components as comps`)
  in each env's `main.k`, replace `fc.load_migrate(fc.COMPONENTS_GEN)`
  with the literal argv, and delete the gitignore entry. `forge generate`
  scaffolds `components.k` for you if it is absent.

- **The per-component port.** `config.ComponentConfig.Ports`,
  `config.PortSpec`, `config.HTTPPortName`, `ComponentConfig.PrimaryPort()`
  and the `ports` key in `deploy/kcl/components_gen.json` are gone — no
  alias, no shim. A component never had a port to carry: every service in
  a binary mounts onto the SAME Connect mux and the process listens once,
  on `AppConfig.port` (env `PORT`, default 8080 — now
  `config.DefaultServePort`). Any other port is a DEPLOY fact, declared
  per environment on `forge.components.Component.ports` in
  `deploy/kcl/<env>/main.k`, which is unchanged and still the place to
  state one. The rendered manifests and the `forge build` / `forge env`
  JSON contract are byte-identical across the change: nothing ever
  populated the field, so `components_gen.json` shipped `"ports": []` for
  every component and every reader hit its fallback.
- The `Port` column in the generated `docs/generated/architecture.md`
  component table. It printed `0` for every component in every project.
- The `(forge.v1.entity)` / `(forge.v1.field)` annotation handling,
  `protoc-gen-forge mode=orm` (`*.pb.orm.go`), `proto/db` scaffolding,
  proto-derived migrations (`--from-proto`, the boilerplate entity
  migration), `forge db proto`/`forge db codegen`, the
  `proto_migration_alignment` audit category, the
  `proto-orm-out-of-sync` lint, and the `internal/db/types.go` proto
  alias file. The annotation definitions remain in
  `forge/v1/forge.proto` as deprecated tombstones (ignored, with a
  generate-time notice) so existing projects keep compiling; their
  migrations were already the de-facto schema truth — see the
  `proto-entities-to-schema-truth` migration skill.
- Initial project scaffold generated by `forge project new`.
- `forge/pkg/serverkit`: extracted the uniform `cmd/server.go` lifecycle
  (HTTP listener, observability chain, healthz/readyz, worker supervisor,
  operator manager, graceful shutdown) into a reusable library. The
  generated `cmd/server.go` is now a ~50-line shim that projects the
  project's typed `*config.Config` onto `serverkit.Config` and wires
  per-project `Hooks` (Bootstrap, PostBootstrap, AutoMigrate, SetupOTel,
  ProjectInterceptors, CORS/SecurityHeaders/RequestID middleware factories).
  Cuts ~470 lines from every project's generated output.

### Changed

- **One custom RPC, one handler file.** The generate pipeline now
  scaffolds each unimplemented custom RPC into its own
  `internal/handlers/<svc>/rpc_<snake_name>.go` — package clause,
  imports, one `*Service` method, its `// forge:gen unwired-stub
symbol=<pkg>.<Method>` marker — instead of piling every RPC into one
  shared `handlers.go`. This is the same filename `forge scaffold rpc`
  has always written for an RPC that is not in the proto yet, so the two
  paths agree; previously the layout depended on whether you scaffolded
  the RPC before or after declaring it in the proto. A handler package's
  file layout is invisible to Go and to every forge reader (all of them
  walk the directory), so it is chosen for the one thing it does decide:
  who has to merge with whom. Two authors implementing two RPCs of the
  same service now have nothing to split. This matches the scaffold
  TESTS, which have been one file per RPC
  (`handlers_scaffold_<rpc>_test.go`) since the same reasoning was
  applied to them. When CRUD gen excises an RPC's stub because the RPC
  became entity-backed, the emptied `rpc_<name>.go` is removed with it
  rather than left as a bare `package x` husk. **Existing projects need
  no migration:** forge only ever writes a stub for an RPC it finds
  unimplemented, so a `handlers.go` already holding your handlers is
  never read, rewritten, or re-stubbed — it keeps working exactly as it
  is, and only NEW RPCs land in per-RPC files. The `forgeconv-handler-
file-size` lint now names `rpc_<name>.go` in its remediation instead of
  a third spelling.
- **Scaffolding is one verb: `forge scaffold`.** Arity picks the
  granularity — bare `forge scaffold` births every `// forge:entity`-marked
  message the protos imply and then projects them; `forge scaffold <noun>
…` scaffolds exactly one thing (`entity`, `service`, `worker`,
  `operator`, `crd`, `binary`, `frontend`, `scenario`, `webhook`,
  `package`, `adapter`, `library`, `handler-file`, `rpc`). This replaces
  `forge project add <noun>` and `forge project scaffold`, which were the
  same operation spelled as unrelated commands. There is no alias and no
  deprecation shim: the old spellings exit non-zero with `unknown
command`. `forge project` keeps the commands that act on the project as
  a whole — `new`, `delete`, `disown`, `migrate`, `upgrade`, `map`,
  `graph`, `introspect`, `features`, `annotations`, `audit`.
- `pkg/app/app_gen.go`: `RESTHandler` is now a method (`func (a *App)
RESTHandler() http.Handler`) backed by an unexported `restHandler`
  field. Required by the `serverkit.Application` interface contract;
  the renamed accessor preserves the public read path
  (`app.RESTHandler()` instead of `app.RESTHandler`).
- `pkg/app/bootstrap.go`: `WorkerList()` returns `[]serverkit.Worker`
  (was `[]*WorkerInstance`); `OperatorList()` returns
  `[]serverkit.Operator` (was `[]*OperatorInstance`). The wrapper types
  unchanged — they satisfy the interfaces directly.
- `pkg/app/bootstrap.go`: `RunOperators` signature gains
  `healthProbeAddr string` so projects that bind a controller-runtime
  probe listener can forward `serverkit.Config.OperatorHealthProbeAddr`.

### Migration

- See the new `v0.x-to-serverkit` migration skill for the upgrade path,
  including the manual edits required for projects whose
  `pkg/app/bootstrap.go` is forge-forked (e.g. cp-forge).

### Deprecated

### Removed

### Fixed

- `forge env up --target <name>` did cluster work for targets that have
  no cluster deployment edge. Target selection filtered the manifest
  stream, which is too late: cluster creation and cross-cluster
  kubeconfig minting run before render/apply, so targeting a host
  service still created a k3d cluster and the deploy pipeline reached
  its empty-manifest fallback. Phase requirements are now derived from
  the rendered placement graph — host, build-only and dev-served
  frontend targets skip both phases; compose and external targets deploy
  without a cluster; cluster services, operators, platform charts and
  cluster frontends require both. Infra pre-warm remains independent, so
  host processes can still depend on compose services when nothing
  selected runs in Kubernetes. The build-plan summary now reflects the
  filtered set rather than announcing work that will not happen.
- `forge scaffold frontend` baked `DEV_API_URL = "http://localhost:0"` into
  the new frontend's `src/lib/apiurl_gen.ts` whenever the project already
  had a server component — it read the always-zero per-component port and
  overwrote its own 8080 default with it. Non-mock dev then pointed
  `connect.ts` at port 0 until the next `forge generate` happened to
  rewrite the file.
- `forge project audit`'s ingress category could never emit its
  "declared but no route" finding: it gated on a port that was always 0,
  so it reported `0 service(s) without route` for every project. It now
  reports every SERVER with no route (workers/crons/operators/binaries get
  no k8s Service, so nothing can route to them).

### Security

[Unreleased]: https://github.com/reliant-labs/forge/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/reliant-labs/forge/compare/v0.0.8...v0.1.0
