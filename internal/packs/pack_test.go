package packs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/reliant-labs/forge/internal/config"
)

func TestLoadPack(t *testing.T) {
	p, err := LoadPack("jwt-auth")
	if err != nil {
		t.Fatalf("LoadPack(jwt-auth) error: %v", err)
	}

	if p.Name != "jwt-auth" {
		t.Errorf("Name = %q, want %q", p.Name, "jwt-auth")
	}
	if p.Version != "1.0.0" {
		t.Errorf("Version = %q, want %q", p.Version, "1.0.0")
	}
	if p.Description == "" {
		t.Error("Description is empty")
	}
	if p.Config.Section != "auth" {
		t.Errorf("Config.Section = %q, want %q", p.Config.Section, "auth")
	}
	if len(p.Files) != 2 {
		t.Errorf("len(Files) = %d, want 2", len(p.Files))
	}
	if len(p.Dependencies) != 2 {
		t.Errorf("len(Dependencies) = %d, want 2", len(p.Dependencies))
	}
	if len(p.Generate) != 1 {
		t.Errorf("len(Generate) = %d, want 1", len(p.Generate))
	}
}

func TestLoadPackNotFound(t *testing.T) {
	_, err := LoadPack("nonexistent-pack")
	if err == nil {
		t.Fatal("LoadPack(nonexistent-pack) expected error, got nil")
	}
}

func TestListPacks(t *testing.T) {
	packs, err := ListPacks()
	if err != nil {
		t.Fatalf("ListPacks() error: %v", err)
	}

	if len(packs) == 0 {
		t.Fatal("ListPacks() returned no packs")
	}

	found := false
	for _, p := range packs {
		if p.Name == "jwt-auth" {
			found = true
			break
		}
	}
	if !found {
		t.Error("ListPacks() did not include jwt-auth")
	}
}

func TestGetPack(t *testing.T) {
	p, err := GetPack("jwt-auth")
	if err != nil {
		t.Fatalf("GetPack(jwt-auth) error: %v", err)
	}
	if p.Name != "jwt-auth" {
		t.Errorf("Name = %q, want %q", p.Name, "jwt-auth")
	}
}

func TestGetPackInvalidName(t *testing.T) {
	_, err := GetPack("../etc/passwd")
	if err == nil {
		t.Fatal("GetPack(../etc/passwd) expected error, got nil")
	}
}

