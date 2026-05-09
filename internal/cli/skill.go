package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/templates"
)

// forgeCmdRE matches "forge" used as a CLI command — i.e. followed by a space.
// Duplicated from generator/project.go to avoid a cross-package dependency.
var forgeCmdRE = regexp.MustCompile(`\bforge( )`)

// SkillScope identifies where a skill was discovered from.
type SkillScope string

const (
	// SkillScopeForge is a skill bundled with the forge binary (templates/project/skills).
	SkillScopeForge SkillScope = "forge"
	// SkillScopeProject is a skill discovered under <project_root>/.forge/skills/.
	SkillScopeProject SkillScope = "project"
	// SkillScopeUser is a skill discovered under ~/.forge/skills/.
	SkillScopeUser SkillScope = "user"
)

// skillMeta holds parsed YAML frontmatter from a SKILL.md file plus the scope
// the skill was loaded from.
type skillMeta struct {
	Path        string // e.g. "db", "frontend/state", "debug/investigate"
	Name        string
	Description string
	Scope       SkillScope // forge | project | user (inferred from source)

	// fsPath is non-empty for project/user-scope skills and points at the
	// SKILL.md on disk. For forge-shipped skills it is empty (content is
	// fetched from the embedded templates FS instead).
	fsPath string
}

// body returns the SKILL.md content for this skill.
//
// Forge-shipped skills load from the embedded templates FS; project/user
// skills load from disk via fsPath. Errors propagate to the caller.
func (m skillMeta) body() ([]byte, error) {
	if m.fsPath != "" {
		return os.ReadFile(m.fsPath)
	}
	return loadForgeShippedSkill(m.Path)
}

func newSkillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Manage Forge skills — conventions and playbooks for LLM agents",
	}
	cmd.AddCommand(newSkillListCmd())
	cmd.AddCommand(newSkillLoadCmd())
	cmd.AddCommand(newSkillWriteCmd())
	cmd.AddCommand(newSkillSearchCmd())
	return cmd
}

func newSkillListCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List available skills (forge-shipped, project, and user-global)",
		RunE: func(cmd *cobra.Command, args []string) error {
			skills, err := listSkills()
			if err != nil {
				return err
			}
			if jsonOut {
				return writeSkillsJSON(cmd.OutOrStdout(), skills)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "PATH\tSCOPE\tNAME\tDESCRIPTION")
			for _, s := range skills {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.Path, s.Scope, s.Name, s.Description)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output JSON instead of a tab-separated table")
	return cmd
}

func newSkillLoadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "load <name>",
		Short: "Print a skill's content to stdout (resolves user > project > forge)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			// Allow both "db" and "forge/db"
			name = strings.TrimPrefix(name, "forge/")

			content, scope, err := resolveSkillContent(name)
			if err != nil {
				return cliutil.UserErr(fmt.Sprintf("forge skill load %s", name),
					fmt.Sprintf("skill %q not found", name),
					"",
					"run 'forge skill list' to see available skills, or 'forge skill search <keyword>' to find one")
			}
			_ = scope // available if we want to log; load is silent.

			// Rewrite CLI command references if running under a different binary name.
			cliName := CLIName()
			if cliName != "forge" {
				content = forgeCmdRE.ReplaceAll(content, []byte(cliName+"$1"))
			}

			_, err = cmd.OutOrStdout().Write(content)
			return err
		},
	}
}

// newSkillSearchCmd implements `forge skill search "<query>"`. The scoring
// algorithm is ported verbatim from reliant's skill tool (path/name = 3,
// description = 1, body = 1, summed across query words).
func newSkillSearchCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search skills across all scopes by keyword (path/name=3, desc/body=1)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := args[0]
			results, err := searchSkills(query)
			if err != nil {
				return err
			}
			if jsonOut {
				return writeSearchJSON(cmd.OutOrStdout(), query, results)
			}
			out := cmd.OutOrStdout()
			if len(results) == 0 {
				fmt.Fprintf(out, "No skills found matching query: %s\n", query)
				return nil
			}
			fmt.Fprintf(out, "Skills matching %q:\n\n", query)
			w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "SCORE\tPATH\tSCOPE\tDESCRIPTION")
			for _, r := range results {
				fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", r.Score, r.Skill.Path, r.Skill.Scope, r.Skill.Description)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output JSON instead of a tab-separated table")
	return cmd
}

