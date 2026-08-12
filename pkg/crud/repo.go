package crud

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/schema"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/reliant-labs/forge/pkg/orm"
	"github.com/reliant-labs/forge/pkg/svcerr"
)

// repoTracer is the span source for the generic repository. The pre-generic
// per-entity ORM emitted one tracer per package ("orm"); the library keeps
// the same tracer name so existing dashboards/queries that filter on the
// "orm" tracer and the "orm.<Op><Entity>" span names keep working.
var repoTracer = otel.Tracer("orm")

// Everything a Repo needs is DERIVED from the Bun-tagged model at first use
// (once per entity, never in the hot path): table name, primary key column +
// Go field, server-allocated PK, soft delete in both its native and legacy
// forms, managed timestamps, array columns, the full column set, and the
// per-column write policy carried by Bun's ,skipupdate tag.
//
// There is no per-entity descriptor to pass, and that is the point. A
// descriptor would be a second copy of facts the struct already carries —
// checked against nothing, silently wrong when the two drifted. The struct
// tags are the projection of the applied schema, so reading them IS reading
// the schema.
//
// The one thing reflection cannot answer is where a column's write policy
// comes from: `forge:immutable` is declared in the migration (COMMENT ON
// COLUMN) and projected onto the tag by the generator. The repo reads that
// tag itself in ensureMeta rather than letting Bun enforce it at write time —
// Bun's own enforcement is unconditional and would also block the masked
// path, which must be able to write the column on an explicit, deliberate
// mask (see updatable vs updatableSet below).
//
// The same is true of `forge:version`, the optimistic-concurrency column,
// with one extra wrinkle: Bun has no native concept for it at all, so the
// generator marks it in a SECOND tag namespace (`forge:"version"`) that
// Bun's tag parser never inspects. See ensureMeta for why that beats
// inventing a private bun tag option.

// meta is the reflection-derived, cached half of a Repo's knowledge. It is
// computed once (sync.Once, off the first IDB the repo sees) from Bun's
// table schema for Model and never recomputed — Bun itself caches the
// *schema.Table per type, so even the first computation is a single schema
// build shared process-wide.
type meta struct {
	table         string // SQL table name
	entityName    string // model Go type name (for span names)
	pkColumn      string // primary-key column name
	pkGoField     string // primary-key struct field (Go) name
	pkAutoInc     bool   // server-allocated PK (SERIAL/IDENTITY) → RETURNING
	pkIsString    bool   // string PK → ULID-generate when empty on Create
	nativeSoftDel bool   // Bun owns soft delete (,soft_delete time column)
	// legacyTextSoftDel: a deleted_at column Bun's ,soft_delete did not
	// claim (a TEXT column it cannot round-trip a time.Time through), so
	// the repo hand-rolls the IS NULL filter and the stamp.
	legacyTextSoftDel bool
	// timestamps: created_at AND updated_at both exist and both project to
	// a stampable type — forge's managed-timestamp convention, read off the
	// columns rather than declared.
	timestamps   bool
	columns      []string // ordered declared column allowlist
	hasUpdatedAt bool     // updated_at column exists (managed-stamp target)
	nilSliceCols []sliceCol
	updatable    []string // columns settable by a full Update (SET clause)
	updatableSet map[string]bool
	skipUpdate   map[string]int        // ,skipupdate column → struct field index; see UpdateMasked
	stampFields  map[string]stampField // created_at/updated_at → field info
	// versionColumn is the SQL name of the `forge:version` column, "" when
	// the entity declared none. A non-empty value is what switches
	// Update/UpdateMasked from last-writer-wins to optimistic concurrency
	// control; an entity without one never builds the predicate and never
	// issues the disambiguating re-query, so it costs exactly one string
	// comparison per write.
	versionColumn string
	// versionFieldIndex is the struct-field index of that column, used to
	// read the caller's last-seen value for the WHERE predicate.
	versionFieldIndex int
	// fillULIDFields are the struct-field indices of columns declared
	// `forge:fill=ulid` (a NON-PK column — the PK's own ULID generation is
	// pkIsString above). Create ULID-generates each when the caller left it
	// at its Go zero (empty string), the same chokepoint and same
	// empty-means-unset convention as the PK.
	fillULIDFields []int
}

// sliceCol pairs a NOT NULL slice-typed column's struct-field index with
// its SQL name so the repo can nil-normalize it (and, for masked writes,
// only when named).
//
// In Go a nil slice binds as SQL NULL, which a NOT NULL column rejects — so
// any such column left unset fails the INSERT outright. That is true of the
// `repeated <scalar>` columns forge has always emitted (`TEXT[] NOT NULL
// DEFAULT '{}'`) and equally of a `bytes` column (`BYTEA NOT NULL DEFAULT
// '\x'`), which is a slice field WITHOUT the `,array` tag and so was missed
// while `bytes` was a shape the generator could not produce at all.
//
// Selected on NOT NULL rather than on the tag: for a NULLABLE slice column
// nil IS the absent value, and normalizing it would erase the distinction
// between "no value" and "empty value" that the column was declared to keep.
type sliceCol struct {
	fieldIndex int
	column     string
}

// stampField records how to set a managed-timestamp column: its struct
// field index, whether it is a pointer field, and whether its Go type is
// string (RFC3339Nano) or time.Time. A column whose type the repo can't
// stamp is absent from the map (left untouched, matching the generator's
// stampableTimestamp guard).
type stampField struct {
	index    int
	isPtr    bool
	isString bool
}

// Repo is the generic data-access layer over a Bun-tagged model M. One
// Repo per entity replaces the ~250 LOC of per-entity Create/Get/List/
// Count/ListAll/Update/UpdateMasked/Delete the generator used to emit; the
// generated code now supplies only the Bun-tagged struct, the ToProto/
// FromProto pair, and a single crud.NewRepo[Model]() line.
//
// All lifecycle semantics the pre-generic code carried are preserved
// exactly: Bun-native + legacy-TEXT soft delete, the
// deleted_at IS NULL update-guard (Bun auto-scopes SELECT/DELETE to live
// rows but NOT UPDATE), AIP-134 masked updates with an updatable allowlist
// → orm.UnknownFieldError, managed-timestamp stamping, server-allocated /
// ULID PKs, and array nil→{} normalization. The QueryOption escape hatch
// (orm.QueryOption func(*bun.SelectQuery)) is threaded straight through to
// List/Count, and bun.IDB (db.Bun()) remains the raw-SQL escape hatch.
type Repo[M any] struct {
	once sync.Once
	m    meta
}

