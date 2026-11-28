package forgecompat

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/testkit"
)

func TestBridgeStringAndUndoByKind(t *testing.T) {
	tests := []struct {
		name       string
		b          Bridge
		wantString string
		wantUndo   string
	}{
		{
			name:       "go.work use",
			b:          Bridge{Dir: "/src/forge", File: "/p/go.work", written: "../forge"},
			wantString: "go.work bridges /src/forge",
			wantUndo:   "go work edit -dropuse=../forge",
		},
		{
			name:       "go.work replace",
			b:          Bridge{Dir: "/src/forge", File: "/p/go.work", Replace: true},
			wantString: "go.work replaces forge with /src/forge",
			wantUndo:   "go work edit -dropreplace=" + ModulePath,
		},
		{
			name:       "go.mod replace",
			b:          Bridge{Dir: "/src/forge", File: "/p/go.mod", Replace: true},
			wantString: "go.mod replaces forge with /src/forge",
			wantUndo:   "go mod edit -dropreplace=" + ModulePath,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.b.String(); got != tt.wantString {
				t.Errorf("String = %q, want %q", got, tt.wantString)
			}
			if got := tt.b.Undo(); got != tt.wantUndo {
				t.Errorf("Undo = %q, want %q", got, tt.wantUndo)
			}
		})
	}
}

func TestShort(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"abc", "abc"},
		{"123456789012", "123456789012"},
		{"1234567890123", "123456789012"},
		{"750833054b78ab216150bdaad19c389cb91cbc53", "750833054b78"},
	}
	for _, tt := range tests {
		if got := short(tt.in); got != tt.want {
			t.Errorf("short(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestBridgeReportLine(t *testing.T) {
	if got := (BridgeReport{}).Line(); got != "" {
		t.Errorf("an unskewed report must say nothing, got %q", got)
	}
	r := BridgeReport{
		Bridged: true,
		Bridge:  Bridge{Dir: "/src/forge", File: "/p/go.work", written: "../forge"},
		Binary:  Binary{Name: "forge", Revision: "6d2870f016fb0473"},
		Checkout: Checkout{
			Dir:  "/src/forge",
			Head: "750833054b78ab216150bdaad19c389cb91cbc53",
		},
		Skewed: true,
		Kind:   SkewRevision,
	}
	got := r.Line()
	for _, want := range []string{"forge skew", "6d2870f016fb", "750833054b78", "go work edit -dropuse=../forge"} {
		if !strings.Contains(got, want) {
			t.Errorf("Line() = %q, missing %q", got, want)
		}
	}
}

func TestDirtyVersion(t *testing.T) {
	tests := []struct {
		v    string
		want bool
	}{
		{"", false},
		{"   ", false},
		{"v1.2.3", false},
		{"v1.2.3+dev", false},
		{"v1.2.3+dirty", true},
		{"1.2.3+dirty", true},
		{"  v1.2.3+dirty  ", true},
		{"v0.0.0-20260101000000-abcdef123456+dirty", true},
		{"(devel)", false},
	}
	for _, tt := range tests {
		t.Run(tt.v, func(t *testing.T) {
			if got := DirtyVersion(tt.v); got != tt.want {
				t.Errorf("DirtyVersion(%q) = %v, want %v", tt.v, got, tt.want)
			}
		})
	}
}

func TestComparablePins(t *testing.T) {
	tests := []struct {
		name           string
		pinned, binary string
		wantP, wantB   string
		wantOK         bool
	}{
		{"both bare releases get a v prefix", "1.2.3", "1.2.4", "v1.2.3", "v1.2.4", true},
		{"already prefixed", "v1.2.3", "v1.2.4", "v1.2.3", "v1.2.4", true},
		{"whitespace is trimmed", " v1.2.3 ", "\tv1.2.4\n", "v1.2.3", "v1.2.4", true},
		{"empty pin", "", "v1.2.4", "", "", false},
		{"first-user sentinel is never a real pin", firstUserSentinel, "v1.2.4", "", "", false},
		{"empty binary", "v1.2.3", "", "", "", false},
		{"dev build is not comparable", "v1.2.3", "v1.2.4+dev", "", "", false},
		{"dirty build is not comparable", "v1.2.3", "v1.2.4+dirty", "", "", false},
		{"pseudo-version binary", "v1.2.3", "v0.0.0-20260101000000-abcdef123456", "", "", false},
		{"base-version pseudo", "v1.2.3", "v1.2.4-0.20260101000000-abcdef123456", "", "", false},
		{"invalid pin", "not-a-version", "v1.2.4", "", "", false},
		{"invalid binary", "v1.2.3", "garbage", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, b, ok := comparablePins(tt.pinned, tt.binary)
			if p != tt.wantP || b != tt.wantB || ok != tt.wantOK {
				t.Errorf("comparablePins(%q, %q) = (%q, %q, %v), want (%q, %q, %v)",
					tt.pinned, tt.binary, p, b, ok, tt.wantP, tt.wantB, tt.wantOK)
			}
		})
	}
}

func TestIsPseudoVersion(t *testing.T) {
	tests := []struct {
		v    string
		want bool
	}{
		{"v0.0.0-20260101000000-abcdef123456", true},
		{"v1.2.4-0.20260101000000-abcdef123456", true},
		{"v1.2.4-pre.0.20260101000000-abcdef123456", true},
		{"v0.0.0-20260101000000-abcdef123456+dirty", true},
		{"v1.2.3", false},
		{"v1.2.3-rc1", false},
		{"v0.0.0-20260101000000-ABCDEF123456", false},  // uppercase hex is not a commit
		{"v0.0.0-20260101000000-abcdef12345", false},   // 11-char commit
		{"v0.0.0-20260101000000-abcdef1234567", false}, // 13-char commit
		{"v0.0.0-2026010100000x-abcdef123456", false},  // non-digit timestamp
		{"v0.0.0-202601010000-abcdef123456", false},    // short timestamp
		{"v0.0.0-20260101000000-abcdef12345g", false},  // non-hex in commit
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.v, func(t *testing.T) {
			if got := isPseudoVersion(tt.v); got != tt.want {
				t.Errorf("isPseudoVersion(%q) = %v, want %v", tt.v, got, tt.want)
			}
		})
	}
}

