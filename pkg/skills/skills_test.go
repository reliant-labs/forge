package skills

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/skillcatalog"
)

// ListSkills lists the embedded skills, the entry point among them, each
// stamped with the forge serving it.
func TestListSkillsListsTheShippedSkills(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	listed, err := ListSkills("")
	if err != nil {
		t.Fatal(err)
	}
	var entry *Skill
	for i := range listed {
		if listed[i].Path == "forge" {
			entry = &listed[i]
		}
		if listed[i].Relevance == string(skillcatalog.SkillRelevanceMigration) {
			t.Errorf("default listing includes migration skill %s", listed[i].Path)
		}
	}
	if entry == nil {
		t.Fatalf("no entry-point skill \"forge\" among %d listed", len(listed))
	}
	if entry.Scope != "forge" || entry.Description == "" || entry.SkillForgeVersion == "" {
		t.Errorf("entry point = %+v, want scope forge with a description and the serving forge version", *entry)
	}
}

// RenderSkill is what `skill load` prints: the one renderer, the mount name
// applied, and the "forge/" prefix accepted.
func TestRenderSkillIsSkillLoad(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	want, err := skillcatalog.RenderSkillContentAt("", "db", skillcatalog.SkillAudienceAll, "reliant forge")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"db", "forge/db"} {
		got, err := RenderSkill("", path, RenderSkillOptions{CLIName: "reliant forge"})
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("RenderSkill(%q) differs from the skill load renderer", path)
		}
	}
	if !bytes.Contains(want, []byte("reliant forge ")) {
		t.Error("the db skill names no `reliant forge` command; the mount rewrite was not applied")
	}
	if _, err := RenderSkill("", "no-such-skill", RenderSkillOptions{}); err == nil {
		t.Error("RenderSkill of an unknown skill succeeded")
	}
}

// LoadSkillForAudience strips @forge-only blocks for a general audience and
// keeps them for a forge one; a project skill is found under .forge/skills.
func TestLoadSkillForAudience(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	dir := filepath.Join(root, ".forge", "skills", "probe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: probe\ndescription: d\nemit: both\n---\n\nfor everyone\n\n<!-- @forge-only:start -->\nforge only\n<!-- @forge-only:end -->\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	general, err := LoadSkillForAudience(root, "probe", "general")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(general, []byte("forge only")) || !bytes.Contains(general, []byte("for everyone")) {
		t.Errorf("general audience body = %q, want the @forge-only block stripped and the rest kept", general)
	}
	forge, err := LoadSkillForAudience(root, "probe", "forge")
	if err != nil {
		t.Fatal(err)
	}
	if string(forge) != body {
		t.Errorf("forge audience body = %q, want the file as written", forge)
	}
}