// NewRepo constructs a Repo for model M. Metadata derivation is deferred to
// the first call, which needs a live bun.IDB to reach the dialect's table
// cache; everything it needs comes off the model's own Bun tags.
func NewRepo[M any]() *Repo[M] {
	return &Repo[M]{}
}

// modelType returns the (dereferenced) struct type of M.
func (r *Repo[M]) modelType() reflect.Type {
	var zero M
	typ := reflect.TypeOf(zero)
	if typ == nil {
		// M is an interface or pointer with a nil zero value; fall back to
		// the element type of *M.
		typ = reflect.TypeOf((*M)(nil)).Elem()
	}
	for typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	return typ
}

// ensureMeta derives and caches the reflection metadata from Bun's schema
// for M, the first time the repo is handed a database handle. Bun caches
// the *schema.Table per type process-wide, so this is one schema build per
// entity regardless of how many Repos or calls reference it.
//
// # Finding the optimistic-concurrency column
//
// Every other fact here is read off Bun's own parsed tag, because Bun has
// a native concept for it (f.SkipUpdate, f.IsPK, tbl.SoftDeleteField).
// Optimistic concurrency is the one fact Bun has no vocabulary for, so the
// generator declares it in a SEPARATE struct-tag namespace and this reads
// it back off the raw reflect.StructField Bun keeps beside its own tag:
//
//	Version int64 `bun:"version,notnull,skipupdate" forge:"version"`
//
// The alternative — an invented bun option, `bun:"version,occ"` — was
// rejected because Bun validates its own option vocabulary
// (schema.isKnownFieldOption) and logs "unknown tag option" for anything
// outside it, so every generated struct carrying one would print a warning
// forge cannot suppress and users cannot act on. A separate namespace is
// also the honest description: this is forge's declaration, not Bun's, and
// nothing about it should look like a feature of the query engine.
//
// Deriving it from the column NAME instead (any column called "version")
// was rejected for the opposite reason: "version" is an ordinary domain
// word — a document's revision label, a schema_version, an app's semver
// string — and silently adding a WHERE predicate to a table because a
// column's name matched would break writes on schemas that never asked for
// OCC. The marker is opt-in precisely so it cannot be triggered by accident.
func (r *Repo[M]) ensureMeta(db orm.Context) {
	r.once.Do(func() {
		typ := r.modelType()
		tbl := db.Bun().Dialect().Tables().Get(typ)

		r.m.table = tbl.Name
		r.m.entityName = typ.Name()
		r.m.columns = make([]string, 0, len(tbl.Fields))
		r.m.stampFields = map[string]stampField{}

		// Managed timestamps are a property of the COLUMNS, so they are read
		// off the columns rather than declared: forge stamps created_at and
		// updated_at only when both exist AND both project to a type the repo
		// can actually stamp. An exotic pair (epoch integers, arrays) is plain
		// schema, and stampFieldFor is the same gate the generator applies.
		//
		// Deriving it here rather than accepting it as a flag keeps one answer
		// to the question. A flag would be a second copy of a fact the struct
		// already carries, and the two could disagree.
		var created, updated *schema.Field
		for _, f := range tbl.Fields {
			switch f.Name {
			case "created_at":
				created = f
			case "updated_at":
				updated = f
			}
		}
		if created != nil && updated != nil {
			_, createdOK := stampFieldFor(created)
			_, updatedOK := stampFieldFor(updated)
			r.m.timestamps = createdOK && updatedOK
		}

		for _, f := range tbl.Fields {
			r.m.columns = append(r.m.columns, f.Name)
			if f.NotNull && f.IndirectType.Kind() == reflect.Slice {
				r.m.nilSliceCols = append(r.m.nilSliceCols, sliceCol{fieldIndex: f.Index[0], column: f.Name})
			}
			if f.Name == "updated_at" {
				r.m.hasUpdatedAt = true
			}
			if r.m.timestamps && (f.Name == "created_at" || f.Name == "updated_at") {
				if sf, ok := stampFieldFor(f); ok {
					r.m.stampFields[f.Name] = sf
				}
			}
		}
		switch len(tbl.PKs) {
		case 1:
			pk := tbl.PKs[0]
			r.m.pkColumn = pk.Name
			r.m.pkGoField = pk.GoName
			r.m.pkAutoInc = pk.AutoIncrement || pk.Identity
			r.m.pkIsString = pk.IndirectType.Kind() == reflect.String
		case 0:
			r.m.pkColumn = "id"
			r.m.pkGoField = "Id"
			r.m.pkIsString = true
		default:
			// Composite PK (bun's tbl.PKs has more than one member): auto id
			// generation and PK-cursor pagination are only meaningful for a
			// single key column, so both are disabled rather than guessing
			// off PKs[0]. Guessing is exactly the bug this guards — a table
			// like PRIMARY KEY (company_id, kind) where company_id is ALSO
			// a foreign key would otherwise report pkIsString=true for
			// company_id, and Create's ULID-on-empty branch below would
			// overwrite the FK with a fresh ULID, silently corrupting the
			// reference. Leaving pkColumn "" is the existing, correct
			// escape hatch orderKeysetSafe already treats as "no PK-cursor
			// pagination" (see pkg/crud/crud.go); pkGoField/pkAutoInc/
			// pkIsString stay at their zero values so pkFieldValue and the
			// ULID branch are never reached for these entities.
			r.m.pkColumn = ""
		}
		if tbl.SoftDeleteField != nil {
			r.m.nativeSoftDel = true
		}

		// A deleted_at column Bun's ,soft_delete did NOT claim is the legacy
		// TEXT form: Bun stamps a time.Time that a TEXT column cannot
		// round-trip, so the repo hand-rolls the filter and the stamp. The
		// repo already derives the native half from tbl.SoftDeleteField; the
		// legacy half is its complement, so deriving it here replaces a flag
		// that only ever restated what these two facts already imply.
		if !r.m.nativeSoftDel {
			for _, f := range tbl.Fields {
				if f.Name == "deleted_at" {
					r.m.legacyTextSoftDel = true
					break
				}
			}
		}

		// The optimistic-concurrency column, if the entity declared one.
		// Resolved BEFORE the allowlists below, which exclude it by name
		// through columnExcludedFromSet.
		//
		// Only the FIRST such column is honored. Two version columns cannot
		// both be authoritative — a write satisfying one predicate and
		// failing the other has no coherent answer — and silently checking
		// only one while incrementing both would be worse than either. The
		// generator cannot produce this shape (a second forge:version marker
		// is a migration authoring mistake), so the recovery is to pick
		// deterministically rather than to panic in a library that runs
		// inside somebody's request path.
		for _, f := range tbl.Fields {
			if versionTagged(f) {
				r.m.versionColumn = f.Name
				r.m.versionFieldIndex = f.Index[0]
				break
			}
		}

		// forge:fill=ulid columns: every one of them, not just the first —
		// unlike forge:version (a single predicate slot) there is no
		// conflict in generating more than one ULID per row (an invite
		// code AND a share token, say).
		for _, f := range tbl.Fields {
			if fillULIDTagged(f) {
				r.m.fillULIDFields = append(r.m.fillULIDFields, f.Index[0])
			}
		}

		// Two distinct allowlists, both starting from every declared column
		// EXCEPT the PK, deleted_at, the version column, and — under managed
		// timestamps — created_at and updated_at (the latter is repo-stamped,
		// never caller-set):
		//
		//   - updatableSet — the MASKED-update allowlist: a path an
		//     update_mask may name. A ,skipupdate column IS here, because a
		//     mask that names it is the caller deliberately asserting a new
		//     value (Create's sibling write path).
		//   - updatable    — the FULL-REPLACE SET list. A ,skipupdate column
		//     is EXCLUDED here: Bun's own ,skipupdate enforcement is
		//     unconditional (it would drop the column from a masked SET too,
		//     see UpdateMasked), so the repo applies forge's conditional
		//     rule itself instead of delegating to Bun.
		r.m.updatableSet = make(map[string]bool, len(tbl.Fields))
		r.m.skipUpdate = make(map[string]int)
		for _, f := range tbl.Fields {
			if r.columnExcludedFromSet(f.Name) {
				continue
			}
			r.m.updatableSet[f.Name] = true
			if f.SkipUpdate() {
				r.m.skipUpdate[f.Name] = f.Index[0]
				continue
			}
			r.m.updatable = append(r.m.updatable, f.Name)
		}
	})
}

