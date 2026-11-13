// Package dbanalyze measures what SQL does on a real postgres — the locks a
// migration statement takes, the tables it rewrites or scans, how long it
// runs — inside ONE transaction that is always rolled back, and reports it as
// the `forge.db-analyze/v1` document `forge db analyze` prints.
//
// The pieces, for any pass that analyzes a target:
//
//   - [Open] starts a [Session]: one connection, one transaction with SET
//     LOCAL statement_timeout / lock_timeout, rolled back by [Session.Close].
//     [Session.Exec] runs a statement under a savepoint, so a failure leaves
//     the transaction usable; [Session.Tx] is the transaction for a caller's
//     own reads (EXPLAIN).
//   - [PrepareScratch] is a scratch target: pgtest's embedded postgres,
//     migrated with a base set of migrations from a cached template.
//   - [SplitStatements] splits a SQL file the way postgres reads it.
//   - [AnalyzeMigrations] applies migrations statement by statement in the
//     session, measures each (pg_locks, pg_class.relfilenode,
//     pg_stat_xact_user_tables, wall time) and applies the rules.
//   - [Document] and its parts are the JSON document.
//
// Design: docs/proposals/db-analyze.md in the forge repository.
package dbanalyze
