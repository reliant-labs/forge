// File: internal/cli/scaffold/entity_test.go
//
// The proto-injection half of an entity birth: quintet completion
// (completeEntityCRUDProto) writing the CRUD RPCs and envelope messages into
// a service proto that already declares the entity message.
//
// The import assertions are the load-bearing ones. Emitting an rpc that
// carries `(forge.v1.method)` options, or a field carrying
// `(buf.validate.field)` options, WITHOUT the matching import fails the whole
// next `forge generate` inside buf with "unknown extension …" — the proto
// text reads as perfectly correct and nothing compiles. That is the repo's
// most expensive recurring bug shape, so each import forge can emit a use of
// has a test that it is also emitted.

package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
)

const entityTestProto = `syntax = "proto3";

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

// forge:entity
message Bookmark {
  string id = 1;
  string url = 2;
  repeated string tags = 3;
  bool done = 4;
}
`

func scaffoldEntityProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "forge.yaml"), []byte("name: x\nmodule: example.com/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	protoDir := filepath.Join(root, "proto", "services", "item", "v1")
	if err := os.MkdirAll(protoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(protoDir, "item.proto"), []byte(entityTestProto), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestWriteMigrationPair_SequencesAfterExisting pins the migration
// numbering every birth shares: the next pair lands after the highest
// existing sequence, never colliding with a migration already on disk.
func TestWriteMigrationPair_SequencesAfterExisting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db", "migrations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "00007_other.up.sql"), []byte("CREATE TABLE x (id TEXT PRIMARY KEY);"), 0o644); err != nil {
		t.Fatal(err)
	}
	up, down, err := writeMigrationPair(dir, "things", "CREATE TABLE things (id TEXT PRIMARY KEY);\n", "DROP TABLE things;\n")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(up) != "00008_create_things.up.sql" {
		t.Errorf("expected sequence 00008, got %s", up)
	}
	if filepath.Base(down) != "00008_create_things.down.sql" {
		t.Errorf("down pair out of step with up: %s", down)
	}
}

// completeQuintetIn runs the real quintet completion over the entity message
// declared in protoPath, feeding it the field list production feeds it: the
// message's own proto fields, read back by the raw scanner.
func completeQuintetIn(t *testing.T, root, protoPath, entity string) {
	t.Helper()
	scan, err := codegen.ScanRawProtoDir(filepath.Dir(protoPath))
	if err != nil {
		t.Fatalf("scan authored proto: %v", err)
	}
	m, ok := scan.MessageByName(entity)
	if !ok {
		t.Fatalf("raw scan did not find the %s entity message", entity)
	}
	fields, _ := entityFieldsFromSchemaDefs("services.item.v1", m.Fields)
	if _, err := completeEntityCRUDProto(root, protoPath, m.File, entity, fields, false); err != nil {
		t.Fatalf("complete CRUD quintet: %v", err)
	}
}

func TestCompleteEntityCRUDProto(t *testing.T) {
	root := scaffoldEntityProject(t)
	protoPath := filepath.Join(root, "proto", "services", "item", "v1", "item.proto")
	completeQuintetIn(t, root, protoPath, "Bookmark")
	got := readFileT(t, protoPath)

	for _, want := range []string{
		`import "google/protobuf/timestamp.proto";`,
		`import "google/protobuf/field_mask.proto";`,
		"rpc CreateBookmark(CreateBookmarkRequest) returns (CreateBookmarkResponse)",
		"rpc ListBookmarks(ListBookmarksRequest) returns (ListBookmarksResponse)",
		"message UpdateBookmarkRequest {",
		"google.protobuf.FieldMask update_mask = 2;",
		// The Create request FLATTENS the entity's client-settable fields,
		// renumbered contiguously (id is server-owned and omitted).
		"string url = 1;",
		"repeated string tags = 2;",
		"bool done = 3;",
		// List's filter surface is derived from the entity's plain scalars:
		// a text field yields `search`, each bool yields its own filter.
		"optional string search = 3;",
		"optional bool done = 4;",
		"repeated Bookmark bookmarks = 1;",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("proto missing %q:\n%s", want, got)
		}
	}

	// The author's entity message is never rewritten — completion adds the
	// surface AROUND it.
	if n := strings.Count(got, "message Bookmark {"); n != 1 {
		t.Errorf("the entity message was re-emitted (%d copies):\n%s", n, got)
	}

	// RPCs must land INSIDE the service block.
	svcEnd := strings.Index(got, "\n}")
	if createIdx := strings.Index(got, "rpc CreateBookmark"); createIdx > svcEnd {
		t.Errorf("CreateBookmark rpc landed outside the service block")
	}

	// Idempotent: a second run adds nothing.
	completeQuintetIn(t, root, protoPath, "Bookmark")
	again := readFileT(t, protoPath)
	for _, once := range []string{"message CreateBookmarkRequest {", "rpc CreateBookmark", "message ListBookmarksResponse {"} {
		if n := strings.Count(again, once); n != 1 {
			t.Errorf("re-running completion duplicated %q (%d copies)", once, n)
		}
	}
}

