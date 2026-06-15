package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/generator"
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

// SkillAudience names the consumer of a skill emission. Combined with the
// per-skill frontmatter `emit:` field ([SkillEmit]) it decides which skills
// are written and how their bodies are rendered:
//
//	audience=All ("") — every skill, full body (no stripping). Default.
//	audience=General  — emit:general|both skills only; @forge-only blocks stripped.
//	audience=Forge    — emit:forge|both skills only; full body retained.
type SkillAudience string

const (
	// SkillAudienceAll disables filtering entirely. Use when bulk-exporting
	// the canonical catalog (e.g. forge's own .claude/skills/) where the
	// reader can decide what to surface.
	SkillAudienceAll SkillAudience = ""
	// SkillAudienceGeneral targets consumers outside a forge project. The
	// renderer strips `<!-- @forge-only:start/end -->` blocks from emit:both
	// skills, and drops emit:forge skills entirely.
	SkillAudienceGeneral SkillAudience = "general"
	// SkillAudienceForge targets consumers inside a forge project. Full
	// body is preserved; emit:general skills are also included.
	SkillAudienceForge SkillAudience = "forge"
)

// emitMatchesAudience decides whether a skill should be included in an
// emission for the given audience. An empty Emit value is treated as
// SkillEmitForge — legacy skills under templates/project/skills/forge/
// pre-date the field and are framework-specific by default.
func emitMatchesAudience(emit SkillEmit, audience SkillAudience) bool {
	if audience == SkillAudienceAll {
		return true
	}
	if emit == "" {
		emit = SkillEmitForge
	}
	if emit == SkillEmitBoth {
		return true
	}
	return string(emit) == string(audience)
}

// RenderSkillForAudience returns the skill body filtered for the given
// audience. For SkillAudienceGeneral, `<!-- @forge-only:start -->` ...
// `<!-- @forge-only:end -->` blocks are removed (markers included). For
// every other audience the body is returned unchanged.
//
// Exported so out-of-process consumers (e.g. reliant) can apply the same
// filtering rule when serving a skill loaded via [LoadSkill]. Forge-side
// callers should generally use WriteSkills, which calls this internally.
func RenderSkillForAudience(body []byte, audience SkillAudience) []byte {
	if audience != SkillAudienceGeneral {
		return body
	}
	return stripForgeOnlyBlocks(body)
}

// stripForgeOnlyBlocks removes every `@forge-only` block from the body,
// inclusive of the marker lines, and collapses any runs of blank lines
// created by the removal down to a single blank line. An unterminated
// block drops content from the start marker to EOF — authors notice via
// missing content rather than silent passthrough.
func stripForgeOnlyBlocks(body []byte) []byte {
	lines := strings.Split(string(body), "\n")
	out := make([]string, 0, len(lines))
	inBlock := false
	for _, line := range lines {
		switch {
		case isForgeOnlyMarker(line, "start"):
			inBlock = true
		case isForgeOnlyMarker(line, "end"):
			inBlock = false
		case inBlock:
			// skip
		default:
			out = append(out, line)
		}
	}
	return []byte(collapseBlankRuns(strings.Join(out, "\n")))
}

// isForgeOnlyMarker reports whether a single line is the `<!-- @forge-only:start -->`
// or `<!-- @forge-only:end -->` HTML comment, tolerating surrounding
// whitespace both around the line and inside the comment.
func isForgeOnlyMarker(line, kind string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "<!--") || !strings.HasSuffix(trimmed, "-->") {
		return false
	}
	inner := strings.TrimSpace(trimmed[4 : len(trimmed)-3])
	return inner == "@forge-only:"+kind
}

// collapseBlankRuns rewrites consecutive blank lines as a single blank
// line — used after block stripping so removed sections don't leave a
// visible double-blank gap in the rendered output.
func collapseBlankRuns(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	prevBlank := false
	for _, line := range lines {
		blank := strings.TrimSpace(line) == ""
		if blank && prevBlank {
			continue
		}
		out = append(out, line)
		prevBlank = blank
	}
	return strings.Join(out, "\n")
}

// skillStyleForHarness maps a Harness to the SkillWriteStyle that should
// be used when emitting skills on `forge new`. Returns ("", false) for
// harnesses with no native skills concept — reliant (auto-discovers via
// forge.yaml), copilot, codex.
func skillStyleForHarness(h generator.Harness) (SkillWriteStyle, bool) {
	switch h {
	case generator.HarnessClaude:
		return SkillWriteStyleClaude, true
	default:
		return "", false
	}
}

// newSkillWriteCmd implements `forge skill write` — bulk-export every bundled
// skill to a target directory in one of three layouts. The command lives under
// the existing `forge skill` group (rather than a parallel `forge skills`
// group) so list / load / write share the same noun.
func newSkillWriteCmd() *cobra.Command {
	var (
		outDir            string
		style             string
		includeMigrations bool
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

The skill content is the same body returned by ` + "`forge skill load <name>`" + `.

Note: inside a forge project you don't need this for .claude/skills/ —
'forge generate' regenerates the forge-shipped skills there on every run
(Tier-1 tracked), keeping them in sync with the forge binary version.
'skill write' remains for exporting skills to arbitrary locations.`,
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
			n, err := WriteSkillsWithOptions(outDir, s, SkillAudienceAll, SkillListOptions{IncludeMigrations: includeMigrations})
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "wrote %d skills to %s (style=%s)\n", n, outDir, s)
			return nil
		},
	}
	cmd.Flags().StringVar(&outDir, "out", "", "Target directory (created if missing) — required")
	cmd.Flags().StringVar(&style, "style", string(SkillWriteStyleForge), "Output layout: forge (default), claude, md")
	cmd.Flags().BoolVar(&includeMigrations, "include-migrations", false, "Also export one-time migration skills (relevance: migration)")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}

// WriteSkills exports forge-shipped skills into outDir using the requested
// style, filtered by audience. SkillAudienceAll ("") writes every skill
// with the raw body — that's the canonical catalog export used by
// `forge skill write` from inside the forge repo. SkillAudienceGeneral
// drops emit:forge skills and strips @forge-only blocks from emit:both
// skills' bodies. SkillAudienceForge keeps everything for emit:forge|both
// skills and drops emit:general entries. Returns the number of skills
// actually written.
//
// Exported so out-of-process callers (the reliant CLI embedding forge,
// the harness emission in `forge new`) can reuse it.
//
// One-time migration skills (relevance: migration) are excluded — they
// document version transitions, not steady-state conventions, and bulk
// exports are how skill catalogs reach projects. Use
// [WriteSkillsWithOptions] with IncludeMigrations to export them too.
func WriteSkills(outDir string, style SkillWriteStyle, audience SkillAudience) (int, error) {
	return WriteSkillsWithOptions(outDir, style, audience, SkillListOptions{})
}

// WriteSkillsWithOptions is [WriteSkills] with explicit listing options.
// Additive surface — WriteSkills' signature is frozen for embedders.
func WriteSkillsWithOptions(outDir string, style SkillWriteStyle, audience SkillAudience, opts SkillListOptions) (int, error) {
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
	if !opts.IncludeMigrations {
		skills = filterDefaultRelevance(skills)
	}

	count := 0
	for _, s := range skills {
		if !emitMatchesAudience(s.Emit, audience) {
			continue
		}
		body, err := loadSkillContent(s.Path)
		if err != nil {
			return count, fmt.Errorf("load skill %q: %w", s.Path, err)
		}
		body = RenderSkillForAudience(body, audience)

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
