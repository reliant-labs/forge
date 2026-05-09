package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteSkills_Forge verifies the default forge layout writes one
// SKILL.md per skill, and every bundled skill is represented.
func TestWriteSkills_Forge(t *testing.T) {
	dir := t.TempDir()
	n, err := WriteSkills(dir, SkillWriteStyleForge)
	if err != nil {
		t.Fatalf("WriteSkills: %v", err)
	}
	if n == 0 {
		t.Fatal("expected at least one skill to be written")
	}

	skills, err := listForgeShippedSkills()
	if err != nil {
		t.Fatalf("listForgeShippedSkills: %v", err)
	}
	if n != len(skills) {
		t.Errorf("count mismatch: WriteSkills wrote %d, listSkills returned %d", n, len(skills))
	}

	for _, s := range skills {
		flat := strings.ReplaceAll(s.Path, "/", "-")
		dst := filepath.Join(dir, flat, "SKILL.md")
		info, err := os.Stat(dst)
		if err != nil {
			t.Errorf("skill %q: missing %s: %v", s.Path, dst, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("skill %q: %s is empty", s.Path, dst)
		}
	}
}

// TestWriteSkills_Claude verifies the claude layout adds frontmatter when
// missing. All bundled skills already have frontmatter, so we just verify
// pass-through here, plus the synthetic-frontmatter helper directly.
func TestWriteSkills_Claude(t *testing.T) {
	dir := t.TempDir()
	n, err := WriteSkills(dir, SkillWriteStyleClaude)
	if err != nil {
		t.Fatalf("WriteSkills: %v", err)
	}
	if n == 0 {
		t.Fatal("expected at least one skill written")
	}

	// Spot-check a known skill — every bundled skill ships frontmatter.
	body, err := os.ReadFile(filepath.Join(dir, "architecture", "SKILL.md"))
	if err != nil {
		t.Fatalf("read architecture/SKILL.md: %v", err)
	}
	if !strings.HasPrefix(string(body), "---\n") {
		t.Errorf("architecture/SKILL.md missing frontmatter: starts with %q", string(body[:min(40, len(body))]))
	}
	if !strings.Contains(string(body), "name: architecture") {
		t.Errorf("architecture/SKILL.md frontmatter missing name field")
	}
}

// TestEnsureFrontmatter_Synthesizes covers the safety-net branch where a
// skill's source file is missing frontmatter — the helper must inject a
// minimal `---` block derived from the skill's metadata.
func TestEnsureFrontmatter_Synthesizes(t *testing.T) {
	body := []byte("# A skill body\nno frontmatter here\n")
	meta := skillMeta{Name: "demo", Description: "demo desc", Path: "demo"}
	out := ensureFrontmatter(body, meta)
	got := string(out)
	if !strings.HasPrefix(got, "---\n") {
		t.Fatalf("expected frontmatter prefix, got %q", got[:min(40, len(got))])
	}
	if !strings.Contains(got, "name: demo") || !strings.Contains(got, "description: demo desc") {
		t.Errorf("synthesized frontmatter missing fields: %q", got)
	}
	if !strings.HasSuffix(got, "# A skill body\nno frontmatter here\n") {
		t.Errorf("body should be preserved after frontmatter, got %q", got)
	}
}

// TestEnsureFrontmatter_PassthroughWhenPresent verifies that bodies which
// already start with `---\n` are returned unchanged.
func TestEnsureFrontmatter_PassthroughWhenPresent(t *testing.T) {
	body := []byte("---\nname: foo\n---\n# body\n")
	out := ensureFrontmatter(body, skillMeta{})
	if string(out) != string(body) {
		t.Errorf("expected pass-through, got %q", string(out))
	}
}

// TestWriteSkills_MD verifies the flat layout writes one .md file per skill.
func TestWriteSkills_MD(t *testing.T) {
	dir := t.TempDir()
	n, err := WriteSkills(dir, SkillWriteStyleMD)
	if err != nil {
		t.Fatalf("WriteSkills: %v", err)
	}
	if n == 0 {
		t.Fatal("expected at least one skill written")
	}

	skills, err := listForgeShippedSkills()
	if err != nil {
		t.Fatalf("listForgeShippedSkills: %v", err)
	}
	for _, s := range skills {
		flat := strings.ReplaceAll(s.Path, "/", "-")
		dst := filepath.Join(dir, flat+".md")
		info, err := os.Stat(dst)
		if err != nil {
			t.Errorf("skill %q: missing %s: %v", s.Path, dst, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("skill %q: %s is empty", s.Path, dst)
		}
	}
}

// TestWriteSkills_RejectEmptyOut ensures the writer fails fast when called
// without an output directory.
func TestWriteSkills_RejectEmptyOut(t *testing.T) {
	if _, err := WriteSkills("", SkillWriteStyleForge); err == nil {
		t.Fatal("expected error when outDir is empty")
	}
}
