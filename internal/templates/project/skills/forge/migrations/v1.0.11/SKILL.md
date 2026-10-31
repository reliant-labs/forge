---
name: v1.0.11
description: Upgrade a project to forge v1.0.11. More generated files merge with plain git. The read-only and dashboard grants become one constant script per role plus one row file per table (db/grants/readonly/, db/grants/dashboard/), the config module (deploy/kcl/config_gen.k) and the scenario handler map (src/mocks/scenario-rpcs_gen.ts) certify each config block and service separately, every sectioned file (contract mocks included) names each section and puts its hash on the next line, the scaffold ledger .forge/scaffolded.json becomes one entry file per path under .forge/scaffolded/, and the mock-fixture fingerprint leaves the committed fixture-manifest_gen.ts for a gitignored marker. Use when the project commits any of those files.
version: v1.0.11
detection: test -f .forge/scaffolded.json -o -f db/grants/readonly_gen.sql -o -f deploy/kcl/config_gen.k && exit 0; ls frontends/*/src/mocks/scenario-rpcs_gen.ts frontends/*/src/mocks/fixture-manifest_gen.ts >/dev/null 2>&1 && exit 0; exit 1
---

# Upgrading to v1.0.11

The first `forge generate` on v1.0.11 migrates everything below. Commit what it writes in one change.

## 1. Grants, config module and scenario handler map certify each part

Each of these files gathers independent parts into one file, and each carried ONE `forge:hash` over all of it. Two pull requests that changed different parts (two tables' columns, two config blocks, two services' rpcs) both rewrote that one line, and git, including GitHub's server-side merge, which runs no merge driver, reported a conflict. Over the week to 2026-10-30, `readonly_gen.sql` and `config_gen.k` conflicted in 34 pull requests across reliant and control-plane, every time on that line alone.

Each part now begins with a header naming it, `forge:section <name>` (the table, the config block, the service), and the line after it, `forge:hash=…`, certifies that part. The file's head hash covers only the shared preamble. A file still counts as forge's untouched render only when every hash verifies, so hand-edit detection is unchanged. Two pull requests that change different parts now merge into exactly what `forge generate` writes for both.

- **`db/grants/readonly_gen.sql`, `db/grants/dashboard_gen.sql`:** these are now one CONSTANT script per role, plus one row file per table under `db/grants/readonly/` and `db/grants/dashboard/` (`<schema>.<table>_gen.sql`). A row is the table's `(schema, table, readable columns)`, with its withheld columns (`forge:secret`, and credential-shaped columns nobody decided) noted beside it. The migrator splices the rows into the script in file-name order and applies one statement, as before. Two pull requests that each add a table now add two files, even when both tables sort between the same neighbours, which no single file could merge, and dropping a table deletes its file. `db/embed_gen.go` now embeds the `db/grants` directory (`//go:embed migrations/*.sql grants`). `forge generate` writes all of it; commit both directories. A binary built before this regeneration still applies the old one-file script it embeds.
- **`deploy/kcl/config_gen.k`:** one section per config block schema, per root schema, per env-projection lambda and per process selector. Inside each lambda, one section per config block's env entries.
- **`src/mocks/scenario-rpcs_gen.ts`:** `ScenarioRpcHandlers` is an intersection of one object type per service, each its own section. Every request and response type is imported inline (`import("@/gen/…").Name`), so the shared `import type` block is gone, and so are the `<module>_<Name>` aliases for message names two services share. Scenario files that use `ScenarioRpcHandlers` need no change.

### Named headers, in every sectioned file

v1.0.10 sectioned the contract mocks (`mock_gen.go`, one section per interface) with the hash on the header itself: `// forge:section forge:hash=…`. That header is the first line of its part, and a part inserted into the file lands directly before the header of the part after it. So a pull request that changed one part and another that added a part right before it conflicted on that header. This is the common case: in a project with two services, a new `orders` service sorts directly before a changed `users` service. Every sectioned file, the mocks included, now writes the named two-line form, and the header stays the same while its part exists. The name also makes each header unique, which keeps git from aligning one part's header with another's.

