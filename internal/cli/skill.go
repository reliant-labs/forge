package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/reliant-labs/forge/internal/buildinfo"
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

// SkillEmit declares which audience(s) a skill is authored for. Read from
// the YAML frontmatter `emit:` field. Drives the dual-audience compile
// path: a single SKILL.md source can target general consumers, forge
// consumers, or both — with `<!-- @forge-only:start/end -->` blocks
// stripped from the body when emitting to a general audience.
//
// An empty value is treated as SkillEmitForge by [emitMatchesAudience] —
// legacy skills shipped under templates/project/skills/forge/ pre-date
// the field and are framework-specific by default.
type SkillEmit string

const (
	// SkillEmitForge — framework skills (proto, db, api, etc.) that only
	// make sense in a forge project. The legacy default.
	SkillEmitForge SkillEmit = "forge"
	// SkillEmitGeneral — methodology skills (debug, code-review, etc.)
	// that apply to any project, forge or not.
	SkillEmitGeneral SkillEmit = "general"
	// SkillEmitBoth — the body has both general and framework content,
	// with the latter inside `@forge-only` blocks. The renderer keeps the
	// whole body for forge audiences and strips the blocks for general.
	SkillEmitBoth SkillEmit = "both"
)

// skillMeta holds parsed YAML frontmatter from a SKILL.md file plus the scope
// the skill was loaded from.
type skillMeta struct {
	Path        string // e.g. "db", "frontend/state", "debug/investigate"
	Name        string
	Description string
	Scope       SkillScope // forge | project | user (inferred from source)
	Emit        SkillEmit  // forge | general | both (from frontmatter; empty == legacy forge default)

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
			_, _ = fmt.Fprintln(w, "PATH\tSCOPE\tNAME\tDESCRIPTION")
			for _, s := range skills {
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.Path, s.Scope, s.Name, s.Description)
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

			// Resolve via the exported wrapper so the version-skew
			// advisory (running forge vs. project forge_version pin)
			// is included exactly as harness consumers see it.
			root, _ := findProjectRoot()
			content, scope, err := ResolveSkillContentAt(root, name)
			if err != nil {
				return cliutil.UserErr(fmt.Sprintf("forge skill load %s", name),
					fmt.Sprintf("skill %q not found", name),
					"",
					"run 'forge skill list' to see available skills, or 'forge skill search <keyword>' to find one")
			}
			_ = scope // available if we want to log; load is silent.

			// Rewrite CLI command references if running under a different binary name.
			cliName := Name()
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
				_, _ = fmt.Fprintf(out, "No skills found matching query: %s\n", query)
				return nil
			}
			_, _ = fmt.Fprintf(out, "Skills matching %q:\n\n", query)
			w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "SCORE\tPATH\tSCOPE\tDESCRIPTION")
			for _, r := range results {
				_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", r.Score, r.Skill.Path, r.Skill.Scope, r.Skill.Description)
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
				score++
			}
		}

		// Body match — load lazily; skip on read error.
		if body, err := s.body(); err == nil && len(body) > 0 {
			bodyLower := strings.ToLower(string(body))
			for _, word := range queryWords {
				if strings.Contains(bodyLower, word) {
					score++
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

// SkillMetaPublic is the cross-package view of a skill returned from
// [ListSkillsAt]. Only metadata is included; bodies are fetched separately
// via [ResolveSkillContentAt] to keep enumeration cheap.
type SkillMetaPublic struct {
	Path        string
	Name        string
	Description string
	Scope       SkillScope
	Emit        SkillEmit

	// SkillForgeVersion is the forge version the skill content ships with —
	// i.e. the version of the forge module/binary serving this listing.
	// Empty for project/user-scope skills (their content is user-owned and
	// not tied to a forge release).
	SkillForgeVersion string
	// ProjectForgeVersion is the forge_version pinned in the project's
	// forge.yaml ("" when no project root was given, no pin exists, or the
	// config could not be read).
	ProjectForgeVersion string
	// VersionSkew is true when SkillForgeVersion and ProjectForgeVersion
	// are both known, comparable release versions and differ — i.e. the
	// skill content served here comes from a different forge version than
	// the one the project was generated with.
	VersionSkew bool
}

// ListSkillsAt is the exported wrapper around [listSkillsAt], intended for
// consumers in sibling packages (e.g. forge/cli's public shim). It hides the
// unexported skillMeta type behind a stable struct.
func ListSkillsAt(projectRoot string) ([]SkillMetaPublic, error) {
	metas, err := listSkillsAt(projectRoot)
	if err != nil {
		return nil, err
	}
	binaryVersion := runningForgeVersion()
	projectVersion := projectForgeVersionAt(projectRoot)
	skew := isForgeVersionSkew(binaryVersion, projectVersion)
	out := make([]SkillMetaPublic, 0, len(metas))
	for _, m := range metas {
		pub := SkillMetaPublic{
			Path:                m.Path,
			Name:                m.Name,
			Description:         m.Description,
			Scope:               m.Scope,
			Emit:                m.Emit,
			ProjectForgeVersion: projectVersion,
		}
		if m.Scope == SkillScopeForge {
			pub.SkillForgeVersion = binaryVersion
			pub.VersionSkew = skew
		}
		out = append(out, pub)
	}
	return out, nil
}

// ResolveSkillContentAt is the exported wrapper around [resolveSkillContentAt],
// with one addition for out-of-process consumers: when the skill is
// forge-shipped and the running forge version differs from the project's
// pinned forge_version, a one-line advisory is prepended to the body
// (after the YAML frontmatter) so the reader knows the guidance may not
// match the project's generated code.
func ResolveSkillContentAt(projectRoot, skillPath string) ([]byte, SkillScope, error) {
	body, scope, err := resolveSkillContentAt(projectRoot, skillPath)
	if err != nil || scope != SkillScopeForge {
		return body, scope, err
	}
	binaryVersion := runningForgeVersion()
	projectVersion := projectForgeVersionAt(projectRoot)
	if !isForgeVersionSkew(binaryVersion, projectVersion) {
		return body, scope, nil
	}
	advisory := fmt.Sprintf("Note: this guidance is from forge %s; this project pins forge %s. Prefer `forge map --json`/`forge audit --json` for current project facts.\n",
		binaryVersion, projectVersion)
	return insertAfterFrontmatter(body, []byte(advisory)), scope, nil
}

// forgeModulePath is forge's Go module path, used to discover the forge
// version when forge is linked into another binary (e.g. reliant imports
// forge as a module dependency).
const forgeModulePath = "github.com/reliant-labs/forge"

// runningForgeVersion returns the version of the forge code that is
// actually executing — NOT necessarily the main module's version:
//
//  1. If the main module IS forge, use buildinfo (ldflags-stamped, with
//     a module-version fallback).
//  2. If forge is linked in as a dependency (reliant et al.), return the
//     dependency's resolved module version.
//  3. Otherwise fall back to buildinfo (covers ldflags-stamped embedding
//     binaries and "dev" local builds).
func runningForgeVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Path != forgeModulePath {
			for _, dep := range info.Deps {
				if dep == nil || dep.Path != forgeModulePath {
					continue
				}
				m := dep
				if m.Replace != nil {
					m = m.Replace
				}
				if m.Version != "" && m.Version != "(devel)" {
					return m.Version
				}
			}
		}
	}
	return buildinfo.Version()
}

// projectForgeVersionAt reads the forge_version pin from
// <projectRoot>/forge.yaml. Returns "" when projectRoot is empty, the file
// is missing/unreadable, or no pin is declared. The parse is intentionally
// tolerant (single-field unmarshal, not LoadStrict) — version-skew
// annotation must work even across config-schema drift between forge
// versions.
func projectForgeVersionAt(projectRoot string) string {
	if projectRoot == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(projectRoot, "forge.yaml"))
	if err != nil {
		return ""
	}
	var pin struct {
		ForgeVersion string `yaml:"forge_version"`
	}
	if err := yaml.Unmarshal(data, &pin); err != nil {
		return ""
	}
	return strings.TrimSpace(pin.ForgeVersion)
}

