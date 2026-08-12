package config

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestSchemaSurface_TopLevelKeys pins the COMPLETE set of keys forge.yaml
// accepts at the top level, and on the blocks whose surface has been trimmed
// down to what a reader actually resolves.
//
// The rule this guards: a forge.yaml key earns its place by being READ
// somewhere that changes behavior AND by not being derivable from the code,
// the protos, or KCL. Three keys shipped without meeting it — a top-level
// `hot_reload:` that resolved through an accessor no caller invoked (the live
// switch is `features.hot_reload`), a `version:` nothing read, and
// `ci.extra_jobs:` that the workflow generator never fed to its template, so
// declared jobs silently vanished. Each parsed clean and configured nothing,
// which is the failure mode a schema test catches and a unit test does not:
// there is no behavior to assert on a key with no reader.
//
// Adding a key here must therefore be a deliberate edit with a read site to
// point at. Deleting one is free.
func TestSchemaSurface_TopLevelKeys(t *testing.T) {
	cases := []struct {
		name string
		typ  reflect.Type
		want []string
	}{
		{
			name: "ProjectConfig",
			typ:  reflect.TypeFor[ProjectConfig](),
			want: []string{
				"api", "binary", "ci", "config", "contracts", "database",
				"deploy", "docker", "docs", "features", "forge_version",
				"frontend", "frontends", "harness", "k8s", "lint",
				"module_path", "name", "observability", "smoke", "stack",
			},
		},
		{
			// No extra_jobs: the ci: block only feeds the ONE-TIME workflow
			// render. A job you want in .github/workflows/ci.yml is one edit
			// in the file you already own.
			name: "CIConfig",
			typ:  reflect.TypeFor[CIConfig](),
			want: []string{
				"e2e", "lint", "permissions", "provider",
				"test", "vuln_scan",
			},
		},
		{
			// Only the two escape hatches. The contract rules are
			// unconditional; whether the lint runs is features.contracts.
			name: "ContractsConfig",
			typ:  reflect.TypeFor[ContractsConfig](),
			want: []string{"exclude", "interface_types"},
		},
		{
			name: "LintConfig",
			typ:  reflect.TypeFor[LintConfig](),
			want: []string{"frontend", "handler_file_max_loc", "rules"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := knownNames(yamlKeysOf(tc.typ))
			sort.Strings(got)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if len(got) != len(want) {
				t.Fatalf("%s yaml keys:\n got %v\nwant %v", tc.name, got, want)
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("%s yaml keys:\n got %v\nwant %v", tc.name, got, want)
				}
			}
		})
	}
}

// TestSchemaSurface_KindIsNotAuthored pins that `kind` is derived, never
// parsed: the field carries a yaml:"-" tag so a stale `kind:` in a file is
// reported as a retired key rather than silently overriding what the
// project's real sources say.
func TestSchemaSurface_KindIsNotAuthored(t *testing.T) {
	if _, ok := yamlKeysOf(reflect.TypeFor[ProjectConfig]())["kind"]; ok {
		t.Error("`kind` must not be a parsed forge.yaml key — it derives from the project's sources")
	}
	if _, retired := removedSchemaKeys["kind"]; !retired {
		t.Error("`kind` must stay in removedSchemaKeys so a stale key gets a migration hint")
	}
}

// TestConfigWarnings_DedupAcrossLabelSpellings pins that the same retired key
// in the same file warns ONCE per process even when callers spell the path
// differently. A single command loads forge.yaml through several call sites,
// some passing an absolute path and some the bare filename; keying the dedup
// set on the raw label made those look like two files and printed every
// retired-key notice twice.
func TestConfigWarnings_DedupAcrossLabelSpellings(t *testing.T) {
	var sink strings.Builder
	prev := SetConfigWarningSink(&sink)
	defer SetConfigWarningSink(prev)

	const in = "name: demo\nmodule_path: github.com/example/demo\nversion: 1.0.0\n"
	for _, label := range []string{"forge.yaml", "/abs/path/to/forge.yaml", "./forge.yaml"} {
		if _, err := LoadProject([]byte(in), label); err != nil {
			t.Fatalf("retired key must not gate the load (%s): %v", label, err)
		}
	}

	if got := strings.Count(sink.String(), `"version" is no longer`); got != 1 {
		t.Errorf("warning emitted %d times across label spellings, want 1:\n%s", got, sink.String())
	}
}