func TestValidPackName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"jwt-auth", true},
		{"my_pack", true},
		{"auth123", true},
		{"", false},
		{"-leading-hyphen", false},
		{"_leading-underscore", false},
		{"has spaces", false},
		{"has/slash", false},
		{"has.dot", false},
		{"UPPERCASE", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ValidPackName(tt.name)
			if got != tt.want {
				t.Errorf("ValidPackName(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// packs-1 regression: nextMigrationID must scan db/migrations/ for the
// highest existing numeric prefix and return max+1, regardless of whether the
// existing files use 4- or 5-digit zero-pad.
func TestNextMigrationID(t *testing.T) {
	tests := []struct {
		name    string
		files   []string
		want    int
		wantErr bool
	}{
		{"no_dir", nil, 1, false},
		{"only_init", []string{"00001_init.up.sql", "00001_init.down.sql"}, 2, false},
		{"five_digit_chain", []string{"00001_init.up.sql", "00002_api_keys.up.sql", "00003_audit_log.up.sql"}, 4, false},
		{"mixed_widths", []string{"00001_init.up.sql", "0002_api_keys.up.sql"}, 3, false},
		{"unrelated_files_ignored", []string{"00001_init.up.sql", "README.md", "junk"}, 2, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.files != nil {
				migDir := filepath.Join(dir, "db", "migrations")
				if err := os.MkdirAll(migDir, 0755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				for _, f := range tt.files {
					path := filepath.Join(migDir, f)
					if err := os.WriteFile(path, []byte("-- empty"), 0644); err != nil {
						t.Fatalf("write %s: %v", f, err)
					}
				}
			}
			got, err := nextMigrationID(dir)
			if (err != nil) != tt.wantErr {
				t.Fatalf("nextMigrationID err = %v, wantErr = %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("nextMigrationID = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestFindMigrationIDBySlug ensures we recognise an already-installed pack
// migration by its slug regardless of zero-padding width, so resync skips
// the duplicate emission.
func TestFindMigrationIDBySlug(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		files     []string
		slug      string
		wantID    int
		wantFound bool
	}{
		{"empty_dir", nil, "audit_log", 0, false},
		{"slug_present_5digit", []string{"00001_baseline.up.sql", "00002_audit_log.up.sql", "00002_audit_log.down.sql"}, "audit_log", 2, true},
		{"slug_present_4digit", []string{"0002_api_keys.up.sql"}, "api_keys", 2, true},
		{"slug_absent", []string{"00001_baseline.up.sql"}, "audit_log", 0, false},
		{"slug_substring_no_match", []string{"00002_audit_log_other.up.sql"}, "audit_log", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.files != nil {
				mig := filepath.Join(dir, "db", "migrations")
				if err := os.MkdirAll(mig, 0755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				for _, f := range tt.files {
					if err := os.WriteFile(filepath.Join(mig, f), []byte("-- x"), 0644); err != nil {
						t.Fatalf("write %s: %v", f, err)
					}
				}
			}
			id, found, err := findMigrationIDBySlug(dir, tt.slug)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if found != tt.wantFound {
				t.Errorf("found = %v, want %v", found, tt.wantFound)
			}
			if id != tt.wantID {
				t.Errorf("id = %d, want %d", id, tt.wantID)
			}
		})
	}
}

// TestPackInstallIdempotentMigrations is the regression for backlog item
// "Pack install is not idempotent for migrations": running InstallWithConfig
// against a project that already has the pack listed and the migration on
// disk must NOT emit a second migration with a higher numeric ID.
func TestPackInstallIdempotentMigrations(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "db", "migrations"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, f := range []string{"00001_baseline.up.sql", "00001_baseline.down.sql", "00002_audit_log.up.sql", "00002_audit_log.down.sql"} {
		if err := os.WriteFile(filepath.Join(dir, "db", "migrations", f), []byte("-- existing"), 0644); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	// go.mod stub — InstallWithConfig calls `go mod tidy` near the end.
	// Writing a minimal valid go.mod plus the proto-skip path keeps the
	// test hermetic: audit-log ships a .proto, so tidy is skipped.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/test\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	cfg := &config.ProjectConfig{
		Name:       "test",
		ModulePath: "example.com/test",
		Packs:      []string{"audit-log"}, // already installed
	}

	pack, err := LoadPack("audit-log")
	if err != nil {
		t.Fatalf("LoadPack: %v", err)
	}

	if err := pack.InstallWithConfig(dir, cfg, nil); err != nil {
		t.Fatalf("re-install: %v", err)
	}

	// Verify no 0000{3,4,5}_audit_log files were emitted.
	entries, err := os.ReadDir(filepath.Join(dir, "db", "migrations"))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	auditCount := 0
	for _, e := range entries {
		if strings.Contains(e.Name(), "audit_log") && strings.HasSuffix(e.Name(), ".up.sql") {
			auditCount++
		}
	}
	if auditCount != 1 {
		t.Errorf("expected exactly 1 audit_log up-migration after re-install, found %d", auditCount)
		for _, e := range entries {
			t.Logf("  - %s", e.Name())
		}
	}

	// Verify cfg.Packs wasn't doubled.
	count := 0
	for _, n := range cfg.Packs {
		if n == "audit-log" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("cfg.Packs has audit-log %d times, want 1", count)
	}
}

// TestPackInstallCollisionDetected is the regression for backlog item
// "Pack handlers and forge service scaffolds collide": a fresh install (pack
// NOT yet in cfg.Packs) must fail fast when any overwrite=once target
// already exists, surfacing the conflicting paths and a rename recipe rather
// than silently skipping and producing a half-installed pack.
func TestPackInstallCollisionDetected(t *testing.T) {
	dir := t.TempDir()
	// Pre-create one of the audit-log pack's overwrite=once targets — this
	// is the colliding "user already scaffolded a handler under this name"
	// case the backlog item describes.
	target := filepath.Join(dir, "pkg", "middleware", "audit", "auditlog", "handler.go")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(target, []byte("package auditlog\n// hand-written\n"), 0644); err != nil {
		t.Fatalf("write target: %v", err)
	}

	cfg := &config.ProjectConfig{
		Name:       "test",
		ModulePath: "example.com/test",
		Packs:      nil, // fresh install
	}
	pack, err := LoadPack("audit-log")
	if err != nil {
		t.Fatalf("LoadPack: %v", err)
	}

	err = pack.InstallWithConfig(dir, cfg, nil)
	if err == nil {
		t.Fatal("expected collision error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "would clobber") {
		t.Errorf("error missing 'would clobber' marker: %s", msg)
	}
	if !strings.Contains(msg, "pkg/middleware/audit/auditlog/handler.go") {
		t.Errorf("error missing colliding path: %s", msg)
	}
	// Hand-written file content must be untouched.
	got, _ := os.ReadFile(target)
	if !strings.Contains(string(got), "hand-written") {
		t.Errorf("collision check did not preserve user file; got: %s", got)
	}
	// cfg.Packs must NOT have been mutated on the failed install.
	for _, n := range cfg.Packs {
		if n == "audit-log" {
			t.Error("failed install left audit-log in cfg.Packs")
		}
	}
}

func TestIsInstalled(t *testing.T) {
	cfg := &config.ProjectConfig{
		Packs: []string{"jwt-auth", "payments"},
	}

	if !IsInstalled("jwt-auth", cfg) {
		t.Error("IsInstalled(jwt-auth) = false, want true")
	}
	if IsInstalled("nonexistent", cfg) {
		t.Error("IsInstalled(nonexistent) = true, want false")
	}
}

// TestPackSubpathParsed verifies the optional `subpath:` manifest hint is
// loaded as a string. The subpath is informational — forge does NOT enforce
// any category matrix — but tests assert each shipped pack declares one so
// `forge pack info` shows users what subtree the pack will touch.
func TestPackSubpathParsed(t *testing.T) {
	cases := map[string]string{
		"jwt-auth":      "middleware/auth/jwtauth",
		"firebase-auth": "middleware/auth/firebase",
		"clerk":         "middleware/auth/clerk",
		"api-key":       "middleware/auth/apikey",
		"audit-log":     "middleware/audit/auditlog",
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			p, err := LoadPack(name)
			if err != nil {
				t.Fatalf("LoadPack(%s): %v", name, err)
			}
			if p.Subpath != want {
				t.Errorf("Subpath = %q, want %q", p.Subpath, want)
			}
		})
	}
}

// TestPackSubpathOptional confirms a manifest without `subpath:` parses
// fine and yields the empty string (the documented default = top-level).
func TestPackSubpathOptional(t *testing.T) {
	const manifest = `
name: minimal-pack
version: 1.0.0
files: []
`
	var p Pack
	if err := yaml.Unmarshal([]byte(manifest), &p); err != nil {
		t.Fatalf("unmarshal minimal manifest: %v", err)
	}
	if p.Subpath != "" {
		t.Errorf("Subpath = %q, want empty string for missing field", p.Subpath)
	}
}

// TestRenderPathTemplate confirms the path-templating helper is a no-op
// for plain strings and substitutes Go-template fields when present.
func TestRenderPathTemplate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		data map[string]any
		want string
	}{
		{"plain", "pkg/clients/nats/client.go", nil, "pkg/clients/nats/client.go"},
		{"frontend", "{{.FrontendPath}}/x/y.tsx", map[string]any{"FrontendPath": "frontends/web"}, "frontends/web/x/y.tsx"},
		{"multiple", "{{.A}}/{{.B}}", map[string]any{"A": "x", "B": "y"}, "x/y"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := renderPathTemplate(tc.in, tc.data)
			if err != nil {
				t.Fatalf("renderPathTemplate: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestEffectiveKindDefault confirms a manifest without `kind:` defaults
// to the Go pack kind so legacy packs keep working unchanged.
func TestEffectiveKindDefault(t *testing.T) {
	t.Parallel()
	p := &Pack{}
	if got := p.EffectiveKind(); got != PackKindGo {
		t.Errorf("EffectiveKind() = %q, want %q", got, PackKindGo)
	}
	if p.IsFrontendKind() {
		t.Error("IsFrontendKind() = true for default kind")
	}

	p.Kind = "frontend"
	if !p.IsFrontendKind() {
		t.Error("IsFrontendKind() = false for kind: frontend")
	}
}

// TestDataTablePackManifest confirms the data-table pack declares the
// frontend kind and the expected output paths.
func TestDataTablePackManifest(t *testing.T) {
	t.Parallel()
	p, err := LoadPack("data-table")
	if err != nil {
		t.Fatalf("LoadPack(data-table): %v", err)
	}
	if !p.IsFrontendKind() {
		t.Errorf("data-table pack must be Kind=frontend, got %q", p.Kind)
	}
	if len(p.NPMDependencies) == 0 {
		t.Error("data-table pack must declare npm dependencies")
	}
	for _, f := range p.Files {
		if !strings.Contains(f.Output, "{{.FrontendPath}}") {
			t.Errorf("data-table file %q output must reference {{.FrontendPath}}", f.Output)
		}
	}
}

// TestNATSPackManifest confirms the nats pack declares the expected
// shape — Go kind, the conventional subpath, and a pinned dependency.
func TestNATSPackManifest(t *testing.T) {
	t.Parallel()
	p, err := LoadPack("nats")
	if err != nil {
		t.Fatalf("LoadPack(nats): %v", err)
	}
	if p.IsFrontendKind() {
		t.Errorf("nats pack must be Go kind, got Kind=%q", p.Kind)
	}
	if p.Subpath != "clients/nats" {
		t.Errorf("nats pack Subpath = %q, want clients/nats", p.Subpath)
	}
	if len(p.Dependencies) == 0 {
		t.Error("nats pack must declare at least one Go dependency")
	}
	foundNATS := false
	for _, dep := range p.Dependencies {
		if strings.HasPrefix(dep, "github.com/nats-io/nats.go") {
			foundNATS = true
			break
		}
	}
	if !foundNATS {
		t.Error("nats pack must depend on github.com/nats-io/nats.go")
	}
}

func TestPackFileOverwrite(t *testing.T) {
	p, err := LoadPack("jwt-auth")
	if err != nil {
		t.Fatalf("LoadPack error: %v", err)
	}

	for _, f := range p.Files {
		switch f.Overwrite {
		case "always", "once", "never":
			// valid
		default:
			t.Errorf("File %s has invalid overwrite value %q", f.Template, f.Overwrite)
		}
	}
}

// TestResolveInstallOrder_AutoInstallsDeps verifies that requesting a
// pack with declared `depends_on` returns the deps first, then the
// requested pack. This is the cpnext fresh-cut scenario: `forge pack
// add api-key` in an empty project should auto-pull audit-log first.
func TestResolveInstallOrder_AutoInstallsDeps(t *testing.T) {
	t.Parallel()
	order, err := ResolveInstallOrder([]string{"api-key"}, nil)
	if err != nil {
		t.Fatalf("ResolveInstallOrder: %v", err)
	}
	// audit-log must come BEFORE api-key — we install producers first.
	wantPrefix := []string{"audit-log", "api-key"}
	if len(order) != len(wantPrefix) {
		t.Fatalf("order = %v, want %v", order, wantPrefix)
	}
	for i, name := range wantPrefix {
		if order[i] != name {
			t.Errorf("order[%d] = %q, want %q (full order: %v)", i, order[i], name, order)
		}
	}
}

// TestResolveInstallOrder_ExistingPreserved verifies that already-installed
// packs are kept at the head of the returned slice in their existing order.
func TestResolveInstallOrder_ExistingPreserved(t *testing.T) {
	t.Parallel()
	// audit-log already installed; user adds api-key. Expected output:
	// [audit-log, api-key] — audit-log retained at front, api-key
	// emitted (its dep is satisfied).
	order, err := ResolveInstallOrder([]string{"api-key"}, []string{"audit-log"})
	if err != nil {
		t.Fatalf("ResolveInstallOrder: %v", err)
	}
	want := []string{"audit-log", "api-key"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i, name := range want {
		if order[i] != name {
			t.Errorf("order[%d] = %q, want %q", i, order[i], name)
		}
	}
}

// TestResolveInstallOrder_NoDeps verifies a pack without depends_on is
// emitted by itself, no spurious transitive lookups.
func TestResolveInstallOrder_NoDeps(t *testing.T) {
	t.Parallel()
	order, err := ResolveInstallOrder([]string{"audit-log"}, nil)
	if err != nil {
		t.Fatalf("ResolveInstallOrder: %v", err)
	}
	if len(order) != 1 || order[0] != "audit-log" {
		t.Errorf("order = %v, want [audit-log]", order)
	}
}

// TestSortInstalledByDependencies verifies the generate-time topo sort
// returns producers before consumers. The api-key→audit-log edge means
// audit-log's generate hooks must run before api-key's.
func TestSortInstalledByDependencies(t *testing.T) {
	t.Parallel()
	// Even if cfg.Packs lists api-key BEFORE audit-log (legal — no
	// cfg.Packs ordering invariant pre-this-feature), sort must reorder.
	order, err := SortInstalledByDependencies([]string{"api-key", "audit-log"})
	if err != nil {
		t.Fatalf("SortInstalledByDependencies: %v", err)
	}
	if len(order) != 2 {
		t.Fatalf("order = %v, want 2 entries", order)
	}
	// audit-log must come before api-key.
	auditIdx, apikeyIdx := -1, -1
	for i, n := range order {
		switch n {
		case "audit-log":
			auditIdx = i
		case "api-key":
			apikeyIdx = i
		}
	}
	if auditIdx < 0 || apikeyIdx < 0 || auditIdx >= apikeyIdx {
		t.Errorf("expected audit-log before api-key, got %v", order)
	}
}

// TestMissingDependencies verifies the audit-time helper that surfaces
// installed packs whose declared deps are NOT installed. This is the
// "user hand-edited cfg.Packs" failure mode.
func TestMissingDependencies(t *testing.T) {
	t.Parallel()
	// api-key without audit-log: audit-log should be reported missing.
	missing := MissingDependencies([]string{"api-key"})
	if len(missing) != 1 || missing[0] != "audit-log" {
		t.Errorf("MissingDependencies(api-key only) = %v, want [audit-log]", missing)
	}
	// Both installed: no missing deps.
	missing = MissingDependencies([]string{"audit-log", "api-key"})
	if len(missing) != 0 {
		t.Errorf("MissingDependencies(both) = %v, want empty", missing)
	}
	// No packs at all: trivially no missing.
	missing = MissingDependencies(nil)
	if len(missing) != 0 {
		t.Errorf("MissingDependencies(nil) = %v, want empty", missing)
	}
}