// isForgeVersionSkew reports whether the running forge version and the
// project's pinned forge_version are both real, comparable versions AND
// differ. Mirrors forgeVersionMismatchWarning's comparability rules:
// unreleased binary versions (dev / (devel) / pseudoversions) and missing
// project pins never count as skew — we don't want advisory noise during
// local tip-of-tree development or on legacy projects without a baseline.
func isForgeVersionSkew(binaryVersion, projectVersion string) bool {
	binaryVersion = strings.TrimSpace(binaryVersion)
	projectVersion = strings.TrimSpace(projectVersion)
	if isUnreleasedBinaryVersion(binaryVersion) || projectVersion == "" {
		return false
	}
	return strings.TrimPrefix(binaryVersion, "v") != strings.TrimPrefix(projectVersion, "v")
}

// insertAfterFrontmatter inserts chunk immediately after the YAML
// frontmatter block of a SKILL.md body (frontmatter must stay at byte 0
// for skill loaders). When the body has no parseable frontmatter, chunk
// is prepended.
func insertAfterFrontmatter(content, chunk []byte) []byte {
	if len(content) >= 4 && string(content[:4]) == "---\n" {
		if end := strings.Index(string(content[4:]), "\n---"); end >= 0 {
			closeStart := 4 + end + 1 // start of the closing "---" line
			rest := content[closeStart:]
			if nl := strings.IndexByte(string(rest), '\n'); nl >= 0 {
				insertAt := closeStart + nl + 1
				out := make([]byte, 0, len(content)+len(chunk))
				out = append(out, content[:insertAt]...)
				out = append(out, chunk...)
				out = append(out, content[insertAt:]...)
				return out
			}
		}
	}
	out := make([]byte, 0, len(content)+len(chunk))
	out = append(out, chunk...)
	out = append(out, content...)
	return out
}

