---
name: audit-json
description: Reading `forge audit --json` and `forge map --json` — the JSON shapes, common jq queries, CI integration patterns, and the additive-extension contract that keeps consumers stable as new finding types appear.
---

# Audit + Map JSON Output

`forge audit --json` and `forge map --json` are the machine-readable
counterparts to the human-formatted `forge audit` and `forge map`.
Both ship live today and are the right entrypoint for CI gates,
dashboards, sub-agent introspection, and scripted audits. The output
is stable enough to grep/jq against; new finding types are added
additively (existing keys never change shape).

## Purpose

- **`forge audit --json`** — per-category roll-up of project health:
  forge version pin, project shape, lint status, codegen drift, pack
  health, proto vs migration alignment, scaffold markers, deps. One
  JSON object per run.
- **`forge map --json`** — annotated project tree: every file labelled
  user-owned, forge-space (regenerated), scaffold-with-markers, or
  drifted (forge-space file with hand-edits). One nested-tree JSON
  object per run.

Reach for these when:

- A CI workflow needs to fail a build on `overall_status == "error"`.
- A dashboard wants to count drift across N projects.
- A sub-agent needs to know which `_gen.go` files were hand-edited
  before deciding whether to regenerate.
- An LLM needs the project shape (kind, services, frontends, packs)
  before scaffolding new code.

The human output (`forge audit`, `forge map`) has the same data with
ANSI colours and tree-drawing characters. Use that for terminal eyes,
JSON for everything else.

## `forge audit --json` shape

```jsonc
{
  "project_name": "myproject",
  "project_kind": "service",
  "binary_version": "0.7.2",
  "generated_at": "2026-05-07T15:06:12.407Z",
  "categories": {
    "version": { "status": "ok",   "summary": "...", "details": { ... } },
    "shape":   { "status": "ok",   "summary": "...", "details": { ... } },
    "conventions": { "status": "warn", "summary": "...", "details": { ... } },
    "codegen": { "status": "warn", "summary": "...", "details": { ... } },
    "packs":   { "status": "ok",   "summary": "...", "details": { ... } },
    "proto_migration_alignment": { "status": "ok", "summary": "..." },
    "scaffold_markers":  { "status": "ok", "summary": "...", "details": { ... } },
    "deps":    { "status": "ok",   "summary": "...", "details": { ... } }
  },
  "overall_status": "warn"
}
```

Top-level keys are stable:

| Key | Type | Meaning |
|-----|------|---------|
| `project_name` | string | `name:` from forge.yaml, or directory basename if no forge.yaml |
| `project_kind` | string | `service`, `cli`, `library`, or `unknown` |
| `binary_version` | string | The forge binary that produced the audit (`dev` for local builds) |
| `generated_at` | RFC3339 timestamp (UTC) | When the audit ran |
| `categories` | object | Per-category roll-ups, keyed by category name |
| `overall_status` | string | `ok` / `warn` / `error` — worst category status wins |

Each category is the same shape:

```jsonc
{
  "status":  "ok" | "warn" | "error",
  "summary": "human-readable one-liner",
  "details": { ... category-specific keys ... }   // optional
}
```

### Category keys

| Category | What's in `details` (when relevant) |
|----------|-------------------------------------|
| `version` | `pinned_version`, `binary_version`, `hint` (when mismatch) |
| `shape` | `services[]`, `workers[]`, `operators[]`, `frontends[]`, `packs[]`, `packages[]` |
| `conventions` | `counts{}` (per-rule violation counts), `hint` |
| `codegen` | `tracked_files`, `forge_version`, `last_generate`, `user_edited_gen_files[]`, `orphan_gen_files[]` |
| `packs` | per-pack `{name, installed_version, latest_version, status}` |
| `proto_migration_alignment` | `divergence[]` (entities whose proto definition disagrees with migrations) |
| `scaffold_markers` | `total_markers`, `files[]` (paths still carrying `FORGE_SCAFFOLD:` lines) |
| `deps` | `go_mod`, `go_sum` presence flags |

The full set of keys per category is stable; new categories may be
added (additive). Consumers should `select` the keys they care about
and tolerate unknown extras.

### Status semantics

| Status | When |
|--------|------|
| `ok` | Category looks healthy. |
| `warn` | Soft drift — fixable without blocking work. Codegen orphans, version-mismatch with newer-binary, deprecated-pack pinning. |
| `error` | Hard problem — build will fail or behaviour is broken. Missing forge.yaml, error-severity convention violations, missing required packs. |

