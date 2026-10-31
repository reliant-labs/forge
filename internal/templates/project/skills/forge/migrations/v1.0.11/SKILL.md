---
name: v1.0.11
description: Upgrade a project to forge v1.0.11. More generated files merge with plain git. The read-only and dashboard grants (db/grants/*_gen.sql), the config module (deploy/kcl/config_gen.k) and the scenario handler map (src/mocks/scenario-rpcs_gen.ts) certify each table, config block and service separately, and the scaffold ledger .forge/scaffolded.json becomes one entry file per path under .forge/scaffolded/. Use when the project commits any of those files.
version: v1.0.11
detection: test -f .forge/scaffolded.json -o -f db/grants/readonly_gen.sql -o -f deploy/kcl/config_gen.k && exit 0; ls frontends/*/src/mocks/scenario-rpcs_gen.ts >/dev/null 2>&1 && exit 0; exit 1
---

# Upgrading to v1.0.11

The first `forge generate` on v1.0.11 migrates everything below. Commit what it writes in one change.

## 1. Grants, config module and scenario handler map certify each part

Each of these files gathers independent parts into one file, and each carried ONE `forge:hash` over all of it. Two pull requests that changed different parts (two tables' columns, two config blocks, two services' rpcs) both rewrote that one line, and git, including GitHub's server-side merge, which runs no merge driver, reported a conflict. Over the week to 2026-10-30, `readonly_gen.sql` and `config_gen.k` conflicted in 34 pull requests across reliant and control-plane, every time on that line alone.

Each part now begins with its own `forge:section forge:hash=…` comment, which certifies that part. The header hash covers only the shared preamble. A file still counts as forge's untouched render only when every hash verifies, so hand-edit detection is unchanged. Two pull requests that change different parts now merge into exactly what `forge generate` writes for both.

- **`db/grants/readonly_gen.sql`, `db/grants/dashboard_gen.sql`:** one section per table. The table's columns list starts with a constant first row, so every table's row starts with its own comma, and adding the last table never rewrites the row before it. The withheld-column notes (`forge:secret`, and credential-shaped columns nobody decided) moved from the end of the file into each table's section. The SQL the migrator applies is unchanged in effect.
- **`deploy/kcl/config_gen.k`:** one section per config block schema, per root schema, per env-projection lambda and per process selector. Inside each lambda, one section per config block's env entries.
- **`src/mocks/scenario-rpcs_gen.ts`:** `ScenarioRpcHandlers` is an intersection of one object type per service, each its own section. Every request and response type is imported inline (`import("@/gen/…").Name`), so the shared `import type` block is gone, and so are the `<module>_<Name>` aliases for message names two services share. Scenario files that use `ScenarioRpcHandlers` need no change.

### Still a conflict

Two pull requests that change the SAME table, config block or service still conflict there, because both changed that part. So do two that each add a new one between the same two neighbours. `forge generate` on the merged inputs resolves either.

## 2. `.forge/scaffolded.json` becomes `.forge/scaffolded/`

**Use when** the project commits `.forge/scaffolded.json`.

**What changed.** The scaffold-once birth ledger, the record that keeps a deleted scaffold deleted, was one sorted file. Every branch that scaffolds anything records births in it, so two branches' births were neighbouring insertions there, and git conflicted. Each path's record is now its own file, at the path under `.forge/scaffolded/` with `.json` appended (`.forge/scaffolded/internal/app/compose.go.json`). It holds `{}` for an ordinary birth, or the decline reason, birth hash or reported-absent mark the path carries. The birth timestamp is gone: nothing read it, and it made two branches scaffolding the same path disagree. Two branches now conflict there only over the same path.

**What generate does.** It reads the old file, writes one entry per path, deletes `.forge/scaffolded.json`, and rewrites `.gitignore`'s `!.forge/scaffolded.json` to `!.forge/scaffolded/`. The line is rewritten only when it is exactly forge's own; otherwise re-include `.forge/scaffolded/` yourself. In a `.gitattributes` copied from forge's scaffold, replace `.forge/scaffolded.json merge=forge-generated` with `.forge/scaffolded/** merge=forge-generated`.
