---
name: forge/lint
description: Run linters across Go code, protos, and frontend sources.
when_to_use:
  - Before committing — you want the same checks CI runs
  - CI lint is red and you need to reproduce locally
  - You added a new proto and want to verify proto-method enforcement
---

# forge/lint

`forge lint` runs the full lint matrix: `golangci-lint` for Go (config at `.golangci.yml`), `buf lint` for protos, and the frontend's own linters (ESLint / `next lint`) if frontends exist.

## Core commands

```
forge lint                  # all standard linters (Go + proto + frontend)
forge lint --fix            # auto-fix issues where possible
forge lint --proto          # run the proto method enforcement linter
forge lint --contract       # run the contract interface enforcement linter
forge lint ./handlers/...   # restrict to a path
```

## Workflow

1. Run before committing:
   ```
   forge lint
   ```
2. If issues are reported, fix them. Do **not** disable rules inline (`//nolint:...`) just to land a commit — the `.golangci.yml` config is the agreed contract. If a rule is genuinely wrong for this project, disable it in the config file with a comment explaining why.
3. Re-run to confirm clean.
4. When proto contracts tighten, run the enforcement linters:
   ```
   forge lint --proto
   forge lint --contract
   ```

## Rules

- Never `//nolint` without a reason. If you must suppress a lint, add a same-line comment: `//nolint:errcheck // best-effort cleanup in defer`.
- `.golangci.yml` and `buf.yaml` are the sources of truth. Don't pass extra flags to `golangci-lint` or `buf` that aren't in those files — CI won't have them.
- Formatting is linting. Run `gofmt` / `goimports` on save so `forge lint` doesn't just tell you to format.

## When this skill is not enough

- You need static analysis beyond golangci-lint → check if the rule is already in `.golangci.yml` first. If not, add it there rather than running standalone tools ad hoc.
- You need dependency vulnerability scans → the CI `vuln-scan` job handles this. Don't bolt it onto lint.
