// Package seeddata materializes deterministic, FK-coherent demo data
// directly into a development database — at RUNTIME, never as a persisted
// seed file in the user's project.
//
// # Why runtime, not a file
//
// The vertical-scaffolding law forbids forge from writing new forge-owned
// (regenerated) files into the user's project space. An earlier design
// persisted db/seeds/gen/*.sql; that is disqualified. Instead this package
// introspects the APPLIED schema (schemadef.ApplyAndIntrospect against the
// real ephemeral-postgres shadow, including the real introspected foreign
// keys), synthesizes deterministic per-cell values, builds a foreign-key
// topological INSERT order, and executes the INSERTs against the target dev
// database. No .sql seed file touches the user project.
//
// # Never-in-prod is structural, not conventional
//
// This package lives in forge's root module under internal/. Generated
// applications import only github.com/reliant-labs/forge/pkg/... — a
// separate module — so they CANNOT import this package: Go's module
// boundary plus internal-visibility make the applier unreachable from any
// shipped server/app binary by construction, not by convention. The CLI is
// the only caller. forge/pkg/app/migrate.go has no seed code path, so a
// production system has no component that can apply seeds. On top of that
// structural floor, the `forge db seed apply`/`reset` CLI refuses any
// non-dev environment with no override flag (see internal/cli).
//
// # Determinism
//
// Every cell value is a pure function of (salt, table, column, rowIndex)
// plus the column's DECLARED type and constraints — stateless per cell, no
// sequential PRNG. Consequences: identical (schema, config) yields
// byte-identical INSERTs; adding a column never reshuffles other columns'
// values (each column's value depends only on its own name); adding rows
// never changes existing rows. Foreign-key values are derived from the
// referenced table's own primary-key function, so every reference resolves
// by construction.
//
// # Two paths to one parent
//
// Determinism above is per-CELL, and that is exactly what makes a diamond
// invisible to it: when a table references a parent both directly and through
// another parent (orders.patient_id, and orders.prescription_id ->
// prescriptions.patient_id), every edge is individually valid and the two
// routes still name different rows. forge detects that from the foreign-key
// graph and REFUSES to seed until the project declares which route is
// authoritative, as a COMMENT on the foreign-key constraint. See diamond.go —
// it carries the reasoning, the vocabulary, and why the declaration lives on
// the constraint rather than in a forge-owned file.
//
// # Domain vocabulary
//
// Built-in synthesis invents values that are OBVIOUSLY invented: a string
// column nothing describes gets SyntheticStringPrefix + its own name + the row
// number. A project teaches the seeder its own vocabulary through the
// user-owned db/seeds/vocab.yaml overlay (see Vocab/LoadVocab/ApplyVocab):
// per-column value pools, validated against the introspected constraints,
// drawn with the same deterministic hash-pick. forge owns the seeding
// MECHANICS; the overlay supplies the domain WORDS.
//
// The overlay is not a nicety, it is the whole declaration surface, so it has
// to be findable: the scaffolded db/seeds/vocab.yaml says so in its header and
// the seeding skill says so in its second paragraph. A database full of
// `sample_*` is forge reporting that nobody told it anything, not forge
// failing.
//
// A pool of ONE is how a project pins an exact value: `users.email:
// ["known@example.com"]` puts that address in every row, and a `uuid`-typed
// column's pin is checked against the canonical UUID spelling before the seed
// runs (see vocabValueProblem) rather than aborting the transaction at INSERT.
// That is the whole of what the overlay decides — every value forge would
// otherwise have invented.
//
// # Semantic types
//
// An entry may also declare what KIND of value a column holds, instead of
// listing values:
//
//	columns:
//	  orders.customer_email:   {type: email}
//	  orders.shipping_country: {type: country_code}
//	  products.sku:            {type: uuid}
//
// The values come from gofakeit (see vocabtype.go), expanded into an ordinary
// pool at load, so everything downstream — validation against the column's
// constraints, the deterministic per-column draw, the UNIQUE assignment —
// treats them exactly like a hand-written list. VocabTypeNames() is the
// supported set, derived from the same table the resolver uses.
//
// This exists because constraint-derivation has a floor. forge satisfies the
// constraints it can INVERT — lengths, IN-lists, tractable patterns — but a
// regex postgres accepts that Go's RE2 cannot compile has nothing to invert,
// and `char_length(country) = 2` is honestly satisfied by "00" even though the
// column means an ISO code. Neither gap is closed by guessing: a wrong fixture
// that happens to pass is how a bad value ships silently. What closes them is
// the author declaring a type, and — for the generated CRUD lifecycle test —
// codegen's generate-time guard, which asks postgres to evaluate each fixture
// against the applied CHECK and REFUSES to emit a file its own migration would
// reject, naming the column, the constraint, and the type to declare.
//
// # What the overlay cannot say, and what stands in for it
//
// KEYS are not overridable. ApplyVocab refuses an entry on a primary-key or
// foreign-key column, because referential coherence is the one thing the
// seeder guarantees by construction and a hand-written key would be a
// reference forge cannot prove resolves. Two consequences worth knowing before
// reaching for the overlay:
//
//   - A project cannot DECLARE the id of a particular row — not its dev
//     principal's, not any other's. Ids come from the key function
//     (deterministicUUID), which is a pure function of (salt, table, row).
//   - A project cannot point one row at another BY NAME. There is no way to
//     write "this order belongs to that user" in vocab.yaml, because both
//     halves are keys.
//
// What stands in for the second is the row-0 spine: every non-self foreign key
// on row 0 resolves to its parent's row 0 (see fkParentRowIndependent), so one
// fully-connected dataset always exists — row 0 of every table belongs to row 0
// of every table it references. Plan.SeedValue(table, column, row) reads any of
// those values back out, which is how a caller obtains the id it could not
// name. That is a discovery mechanism, not a declaration one, and the gap is
// real: a value that has to exist BEFORE the seed runs — a principal id baked
// into an issued token, say — has nowhere in this overlay to come from.
//
// # What the seeder will not infer
//
// No IDENTIFIER says anything to the seeder — not a table's name, not a
// column's. An identifier is evidence of what someone called a thing, never
// of what the thing means, and a generator that reads one is right for the
// vocabulary it was written against and silently wrong everywhere else.
// Which row is an authenticated principal, which entity is "the user", what
// a `last4` column holds, which currency an app trades in, what a JSON
// document contains — all of it is domain knowledge the project declares (in
// the overlay, in a constraint COMMENT, or in db/seeds/custom/).
//
// The rule that decides every case: DERIVABLE → DERIVE, DECISION → DECLARE.
// A column's type, its CHECK vocabulary, its pattern, its length and range
// bounds, its NOT NULL and its foreign keys are all derivable, and synthesis
// is built from every one of them. What noun belongs in the column is a
// decision, it is not in the schema, and forge does not invent it. See
// synth.go for what that costs and what it bought.
package seeddata
