package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/templates"
)

// TestFrontendFormDialect reads the dialect a page forge adds to an existing
// frontend is written in from that frontend's own package.json: one that
// predates zod 4 keeps getting forms its packages compile.
func TestFrontendFormDialect(t *testing.T) {
	for name, tc := range map[string]struct {
		pkg  string // "" = no package.json
		want codegen.FormDialect
	}{
		"scaffolded today": {`{"dependencies": {"zod": "^4.6.6", "@hookform/resolvers": "^7.0.7"}}`, codegen.FormDialect{}},
		"scaffolded before zod 4": {`{"dependencies": {"zod": "^3.23.0", "@hookform/resolvers": "^3.9.0"}}`,
			codegen.FormDialect{Zod3: true, LegacyResolver: true}},
		// Half-upgraded: resolvers 5 accepts zod 3 and already types the
		// form by input and output.
		"zod 3 with resolvers 5":   {`{"dependencies": {"zod": "~3.25.76", "@hookform/resolvers": "^5.2.2"}}`, codegen.FormDialect{Zod3: true}},
		"declared as a dev dep":    {`{"devDependencies": {"zod": "3.25.76"}}`, codegen.FormDialect{Zod3: true}},
		"unreadable range":         {`{"dependencies": {"zod": "catalog:", "@hookform/resolvers": "workspace:*"}}`, codegen.FormDialect{}},
		"no zod at all":            {`{"dependencies": {"react": "^19.1.0"}}`, codegen.FormDialect{}},
		"no package.json":          {"", codegen.FormDialect{}},
		"package.json is not JSON": {`{`, codegen.FormDialect{}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.pkg != "" {
				if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(tc.pkg), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := frontendFormDialect(dir); got != tc.want {
				t.Errorf("frontendFormDialect = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestDeclaredMajor(t *testing.T) {
	for spec, want := range map[string]int{
		"^4.6.6": 4, "~3.25": 3, ">=3.0.0": 3, "3.x": 3, "v4": 4, " ^10.1 ": 10,
	} {
		if got, ok := declaredMajor(spec); !ok || got != want {
			t.Errorf("declaredMajor(%q) = %d, %v; want %d", spec, got, ok, want)
		}
	}
	for _, spec := range []string{"", "latest", "workspace:*", "catalog:", "github:colinhacks/zod", "*"} {
		if got, ok := declaredMajor(spec); ok {
			t.Errorf("declaredMajor(%q) = %d, true; want no major", spec, got)
		}
	}
}

// scaffoldedPackageJSON is a frontend kind's package.json as forge
// scaffolds it today, with the template's optional workspace block dropped
// so it parses as JSON.
func scaffoldedPackageJSON(t *testing.T, kind string) string {
	t.Helper()
	raw, err := templates.FrontendTemplates().Get(kind + "/package.json.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if start, end := strings.Index(text, "{{- if .Workspaces}}"), strings.Index(text, "{{- end}}"); start >= 0 && end > start {
		text = text[:start] + text[end+len("{{- end}}"):]
	}
	return text
}

// TestZod4Migration_Detection runs the SHIPPED v1.8.0 detection: a frontend
// that declares zod 3 or an @hookform/resolvers below 5 is offered the move
// to zod 4, and one scaffolded today is not — nor is it read as anything
// but today's form dialect.
func TestZod4Migration_Detection(t *testing.T) {
	metas, err := loadMigrationMetas()
	if err != nil {
		t.Fatalf("loadMigrationMetas: %v", err)
	}
	var m migrationMeta
	for _, candidate := range metas {
		if candidate.SkillPath == zod4MigrationSkill {
			m = candidate
		}
	}
	if m.ID == "" {
		t.Fatalf("no shipped migration at %s — `forge generate` points users at it", zod4MigrationSkill)
	}

	cases := map[string]struct {
		rel, pkg string
		want     bool
	}{
		"zod 3 frontend":            {"frontends/web/package.json", `{"dependencies": {"zod": "^3.23.0", "@hookform/resolvers": "^3.9.0"}}`, true},
		"exact zod 3 pin":           {"frontends/web/package.json", `{"dependencies": {"zod": "3.25.76"}}`, true},
		"zod 4 with old resolvers":  {"frontends/web/package.json", `{"dependencies": {"zod": "^4.6.6", "@hookform/resolvers": "~3.10.0"}}`, true},
		"site at the project root":  {"package.json", `{"dependencies": {"zod": "^3.23.0"}}`, true},
		"zod 4":                     {"frontends/web/package.json", `{"dependencies": {"zod": "^4.6.6", "@hookform/resolvers": "^7.0.7"}}`, false},
		"a later major, not zod 1+": {"frontends/web/package.json", `{"dependencies": {"zod": "^13.0.0", "@hookform/resolvers": "^10.0.0"}}`, false},
		"no zod":                    {"frontends/web/package.json", `{"dependencies": {"react": "^19.1.0"}}`, false},
	}
	for _, kind := range []string{"nextjs", "vite-spa", "react-native"} {
		cases["scaffolded today: "+kind] = struct {
			rel, pkg string
			want     bool
		}{"frontends/web/package.json", scaffoldedPackageJSON(t, kind), false}
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, tc.rel)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.pkg), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := migrationApplies(m, "v1.7.0", dir); got != tc.want {
				t.Errorf("migration applies = %v, want %v", got, tc.want)
			}
			if strings.HasPrefix(name, "scaffolded today") {
				if d := frontendFormDialect(filepath.Dir(path)); d != (codegen.FormDialect{}) {
					t.Errorf("today's scaffold reads as form dialect %+v, want the zod 4 default", d)
				}
			}
		})
	}
}
