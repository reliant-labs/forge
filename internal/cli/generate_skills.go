// `forge generate` step: version-true delivery of forge-shipped skills.
//
// Skills used to reach generated projects only as a scaffold-time copy
// (or a manual `forge skill write`), after which the on-disk SKILL.md
// files froze at scaffold-date and drifted from the project's pinned
// forge version. This step makes `.claude/skills/` a Tier-1 codegen
// output: every `forge generate` re-renders the forge-namespace skills
// from the embedded templates of the RUNNING forge binary, so the disk
// copies always match the binary that last generated the project.
//
// Scope is strictly the forge-shipped skill set — the step only ever
// writes the exact `.claude/skills/<flat>/SKILL.md` paths derived from
// the embedded skill list. User-created skill directories under
// `.claude/skills/` are never touched (and never deleted).
//
// WHICH projects get that delivery is the harness's call, read from
// forge.yaml's `harness:` (see harnessSkillsDirFor). Only `claude` has a
// native on-disk skills concept; the default `reliant` harness reads
// forge's skills from the binary via `forge skill load`, so it receives
// NOTHING — no directory, no files, no manifest entries. Delivering to it
// anyway put 81 hash-guarded SKILL.md files (the large majority of a
// scaffolded project's Tier-1 surface) into every project and every diff,
// to hand the harness a staler copy of what it already had.
//
// Tier transition: projects that predate this step have skill files
// that carry no forge:hash certification marker — they froze as
// Tier-2/legacy scaffold output. Those entries migrate to Tier-1 on the
// first run: a differing on-disk copy is a stale scaffold-era render,
// NOT a precious user edit, so it is overwritten with a one-line notice
// per file. Once an entry is Tier-1, the standard pre-pipeline stomp
// guard (stepCheckTier1Drift) applies — hand-edits surface as drift and
// require --force / `forge project disown` like any other Tier-1 file.
package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/generator"
)

// agentSkillsDir is the project-relative directory that receives the
// forge-shipped skills in Claude Code layout. It is the ONLY directory
// this step ever writes; which projects get it is decided by
// harnessSkillsDirFor below, from the project's `harness:` setting.
const agentSkillsDir = ".claude/skills"

// harnessSkillsDirFor resolves the project-relative skills directory this
// run should deliver into, or "" for "deliver nothing".
//
// The harness is the whole decision, and it comes from forge.yaml's
// `harness:` key (written by `forge project new --harness`). Delivery is
// exactly generator.Harness.SkillsDir(): `claude` has a native on-disk
// skills concept and gets .claude/skills/; `reliant` does NOT — the
// reliant CLI discovers forge's skills through forge.yaml and `forge
// skill load <name>` prints them straight from the binary, so writing 81
// hash-guarded SKILL.md files into the tree delivers a second, staler
// copy of what the harness already has. cursor/copilot/codex have no
// native skills mechanism and likewise get nothing. That mapping is
// forge's DOCUMENTED --harness contract; this function only reads it, so
// the scaffold path (new.go emitHarnessSkills) and the regenerate path
// can no longer disagree about what a given harness receives.
//
// UNSET (legacy project, or a config that failed to load) returns the
// Claude directory, i.e. "keep doing what forge did before". Every
// project scaffolded before `harness:` existed got .claude/skills/
// unconditionally, and some of those readers depend on it. Silently
// deleting a live skills tree on the first run of a new forge binary
// would be a destructive answer to a question the project never got
// asked; leaving the files and continuing to refresh them is the
// conservative one. Such a project opts out by writing `harness:
// reliant` into forge.yaml — after which the retirement sweep below
// removes the now-unwanted copies.
func harnessSkillsDirFor(ctx *pipelineContext) string {
	if ctx.Cfg == nil || strings.TrimSpace(ctx.Cfg.Harness) == "" {
		return agentSkillsDir // legacy/unknown: preserve pre-harness behavior
	}
	h, err := generator.ParseHarness(strings.TrimSpace(ctx.Cfg.Harness))
	if err != nil {
		// An unrecognized harness is a typo in a hand-edited forge.yaml,
		// not a licence to delete: same conservative answer as unset.
		fmt.Fprintf(os.Stderr, "Warning: forge.yaml harness %q not recognized; delivering skills as before (valid: reliant, claude, cursor, copilot, codex)\n", ctx.Cfg.Harness)
		return agentSkillsDir
	}
	return h.SkillsDir()
}

// skillGeneratedBanner returns the DO-NOT-EDIT banner injected at the
// top of every regenerated SKILL.md body (immediately after the YAML
// frontmatter, which must stay at byte 0 for Claude Code's loader).
func skillGeneratedBanner() string {
	return fmt.Sprintf("<!-- Code generated by forge. DO NOT EDIT. -->\n<!-- forge-owned: regenerated every run — do not edit (forge project disown to take ownership) -->\n<!-- rendered by forge %s; project-specific guidance belongs in your own skills directory -->",
		displayForgeVersion())
}

