---
name: vuln/patch
description: `forge vuln patch` — bump vulnerable Go and npm/pnpm/yarn dependencies to the lowest version that fixes them (inside the major unless `--allow-major`) with the ecosystem's own tool, from an env's findings or a scanned directory; what it skips and why (OS packages, the Go standard library, Python, ambiguous manifests), `--verify` and `vuln.patch.verify`, `--pr`, the `forge.dev/vuln-patch/v1` JSON and its exit codes. Use when a finding has a fix and you want the bump made, or to automate patching in CI or a workflow.
---

# Fixing findings: `forge vuln patch`

`forge vuln patch` bumps each vulnerable dependency to the **lowest version that
fixes it, inside its major version**, with the ecosystem's own tool so lockfiles stay
consistent:

| Ecosystem | Direct dependency | Transitive |
|---|---|---|
| Go | `go get <module>@<fix> && go mod tidy`, in every module that requires it (`go.work` aware) | the same |
| npm | `npm install <pkg>@<fix>` | an `overrides` entry |
| pnpm | `pnpm update <pkg>@<fix>` | `pnpm.overrides` |
| yarn | `yarn up <pkg>@<fix>` (`yarn upgrade` on yarn 1) | `resolutions` |

```bash
forge vuln patch prod                        # the findings of the release prod runs (this machine's ledger)
forge vuln patch . CVE-2026-1234             # scan a directory first; patch only this advisory (a CVE finds its GHSA)
forge vuln patch ./web --ecosystem npm --allow-major
forge vuln patch prod --verify --pr          # verify, then commit on forge/vuln-patch/<date>-<commit>, push, open a PR with gh
forge vuln patch prod --json                 # forge.dev/vuln-patch/v1 (forge project schema vuln-patch)
```

With no advisory ids it patches every finding that has a fix and no exemption
(it says how many exempt or unfixable ones it left out); a named id is patched
whatever its exemption. Nothing is guessed: a finding it cannot tie to one manifest,
a fix only in a new major (without `--allow-major`), an OS package, the Go standard
library or a Python package is reported **skipped** with the reason. Python
(pip, pip-compile, poetry, uv; forge#2510) and base-image refreshes for OS packages
plus a Go toolchain raise for stdlib findings (forge#2511) are follow-ups not on main
yet.

`--verify` runs the project's gate after the bump, then rescans each changed
manifest's directory: a finding still reported is `still_present`. The gate is
`vuln.patch.verify` in forge.yaml when set (run with `sh -c` in the project),
else `task test:short` when the Taskfile has it, else `go build ./... && go test
-short ./...` for a Go module, else the package manager's `test` script;
`--verify=<cmd>` overrides all of them.

```yaml
vuln:
  patch:
    verify: task test:short && task lint
```

`--pr` refuses a tree with uncommitted changes; `--no-pr` (the default) leaves the
tree changed. `--branch` names the PR branch.

| Exit | Means |
|---|---|
| 0 | every requested finding bumped (and verified, with `--verify`) |
| 1 | a bump or the verification failed: hand the tree to a person or an agent |
| 2 | nothing deterministic to do for at least one requested finding (skipped), or the findings could not be read (including an env whose ledger is a control plane) |
| 4 | refused, nothing changed: a dirty tree with `--pr`, or a named advisory whose fix needs `--allow-major` |

## Next

- What the findings are and where they come from: `vuln`.
- Accepting an advisory instead of patching it: `forge vuln exempt` (`vuln`).
