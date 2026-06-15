package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseFrontmatter_ParsesRelevanceField verifies the `relevance:`
// frontmatter field (and the applies-from/applies-to bounds) flow through
// to skillMeta verbatim.
func TestParseFrontmatter_ParsesRelevanceField(t *testing.T) {
	body := []byte("---\nname: x\ndescription: y\nrelevance: migration\napplies-from: v0.5.0\napplies-to: v0.6.0\n---\nbody\n")
	got := parseFrontmatter(body)
	if got.Relevance != SkillRelevanceMigration {
		t.Errorf("Relevance = %q, want %q", got.Relevance, SkillRelevanceMigration)
	}
	if got.AppliesFrom != "v0.5.0" {
		t.Errorf("AppliesFrom = %q, want v0.5.0", got.AppliesFrom)
	}
	if got.AppliesTo != "v0.6.0" {
		t.Errorf("AppliesTo = %q, want v0.6.0", got.AppliesTo)
	}
}

// TestParseFrontmatter_RelevanceDefaultsToEmpty pins the legacy default:
// skills without the field stay always-relevant.
func TestParseFrontmatter_RelevanceDefaultsToEmpty(t *testing.T) {
	got := parseFrontmatter([]byte("---\nname: x\ndescription: y\n---\nbody\n"))
	if got.Relevance != "" {
		t.Errorf("expected empty Relevance when frontmatter omits the field, got %q", got.Relevance)
	}
}

// TestListForgeShippedSkills_MigrationRelevance verifies that every
// shipped skill under migrations/ carries relevance=migration (stamped
// frontmatter, with the directory-derived default as backstop), and that
// non-migration skills don't.
func TestListForgeShippedSkills_MigrationRelevance(t *testing.T) {
	skills, err := listForgeShippedSkills()
	if err != nil {
		t.Fatalf("listForgeShippedSkills: %v", err)
	}
	migrations := 0
	for _, s := range skills {
		isMigrationDir := strings.HasPrefix(s.Path, "migrations/")
		isTagged := s.Relevance == SkillRelevanceMigration
		if isMigrationDir != isTagged {
			t.Errorf("skill %q: relevance=%q, migrations-dir=%v — the two must agree", s.Path, s.Relevance, isMigrationDir)
		}
		if isTagged {
			migrations++
		}
	}
	if migrations == 0 {
		t.Fatal("no migration skills found — the relevance gate has nothing to gate")
	}
}

// TestListSkillsAtExcludesMigrationsByDefault covers the default listing
// surface (what reliant's cli.ListSkills sees) and the opt-in.
func TestListSkillsAtExcludesMigrationsByDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate from ~/.forge/skills

	byDefault, err := ListSkillsAt("")
	if err != nil {
		t.Fatalf("ListSkillsAt: %v", err)
	}
	for _, m := range byDefault {
		if m.Relevance == SkillRelevanceMigration {
			t.Errorf("default listing contains migration skill %q", m.Path)
		}
	}

	withOpt, err := ListSkillsAtWithOptions("", SkillListOptions{IncludeMigrations: true})
	if err != nil {
		t.Fatalf("ListSkillsAtWithOptions: %v", err)
	}
	if len(withOpt) <= len(byDefault) {
		t.Errorf("opt-in listing (%d) not larger than default (%d) — migrations missing", len(withOpt), len(byDefault))
	}
	var found *SkillMetaPublic
	for i := range withOpt {
		if withOpt[i].Path == "migrations/v0.x-to-contractkit" {
			found = &withOpt[i]
			break
		}
	}
	if found == nil {
		t.Fatal("opt-in listing lacks migrations/v0.x-to-contractkit")
	}
	if found.Relevance != SkillRelevanceMigration {
		t.Errorf("Relevance = %q, want migration", found.Relevance)
	}
}

// TestListSkillsAtExposesAppliesBounds verifies the version bounds pass
// through for migration skills that declare them.
func TestListSkillsAtExposesAppliesBounds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	metas, err := ListSkillsAtWithOptions("", SkillListOptions{IncludeMigrations: true})
	if err != nil {
		t.Fatalf("ListSkillsAtWithOptions: %v", err)
	}
	for _, m := range metas {
		if m.Path != "migrations/dev-target-to-kcl-deploy" {
			continue
		}
		if m.AppliesFrom != "v0.5.0" || m.AppliesTo != "v0.6.0" {
			t.Errorf("applies bounds = [%q, %q), want [v0.5.0, v0.6.0)", m.AppliesFrom, m.AppliesTo)
		}
		return
	}
	t.Fatal("migrations/dev-target-to-kcl-deploy not in opt-in listing")
}

// TestMigrationSkillStillLoadableByPath pins the on-demand escape hatch:
// `forge skill load migrations/<id>` (and forge upgrade list's "To load"
// hint) must keep working even though listings hide migration skills.
func TestMigrationSkillStillLoadableByPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	body, scope, err := ResolveSkillContentAt("", "migrations/v0.x-to-contractkit")
	if err != nil {
		t.Fatalf("ResolveSkillContentAt: %v", err)
	}
	if scope != SkillScopeForge {
		t.Errorf("scope = %q, want forge", scope)
	}
	if !strings.Contains(string(body), "contractkit") {
		t.Error("loaded migration skill body looks wrong")
	}
}

// TestMigrationSkillsSearchable pins that keyword search still surfaces
// migration skills — an explicit query is opt-in by nature.
func TestMigrationSkillsSearchable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	results, err := searchSkills("contractkit")
	if err != nil {
		t.Fatalf("searchSkills: %v", err)
	}
	for _, r := range results {
		if r.Skill.Path == "migrations/v0.x-to-contractkit" {
			return
		}
	}
	t.Error("search did not surface migrations/v0.x-to-contractkit")
}

// TestWriteSkillsSkipsMigrationsByDefault verifies bulk export honors the
// relevance gate and the opt-in restores it.
func TestWriteSkillsSkipsMigrationsByDefault(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteSkills(dir, SkillWriteStyleForge, SkillAudienceAll); err != nil {
		t.Fatalf("WriteSkills: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "migrations-v0.x-to-contractkit", "SKILL.md")); !os.IsNotExist(err) {
		t.Error("default WriteSkills exported a migration skill")
	}

	dir2 := t.TempDir()
	if _, err := WriteSkillsWithOptions(dir2, SkillWriteStyleForge, SkillAudienceAll, SkillListOptions{IncludeMigrations: true}); err != nil {
		t.Fatalf("WriteSkillsWithOptions: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir2, "migrations-v0.x-to-contractkit", "SKILL.md")); err != nil {
		t.Errorf("opt-in WriteSkills missing migration skill: %v", err)
	}
}
