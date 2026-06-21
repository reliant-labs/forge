package add

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/schemadef"
)

const addEntityTestProto = `syntax = "proto3";

package services.item.v1;

import "forge/v1/forge.proto";

option go_package = "example.com/x/gen/services/item/v1;itemv1";

service ItemService {
  option (forge.v1.service) = {
    name: "ItemService"
  };

  rpc GetItem(GetItemRequest) returns (GetItemResponse) {}
}

message GetItemRequest {
  string id = 1;
}

message GetItemResponse {
  string id = 1;
}
`

func scaffoldAddEntityProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "forge.yaml"), []byte("name: x\nmodule: example.com/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	protoDir := filepath.Join(root, "proto", "services", "item", "v1")
	if err := os.MkdirAll(protoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(protoDir, "item.proto"), []byte(addEntityTestProto), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestWriteEntityMigration_EmitsPortableSQL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db", "migrations")
	fields, err := parseEntityFieldArgs([]string{"url:string", "title:string", "tags:[]string", "done:bool"})
	if err != nil {
		t.Fatal(err)
	}
	up, down, err := writeEntityMigration(dir, "bookmarks", fields, addEntityOpts{SoftDelete: true})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(up) != "00001_create_bookmarks.up.sql" {
		t.Errorf("up file = %s", up)
	}
	sql := readFileT(t, up)
	for _, want := range []string{
		"CREATE TABLE bookmarks",
		"id TEXT PRIMARY KEY CHECK (id <> '')",
		"url TEXT NOT NULL DEFAULT ''",
		"tags TEXT[] NOT NULL DEFAULT '{}'",
		"done BOOLEAN NOT NULL DEFAULT FALSE",
		"created_at TIMESTAMPTZ NOT NULL DEFAULT (now())",
		"updated_at TIMESTAMPTZ NOT NULL DEFAULT (now())",
		"deleted_at TIMESTAMPTZ",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("migration missing %q:\n%s", want, sql)
		}
	}
	if got := readFileT(t, down); !strings.Contains(got, "DROP TABLE bookmarks;") {
		t.Errorf("down migration = %q", got)
	}

	// The emitted SQL must survive the shadow schema round trip — this
	// is the whole contract: add entity emits, generate introspects.
	tables, err := schemadef.ApplyAndIntrospect(dir)
	if err != nil {
		t.Fatalf("emitted migration failed shadow apply: %v", err)
	}
	if len(tables) != 1 || tables[0].Name != "bookmarks" {
		t.Fatalf("tables = %+v", tables)
	}
	conv := schemadef.DetectConventions(tables[0])
	if !conv.SoftDelete || !conv.Timestamps {
		t.Errorf("conventions from emitted SQL = %+v, want soft-delete + timestamps", conv)
	}
}

func TestWriteEntityMigration_SequencesAfterExisting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db", "migrations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "00007_other.up.sql"), []byte("CREATE TABLE x (id TEXT PRIMARY KEY);"), 0o644); err != nil {
		t.Fatal(err)
	}
	up, _, err := writeEntityMigration(dir, "things", nil, addEntityOpts{NoTimestamps: true})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(up) != "00008_create_things.up.sql" {
		t.Errorf("expected sequence 00008, got %s", up)
	}
}

func TestInjectEntityCRUDProto(t *testing.T) {
	root := scaffoldAddEntityProject(t)
	protoPath := filepath.Join(root, "proto", "services", "item", "v1", "item.proto")
	fields, err := parseEntityFieldArgs([]string{"url:string", "tags:[]string", "done:bool"})
	if err != nil {
		t.Fatal(err)
	}
	if err := injectEntityCRUDProto(protoPath, "Bookmark", fields, addEntityOpts{}); err != nil {
		t.Fatal(err)
	}
	got := readFileT(t, protoPath)

	for _, want := range []string{
		`import "google/protobuf/timestamp.proto";`,
		`import "google/protobuf/field_mask.proto";`,
		"rpc CreateBookmark(CreateBookmarkRequest) returns (CreateBookmarkResponse)",
		"rpc ListBookmarks(ListBookmarksRequest) returns (ListBookmarksResponse)",
		"message Bookmark {",
		"repeated string tags = 3;",
		"google.protobuf.Timestamp created_at = 5;",
		"message UpdateBookmarkRequest {",
		"google.protobuf.FieldMask update_mask = 2;",
		"optional string search = 3;",
		"optional bool done = 4;",
		"repeated Bookmark bookmarks = 1;",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("proto missing %q", want)
		}
	}

	// RPCs must land INSIDE the service block.
	svcEnd := strings.Index(got, "\n}")
	if createIdx := strings.Index(got, "rpc CreateBookmark"); createIdx > svcEnd {
		t.Errorf("CreateBookmark rpc landed outside the service block")
	}

	// Idempotent: a second run leaves the wire contract alone.
	if err := injectEntityCRUDProto(protoPath, "Bookmark", fields, addEntityOpts{}); err != nil {
		t.Fatal(err)
	}
	if again := readFileT(t, protoPath); strings.Count(again, "message Bookmark {") != 1 {
		t.Error("second injection duplicated the entity message")
	}
}

