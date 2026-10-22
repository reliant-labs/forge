package codemod

import (
	"bufio"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The proof. testdata/connecterrors is a module holding every
// connect.NewError shape of reliant's P7a census, the CodeOf test
// assertions, an in-process reader in the package and in an importer, and
// an interceptor. Its wiretest prints what a real Connect client reads for
// each site, through svcerr.Interceptor for handler code and without it for
// the interceptor. This test runs that before the rewrite and after it, and
// requires the identical lines — then checks the rewrite against goldens,
// the worklist against the shapes that must be left alone, and a second run
// against a no-op.
func TestConnectErrors_WireIdenticalOverTheCorpus(t *testing.T) {
	if testing.Short() {
		t.Skip("type-checks a scratch module against forge and runs its tests twice; runs in task test")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join("testdata", "connecterrors")
	dir := t.TempDir()
	copyTree(t, fixture, dir)
	copyFile(t, filepath.Join(root, "go.sum"), filepath.Join(dir, "go.sum"))
	writeScratchGoMod(t, root, dir, "example.com/cescratch")

	before := wireLines(t, dir)
	if len(before) < 30 {
		t.Fatalf("wiretest printed %d lines, want one per site:\n%s", len(before), strings.Join(before, "\n"))
	}

	dry, err := runConnectErrors(dir, false)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if got := readFile(t, dir, "internal/handlers/things/things.go"); got != readFile(t, fixture, "internal/handlers/things/things.go") {
		t.Fatal("--dry-run wrote a file")
	}

	rep, err := runConnectErrors(dir, true)
	if err != nil {
		t.Fatalf("codemod: %v", err)
	}
	t.Logf("rewrites:\n%s\n\nnext:\n%s", strings.Join(rep.Rewrites, "\n"), strings.Join(rep.Next, "\n"))
	if strings.Join(rep.Rewrites, "\n") != strings.Join(dry.Rewrites, "\n") {
		t.Errorf("dry run and run disagree:\n%s\n---\n%s", strings.Join(dry.Rewrites, "\n"), strings.Join(rep.Rewrites, "\n"))
	}
	for _, rel := range []string{
		"internal/handlers/things/things.go",
		"internal/handlers/things/things_test.go",
		"internal/handlers/reader/reader.go",
		"internal/handlers/blocked/blocked.go",
		"internal/handlers/asserted/asserted.go",
		"internal/handlers/testifyas/testifyas.go",
		"internal/handlers/suiteas/suiteas.go",
		"internal/interceptors/auth.go",
	} {
		got := readFile(t, dir, rel)
		if os.Getenv("FORGE_UPDATE_GOLDEN") == "1" {
			if err := os.WriteFile(filepath.Join(fixture, filepath.FromSlash(rel)+".golden"), []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if want := readFile(t, fixture, rel+".golden"); got != want {
			t.Errorf("rewritten %s differs from %s.golden:\n%s", rel, rel, got)
		}
	}
	for _, rel := range []string{"internal/app/retry.go", "internal/wiretest/wire_test.go", "internal/handlers/asserted/asserted_test.go",
		"internal/handlers/testifyas/testifyas_test.go", "internal/handlers/suiteas/suiteas_test.go"} {
		if readFile(t, dir, rel) != readFile(t, fixture, rel) {
			t.Errorf("%s has nothing to rewrite but changed", rel)
		}
	}

	// Every left-alone shape is on the worklist, with its reason.
	next := strings.Join(rep.Next, "\n")
	for _, want := range []string{
		"things.go:120: connect.NewError(connect.CodeInternal, fmt.Errorf(\"failed to push: %w\", err)) — wraps an error value",
		"things.go:123: connect.NewError(connect.CodeInvalidArgument, err) — wraps an error value",
		"things.go:126: connect.NewError(connect.CodeInternal, fmt.Errorf(\"list failed: %v\", err)) — wraps an error value",
		"things.go:130: connect.NewError(connect.CodeUnavailable, errors.New(err.Error())) — wraps an error value",
		"things.go:133: connect.NewError(connect.CodeUnauthenticated, nil) — nil error",
		"things.go:135: connect.NewError(connect.CodeInvalidArgument, errors.New(msg)) — computed message",
		"things.go:138: connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(\"%s\", s)) — computed message",
		"things.go:141: connect.NewError(connect.CodeInternal, errors.New(\"\")) — empty message",
		"things.go:144: connect.NewError(code, errors.New(\"x\")) — computed code",
		"things.go:147: connect.NewError(connect.CodeFailedPrecondition, errLegacy) — wraps an error value",
		"example.com/cescratch/internal/handlers/reader: its rewrites stay *connect.Error (svcerr.ToConnect) because internal/handlers/reader/reader.go:18 (connect.CodeOf)",
		"example.com/cescratch/internal/handlers/blocked: its rewrites stay *connect.Error (svcerr.ToConnect) because internal/app/retry.go:17 (errors.As into a *connect.Error)",
		"example.com/cescratch/internal/handlers/asserted: its rewrites stay *connect.Error (svcerr.ToConnect) because internal/handlers/asserted/asserted_test.go:13 (errors.As into a *connect.Error)",
		// An As-style helper (testify's require.ErrorAs, a suite's ErrorAs
		// method) is a reader by its **connect.Error argument alone.
		"example.com/cescratch/internal/handlers/testifyas: its rewrites stay *connect.Error (svcerr.ToConnect) because internal/handlers/testifyas/testifyas_test.go:20 (errorAs into a *connect.Error)",
		"example.com/cescratch/internal/handlers/suiteas: its rewrites stay *connect.Error (svcerr.ToConnect) because internal/handlers/suiteas/suiteas_test.go:23 (assertions{…}.ErrorAs into a *connect.Error)",
		"must end with svcerr.Interceptor()",
	} {
		if !strings.Contains(next, want) {
			t.Errorf("worklist is missing %q:\n%s", want, next)
		}
	}

	again, err := runConnectErrors(dir, true)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if again.Changed() {
		t.Errorf("second run is not a no-op:\n%s", strings.Join(again.Rewrites, "\n"))
	}

	// The report's advice for a kept package, followed: switch the reader in
	// internal/app to svcerr.Code and re-run. blocked's svcerr.ToConnect
	// wrapper goes; what a client (and app.Probe) reads does not change.
	retry := filepath.Join(dir, "internal", "app", "retry.go")
	src := readFile(t, dir, "internal/app/retry.go")
	src = strings.Replace(src, "\tvar ce *connect.Error\n\treturn errors.As(err, &ce) && ce.Code() == connect.CodeUnavailable",
		"\treturn svcerr.Code(err) == connect.CodeUnavailable", 1)
	src = strings.Replace(src, "import (\n\t\"errors\"\n\n\t\"connectrpc.com/connect\"\n",
		"import (\n\t\"connectrpc.com/connect\"\n\n\t\"github.com/reliant-labs/forge/pkg/svcerr\"\n", 1)
	if err := os.WriteFile(retry, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	third, err := runConnectErrors(dir, true)
	if err != nil {
		t.Fatalf("run after switching the reader: %v", err)
	}
	if got := strings.Join(third.Rewrites, "\n"); !strings.Contains(got, `blocked.go:11: svcerr.ToConnect(svcerr.Unavailable("no daemon connected")) → svcerr.Unavailable("no daemon connected")`) || len(third.Rewrites) != 1 {
		t.Errorf("after the reader moved to svcerr.Code, want blocked's wrapper unwrapped and nothing else, got:\n%s", got)
	}

	goCmd(t, dir, "vet", "./...")
	if after := wireLines(t, dir); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("the rewrite changed what a client reads.\nafter:\n%s\n\nbefore:\n%s", strings.Join(after, "\n"), strings.Join(before, "\n"))
	}
}

// wireLines runs the corpus's tests (TestCodes included, which must pass
// before and after) and returns wiretest's WIRE lines.
func wireLines(t *testing.T, dir string) []string {
	t.Helper()
	out := goCmd(t, dir, "test", "-count=1", "-v", "./...")
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "WIRE ") {
			lines = append(lines, sc.Text())
		}
	}
	return lines
}

// copyTree copies the fixture's .go sources (not its goldens) under dst.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return err
		}
		copyFile(t, path, to)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFormatText(t *testing.T) {
	cases := []struct {
		format string
		lit    string
		verbs  int
		hasW   bool
	}{
		{"plain", "plain", 0, false},
		{"%s", "", 1, false},
		{"%s not found", " not found", 1, false},
		{"100%% used", "100% used", 0, false},
		{"%-8.3f|%[1]d|%*d", "||", 3, false},
		{"push: %w", "push: ", 1, true},
		{"%+v", "", 1, false},
		{"trailing %", "trailing ", 1, false},
	}
	for _, tc := range cases {
		lit, verbs, hasW := formatText(tc.format)
		if lit != tc.lit || verbs != tc.verbs || hasW != tc.hasW {
			t.Errorf("formatText(%q) = %q, %d, %v; want %q, %d, %v", tc.format, lit, verbs, hasW, tc.lit, tc.verbs, tc.hasW)
		}
	}
}

func TestIsHandlerFile(t *testing.T) {
	for rel, want := range map[string]bool{
		"internal/handlers/chat/chat.go":  true,
		"handlers/x.go":                   true,
		"internal/handlersx/x.go":         false,
		"internal/grpc/interceptors/a.go": false,
		"main.go":                         false,
	} {
		if got := isHandlerFile(rel); got != want {
			t.Errorf("isHandlerFile(%q) = %v, want %v", rel, got, want)
		}
	}
}

// A project that never imports connect is not loaded at all.
func TestConnectErrors_NothingToDoWithoutConnect(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"go.mod":  "module example.com/plain\n\ngo 1.27.0\n",
		"main.go": "package main\n\nimport \"errors\"\n\nvar ErrX = errors.New(\"NewError CodeOf\")\n",
	})
	rep, err := runConnectErrors(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Changed() || len(rep.Next) != 0 {
		t.Errorf("rewrote a project that does not use connect: %+v", rep)
	}
}
