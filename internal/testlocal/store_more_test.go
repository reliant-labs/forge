package testlocal

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/testreport"
)

func TestRunSourceAndURL(t *testing.T) {
	tests := []struct {
		name       string
		ci         *testreport.ManifestCI
		wantSource string
		wantURL    string
	}{
		{"no CI identity is a local run", nil, "local", ""},
		{"a CI identity with no provider is still local", &testreport.ManifestCI{RunURL: "https://ci/run/1"}, "local", "https://ci/run/1"},
		{"a provider makes it a CI run", &testreport.ManifestCI{Provider: "github", RunURL: "https://ci/run/2"}, "ci", "https://ci/run/2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Run{}
			r.CI = tt.ci
			if got := r.Source(); got != tt.wantSource {
				t.Errorf("Source = %q, want %q", got, tt.wantSource)
			}
			if got := r.URL(); got != tt.wantURL {
				t.Errorf("URL = %q, want %q", got, tt.wantURL)
			}
		})
	}
}

func TestLaneResultsPath(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lane := func(results testreport.LaneResults, file string) *Lane {
		l := &Lane{Dir: dir}
		l.Results, l.ResultsFile = results, file
		return l
	}

	if got := lane(testreport.LaneResultsMissing, "").ResultsPath(); got != "" {
		t.Errorf("a lane that recorded no results has no path, got %q", got)
	}
	if got := lane(testreport.LaneResultsParsed, "").ResultsPath(); got != "" {
		t.Errorf("the conventional file is absent, got %q", got)
	}
	write(testreport.LaneResultsFile)
	if got, want := lane(testreport.LaneResultsParsed, "").ResultsPath(), filepath.Join(dir, testreport.LaneResultsFile); got != want {
		t.Errorf("conventional name: got %q, want %q", got, want)
	}
	write("junit.xml")
	if got, want := lane(testreport.LaneResultsParsed, "junit.xml").ResultsPath(), filepath.Join(dir, "junit.xml"); got != want {
		t.Errorf("recorded name: got %q, want %q", got, want)
	}
	if err := os.Mkdir(filepath.Join(dir, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := lane(testreport.LaneResultsParsed, "adir").ResultsPath(); got != "" {
		t.Errorf("a directory is not a results file, got %q", got)
	}
	// A recorded name wins even when the lane says its results are missing.
	if got := lane(testreport.LaneResultsMissing, "junit.xml").ResultsPath(); got == "" {
		t.Error("an explicit results file must be honoured")
	}
}

func TestLaneCoverageInputs(t *testing.T) {
	dir := t.TempDir()
	mk := func(rel string, size int) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.Repeat("x", size)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l := &Lane{Dir: dir}
	if got := l.CoverageInputs(); len(got) != 0 {
		t.Errorf("an empty lane has no coverage, got %+v", got)
	}

	mk(testreport.LaneCoverProfile, 10)
	mk("lcov.info", 0) // an empty file is not coverage
	mk("coverage-final.json", 5)
	mk("cobertura-coverage.xml", 5)
	if err := os.MkdirAll(filepath.Join(dir, testreport.LaneCoverDataDir), 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory where a file is expected (and vice versa) does not count.
	if err := os.MkdirAll(filepath.Join(dir, "lcov.info.d"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := l.CoverageInputs()
	var formats []string
	for _, c := range got {
		formats = append(formats, c.Format)
	}
	want := []string{testreport.CoverageGoProfile, testreport.CoverageGoCovdata, testreport.CoverageIstanbul, testreport.CoverageCobertura}
	if !reflect.DeepEqual(formats, want) {
		t.Errorf("formats = %v, want %v (empty lcov.info skipped, covdata found as a directory)", formats, want)
	}

	declared := &Lane{Dir: dir}
	declared.Coverage = []testreport.LaneCoverageInput{{Path: "only.out", Format: testreport.CoverageGoProfile}}
	if got := declared.CoverageInputs(); len(got) != 1 || got[0].Path != "only.out" {
		t.Errorf("capture.json's list must win over probing, got %+v", got)
	}
}

func TestOpen_Variants(t *testing.T) {
	t.Run("a run with no run.json but lanes is read with a problem", func(t *testing.T) {
		root := t.TempDir()
		dir := fxWriteRun(t, root, fxRun{id: "01NOJSON", created: time.Now(), commit: fxCommit, lanes: []fxLane{{name: "unit", results: goJSON("p:TestA:pass")}}})
		if err := os.Remove(filepath.Join(dir, testreport.RunManifestFile)); err != nil {
			t.Fatal(err)
		}
		r, err := Open(dir)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if r.ID != "01NOJSON" {
			t.Errorf("ID = %q, want the directory name", r.ID)
		}
		if len(r.Lanes) != 1 {
			t.Errorf("lanes = %d, want 1", len(r.Lanes))
		}
		if len(r.Problems) != 1 || !strings.Contains(r.Problems[0], "no run.json") {
			t.Errorf("problems = %v, want the missing run.json named", r.Problems)
		}
		if r.CreatedAt.IsZero() {
			t.Error("CreatedAt must fall back to the directory's modification time")
		}
	})

	t.Run("a directory with nothing in it is not a run", func(t *testing.T) {
		_, err := Open(t.TempDir())
		if !errors.Is(err, ErrNotRun) {
			t.Errorf("Open = %v, want ErrNotRun", err)
		}
	})

	t.Run("an unreadable lane is a problem, not a failure", func(t *testing.T) {
		root := t.TempDir()
		dir := fxWriteRun(t, root, fxRun{id: "01BADLANE", created: time.Now(), commit: fxCommit, lanes: []fxLane{
			{name: "unit", results: goJSON("p:TestA:pass")},
			{name: "broken"},
		}})
		if err := os.WriteFile(filepath.Join(dir, "broken", testreport.LaneManifestFile), []byte("{nope"), 0o644); err != nil {
			t.Fatal(err)
		}
		r, err := Open(dir)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if len(r.Lanes) != 1 || r.Lanes[0].Lane != "unit" {
			t.Errorf("lanes = %+v, want only the readable one", r.Lanes)
		}
		if len(r.Problems) != 1 || !strings.Contains(r.Problems[0], "lane broken") {
			t.Errorf("problems = %v", r.Problems)
		}
	})

	t.Run("a relative path is made absolute", func(t *testing.T) {
		root := t.TempDir()
		fxWriteRun(t, root, fxRun{id: "01REL", created: time.Now(), commit: fxCommit, lanes: []fxLane{{name: "unit", results: goJSON("p:TestA:pass")}}})
		t.Chdir(root)
		r, err := Open(filepath.Join(".forge", "test-runs", "01REL"))
		if err != nil {
			// The runs root is a testreport detail: fall back to what it names.
			r, err = Open(strings.TrimPrefix(filepath.Join(testreport.RunsRoot("."), "01REL"), "./"))
		}
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if !filepath.IsAbs(r.Dir) {
			t.Errorf("Dir = %q, want an absolute path", r.Dir)
		}
	})
}

func TestList_Problems(t *testing.T) {
	if runs, problems, err := List(t.TempDir()); err != nil || runs != nil || problems != nil {
		t.Errorf("a project with no runs = (%v, %v, %v), want nothing", runs, problems, err)
	}

	root := t.TempDir()
	fxWriteRun(t, root, fxRun{id: "01GOOD", created: time.Now(), commit: fxCommit, lanes: []fxLane{{name: "unit", results: goJSON("p:TestA:pass")}}})
	if err := os.MkdirAll(filepath.Join(testreport.RunsRoot(root), "01EMPTY"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(testreport.RunsRoot(root), "stray-file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runs, problems, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != "01GOOD" {
		t.Errorf("runs = %v, want only the good run", runs)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "run 01EMPTY") {
		t.Errorf("problems = %v, want the empty directory named and the stray file ignored", problems)
	}
}

func TestSortNewestFirst(t *testing.T) {
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	mk := func(id string, at time.Time) *Run {
		r := &Run{}
		r.ID, r.CreatedAt = id, at
		return r
	}
	runs := []*Run{mk("01A", t0), mk("01C", t0.Add(time.Hour)), mk("01B", t0), mk("01D", t0.Add(-time.Hour))}
	SortNewestFirst(runs)
	var ids []string
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	if want := "01C,01B,01A,01D"; strings.Join(ids, ",") != want {
		t.Errorf("order = %v, want %s (newest first; equal times by descending id)", ids, want)
	}
}

func TestResolve_Paths(t *testing.T) {
	root := t.TempDir()
	dir := fxWriteRun(t, root, fxRun{id: "01ABCD", created: time.Now(), commit: fxCommit, lanes: []fxLane{{name: "unit", results: goJSON("p:TestA:pass")}}})

	t.Run("a directory path opens that run directly", func(t *testing.T) {
		r, err := Resolve(root, dir, nil)
		if err != nil || r.ID != "01ABCD" {
			t.Errorf("Resolve(path) = (%v, %v)", r, err)
		}
	})

	runs, _, _ := List(root)
	t.Run("an exact id wins over prefixes", func(t *testing.T) {
		a, b := &Run{}, &Run{}
		a.ID, b.ID = "01AB", "01ABCD"
		got, err := Resolve(root, "01AB", []*Run{b, a})
		if err != nil || got != a {
			t.Errorf("Resolve = (%v, %v), want the exact id", got, err)
		}
	})

	t.Run("an unknown ref names where it looked", func(t *testing.T) {
		_, err := Resolve(root, "zzz", runs)
		if err == nil || !strings.Contains(err.Error(), `no run "zzz"`) || !strings.Contains(err.Error(), testreport.RunsRoot(root)) {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("an ambiguous prefix says how many", func(t *testing.T) {
		a, b := &Run{}, &Run{}
		a.ID, b.ID = "01XA", "01XB"
		_, err := Resolve(root, "01x", []*Run{a, b})
		if err == nil || !strings.Contains(err.Error(), "ambiguous: 2 runs") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestMatchCommit(t *testing.T) {
	r := &Run{}
	r.Commit = "ABCDEF0123456789"
	tests := []struct {
		sha  string
		want bool
	}{
		{"abcdef01", true},
		{"  ABCD  ", true},
		{"abcd", true},
		{"abc", false}, // under 4 characters is not a prefix anyone should match on
		{"", false},
		{"ffff", false},
	}
	for _, tt := range tests {
		if got := MatchCommit(r, tt.sha); got != tt.want {
			t.Errorf("MatchCommit(%q) = %v, want %v", tt.sha, got, tt.want)
		}
	}
	if MatchCommit(&Run{}, "abcd") {
		t.Error("a run with no commit matches nothing")
	}
}

func TestTreeRuns_Variants(t *testing.T) {
	mk := func(id, commit, tree string, dirty bool) *Run {
		r := &Run{}
		r.ID, r.Commit, r.TreeHash, r.Dirty = id, commit, tree, dirty
		return r
	}
	clean1 := mk("c1", fxCommit, fxTree, false)
	clean2 := mk("c2", fxCommit, fxTree, false)
	dirtyA := mk("d1", fxCommit, "treeA", true)
	dirtyA2 := mk("d2", fxCommit, "treeA", true)
	dirtyB := mk("d3", fxCommit, "treeB", true)
	noTree := mk("d4", fxCommit, "", true)
	all := []*Run{clean1, clean2, dirtyA, dirtyA2, dirtyB, noTree}

	ids := func(rs []*Run) string {
		var s []string
		for _, r := range rs {
			s = append(s, r.ID)
		}
		return strings.Join(s, ",")
	}
	if got := ids(TreeRuns(all, clean1)); got != "c1,c2" {
		t.Errorf("a clean run's tree is its commit's clean runs, got %s", got)
	}
	if got := ids(TreeRuns(all, dirtyA)); got != "d1,d2" {
		t.Errorf("a dirty run's tree is the runs on the same tree hash, got %s", got)
	}
	if got := ids(TreeRuns(all, noTree)); got != "d4" {
		t.Errorf("a dirty run with an unknown tree stands alone, got %s", got)
	}
	orphan := mk("o", "9999999999999999999999999999999999999999", fxTree, false)
	if got := ids(TreeRuns(all, orphan)); got != "o" {
		t.Errorf("a clean run whose commit has no other clean run stands alone, got %s", got)
	}
	lonelyDirty := mk("ld", fxCommit, "other", true)
	if got := ids(TreeRuns([]*Run{clean1}, lonelyDirty)); got != "ld" {
		t.Errorf("a dirty run with no peers stands alone, got %s", got)
	}
}

func TestGitHelpers(t *testing.T) {
	ctx := context.Background()
	g := &fakeGit{top: "/repo", head: fxCommit, blobs: map[string]string{"a.go": "aaa", "dir/b.go": "bbb"}}

	if got, err := Toplevel(ctx, g.run, "/repo/x"); err != nil || got != "/repo" {
		t.Errorf("Toplevel = (%q, %v)", got, err)
	}
	if _, err := Toplevel(ctx, (&fakeGit{}).run, "/nowhere"); err == nil {
		t.Error("Toplevel outside a repository must fail")
	}

	ids, err := BlobIDs(ctx, g.run, "/repo", fxTree)
	if err != nil || !reflect.DeepEqual(ids, g.blobs) {
		t.Errorf("BlobIDs = (%v, %v), want %v", ids, err, g.blobs)
	}
	if _, err := BlobIDs(ctx, g.run, "/repo", "badtree"); err == nil {
		t.Error("an unknown tree must fail")
	}

	if got, err := MergeBase(ctx, g.run, "/repo", "a", "b"); err != nil || got != fxBase {
		t.Errorf("MergeBase = (%q, %v)", got, err)
	}
	if got, err := ResolveRev(ctx, g.run, "/repo", "HEAD"); err != nil || got != fxCommit {
		t.Errorf("ResolveRev = (%q, %v)", got, err)
	}
	if _, err := ResolveRev(ctx, g.run, "/repo", "nope"); err == nil {
		t.Error("an unknown revision must fail")
	}
	if got := DefaultBranch(ctx, g.run, "/repo"); got != "origin/main" {
		t.Errorf("DefaultBranch = %q", got)
	}
}

func TestDefaultBranch_Fallbacks(t *testing.T) {
	ctx := context.Background()
	known := func(have ...string) Git {
		return func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if args[0] == "symbolic-ref" {
				return nil, errors.New("no origin/HEAD")
			}
			rev := strings.TrimSuffix(args[len(args)-1], "^{commit}")
			for _, h := range have {
				if h == rev {
					return []byte(fxBase + "\n"), nil
				}
			}
			return nil, errors.New("unknown revision")
		}
	}
	tests := []struct {
		have []string
		want string
	}{
		{[]string{"origin/master", "main"}, "origin/master"},
		{[]string{"master"}, "master"},
		{[]string{"main", "master"}, "main"},
		{nil, ""},
	}
	for _, tt := range tests {
		if got := DefaultBranch(ctx, known(tt.have...), "/repo"); got != tt.want {
			t.Errorf("with %v: DefaultBranch = %q, want %q", tt.have, got, tt.want)
		}
	}

	empty := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] == "symbolic-ref" {
			return []byte("\n"), nil
		}
		return nil, errors.New("unknown")
	}
	if got := DefaultBranch(ctx, empty, "/repo"); got != "" {
		t.Errorf("an empty symbolic-ref answer must fall through, got %q", got)
	}
}

func TestIndexBlobIDs(t *testing.T) {
	ctx := context.Background()
	script := func(diff []byte, diffErr error) Git {
		return func(_ context.Context, _ string, args ...string) ([]byte, error) {
			switch args[0] {
			case "ls-files":
				return []byte("100644 aaa 0\ta.go\x00100644 bbb 0\tdir/b.go\x00100644 ccc 0\tgone.go\x00bad record\x00"), nil
			case "diff":
				return diff, diffErr
			}
			return nil, errors.New("unexpected")
		}
	}
	hash := func(rel string) (string, bool) {
		switch rel {
		case "a.go":
			return "a-edited", true
		case "gone.go":
			return "", false // deleted in the worktree
		case "new.go":
			return "new-id", true
		}
		return "", false
	}

	got, err := IndexBlobIDs(ctx, script([]byte("a.go\x00gone.go\x00new.go\x00\x00"), nil), "/repo", hash)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a.go": "a-edited", "dir/b.go": "bbb", "new.go": "new-id"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("IndexBlobIDs = %v, want %v", got, want)
	}

	// No HEAD yet: the index is all there is, and that is not an error.
	got, err = IndexBlobIDs(ctx, script(nil, errors.New("bad revision HEAD")), "/repo", hash)
	if err != nil || got["a.go"] != "aaa" || len(got) != 3 {
		t.Errorf("without HEAD = (%v, %v), want the raw index", got, err)
	}

	failing := func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("not a repo") }
	if _, err := IndexBlobIDs(ctx, failing, "/repo", hash); err == nil {
		t.Error("an unreadable index must fail")
	}
}

func TestChangedLines(t *testing.T) {
	ctx := context.Background()
	var gotArgs []string
	git := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte("diff --git a/x.go b/x.go\n+++ b/x.go\n@@ -1,0 +2,3 @@\n"), nil
	}
	files, err := ChangedLines(ctx, git, "/repo", "base", "head")
	if err != nil || len(files) != 1 || files[0].Path != "x.go" || files[0].Ranges[0] != (testreport.LineRange{Start: 2, End: 4}) {
		t.Fatalf("ChangedLines = (%+v, %v)", files, err)
	}
	if last := gotArgs[len(gotArgs)-1]; last != "base...head" {
		t.Errorf("a commit diff must be the three-dot range, got %q", last)
	}
	if _, err := ChangedLines(ctx, git, "/repo", "base", ""); err != nil {
		t.Fatal(err)
	}
	if last := gotArgs[len(gotArgs)-1]; last != "base" {
		t.Errorf("a worktree diff is against base alone, got %q", last)
	}
	failing := func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("bad object") }
	if _, err := ChangedLines(ctx, failing, "/repo", "b", "h"); err == nil {
		t.Error("a failing diff must be reported")
	}
}

func TestParseUnifiedZero_Details(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/a.go b/a.go",
		"+++ b/a.go",
		"@@ -1 +1 @@",     // single line, count omitted
		"@@ -5,2 +9,0 @@", // a pure deletion adds nothing
		"@@ -7,0 +20,2 @@",
		"diff --git a/gone.go b/gone.go",
		"+++ /dev/null", // a deleted file
		"@@ -1,3 +0,0 @@",
		"diff --git a/spaced name.go b/spaced name.go",
		`+++ "b/q\"uote.go"`,
		"@@ -0,0 +1,2 @@",
		"diff --git a/only-deletions.go b/only-deletions.go",
		"+++ b/only-deletions.go",
		"@@ -3,2 +2,0 @@",
		"@@ malformed",
	}, "\n")
	got := ParseUnifiedZero([]byte(diff))
	want := []testreport.ChangedFile{
		{Path: "a.go", Ranges: []testreport.LineRange{{Start: 1, End: 1}, {Start: 20, End: 21}}},
		{Path: `q"uote.go`, Ranges: []testreport.LineRange{{Start: 1, End: 2}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseUnifiedZero = %+v, want %+v", got, want)
	}
	if got := ParseUnifiedZero(nil); len(got) != 0 {
		t.Errorf("an empty diff = %+v", got)
	}
}

func TestExecGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git init: %v\n%s", err, out)
	}
	out, err := ExecGit(ctx, dir, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Errorf("ExecGit = (%q, %v)", out, err)
	}

	_, err = ExecGit(ctx, dir, "rev-parse", "--verify", "--quiet", "no-such-ref^{commit}")
	if err == nil || !strings.Contains(err.Error(), "git rev-parse") {
		t.Errorf("a failing git must name the command, got %v", err)
	}
	_, err = ExecGit(ctx, dir, "cat-file", "-p", strings.Repeat("0", 40))
	if err == nil || !strings.Contains(err.Error(), "git cat-file") {
		t.Errorf("err = %v", err)
	}
	if _, err := ExecGit(ctx, filepath.Join(dir, "no-such-dir"), "status"); err == nil {
		t.Error("a missing working directory must fail")
	}
}