// addEntityBareServiceProto is the shape `forge add service` scaffolds:
// no RPCs yet, and therefore NO forge/v1/forge.proto import. The entity
// injection adds RPCs carrying (forge.v1.method) options, so it MUST
// also add the import or `buf` fails every subsequent generate with
// "unknown extension forge.v1.method" (journey fr-af7355dd63).
const addEntityBareServiceProto = `syntax = "proto3";

package services.item.v1;

option go_package = "example.com/x/gen/services/item/v1;itemv1";

// ItemService defines the item service RPCs.
service ItemService {
  // TODO: Add your RPC methods here.
}
`

func TestInjectEntityCRUDProto_AddsForgeImportToBareServiceProto(t *testing.T) {
	root := scaffoldAddEntityProject(t)
	protoPath := filepath.Join(root, "proto", "services", "item", "v1", "item.proto")
	if err := os.WriteFile(protoPath, []byte(addEntityBareServiceProto), 0o644); err != nil {
		t.Fatal(err)
	}
	fields, err := parseEntityFieldArgs([]string{"url:string"})
	if err != nil {
		t.Fatal(err)
	}
	if err := injectEntityCRUDProto(protoPath, "Bookmark", fields, addEntityOpts{}); err != nil {
		t.Fatal(err)
	}
	got := readFileT(t, protoPath)

	if !strings.Contains(got, `import "forge/v1/forge.proto";`) {
		t.Fatalf("injected (forge.v1.method) options without importing forge/v1/forge.proto — buf fails with 'unknown extension forge.v1.method':\n%s", got)
	}
	// The import must land between the package line and the service block
	// (i.e. in the file header, not appended after messages).
	impIdx := strings.Index(got, `import "forge/v1/forge.proto";`)
	pkgIdx := strings.Index(got, "package services.item.v1;")
	svcIdx := strings.Index(got, "service ItemService {")
	if impIdx < pkgIdx || impIdx > svcIdx {
		t.Errorf("forge import landed outside the file header (pkg=%d import=%d service=%d):\n%s", pkgIdx, impIdx, svcIdx, got)
	}

	// Second injection of another entity must not duplicate the import.
	if err := injectEntityCRUDProto(protoPath, "Widget", nil, addEntityOpts{}); err != nil {
		t.Fatal(err)
	}
	if again := readFileT(t, protoPath); strings.Count(again, `import "forge/v1/forge.proto";`) != 1 {
		t.Errorf("repeat injection duplicated the forge import:\n%s", again)
	}
}

// TestInjectEntityCRUDProto_UserReorderedImports pins that the ensure-
// import logic appends after the LAST import regardless of how the user
// ordered the block, and never duplicates an import that is already
// present anywhere in it.
func TestInjectEntityCRUDProto_UserReorderedImports(t *testing.T) {
	root := scaffoldAddEntityProject(t)
	protoPath := filepath.Join(root, "proto", "services", "item", "v1", "item.proto")
	reordered := `syntax = "proto3";

package services.item.v1;

import "google/protobuf/timestamp.proto";
import "forge/v1/forge.proto";

option go_package = "example.com/x/gen/services/item/v1;itemv1";

service ItemService {
}
`
	if err := os.WriteFile(protoPath, []byte(reordered), 0o644); err != nil {
		t.Fatal(err)
	}
	fields, err := parseEntityFieldArgs([]string{"url:string"})
	if err != nil {
		t.Fatal(err)
	}
	if err := injectEntityCRUDProto(protoPath, "Bookmark", fields, addEntityOpts{}); err != nil {
		t.Fatal(err)
	}
	got := readFileT(t, protoPath)
	for _, imp := range []string{
		`import "forge/v1/forge.proto";`,
		`import "google/protobuf/timestamp.proto";`,
		`import "google/protobuf/field_mask.proto";`,
	} {
		if n := strings.Count(got, imp); n != 1 {
			t.Errorf("%s appears %d times, want exactly 1:\n%s", imp, n, got)
		}
	}
	// New imports must still land in the header (after the existing
	// import block, before the service).
	fmIdx := strings.Index(got, `import "google/protobuf/field_mask.proto";`)
	svcIdx := strings.Index(got, "service ItemService {")
	if fmIdx > svcIdx {
		t.Errorf("field_mask import landed after the service block:\n%s", got)
	}
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