// entityBareServiceProto is the shape `forge scaffold service` scaffolds:
// no RPCs yet, and therefore NO forge/v1/forge.proto import — plus the
// entity message an author has just written into it. Completion adds RPCs
// carrying (forge.v1.method) options, so it MUST also add the import or
// `buf` fails every subsequent generate with "unknown extension
// forge.v1.method" (journey fr-af7355dd63).
const entityBareServiceProto = `syntax = "proto3";

package services.item.v1;

option go_package = "example.com/x/gen/services/item/v1;itemv1";

// ItemService defines the item service RPCs.
service ItemService {
  // TODO: Add your RPC methods here.
}

// forge:entity
message Bookmark {
  string id = 1;
  string url = 2;
}
`

func TestCompleteEntityCRUDProto_AddsForgeImportToBareServiceProto(t *testing.T) {
	root := scaffoldEntityProject(t)
	protoPath := filepath.Join(root, "proto", "services", "item", "v1", "item.proto")
	if err := os.WriteFile(protoPath, []byte(entityBareServiceProto), 0o644); err != nil {
		t.Fatal(err)
	}
	completeQuintetIn(t, root, protoPath, "Bookmark")
	got := readFileT(t, protoPath)

	if !strings.Contains(got, `import "forge/v1/forge.proto";`) {
		t.Fatalf("emitted (forge.v1.method) options without importing forge/v1/forge.proto — buf fails with 'unknown extension forge.v1.method':\n%s", got)
	}
	// The import must land between the package line and the service block
	// (i.e. in the file header, not appended after messages).
	impIdx := strings.Index(got, `import "forge/v1/forge.proto";`)
	pkgIdx := strings.Index(got, "package services.item.v1;")
	svcIdx := strings.Index(got, "service ItemService {")
	if impIdx < pkgIdx || impIdx > svcIdx {
		t.Errorf("forge import landed outside the file header (pkg=%d import=%d service=%d):\n%s", pkgIdx, impIdx, svcIdx, got)
	}

	// Completing a SECOND entity in the same file must not duplicate the import.
	appended := got + "\n// forge:entity\nmessage Widget {\n  string id = 1;\n}\n"
	if err := os.WriteFile(protoPath, []byte(appended), 0o644); err != nil {
		t.Fatal(err)
	}
	completeQuintetIn(t, root, protoPath, "Widget")
	if again := readFileT(t, protoPath); strings.Count(again, `import "forge/v1/forge.proto";`) != 1 {
		t.Errorf("a second completion duplicated the forge import:\n%s", again)
	}
}

// TestCompleteEntityCRUDProto_UserReorderedImports pins that the ensure-
// import logic appends after the LAST import regardless of how the user
// ordered the block, and never duplicates an import that is already
// present anywhere in it.
func TestCompleteEntityCRUDProto_UserReorderedImports(t *testing.T) {
	root := scaffoldEntityProject(t)
	protoPath := filepath.Join(root, "proto", "services", "item", "v1", "item.proto")
	reordered := `syntax = "proto3";

package services.item.v1;

import "google/protobuf/timestamp.proto";
import "forge/v1/forge.proto";

option go_package = "example.com/x/gen/services/item/v1;itemv1";

service ItemService {
}

// forge:entity
message Bookmark {
  string id = 1;
  string url = 2;
}
`
	if err := os.WriteFile(protoPath, []byte(reordered), 0o644); err != nil {
		t.Fatal(err)
	}
	completeQuintetIn(t, root, protoPath, "Bookmark")
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
