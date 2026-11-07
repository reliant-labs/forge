package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUIWebDefaultExportNames(t *testing.T) {
	for _, tc := range []struct {
		source string
		want   bool
	}{
		{"export default function Button() {}", true},
		{"export { default as NamedThing } from './thing';", false},
		{"export {\n Thing as default,\n};", true},
		{"export { default } from './thing';", true},
		{"export const helper = 1;", false},
	} {
		if got := uiWebHasDefaultExport(tc.source); got != tc.want {
			t.Errorf("%q default=%v want %v", tc.source, got, tc.want)
		}
	}
}

func TestUIWebBarrelPreservesNamedHelpers(t *testing.T) {
	root := t.TempDir()
	layout := NewFrontendWorkspaceLayout("fixture")
	if err := writeUIWebPackage(root, layout); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "packages/ui-web/src/index.ts")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, helper := range []string{"button_classes", "form_field_context"} {
		if strings.Contains(string(body), "default as "+toPascalCase(helper)) {
			t.Errorf("helper %s falsely exports default", helper)
		}
		if !strings.Contains(string(body), "export * from \"./components/ui/"+helper+"\";") {
			t.Errorf("helper %s loses named exports", helper)
		}
	}
	if !strings.Contains(string(body), "export { default as Button }") {
		t.Fatal("default component contract changed")
	}
	owned := "// owned\nexport {};\n"
	if err := os.WriteFile(path, []byte(owned), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeUIWebBarrel(filepath.Join(root, "packages/ui-web")); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != owned {
		t.Fatal("owned barrel overwritten")
	}
}