func TestCurrentBinary(t *testing.T) {
	b := CurrentBinary()
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("no executable path: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if want := strings.TrimSuffix(filepath.Base(exe), ".exe"); b.Name != want {
		t.Errorf("Name = %q, want the running executable's name %q", b.Name, want)
	}
	st, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if !b.BuiltAt.Equal(st.ModTime()) {
		t.Errorf("BuiltAt = %v, want the executable's mtime %v", b.BuiltAt, st.ModTime())
	}
}

// gitRepo makes dir a git repository with one commit and returns its HEAD.
func gitRepo(t *testing.T, dir string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("add", ".")
	git("commit", "-q", "-m", "init")
	git("tag", "v1.4.0")
	return git("rev-parse", "HEAD")
}

func TestResolveTag(t *testing.T) {
	testkit.Slow(t, "execs git")
	dir := forgeModule(t, t.TempDir())
	head := gitRepo(t, dir)

	if got := ResolveTag(dir, "v1.4.0"); got != head {
		t.Errorf("ResolveTag(v1.4.0) = %q, want HEAD %q", got, head)
	}
	if got := ResolveTag(dir, "v9.9.9"); got != "" {
		t.Errorf("an unknown tag must resolve to \"\", got %q", got)
	}
	if got := ResolveTag(t.TempDir(), "v1.4.0"); got != "" {
		t.Errorf("a non-repository must resolve to \"\", got %q", got)
	}
}

