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
)

// repoTracer is the span source for the generic repository. The pre-generic
// per-entity ORM emitted one tracer per package ("orm"); the library keeps
// the same tracer name so existing dashboards/queries that filter on the
// "orm" tracer and the "orm.<Op><Entity>" span names keep working.
var repoTracer = otel.Tracer("orm")

// Spec is the IRREDUCIBLE per-entity descriptor the generator emits for a
// Repo. Everything Bun's own table schema can answer — table name, primary
// key column + Go field, autoincrement/server-allocated PK, the native
// (,soft_delete) deleted-at field, the full column set, array columns — is
// derived by reflection from the Bun-tagged model at first use (once per
// entity, never in the hot path). Spec carries ONLY the forge conventions
// Bun cannot infer from struct tags:
//
//   - Timestamps          — forge's managed created_at/updated_at stamping
//     (set on Create, re-stamped on every Update/UpdateMasked). Distinct
//     from a DB DEFAULT: forge stamps in Go so the value is identical
//     across the row and visible to the caller without a re-read.
//   - LegacyTextDeletedAt — soft delete whose deleted_at column is a legacy
//     TEXT column (Go type string) that Bun's time.Time-based ,soft_delete
//     cannot round-trip. When true the repo hand-rolls the deleted_at IS
//     NULL filter and the CURRENT_TIMESTAMP stamp (kalshi fr-3fba9166ba).
//     Bun-native soft delete (proper time deleted_at) is detected from the
//     schema and needs no flag.
//   - SecretColumns        — columns carrying the `// forge:secret` marker.
//     The read path (toProto) never packs them, so a client Get/List always
//     reads the secret back as "". A maskless full-replace Update built from
//     that round-tripped entity would therefore overwrite the stored secret
//     (e.g. an encrypted credential) with "" — silent data loss. These
//     columns are consequently PRESERVED on a full-replace Update (excluded
//     from its SET clause, exactly like the PK/created_at columns),
//     yet stay settable on Create and on an EXPLICIT masked Update that names
//     the column (deliberate intent — the client is asserting a new value).
//     Bun cannot infer this from struct tags; the generator threads the
//     column names from the field's `// forge:secret` marker.
type Spec struct {
	Timestamps          bool
	LegacyTextDeletedAt bool
	SecretColumns       []string
}

// meta is the reflection-derived, cached half of a Repo's knowledge. It is
// computed once (sync.Once, off the first IDB the repo sees) from Bun's
// table schema for Model and never recomputed — Bun itself caches the
// *schema.Table per type, so even the first computation is a single schema
// build shared process-wide.
type meta struct {
	table         string   // SQL table name
	entityName    string   // model Go type name (for span names)
	pkColumn      string   // primary-key column name
	pkGoField     string   // primary-key struct field (Go) name
	pkAutoInc     bool     // server-allocated PK (SERIAL/IDENTITY) → RETURNING
	pkIsString    bool     // string PK → ULID-generate when empty on Create
	nativeSoftDel bool     // Bun owns soft delete (,soft_delete time column)
	columns       []string // ordered declared column allowlist
	hasUpdatedAt  bool     // updated_at column exists (managed-stamp target)
	nilSliceCols  []sliceCol
	updatable     []string // columns settable by a full Update (SET clause)
	updatableSet  map[string]bool
	stampFields   map[string]stampField // created_at/updated_at → field info
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
// FromProto pair, and a single crud.NewRepo[Model](Spec{...}) line.
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
	spec Spec
	once sync.Once
	m    meta
}

