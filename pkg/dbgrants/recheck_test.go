package dbgrants

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/schemadef"
)

// The runtime package keeps its own copy of the allow grammar (it must not
// depend on the schema reader); this holds the copy to the original.
func TestDashboardAllowPatternIsSchemadefs(t *testing.T) {
	if want := schemadef.MarkerListPattern(schemadef.ColumnMarkerDashboard); dashboardAllowPattern != want {
		t.Errorf("dashboardAllowPattern = %q\nschemadef.MarkerListPattern = %q", dashboardAllowPattern, want)
	}
}

// Both scripts re-check each listed column against its catalog comment; only
// the dashboard one requires the allow, and both leave the refused list for
// Apply.
func TestRenderedScriptsRecheckTheCatalog(t *testing.T) {
	ro, err := Render(fixture())
	if err != nil {
		t.Fatal(err)
	}
	dash, err := RenderDashboard(fixture())
	if err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{"read-only": ro, "dashboard": dash} {
		for _, want := range []string{"col_description(a.attrelid, a.attnum)", " !~ '", "set_config('" + refusedSetting + "'", "RAISE WARNING"} {
			if !strings.Contains(script, want) {
				t.Errorf("%s script lacks %q", name, want)
			}
		}
	}
	if !strings.Contains(ro, quoteLiteral(secretDenyPattern)) || !strings.Contains(dash, quoteLiteral(dashboardDenyPattern)) {
		t.Error("a script does not refuse the denies")
	}
	if strings.Contains(ro, dashboardAllowPattern) {
		t.Error("the read-only script requires the dashboard allow")
	}
	if !strings.Contains(dash, "obj_description(a.attrelid, 'pg_class')") || !strings.Contains(dash, quoteLiteral("(?n)"+dashboardAllowPattern)) {
		t.Error("the dashboard script does not require the table and column allows")
	}
}