// displayForgeVersion renders the running forge version for human-facing
// banners: release versions get a "v" prefix when missing; dev builds
// stay as-is ("dev").
func displayForgeVersion() string {
	v := runningForgeVersion()
	if v == "" {
		return "dev"
	}
	if v[0] >= '0' && v[0] <= '9' {
		return "v" + v
	}
	return v
}

// agentOperationPreamble is the shared agent-operation guidance injected
// verbatim at the top of EVERY forge-generated skill (right after the
// generated-by banner). It is defined ONCE here and never duplicated
// per-skill: renderAgentSkill prepends it to every skill body, so editing
// this constant changes the preamble on the next `forge generate` for
// every skill in the project.
//
// Forge is an LLM-first tool — the reader of these skills is an agent, not
// a human. The preamble tells that agent how to operate forge commands
// reliably: run them directly, trust the non-interactive contract, treat
// refusals as runbooks, background the slow ones, and hand back only true
// interactive auth.
//
// The last bullet names `forge skill load`. This file — the delivered
// render under .claude/skills/ — is the copy a harness preloads, and it is
// only as current as the last `forge generate` in this checkout. A reader
// who never learns that the binary can print the skill itself has no way
// to notice they are reading an older vintage, so the pointer belongs on
// every delivered copy rather than in one skill nobody happens to open.
const agentOperationPreamble = `> **Operating forge as an agent.** You run these forge commands yourself — don't ask the human to run them and paste the output back.
> - forge commands are non-interactive without a TTY: they will never hang waiting for input. Each either applies a safe default or fails fast.
> - A forge refusal or error is a runbook: it states what was expected vs. what was found and gives the literal fix command. Apply that fix and retry.
> - Background long-running commands (` + "`forge env up`" + `, ` + "`forge env deploy`" + `, heavy ` + "`forge generate`" + `) and foreground quick ones, so you can iterate in a single turn.
> - Hand back to the human only for interactive auth you cannot perform (e.g. ` + "`gcloud auth login`" + `).
> - This is a delivered render. ` + "`forge skill load <name>`" + ` prints the copy your forge binary ships — authoritative when the two differ — and ` + "`forge skill list`" + ` names them all. Your own guidance goes in your own skills dir; edits here are overwritten.

`

// renderAgentSkill produces the final on-disk bytes for one forge-shipped
// skill: frontmatter (guaranteed), then the generated-by banner, then the
// shared agent-operation preamble, then the body. The banner+preamble go
// after the frontmatter close so the YAML block stays at byte 0 (Claude
// Code's loader requires that).
func renderAgentSkill(meta skillMeta, body []byte) []byte {
	content := ensureFrontmatter(body, meta)
	chunk := []byte(skillGeneratedBanner() + "\n\n" + agentOperationPreamble)
	return insertAfterFrontmatter(content, chunk)
}

// agentSkillRelPath maps a forge skill path (e.g. "frontend/state") to its
// project-relative Claude Code location. Hierarchical skill paths flatten
// with "-" to match the layout `forge skill write --style claude` used,
// so existing scaffolded projects migrate in place.
func agentSkillRelPath(skillsDir, skillPath string) string {
	flat := strings.ReplaceAll(skillPath, "/", "-")
	return filepath.Join(skillsDir, flat, "SKILL.md")
}

// stepAgentSkills re-renders every forge-shipped skill into
// .claude/skills/ as Tier-1 tracked files. See the package comment at the
// top of this file for the full delivery + tier-transition contract.
//
// One-time migration skills (relevance: migration) are NOT delivered:
// they document a specific forge version transition and are retrieval
// noise for every project not mid-transition. They stay reachable on
// demand via `forge skill load migrations/<id>` and are surfaced with
// project-aware version gating by `forge project upgrade list`. Previously
// delivered copies (and any skill forge has since retired or renamed)
// are garbage-collected by gcRetiredAgentSkills below.
func stepAgentSkills(ctx *pipelineContext) error {
	// Harness gate. A harness with no on-disk skills concept — the default
	// `reliant`, plus cursor/copilot/codex — gets NOTHING written: no
	// directory, no files, no manifest entries. The retirement sweep still
	// runs so a project that switches to such a harness has its previously
	// delivered copies cleaned up rather than left to rot.
	skillsDir := harnessSkillsDirFor(ctx)
	if skillsDir == "" {
		gcRetiredAgentSkills(ctx, agentSkillsDir, nil)
		return nil
	}

	skills, err := listForgeShippedSkills()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: agent skills generation failed: %v\n", err)
		return nil
	}
	skills = filterDefaultRelevance(skills)

	cs := ctx.Checksums
	wrote := 0
	expected := make(map[string]bool, len(skills))
	for _, s := range skills {
		body, err := loadForgeShippedSkill(s.Path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: load skill %q: %v\n", s.Path, err)
			continue
		}
		content := renderAgentSkill(s, body)
		rel := agentSkillRelPath(skillsDir, s.Path)
		expected[rel] = true

		// Tier transition: an on-disk copy WITHOUT a certification
		// marker is a stale scaffold-era render (or first-time
		// delivery) — overwrite, with a one-line notice when the bytes
		// actually differ. Steady-state marker-bearing copies
		// regenerate unconditionally: the pre-pipeline stomp guard
		// already adjudicated hand-edit drift (--force / `forge
		// disown`), so force=true here cannot stomp anything the guard
		// didn't approve. Disowned paths are skipped by the writer
		// chokepoint (user-owned).
		if old, err := os.ReadFile(filepath.Join(ctx.AbsPath, rel)); err == nil {
			if checksums.Verify(old) == checksums.NoMarker && !cs.IsDisowned(filepath.ToSlash(rel)) && checksums.BodyHash(old) != checksums.BodyHash(content) {
				fmt.Printf("   refreshed stale skill: %s\n", rel)
			}
		}

		ok, err := checksums.WriteGeneratedFileTier1(ctx.AbsPath, rel, content, cs, true)
		if err != nil {
			return fmt.Errorf("write skill %s: %w", rel, err)
		}
		if ok {
			wrote++
		}
	}
	if wrote > 0 {
		fmt.Printf("🧠 Refreshed %d forge skills in %s/ (Tier-1, regenerated every run)\n", wrote, skillsDir)
	}

	gcRetiredAgentSkills(ctx, skillsDir, expected)
	return nil
}

