package codegen

import (
	"os"
	"path/filepath"
	"testing"
)

func writeGoFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHasExcludeContractDirective(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{
			name: "package doc directive, spaced",
			files: map[string]string{
				"contract.go": "// forge:exclude-contract\npackage foo\n",
			},
			want: true,
		},
		{
			name: "package doc directive, unspaced",
			files: map[string]string{
				"contract.go": "//forge:exclude-contract\npackage foo\n",
			},
			want: true,
		},
		{
			name: "free-standing directive in doc.go",
			files: map[string]string{
				"contract.go": "package foo\n",
				"doc.go":      "package foo\n\n//forge:exclude-contract\n",
			},
			want: true,
		},
		{
			name: "no directive",
			files: map[string]string{
				"contract.go": "package foo\n\ntype Service interface{}\n",
			},
			want: false,
		},
		{
			name: "prose mentioning directive is not a match",
			files: map[string]string{
				"contract.go": "// This package could use forge:exclude-contract but does not.\npackage foo\n",
			},
			want: false,
		},
		{
			name: "directive only in _test.go is ignored",
			files: map[string]string{
				"contract.go":      "package foo\n",
				"contract_test.go": "//forge:exclude-contract\npackage foo\n",
			},
			want: false,
		},
		{
			name: "directive only in _gen.go is ignored",
			files: map[string]string{
				"contract.go": "package foo\n",
				"mock_gen.go": "//forge:exclude-contract\npackage foo\n",
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range tc.files {
				writeGoFile(t, dir, name, content)
			}
			if got := HasExcludeContractDirective(dir); got != tc.want {
				t.Fatalf("HasExcludeContractDirective = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHasExternalComponentDirective(t *testing.T) {
	cases := []struct {
		name string
		file string
		want bool
	}{
		{"external-component spaced", "// forge:external-component\npackage foo\n", true},
		{"external-component unspaced", "//forge:external-component\npackage foo\n", true},
		{"provided alias", "//forge:provided\npackage foo\n", true},
		{"no directive", "package foo\n", false},
		{"exclude-contract is NOT external-component", "//forge:exclude-contract\npackage foo\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeGoFile(t, dir, "contract.go", tc.file)
			if got := HasExternalComponentDirective(dir); got != tc.want {
				t.Fatalf("HasExternalComponentDirective = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFilterExternalComponents proves the external-component marker drops a
// component from the Build graph while leaving unmarked ones in place.
func TestFilterExternalComponents(t *testing.T) {
	projectDir := t.TempDir()

	// Two packages under internal/: one marked external, one normal.
	writeGoFile(t, filepath.Join(projectDir, "internal", "billing"),
		"contract.go", "//forge:external-component\npackage billing\n\ntype Service interface{}\n")
	writeGoFile(t, filepath.Join(projectDir, "internal", "user"),
		"contract.go", "package user\n\ntype Service interface{}\n")

	comps := []BuildComponent{
		{FieldName: "Billing", compRoleRoot: "internal", compImportLeaf: "billing"},
		{FieldName: "User", compRoleRoot: "internal", compImportLeaf: "user"},
	}

	got := filterExternalComponents(projectDir, comps)
	if len(got) != 1 || got[0].FieldName != "User" {
		t.Fatalf("filterExternalComponents kept %+v, want only [User]", got)
	}
}
