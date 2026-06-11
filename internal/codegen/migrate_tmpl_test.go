package codegen

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/templates"
)

// TestMigrateTemplate_FailsLoud pins the generated AutoMigrate's
// fail-loud contract:
//
//   - an embedded-FS ReadDir ERROR is a real failure (corrupt embed,
//     bad path), not a reason to silently skip migrations;
//   - m.Version() errors are checked, not discarded with `_`;
//   - a dirty schema is a hard error requiring manual intervention,
//     never something to Info-log and drive past.
func TestMigrateTemplate_FailsLoud(t *testing.T) {
	out, err := templates.ProjectTemplates().Render("migrate.go.tmpl", struct {
		HasMigrations bool
		ModulePath    string
	}{HasMigrations: true, ModulePath: "example.com/proj"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(out)

	if strings.Contains(got, "version, dirty, _ :=") || strings.Contains(got, "newVersion, _, _ :=") {
		t.Errorf("m.Version() errors must be checked, not discarded\n--- RENDERED ---\n%s", got)
	}
	if !strings.Contains(got, "dirty") || !strings.Contains(got, "return fmt.Errorf(\"database schema is dirty") {
		t.Errorf("dirty schema must be a hard error\n--- RENDERED ---\n%s", got)
	}
	// ReadDir error path: must return the error. The only legitimate
	// skip is a genuinely empty migrations dir.
	if !strings.Contains(got, "reading embedded migrations") {
		t.Errorf("ReadDir error must surface as an error, not an Info-skip\n--- RENDERED ---\n%s", got)
	}
	if strings.Contains(got, "err != nil || len(entries) == 0") {
		t.Errorf("ReadDir error must not be folded into the empty-dir skip\n--- RENDERED ---\n%s", got)
	}
}
