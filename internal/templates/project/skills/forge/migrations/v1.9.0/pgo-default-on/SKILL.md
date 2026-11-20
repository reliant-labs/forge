---
name: migrations/v1.9.0/pgo-default-on
description: Profile-guided builds on by default (forge v1.9.0). A project scaffolded since v1.9.0 keeps continuous CPU profiles ON in its hosted envs (`signals = forge.ObservabilitySignals {profiles = True}` on the hosted bundle's observability, which `forge.Observability {}` left off) and gets `.github/workflows/pgo-refresh.yml`, a weekly `forge pgo pull --env prod` that opens a pull request with each Go binary's refreshed cmd/<bin>/default.pgo. An existing project opts in: set profiles = True in each hosted env's main.k, let `forge generate` add the workflow, store an obs:read token as FORGE_OBS_TOKEN, then commit the first pull. Use when a deploy/kcl/<env>/main.k binds forge.OnHosted and declares `observability = forge.Observability {}`, or a hosted project has no pgo-refresh.yml.
version: v1.9.0
detection: grep -lqs 'forge.OnHosted' deploy/kcl/*/main.k && { grep -qsE 'observability = forge\.Observability \{\}' deploy/kcl/*/main.k || test ! -f .github/workflows/pgo-refresh.yml; }
---

# Profile-guided builds on by default (v1.9.0)

forge has built every Go binary with its committed `cmd/<bin>/default.pgo`
since v1.8.0 (Go's default `-pgo=auto`), and `forge pgo pull --env prod`
writes that file from production's CPU profiles. Two things kept a new hosted
project from ever getting one, and v1.9.0 scaffolds both on:

1. **Profiles were off in hosted envs that declare an observability block.**
   `signals.profiles` unset means "on where forge runs the store", which is
   the local ClickStack only, so a hosted env with
   `observability = forge.Observability {}` (every scaffolded project with a
   frontend) rendered `OTEL_PROFILES_EXPORTER=none` and prod had no CPU
   profiles to pull. A hosted env with no observability block already
   profiles.
2. **Nothing refreshed the file.** A one-off `forge pgo pull` goes stale as
   the code moves.

Each section is independent. None changes a build until you commit a
`default.pgo`, and a missing profile never fails one.

## 1. Keep profiles on in hosted envs

**Use when** a `deploy/kcl/<env>/main.k` binds `forge.OnHosted` and its bundle
declares `observability = forge.Observability {}` (or a block with no
`signals.profiles`).

Set it explicitly:

```kcl
observability = forge.Observability {signals = forge.ObservabilitySignals {profiles = True}}
```

Keep any other field the block already sets. `forge env render <env>` should
no longer show `OTEL_PROFILES_EXPORTER=none` on the hosted workloads; the next
`forge env deploy <env>` starts sending a 5s CPU profile every 5 minutes per
process (about 0.15% of a core) and a live-heap snapshot every two hours.
Profiles count toward the env's telemetry usage. To stay off, write
`profiles = False` instead: that says so on purpose.

## 2. The weekly refresh workflow

**Use when** the project has a hosted env and no
`.github/workflows/pgo-refresh.yml`.

`forge generate` (or `forge project upgrade`) offers the file: it is
scaffold-once and yours after that, and `ci.workflows: {pgo-refresh: false}`
in forge.yaml declines it. It needs one repository secret, a token with
`obs:read` (Settings → API tokens in the web app):

```bash
gh secret set FORGE_OBS_TOKEN
```

Without it the job prints a notice and ends green.

## 3. The first profile

Once the env has served about a day of real traffic:

```bash
forge pgo pull --env prod        # writes cmd/<bin>/default.pgo per Go binary
forge pgo status                 # provenance, age and config per binary
git add cmd/*/default.pgo && git commit -m "chore(pgo): first default.pgo from prod"
```

A binary with too little CPU in the window is skipped with a note; pull again
later. Depth: `forge skill load observability/profiles`.
