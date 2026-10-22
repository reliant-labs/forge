---
name: query
description: Read an environment's app data with forge db query <env> — one read-only statement, the transports (direct, port-forward), the env's data_access policy (open, audited, deny), the forge_ro role that cannot read forge:secret columns, the JSON document and exit codes.
---

# Reading an env's data: `forge db query`

`forge db query <env> "<sql>"` runs ONE read-only statement against an environment's app database and prints the rows. It is the database leg of the debug loop: a trace or an issue says what failed, and the rows say what state caused it.

```bash
forge db query dev "select count(*) from jobs where crew_id is null"
forge db query dev -f report.sql --format csv > report.csv
forge db query dev "select * from invoices order by created_at desc limit 5" --json
forge db query preview --database ledger "select id, status from entries limit 20"
forge db query staging "select status, count(*) from jobs group by 1" --confirm-env staging --reason "ISS-7f3a"
```

**The env is required, dev included.** A query names the database it reads; there is no default env to read by accident.

## What a statement may do

Every query runs in a `READ ONLY` transaction with a statement timeout and a lock timeout, as one statement over the extended protocol, then rolls back. Writes, `nextval()`, DDL and a second statement are refused. A lexical guard refuses them first so the message is readable; Postgres refuses them anyway. `SELECT`, `WITH`, `TABLE`, `VALUES`, `SHOW` and `EXPLAIN` lead a statement. `EXPLAIN ANALYZE` is allowed only over a `SELECT`.

| Limit | Default | Flag | Maximum |
|---|---|---|---|
| rows | 200 | `--max-rows` | 10,000 |
| statement timeout | 5s | `--timeout` | 30s |
| one cell | 4 KiB, then cut with a marker | — | — |
| whole response | 4 MiB | — | — |

A cap that cuts the result sets `truncated` (and `truncated_by`: `rows` or `bytes`). Narrow the query rather than raising the cap.

## Which database, and how forge reaches it

The workload decides (`--workload`, default `api`):

| Workload runs | Transport | Connects as |
|---|---|---|
| OnHost / OnCompose | **direct**: the workload's resolved `DATABASE_URL`, the same value `forge env exec` gives the process | whatever that URL names |
| OnCluster, in an env declared `lifecycle = "local"` or `"ephemeral"` | **port-forward**: `kubectl port-forward` with your kube context to the env's ManagedDatabase (`<name>-ro` when it runs standbys, else `<name>-rw`) | `forge_ro` |
| OnCluster in a real env, or OnHosted | not reachable from forge yet: exit 2 | — |

`--database <name>` picks the ManagedDatabase when a cluster env declares several.

## The read-only role

Every ManagedDatabase carries `forge_ro`, a role forge declares on the CNPG Cluster. Its password is in the `<name>-forge-ro` Secret. `forge generate` projects its grants from the applied schema into `db/grants/readonly_gen.sql`, and the migrator applies them after every run. The role may SELECT every column **except** those marked `forge:secret` in the proto, and nothing else: no writes, no sequences, no views.

The boundary is Postgres, not a redactor. `select upper(api_key) from …` is refused (`permission_denied`, exit 1) just like `select api_key`. A column added later is unreadable until a migration converges the grants. Don't grant the role more by hand; the next migration revokes it.

## Who may query: `data_access`

The env declares its policy in KCL, and forge enforces it before connecting:

```
data_access = forge.DataAccess {db_query = "audited"}
```

| `db_query` | Who | Needs |
|---|---|---|
| `open` | anyone with the env | nothing. The default for `lifecycle` local or ephemeral |
| `audited` | a person | `--confirm-env <env>` (the env name again) and `--reason "…"` (at most 500 characters). The default for every other env |
| `deny` | nobody | — |

The policy is recorded in the release shape. For a hosted env the control plane enforces it server-side and audits every call; a client flag cannot weaken it.

## Output for agents

`--json` (the same as `--format json`) prints one document to stdout:

```json
{"ok": true, "exit_code": 0, "schema": "forge.db.query/v1",
 "scope": {"env": "dev", "transport": "direct", "workload": "api", "policy": "open"},
 "columns": [{"name": "n", "pg_type": "int8"}], "rows": [[3]],
 "row_count": 1, "truncated": false, "duration_ms": 4, "bytes": 1}
```

- `ok` and `exit_code` are computed from the same error the process exits with.
- On failure `error` carries the message, and `reason` carries a stable subcode such as `multiple_statements` or `statement_timeout`.
- Cells are JSON values. A bigint or numeric beyond 15 digits is a string, so no reader rounds it. Dates, uuids and arrays are Postgres's text. `json`/`jsonb` is inline JSON.
- `scope.database` is present on the port-forward transport.
- `release` is the release live in the env when the query ran (`version`, `promotion_id`, `promoted_at`, and `unsettled` when its rollout hadn't finished), read from the env's release ledger. It's absent when nothing was promoted yet; an unreadable ledger warns on stderr and never fails the query. `forge env releases at <env> <time>` answers the same question for any moment.

| Exit | Meaning |
|---|---|
| 0 | rows returned |
| 1 | the statement or the request is wrong: the guard, the read-only transaction, a SQL error, a permission error, the env's policy |
| 2 | forge could not look: no database reachable, the tunnel, a timeout, a lost connection |

Treat exit 2 as "not answered", never as "no rows".

## Rules

- Never point the query at a database by URL to dodge the policy. `forge db query` is the audited path.
- Never ask for a write through it. Data fixes are migrations or application code, reviewed like any change.
- Prefer aggregates and `limit` to dumping tables: the caps exist so an agent's loop cannot scan a production database.