func TestInspectBridge(t *testing.T) {
	testkit.Slow(t, "execs git")
	t.Setenv("GOWORK", "")

	t.Run("an unbridged project has nothing to report", func(t *testing.T) {
		project := t.TempDir()
		writeFile(t, filepath.Join(project, "go.mod"), "module example.com/p\n")
		r := InspectBridge(project)
		if r.Bridged || r.Skewed || r.Line() != "" {
			t.Errorf("report = %+v, want an empty one", r)
		}
	})

	t.Run("a bridge that is not a git checkout reports nothing", func(t *testing.T) {
		base := t.TempDir()
		forgeModule(t, filepath.Join(base, "forge"))
		project := filepath.Join(base, "p")
		writeFile(t, filepath.Join(project, "go.work"), "go 1.26\n\nuse (\n\t.\n\t../forge\n)\n")
		r := InspectBridge(project)
		if !r.Bridged || r.CheckoutOK || r.Skewed || r.Line() != "" {
			t.Errorf("report = %+v, want Bridged without a readable checkout and no warning", r)
		}
	})

	t.Run("a bridged checkout this binary was not built from is skew", func(t *testing.T) {
		base := t.TempDir()
		checkout := forgeModule(t, filepath.Join(base, "forge"))
		head := gitRepo(t, checkout)
		project := filepath.Join(base, "p")
		writeFile(t, filepath.Join(project, "go.work"), "go 1.26\n\nuse (\n\t.\n\t../forge\n)\n")

		r := InspectBridge(project)
		if !r.Bridged || !r.CheckoutOK {
			t.Fatalf("report = %+v, want a bridged, readable checkout", r)
		}
		if r.Checkout.Head != head {
			t.Errorf("Checkout.Head = %q, want %q", r.Checkout.Head, head)
		}
		if !r.Skewed {
			t.Fatalf("the test binary was not built from a throwaway checkout, yet no skew was reported: %+v", r)
		}
		line := r.Line()
		if !strings.Contains(line, "forge skew") || !strings.Contains(line, "go work edit -dropuse=../forge") {
			t.Errorf("Line() = %q, want a skew warning that names the undo command", line)
		}
	})
}

func TestResolveProjectForge(t *testing.T) {
	testkit.Slow(t, "execs go list")

	t.Run("a directory replace is local", func(t *testing.T) {
		base := t.TempDir()
		forgeModule(t, filepath.Join(base, "forge"))
		project := filepath.Join(base, "p")
		writeFile(t, filepath.Join(project, "go.mod"),
			"module example.com/p\n\ngo 1.26\n\nrequire "+ModulePath+" v1.2.3\n\nreplace "+ModulePath+" => ../forge\n")
		t.Setenv("GOWORK", "off")
		t.Setenv("GOFLAGS", "")
		version, local, ok := ResolveProjectForge(project)
		if !ok || !local || version != "" {
			t.Errorf("ResolveProjectForge = (%q, %v, %v), want (\"\", true, true)", version, local, ok)
		}
	})

	t.Run("a directory that is not a module cannot be answered", func(t *testing.T) {
		t.Setenv("GOWORK", "off")
		t.Setenv("GOFLAGS", "")
		version, local, ok := ResolveProjectForge(t.TempDir())
		if ok || local || version != "" {
			t.Errorf("ResolveProjectForge = (%q, %v, %v), want (\"\", false, false)", version, local, ok)
		}
	})
}

func TestAssess(t *testing.T) {
	t.Run("no go.mod does not apply", func(t *testing.T) {
		if a, ok := Assess(t.TempDir()); ok || a != (Assessment{}) {
			t.Errorf("Assess = (%+v, %v), want it not to apply", a, ok)
		}
	})

	t.Run("a go.mod that never mentions forge does not apply", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/p\n\ngo 1.26\n")
		if a, ok := Assess(dir); ok || a != (Assessment{}) {
			t.Errorf("Assess = (%+v, %v), want it not to apply", a, ok)
		}
	})

	t.Run("a locally replaced forge always passes", func(t *testing.T) {
		testkit.Slow(t, "execs go list")
		base := t.TempDir()
		forgeModule(t, filepath.Join(base, "forge"))
		project := filepath.Join(base, "p")
		writeFile(t, filepath.Join(project, "go.mod"),
			"module example.com/p\n\ngo 1.26\n\nrequire "+ModulePath+" v1.2.3\n\nreplace "+ModulePath+" => ../forge\n")
		t.Setenv("GOWORK", "off")
		t.Setenv("GOFLAGS", "")
		a, ok := Assess(project)
		if !ok {
			t.Fatal("Assess did not apply to a project that requires forge")
		}
		if a.Verdict != OK || a.ProjectVersion != "" {
			t.Errorf("Assessment = %+v, want Verdict OK and no project version (it resolves to a directory)", a)
		}
	})

	t.Run("a go.mod the toolchain cannot resolve does not apply", func(t *testing.T) {
		testkit.Slow(t, "execs go list")
		dir := t.TempDir()
		// Mentions forge, but go.mod is malformed, so `go list` cannot answer.
		writeFile(t, filepath.Join(dir, "go.mod"), "// "+ModulePath+"\nthis is not a go.mod\n")
		t.Setenv("GOWORK", "off")
		t.Setenv("GOFLAGS", "")
		if a, ok := Assess(dir); ok {
			t.Errorf("Assess = (%+v, true), want it not to apply when go list fails", a)
		}
	})
}
