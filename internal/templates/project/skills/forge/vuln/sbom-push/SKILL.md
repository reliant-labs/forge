---
name: vuln/sbom-push
description: `forge vuln sbom push` — re-send a release's recorded SBOMs from this machine's ledger to the env's control plane (RecordReleaseSBOM), for a release cut while the control plane did not take SBOMs yet or whose upload failed. Per-artifact statuses (sent, already_recorded, differs, missing, failed, not_applicable), how each artifact's SBOM is found (--sbom, the control plane's record, the per-release sbom-index, CycloneDX metadata), --sbom overrides for static sites, exit codes and the `forge.dev/vuln-sbom-push/v1` JSON. Use when a control plane shows a release with fewer SBOMs than it ships, or a cut said its SBOMs will be re-sent.
---

# Re-sending a release's SBOMs

A cut uploads its SBOMs itself, and re-sends a release an older control plane
refused on the next cut to it (`vuln`, "Control-plane envs"). This verb does it now,
for one release.

`forge vuln sbom push --release <v>` sends the
release's SBOMs from this machine's ledger to the env's control plane now
(`--env`, default the one env whose ledger is a control plane). The control plane
keeps one SBOM per artifact of a release, so running it again is safe. Per artifact:

| Status | Means |
|---|---|
| `sent` | uploaded now |
| `already_recorded` | the control plane holds this SBOM (it named it, or answered so) |
| `differs` | it holds a different SBOM for the artifact, and keeps it |
| `missing` | no SBOM this ledger can prove describes it (`needs_override` when one must be named) |
| `failed` | the upload failed |
| `not_applicable` | a git source pin or source maps: no SBOM describes them |

Which SBOM describes an artifact: `--sbom <artifact>=<sha256:digest>`; the one the
control plane already names; the index above; and, for a release cut before the
index existed, the SBOM files by their CycloneDX metadata (`metadata.component.name`
is the artifact, `.version` its image digest). A static site's SBOM records its
lockfile's digest, not the bundle's, so metadata cannot prove it: the output lists
the SBOMs that name the site as `candidates`, and `--sbom` names the one to send (its
bytes are verified against the digest, and it must name the artifact). Exit 0 every
applicable artifact's SBOM is on the control plane (`differs` included); 2 one is
missing or failed; 4 refused (no sign-in or scope, or a control plane that does not
take SBOMs). `--json` prints `forge.dev/vuln-sbom-push/v1`
(`forge project schema vuln-sbom-push`).

```bash
forge vuln sbom push --release v1.7.0 --env prod
forge vuln sbom push --release v1.7.0 --sbom registry.example.com/acme/web/static.v1=sha256:… --json
```
