---
name: v1.0.10
description: Upgrade a project to forge v1.0.10. Generated files merge with plain git. .forge/hashes.json becomes one certificate file per JSON output under .forge/hashes/. Contract mocks (mock_gen.go), the read-only and dashboard grants (db/grants/*_gen.sql), the config module (deploy/kcl/config_gen.k) and the scenario handler map (src/mocks/scenario-rpcs_gen.ts) certify each interface, table, config block and service separately, so pull requests that change different ones no longer conflict. Use when the project commits any of those files.
version: v1.0.10
detection: test -f .forge/hashes.json -o -f db/grants/readonly_gen.sql -o -f deploy/kcl/config_gen.k && exit 0; ls internal/*/mock_gen.go frontends/*/src/mocks/scenario-rpcs_gen.ts >/dev/null 2>&1 && exit 0; exit 1
---

# Upgrading to v1.0.10

The first `forge generate` on v1.0.10 migrates everything below. Commit what it writes in one change.

## 1. `.forge/hashes.json` becomes `.forge/hashes/`

**Use when** the project commits `.forge/hashes.json`.

**What changed.** A JSON output (observability dashboards and specs, the config schema) cannot carry the embedded `forge:hash` header, so its certificate, the hash of forge's last render, lived in one shared `.forge/hashes.json`. Every branch that re-rendered any JSON output edited that file, and two such branches conflicted in it even when the outputs did not. Each certificate is now its own file, at the output's path under `.forge/hashes/` with `.sha256` appended:

```
deploy/observability/spec/alert-service-chat_gen.json
.forge/hashes/deploy/observability/spec/alert-service-chat_gen.json.sha256
```

Two branches now conflict there only when both changed the same output.

**What generate does.** It reads the old file, writes one certificate per entry, deletes `.forge/hashes.json`, and rewrites `.gitignore`'s `!.forge/hashes.json` to `!.forge/hashes/`. The `.gitignore` line is rewritten only when it is exactly forge's own. If your `.gitignore` re-includes `.forge` state some other way, re-include `.forge/hashes/` yourself, or every clone will see its JSON outputs as uncertified. A `.gitattributes` copied from forge's scaffold may still name `.forge/hashes.json merge=forge-generated`: replace that line with `.forge/hashes/** merge=forge-generated`. forge's merge driver writes the new route into the clone's own attributes either way.

## 2. Mocks certify each interface separately

**Use when** the project has contract packages (`internal/<pkg>/mock_gen.go`).

**What changed.** `mock_gen.go` carried one `forge:hash` over the whole file, so two branches that changed different interfaces of one package conflicted on that one header line. Each mock now begins with its own `// forge:section forge:hash=…` line, which certifies that mock alone. The header hash covers only the package clause and imports. A file is pristine only when every hash verifies, so hand-edit detection is unchanged. Two branches that change different interfaces now merge with plain git into what `forge generate` writes for both.

**What generate does.** It rewrites every `mock_gen.go`. Mock names and behaviour are unchanged. Nothing to do by hand.

## 3. Grants, config module and scenario handler map certify each part

Each of these files gathers independent parts into one file, and each carried ONE `forge:hash` over all of it. Two pull requests that changed different parts (two tables' columns, two config blocks, two services' rpcs) both rewrote that one line, and git, including GitHub's server-side merge, which runs no merge driver, reported a conflict. Over the week to 2026-10-30, `readonly_gen.sql` and `config_gen.k` conflicted in 34 pull requests across reliant and control-plane, every time on that line alone.

Each part now begins with its own `forge:section forge:hash=…` comment, which certifies that part. The header hash covers only the shared preamble. A file still counts as forge's untouched render only when every hash verifies, so hand-edit detection is unchanged. Two pull requests that change different parts now merge into exactly what `forge generate` writes for both.

- **`db/grants/readonly_gen.sql`, `db/grants/dashboard_gen.sql`:** one section per table. The table's columns list starts with a constant first row, so every table's row starts with its own comma, and adding the last table never rewrites the row before it. The withheld-column notes (`forge:secret`, and credential-shaped columns nobody decided) moved from the end of the file into each table's section. The SQL the migrator applies is unchanged in effect.
- **`deploy/kcl/config_gen.k`:** one section per config block schema, per root schema, per env-projection lambda and per process selector. Inside each lambda, one section per config block's env entries.
- **`src/mocks/scenario-rpcs_gen.ts`:** `ScenarioRpcHandlers` is an intersection of one object type per service, each its own section. Every request and response type is imported inline (`import("@/gen/…").Name`), so the shared `import type` block is gone, and so are the `<module>_<Name>` aliases for message names two services share. Scenario files that use `ScenarioRpcHandlers` need no change.

### Still a conflict

Two pull requests that change the SAME table, config block or service still conflict there, because both changed that part. So do two that each add a new one between the same two neighbours. `forge generate` on the merged inputs resolves either.