The first `forge generate` restamps every sectioned file into the new form. The old form still verifies as forge's own render, so nothing reads as hand-edited during the upgrade. In `config_gen.k`, the `<SCHEMA>_SENSITIVE_ENV` and `<SCHEMA>_OPTIONAL_SECRET_ENV` lists now hold one entry per line, grouped by config block like the env map, so two blocks gaining credentials no longer rewrite one shared line. Their values are unchanged.

### Still a conflict

Two pull requests that change the SAME table, config block or service still conflict there, because both changed that part. Line merging cannot take two other shapes either: two pull requests that each add a new part between the same two neighbours, and one that deletes a part while the other deletes or adds a part right beside it. `forge generate` on the merged inputs resolves each of them. Generators whose parts are born and removed independently are moving to one file per part, which removes these shapes too.

## 2. `.forge/scaffolded.json` becomes `.forge/scaffolded/`

**Use when** the project commits `.forge/scaffolded.json`.

**What changed.** The scaffold-once birth ledger, the record that keeps a deleted scaffold deleted, was one sorted file. Every branch that scaffolds anything records births in it, so two branches' births were neighbouring insertions there, and git conflicted. Each path's record is now its own file, at the path under `.forge/scaffolded/` with `.json` appended (`.forge/scaffolded/internal/app/compose.go.json`). It holds `{}` for an ordinary birth, or the decline reason, birth hash or reported-absent mark the path carries. The birth timestamp is gone: nothing read it, and it made two branches scaffolding the same path disagree. Two branches now conflict there only over the same path.

**What generate does.** It reads the old file, writes one entry per path, deletes `.forge/scaffolded.json`, and rewrites `.gitignore`'s `!.forge/scaffolded.json` to `!.forge/scaffolded/`. The line is rewritten only when it is exactly forge's own; otherwise re-include `.forge/scaffolded/` yourself. In a `.gitattributes` copied from forge's scaffold, replace `.forge/scaffolded.json merge=forge-generated` with `.forge/scaffolded/** merge=forge-generated`.

## 3. The mock-fixture fingerprint is no longer committed

**Use when** a frontend has `src/mocks/fixture-manifest_gen.ts`.

**What changed.** `fixture-manifest_gen.ts` recorded `SEED_FINGERPRINT`, one digest of every migration and `db/seeds/vocab.yaml`. Every branch that added a migration rewrote that line, so any two such branches conflicted on it (control-plane's manifest did, in 45 merges in the week to 2026-10-30). A per-migration list would not help, because two new migrations always sort to the same place. The fingerprint is tooling state that no compiler needs, so, like the descriptor (v1.0.10), it leaves git. `forge generate` now writes it to a marker, `.forge/fixtures/<frontend>/generated-from.json`, which every forge project already ignores. The marker also records the sha256 of each generated file it wrote beside the fixtures. The committed manifest keeps only constants (`PROJECT_ROOT_FROM_FRONTEND`, `FIXTURE_MARKER`), so a migration no longer changes it. `SEED_FINGERPRINT`, `SEED_CONFIG_FINGERPRINT` and `SEED_FINGERPRINT_FILES` are gone; the marker carries the same values as `seed_fingerprint`, `seed_config_fingerprint` and `seed_fingerprint_files`.

**What the freshness guard does now.** `fixture-freshness_gen.test.ts` still recomputes the digest from the files on disk.
- When the marker describes the fixtures on disk, a changed digest **fails** with the fix, as before.
- With no marker (a fresh clone, CI), or with fixtures that differ from the ones it describes (pulled or checked out since), the guard **skips**, naming `forge generate`. Committed fixtures are checked against their inputs by `forge ci verify-generated`.

**What to do.** Nothing by hand, unless your own code imports one of the removed constants. In that case, read the marker instead.
