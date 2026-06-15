package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfigDepsFixture lays out a minimal project with one worker
// whose Deps mixes scalars (flagged) and collaborators (not flagged) —
// the kalshi-trader naked-scalar shape.
func writeConfigDepsFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	workerDir := filepath.Join(dir, "workers", "trader")
	if err := os.MkdirAll(workerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := `package trader

import (
	"log/slog"
	"time"

	"example.com/proj/pkg/config"
)

type Deps struct {
	Logger *slog.Logger
	Config *config.Config
	Cfg    config.TraderConfig

	WTIPersistMaxPerTick int           // the kalshi friction shape
	CycleInterval        time.Duration // hand-projected via AppExtras today
	DryRun               bool
	APIBase              *string

	Repo Repository
}

type Repository interface{}
`
	if err := os.WriteFile(filepath.Join(workerDir, "worker.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	// Generated + test files must be skipped even when they declare
	// scalar Deps shapes.
	genSource := `package trader

type Deps2 struct{}

// regenerated content referencing Deps would not be scanned anyway,
// but keep a scalar-bearing Deps here to prove _gen.go files are skipped.
`
	if err := os.WriteFile(filepath.Join(workerDir, "mock_gen.go"), []byte(genSource), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCollectConfigDepsFindings_FlagsScalarsOnly(t *testing.T) {
	dir := writeConfigDepsFixture(t)

	findings, err := collectConfigDepsFindings(dir)
	if err != nil {
		t.Fatalf("collectConfigDepsFindings: %v", err)
	}

	got := map[string]string{}
	for _, f := range findings {
		got[f.Field] = f.Type
		if f.Role != "workers" || f.Package != "trader" {
			t.Errorf("finding %s: role/package = %s/%s, want workers/trader", f.Field, f.Role, f.Package)
		}
		if f.Line == 0 || f.Col == 0 {
			t.Errorf("finding %s: missing position (%d:%d)", f.Field, f.Line, f.Col)
		}
		if !strings.HasPrefix(f.File, "workers"+string(filepath.Separator)+"trader") {
			t.Errorf("finding %s: file %q not project-relative", f.Field, f.File)
		}
	}

	want := map[string]string{
		"WTIPersistMaxPerTick": "int",
		"CycleInterval":        "time.Duration",
		"DryRun":               "bool",
		"APIBase":              "*string",
	}
	for field, typ := range want {
		if got[field] != typ {
			t.Errorf("expected finding %s (%s), got %q", field, typ, got[field])
		}
	}
	for field := range got {
		if _, ok := want[field]; !ok {
			t.Errorf("unexpected finding for non-scalar field %s (%s)", field, got[field])
		}
	}
	if len(findings) != len(want) {
		t.Errorf("finding count = %d, want %d: %+v", len(findings), len(want), findings)
	}
}

func TestConfigDepsFixHint_CarriesSnippet(t *testing.T) {
	f := configDepsFinding{
		Role: "workers", Package: "trader",
		Field: "WTIPersistMaxPerTick", Type: "int",
	}
	hint := configDepsFixHint(f)
	for _, want := range []string{
		"message TraderConfig",
		"int64 wti_persist_max_per_tick = 1",
		"(forge.v1.config)",
		"Cfg config.TraderConfig",
		"config.<env>.yaml",
		"wti_persist_max_per_tick: <value>",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("fix hint missing %q:\n%s", want, hint)
		}
	}
}

func TestFormatConfigDeps_CleanAndDirty(t *testing.T) {
	var buf bytes.Buffer
	formatConfigDeps(&buf, nil)
	if !strings.Contains(buf.String(), "config-deps clean") {
		t.Errorf("clean output missing success line: %s", buf.String())
	}

	buf.Reset()
	formatConfigDeps(&buf, []configDepsFinding{{
		File: "workers/trader/worker.go", Line: 12, Col: 2,
		Role: "workers", Package: "trader",
		Field: "WTIPersistMaxPerTick", Type: "int",
	}})
	out := buf.String()
	for _, want := range []string{
		"[forge-config-deps] workers/trader/worker.go:12:2",
		"Deps.WTIPersistMaxPerTick is a naked scalar (int)",
		"scalars are configuration",
		"warnings only",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dirty output missing %q:\n%s", want, out)
		}
	}
}

func TestCollectConfigDepsJSON_Shape(t *testing.T) {
	dir := writeConfigDepsFixture(t)
	fs, err := collectConfigDepsJSON(dir)
	if err != nil {
		t.Fatalf("collectConfigDepsJSON: %v", err)
	}
	if len(fs) == 0 {
		t.Fatal("expected JSON findings")
	}
	for _, f := range fs {
		if f.Rule != "forge-config-deps" {
			t.Errorf("rule = %q, want forge-config-deps", f.Rule)
		}
		if f.Severity != lintSevWarning {
			t.Errorf("severity = %q, want %q (advisory linter)", f.Severity, lintSevWarning)
		}
		if f.FixHint == "" {
			t.Error("missing fix_hint")
		}
	}
}

func TestAuditConfigDeps_Category(t *testing.T) {
	dir := writeConfigDepsFixture(t)
	cat := auditConfigDeps(dir)
	if cat.Status != AuditStatusWarn {
		t.Errorf("status = %q, want warn", cat.Status)
	}
	if !strings.Contains(cat.Summary, "scalar Deps field(s)") {
		t.Errorf("summary = %q", cat.Summary)
	}
	if cat.Details["finding_count"].(int) == 0 {
		t.Error("expected non-zero finding_count")
	}

	clean := auditConfigDeps(t.TempDir())
	if clean.Status != AuditStatusOK {
		t.Errorf("clean project status = %q, want ok", clean.Status)
	}
}