// skillSearchResult is one scored skill.
type skillSearchResult struct {
	Skill skillMeta `json:"-"`
	Score int       `json:"score"`
}

// searchSkills runs the reliant-ported word-scoring algorithm across all
// skills (forge + project + user). Returns results with score > 0, sorted
// by score desc then path asc.
func searchSkills(query string) ([]skillSearchResult, error) {
	skills, err := listSkills()
	if err != nil {
		return nil, err
	}

	queryLower := strings.ToLower(query)
	queryWords := strings.Fields(queryLower)
	if len(queryWords) == 0 {
		return nil, nil
	}

	var results []skillSearchResult
	for _, s := range skills {
		score := 0
		pathLower := strings.ToLower(s.Path)
		nameLower := strings.ToLower(s.Name)
		descLower := strings.ToLower(s.Description)

		for _, word := range queryWords {
			if strings.Contains(pathLower, word) {
				score += 3
			} else if strings.Contains(nameLower, word) {
				score += 3
			}
			if strings.Contains(descLower, word) {
				score += 1
			}
		}

		// Body match — load lazily; skip on read error.
		if body, err := s.body(); err == nil && len(body) > 0 {
			bodyLower := strings.ToLower(string(body))
			for _, word := range queryWords {
				if strings.Contains(bodyLower, word) {
					score += 1
				}
			}
		}

		if score > 0 {
			results = append(results, skillSearchResult{Skill: s, Score: score})
		}
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].Skill.Path < results[j].Skill.Path
	})
	return results, nil
}

// listSkills returns all available skills (forge-shipped + project + user),
// sorted alphabetically by path. On collision, precedence is:
//
//	forge-shipped < project < user-global
//
// This lets a user override anything the project defines, and a project
// override anything forge ships. Each returned skillMeta carries its Scope.
func listSkills() ([]skillMeta, error) {
	bySource := map[SkillScope][]skillMeta{}

	// Forge-shipped (lowest precedence).
	forgeSkills, err := listForgeShippedSkills()
	if err != nil {
		return nil, fmt.Errorf("list forge skills: %w", err)
	}
	bySource[SkillScopeForge] = forgeSkills

	// Project-scope (.forge/skills under project root).
	if root, err := findProjectRoot(); err == nil && root != "" {
		projSkills, err := listDiskSkills(filepath.Join(root, ".forge", "skills"), SkillScopeProject)
		if err == nil {
			bySource[SkillScopeProject] = projSkills
		}
	}

	// User-global (~/.forge/skills).
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		userSkills, err := listDiskSkills(filepath.Join(home, ".forge", "skills"), SkillScopeUser)
		if err == nil {
			bySource[SkillScopeUser] = userSkills
		}
	}

	// Merge with precedence: forge < project < user.
	merged := map[string]skillMeta{}
	for _, s := range bySource[SkillScopeForge] {
		merged[s.Path] = s
	}
	for _, s := range bySource[SkillScopeProject] {
		merged[s.Path] = s
	}
	for _, s := range bySource[SkillScopeUser] {
		merged[s.Path] = s
	}

	out := make([]skillMeta, 0, len(merged))
	for _, s := range merged {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// listForgeShippedSkills enumerates the skills embedded under
// internal/templates/project/skills/forge/. The on-disk path mirrors the
// skill path (with a "forge" root rolled up to skill path "forge").
func listForgeShippedSkills() ([]skillMeta, error) {
	files, err := templates.ProjectTemplates().List("skills")
	if err != nil {
		return nil, fmt.Errorf("list skill templates: %w", err)
	}
	var out []skillMeta
	for _, rel := range files {
		if !strings.HasSuffix(rel, "/SKILL.md") && rel != "SKILL.md" {
			continue
		}
		skillPath := strings.TrimPrefix(rel, "forge/")
		skillPath = strings.TrimSuffix(skillPath, "/SKILL.md")
		skillPath = strings.TrimSuffix(skillPath, "SKILL.md")
		skillPath = strings.TrimSuffix(skillPath, "/")
		if skillPath == "" {
			skillPath = "forge"
		}
		content, err := templates.ProjectTemplates().Get(path.Join("skills", rel))
		if err != nil {
			continue
		}
		meta := parseFrontmatter(content)
		meta.Path = skillPath
		meta.Scope = SkillScopeForge
		out = append(out, meta)
	}
	return out, nil
}

// listDiskSkills enumerates skills under a filesystem root. Each skill is
// either:
//   - <root>/<name>/SKILL.md   → skill path = <name>
//   - <root>/<a>/<b>/SKILL.md  → skill path = "<a>/<b>" (one level of nesting)
//
// Missing root returns (nil, nil) — absent skill dirs are normal, not errors.
func listDiskSkills(root string, scope SkillScope) ([]skillMeta, error) {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, nil
	}
	var out []skillMeta
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // tolerate per-entry errors
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Base(p) != "SKILL.md" {
			return nil
		}
		// Compute the skill path from the relative directory chain.
		rel, err := filepath.Rel(root, filepath.Dir(p))
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." || rel == "" {
			// SKILL.md directly under root — treat parent dir name as path.
			rel = filepath.Base(filepath.Dir(p))
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		meta := parseFrontmatter(content)
		meta.Path = rel
		meta.Scope = scope
		meta.fsPath = p
		out = append(out, meta)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// resolveSkillContent looks up a skill by path across all scopes, honoring the
// user > project > forge precedence. Returns the body, the scope it came
// from, or an error if the skill is unknown.
func resolveSkillContent(skillPath string) ([]byte, SkillScope, error) {
	skills, err := listSkills()
	if err != nil {
		return nil, "", err
	}
	for _, s := range skills {
		if s.Path == skillPath {
			body, err := s.body()
			if err != nil {
				return nil, s.Scope, err
			}
			return body, s.Scope, nil
		}
	}
	return nil, "", fmt.Errorf("skill %q not found", skillPath)
}

// loadForgeShippedSkill reads a forge-bundled skill's body from the embedded
// templates FS. Path resolution mirrors the existing convention: the root
// "forge" skill lives at skills/forge/SKILL.md; everything else lives at
// skills/forge/<path>/SKILL.md.
func loadForgeShippedSkill(skillPath string) ([]byte, error) {
	skillPath = strings.TrimPrefix(skillPath, "forge/")
	var templatePath string
	if skillPath == "forge" {
		templatePath = path.Join("skills", "forge", "SKILL.md")
	} else {
		templatePath = path.Join("skills", "forge", skillPath, "SKILL.md")
	}
	return templates.ProjectTemplates().Get(templatePath)
}

// parseFrontmatter extracts name and description from YAML frontmatter.
func parseFrontmatter(content []byte) skillMeta {
	s := string(content)
	if !strings.HasPrefix(s, "---\n") {
		return skillMeta{}
	}
	end := strings.Index(s[4:], "\n---")
	if end < 0 {
		return skillMeta{}
	}
	block := s[4 : 4+end]

	var meta skillMeta
	for _, line := range strings.Split(block, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			k = strings.TrimSpace(k)
			v = strings.TrimSpace(v)
			switch k {
			case "name":
				meta.Name = v
			case "description":
				meta.Description = v
			}
		}
	}
	return meta
}

