// File: internal/cli/scaffold/entity_unique_test.go
//
// The `// forge:unique` birth path: a key births its CREATE UNIQUE INDEX
// into the owned migration, every key birth cannot honour refuses the birth
// by line before anything is written, and a key added after birth is
// reported with the migration to write.

package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/pkg/schemadef"
)

// scanJobs writes proto into a fresh project's jobs service proto dir and
// returns the project root, the migrations dir, the scan and message Job.
func scanJobs(t *testing.T, proto string) (root, migDir string, scan *codegen.RawProtoScan, m codegen.RawProtoMessage) {
	t.Helper()
	root = t.TempDir()
	protoDir := filepath.Join(root, "proto", "services", "jobs", "v1")
	if err := os.MkdirAll(protoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(protoDir, "jobs.proto"), []byte(proto), 0o644); err != nil {
		t.Fatal(err)
	}
	migDir = filepath.Join(root, "db", "migrations")
	scan, err := codegen.ScanRawProtoDir(protoDir)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := scan.MessageByName("Job")
	if !ok {
		t.Fatal("Job not scanned")
	}
	return root, migDir, scan, m
}

func jobsProto(markers string) string {
	return `syntax = "proto3";

package services.jobs.v1;

// forge:entity
` + markers + `
message Job {
  string namespace = 1;
  string name = 2;
}

service JobsService {
}
`
}

// Ranger's jobs table: UNIQUE (namespace, name), declared on the message and
// born into the migration — no hand edit.
func TestBirthMarkedEntity_UniqueKey(t *testing.T) {
	root, migDir, scan, m := scanJobs(t, jobsProto("// forge:unique namespace,name — one job name per namespace"))
	rep, err := birthMarkedEntity(migDir, root, scan, m, map[string]bool{"jobs": true}, entityOpts{}, newFKRegistry(nil))
	if err != nil {
		t.Fatalf("birth: %v", err)
	}
	up, err := os.ReadFile(rep.UpPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := "CREATE UNIQUE INDEX jobs_namespace_name_key ON jobs (namespace, name);"; !strings.Contains(string(up), want) {
		t.Errorf("born migration missing %q:\n%s", want, up)
	}
}

// Every key birth cannot honour refuses the birth, naming the line, and
// writes nothing: no migration, and the proto untouched by the managed-field
// injection.
func TestBirthMarkedEntity_UniqueKeyRefusals(t *testing.T) {
	cases := map[string]struct {
		markers string
		want    string
	}{
		"unknown column":     {"// forge:unique namespce,name", "jobs.proto:6: // forge:unique namespce,name\n      forge:unique names namespce, which is not a column of jobs — did you mean namespace?"},
		"no columns":         {"// forge:unique", "jobs.proto:6: // forge:unique\n      forge:unique names no columns"},
		"bare-word prose":    {"// forge:unique namespace and name", "names no columns"},
		"primary key":        {"// forge:unique id,name", "includes id, the primary key"},
		"repeated key":       {"// forge:unique namespace,name\n// forge:unique name,namespace", "jobs.proto:7: // forge:unique name,namespace\n      repeats the key declared at jobs.proto:6"},
		"near-miss spelling": {"// forge:uniqe namespace,name", "jobs.proto:6: // forge:uniqe namespace,name\n      looks like a misspelling of forge:unique"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			proto := jobsProto(c.markers)
			root, migDir, scan, m := scanJobs(t, proto)
			_, err := birthMarkedEntity(migDir, root, scan, m, map[string]bool{"jobs": true}, entityOpts{}, newFKRegistry(nil))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("birth must refuse with %q, got: %v", c.want, err)
			}
			if entries, _ := os.ReadDir(migDir); len(entries) != 0 {
				t.Errorf("a refused birth wrote %d migration file(s)", len(entries))
			}
			if after, _ := os.ReadFile(m.File); string(after) != proto {
				t.Errorf("a refused birth edited the proto:\n%s", after)
			}
		})
	}
}

// A key misplaced inside the body refuses too — the scan attributes it to
// the message, and the birth names its line.
func TestBirthMarkedEntity_MisplacedUniqueKey(t *testing.T) {
	proto := `syntax = "proto3";

package services.jobs.v1;

// forge:entity
message Job {
  string namespace = 1;
  string name = 2; // forge:unique namespace,name
}
`
	root, migDir, scan, m := scanJobs(t, proto)
	_, err := birthMarkedEntity(migDir, root, scan, m, map[string]bool{"jobs": true}, entityOpts{}, newFKRegistry(nil))
	if err == nil || !strings.Contains(err.Error(), "jobs.proto:8:") || !strings.Contains(err.Error(), "directly above `message Job {`") {
		t.Fatalf("birth must refuse the misplaced key by line, got: %v", err)
	}
}

// After birth the marker is inert: a key added to a born message is reported
// with the statement to put in a new migration. A key the applied table
// already holds — in any column order, under any name or predicate — is not.
func TestUniqueDriftNotes(t *testing.T) {
	_, _, scan, _ := scanJobs(t, jobsProto("// forge:unique namespace,name\n// forge:unique name"))
	tables := []schemadef.Table{{
		Name: "jobs",
		Indexes: []schemadef.Index{
			{Name: "jobs_by_ns", Columns: []string{"name", "namespace"}, Unique: true, Predicate: "deleted_at IS NULL"},
			{Name: "jobs_name_idx", Columns: []string{"name"}}, // not unique: does not satisfy the key
		},
	}}
	notes := uniqueDriftNotes(scan, tables)
	if len(notes) != 1 {
		t.Fatalf("want one note (the name key), got %q", notes)
	}
	for _, want := range []string{
		"Job: forge:unique (name) at jobs.proto:7",
		"birth is one-time",
		"CREATE UNIQUE INDEX jobs_name_key ON jobs (name);",
	} {
		if !strings.Contains(notes[0], want) {
			t.Errorf("note %q lacks %q", notes[0], want)
		}
	}
	// An unborn table has no drift: its birth emits the keys.
	if got := uniqueDriftNotes(scan, []schemadef.Table{{Name: "other"}}); len(got) != 0 {
		t.Errorf("an unborn message reported drift: %q", got)
	}
}