// gcRetiredAgentSkills removes previously delivered skills that the
// current forge binary no longer ships into projects: retired/renamed
// skills, and migration skills delivered before they were excluded from
// default delivery.
//
// Why this is safe to do unconditionally (unlike the general
// report-only stale sweep in cleanupStaleArtifacts):
//
//   - Scope is exactly the manifest entries under .claude/skills/, and
//     the ONLY writer of tracked paths there is this step. A tracked
//     entry not in the expected render set is by construction a skill
//     forge delivered and has since stopped shipping.
//   - User-created skill dirs are never checksum-tracked (see
//     TestStepAgentSkillsNeverTouchesUserSkillDirs), so they can never
//     become candidates.
//   - Banner backstop: the on-disk file must still carry the
//     "Code generated by forge" marker. A user who replaced a retired
//     skill's content keeps the file — only the manifest entry is
//     dropped (forge stops claiming a path it no longer manages).
//
// Without this, retired skills lingered forever as Tier-1 entries: the
// general stale sweep only reports them (deletion is gated on
// --force-cleanup, and the sweep itself is gated on codegen being
// active), so every regenerated project accumulated dead guidance.
func gcRetiredAgentSkills(ctx *pipelineContext, skillsDir string, expected map[string]bool) {
	cs := ctx.Checksums

	// Enumerate delivered skills from disk: every SKILL.md under
	// .claude/skills/ that isn't in this version's expected set is a
	// retirement candidate. Authorship is read from the file itself.
	// A nil `expected` (the harness no longer takes on-disk delivery at
	// all) makes EVERY forge-authored copy a candidate — the same
	// authorship checks below still apply, so a hand-edited or disowned
	// file is kept and only forge's own renders are withdrawn.
	skillsRoot := filepath.Join(ctx.AbsPath, skillsDir)
	dirs, err := os.ReadDir(skillsRoot)
	if err != nil {
		return // no skills dir — nothing delivered, nothing retired
	}
	var stale []string
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		rel := filepath.Join(skillsDir, d.Name(), "SKILL.md")
		if expected[rel] {
			continue
		}
		if _, statErr := os.Stat(filepath.Join(ctx.AbsPath, rel)); statErr == nil {
			stale = append(stale, rel)
		}
	}
	sort.Strings(stale)

	removed := 0
	for _, rel := range stale {
		if cs.IsDisowned(filepath.ToSlash(rel)) {
			continue // user-owned by recorded intent
		}
		full := filepath.Join(ctx.AbsPath, rel)
		body, err := os.ReadFile(full)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: retired-skill cleanup: could not read %s: %v\n", full, err)
			continue
		}
		// Only delete what certifies (or, for pre-marker deliveries,
		// banners) itself as forge output. A hand-edited marker
		// (Modified) means the content is the user's now — leave it.
		switch checksums.Verify(body) {
		case checksums.Modified:
			continue
		case checksums.NoMarker:
			if !bytes.Contains(body, []byte("Code generated by forge")) {
				continue
			}
		}
		if err := os.Remove(full); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: retired-skill cleanup: could not remove %s: %v\n", full, err)
			continue
		}
		removed++
		// Best-effort prune of the now-empty per-skill directory.
		_ = os.Remove(filepath.Dir(full))
	}
	if removed > 0 {
		fmt.Printf("🧹 Removed %d retired skill(s) from %s/ (no longer shipped by this forge version)\n", removed, skillsDir)
	}
	// Prune the skills root (and .claude/ itself) once emptied — a harness
	// that takes no on-disk delivery should leave no husk behind. Both
	// removes are best-effort and only succeed on an EMPTY directory, so a
	// project with its own skills, or any other .claude/ content, keeps it.
	if removed > 0 {
		_ = os.Remove(skillsRoot)
		_ = os.Remove(filepath.Dir(skillsRoot))
	}
}
