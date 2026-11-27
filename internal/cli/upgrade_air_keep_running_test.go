package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/reliant-labs/forge/internal/codemod"
	"github.com/reliant-labs/forge/internal/templates"
)

// renderAirConfig renders a scaffolded air config for a project pinned to pin.
func renderAirConfig(t *testing.T, tmpl, name, pin string) string {
	t.Helper()
	b, err := templates.ProjectTemplates().Render(tmpl, airTemplateData{Name: name, ForgeVersion: pin})
	if err != nil {
		t.Fatalf("render %s: %v", tmpl, err)
	}
	return string(b)
}

// airConfigCmds decodes an air config's [build] and [build.windows] cmds.
func airConfigCmds(t *testing.T, src string) (build, windows string) {
	t.Helper()
	var doc struct {
		Build struct {
			Cmd     string `toml:"cmd"`
			Windows struct {
				Cmd string `toml:"cmd"`
			} `toml:"windows"`
			StopOnError bool `toml:"stop_on_error"`
		} `toml:"build"`
	}
	if _, err := toml.Decode(src, &doc); err != nil {
		t.Fatalf("decode: %v\n%s", err, src)
	}
	if doc.Build.StopOnError {
		t.Fatalf("stop_on_error is true: air would stop the server on a failed build")
	}
	return doc.Build.Cmd, doc.Build.Windows.Cmd
}

// A project pinned to v1.12.0 gets air configs that build through `forge dev
// air-build`; an older pin keeps the form its forge can run, byte for byte
// what it got before. Both keep the leading `forge storage check ./tmp` and
// the same [build.windows] cmd.
func TestAirTemplatesBuildThroughAirBuildFromV1120(t *testing.T) {
	for _, tc := range []struct{ tmpl, flags string }{
		{"air.toml.tmpl", "$FORGE_GO_BUILD_FLAGS -buildvcs=false "},
		{"air-debug.toml.tmpl", "-gcflags='all=-N -l' -trimpath=false -buildvcs=false "},
	} {
		t.Run(tc.tmpl, func(t *testing.T) {
			newCmd, newWin := airConfigCmds(t, renderAirConfig(t, tc.tmpl, "acme", airKeepRunningRelease))
			wantNew := "forge storage check ./tmp; CGO_ENABLED=0 forge dev air-build --precheck-status $? --out ./tmp/acme -- go build " + tc.flags + "-o ./tmp/acme.tmp ./cmd/acme"
			if newCmd != wantNew {
				t.Errorf("v1.12.0 [build] cmd =\n  %s\nwant\n  %s", newCmd, wantNew)
			}
			oldSrc := renderAirConfig(t, tc.tmpl, "acme", "v1.11.0")
			oldCmd, oldWin := airConfigCmds(t, oldSrc)
			wantOld := "forge storage check ./tmp && CGO_ENABLED=0 go build " + tc.flags + "-o ./tmp/acme.tmp ./cmd/acme && mv ./tmp/acme.tmp ./tmp/acme"
			if oldCmd != wantOld {
				t.Errorf("v1.11.0 [build] cmd =\n  %s\nwant\n  %s", oldCmd, wantOld)
			}
			if strings.Contains(oldSrc, "air-build") {
				t.Errorf("an older pin's config names forge dev air-build, which its forge does not have:\n%s", oldSrc)
			}
			if newWin != oldWin || strings.Contains(newWin, "air-build") {
				t.Errorf("[build.windows] cmd changed with the pin: %q vs %q", newWin, oldWin)
			}
		})
	}
}

// The codemod turns each [build] cmd an older forge scaffolded into the
// current template's, byte for byte, and changes nothing else.
func TestAirKeepRunningRewritesScaffoldedForms(t *testing.T) {
	for _, tmpl := range []string{"air.toml.tmpl", "air-debug.toml.tmpl"} {
		t.Run(tmpl, func(t *testing.T) {
			old := renderAirConfig(t, tmpl, "my-app.v2", "v1.11.0")
			out, rewrites, next := planAirKeepRunning(".air.toml", old)
			if len(rewrites) != 1 || len(next) != 0 {
				t.Fatalf("rewrites %v, next %v; want one rewrite", rewrites, next)
			}
			gotCmd, gotWin := airConfigCmds(t, out)
			wantCmd, _ := airConfigCmds(t, renderAirConfig(t, tmpl, "my-app.v2", airKeepRunningRelease))
			_, oldWin := airConfigCmds(t, old)
			if gotCmd != wantCmd {
				t.Errorf("rewritten cmd =\n  %s\nwant the current template's\n  %s", gotCmd, wantCmd)
			}
			if gotWin != oldWin {
				t.Errorf("[build.windows] cmd changed: %q", gotWin)
			}
			if again, rw, nx := planAirKeepRunning(".air.toml", out); again != out || len(rw)+len(nx) != 0 {
				t.Errorf("a second run changed something: %v %v", rw, nx)
			}
		})
	}
}