// NewRepo constructs a Repo for model M. Metadata derivation is deferred to
// the first call (it needs a live bun.IDB to reach the dialect's table
// cache); spec carries the forge conventions Bun's schema can't infer.
func NewRepo[M any](spec Spec) *Repo[M] {
	return &Repo[M]{spec: spec}
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
func (r *Repo[M]) ensureMeta(db orm.Context) {
	r.once.Do(func() {
		typ := r.modelType()
		tbl := db.Bun().Dialect().Tables().Get(typ)

		r.m.table = tbl.Name
		r.m.entityName = typ.Name()
		r.m.columns = make([]string, 0, len(tbl.Fields))
		r.m.stampFields = map[string]stampField{}
		for _, f := range tbl.Fields {
			r.m.columns = append(r.m.columns, f.Name)
			if f.NotNull && f.IndirectType.Kind() == reflect.Slice {
				r.m.nilSliceCols = append(r.m.nilSliceCols, sliceCol{fieldIndex: f.Index[0], column: f.Name})
			}
			if f.Name == "updated_at" {
				r.m.hasUpdatedAt = true
			}
			if r.spec.Timestamps && (f.Name == "created_at" || f.Name == "updated_at") {
				if sf, ok := stampFieldFor(f); ok {
					r.m.stampFields[f.Name] = sf
				}
			}
		}
		if len(tbl.PKs) > 0 {
			pk := tbl.PKs[0]
			r.m.pkColumn = pk.Name
			r.m.pkGoField = pk.GoName
			r.m.pkAutoInc = pk.AutoIncrement || pk.Identity
			r.m.pkIsString = pk.IndirectType.Kind() == reflect.String
		} else {
			r.m.pkColumn = "id"
			r.m.pkGoField = "Id"
			r.m.pkIsString = true
		}
		if tbl.SoftDeleteField != nil {
			r.m.nativeSoftDel = true
		}

		// Secret columns are settable via an EXPLICIT mask but never written
		// by a maskless full-replace (see below).
		secretCols := make(map[string]bool, len(r.spec.SecretColumns))
		for _, c := range r.spec.SecretColumns {
			secretCols[c] = true
		}

		// Two distinct allowlists, both starting from every declared column
		// EXCEPT the PK, deleted_at, and — under managed timestamps —
		// created_at and updated_at (the latter is repo-stamped, never
		// caller-set):
		//
		//   - updatableSet — the MASKED-update allowlist: a path an
		//     update_mask may name. A `:secret` column IS here, because a
		//     mask that names it is the client deliberately asserting a new
		//     value (Create's sibling write path).
		//   - updatable    — the FULL-REPLACE SET list: the columns a maskless
		//     Update writes. A `:secret` column is EXCLUDED, because the client
		//     can never have read the real value (the read path strips it to
		//     ""), so a full replace built from a round-tripped entity would
		//     clobber the stored secret with "". Preserving it here is the
		//     data-loss fix; it mirrors how the PK/created_at columns
		//     are already excluded.
		r.m.updatableSet = make(map[string]bool, len(tbl.Fields))
		for _, f := range tbl.Fields {
			if r.columnExcludedFromSet(f.Name) {
				continue
			}
			r.m.updatableSet[f.Name] = true
			if secretCols[f.Name] {
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

// columnExcludedFromSet mirrors the generator's excludedFromSet: columns
// that never appear in an UPDATE SET clause.
func (r *Repo[M]) columnExcludedFromSet(col string) bool {
	if col == r.m.pkColumn || col == "deleted_at" {
		return true
	}
	if r.spec.Timestamps && (col == "created_at" || col == "updated_at") {
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
	if r.spec.Timestamps {
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
	if r.spec.LegacyTextDeletedAt {
		q.Where("? IS NULL", bun.Ident("deleted_at"))
	}
}

// ─── Update / UpdateMasked ────────────────────────────────────────────────

// Update writes the full updatable column set of an existing row by PK.
// updated_at is re-stamped under managed timestamps; created_at, the PK,
// deleted_at, AND any Spec.SecretColumns are excluded from the
// SET clause. Secret columns are preserved on this full-replace path because
// the client can never have read the real value (the read path strips it), so
// a maskless Update built from a round-tripped entity must not clobber the
// stored secret with "" — a new value is set only via Create or a masked
// Update that names the column. The deleted_at IS NULL guard applies to BOTH
// soft-delete modes: Bun auto-scopes SELECT/DELETE to live rows but NOT
// UPDATE, so without the guard an UPDATE could mutate a tombstoned row.
func (r *Repo[M]) Update(ctx context.Context, db orm.Context, entity *M) error {
	r.ensureMeta(db)
	if len(r.m.updatable) == 0 {
		return nil // no updatable fields
	}
	ctx, span := r.startSpan(ctx, "Update")
	defer span.End()

	if r.spec.Timestamps {
		r.stampUpdated(entity)
	}
	r.normalizeArrays(entity)

	cols := r.m.updatable
	if r.spec.Timestamps && r.m.hasUpdatedAt {
		cols = appendCol(cols, "updated_at")
	}
	q := db.Bun().NewUpdate().Model(entity).
		Column(cols...).
		Where("? = ?", bun.Ident(r.m.pkColumn), r.pkFieldValue(entity).Interface())
	r.scopeWrite(q)
	res, err := q.Exec(ctx)
	if err != nil {
		recordErr(span, err)
		return fmt.Errorf("update %s: %w", r.m.table, err)
	}
	if err := requireRowTouched(res, "update", r.m.table); err != nil {
		recordErr(span, err)
		return err
	}
	return nil
}

// UpdateMasked writes ONLY the named columns (AIP-134 update_mask paths;
// proto field names == column names). Paths outside the updatable allowlist
// return *orm.UnknownFieldError, which pkg/crud maps to a clean
// InvalidArgument. updated_at is stamped on masked writes too.
func (r *Repo[M]) UpdateMasked(ctx context.Context, db orm.Context, entity *M, fields []string) error {
	r.ensureMeta(db)
	if len(fields) == 0 {
		return nil
	}
	if len(r.m.updatable) == 0 {
		// A concrete path can only ever be unknown.
		return &orm.UnknownFieldError{Field: fields[0]}
	}
	ctx, span := r.startSpan(ctx, "UpdateMasked")
	defer span.End()

	stampUpdated := r.spec.Timestamps && r.m.hasUpdatedAt
	if stampUpdated {
		r.stampUpdated(entity)
	}

	cols := make([]string, 0, len(fields)+1)
	seen := make(map[string]bool, len(fields))
	for _, f := range fields {
		if !r.m.updatableSet[f] {
			return &orm.UnknownFieldError{Field: f}
		}
		if seen[f] {
			continue
		}
		seen[f] = true
		cols = append(cols, f)
	}
	r.normalizeArraysFor(entity, seen)
	if stampUpdated && !seen["updated_at"] {
		cols = append(cols, "updated_at")
	}

	q := db.Bun().NewUpdate().Model(entity).
		Column(cols...).
		Where("? = ?", bun.Ident(r.m.pkColumn), r.pkFieldValue(entity).Interface())
	r.scopeWrite(q)
	res, err := q.Exec(ctx)
	if err != nil {
		recordErr(span, err)
		return fmt.Errorf("update %s: %w", r.m.table, err)
	}
	if err := requireRowTouched(res, "update", r.m.table); err != nil {
		recordErr(span, err)
		return err
	}
	return nil
}

// scopeWrite applies the soft-delete guard to an UPDATE.
func (r *Repo[M]) scopeWrite(q *bun.UpdateQuery) {
	if r.m.nativeSoftDel || r.spec.LegacyTextDeletedAt {
		q.Where("? IS NULL", bun.Ident("deleted_at"))
	}
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

	if r.spec.LegacyTextDeletedAt {
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
