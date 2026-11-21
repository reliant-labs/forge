---
name: migrations/v1.9.0/pgo-default-on
description: Profile-guided builds turn on by themselves wherever an env has CPU profiles (forge v1.9.0). A hosted project gets `.github/workflows/pgo-refresh.yml` on its next `forge generate` (new projects at `forge project new`): every Monday it runs `forge pgo pull --env <prod>` and opens a pull request with each Go binary's cmd/<bin>/default.pgo when the env's profiles changed it, and does nothing where there are none. It needs a FORGE_OBS_TOKEN secret with deploy:read,obs:read. Profile collection is not changed for an existing project; new scaffolds keep it on in hosted envs. Use when a deploy/kcl/<env>/main.k binds forge.OnHosted and no cmd/<bin>/default.pgo is committed yet.
version: v1.9.0
detection: grep -lqs 'forge.OnHosted' deploy/kcl/*/main.k && ! grep -qs 'pgo-refresh: false' forge.yaml && ! ls cmd/*/default.pgo >/dev/null 2>&1
---

# Profile-guided builds turn on where profiles exist (v1.9.0)

forge has built every Go binary with its committed `cmd/<bin>/default.pgo`
since v1.8.0 (Go's default `-pgo=auto`), and `forge pgo pull --env prod`
writes that file from the env's CPU profiles. What was missing was anything
that ran the pull. v1.9.0 adds it, and it applies itself.

## What changes by itself

**`.github/workflows/pgo-refresh.yml` appears on your next `forge generate`**
when the project has a hosted env (`ci.provider: github`). It is a
scaffold-once file like `release.yml`: yours after it is written, never
rewritten, and a deleted one stays deleted. Every Monday it pulls the last
hosted env in promotion order and:

- the env has profiles, and a binary's profile changed: it opens (or updates)
  one pull request with the new `default.pgo`. Merging it is what turns PGO on
  for that binary; every build after it uses the profile;
- the env has no profiles, or too little CPU in the week (`pgo.min_weight`):
  it commits nothing and ends green.

A build never fetches a profile: it reads the committed file, so it stays
reproducible from the commit alone, and a missing profile never fails one.

**Profile collection is not changed.** An existing project keeps whatever its
envs declare (`signals.profiles`; a hosted env with
`observability = forge.Observability {}` does not collect them, one with no
observability block does). A project scaffolded since v1.9.0 sets
`profiles = True` in its hosted envs. Profiles count toward the env's
telemetry allowance like logs and traces.

## What to do

1. Store the token the workflow reads, with exactly these scopes:
   `deploy:read,obs:read` (the pull looks the env up, then reads its
   profiles; it never deploys). Mint it under Settings → API tokens in the
   web app, then:

   ```bash
   gh secret set FORGE_OBS_TOKEN
   ```

   Until it is set the job prints a notice and ends green.
2. To have profiles to pull from an env that does not collect them, set
   `signals = forge.ObservabilitySignals {profiles = True}` on that env's
   `forge.Observability` and redeploy. That is your call: it adds a 5s CPU
   profile every 5 minutes per process and a live-heap snapshot every two
   hours to the env's telemetry.
3. Optional, to start now instead of on Monday:

   ```bash
   forge pgo pull --env prod   # writes cmd/<bin>/default.pgo per Go binary with data
   forge pgo status            # provenance, age and config per binary
   git add cmd/*/default.pgo && git commit -m "chore(pgo): first default.pgo from prod"
   ```

To decline the workflow: `ci.workflows: {pgo-refresh: false}` in forge.yaml
(before `forge generate`), or delete the file after. Depth:
`forge skill load observability/profiles`.