func TestAirKeepRunningOlderAndEditedForms(t *testing.T) {
	cfg := func(cmd string) string {
		return "root = \".\"\n\n[build]\n  # keep this comment\n  cmd = \"" + cmd + "\"\n  stop_on_error = false\n"
	}
	for _, tc := range []struct {
		name, cmd, want string
		next            bool
	}{
		{
			name: "before $FORGE_GO_BUILD_FLAGS",
			cmd:  "forge storage check ./tmp && CGO_ENABLED=0 go build -buildvcs=false -o ./tmp/app.tmp ./cmd/app && mv ./tmp/app.tmp ./tmp/app",
			want: "forge storage check ./tmp; CGO_ENABLED=0 forge dev air-build --precheck-status $? --out ./tmp/app -- go build -buildvcs=false -o ./tmp/app.tmp ./cmd/app",
		},
		{
			name: "debug before -trimpath=false",
			cmd:  "forge storage check ./tmp && CGO_ENABLED=0 go build -gcflags='all=-N -l' -buildvcs=false -o ./tmp/app.tmp ./cmd/app && mv ./tmp/app.tmp ./tmp/app",
			want: "forge storage check ./tmp; CGO_ENABLED=0 forge dev air-build --precheck-status $? --out ./tmp/app -- go build -gcflags='all=-N -l' -buildvcs=false -o ./tmp/app.tmp ./cmd/app",
		},
		{name: "an added flag", cmd: "forge storage check ./tmp && CGO_ENABLED=0 go build -race -buildvcs=false -o ./tmp/app.tmp ./cmd/app && mv ./tmp/app.tmp ./tmp/app", next: true},
		{name: "names that differ", cmd: "forge storage check ./tmp && CGO_ENABLED=0 go build -buildvcs=false -o ./tmp/app.tmp ./cmd/srv && mv ./tmp/app.tmp ./tmp/app", next: true},
		{name: "before forge storage check", cmd: "CGO_ENABLED=0 go build -buildvcs=false -o ./tmp/app.tmp ./cmd/app && mv ./tmp/app.tmp ./tmp/app", next: true},
		{name: "already holds", cmd: "forge storage check ./tmp; CGO_ENABLED=0 forge dev air-build --precheck-status $? --out ./tmp/app -- go build -o ./tmp/app.tmp ./cmd/app"},
		{name: "custom build", cmd: "make dev-build", next: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := cfg(tc.cmd)
			out, rewrites, next := planAirKeepRunning(".air.toml", src)
			if (len(next) > 0) != tc.next {
				t.Fatalf("next = %v, want a hand edit: %v", next, tc.next)
			}
			if tc.want == "" {
				if out != src || len(rewrites) != 0 {
					t.Fatalf("changed a cmd it must leave alone:\n%s", out)
				}
				return
			}
			if want := cfg(tc.want); out != want {
				t.Fatalf("got\n%s\nwant\n%s", out, want)
			}
		})
	}
}

// The codemod writes only for a project whose pin has `forge dev air-build`;
// for an older pin it names the upgrade, and the playbook's detection is its
// own decision.
func TestAirKeepRunningCodemodHonoursThePin(t *testing.T) {
	old := renderAirConfig(t, "air.toml.tmpl", "test-project", "v1.11.0")
	c, ok := codemod.Lookup("air-keep-running")
	if !ok {
		t.Fatal("forge fix air-keep-running is not registered")
	}

	dir := newTestProjectWithVersion(t, "v1.11.0")
	writeProjectFile(t, dir, ".air.toml", old)
	rep, err := c.Run(dir, codemod.Options{Write: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Changed() || len(rep.Next) != 1 || !strings.Contains(rep.Next[0], "project upgrade --to "+airKeepRunningRelease) {
		t.Fatalf("an older pin: %+v, want no rewrite and the upgrade named", rep)
	}
	if got := readProjectFile(t, dir, ".air.toml"); got != old {
		t.Fatal("wrote .air.toml for a pin without forge dev air-build")
	}
	if !projectNeedsAirKeepRunning(dir) {
		t.Error("detection: an old cmd under an old pin does not need the playbook")
	}

	dir = newTestProjectWithVersion(t, airKeepRunningRelease)
	writeProjectFile(t, dir, ".air.toml", old)
	rep, err = c.Run(dir, codemod.Options{Write: true})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Changed() || len(rep.Next) != 0 {
		t.Fatalf("a v1.12.0 pin: %+v, want the rewrite", rep)
	}
	if cmd, _ := airConfigCmds(t, readProjectFile(t, dir, ".air.toml")); !strings.Contains(cmd, "forge dev air-build --precheck-status $? --out ./tmp/test-project --") {
		t.Fatalf("not rewritten: %s", cmd)
	}
	if projectNeedsAirKeepRunning(dir) {
		t.Error("detection: still needed after the rewrite")
	}
}

func TestAirKeepRunningPlaybookIsShipped(t *testing.T) {
	metas, err := loadMigrationMetas()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range metas {
		if m.ID != airKeepRunningPlaybookID {
			continue
		}
		if m.Version != airKeepRunningRelease || len(m.Automation) != 1 || m.Automation[0] != "fix:air-keep-running" {
			t.Fatalf("meta = %+v", m)
		}
		dir := newTestProjectWithVersion(t, "v1.11.0")
		writeProjectFile(t, dir, ".air.toml", renderAirConfig(t, "air.toml.tmpl", "test-project", "v1.11.0"))
		if !migrationApplies(m, "v1.11.0", dir) {
			t.Error("the playbook does not apply to an older project with forge's old air cmd")
		}
		if migrationApplies(m, airKeepRunningRelease, dir) {
			t.Error("the playbook applies to a project already at its release")
		}
		return
	}
	t.Fatalf("no shipped playbook %s", airKeepRunningPlaybookID)
}

func readProjectFile(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
