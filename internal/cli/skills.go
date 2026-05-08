package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// SkillWriteStyle controls the on-disk layout produced by `forge skill write`.
type SkillWriteStyle string

const (
	// SkillWriteStyleForge mirrors forge's own layout: <out>/<skill>/SKILL.md.
	SkillWriteStyleForge SkillWriteStyle = "forge"
	// SkillWriteStyleClaude is Claude Code's `.claude/skills/` layout —
	// same on-disk shape as forge (<out>/<skill>/SKILL.md), but always
	// includes YAML frontmatter so Claude can discover/route the skill.
	SkillWriteStyleClaude SkillWriteStyle = "claude"
	// SkillWriteStyleMD is a flat layout: <out>/<skill>.md, no per-skill dir.
	SkillWriteStyleMD SkillWriteStyle = "md"
)

// newSkillWriteCmd implements `forge skill write` — bulk-export every bundled
// skill to a target directory in one of three layouts. The command lives under
// the existing `forge skill` group (rather than a parallel `forge skills`
// group) so list / load / write share the same noun.
func newSkillWriteCmd() *cobra.Command {
	var (
		outDir string
		style  string
	)
	cmd := &cobra.Command{
		Use:   "write --out <dir> [--style claude|forge|md]",
		Short: "Write every bundled skill to a directory",
		Long: `Bulk-export forge's skills to a target directory.

Layouts:
  --style forge   (default) Forge's native layout: <dir>/<skill>/SKILL.md.
  --style claude            Claude Code-compatible layout: <dir>/<skill>/SKILL.md
                            with YAML frontmatter guaranteed (synthesized if
                            missing). Drop --out at <repo>/.claude/skills/ so
                            an LLM running Claude Code picks them up.
  --style md                Flat: <dir>/<skill>.md, no per-skill subdirectory.

The skill content is the same body returned by ` + "`forge skill load <name>`" + `.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if outDir == "" {
				return fmt.Errorf("--out is required")
			}
			s := SkillWriteStyle(strings.ToLower(strings.TrimSpace(style)))
			switch s {
			case "":
				s = SkillWriteStyleForge
			case SkillWriteStyleForge, SkillWriteStyleClaude, SkillWriteStyleMD:
				// ok
			default:
				return fmt.Errorf("invalid --style %q: valid values are forge, claude, md", style)
			}
			n, err := WriteSkills(outDir, s)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %d skills to %s (style=%s)\n", n, outDir, s)
			return nil
		},
	}
	cmd.Flags().StringVar(&outDir, "out", "", "Target directory (created if missing) — required")
	cmd.Flags().StringVar(&style, "style", string(SkillWriteStyleForge), "Output layout: forge (default), claude, md")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}

// WriteSkills exports every bundled skill into outDir using the requested
// style. It returns the number of skills written. The function is exported so
// callers (e.g. the reliant CLI embedding forge) can reuse it.
func WriteSkills(outDir string, style SkillWriteStyle) (int, error) {
	if outDir == "" {
		return 0, fmt.Errorf("outDir is required")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return 0, fmt.Errorf("create out dir %s: %w", outDir, err)
	}

	// Bulk-export only forge-shipped skills. User/project skills already
	// live on disk under .forge/skills/ or ~/.forge/skills/ and don't need
	// re-emission to a target dir.
	skills, err := listForgeShippedSkills()
	if err != nil {
		return 0, err
	}

	count := 0
	for _, s := range skills {
		body, err := loadSkillContent(s.Path)
		if err != nil {
			return count, fmt.Errorf("load skill %q: %w", s.Path, err)
		}

		// Skill paths can be hierarchical ("debug/investigate"). Flatten
		// hierarchy into a single dir/file name per skill so the output is
		// uniform across styles. Use "-" as the separator: claude expects
		// a flat skill dir under .claude/skills/, and md style needs a
		// flat filename.
		flat := strings.ReplaceAll(s.Path, "/", "-")

		switch style {
		case SkillWriteStyleClaude:
			content := ensureFrontmatter(body, s)
			dir := filepath.Join(outDir, flat)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return count, fmt.Errorf("create %s: %w", dir, err)
			}
			dst := filepath.Join(dir, "SKILL.md")
			if err := os.WriteFile(dst, content, 0o644); err != nil {
				return count, fmt.Errorf("write %s: %w", dst, err)
			}
		case SkillWriteStyleForge:
			dir := filepath.Join(outDir, flat)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return count, fmt.Errorf("create %s: %w", dir, err)
			}
			dst := filepath.Join(dir, "SKILL.md")
			if err := os.WriteFile(dst, body, 0o644); err != nil {
				return count, fmt.Errorf("write %s: %w", dst, err)
			}
		case SkillWriteStyleMD:
			dst := filepath.Join(outDir, flat+".md")
			if err := os.WriteFile(dst, body, 0o644); err != nil {
				return count, fmt.Errorf("write %s: %w", dst, err)
			}
		default:
			return count, fmt.Errorf("unknown style %q", style)
		}
		count++
	}
	return count, nil
}

// loadSkillContent reads a forge-shipped skill's raw bytes from the embedded
// templates. Used by `forge skill write` to bulk-export the bundled skill set
// (which is the only thing it should export; user/project skills already live
// on disk and don't need re-emission).
//
// Unlike `forge skill load`, this does NOT rewrite "forge" -> embedding-binary
// names; the output is the canonical skill text suitable for any consumer.
func loadSkillContent(skillPath string) ([]byte, error) {
	return loadForgeShippedSkill(skillPath)
}

// ensureFrontmatter guarantees the skill body starts with YAML frontmatter
// (`---\n...\n---\n`). All bundled skills already do — this is a safety net
// for any skill whose source file lost its frontmatter, so Claude Code's
// loader has the metadata it needs to route the skill.
func ensureFrontmatter(body []byte, meta skillMeta) []byte {
	if len(body) >= 4 && string(body[:4]) == "---\n" {
		// Already has frontmatter — pass through unmodified.
		return body
	}
	name := meta.Name
	if name == "" {
		name = meta.Path
	}
	desc := meta.Description
	if desc == "" {
		desc = "Forge skill: " + name
	}
	header := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n", name, desc)
	return append([]byte(header), body...)
}
