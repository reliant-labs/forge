---
name: migrations/v1.12.0/vuln-gate-default
description: Vulnerability scanning gates every release by default (forge v1.12.0). `vuln_gate` and `provenance_sbom` default to `enforce`, so a self-hosted promotion of a release with a KEV-listed or fixable critical advisory, or one nobody scanned, is refused from v1.13.0; at a v1.12.x pin it only warns `would block`. `vuln_gate` is `audit` when `provenance_sbom` is `off` and it declares no mode. A release cut now downloads the grype database when the machine has none (--no-vuln-db-pull or FORGE_VULN_DB_PULL=never opts out). Nothing is changed in your files. Before v1.13.0, read `forge vuln list <env>`, fix with `forge vuln patch`, accept with `forge vuln exempt`, or set `mode = "audit"` per env to opt out; cache or import the database on CI runners. Use when moving the pin to v1.12.0 or later, when a deploy prints `would block (new in v1.12.0; gates from v1.13.0)`, or when a promotion is refused `vuln_gate_blocked`, `vuln_gate_unscanned` or `sbom_missing` after the pin reaches v1.13.0.
version: v1.12.0
detection: ls deploy/kcl/*/main.k > /dev/null 2>&1 && ! grep -qs -E 'vuln_gate *= *forge\.VulnPolicy *\{ *mode *= *"(audit|off)"' deploy/kcl/*/main.k
---

# Vulnerability scanning gates by default (v1.12.0)

From v1.10.0 forge records an SBOM per artifact at every cut and scans it, but a
finding could only stop a deploy if an env opted in. From v1.12.0 it can stop one
unless an env opts out: `vuln_gate` and `provenance_sbom` default to `enforce`,
the way lint and tests gate a change. A self-hosted promotion of a release
carrying a KEV-listed advisory, or a critical one that has a fix, is refused
`vuln_gate_blocked` (exit 4); a release nobody scanned, or scanned with a database
older than 5 days, is refused `vuln_gate_unscanned`; an artifact with no SBOM is
refused `sbom_missing`. High findings warn and do not block, and the rule that
decides is unchanged (`vuln`).

Nothing in your files changes, and moving the pin to v1.12.0 refuses nothing.

## What happens when the pin moves

Both controls are graded through the lint rollout rows `vuln-gate` and
`sbom-required`, new in v1.12.0 and gating from **v1.13.0** (`ci/lint-rollout`).
While `forge_version` is below v1.13.0 an `enforce`, declared or default, is
judged as `audit` at the cut, at a self-hosted promote and in the `vuln-sbom` gate
step, and prints:

```text
would block (new in v1.12.0; gates from v1.13.0; gate now with lint.rules: {vuln-gate: error})
```

That includes an env that declared `vuln_gate = forge.VulnPolicy {mode =
"enforce"}` under an older pin: its window reopens, because the rule was tightened
and a tightened rule moves to the tightening release. A pin that jumps from before
v1.12.0 straight to v1.13.0 warns at that pin and gates from the next one.

Two things differ from the opt-in era:

- **`vuln_gate` follows `provenance_sbom`.** With no mode declared, `vuln_gate` is
  `enforce`, except under `provenance_sbom = forge.Control {mode = "off"}`, where
  there is no SBOM to scan and it is `audit`. A declared mode always wins, and a
  declared `enforce` over `provenance_sbom = off` is still refused at render.
- **The cut fetches the database.** When this machine has no vulnerability
  database the cut downloads it (about 205 MB, 3.3 GiB unpacked, once per machine,
  shared by every project, in `$FORGE_VULN_DB_DIR`), and refreshes it when it is
  more than a day old. Before, it recorded `not scanned`, which an enforced gate
  refuses. A download that fails (offline, mirror down) is `not scanned` too, never
  a failed cut. A control-plane env is unchanged: the control plane judges it,
  graded by the forge that cut the release.

## Before v1.13.0

1. **See what would block.** The first cut fetches the database; `forge vuln db
   pull` does it ahead of time. Then:

   ```bash
   forge vuln list prod                 # exit 1: something would block; 2: could not determine
   forge vuln scan --release v1.7.0     # every SBOM that release recorded, scanned now
   ```

2. **Fix or accept each finding.** `forge vuln patch prod --verify` bumps what the
   findings name to their fix and runs your gate (`vuln/patch`). An advisory you
   accept goes in the env's own file, with a reason and an expiry:

   ```bash
   forge vuln exempt prod CVE-2026-1234 --reason "unreachable: we never call the parser" --expires 2026-12-31
   ```

   Review and commit it. For one promotion, `--override-vuln-gate "<why>"` records
   who, why and which findings.

3. **Or opt out, per env, for good.** In that env's `delivery_policy`:

   ```kcl
   delivery_policy = forge.DeliveryPolicy {
       vuln_gate       = forge.VulnPolicy {mode = "audit"}    # or "off"
       provenance_sbom = forge.Control {mode = "audit"}
   }
   ```

   Findings are then recorded and printed as "would block", and nothing is refused.
   An env that sets only `vuln_gate` to `audit` still refuses a release with no SBOM
   (`sbom_missing`) until `provenance_sbom` is `audit` too.

## CI runners and other machines

A fresh runner has no database, so the cut downloads it on every run. Two ways to
stop paying for that:

- **Cache it.** Set `FORGE_VULN_DB_DIR` and cache the directory under a daily key;
  the scaffolded `build-images.yml` does (`vuln/database`).
- **Never download, and import it.** `FORGE_VULN_DB_PULL=never` (also `off`,
  `false`, `0`, `no`) stops every cut on the machine from downloading, and
  `--no-vuln-db-pull` does it for one `forge env build --release` or one
  `forge env deploy` that cuts. Load the database yourself with `forge vuln db
  import <archive>` (or point `FORGE_VULN_DB_URL` at a mirror). With neither a
  database nor a download, the cut records `not scanned`, and an enforced gate
  refuses that at promote as `vuln_gate_unscanned`.

## The lint dials

`lint.rules` in `forge.yaml` moves one rule's severity (`ci/lint-rollout`):

| Setting | Effect |
|---|---|
| `{vuln-gate: error}`, `{sbom-required: error}` | gate now, before v1.13.0 |
| `{vuln-gate: warn}`, `{sbom-required: warn}` | keep warning past v1.13.0 |

`warn` is a way to extend the window, not to opt out: the opt-out is `mode =
"audit"` above.

## Not affected

- A release cut by a forge older than v1.10.0 has no SBOM and is never refused for
  lacking one. The release's era is the forge that cut it, not this rule's
  introduction.
- `forge vuln list` and `show` still never download the database, and `forge ci
  vuln-sbom` does not unless `--pull`.
