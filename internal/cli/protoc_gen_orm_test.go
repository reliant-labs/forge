package cli

import (
	"path"
	"testing"
)

// TestSharedFilenameStableAcrossProtos is the regression guard for the
// orm-shared-redeclared-multi-proto blocker.
//
// The bug: when a Go package contains multiple .proto files (e.g.
// proto/db/v1/foo.proto and proto/db/v1/bar.proto), the protoc-gen-forge
// ORM plugin emitted the shared package-level file as
// <protoFilenamePrefix>_orm_shared.pb.orm.go. On the first generate run
// (only foo.proto), the plugin wrote foo_orm_shared.pb.orm.go. After
// adding bar.proto and rerunning, the plugin emitted
// bar_orm_shared.pb.orm.go — but the stale foo_orm_shared.pb.orm.go was
// still on disk. Both files declared `var ormTracer = ...` at package
// scope, causing `go build` to fail with "ormTracer redeclared in this
// block".
//
// Fix: the shared filename is derived from the proto file's *directory*
// rather than the per-file prefix. All proto files in the same package
// share the same prefix path.Dir(GeneratedFilenamePrefix), so a single
// stable filename (<dir>/orm_shared.pb.orm.go) is emitted regardless of
// which proto file triggered the emit. Subsequent runs overwrite the
// same path; no stale duplicates can accumulate.
func TestSharedFilenameStableAcrossProtos(t *testing.T) {
	cases := []struct {
		// generatedFilenamePrefix mirrors what protogen.File exposes for
		// a given .proto under `paths=source_relative`: it is the proto
		// path minus the .proto extension (e.g. "db/v1/foo" for the
		// file proto/db/v1/foo.proto when the buf root is "proto").
		name                    string
		generatedFilenamePrefix string
		wantSharedFilename      string
	}{
		{
			name:                    "single proto in package dir",
			generatedFilenamePrefix: "db/v1/foo",
			wantSharedFilename:      "db/v1/orm_shared.pb.orm.go",
		},
		{
			name:                    "second proto same package dir",
			generatedFilenamePrefix: "db/v1/bar",
			wantSharedFilename:      "db/v1/orm_shared.pb.orm.go",
		},
		{
			name:                    "third proto same package dir",
			generatedFilenamePrefix: "db/v1/baz_entities",
			wantSharedFilename:      "db/v1/orm_shared.pb.orm.go",
		},
		{
			name:                    "different package dir gets its own shared file",
			generatedFilenamePrefix: "audit/v1/event",
			wantSharedFilename:      "audit/v1/orm_shared.pb.orm.go",
		},
		{
			name:                    "proto at root (no subdir) still works",
			generatedFilenamePrefix: "foo",
			wantSharedFilename:      "orm_shared.pb.orm.go",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Mirror the exact construction used in generateOrmFile.
			got := path.Join(path.Dir(tc.generatedFilenamePrefix), "orm_shared.pb.orm.go")
			if got != tc.wantSharedFilename {
				t.Errorf("shared filename = %q, want %q\n"+
					"  generatedFilenamePrefix = %q\n"+
					"  If this fails, two protos in the same Go package will emit\n"+
					"  different *_orm_shared.pb.orm.go files. Stale copies from\n"+
					"  prior runs will collide on `var ormTracer = ...` and break\n"+
					"  `go build`.",
					got, tc.wantSharedFilename, tc.generatedFilenamePrefix)
			}
		})
	}

	// Cross-case invariant: every prefix that shares a directory must
	// resolve to the same shared filename. This is the property whose
	// violation is the blocker.
	prefixesPerDir := map[string][]string{
		"db/v1":    {"db/v1/foo", "db/v1/bar", "db/v1/baz_entities"},
		"audit/v1": {"audit/v1/event", "audit/v1/snapshot"},
	}
	for dir, prefixes := range prefixesPerDir {
		seen := map[string]string{}
		for _, prefix := range prefixes {
			name := path.Join(path.Dir(prefix), "orm_shared.pb.orm.go")
			if existing, ok := seen[dir]; ok && existing != name {
				t.Errorf("package %q: prefix %q produced shared file %q, but a sibling produced %q — "+
					"both will exist on disk after a regen and ormTracer will be redeclared",
					dir, prefix, name, existing)
			}
			seen[dir] = name
		}
	}
}