// stampFieldFor builds the stampField for a managed-timestamp Bun field, or
// returns ok=false when its Go type isn't one the repo can stamp (matching
// the generator's stampableTimestamp: time.Time or string, plus their
// nullable pointer variants).
func stampFieldFor(f *schema.Field) (stampField, bool) {
	t := f.IndirectType
	switch t.Kind() {
	case reflect.String:
		return stampField{index: f.Index[0], isPtr: f.IsPtr, isString: true}, true
	case reflect.Struct:
		if t == reflect.TypeOf(time.Time{}) {
			return stampField{index: f.Index[0], isPtr: f.IsPtr, isString: false}, true
		}
	}
	return stampField{}, false
}

// forgeTagKey is the struct-tag namespace forge uses for declarations Bun
// has no vocabulary for. Separate from `bun:"..."` so Bun's tag parser
// never sees it (see ensureMeta).
const forgeTagKey = "forge"

// versionTagValue is the forge-tag value marking the optimistic-concurrency
// column. It matches the `forge:version` catalog-comment marker the user
// writes in the migration (pkg/schemadef.ColumnMarkerVersion), minus
// the `forge:` prefix the tag namespace already supplies.
const versionTagValue = "version"

// fillULIDTagValue is the forge-tag value marking a `forge:fill=ulid`
// column: a non-PK column the generic Repo ULID-generates at Create when
// the caller left it at its Go zero (empty string), the same convention
// as the PK's own ULID-on-empty behavior below.
const fillULIDTagValue = "fill=ulid"

// versionTagged reports whether a Bun field carries forge's version tag.
// Read off the raw StructField rather than Bun's parsed Tag: the value
// lives in forge's own tag namespace, which Bun does not parse.
func versionTagged(f *schema.Field) bool {
	return f.StructField.Tag.Get(forgeTagKey) == versionTagValue
}

// fillULIDTagged reports whether a Bun field carries forge's fill=ulid tag.
func fillULIDTagged(f *schema.Field) bool {
	return f.StructField.Tag.Get(forgeTagKey) == fillULIDTagValue
}

// columnExcludedFromSet mirrors the generator's excludedFromSet: columns
// that never appear in an UPDATE SET clause a CALLER controls.
//
// The version column is excluded for the same reason the PK and created_at
// are: the repo owns its value. It is stricter than ,skipupdate, which
// only governs the full-replace path — a version column is barred from the
// masked path too, so an update_mask naming it is an UnknownFieldError
// rather than a way to hand-pick the version a write claims to have read.
// Letting a client set it would make the predicate self-satisfying, which
// is the whole guarantee gone.
func (r *Repo[M]) columnExcludedFromSet(col string) bool {
	if col == r.m.pkColumn || col == "deleted_at" {
		return true
	}
	if r.m.versionColumn != "" && col == r.m.versionColumn {
		return true
	}
	if r.m.timestamps && (col == "created_at" || col == "updated_at") {
		return true
	}
	return false
}

// Columns is the entity's declared column allowlist — the value the List
// handler shim used to pull from the generated db.<Entity>Columns var and
// hand to pkg/crud for order_by validation. Derived from Bun's schema.
func (r *Repo[M]) Columns(db orm.Context) []string {
	r.ensureMeta(db)
	return r.m.columns
}

// PkColumn is the primary-key column name (the List handler's cursor
// column). Derived from Bun's schema.
func (r *Repo[M]) PkColumn(db orm.Context) string {
	r.ensureMeta(db)
	return r.m.pkColumn
}

// startSpan opens a child span named like the pre-generic per-entity ORM
// ("orm.<Op><Entity>") so existing traces/dashboards keep their span names.
func (r *Repo[M]) startSpan(ctx context.Context, op string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	name := "orm." + op + r.m.entityName
	all := append([]attribute.KeyValue{attribute.String("table", r.m.table)}, attrs...)
	return repoTracer.Start(ctx, name, trace.WithAttributes(all...))
}