`overall_status` is the worst category status. CI gates that block on
`error` (and only `error`) hit the right balance for most projects.

## `forge map --json` shape

```jsonc
{
  "path": ".",
  "name": "myproject/",
  "is_dir": true,
  "children": [
    {
      "path": ".github/workflows",
      "name": "workflows/",
      "is_dir": true,
      "children": [
        {
          "path": ".github/workflows/ci.yml",
          "name": "ci.yml",
          "is_dir": false,
          "ownership": "forge-space, regenerated"
        },
        {
          "path": ".github/workflows/release.yml",
          "name": "release.yml",
          "is_dir": false,
          "ownership": "user-owned"
        }
      ]
    },
    {
      "path": "handlers/users/handlers_crud_gen.go",
      "name": "handlers_crud_gen.go",
      "is_dir": false,
      "ownership": "forge-space, hand-edited (drift from regen)",
      "flags": ["drift"]
    }
  ]
}
```

Each `MapNode` carries:

| Key | Type | Meaning |
|-----|------|---------|
| `path` | string | Path relative to the project root (forward slashes) |
| `name` | string | Display name (trailing `/` for directories) |
| `is_dir` | bool | Directory vs file |
| `ownership` | string (optional) | One of the ownership classes below |
| `flags` | string[] (optional) | Health flags — `drift`, `FORGE_SCAFFOLD`, `diverged-from-migrations` |
| `children` | []MapNode (optional) | Subdirectory contents |

### Ownership classes

| Value | Meaning |
|-------|---------|
| `user-owned` | Hand-written code; forge never touches it. |
| `user-owned, scaffolded once` | Tier-2 file; forge wrote it on `forge new` and won't touch it again. |
| `forge-space, regenerated` | Tier-1 codegen; rewritten every `forge generate`. |
| `forge-space, hand-edited (drift from regen)` | Tier-1 file whose checksum no longer matches the generator output. Flagged with `drift`. |
| `scaffold, FORGE_SCAFFOLD markers present` | Tier-2 scaffold with at least one `FORGE_SCAFFOLD:` line still present — not yet customised. |

`flags` adds machine-greppable health hints orthogonal to ownership:

- `drift` — Tier-1 file with hand-edits.
- `FORGE_SCAFFOLD` — file still carries placeholder markers.
- `diverged-from-migrations` — proto entity whose shape disagrees with
  the migrations that own the schema.

## Common queries

```bash
# Fail CI on any error-severity category.
forge audit --json | jq -e '.overall_status == "error" | not' \
  || (echo "forge audit found errors"; exit 1)

# List every hand-edited generated file.
forge audit --json | jq -r '.categories.codegen.details.user_edited_gen_files[]?'

# Count Tier-1 files (forge-regenerated).
forge map --json | jq '[.. | select(.ownership? == "forge-space, regenerated")] | length'

# List every drifted file with its path.
forge map --json | jq -r '.. | select(.flags? // [] | index("drift")) | .path'

# List every scaffold still carrying FORGE_SCAFFOLD markers.
forge map --json | jq -r '.. | select(.flags? // [] | index("FORGE_SCAFFOLD")) | .path'

# Project shape: how many services, workers, frontends?
forge audit --json | jq '.categories.shape.details |
  {services: (.services | length // 0),
   workers:  (.workers  | length // 0),
   frontends:(.frontends | length // 0)}'

# Pack health: any pack pinned older than what the binary ships?
forge audit --json | jq -r '.categories.packs.details[]?
  | select(.installed_version != .latest_version)
  | "\(.name): \(.installed_version) -> \(.latest_version)"'

# Convention violations by severity.
forge audit --json | jq '.categories.conventions.details.counts'

# Orphan _gen files (sources removed, file forgotten).
forge audit --json | jq -r '.categories.codegen.details.orphan_gen_files[]?'
```

## CI integration

A drop-in workflow that runs both commands, uploads the JSON as
artifacts, and posts a summary comment on PRs:

