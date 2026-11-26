package testretry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestScan(t *testing.T) {
	dir := t.TempDir()

	t.Run("visits every event in order", func(t *testing.T) {
		p := eventPath(t, event{Action: "start"}, event{Action: "run", Test: "TestA"})
		var got []string
		if err := scan(p, func(ev event) error { got = append(got, ev.Action); return nil }); err != nil {
			t.Fatal(err)
		}
		if strings.Join(got, ",") != "start,run" {
			t.Errorf("visited %v", got)
		}
	})

	t.Run("a visitor error stops the scan and is returned", func(t *testing.T) {
		p := eventPath(t, event{Action: "start"}, event{Action: "run", Test: "TestA"})
		stop := errors.New("enough")
		n := 0
		err := scan(p, func(event) error { n++; return stop })
		if !errors.Is(err, stop) || n != 1 {
			t.Errorf("err = %v after %d events, want the visitor's error after 1", err, n)
		}
	})

	t.Run("an empty stream has no events", func(t *testing.T) {
		if err := scan(writeFile(t, filepath.Join(dir, "empty"), ""), func(event) error { t.Error("visited"); return nil }); err != nil {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("a missing file", func(t *testing.T) {
		if err := scan(filepath.Join(dir, "absent"), func(event) error { return nil }); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("err = %v, want ErrNotExist", err)
		}
	})

	t.Run("a directory is not a regular event stream", func(t *testing.T) {
		if err := scan(dir, func(event) error { return nil }); err == nil || !strings.Contains(err.Error(), "unsupported Go event stream") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("a line that is not JSON", func(t *testing.T) {
		p := writeFile(t, filepath.Join(dir, "junk"), "not json\n")
		if err := scan(p, func(event) error { return nil }); err == nil || !strings.Contains(err.Error(), "invalid Go event") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("a final line with no newline is an incomplete event", func(t *testing.T) {
		p := writeFile(t, filepath.Join(dir, "cut"), `{"Action":"start","Package":"x"}`)
		if err := scan(p, func(event) error { return nil }); err == nil || !strings.Contains(err.Error(), "incomplete or oversized Go event") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("a line over the size limit", func(t *testing.T) {
		p := writeFile(t, filepath.Join(dir, "huge"), strings.Repeat("x", maxLine+10)+"\n")
		if err := scan(p, func(event) error { return nil }); err == nil || !strings.Contains(err.Error(), "incomplete or oversized Go event") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestJudge_Rejections(t *testing.T) {
	start := event{Action: "start"}
	run := event{Action: "run", Test: "TestOriginal"}
	pass := event{Action: "pass", Test: "TestOriginal"}
	fail := event{Action: "fail", Test: "TestOriginal"}
	out := event{Action: "output", Test: "TestOriginal", Output: "expected true"}
	selected := map[string]bool{fixturePackage: true}

	cases := []struct {
		name  string
		items []event
		want  string
	}{
		{"an event for a package that was not asked for", []event{start, {Action: "run", Test: "T", Package: "other.example/p"}}, "unselected package"},
		{"a second start for the same package", []event{start, start}, "duplicate or malformed package start"},
		{"a start that names a test", []event{{Action: "start", Test: "TestX"}}, "duplicate or malformed package start"},
		{"a test event before its package started", []event{run}, "event outside active package"},
		{"an event after the package ended", []event{start, {Action: "pass"}, run}, "event outside active package"},
		{"a package that ends with a test still running", []event{start, run, {Action: "pass"}}, "package ended with incomplete tests"},
		{"a package verdict that is not terminal", []event{start, {Action: "bogus"}}, "package ended with incomplete tests"},
		{"pause without a run", []event{start, {Action: "pause", Test: "TestOriginal"}}, "invalid test schedule event"},
		{"cont without a run", []event{start, {Action: "cont", Test: "TestOriginal"}}, "invalid test schedule event"},
		{"a terminal event without a run", []event{start, pass}, "test terminal without a run"},
		{"an action cmd/test2json does not emit", []event{start, run, {Action: "weird", Test: "TestOriginal"}}, "unrecognized Go test action"},
		{"a package that never reported a verdict", []event{start, run, pass}, "missing package verdict"},
		{"no package started at all", nil, "incomplete package evidence"},
		{"every test passed but the package failed", []event{start, run, pass, {Action: "fail"}}, "no isolated failing tests"},
		{"a failing test whose package passed", []event{start, run, out, fail, {Action: "pass"}}, "not an attributable top-level Go test"},
		{"a failure with no output to sign", []event{start, run, fail, {Action: "fail"}}, "not an attributable top-level Go test"},
		{"a lowercase test name is not a top-level test", []event{start, {Action: "run", Test: "helper"}, {Action: "output", Test: "helper", Output: "x"}, {Action: "fail", Test: "helper"}, {Action: "fail"}}, "not an attributable top-level Go test"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := judge(eventPath(t, tc.items...), selected, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("judge = %v, want an error containing %q", err, tc.want)
			}
		})
	}

	t.Run("an exact retry that did not pass", func(t *testing.T) {
		id := identity{fixturePackage, "TestOriginal"}
		_, err := judge(eventPath(t, start, run, out, fail, event{Action: "fail"}), selected, &id)
		if err == nil || !strings.Contains(err.Error(), "exact retry did not run once and pass") {
			t.Errorf("judge = %v", err)
		}
	})

	t.Run("a retry may not run two tests", func(t *testing.T) {
		id := identity{fixturePackage, "TestOriginal"}
		items := []event{start, run, pass, {Action: "run", Test: "TestOriginal2"}, {Action: "pass", Test: "TestOriginal2"}, {Action: "pass"}}
		if _, err := judge(eventPath(t, items...), selected, &id); err == nil {
			t.Error("a retry that ran a second test was accepted")
		}
	})

	t.Run("pause and cont between run and terminal are fine", func(t *testing.T) {
		items := []event{start, run, {Action: "pause", Test: "TestOriginal"}, {Action: "cont", Test: "TestOriginal"}, out, fail, {Action: "fail"}}
		failed, err := judge(eventPath(t, items...), selected, nil)
		if err != nil || len(failed) != 1 {
			t.Errorf("judge = (%+v, %v), want the one failure", failed, err)
		}
	})
}

func TestJudge_TooManyFailures(t *testing.T) {
	items := []event{{Action: "start"}}
	for i := range maxTests + 1 {
		name := "TestFail" + string(rune('A'+i))
		items = append(items,
			event{Action: "run", Test: name},
			event{Action: "output", Test: name, Output: "boom"},
			event{Action: "fail", Test: name})
	}
	items = append(items, event{Action: "fail"})
	// More than one test ran in the package, so it is rejected for the
	// selection change before the count is even considered; both guard the
	// same property: a singleton retry cannot reproduce a crowded package.
	if _, err := judge(eventPath(t, items...), map[string]bool{fixturePackage: true}, nil); err == nil {
		t.Fatal("a run with more failures than the retry budget was accepted")
	}

	// Across packages each running one test, the budget itself is what refuses.
	var multi []event
	selected := map[string]bool{}
	for i := range maxTests + 1 {
		pkg := fixturePackage + string(rune('a'+i))
		selected[pkg] = true
		multi = append(multi,
			event{Action: "start", Package: pkg},
			event{Action: "run", Test: "TestOriginal", Package: pkg},
			event{Action: "output", Test: "TestOriginal", Package: pkg, Output: "boom"},
			event{Action: "fail", Test: "TestOriginal", Package: pkg},
			event{Action: "fail", Package: pkg})
	}
	_, err := judge(eventPath(t, multi...), selected, nil)
	if err == nil || !strings.Contains(err.Error(), "too many failures") {
		t.Errorf("judge = %v, want the retry budget refusal", err)
	}
}

func TestJudge_FailuresAreSortedAndSigned(t *testing.T) {
	selected := map[string]bool{}
	var items []event
	for _, pkg := range []string{"z", "a"} {
		p := fixturePackage + "/" + pkg
		selected[p] = true
		items = append(items,
			event{Action: "start", Package: p},
			event{Action: "run", Test: "TestOriginal", Package: p},
			event{Action: "output", Test: "TestOriginal", Package: p, Output: "failed in " + pkg},
			event{Action: "fail", Test: "TestOriginal", Package: p},
			event{Action: "fail", Package: p})
	}
	failed, err := judge(eventPath(t, items...), selected, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 2 || failed[0].pkg != fixturePackage+"/a" || failed[1].pkg != fixturePackage+"/z" {
		t.Fatalf("failures = %+v, want sorted by package", failed)
	}
	if failed[0].signature != "failed in a" {
		t.Errorf("signature = %q, want the captured output", failed[0].signature)
	}
}

func TestJudge_AFailedPackageWithNoFailingTestBesideAFailedTest(t *testing.T) {
	other := fixturePackage + "/other"
	items := []event{
		{Action: "start"}, {Action: "run", Test: "TestOriginal"},
		{Action: "output", Test: "TestOriginal", Output: "boom"},
		{Action: "fail", Test: "TestOriginal"}, {Action: "fail"},
		// A second package failed without any test failing (a TestMain exit).
		{Action: "start", Package: other}, {Action: "run", Test: "TestFine", Package: other},
		{Action: "pass", Test: "TestFine", Package: other}, {Action: "fail", Package: other},
	}
	_, err := judge(eventPath(t, items...), map[string]bool{fixturePackage: true, other: true}, nil)
	if err == nil || !strings.Contains(err.Error(), "package failure has no test cause") {
		t.Errorf("judge = %v, want the unexplained package failure to keep the run red", err)
	}
}

func TestJudge_OutputIsBounded(t *testing.T) {
	long := strings.Repeat("y", 1000)
	items := []event{{Action: "start"}, {Action: "run", Test: "TestOriginal"}}
	for range 20 {
		items = append(items, event{Action: "output", Test: "TestOriginal", Output: long})
	}
	items = append(items, event{Action: "fail", Test: "TestOriginal"}, event{Action: "fail"})
	failed, err := judge(eventPath(t, items...), map[string]bool{fixturePackage: true}, nil)
	if err != nil || len(failed) != 1 {
		t.Fatalf("judge = (%+v, %v)", failed, err)
	}
	if n := len(failed[0].signature); n > 3500+400 {
		t.Errorf("signature is %d bytes; the evidence kept per test must stay bounded", n)
	}
}

func TestPackageSet(t *testing.T) {
	const mod = "example.com/fixture"
	got, err := packageSet(mod, []string{".", "./a", "./a/b", mod + "/c"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{mod, mod + "/a", mod + "/a/b", mod + "/c"} {
		if !got[want] {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	if len(got) != 4 {
		t.Errorf("got %v", got)
	}

	bad := map[string][]string{
		"no packages":        nil,
		"a duplicate":        {"./a", "./a"},
		"dot and its module": {".", mod},
		"a wildcard":         {"./a/..."},
		"a parent element":   {"./a/../b"},
		"a dot element":      {"./a/./b"},
		"another module":     {"other.example/x"},
		"a prefix lookalike": {mod + "x/y"},
		"an option":          {"-race"},
		"a space":            {"./a b"},
		"an empty name":      {""},
	}
	for name, args := range bad {
		t.Run(name, func(t *testing.T) {
			if got, err := packageSet(mod, args); err == nil {
				t.Errorf("admitted %q as %v", args, got)
			}
		})
	}
}

func TestFileDigest(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, filepath.Join(dir, "f"), "abc")
	got, err := fileDigest(p)
	if err != nil {
		t.Fatal(err)
	}
	// sha256("abc")
	if want := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"; got != want {
		t.Errorf("digest = %s, want %s", got, want)
	}
	if _, err := fileDigest(filepath.Join(dir, "absent")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want ErrNotExist", err)
	}
	if _, err := fileDigest(dir); err == nil {
		t.Error("a directory cannot be digested")
	}
}

func TestFreshPath(t *testing.T) {
	dir := t.TempDir()
	if err := freshPath(filepath.Join(dir, "new.json")); err != nil {
		t.Errorf("a new absolute path was refused: %v", err)
	}
	if err := freshPath("relative.json"); err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Errorf("a relative path = %v", err)
	}
	existing := writeFile(t, filepath.Join(dir, "old.json"), "x")
	if err := freshPath(existing); err == nil || !strings.Contains(err.Error(), "must not exist") {
		t.Errorf("an existing file = %v", err)
	}
	if err := freshPath(dir); err == nil {
		t.Error("an existing directory was accepted")
	}
}

func TestWriteReport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.json")
	want := Report{Schema: Schema, Repository: "acme/lib", Head: "h", PR: 7, Tests: []Test{{Package: "p", Test: "TestA", RetrySHA256: "r"}}}
	if err := writeReport(path, want); err != nil {
		t.Fatal(err)
	}
	var got Report
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the report is not JSON: %v", err)
	}
	if got.Repository != want.Repository || got.PR != 7 || len(got.Tests) != 1 || got.Tests[0].RetrySHA256 != "r" {
		t.Errorf("report = %+v", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("the temporary file was left behind: %v", entries)
	}

	// The report is published by hard link: it never replaces an existing one.
	if err := writeReport(path, want); err == nil {
		t.Error("an existing report was overwritten")
	}
	if err := writeReport(filepath.Join(dir, "missing", "result.json"), want); err == nil {
		t.Error("writing into a directory that does not exist must fail")
	}
}

func TestObservedWriter(t *testing.T) {
	var sink bytes.Buffer
	w := &observedWriter{target: &sink}
	if _, err := w.Write(nil); err != nil || w.written {
		t.Errorf("an empty write must not count as output (written=%v, err=%v)", w.written, err)
	}
	if n, err := w.Write([]byte("warn")); err != nil || n != 4 || !w.written || sink.String() != "warn" {
		t.Errorf("Write = (%d, %v), written=%v, sink=%q", n, err, w.written, sink.String())
	}
	if _, err := w.Write(nil); err != nil || !w.written {
		t.Error("written must stay set once anything was written")
	}
}

func validConfig(t *testing.T) Config {
	t.Helper()
	sha := strings.Repeat("a", 40)
	return Config{
		Root: t.TempDir(), Repository: "acme/lib", Base: sha, Head: strings.Repeat("b", 40),
		RunID: "run-1", PR: 3, Timeout: time.Minute, Packages: []string{"./pkg"},
		JSONPath: filepath.Join(t.TempDir(), "e.ndjson"), ReportPath: filepath.Join(t.TempDir(), "r.json"),
	}
}

func TestRun_RefusesInvalidArguments(t *testing.T) {
	mutations := map[string]func(*Config){
		"a repository that is not owner/name": func(c *Config) { c.Repository = "lib" },
		"a repository with a shell character": func(c *Config) { c.Repository = "acme/lib;rm" },
		"a base that is not a full sha":       func(c *Config) { c.Base = "abc" },
		"a head that is not a full sha":       func(c *Config) { c.Head = strings.Repeat("G", 40) },
		"a run id with a space":               func(c *Config) { c.RunID = "run 1" },
		"an empty run id":                     func(c *Config) { c.RunID = "" },
		"a negative PR":                       func(c *Config) { c.PR = -1 },
		"a negative parallelism":              func(c *Config) { c.Parallel = -1 },
		"parallelism over the cap":            func(c *Config) { c.Parallel = 257 },
		"no timeout":                          func(c *Config) { c.Timeout = 0 },
		"a timeout over ten minutes":          func(c *Config) { c.Timeout = 11 * time.Minute },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig(t)
			mutate(&cfg)
			err := Run(context.Background(), cfg)
			if err == nil || !strings.Contains(err.Error(), "invalid merge test gate arguments") {
				t.Errorf("Run = %v", err)
			}
		})
	}

	for _, arg := range []string{"", "-race", "-run=X"} {
		cfg := validConfig(t)
		cfg.Packages = []string{"./pkg", arg}
		err := Run(context.Background(), cfg)
		if err == nil || !strings.Contains(err.Error(), "cannot contain Go test options") {
			t.Errorf("package operand %q: Run = %v", arg, err)
		}
	}
}

func TestRun_RefusesAnExistingReport(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	cfg := validConfig(t)
	cfg.ReportPath = writeFile(t, cfg.ReportPath, "stale")
	if err := Run(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "must not exist") {
		t.Errorf("Run = %v, want the existing report refused before anything runs", err)
	}
	if _, err := os.Stat(cfg.JSONPath); !os.IsNotExist(err) {
		t.Errorf("the gate ran despite a stale report (stat err = %v)", err)
	}
}

func TestRun_GreenRunNeedsNoRetry(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns go test against an isolated module; runs in task test")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/green\n\ngo 1.21\n")
	writeFile(t, filepath.Join(root, "ok", "ok_test.go"), "package ok\nimport \"testing\"\nfunc TestOK(t *testing.T) {}\n")
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "")

	var stdout bytes.Buffer
	cfg := validConfig(t)
	cfg.Root = root
	cfg.Packages = []string{"./ok"}
	cfg.Echo = true
	cfg.Stdout = &stdout
	cfg.Parallel = 1
	cfg.NoBuildVCS = true
	if err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run on a green package = %v", err)
	}
	if !strings.Contains(stdout.String(), `"Action":"pass"`) {
		t.Errorf("-json events were not echoed to Stdout: %q", stdout.String())
	}
	if _, err := os.Stat(cfg.ReportPath); !os.IsNotExist(err) {
		t.Errorf("a green run must not write a retry report (stat err = %v)", err)
	}
	if b, err := os.ReadFile(cfg.JSONPath); err != nil || !bytes.Contains(b, []byte(`"Action":"pass"`)) {
		t.Errorf("the event stream was not kept at JSONPath: %v", err)
	}
}

func TestRun_FailureStaysRedWhenNotEligible(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns go test against an isolated module; runs in task test")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/red\n\ngo 1.21\n")
	writeFile(t, filepath.Join(root, "bad", "bad_test.go"), "package bad\nimport \"testing\"\nfunc TestBad(t *testing.T) { t.Fatal(\"boom\") }\n")
	t.Setenv("GOWORK", "off")

	t.Run("flags that change what a rerun would mean", func(t *testing.T) {
		t.Setenv("GOFLAGS", "-race")
		cfg := validConfig(t)
		cfg.Root = root
		cfg.Packages = []string{"./bad"}
		var diag bytes.Buffer
		cfg.Stderr = &diag
		err := Run(context.Background(), cfg)
		if err == nil || !strings.Contains(err.Error(), "original failure stands (not an ordinary non-race invocation)") {
			t.Fatalf("Run = %v, want the original failure to stand", err)
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Errorf("the original go test exit error must stay reachable, got %v", err)
		}
		if strings.Contains(diag.String(), "checking") {
			t.Errorf("an ineligible run was retried: %s", diag.String())
		}
	})

	t.Run("an ordinary run in a checkout that is not the gated commit", func(t *testing.T) {
		t.Setenv("GOFLAGS", "-trimpath")
		cfg := validConfig(t)
		cfg.Root = root
		cfg.Packages = []string{"./bad"}
		err := Run(context.Background(), cfg)
		if err == nil || !strings.Contains(err.Error(), "original failure stands") {
			t.Fatalf("Run = %v, want the original failure to stand", err)
		}
		if _, statErr := os.Stat(cfg.ReportPath); !os.IsNotExist(statErr) {
			t.Errorf("a failed recovery left a report (stat err = %v)", statErr)
		}
	})
}

func TestRun_GoToolMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := Run(context.Background(), validConfig(t)); err == nil {
		t.Error("Run without a go tool on PATH must fail")
	}
}

func TestInvoke_ReportsStderrAndKeepsEvidence(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	root := t.TempDir()
	dir := t.TempDir()
	goExe, _ := exec.LookPath("go")

	cfg := Config{Root: root, Stderr: io.Discard, Stdout: io.Discard}
	out := filepath.Join(dir, "version.out")
	noisy, err := invoke(context.Background(), cfg, goExe, os.Environ(), []string{"version"}, out, true)
	if err != nil || noisy {
		t.Fatalf("invoke go version = (%v, %v)", noisy, err)
	}
	if b, _ := os.ReadFile(out); !strings.HasPrefix(string(b), "go version") {
		t.Errorf("stdout was not captured: %q", b)
	}

	// The evidence file is created exclusively: an existing one is never reused.
	if _, err := invoke(context.Background(), cfg, goExe, os.Environ(), []string{"version"}, out, false); err == nil {
		t.Error("invoke overwrote existing evidence")
	}

	noisy, err = invoke(context.Background(), cfg, goExe, os.Environ(), []string{"definitely-not-a-go-command"}, filepath.Join(dir, "bad.out"), false)
	if err == nil || !noisy {
		t.Errorf("a failing command that wrote to stderr = (noisy=%v, err=%v), want both", noisy, err)
	}
}

func TestIsGoTest(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want bool
	}{
		{"a plain test", "package p\nimport \"testing\"\nfunc TestX(t *testing.T) {}", true},
		{"an unnamed parameter", "package p\nimport \"testing\"\nfunc TestX(*testing.T) {}", true},
		{"an aliased testing import", "package p\nimport tt \"testing\"\nfunc TestX(t *tt.T) {}", true},
		{"a benchmark is not a test", "package p\nimport \"testing\"\nfunc TestX(b *testing.B) {}", false},
		{"a value receiver parameter", "package p\nimport \"testing\"\nfunc TestX(t testing.T) {}", false},
		{"two parameters", "package p\nimport \"testing\"\nfunc TestX(t *testing.T, x int) {}", false},
		{"two names in one field", "package p\nimport \"testing\"\nfunc TestX(a, b *testing.T) {}", false},
		{"no parameters", "package p\nfunc TestX() {}", false},
		{"a result", "package p\nimport \"testing\"\nfunc TestX(t *testing.T) int { return 0 }", false},
		{"type parameters", "package p\nimport \"testing\"\nfunc TestX[T any](t *testing.T) {}", false},
		{"a pointer to a local type named T", "package p\ntype T struct{}\nfunc TestX(t *T) {}", false},
		{"a qualifier that is not the testing import", "package p\nimport \"testing\"\nfunc TestX(t *other.T) {}", false},
		{"no testing import at all", "package p\nfunc TestX(t *testing.T) {}", false},
		{"a body-less declaration", "package p\nimport \"testing\"\nfunc TestX(t *testing.T)", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fn, err := testDeclaration([]byte(tt.src), "TestX")
			if tt.want {
				if err != nil || fn == nil {
					t.Fatalf("testDeclaration = (%v, %v), want the declaration", fn, err)
				}
				return
			}
			if err == nil && fn != nil {
				t.Errorf("a non-test declaration was accepted")
			}
		})
	}
}

func TestTestDeclaration_Refusals(t *testing.T) {
	if _, err := testDeclaration([]byte("package p\nfunc ("), "TestX"); err == nil || !strings.Contains(err.Error(), "cannot parse tracked test") {
		t.Errorf("unparseable source = %v", err)
	}
	if _, err := testDeclaration([]byte("package p\nimport \"testing\"\nfunc TestMain(m *testing.M) {}"), "TestX"); err == nil || !strings.Contains(err.Error(), "custom TestMain") {
		t.Errorf("a TestMain = %v", err)
	}
	twice := "package p\nimport \"testing\"\nfunc TestX(t *testing.T) {}\nfunc TestX(t *testing.T) {}"
	if _, err := testDeclaration([]byte(twice), "TestX"); err == nil {
		t.Error("a duplicate declaration was accepted")
	}
	if fn, err := testDeclaration([]byte("package p\nimport \"testing\"\nfunc TestY(t *testing.T) {}"), "TestX"); err != nil || fn != nil {
		t.Errorf("another test's file = (%v, %v), want no match and no error", fn, err)
	}
}
