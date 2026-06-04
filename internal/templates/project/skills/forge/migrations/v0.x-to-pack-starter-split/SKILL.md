---
name: v0.x-to-pack-starter-split
description: Migrate stripe / twilio / clerk-webhook from packs to starters. forge versions before 1.6 shipped these as installable packs; 1.6+ ships them as one-time-copy starters that the user owns.
---

# Migrating business-integration packs to starters

Use this skill when `forge upgrade` reports a jump across the version
that demoted three "packs" to "starters" (typically `1.5.x → 1.6.x`).
It only affects projects whose `forge.yaml` lists `stripe`, `twilio`,
or the webhook half of `clerk` under `packs:`. If your project never
installed any of those, this skill is a no-op.

## 1. What changed

Forge versions before 1.6 shipped **stripe**, **twilio**, and the
**clerk webhook user-sync** half of the clerk pack as installable
packs. Each `forge generate` re-emitted the pack's templated code
into `pkg/clients/<name>/`, etc. — and every project we touched then
hand-edited that code, fighting the regen on every run.

Forge 1.6+ demotes those three to **starters**: `forge starter add
<name>` copies files once and exits. The user owns the code from the
first byte forward; subsequent `forge generate` runs do not touch it.

The split:

| Concern | Status |
|---------|--------|
| `jwt-auth`, `firebase-auth`, `api-key`, `audit-log`, `nats`, `auth-ui`, `data-table` | **stay packs** — pure infrastructure |
| `clerk` JWKS validator + Connect interceptor | **stays a pack** — auth-side is infrastructure |
| `stripe` | **starter** — `forge starter add stripe --service <svc>` |
| `twilio` | **starter** — `forge starter add twilio --service <svc>` |
| `clerk` webhook user-sync (`pkg/clerk/webhook.go`) | **starter** — `forge starter add clerk-webhook --service <svc>` |

Why the split: per-project divergence on those three was 100%. Every
control-plane, every dogfood, every migration we touched rewrote the
pack-emitted business logic. Centrally maintaining business logic
creates more bugs than it prevents (one session bit us with the
stripe proto package, a twilio template-escape, and a clerk svix
import).

What the starter ships vs what the old pack shipped, per integration:

- **stripe** — old pack also emitted `proto/billing/v1/billing.proto`
  and forced a particular Stripe→entity shape. The starter ships only
  `pkg/clients/stripe/{client.go,webhook.go}`; real projects always
  use their own `proto/<billing-domain>/v1/...` shape, so forge no
  longer prescribes one.
- **twilio** — old pack inlined provider-specific request/response
  envelopes that almost everyone tweaked. Starter ships
  `pkg/clients/twilio/{client.go,webhook.go}` only.
- **clerk-webhook** — old `clerk` pack emitted `pkg/clerk/webhook.go`
  alongside the JWKS validator. Starter splits the webhook off into
  its own `forge starter add clerk-webhook`; the JWKS validator stays
  in the `clerk` pack (infrastructure, benefits from forge upkeep).

## 2. Detection

```bash
# Old shape: stripe / twilio / clerk-webhook listed under packs:.
grep -E "^- (stripe|twilio|clerk-webhook)$" forge.yaml

# Or: forge audit's pack-vs-starter section flags the same thing.
forge audit --json | jq '.categories.packs'
```

A new forge build whose registry no longer ships those packs prints a
generate-time warning:

```
Warning: installed pack "stripe" not found in registry; skipping.
```

That is the "you're on a forge that demoted this to a starter" signal.
The user's existing pack-emitted code under `pkg/clients/stripe/` etc.
stays in place — forge no longer claims ownership of it.

## 3. Migration (deterministic part)

For each affected pack:

```bash
# 1. Confirm the pack-emitted code is still in your tree.
ls pkg/clients/stripe/        # or pkg/clients/twilio/, pkg/clerk/, etc.

# 2. Remove the entry from forge.yaml's packs: list.
#    (Hand-edit; the line looks like "- stripe".)

# 3. (Optional) Refresh from the current starter template if you want
#    the latest scaffold rather than your hand-edits. This OVERWRITES
#    files — only do it if you intend to throw your customizations
#    away or reconcile by hand afterward.
forge starter add stripe --service <svc-that-uses-stripe>

# 4. Re-run generate. The cleanup pass strips any stale "stripe pack
#    registered" entries from generated wiring (bootstrap, server.go).
forge generate
```

`forge upgrade` runs steps 2 and 4 for you when it has high
confidence in the diff. Step 3 is always opt-in — re-pulling the
starter is destructive to your customizations, so forge never does
it automatically.

## 4. Migration (manual part)

What user code might need to change:

- **Custom edits to `pkg/clients/stripe/client.go` (or twilio /
  clerk-webhook).** No change. The pack used to overwrite these on
  every `forge generate`; now the file is yours and stays yours.
  This is the *whole point* of the migration — your customizations
  stop creating diff churn.
- **Direct references to the pack's symbol exports.** No change. The
  starter copies the same file shape forward, so import paths and
  exported names are identical to what the pack used to emit.
- **Pack-driven proto** (`proto/billing/v1/billing.proto` from the
  old stripe pack only). The pack no longer emits this; if you have
  it on disk, it's now yours. Either keep it as project-owned proto
  (the common case — every real project shapes its own billing
  domain) or delete it if you've moved billing types into your own
  proto package.
- **`forge.yaml` `pack_overrides:`** entries scoped to a removed pack
  are silently ignored. Drop them when convenient.

## 5. Verification

```bash
forge audit                      # pack-vs-starter section: no warnings
forge pack list                  # stripe / twilio absent; clerk still listed
forge starter list               # stripe / twilio / clerk-webhook present
go build ./...                   # clean — your pkg/clients/<name>/ code is yours now
forge generate                   # idempotent; no diffs in pkg/clients/
```

## 6. Rollback

If the new shape breaks something:

```bash
git checkout HEAD -- forge.yaml  # restore the packs: entries
forge upgrade --to <prior-version>
```

`--to <prior-version>` requires the older forge build on PATH. The
old pack templates ship with the older binary, so re-adding the entry
to `packs:` and running `forge generate` restores pack-emitted files —
**but** any customizations you made will be overwritten on the next
generate, which is exactly the failure mode the migration was meant
to fix. Long-term rollback is not supported once a release after 1.6
removes the pack templates from the embedded registry entirely.

## See also

- `packs` skill — what's still a pack (infrastructure only).
- `starters` skill — when to use `forge starter add` for a new
  greenfield install.
- `MIGRATION_TIPS.md` "Pack → starter split for business
  integrations" for the design rationale.
