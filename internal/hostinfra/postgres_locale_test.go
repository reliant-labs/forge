package hostinfra

import (
	"reflect"
	"testing"
)

// initdb derived its locale from LANG/LC_*, so a macOS shell exporting
// LC_ALL=C.UTF-8 (a locale macOS does not have) failed every first
// `forge env up` with "invalid locale settings". postgresConfig names the
// locale and encoding itself, the way pgtest does for the test databases.
func TestPostgresConfig_NamesItsLocaleAndEncoding(t *testing.T) {
	cfg := reflect.ValueOf(postgresConfig(Spec{Name: "postgres", Port: 5432}, t.TempDir(), ""))
	for field, want := range map[string]string{"locale": "C", "encoding": "UTF8"} {
		f := cfg.FieldByName(field)
		if !f.IsValid() {
			t.Fatalf("embeddedpostgres.Config has no %q field; re-check how the locale reaches initdb", field)
		}
		if got := f.String(); got != want {
			t.Errorf("postgresConfig %s = %q, want %q: initdb would otherwise read it from LANG/LC_*", field, got, want)
		}
	}
}