// findProjectRoot walks upward from the cwd looking for a forge.yaml. Returns
// the directory or "" when no project is found. Mirrors the loadProjectConfig
// walk-up behavior in config.go (kept local to skill.go to avoid a circular
// dep on its private helper).
func findProjectRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "forge.yaml")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
}

// jsonSkill is the JSON shape emitted by `skill list --json` and inside
// `skill search --json`. Keep field tags stable — sub-agents parse this.
type jsonSkill struct {
	Path        string     `json:"path"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Scope       SkillScope `json:"scope"`
}

func toJSONSkill(s skillMeta) jsonSkill {
	return jsonSkill{Path: s.Path, Name: s.Name, Description: s.Description, Scope: s.Scope}
}

func writeSkillsJSON(w interface{ Write([]byte) (int, error) }, skills []skillMeta) error {
	out := make([]jsonSkill, 0, len(skills))
	for _, s := range skills {
		out = append(out, toJSONSkill(s))
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func writeSearchJSON(w interface{ Write([]byte) (int, error) }, query string, results []skillSearchResult) error {
	type jsonResult struct {
		Score int       `json:"score"`
		Skill jsonSkill `json:"skill"`
	}
	out := struct {
		Query   string       `json:"query"`
		Results []jsonResult `json:"results"`
	}{Query: query, Results: make([]jsonResult, 0, len(results))}
	for _, r := range results {
		out.Results = append(out.Results, jsonResult{Score: r.Score, Skill: toJSONSkill(r.Skill)})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
