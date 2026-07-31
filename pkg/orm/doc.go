// Package orm is forge's typed data-access layer: a postgres-pinned query
// builder over uptrace/bun, plus the schema-truth machinery that projects
// entity types out of the applied migrations.
//
// The generated internal/db/<entity>_orm.go consumes it, so most projects
// reach for this package only through generated code. What a hand-written
// caller uses directly:
//
//   - [Client]        the handle; Bun() is the raw engine seam for queries
//     the generated CRUD cannot express.
//   - [ErrNoRows]     the missing-row sentinel. Match it with errors.Is and
//     let pkg/svcerr map it to a clean NotFound — never leak SQL text.
//   - <Entity>Columns the declared-column allowlist generated per entity,
//     used to validate an order_by against a closed set.
//   - [WhereILikeAny] the multi-column case-insensitive search predicate
//     behind generated search/query/q filters.
//   - [NullTime]      the tolerant nullable-timestamp scanner.
//
// Schema truth flows one way: db/migrations/*.up.sql is the source, and the
// entity types, ORM and CRUD wiring are PROJECTIONS of the applied schema
// (introspect.go / differ.go / migration.go). There is no entity annotation
// to declare — an entity is a table plus matching CRUD RPCs in a service
// proto.
package orm
