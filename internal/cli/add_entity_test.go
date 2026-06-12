package cli

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

func readFileT(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