```yaml
# .github/workflows/forge-audit.yml (Tier-2 — user-owned)
# forge:scaffold one-shot — user-owned workflow (not a Tier-1 codegen target).
name: Forge Audit
on:
  pull_request:
  push:
    branches: [main]

jobs:
  audit:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - name: Install forge
        run: go install github.com/reliant-labs/forge/cmd/forge@main

      - name: Forge audit
        id: audit
        run: |
          forge audit --json > audit.json
          forge map --json   > map.json
          echo "status=$(jq -r .overall_status audit.json)" >> "$GITHUB_OUTPUT"
          echo "drift_count=$(jq '[.. | select(.flags? // [] | index("drift"))] | length' map.json)" >> "$GITHUB_OUTPUT"

      - name: Upload artifacts
        uses: actions/upload-artifact@v4
        with:
          name: forge-audit
          path: |
            audit.json
            map.json

      - name: Fail on error
        if: steps.audit.outputs.status == 'error'
        run: |
          echo "::error::forge audit reported overall_status=error"
          jq '.categories | to_entries[] | select(.value.status == "error")' audit.json
          exit 1

      - name: Comment summary
        if: github.event_name == 'pull_request'
        uses: actions/github-script@v7
        with:
          script: |
            const audit = require('./audit.json');
            const drift = '${{ steps.audit.outputs.drift_count }}';
            const lines = ['### Forge audit',
              `**Overall:** \`${audit.overall_status}\` (binary ${audit.binary_version})`,
              `**Drifted files:** ${drift}`,
              '',
              '| Category | Status | Summary |',
              '|----------|--------|---------|',
              ...Object.entries(audit.categories).map(
                ([k, v]) => `| ${k} | \`${v.status}\` | ${v.summary} |`)];
            github.rest.issues.createComment({
              issue_number: context.issue.number,
              owner: context.repo.owner,
              repo:  context.repo.repo,
              body:  lines.join('\n'),
            });
```

Drop the file at `.github/workflows/forge-audit.yml`. `forge generate`
won't touch it (the file name isn't on forge's Tier-1 list).

## Sub-agent patterns

LLM sub-agents calling forge from a parent harness benefit from JSON
because parsing prose is fragile:

```bash
# Before scaffolding a new service: orient.
shape=$(forge audit --json | jq '.categories.shape.details')
kind=$(forge audit --json | jq -r .project_kind)

# Before regenerating: check for drift.
drift=$(forge audit --json | jq -r '.categories.codegen.details.user_edited_gen_files[]?')
if [ -n "$drift" ]; then
  echo "User has hand-edited generated files; regenerate would clobber:"
  echo "$drift"
  # Prompt the user / commit changes / etc.
fi

# Before deleting a directory: check ownership.
ownership=$(forge map --json | jq -r --arg p "internal/things" \
  '.. | select(.path? == $p) | .ownership')
```

## Extending

The JSON shape is **additive**. The contract:

- Existing keys in `categories`, top-level fields, and per-`MapNode`
  fields keep their meaning across forge releases.
- New categories may appear; consumers should iterate
  `.categories | keys[]` rather than assume a closed set.
- New `details` keys per category may appear; consumers should `?`
  every nested lookup so a missing key is `null` not an error.
- New `flags` values may appear; treat the array as a free-form set,
  match on the values you know.
- New ownership-class strings may appear (rare); fall back to a
  default treatment for unknown values.

The status enum (`ok` / `warn` / `error`) and the
`status / summary / details` per-category shape are **frozen** —
those are the load-bearing pieces of the contract.

If you need a new finding type that doesn't fit an existing category,
file a forge issue (or contribute a PR adding the new category).
Don't shoehorn into an unrelated category — consumers will end up
with mismatched expectations.

## Rules

- Use `forge audit --json` for category roll-ups; use `forge map
  --json` for per-file ownership.
- Gate CI on `overall_status == "error"`. `warn` is informational.
- Tolerate unknown keys: forge adds finding types additively, never
  renames existing ones.
- Pipe through `jq` for filtering. Don't grep the prose output —
  that's not a stable interface.
- The status enum (`ok` / `warn` / `error`) is part of the contract.
  Code should match exactly.
- Both commands run from the project root; `forge audit` resolves
  forge.yaml, `forge map` walks the file tree.

## When this skill is not enough

- **What `forge generate` is doing under the hood** — see
  `architecture` and the per-version `migration/v0.x-to-*` skills.
- **CI workflow generation** (where the audit workflow plugs in) —
  see `ci`.
- **Tier-1 vs Tier-2 banner classification** — see `architecture`
  ("Three precise classes") and `pack-development`.
- **Drift remediation** (regenerate vs accept hand-edits) — see
  `migration/upgrade`.