func recordErr(span trace.Span, err error) {
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// normalizeArrays sets every nil NOT NULL slice field to a non-nil empty
// slice so it binds as the empty value the column's DEFAULT already holds
// (`{}` for an array, `\x` for BYTEA) rather than as NULL.
func (r *Repo[M]) normalizeArrays(entity *M) {
	if len(r.m.nilSliceCols) == 0 {
		return
	}
	v := reflect.ValueOf(entity).Elem()
	for _, sc := range r.m.nilSliceCols {
		normalizeSlice(v.Field(sc.fieldIndex))
	}
}

// normalizeArraysFor normalizes only the slice fields named in mask.
func (r *Repo[M]) normalizeArraysFor(entity *M, mask map[string]bool) {
	if len(r.m.nilSliceCols) == 0 {
		return
	}
	v := reflect.ValueOf(entity).Elem()
	for _, sc := range r.m.nilSliceCols {
		if mask[sc.column] {
			normalizeSlice(v.Field(sc.fieldIndex))
		}
	}
}

func normalizeSlice(f reflect.Value) {
	if f.Kind() == reflect.Slice && f.IsNil() && f.CanSet() {
		f.Set(reflect.MakeSlice(f.Type(), 0, 0))
	}
}

// pkFieldValue returns the addressable PK struct field of entity.
func (r *Repo[M]) pkFieldValue(entity *M) reflect.Value {
	return reflect.ValueOf(entity).Elem().FieldByName(r.m.pkGoField)
}

// fillULIDColumns generates a ULID for every `forge:fill=ulid` column left
// at its Go zero (empty string) — the same empty-means-unset convention
// pkIsString uses for the PK. Called only from Create, matching where the
// PK's own ULID generation lives: Upsert deliberately does NOT ULID-generate
// its PK (see that method's doc comment), and the same reasoning against
// silent rotation applies here — an Upsert whose DO UPDATE branch ran this
// on every call would stomp an existing row's real token with a fresh one
// whenever the caller's struct left the field zero.
func (r *Repo[M]) fillULIDColumns(entity *M) {
	if len(r.m.fillULIDFields) == 0 {
		return
	}
	v := reflect.ValueOf(entity).Elem()
	for _, idx := range r.m.fillULIDFields {
		f := v.Field(idx)
		if f.Kind() == reflect.String && f.CanSet() && f.String() == "" {
			f.SetString(ulid.Make().String())
		}
	}
}

// ─── Create ────────────────────────────────────────────────────────────

// Create inserts a new row. Plain INSERT, never an upsert: a duplicate PK is
// a real error. Chokepoint invariants (matching the pre-generic emitter):
// a string PK is ULID-generated when empty; a server-allocated integer PK is
// excluded from the INSERT and read back via RETURNING; managed timestamps
// are stamped; nil array fields are normalized to {}.
func (r *Repo[M]) Create(ctx context.Context, db orm.Context, entity *M) error {
	r.ensureMeta(db)
	ctx, span := r.startSpan(ctx, "Create")
	defer span.End()

	if r.m.pkIsString {
		pk := r.pkFieldValue(entity)
		if pk.IsValid() && pk.Kind() == reflect.String && pk.String() == "" {
			pk.SetString(ulid.Make().String())
		}
	}
	r.fillULIDColumns(entity)
	if r.m.timestamps {
		r.stampCreate(entity)
	}
	r.normalizeArrays(entity)

	q := db.Bun().NewInsert().Model(entity)
	if r.m.pkAutoInc {
		q = q.ExcludeColumn(r.m.pkColumn).Returning("?", bun.Ident(r.m.pkColumn))
		pk := r.pkFieldValue(entity)
		if _, err := q.Exec(ctx, pk.Addr().Interface()); err != nil {
			recordErr(span, err)
			return fmt.Errorf("create %s: %w", r.m.table, err)
		}
		return nil
	}
	if _, err := q.Exec(ctx); err != nil {
		recordErr(span, err)
		return fmt.Errorf("create %s: %w", r.m.table, err)
	}
	return nil
}

// ─── Upsert ────────────────────────────────────────────────────────────

// Upsert writes entity by its primary key: INSERT, or on a PK conflict,
// UPDATE the existing row. It is Repo's ONLY upsert verb — Create stays a
// plain INSERT (see its doc comment) for exactly the callers this one is
// not for: an idempotent ingest, a sync-from-external-system, or a
// seed-or-update flow that does not know in advance whether the row
// exists.
//
// # Conflict target
//
// The conflict target is always the primary key. Upserting on an
// arbitrary unique index (a natural key distinct from the PK) is a
// materially bigger design — it needs its own conflict-column parameter,
// its own EXCLUDED-vs-existing-PK reconciliation, and its own tests — and
// is out of scope here. A caller that needs it writes the ON CONFLICT
// query by hand against db.Bun().
//
// # SET list
//
// The DO UPDATE SET list is r.m.updatable — the SAME allowlist a
// full-replace Update writes, which already excludes the PK, deleted_at,
// and (under managed timestamps) created_at/updated_at, plus any
// ,skipupdate column. An upsert is a write like any other: it must not
// let a round-tripped entity clobber a server-owned or secret column any
// more than Update may, so the two share one list rather than each
// maintaining its own idea of what is writable.
//
// # Timestamps
//
// created_at is stamped only on the INSERT path (when unset, same as
// Create); updated_at is stamped on both paths. An upsert that rewrote
// created_at on a conflict would misreport when the row was actually
// born — the DO UPDATE SET list never names created_at for exactly that
// reason.
//
// # Soft delete
//
// An upsert onto a tombstoned row RESURRECTS it (clears deleted_at along
// with the rest of the updatable set) rather than silently no-op'ing or
// erroring. Bun does not auto-scope INSERT the way it scopes SELECT/
// DELETE, and there is no WHERE clause on an INSERT to guard with the way
// Update's deleted_at IS NULL guard works — the row does not "exist" from
// the read side, so an upsert that refused to touch it would look
// indistinguishable from one that inserted successfully. Resurrection is
// the one behavior consistent with the verb's own name: the caller
// asserted "this row should exist with these values", and a soft-deleted
// row is, for that purpose, an absent one that Upsert is allowed to
// (re)create. A caller that wants tombstones to stay dead must Get first
// and branch.
//
// # Server-allocated / string PKs
//
// A caller supplying an EMPTY PK on a server-allocated (autoincrement)
// column cannot conflict with anything — there is no matching value to
// find — so it always inserts a fresh row exactly like Create. The same
// is true of an empty string PK, except Upsert does NOT ULID-generate one
// the way Create does: a generated PK can never already be present in the
// table, so ON CONFLICT could not fire regardless, and doing so would
// make Upsert silently degrade into Create for its most common accidental
// misuse (forgetting to set the PK) instead of surfacing the caller's
// mistake as a NOT NULL violation.
func (r *Repo[M]) Upsert(ctx context.Context, db orm.Context, entity *M) error {
	r.ensureMeta(db)
	ctx, span := r.startSpan(ctx, "Upsert")
	defer span.End()

	if r.m.timestamps {
		r.stampCreate(entity)
	}
	r.normalizeArrays(entity)

	q := db.Bun().NewInsert().Model(entity).
		On("CONFLICT (?) DO UPDATE", bun.Ident(r.m.pkColumn))
	for _, col := range r.m.updatable {
		q = q.Set("? = EXCLUDED.?", bun.Ident(col), bun.Ident(col))
	}
	if r.m.timestamps && r.m.hasUpdatedAt {
		q = q.Set("? = ?", bun.Ident("updated_at"), r.updatedAtStampValue(entity))
	}
	// Resurrection (see the doc comment above): deleted_at is excluded
	// from r.m.updatable like the PK and the other managed timestamps, so
	// without an explicit clear here the DO UPDATE branch would leave a
	// tombstoned row's deleted_at exactly as it was, silently defeating
	// the documented behavior.
	if r.m.nativeSoftDel || r.m.legacyTextSoftDel {
		q = q.Set("? = NULL", bun.Ident("deleted_at"))
	}

	if r.m.pkAutoInc {
		q = q.Returning("?", bun.Ident(r.m.pkColumn))
		pk := r.pkFieldValue(entity)
		if _, err := q.Exec(ctx, pk.Addr().Interface()); err != nil {
			recordErr(span, err)
			return fmt.Errorf("upsert %s: %w", r.m.table, err)
		}
		return nil
	}
	if _, err := q.Exec(ctx); err != nil {
		recordErr(span, err)
		return fmt.Errorf("upsert %s: %w", r.m.table, err)
	}
	return nil
}

// updatedAtStampValue reads back the value stampCreate just wrote to
// updated_at, so the DO UPDATE branch sets it to the SAME instant as the
// DO INSERT branch instead of a value from a second, later time.Now()
// call. EXCLUDED.updated_at would read equally well, but it is excluded
// from r.m.updatable and therefore not guaranteed to be part of the
// INSERT's column list in every future refactor of that allowlist.
func (r *Repo[M]) updatedAtStampValue(entity *M) any {
	sf := r.m.stampFields["updated_at"]
	return reflect.ValueOf(entity).Elem().Field(sf.index).Interface()
}

// ─── Get ───────────────────────────────────────────────────────────────

// Get retrieves a row by primary key. A missing row satisfies
// errors.Is(err, orm.ErrNoRows). The legacy-TEXT deleted_at filter is
// applied; Bun-native soft delete auto-excludes tombstones from the SELECT.
func (r *Repo[M]) Get(ctx context.Context, db orm.Context, id any) (*M, error) {
	r.ensureMeta(db)
	ctx, span := r.startSpan(ctx, "Get", attribute.String("id", fmt.Sprint(id)))
	defer span.End()

	entity := new(M)
	q := db.Bun().NewSelect().Model(entity).
		Where("? = ?", bun.Ident(r.m.pkColumn), id).Limit(1)
	r.scopeRead(q)
	if err := q.Scan(ctx); err != nil {
		recordErr(span, err)
		return nil, fmt.Errorf("get %s by id: %w", r.m.table, err)
	}
	return entity, nil
}

// ─── List / Count / ListAll ──────────────────────────────────────────────

// defaultListLimit is the safety cap List applies when the caller supplies
// no explicit limit. The RPC List path always passes WithLimit(pageSize+1)
// (pageSize is itself clamped to MaxPageSize), which OVERRIDES this cap —
// Bun's Limit is last-wins and the caller's option runs after it — so the
// RPC path is unchanged. The cap only bounds DIRECT repo callers (a worker, a
// _repo_ext due-scan) that would otherwise issue an unbounded SELECT over a
// table that grows without limit and load every row into memory. A caller
// that genuinely needs more must pass an explicit orm.WithLimit. It is a var,
// not a const, so tests can lower it without seeding a huge table.
var defaultListLimit = 10000

// List retrieves rows with optional QueryOption filtering/ordering/limit.
// The legacy-TEXT soft-delete filter is applied; Bun's ,soft_delete excludes
// tombstones natively.
//
// Defense in depth: a default limit (defaultListLimit) is applied BEFORE the
// caller's options so any explicit orm.WithLimit overrides it; a caller that
// passes no limit is still bounded rather than scanning an unbounded table.
func (r *Repo[M]) List(ctx context.Context, db orm.Context, opts ...orm.QueryOption) ([]*M, error) {
	r.ensureMeta(db)
	ctx, span := r.startSpan(ctx, "List")
	defer span.End()

	var results []*M
	q := db.Bun().NewSelect().Model(&results)
	r.scopeRead(q)
	// Applied first so a caller-supplied WithLimit (last-wins in Bun) takes
	// precedence — the RPC path always supplies one, so its behavior is
	// unchanged; only limit-less direct callers get the safety cap.
	q.Limit(defaultListLimit)
	for _, opt := range opts {
		opt(q)
	}
	if err := q.Scan(ctx); err != nil {
		recordErr(span, err)
		return nil, fmt.Errorf("list %s: %w", r.m.table, err)
	}
	return results, nil
}

// Count returns the number of matching rows under the same scope as List.
func (r *Repo[M]) Count(ctx context.Context, db orm.Context, opts ...orm.QueryOption) (int64, error) {
	r.ensureMeta(db)
	ctx, span := r.startSpan(ctx, "Count")
	defer span.End()

	q := db.Bun().NewSelect().Model((*M)(nil))
	r.scopeRead(q)
	for _, opt := range opts {
		opt(q)
	}
	n, err := q.Count(ctx)
	if err != nil {
		recordErr(span, err)
		return 0, fmt.Errorf("count %s: %w", r.m.table, err)
	}
	return int64(n), nil
}

// ListAll retrieves rows INCLUDING soft-deleted ones. Bun-native soft delete
// needs an explicit WhereAllWithDeleted to see tombstones; the legacy-TEXT
// path simply omits the deleted_at filter.
func (r *Repo[M]) ListAll(ctx context.Context, db orm.Context, opts ...orm.QueryOption) ([]*M, error) {
	r.ensureMeta(db)
	ctx, span := r.startSpan(ctx, "ListAll")
	defer span.End()

	var results []*M
	q := db.Bun().NewSelect().Model(&results)
	if r.m.nativeSoftDel {
		q = q.WhereAllWithDeleted()
	}
	for _, opt := range opts {
		opt(q)
	}
	if err := q.Scan(ctx); err != nil {
		recordErr(span, err)
		return nil, fmt.Errorf("list all %s: %w", r.m.table, err)
	}
	return results, nil
}

// scopeRead applies the soft-delete read filter to a SELECT. Bun owns the
// native-soft-delete exclusion automatically; only the legacy-TEXT path needs
// the explicit deleted_at IS NULL filter here.
func (r *Repo[M]) scopeRead(q *bun.SelectQuery) {
	if r.m.legacyTextSoftDel {
		q.Where("? IS NULL", bun.Ident("deleted_at"))
	}
}

// ─── Update / UpdateMasked ────────────────────────────────────────────────

// Update writes the full updatable column set of an existing row by PK.
// updated_at is re-stamped under managed timestamps; created_at, the PK,
// deleted_at, and any column tagged ,skipupdate — the projection of a
// `forge:immutable` column declaration — are excluded from r.m.updatable and
// so never named in the SET clause. That is what keeps a maskless Update
// built from a round-tripped entity (one whose stripped or server-owned
// columns came back zero-valued) from writing those zeros over stored data;
// a new value is set via Create or a masked Update naming the column. The
// deleted_at IS NULL guard applies to BOTH soft-delete modes: Bun
// auto-scopes SELECT/DELETE to live rows but NOT UPDATE, so without the
// guard an UPDATE could mutate a tombstoned row.
//
// An entity that declared a `forge:version` column additionally gets
// optimistic concurrency control: see applyVersionGuard.
func (r *Repo[M]) Update(ctx context.Context, db orm.Context, entity *M) error {
	r.ensureMeta(db)
	if len(r.m.updatable) == 0 {
		return nil // no updatable fields
	}
	ctx, span := r.startSpan(ctx, "Update")
	defer span.End()

	if r.m.timestamps {
		r.stampUpdated(entity)
	}
	r.normalizeArrays(entity)

	cols := r.m.updatable
	if r.m.timestamps && r.m.hasUpdatedAt {
		cols = appendCol(cols, "updated_at")
	}
	q := db.Bun().NewUpdate().Model(entity).
		Column(cols...).
		Where("? = ?", bun.Ident(r.m.pkColumn), r.pkFieldValue(entity).Interface())
	r.scopeWrite(q)
	r.applyVersionGuard(q, entity)
	res, err := q.Exec(ctx)
	if err != nil {
		recordErr(span, err)
		return fmt.Errorf("update %s: %w", r.m.table, err)
	}
	if err := r.requireWriteLanded(ctx, db, res, entity); err != nil {
		recordErr(span, err)
		return err
	}
	r.advanceVersion(entity)
	return nil
}

// UpdateMasked writes ONLY the named columns (AIP-134 update_mask paths;
// proto field names == column names). Paths outside the updatable allowlist
// return *orm.UnknownFieldError, which pkg/crud maps to a clean
// InvalidArgument. updated_at is stamped on masked writes too.
//
// A ,skipupdate column named by the mask is a deliberate assertion (rotating
// a secret, reassigning an owner) and must be written — the opposite of
// Update's full-replace rule. Bun's own SET-clause builder applies
// ,skipupdate unconditionally regardless of which path asked for the
// column, so such columns are pulled out of Column(...) and set explicitly
// via SetColumn(...), which Bun does not filter.
//
// The `forge:version` column is the exception to that exception: it is
// excluded from updatableSet entirely, so a mask naming it is an
// UnknownFieldError rather than a deliberate assertion. A masked write is
// still version-CHECKED — writing one field of a row someone else has since
// rewritten is the same lost update as replacing the whole row — so the
// same predicate and increment apply here. See applyVersionGuard.
func (r *Repo[M]) UpdateMasked(ctx context.Context, db orm.Context, entity *M, fields []string) error {
	r.ensureMeta(db)
	if len(fields) == 0 {
		return nil
	}
	if len(r.m.updatableSet) == 0 {
		// A concrete path can only ever be unknown.
		return &orm.UnknownFieldError{Field: fields[0]}
	}
	ctx, span := r.startSpan(ctx, "UpdateMasked")
	defer span.End()

	stampUpdated := r.m.timestamps && r.m.hasUpdatedAt
	if stampUpdated {
		r.stampUpdated(entity)
	}

	cols := make([]string, 0, len(fields)+1)
	var forced []string
	seen := make(map[string]bool, len(fields))
	for _, f := range fields {
		if !r.m.updatableSet[f] {
			return &orm.UnknownFieldError{Field: f}
		}
		if seen[f] {
			continue
		}
		seen[f] = true
		if _, skip := r.m.skipUpdate[f]; skip {
			forced = append(forced, f)
			continue
		}
		cols = append(cols, f)
	}
	r.normalizeArraysFor(entity, seen)
	if stampUpdated && !seen["updated_at"] {
		cols = append(cols, "updated_at")
	}

	q := db.Bun().NewUpdate().Model(entity)
	if len(cols) > 0 {
		q = q.Column(cols...)
	}
	v := reflect.ValueOf(entity).Elem()
	for _, f := range forced {
		q = q.SetColumn(f, "?", v.Field(r.m.skipUpdate[f]).Interface())
	}
	q = q.Where("? = ?", bun.Ident(r.m.pkColumn), r.pkFieldValue(entity).Interface())
	r.scopeWrite(q)
	r.applyVersionGuard(q, entity)
	res, err := q.Exec(ctx)
	if err != nil {
		recordErr(span, err)
		return fmt.Errorf("update %s: %w", r.m.table, err)
	}
	if err := r.requireWriteLanded(ctx, db, res, entity); err != nil {
		recordErr(span, err)
		return err
	}
	r.advanceVersion(entity)
	return nil
}

// scopeWrite applies the soft-delete guard to an UPDATE.
func (r *Repo[M]) scopeWrite(q *bun.UpdateQuery) {
	if r.m.nativeSoftDel || r.m.legacyTextSoftDel {
		q.Where("? IS NULL", bun.Ident("deleted_at"))
	}
}

// ─── optimistic concurrency control ───────────────────────────────────────
//
// Opt-in, per entity, by declaring one column `forge:version` in a
// migration. An entity without one never reaches any of the code below:
// versionColumn is "", every function here returns immediately, and the
// write path is byte-for-byte the last-writer-wins behaviour it always was.

// applyVersionGuard turns an UPDATE into a compare-and-swap: the row is
// matched only while its stored version still equals the one the caller
// read, and the same statement increments it.
//
// Both halves must be in the ONE statement. Reading the version, comparing
// it in Go and then writing is the lost update this exists to prevent,
// merely with a smaller window — the compare and the write have to be a
// single atomic act, and `WHERE version = $n` + `SET version = version + 1`
// is that act, enforced by the row lock the UPDATE already takes. It needs
// no explicit transaction and no isolation level above postgres's default
// READ COMMITTED: a concurrent writer either committed before this
// statement acquired the row (the predicate then fails and matches nothing)
// or after (it finds the incremented value and fails in turn).
//
// The increment is expressed as `version = version + 1` — the DATABASE's
// value plus one, not the in-memory value plus one. They are equal whenever
// the predicate matches, but writing it as an expression keeps the
// statement true by construction rather than true by an assumption about
// what the caller's struct holds.
func (r *Repo[M]) applyVersionGuard(q *bun.UpdateQuery, entity *M) {
	if r.m.versionColumn == "" {
		return
	}
	col := bun.Ident(r.m.versionColumn)
	q.Where("? = ?", col, r.versionFieldValue(entity).Interface())
	q.Set("? = ? + 1", col, col)
}

// versionFieldValue is the version column's struct field on entity.
func (r *Repo[M]) versionFieldValue(entity *M) reflect.Value {
	return reflect.ValueOf(entity).Elem().Field(r.m.versionFieldIndex)
}

// advanceVersion mirrors the increment the database just performed onto the
// caller's in-memory entity, so a caller holding it can issue a SECOND
// update without re-reading the row.
//
// Without this, the write succeeds, the stored version moves to n+1, the
// struct still says n, and the caller's next Update fails Aborted against a
// row nobody else touched — a conflict invented by this package. Only ever
// called after a write that DID land, so it can never advance past a
// version the database rejected.
func (r *Repo[M]) advanceVersion(entity *M) {
	if r.m.versionColumn == "" {
		return
	}
	f := r.versionFieldValue(entity)
	if !f.CanSet() {
		return
	}
	switch f.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		f.SetInt(f.Int() + 1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		f.SetUint(f.Uint() + 1)
	}
	// Any other Go type is left alone rather than guessed at. The column is
	// meant to be an integer counter; a non-integer one still gets the
	// database-side `+ 1` (postgres decides whether that is legal for the
	// type) and simply forces the caller to re-read before writing again.
}

// requireWriteLanded is requireRowTouched plus the disambiguation optimistic
// concurrency control forces on it.
//
// Zero rows affected used to have exactly one meaning — no such row — and
// requireRowTouched turned it into orm.ErrNoRows → CodeNotFound. With a
// version predicate in the WHERE clause it has two, and they are not
// remotely the same news for a client:
//
//   - the row is gone (or was never there, or is soft-deleted): NotFound.
//     Retrying is pointless; the resource does not exist.
//   - the row is there but its version moved: somebody else committed a
//     write between this caller's read and its write. Aborted. Re-read and
//     retry is not just viable, it is the prescribed recovery — which is
//     precisely what connect.CodeAborted means, and why it is not
//     FailedPrecondition (a state the caller must fix) or AlreadyExists.
//
// Reporting a conflict as NotFound would tell a client its row had been
// deleted when it had merely been edited, and a UI acting on that ("this
// record no longer exists") would be lying about data still sitting in the
// table. So on zero rows the repo asks ONE narrow follow-up question — does
// a row with this PK exist at all, ignoring the version — and answers from
// the result.
//
// That re-query is racy in the strict sense: the row could be deleted
// between the failed UPDATE and this SELECT, turning a genuine conflict
// into a NotFound. That is acceptable and the ordering makes it honest —
// the row IS gone by the time the caller is told so, and re-reading (the
// prescribed response to Aborted) would have discovered exactly that. The
// converse mistake, calling a deletion a conflict, is equally bounded and
// equally self-correcting on the caller's re-read.
//
// It costs one extra round trip ONLY on the failure path of a
// version-checked entity: the happy path never reaches it, and an entity
// with no version column returns from the first branch without ever
// building the query.
func (r *Repo[M]) requireWriteLanded(ctx context.Context, db orm.Context, res sql.Result, entity *M) error {
	if err := requireRowTouched(res, "update", r.m.table); err == nil {
		return nil
	}
	if r.m.versionColumn == "" {
		return fmt.Errorf("update %s: %w", r.m.table, orm.ErrNoRows)
	}

	// The version predicate is deliberately absent here: this asks only
	// whether the ROW exists, which is the single fact that separates the
	// two answers. The soft-delete scoping stays, so a tombstoned row reads
	// as absent exactly as it does to Get — the two verbs must keep
	// agreeing about what "this row does not exist" means.
	q := db.Bun().NewSelect().Model((*M)(nil)).
		Where("? = ?", bun.Ident(r.m.pkColumn), r.pkFieldValue(entity).Interface())
	r.scopeRead(q)
	exists, err := q.Exists(ctx)
	if err != nil {
		// The follow-up query itself failed, so which of the two answers
		// applies is unknown. Report the query's error rather than pick one:
		// a guess here would be indistinguishable from a real verdict.
		return fmt.Errorf("update %s: classify zero-row write: %w", r.m.table, err)
	}
	if !exists {
		return fmt.Errorf("update %s: %w", r.m.table, orm.ErrNoRows)
	}
	return svcerr.Aborted(fmt.Sprintf(
		"%s was modified by another writer; re-read it and retry", r.m.entityName))
}

func appendCol(cols []string, c string) []string {
	for _, e := range cols {
		if e == c {
			return cols
		}
	}
	// Copy to avoid mutating the shared r.m.updatable backing array.
	out := make([]string, len(cols), len(cols)+1)
	copy(out, cols)
	return append(out, c)
}

// ─── Delete ───────────────────────────────────────────────────────────────

// Delete removes a row by PK. With Bun-native soft delete a plain NewDelete
// stamps deleted_at (Bun rewrites it to UPDATE) and auto-scopes to live
// rows. With legacy-TEXT soft delete the repo hand-rolls the
// CURRENT_TIMESTAMP stamp + deleted_at IS NULL guard (Bun's time.Time stamp
// can't round-trip a TEXT column). Otherwise it is a hard DELETE.
//
// A PK that matched no live row returns orm.ErrNoRows, which pkg/crud maps
// to CodeNotFound — the same answer Get gives for the same id. See
// requireRowTouched for why that, and not a silent success.
func (r *Repo[M]) Delete(ctx context.Context, db orm.Context, id any) error {
	r.ensureMeta(db)
	ctx, span := r.startSpan(ctx, "Delete", attribute.String("id", fmt.Sprint(id)))
	defer span.End()

	if r.m.legacyTextSoftDel {
		q := db.Bun().NewUpdate().Model((*M)(nil)).
			Set("? = CURRENT_TIMESTAMP", bun.Ident("deleted_at")).
			Where("? = ?", bun.Ident(r.m.pkColumn), id).
			Where("? IS NULL", bun.Ident("deleted_at"))
		res, err := q.Exec(ctx)
		if err != nil {
			recordErr(span, err)
			return fmt.Errorf("delete %s: %w", r.m.table, err)
		}
		if err := requireRowTouched(res, "delete", r.m.table); err != nil {
			recordErr(span, err)
			return err
		}
		return nil
	}

	// Native soft delete (Bun rewrites NewDelete → UPDATE deleted_at and
	// scopes to live rows) AND hard delete share this path.
	q := db.Bun().NewDelete().Model((*M)(nil)).
		Where("? = ?", bun.Ident(r.m.pkColumn), id)
	res, err := q.Exec(ctx)
	if err != nil {
		recordErr(span, err)
		return fmt.Errorf("delete %s: %w", r.m.table, err)
	}
	if err := requireRowTouched(res, "delete", r.m.table); err != nil {
		recordErr(span, err)
		return err
	}
	return nil
}

// requireRowTouched turns a write that matched no row into orm.ErrNoRows.
//
// UPDATE and DELETE report a row count, not sql.ErrNoRows, so a repository
// that discards the count cannot tell "row deleted" from "no such row" —
// and every generated Delete answered a typo'd id with 200 and an empty
// body while Get answered the same id with 404. Two verbs resolving the
// same row, by the same PK, under the same soft-delete scoping and the
// same application-owned filters, must agree about what "this row does not
// exist" means.
//
// NotFound is the answer, not idempotent-200:
//
//   - AIP-135, the resource-Delete design this CRUD shape follows, returns
//     NOT_FOUND unless the request explicitly opts into allow_missing.
//   - HTTP idempotency (RFC 9110 §9.2.2) is a property of the EFFECT on
//     server state, not of the status code. A repeated DELETE that answers
//     404 is still idempotent; nothing about returning 200 is required.
//   - The 200 was not merely permissive, it was unfalsifiable: a caller
//     could not distinguish a delete that worked from a wrong id, a row an
//     application-owned filter excluded, or a row already tombstoned.
//
// A project that genuinely wants absent-is-success owns the shim: swallow
// orm.ErrNoRows in that entity's Persist closure in handlers_crud.go.
//
// A driver that cannot report the count is treated as "touched" —
// inventing a not-found out of a missing count would fail a write that
// actually succeeded.
func requireRowTouched(res sql.Result, op, table string) error {
	n, err := res.RowsAffected()
	if err != nil || n > 0 {
		return nil
	}
	return fmt.Errorf("%s %s: %w", op, table, orm.ErrNoRows)
}

// ─── managed-timestamp stamping ──────────────────────────────────────────

// stampCreate sets created_at (when the caller left it unset) and updated_at
// to now. Fields whose type the repo can't stamp are absent from
// stampFields and left untouched.
func (r *Repo[M]) stampCreate(entity *M) {
	now := time.Now().UTC()
	v := reflect.ValueOf(entity).Elem()
	if sf, ok := r.m.stampFields["created_at"]; ok && stampIsEmpty(v.Field(sf.index), sf) {
		writeStamp(v.Field(sf.index), sf, now)
	}
	if sf, ok := r.m.stampFields["updated_at"]; ok {
		writeStamp(v.Field(sf.index), sf, now)
	}
}

// stampUpdated re-stamps updated_at to now.
func (r *Repo[M]) stampUpdated(entity *M) {
	if sf, ok := r.m.stampFields["updated_at"]; ok {
		writeStamp(reflect.ValueOf(entity).Elem().Field(sf.index), sf, time.Now().UTC())
	}
}

// stampIsEmpty reports whether a created_at field is unset (so a
// caller-provided value wins): nil pointer, empty string, or zero time.
func stampIsEmpty(f reflect.Value, sf stampField) bool {
	if sf.isPtr {
		return f.IsNil()
	}
	if sf.isString {
		return f.String() == ""
	}
	t, ok := f.Interface().(time.Time)
	return ok && t.IsZero()
}

// writeStamp sets f to now in its projected type (string → RFC3339Nano,
// time.Time → the instant), allocating through an addressable local for
// pointer fields.
func writeStamp(f reflect.Value, sf stampField, now time.Time) {
	if !f.CanSet() {
		return
	}
	if sf.isString {
		s := now.Format(time.RFC3339Nano)
		if sf.isPtr {
			p := reflect.New(f.Type().Elem())
			p.Elem().SetString(s)
			f.Set(p)
			return
		}
		f.SetString(s)
		return
	}
	if sf.isPtr {
		p := reflect.New(f.Type().Elem())
		p.Elem().Set(reflect.ValueOf(now))
		f.Set(p)
		return
	}
	f.Set(reflect.ValueOf(now))
}