// listSkills returns all available skills (forge-shipped + project + user),
// sorted alphabetically by path. The project root is discovered by walking
// upward from the cwd; see [listSkillsAt] for explicit-root callers.
func listSkills() ([]skillMeta, error) {
	root, _ := findProjectRoot()
	return listSkillsAt(root)
}

// listSkillsAt is like [listSkills] but takes the project root as an argument
// rather than walking up from cwd. An empty projectRoot skips the project
// scope (only forge-shipped + user-global skills are returned).
//
// Precedence on path collision: forge-shipped < project < user-global. This
// lets a user override anything the project defines, and a project override
// anything forge ships. Each returned skillMeta carries its Scope.
func listSkillsAt(projectRoot string) ([]skillMeta, error) {
	bySource := map[SkillScope][]skillMeta{}

	// Forge-shipped (lowest precedence).
	forgeSkills, err := listForgeShippedSkills()
	if err != nil {
		return nil, fmt.Errorf("list forge skills: %w", err)
	}
	bySource[SkillScopeForge] = forgeSkills

	// Project-scope (.forge/skills under project root).
	if projectRoot != "" {
		projSkills, err := listDiskSkills(filepath.Join(projectRoot, ".forge", "skills"), SkillScopeProject)
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
// internal/templates/project/skills/. Two category roots are recognized:
//
//   - skills/forge/...    → framework skills, default Emit = "forge".
//     Path is "<rest>" (e.g. skills/forge/db/SKILL.md → "db"). The
//     skills/forge/SKILL.md root collapses to the synthetic path "forge".
//   - skills/general/...  → methodology skills, default Emit = "general".
//     Path is "<rest>" (e.g. skills/general/code-review/SKILL.md → "code-review").
//
// Files outside either root are skipped — every shipped skill must live
// under one of the two category dirs. Per-skill frontmatter `emit:`
// overrides the directory-derived default; this is how `debug` (under
// skills/forge/) declares emit:both to surface in non-forge projects too.
func listForgeShippedSkills() ([]skillMeta, error) {
	const (
		forgeDir   = "forge"
		generalDir = "general"
	)
	files, err := templates.ProjectTemplates().List("skills")
	if err != nil {
		return nil, fmt.Errorf("list skill templates: %w", err)
	}
	var out []skillMeta
	for _, rel := range files {
		if !strings.HasSuffix(rel, "/SKILL.md") && rel != "SKILL.md" {
			continue
		}
		var (
			defaultEmit SkillEmit
			skillPath   string
		)
		switch {
		case rel == forgeDir+"/SKILL.md":
			defaultEmit = SkillEmitForge
			skillPath = forgeDir // synthetic "forge" parent skill
		case strings.HasPrefix(rel, forgeDir+"/"):
			defaultEmit = SkillEmitForge
			skillPath = strings.TrimSuffix(strings.TrimPrefix(rel, forgeDir+"/"), "/SKILL.md")
		case strings.HasPrefix(rel, generalDir+"/"):
			defaultEmit = SkillEmitGeneral
			skillPath = strings.TrimSuffix(strings.TrimPrefix(rel, generalDir+"/"), "/SKILL.md")
		default:
			continue
		}
		content, err := templates.ProjectTemplates().Get(path.Join("skills", rel))
		if err != nil {
			continue
		}
		meta := parseFrontmatter(content)
		if meta.Emit == "" {
			meta.Emit = defaultEmit
		}
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

// resolveSkillContentAt looks up a skill by path across all scopes, honoring
// the user > project > forge precedence, scoped to an explicit project root.
// Returns the body, the scope it came from, or an error if the skill is
// unknown.
func resolveSkillContentAt(projectRoot, skillPath string) ([]byte, SkillScope, error) {
	skills, err := listSkillsAt(projectRoot)
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

// loadForgeShippedSkill reads a forge-bundled skill's body from the
// embedded templates FS. Tries the forge category first (back-compat
// with the long-standing "forge skills live under skills/forge/" path
// shape, and the synthetic "forge" parent skill at skills/forge/SKILL.md),
// then falls back to the general category. Returns the body from
// whichever root resolves; both errors only propagate when neither exists.
func loadForgeShippedSkill(skillPath string) ([]byte, error) {
	skillPath = strings.TrimPrefix(skillPath, "forge/")
	var forgeTemplatePath string
	if skillPath == "forge" {
		forgeTemplatePath = path.Join("skills", "forge", "SKILL.md")
	} else {
		forgeTemplatePath = path.Join("skills", "forge", skillPath, "SKILL.md")
	}
	if body, err := templates.ProjectTemplates().Get(forgeTemplatePath); err == nil {
		return body, nil
	}
	generalTemplatePath := path.Join("skills", "general", skillPath, "SKILL.md")
	return templates.ProjectTemplates().Get(generalTemplatePath)
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
			case "emit":
				meta.Emit = SkillEmit(v)
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
	Emit        SkillEmit  `json:"emit,omitempty"`
}

func toJSONSkill(s skillMeta) jsonSkill {
	return jsonSkill{Path: s.Path, Name: s.Name, Description: s.Description, Scope: s.Scope, Emit: s.Emit}
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
