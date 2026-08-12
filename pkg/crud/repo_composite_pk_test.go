package crud

import (
	"context"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"

	"github.com/reliant-labs/forge/pkg/orm"
)

// documentCounter mirrors the motivating shape from the bug report: a
// composite PRIMARY KEY (company_id, kind) where company_id is ALSO a
// foreign key. Both key columns are strings, so pre-fix code (which read
// tbl.PKs[0] and set pkIsString unconditionally) misclassified company_id
// as a ULID-generatable string PK.
type documentCounter struct {
	bun.BaseModel `bun:"table:document_counters,alias:document_counters"`

	CompanyID string `bun:"company_id,pk"`
	Kind      string `bun:"kind,pk"`
	NextValue int64  `bun:"next_value,notnull"`
}

func createDocumentCountersTable(t *testing.T, db orm.Context) {
	t.Helper()
	_, err := db.Bun().NewCreateTable().Model((*documentCounter)(nil)).
		IfNotExists().Exec(context.Background())
	if err != nil {
		t.Fatalf("create document_counters table: %v", err)
	}
}

// TestEnsureMeta_CompositePK_DisablesULIDAndCursor is the hermetic unit for
// the fix: a composite-PK model must NOT classify off tbl.PKs[0] the way a
// single-PK model does. No live database is needed — ensureMeta's
// classification never touches one.
func TestEnsureMeta_CompositePK_DisablesULIDAndCursor(t *testing.T) {
	db := dialectOnlyDB{bdb: bun.NewDB(nil, pgdialect.New())}
	repo := NewRepo[documentCounter]()
	repo.ensureMeta(db)

	if repo.m.pkIsString {
		t.Error("composite PK must not set pkIsString — it would ULID-generate over one key column (possibly a foreign key) on Create")
	}
	if repo.m.pkColumn != "" {
		t.Errorf("composite PK must leave pkColumn empty (the existing PK-cursor-pagination-disabled escape hatch), got %q", repo.m.pkColumn)
	}
	if repo.m.pkAutoInc {
		t.Error("composite PK must not set pkAutoInc")
	}
}

// TestCreate_CompositePK_DoesNotULIDOverwriteFirstKeyColumn is the runtime
// proof against real embedded postgres: Create on a composite-PK entity
// must leave BOTH caller-supplied key columns untouched. Pre-fix, forge
// read tbl.PKs[0] (company_id), saw a string type, and ULID-generated over
// it whenever it looked empty on Create — and even when non-empty, the
// entity was reporting pkIsString=true for a column that, in a real
// schema, is also a foreign key: exactly the corruption this guards
// against. This test pins the caller-supplied value surviving Create
// unmodified.
func TestCreate_CompositePK_DoesNotULIDOverwriteFirstKeyColumn(t *testing.T) {
	db := newRepoTestDB(t)
	ctx := context.Background()
	createDocumentCountersTable(t, db)

	repo := NewRepo[documentCounter]()

	row := &documentCounter{CompanyID: "company-42", Kind: "invoice", NextValue: 1}
	if err := repo.Create(ctx, db, row); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if row.CompanyID != "company-42" {
		t.Errorf("Create overwrote the composite key's first column: got %q, want %q (a real schema would have this column ALSO be a foreign key, so a ULID here silently corrupts the reference)", row.CompanyID, "company-42")
	}
	if row.Kind != "invoice" {
		t.Errorf("Create overwrote the composite key's second column: got %q, want %q", row.Kind, "invoice")
	}
}

// normalPKRow is the contrasting case: a single-string-PK entity, which
// must KEEP generating a ULID on Create when the caller leaves the PK
// empty. This is the behavior the composite-PK fix must not regress.
type normalPKRow struct {
	bun.BaseModel `bun:"table:normal_pk_rows,alias:normal_pk_rows"`

	ID   string `bun:"id,pk"`
	Name string `bun:"name,notnull"`
}

func createNormalPKRowsTable(t *testing.T, db orm.Context) {
	t.Helper()
	_, err := db.Bun().NewCreateTable().Model((*normalPKRow)(nil)).
		IfNotExists().Exec(context.Background())
	if err != nil {
		t.Fatalf("create normal_pk_rows table: %v", err)
	}
}

// TestCreate_SingleStringPK_StillGeneratesULID is the contrast case: an
// ordinary single-column string PK must still get a ULID minted on Create
// when left empty — the composite-PK guard must be surgical, not a
// blanket regression of ULID generation.
func TestCreate_SingleStringPK_StillGeneratesULID(t *testing.T) {
	db := newRepoTestDB(t)
	ctx := context.Background()
	createNormalPKRowsTable(t, db)

	repo := NewRepo[normalPKRow]()

	row := &normalPKRow{Name: "alpha"} // ID left empty on purpose
	if err := repo.Create(ctx, db, row); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if row.ID == "" {
		t.Error("Create must ULID-generate a single-column string PK left empty by the caller")
	}
}
