package codemod

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/templates"
)

// Once each template has its fix, render it, revert the fixed fragment to
// the old shape, and require the codemod to restore it. While the upstream
// template PR is in flight, fixtures exercise the same changes and verify
// that dependent codemods stay inactive.

// runFix runs the named codemod over dir.
func runFix(t *testing.T, dir, name string, write bool) Report {
	t.Helper()
	c, ok := Lookup(name)
	if !ok {
		t.Fatalf("codemod %s is not registered", name)
	}
	if !c.Upgrade || !c.Advise {
		t.Fatalf("template fix %s must be Upgrade and Advise: upgrade applies it, generate names it", name)
	}
	rep, err := c.Run(dir, Options{Write: write})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return rep
}

func tfWrite(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func tfRead(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// oldify reverts newFrag in render to oldFrag, failing when the template does
// not carry newFrag exactly once (the template moved; update the codemod).
func oldify(t *testing.T, render, newFrag, oldFrag string) string {
	t.Helper()
	if n := strings.Count(render, newFrag); n != 1 {
		t.Fatalf("the template carries the fixed fragment %d times, want 1 — the template changed; update the codemod's copy of it:\n%s", n, newFrag)
	}
	return strings.Replace(render, newFrag, oldFrag, 1)
}

// assertConverges checks the codemod turns old into want at rel, and that a
// second run is a no-op.
func assertConverges(t *testing.T, name, rel, old, want string) {
	t.Helper()
	dir := t.TempDir()
	tfWrite(t, dir, "go.mod", "module example.com/fx\n\ngo 1.24\n")
	tfWrite(t, dir, rel, old)
	dry := runFix(t, dir, name, false)
	if !dry.Changed() || tfRead(t, dir, rel) != old {
		t.Fatalf("dry run: want a planned rewrite and no write, got %+v", dry)
	}
	rep := runFix(t, dir, name, true)
	if len(rep.Rewrites) != 1 || !strings.HasPrefix(rep.Rewrites[0], rel+": ") {
		t.Fatalf("want one rewrite of %s, got %+v", rel, rep)
	}
	if got := tfRead(t, dir, rel); got != want {
		t.Fatalf("the rewrite is not the template's render: %s", firstDiff(got, want))
	}
	if again := runFix(t, dir, name, true); again.Changed() || len(again.Next) > 0 {
		t.Fatalf("a second run must change nothing, got %+v", again)
	}
}

func TestTemplateFix_ObserveChainLogger(t *testing.T) {
	t.Parallel()
	out, err := templates.InternalPkgTemplates().Render("observe_chain.go.tmpl", struct {
		Name, ImportPath, Module, Flavor, LogLevel string
	}{"store", "store", "example.com/fx", "", "slog.LevelDebug"})
	if err != nil {
		t.Fatal(err)
	}
	render := string(out)
	old := render
	if strings.Contains(render, "var logger *slog.Logger") {
		old = oldify(t, render, observeChainNewLogger, observeChainOldLogger)
		assertConverges(t, "observe-chain-logger", "internal/store/observe_chain.go", old, render)
	} else {
		assertDependantFixInactive(t, "observe-chain-logger", "internal/store/observe_chain.go", old)
		out, _, _ := rewriteObserveChainLogger("observe_chain.go", []byte(old))
		if got, want := string(out), oldify(t, old, observeChainOldLogger, observeChainNewLogger); got != want {
			t.Fatalf("unexpected patch: %s", firstDiff(got, want))
		}
	}

	// A seam that calls a method on logger is listed, never made nil.
	dir := t.TempDir()
	tfWrite(t, dir, "go.mod", "module example.com/fx\n\ngo 1.24\n")
	edited := strings.Replace(old, "\treturn observe.NewComponentChain(", "\tlogger.Info(\"chain built\")\n\treturn observe.NewComponentChain(", 1)
	tfWrite(t, dir, "internal/store/observe_chain.go", edited)
	out, _, next := rewriteObserveChainLogger("observe_chain.go", []byte(edited))
	if out != nil || next == "" {
		t.Fatalf("a seam that uses logger must be listed and left alone, got %s", out)
	}
}

func TestTemplateFix_CmdServerConfig(t *testing.T) {
	t.Parallel()
	block := regexp.MustCompile(`(?s)// ServerConfig is the serverkit\.Config.*?\n}\n\n?`)
	for name, data := range map[string]codegen.CmdServerTemplateData{
		"one config": {Module: "example.com/fx", Name: "fx", ConfigFields: codegen.DefaultConfigFieldNames()},
		"root configs": {Module: "example.com/fx", Name: "fx", ConfigFields: codegen.DefaultConfigFieldNames(),
			ConfigInputs: []codegen.ServeConfigInput{{Name: "RoleConfig", Doc: "the role config", Fields: map[string]bool{"Port": true}}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, err := templates.ProjectTemplates().Render("cmd-tree-serve-graph.go.tmpl", data)
			if err != nil {
				t.Fatal(err)
			}
			render := string(out)
			m := block.FindString(render)
			if m == "" {
				t.Fatalf("the serve-graph template has no ServerConfig — update the codemod:\n%s", render)
			}
			// The codemod writes the block followed by one blank line; the
			// template may abut the next comment. Blank lines aside, the
			// result is the render.
			dir := t.TempDir()
			tfWrite(t, dir, "go.mod", "module example.com/fx\n\ngo 1.24\n")
			rel := "cmd/fx/cmd/serve.go"
			// On disk the file is formatted the way the codemod formats
			// what it rewrites.
			render = string(formatForProject(dir, filepath.Join(dir, rel), []byte(render)))
			old := strings.Replace(render, m, "", 1)
			tfWrite(t, dir, rel, old)
			rep := runFix(t, dir, "cmd-server-config", true)
			if !rep.Changed() {
				t.Fatalf("want a rewrite, got %+v", rep)
			}
			if got, want := dropBlankLines(tfRead(t, dir, rel)), dropBlankLines(render); got != want {
				t.Fatalf("the rewrite is not the template's render: %s", firstDiff(got, want))
			}
			if again := runFix(t, dir, "cmd-server-config", true); again.Changed() {
				t.Fatalf("a second run must change nothing, got %+v", again)
			}
		})
	}

	// A pre-graph serve.go (no runRoot) never had ServerConfig to gain.
	dir := t.TempDir()
	tfWrite(t, dir, "go.mod", "module example.com/fx\n\ngo 1.24\n")
	tfWrite(t, dir, "cmd/fx/cmd/serve.go", "package cmd\n\nfunc serverkitConfig() {}\n")
	if rep := runFix(t, dir, "cmd-server-config", true); rep.Changed() || len(rep.Next) > 0 {
		t.Fatalf("a serve.go without runRoot must be left alone, got %+v", rep)
	}
}

func dropBlankLines(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func TestTemplateFix_GolangciAppendAssign(t *testing.T) {
	t.Parallel()
	for _, guard := range []string{"", "on"} {
		out, err := templates.ProjectTemplates().Render("golangci.yml.tmpl", struct {
			Module, TypedAccessGuard, LoaderPackage string
		}{"example.com/fx", guard, "pkg/config"})
		if err != nil {
			t.Fatal(err)
		}
		render := string(out)
		old := render
		if strings.Contains(render, `text: "appendAssign"`) {
			old = oldify(t, render, golangciAppendAssignRule, "")
		} else {
			render = oldify(t, old, "          - gocognit\n", "          - gocognit\n"+golangciAppendAssignRule)
		}
		assertConverges(t, "golangci-append-assign", ".golangci.yml", old, render)
	}
}

func TestTemplateFix_TaskfileShortSkipSummary(t *testing.T) {
	t.Parallel()
	golden, err := os.ReadFile(filepath.Join("..", "templates", "testdata", "golden", "Taskfile.yml.golden"))
	if err != nil {
		t.Fatal(err)
	}
	render := string(golden)
	old := render
	if strings.Contains(render, taskfileShortNew) {
		old = oldify(t, render, taskfileShortNew, taskfileShortOld)
		assertConverges(t, "taskfile-short-skip-summary", "Taskfile.yml", old, render)
	} else {
		assertDependantFixInactive(t, "taskfile-short-skip-summary", "Taskfile.yml", old)
		out, _, _ := rewriteTaskfileShortSkipSummary("Taskfile.yml", []byte(old))
		if got, want := string(out), oldify(t, old, taskfileShortOld, taskfileShortNew); got != want {
			t.Fatalf("unexpected patch: %s", firstDiff(got, want))
		}
	}

	// An edited test:short command is not the template's: left alone.
	dir := t.TempDir()
	edited := strings.Replace(old, "go test -short -timeout 60s", "go test -short -timeout 90s", 1)
	tfWrite(t, dir, "Taskfile.yml", edited)
	if rep := runFix(t, dir, "taskfile-short-skip-summary", true); rep.Changed() {
		t.Fatalf("an edited test:short must be left alone, got %+v", rep)
	}
}

// A Tier-2 starter that is still an unedited render is left to upgrade's
// template phase, which replaces it whole; a disowned file is the project's
// decision. Neither is rewritten.
func TestTemplateFix_SkipsPristineAndDisownedFiles(t *testing.T) {
	t.Parallel()
	const path = ".golangci.yml"
	const fix = "golangci-append-assign"
	const old = `linters:
  enable:
    - gocritic
  exclusions:
    rules:
      - path: _test\.go
        linters:
          - gocritic
`

	t.Run("pristine", func(t *testing.T) {
		t.Parallel()
		stamped, ok := checksums.Stamp(path, []byte(old))
		if !ok || checksums.Verify(stamped) != checksums.Pristine {
			t.Fatal("could not stamp a pristine .golangci.yml")
		}
		dir := t.TempDir()
		tfWrite(t, dir, path, string(stamped))
		c, _ := Lookup(fix)
		rep, err := c.Run(dir, Options{Write: true, LeavePristineTemplates: true})
		if err != nil || rep.Changed() {
			t.Fatalf("an unedited render belongs to upgrade's template phase, got %+v, %v", rep, err)
		}
		if rep := runFix(t, dir, fix, true); !rep.Changed() {
			t.Fatalf("an explicit fix may patch a pristine template, got %+v", rep)
		}
	})
	t.Run("disowned", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tfWrite(t, dir, path, old)
		cs, err := checksums.Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		cs.Disowned[path] = checksums.DisownedEntry{Reason: "ours"}
		if err := checksums.Save(dir, cs); err != nil {
			t.Fatal(err)
		}
		if rep := runFix(t, dir, fix, true); rep.Changed() {
			t.Fatalf("a disowned file must be left alone, got %+v", rep)
		}
	})
}

// firstDiff names the first line where got and want differ, with context.
func firstDiff(got, want string) string {
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < len(g) || i < len(w); i++ {
		var gl, wl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl {
			lo := max(0, i-3)
			return fmt.Sprintf("line %d:\n--- got\n%s\n--- want\n%s", i+1,
				strings.Join(g[lo:min(len(g), i+4)], "\n"), strings.Join(w[lo:min(len(w), i+4)], "\n"))
		}
	}
	return "equal"
}

func assertDependantFixInactive(t *testing.T, fix, path, old string) {
	t.Helper()
	dir := t.TempDir()
	tfWrite(t, dir, path, old)
	if rep := runFix(t, dir, fix, true); rep.Changed() || len(rep.Next) != 0 || tfRead(t, dir, path) != old {
		t.Fatalf("%s should be inactive in a binary that lacks the scaffold change: %+v", fix, rep)
	}
}
