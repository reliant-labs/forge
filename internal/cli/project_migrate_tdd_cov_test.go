package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func covDWriteTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A hand-rolled slice with no matching for-range loop: recognised by name,
// but the codemod must say why it cannot rewrite it and leave it alone.
const covDNoLoopTest = `package user_test

import "testing"

func TestHandlers(t *testing.T) {
	tests := []struct {
		name string
		call func() error
	}{}
	_ = tests
}
`

func TestCovDRunMigrateTDD_RewritesMatchesAndReportsTheRest(t *testing.T) {
	root := t.TempDir()
	covDWriteTree(t, root, map[string]string{
		"internal/handlers/user/handlers_test.go": string(loadFixture(t, "svc_basic/input.go.txt")),
		"internal/handlers/user/other_test.go":    "package user_test\n\nimport \"testing\"\n\nfunc TestPlain(t *testing.T) {}\n",
		"internal/handlers/user/noloop_test.go":   covDNoLoopTest,
		"internal/handlers/user/helper.go":        "package user\n",
	})

	var runErr error
	stdout, stderr := captureStdoutAndStderr(t, func() {
		runErr = runMigrateTDD(migrateTDDFlags{path: root})
	})
	if runErr != nil {
		t.Fatalf("runMigrateTDD: %v (stderr %s)", runErr, stderr)
	}
	if !strings.Contains(stdout, "handlers_test.go: rewrote — 2 RPC case(s) lifted from TestHandlers") {
		t.Errorf("missing rewrite line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "noloop_test.go: skipped — function TestHandlers: no `for _, tt := range tests` loop found") {
		t.Errorf("a candidate that cannot be migrated must say why:\n%s", stdout)
	}
	if strings.Contains(stdout, "other_test.go") {
		t.Errorf("a file that is obviously not a candidate must stay quiet:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Summary: 1 transformed, 2 skipped, 0 errors") {
		t.Errorf("summary wrong:\n%s", stdout)
	}
	got, err := os.ReadFile(filepath.Join(root, "internal", "handlers", "user", "handlers_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "tdd.RunRPCCases") {
		t.Errorf("the matching file was not rewritten:\n%s", got)
	}
	left, _ := os.ReadFile(filepath.Join(root, "internal", "handlers", "user", "noloop_test.go"))
	if string(left) != covDNoLoopTest {
		t.Error("a skipped file must be left byte-identical")
	}
}

func TestCovDRunMigrateTDD_DryRunLeavesFilesAlone(t *testing.T) {
	root := t.TempDir()
	src := string(loadFixture(t, "svc_basic/input.go.txt"))
	covDWriteTree(t, root, map[string]string{"internal/handlers/user/handlers_test.go": src})

	var runErr error
	stdout, _ := captureStdoutAndStderr(t, func() {
		runErr = runMigrateTDD(migrateTDDFlags{path: root, dryRun: true})
	})
	if runErr != nil {
		t.Fatal(runErr)
	}
	if !strings.Contains(stdout, "would rewrite") || !strings.Contains(stdout, "1 transformed") {
		t.Errorf("dry run output:\n%s", stdout)
	}
	got, _ := os.ReadFile(filepath.Join(root, "internal", "handlers", "user", "handlers_test.go"))
	if string(got) != src {
		t.Error("dry run modified the file")
	}
}

func TestCovDRunMigrateTDD_FallsBackToTheRootWithoutInternalHandlers(t *testing.T) {
	root := t.TempDir()
	covDWriteTree(t, root, map[string]string{"svc/handlers_test.go": string(loadFixture(t, "svc_basic/input.go.txt"))})
	var runErr error
	stdout, _ := captureStdoutAndStderr(t, func() {
		runErr = runMigrateTDD(migrateTDDFlags{path: root})
	})
	if runErr != nil || !strings.Contains(stdout, "1 transformed") {
		t.Errorf("err=%v\n%s", runErr, stdout)
	}
}

func TestCovDRunMigrateTDD_ParseFailuresFailTheRun(t *testing.T) {
	root := t.TempDir()
	covDWriteTree(t, root, map[string]string{
		"internal/handlers/user/broken_test.go": "package user_test\nfunc TestX(t *testing.T) {\n",
	})
	var runErr error
	stdout, stderr := captureStdoutAndStderr(t, func() {
		runErr = runMigrateTDD(migrateTDDFlags{path: root})
	})
	if runErr == nil || !strings.Contains(runErr.Error(), "1 file(s) failed") {
		t.Fatalf("err = %v, want a failure naming the count", runErr)
	}
	if !strings.Contains(stderr, "broken_test.go: ERROR parse:") {
		t.Errorf("stderr should name the broken file:\n%s", stderr)
	}
	if !strings.Contains(stdout, "0 transformed, 0 skipped, 1 errors") {
		t.Errorf("summary:\n%s", stdout)
	}
}

func TestCovDRunMigrateTDD_RefusesAConnectV2Module(t *testing.T) {
	root := t.TempDir()
	writeV2Module(t, root, "require connectrpc.com/connect/v2 v2.0.2\n")
	err := runMigrateTDD(migrateTDDFlags{path: root})
	if err == nil || !strings.Contains(err.Error(), "connect-go v2") && !strings.Contains(err.Error(), "RunRPCCasesV2") {
		t.Fatalf("err = %v, want the v2 refusal", err)
	}
}

func TestCovDRunMigrateTDD_MissingPathIsAWalkError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "gone")
	err := runMigrateTDD(migrateTDDFlags{path: root})
	if err == nil || !strings.Contains(err.Error(), "walk") {
		t.Fatalf("err = %v, want a walk error", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the walk error should wrap ErrNotExist: %v", err)
	}
}

func TestCovDRunMigrateTDD_DefaultsToTheWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	covDWriteTree(t, root, map[string]string{"internal/handlers/user/handlers_test.go": string(loadFixture(t, "svc_basic/input.go.txt"))})
	t.Chdir(root)
	var runErr error
	stdout, _ := captureStdoutAndStderr(t, func() {
		runErr = runMigrateTDD(migrateTDDFlags{dryRun: true})
	})
	if runErr != nil || !strings.Contains(stdout, filepath.Join("internal", "handlers", "user", "handlers_test.go")+": would rewrite") {
		t.Errorf("err=%v\n%s", runErr, stdout)
	}
}

func TestCovDRelPath(t *testing.T) {
	if got := relPath(filepath.Join("a", "b"), filepath.Join("a", "b", "c", "d.go")); got != filepath.Join("c", "d.go") {
		t.Errorf("relPath = %q", got)
	}
	// A relative base cannot be related to an absolute path on any OS, so the
	// path comes back as given.
	abs := filepath.Join(t.TempDir(), "x")
	if got := relPath("rel", abs); got != abs {
		t.Errorf("an unrelatable path must come back unchanged, got %q want %q", got, abs)
	}
}

func TestCovDMigrateTDDFile_Errors(t *testing.T) {
	if _, err := migrateTDDFile(filepath.Join(t.TempDir(), "absent_test.go"), false); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: %v", err)
	}
	dest := writeTempFile(t, []byte("package x\nfunc ("))
	if _, err := migrateTDDFile(dest, false); err == nil || !strings.Contains(err.Error(), "parse:") {
		t.Errorf("unparseable file: %v", err)
	}
}

func TestCovDPlanMigration_SkipReasons(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{"no candidate function", "package x\nfunc helper() {}\n", "no recognised TestHandlers/TestIntegration body found"},
		{"empty named func body", "package x\nfunc TestHandlers(t int) {}\n", "no `tests := []struct{...}{...}` assignment found"},
		{"no constructor", `package x
func TestHandlers(t int) {
	tests := []struct{ name string }{}
	for _, tt := range tests { _ = tt }
}
`, "no `app.NewTestX(t)` constructor found"},
		{"loop missing", covDNoLoopTest, "no `for _, tt := range tests` loop found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dest := writeTempFile(t, []byte(c.src))
			res, err := migrateTDDFile(dest, false)
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != migrateStatusSkippedNoMatch || !strings.Contains(res.Reason, c.want) {
				t.Errorf("status=%v reason=%q, want skip containing %q", res.Status, res.Reason, c.want)
			}
		})
	}
}
