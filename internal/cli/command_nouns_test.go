package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/forgedelegate"
)

// The 14 nouns, plus version, are the visible top level.
func TestTheFourteenNounsExist(t *testing.T) {
	root := newTestRootCmd(t)
	for _, noun := range []string{"scaffold", "project", "generate", "lint", "ci", "db", "env", "release", "obs", "cloud", "dev", "ui", "machine", "agent", "version"} {
		if c := findChildCommand(root, noun); c == nil || c.Hidden {
			t.Errorf("`forge %s` is missing or hidden", noun)
		}
	}
}

// Each command that moved under a noun lives there, and only there: v1.9.0
// removed the old spellings (the migrations/v1.9.0 playbook maps each).
func TestMovedCommandsLiveOnlyUnderTheirNoun(t *testing.T) {
	root := newTestRootCmd(t)
	resolves := func(rel string) bool {
		c, _, err := root.Find(strings.Fields(rel))
		return err == nil && c != root && commandRel(c, root) == rel
	}
	for _, rel := range []string{
		"project doctor", "project start", "scaffold package", "lint fix narrow-deps",
		"env kcl eval", "env secret set", "release ledger show", "release gate record",
		"release list", "env releases list", "cloud login", "cloud logout", "cloud registry login",
		"cloud domain add", "cloud cluster connect", "dev cluster up", "dev debug start", "dev curl",
		"ui component install", "ui sourcemaps push", "machine slot run", "machine storage gc",
		"machine tools install", "agent skill load", "agent mcp", "agent report-bug",
	} {
		if !resolves(rel) {
			t.Errorf("`forge %s` does not resolve", rel)
		}
	}
	kept := map[string]bool{}
	for _, a := range keptAliases {
		kept[strings.Fields(a.Old)[0]] = true
	}
	for _, old := range []string{
		"doctor", "start", "package", "fix", "kcl", "secret", "ledger", "gate", "login", "logout",
		"registry", "domain", "cluster", "debug", "api", "component", "frontend", "slot", "storage",
		"tools", "skill", "mcp", "report-bug",
	} {
		c := findChildCommand(root, old)
		switch {
		case kept[old] && (c == nil || !c.Hidden):
			t.Errorf("`forge %s` is a kept alias (command_aliases.go): it must be mounted and hidden", old)
		case !kept[old] && c != nil:
			t.Errorf("`forge %s` is still mounted at the root; v1.9.0 removed it", old)
		}
	}
	// Each kept alias runs its new path's code and is keyed as the new path.
	for _, a := range keptAliases {
		c, _, err := root.Find(strings.Fields(a.Old))
		if err != nil || c == root {
			t.Errorf("kept alias `forge %s` does not resolve: %v", a.Old, err)
			continue
		}
		if got := commandRel(c, root); got != a.New {
			t.Errorf("commandRel(`forge %s`) = %q, want %q", a.Old, got, a.New)
		}
	}
}

// Only forge's root has persistent hooks: a group that gained one would run
// code its subcommands' callers do not expect.
func TestNoNounHasItsOwnHooks(t *testing.T) {
	root := newTestRootCmd(t)
	for _, noun := range []string{"dev", "dev cluster", "cloud cluster", "ui", "machine", "agent"} {
		c, _, err := root.Find(strings.Fields(noun))
		if err != nil || c == nil {
			t.Fatalf("`forge %s`: %v", noun, err)
		}
		if c.PersistentPreRun != nil || c.PersistentPreRunE != nil || c.PersistentPostRun != nil || c.PersistentPostRunE != nil {
			t.Errorf("`forge %s` has a persistent hook", noun)
		}
	}
}

// A command is handed to the pinned forge as typed; the machine- and
// agent-level ones that are exempt stay here.
func TestHandOffGivesThePinnedForgeTheSpellingTyped(t *testing.T) {
	skewedProject(t)
	t.Setenv(forgedelegate.MarkerEnv, "")

	args := []string{"release", "ledger", "show", "--json"}
	h, _ := armDelegation(t)
	handed, _, err := handOffForTest(t, args...)
	if err != nil || !handed {
		t.Fatalf("%v: handed %v, err %v", args, handed, err)
	}
	if got, want := strings.Join(h.args, " "), strings.Join(args, " "); got != want {
		t.Errorf("handed over as %q, want %q as typed", got, want)
	}

	for _, args := range [][]string{{"machine", "slot", "status"}, {"agent", "mcp"}, {"agent", "report-bug"}} {
		h, _ := armDelegation(t)
		if handed, _, err := handOffForTest(t, args...); err != nil || handed || h.path != "" {
			t.Errorf("forge %v was handed over (handed %v, err %v); it runs as this forge", args, handed, err)
		}
	}
}

// --version and `version` both print the identity.
func TestVersionFlagAndCommand(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"version"}} {
		root := newTestRootCmd(t)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("forge %v: %v", args, err)
		}
		if !strings.Contains(out.String(), versionLine()) {
			t.Errorf("forge %v printed %q, want the identity %q", args, out.String(), versionLine())
		}
	}
}

// env's help is grouped, and every visible subcommand is in a group.
func TestEnvHelpGroupsEverySubcommand(t *testing.T) {
	env := findChildCommand(newTestRootCmd(t), "env")
	for _, c := range env.Commands() {
		if c.Hidden || c.Name() == "help" {
			continue
		}
		if c.GroupID == "" {
			t.Errorf("`forge env %s` is in no help group: add it to envCommandGroups", c.Name())
		}
	}
}
