package scaffold

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
)

func uniqueJobSpec(keys ...[]string) EntityFromProtoSpec {
	spec := EntityFromProtoSpec{
		Table:     "jobs",
		MessageFQ: entityProtoPkg + ".Job",
		ProtoPkg:  entityProtoPkg,
		Fields: []codegen.SchemaFieldDef{
			{Name: "id", Kind: "string"},
			{Name: "namespace", Kind: "string"},
			{Name: "name", Kind: "string"},
			{Name: "labels", Kind: "map", MapKeyKind: "string", MapValueKind: "message", MapValueTypeName: entityProtoPkg + ".Label"},
		},
		Timestamps: true,
	}
	for _, k := range keys {
		spec.Unique = append(spec.Unique, UniqueKey{Columns: k})
	}
	return spec
}

// `// forge:unique namespace,name` births the composite key into the
// create-table migration — the hand edit ranger's jobs table needed.
func TestRenderEntityMigrationFromProto_UniqueKeys(t *testing.T) {
	mig := RenderEntityMigrationFromProto(uniqueJobSpec([]string{"namespace", "name"}, []string{"name"}))
	for _, want := range []string{
		"CREATE UNIQUE INDEX jobs_namespace_name_key ON jobs (namespace, name);\n",
		"CREATE UNIQUE INDEX jobs_name_key ON jobs (name);\n",
	} {
		if !strings.Contains(mig.UpSQL, want) {
			t.Errorf("migration lacks %q:\n%s", want, mig.UpSQL)
		}
	}
	if strings.Index(mig.UpSQL, "jobs_namespace_name_key") > strings.Index(mig.UpSQL, "jobs_name_key") {
		t.Errorf("keys must be emitted in declaration order:\n%s", mig.UpSQL)
	}
	if strings.Index(mig.UpSQL, "CREATE UNIQUE INDEX") < strings.Index(mig.UpSQL, ");\n") {
		t.Errorf("a key must follow the CREATE TABLE it indexes:\n%s", mig.UpSQL)
	}
	// Migration-text lint lanes read every forge:* token in the SQL; the
	// key's comment must not hand them one to misread.
	if strings.Contains(mig.UpSQL, codegen.ProtoMarkerUnique) {
		t.Errorf("the migration must not spell the proto marker:\n%s", mig.UpSQL)
	}
	if len(mig.Notes) != 0 && strings.Contains(strings.Join(mig.Notes, "\n"), "unique key") {
		t.Errorf("valid keys produce no notes, got %q", mig.Notes)
	}
}

// On a soft-delete table the key covers live rows only, or a deleted row's
// key could never be created again.
func TestRenderEntityMigrationFromProto_UniqueKeySoftDeleteIsPartial(t *testing.T) {
	spec := uniqueJobSpec([]string{"namespace", "name"})
	spec.SoftDelete = true
	mig := RenderEntityMigrationFromProto(spec)
	want := "CREATE UNIQUE INDEX jobs_namespace_name_key ON jobs (namespace, name) WHERE deleted_at IS NULL;\n"
	if !strings.Contains(mig.UpSQL, want) {
		t.Errorf("migration lacks %q:\n%s", want, mig.UpSQL)
	}
}

// Columns a key may name are exactly the columns the table is born with.
func TestUniqueKeyRefusal(t *testing.T) {
	cases := []struct {
		cols []string
		want string // substring of the refusal; "" = accepted
	}{
		{[]string{"namespace", "name"}, ""},
		{[]string{"name", "created_at"}, ""},
		{nil, "names no columns"},
		{[]string{"name", "name"}, "names column name twice"},
		{[]string{"id", "name"}, "includes id, the primary key"},
		{[]string{"namespce", "name"}, "did you mean namespace?"},
		{[]string{"tenant"}, "not a column of jobs (columns: id, namespace, name, created_at, updated_at)"},
		{[]string{"labels"}, "a field birth gives no column"},
		// No deleted_at without soft delete.
		{[]string{"deleted_at"}, "not a column of jobs"},
	}
	for _, c := range cases {
		got := UniqueKeyRefusal(UniqueKey{Columns: c.cols}, uniqueJobSpec())
		switch {
		case c.want == "" && got != "":
			t.Errorf("%q: refused (%s), want accepted", c.cols, got)
		case c.want != "" && !strings.Contains(got, c.want):
			t.Errorf("%q: refusal = %q, want it to contain %q", c.cols, got, c.want)
		}
	}
}

// The renderer never emits a key postgres would reject: a refused key is a
// TODO comment and a note, should a caller skip the CLI pre-flight.
func TestRenderEntityMigrationFromProto_RefusedUniqueKeyIsATodo(t *testing.T) {
	mig := RenderEntityMigrationFromProto(uniqueJobSpec([]string{"namespace", "nmae"}))
	if strings.Contains(mig.UpSQL, "CREATE UNIQUE INDEX") {
		t.Errorf("a refused key must not be emitted:\n%s", mig.UpSQL)
	}
	if !strings.Contains(mig.UpSQL, "-- TODO: unique key (namespace, nmae) skipped — it names nmae") {
		t.Errorf("a refused key must be carried as a TODO:\n%s", mig.UpSQL)
	}
	if !strings.Contains(strings.Join(mig.Notes, "\n"), "unique key (namespace, nmae)") {
		t.Errorf("a refused key must be reported in the notes, got %q", mig.Notes)
	}
}
