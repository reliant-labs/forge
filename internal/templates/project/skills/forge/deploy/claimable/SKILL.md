---
name: deploy/claimable
description: Publish a Reliant Forge app without an account, using an expiring claimable deployment. Covers anonymous scaffold defaults, project identity, limits, preflight, publication status, claim links and refusal recovery.
---

# Claimable deployments

An agent with no Reliant account (standalone forge, nothing signed in) can still
publish:

```bash
forge env deploy prod --claimable
```

forge gets an anonymous identity from the control plane, deploys as it, and
exits **0** with the live URL and a `claim` block: the claim link, when the
deploy goes offline (72h; then deleted 7 days later) and an instruction for you.
**Give the claim link to your user and tell them it expires.** Opening it signs
them up for free and makes the deploy theirs, with nothing to copy. Treat the
link like a password (whoever claims first owns the deploy) and never post it
publicly. `forge env status <env> --claimable` prints it again; `--json` carries
it as `claim` (`state`, `published`, `last_attempt`, `url`, `claim_url`,
`expires_at`, `purge_after`, `agent_instructions`).

The claim and its 72h clock start with the **first attempt**, not the first
success, and a redeploy does not extend them. So `state: "live"` means only that
the unclaimed window is open. **`published: false` means nothing is deployed**:
every attempt failed (`last_attempt.error` says why). Do not give the claim
link to your user then. Claiming signs them up for an empty deploy and revokes
the token you deploy with. Fix the failure and deploy again.

| Rule | Detail |
|---|---|
| Adopts a bare app | In a directory with no `forge.yaml` that holds a Vite or Next.js static export at its root (or an app with its own Dockerfile), `--claimable` first runs `forge project new --in-place` for it (`--kind site`, or `--kind app --claimable`), prints one line saying it added `forge.yaml` and `deploy/kcl/` and left the app's own files untouched, then deploys. A Go module with no Dockerfile, or a directory with no app, is refused with the command to run instead. Only `--claimable` adopts: a plain `forge env deploy` needs an existing project. `npx -y @reliantlabs/forge env deploy prod --claimable` is the whole procedure for a fresh `npm create vite` app |
| Only with the flag | There is no auto-detection. With no credential, a plain `forge env deploy` fails and its error names `--claimable` |
| Signed in? Refused (exit 1) | A user with a login or `$FORGE_CREDENTIAL_HELPER` deploys to their own org: drop the flag |
| Fits | one static site, or one service (1 replica, at most 125m / 128 MiB, request and limit, no burst) with one managed database (256 MiB of data on a 1 GiB volume) and that service's migration job (a gating `before = [fw.BEFORE_ALL]` job, or a standalone pre-rollout one). No workers, cron, second service or database, custom domains or remote builds: exit 4, naming each with its KCL fix |
| **No outbound internet until claimed** | An unclaimed **service** (not a static site) serves requests and uses its own database, but every call out of it fails: a payment, an LLM, a webhook, a package download, and any DNS lookup of a name outside the cluster (the connection errors, or the name does not resolve: the backend's resolver answers only the cluster's own names, such as its database's). That is the platform, not a bug in the app, so do not debug it or work around it; it ends when the deploy is claimed, when the platform restarts the backend on the normal hosted network (TCP 80/443 to the public internet). Plan for it: an app that needs an external API at startup will not come up unclaimed. Do not assume Reliant's gateway is available unclaimed: `deploy/llm` documents that credentials may stay blank until its in-cluster route is enabled. Disable the LLM feature when credentials are absent, or claim first; never weaken networking to work around this. The claim block, `--json` (`claim.no_outbound_internet`, and `claim.agent_instructions`) and `--plan-only` all say so |
| Not the shape band | The billed plan's 4 GiB-per-vCPU band does not apply to an anonymous deploy (it is never billed); the caps above do. `forge env shape <env> --claimable` judges an env that way |
| Preview first | `forge env deploy <env> --claimable --plan-only` checks everything locally (the caps, the hosted plan, the resources, and that every migration is committed on the default branch — the release cut refuses one that is not) and asks the platform whether it takes anonymous deploys right now. No identity, nothing created; exit 0 when the env fits |
| Scaffold for it | `forge project new <name> --claimable ...` writes a prod that fits: `control_plane = forge.ControlPlane {claimable = True}` (every check then judges it under this plan, and a signed-in deploy is refused until you remove the line) and `_claimable_resources` on its hosted services and jobs |
| Unavailable: exit 4 `claimable_unavailable` | The platform cannot take it: `disabled by operator`, an `unavailable dependency (<component>)`, `not supported yet` (a cause the platform reserves for a capability it lacks), or `not deployed` (no ClaimableService). Checked before any build; `--json` carries `unavailable_cause` and `unavailable_component`. Not your project: retry later, or deploy to your own account |
| Never queued | An unclaimed deploy is refused (exit 4), never held for billing; **exit 7 does not occur** |
| Throttled: exit 4 `claimable_throttled` | Our rate limits or the platform's anonymous sign-up limit. The block prints a scope (`network`, `device`, `platform`), a retry-after and the `forge login` alternative; `--json` carries `retry_after_seconds`, `retry_at`, `throttle_scope`. **forge never retries on its own, and neither should you in a loop**: wait, or `forge login` |
| Claimed: exit 4 `claimable_claimed` | A human claimed it. Deploy as them: `forge login`, then `forge env deploy <env>` without the flag |
| Redeploys | Re-running reuses this machine's deploy and its claim link and does not extend the expiry |

The anonymous token is stored beside your logins under its own credentials-file
client id (`forge-cli-claimable/<project>/<env>/instance/<project_instance_id>`
for new projects; the legacy name/env key for existing ones), so it can never pass for a
`forge login` and `forge logout` leaves it alone. It carries only
`deploy:read` and `deploy:write`.

New scaffolds record `project_instance_id` in `forge.yaml`. Commit it: clones
and worktrees on the same machine resume the same deployment, while separately scaffolded projects
with the same name get different deployments. Keep it unchanged on redeploy.
Existing projects without the field retain their current name-based binding.
An older control plane that has not enabled instance identities is refused
before creating an account; upgrade the platform rather than removing the ID.

Anonymous frontends default to `--auth-mode none`. Credential-collection pages
(including unused scaffold login routes in the export) cannot be published
unclaimed. An app that needs sign-in must be claimed before publishing those
pages. Do not remove an app's intended authentication to bypass this rule.
For a public tool, start without authentication; changing an existing project
also requires removing its unused owned routes and identity configuration,
not just making the homepage public. A refusal's evidence ID lets the platform
operator locate the decision without exposing detection signatures.
