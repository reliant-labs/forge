package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/cliutil"
)

// entryPointSkillPath is the skill that orients a reader before they pick
// from the rest — the synthetic "forge" parent (skills/forge/SKILL.md).
// TestSkillListNamesTheEntryPoint pins that it still ships.
const entryPointSkillPath = "forge"

func newSkillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Manage Forge skills — conventions and playbooks for LLM agents",
	}
	cmd.AddCommand(newSkillListCmd())
	cmd.AddCommand(newSkillLoadCmd())
	cmd.AddCommand(newSkillSearchCmd())
	return cmdutil.StrictGroup(cmd)
}

func newSkillListCmd() *cobra.Command {
	var (
		jsonOut           bool
		includeMigrations bool
		all               bool
		withBodies        bool
	)
	cmd := &cobra.Command{
		Use:   "list [group]",
		Short: "List available skills (forge-shipped, project, and user-global)",
		Args:  cobra.MaximumNArgs(1),
		Long: `List available skills (forge-shipped, project, and user-global).

The default view is GROUPED and fits on one screen: the start-here skills
first, then one row per skill group with its sub-skills collapsed into a
'+N' count. Every collapsed sub-skill is still loadable by its exact path
('forge skill load db/seeding'). Pass --all for the exhaustive flat table, or a
group name ('forge skill list observability') for that group's parent and every
sub-skill, each with its full path and description.

In a directory with no forge.yaml (an existing app, any language), the skills
that declare 'applies: any' come first, as APPLIES TO YOUR REPO: the ones that
work there as it is, led by 'adopt'.

'forge skill search <keyword>' is usually faster than reading either view —
it scores paths, descriptions and BODIES, so it finds skills whose title
never mentions your term.

One-time migration playbooks (relevance: migration — the skills under
migrations/) are hidden by default; they only matter while crossing a
specific forge version transition. Pass --include-migrations to list
them, or use 'forge project upgrade list' to see the migrations that actually
apply to this project's pinned forge_version.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			skills, err := listSkills()
			if err != nil {
				return err
			}
			if !includeMigrations {
				skills = filterDefaultRelevance(skills)
			}
			if len(args) == 1 {
				if skills = skillsInGroup(skills, args[0]); len(skills) == 0 {
					return cliutil.UserErr(fmt.Sprintf("forge skill list %s", args[0]),
						fmt.Sprintf("no skill or group named %q", args[0]), "",
						"run 'forge skill list' to see the groups, or 'forge skill search <keyword>' to find a skill")
				}
				all = true
			}
			// --json is the sub-agent contract: a flat array of every
			// skill, unchanged by the grouping the human-facing view does.
			if jsonOut && withBodies {
				return writeSkillsWithBodiesJSON(cmd.OutOrStdout(), skills)
			}
			if jsonOut {
				return writeSkillsJSON(cmd.OutOrStdout(), skills)
			}
			root, _ := findProjectRoot()
			return writeSkillListText(cmd.OutOrStdout(), skills, all, root != "")
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output JSON instead of a tab-separated table")
	cmd.Flags().BoolVar(&all, "all", false, "Print the exhaustive flat table (every sub-skill, full descriptions) instead of the grouped view")
	cmd.Flags().BoolVar(&includeMigrations, "include-migrations", false, "Include one-time migration skills (relevance: migration) in the listing")
	cmd.Flags().BoolVar(&withBodies, "with-bodies", false, "With --json: carry each skill's body, exactly as skill load prints it (one call for a whole catalog)")
	return cmd
}

// writeSkillListText is `skill list`'s human view of skills: grouped, or the
// flat table with all. The hosted MCP skill_list tool renders through it too.
func writeSkillListText(out io.Writer, skills []skillMeta, all, inProject bool) error {
	if !all {
		return writeGroupedSkillsFor(out, skills, inProject)
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "PATH\tSCOPE\tNAME\tDESCRIPTION")
	entryPoint := false
	for _, s := range skills {
		if s.Path == entryPointSkillPath {
			entryPoint = true
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.Path, s.Scope, s.Name, s.Description)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	// An alphabetical table has no "read this one first", and the
	// entry-point skill is exactly the one a reader needs before
	// choosing from the other ninety. Name it, when it is here.
	if entryPoint {
		_, _ = fmt.Fprintf(out,
			"\nStart with `%s skill load %s` — it names the commands that print forge's whole surface.\n",
			cmdutil.Name(), entryPointSkillPath)
	}
	return nil
}

// skillsInGroup keeps the skill at group and everything that collapses into it
// in the grouped listing: path children ("observability/query") and name
// siblings ("api-rest" under "api").
func skillsInGroup(skills []skillMeta, group string) []skillMeta {
	group = strings.TrimSuffix(strings.TrimPrefix(group, "forge/"), "/")
	var out []skillMeta
	for _, s := range skills {
		if s.Path == group || strings.HasPrefix(s.Path, group+"/") || strings.HasPrefix(s.Path, group+"-") {
			out = append(out, s)
		}
	}
	return out
}

func newSkillLoadCmd() *cobra.Command {
	var window skillLoadWindow
	cmd := &cobra.Command{
		Use:   "load <name>",
		Short: "Print a skill's content to stdout (resolves user > project > forge)",
		Example: `  forge skill load api --section "Domain sentinels"
  forge skill load contracts --regex 'mock|contract' -i --context 2`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("section") && strings.TrimSpace(window.section) == "" {
				return cliutil.UserErrf(cmd.CommandPath(), "--section needs a non-empty heading")
			}
			if window.pattern == "" && (cmd.Flags().Changed("regex") || cmd.Flags().Changed("context") || cmd.Flags().Changed("ignore-case")) {
				return cliutil.UserErrf(cmd.CommandPath(), "--regex needs a non-empty expression when using --regex, --context, or --ignore-case")
			}
			name := args[0]
			// Allow both "db" and "forge/db"
			name = strings.TrimPrefix(name, "forge/")

			// The SAME renderer harness consumers call (cli.RenderSkill),
			// so what this prints and what a harness injects cannot drift.
			root, _ := findProjectRoot()
			content, err := RenderSkillContentAt(root, name, SkillAudienceAll, Name())
			if err != nil {
				return cliutil.UserErr(fmt.Sprintf("forge skill load %s", name),
					fmt.Sprintf("skill %q not found", name),
					"",
					"run 'forge skill list' to see available skills, or 'forge skill search <keyword>' to find one")
			}

			content, err = window.selectFromSkill(content)
			if err != nil {
				return cliutil.UserErrf(cmd.CommandPath(), "%v", err)
			}
			_, err = cmd.OutOrStdout().Write(content)
			return err
		},
	}
	cmd.Flags().StringVar(&window.section, "section", "", "print one Markdown heading and its content (exact or unique partial heading)")
	cmd.Flags().StringVar(&window.pattern, "regex", "", "print only matching lines, numbered; may be combined with --section")
	cmd.Flags().BoolVarP(&window.ignoreCase, "ignore-case", "i", false, "match --regex case-insensitively")
	cmd.Flags().IntVar(&window.contextLines, "context", 0, "print this many lines of context on each side of a --regex match")
	return cmd
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
			return writeSkillSearchText(cmd.OutOrStdout(), query, results)
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
//
// Migration skills (relevance: migration) ARE searchable: a keyword query
// is an explicit, intent-carrying request — the opposite of the passive
// listing pollution the default-relevance filter exists to prevent.
func searchSkills(query string) ([]skillSearchResult, error) {
	skills, err := listSkills()
	if err != nil {
		return nil, err
	}
	return searchSkillsIn(skills, query), nil
}

// writeSkillSearchText is `skill search`'s human view of results.
func writeSkillSearchText(out io.Writer, query string, results []skillSearchResult) error {
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
}

// searchSkillsIn scores skills against query (searchSkills over a given set).
func searchSkillsIn(skills []skillMeta, query string) []skillSearchResult {
	queryLower := strings.ToLower(query)
	queryWords := strings.Fields(queryLower)
	if len(queryWords) == 0 {
		return nil
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
		if body, err := s.Body(); err == nil && len(body) > 0 {
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
	return results
}

// listSkills returns all available skills (forge-shipped + project + user),
// sorted alphabetically by path. The project root is discovered by walking
// upward from the cwd; see [listSkillsAt] for explicit-root callers.
func listSkills() ([]skillMeta, error) {
	root, _ := findProjectRoot()
	return listSkillsAt(root)
}

// findProjectRoot forwards to cmdutil.FindProjectRoot — the shared home so the
// dir-nested command groups (backlog, ...) reach it without importing
// internal/cli. The forwarder keeps the unexported name working for the flat
// command files in this package (api, friction, skill, upgrade_migrations)
// without rewriting their call sites.
func findProjectRoot() (string, error) { return cmdutil.FindProjectRoot() }

// jsonSkill is the JSON shape emitted by `skill list --json` and inside
// `skill search --json`. Keep field tags stable — sub-agents parse this.
type jsonSkill struct {
	Path        string         `json:"path"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Scope       SkillScope     `json:"scope"`
	Emit        SkillEmit      `json:"emit,omitempty"`
	Relevance   SkillRelevance `json:"relevance,omitempty"`
	Applies     SkillApplies   `json:"applies,omitempty"`
}

func toJSONSkill(s skillMeta) jsonSkill {
	return jsonSkill{Path: s.Path, Name: s.Name, Description: s.Description, Scope: s.Scope, Emit: s.Emit, Relevance: s.Relevance, Applies: s.Applies}
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

// writeSkillsWithBodiesJSON is writeSkillsJSON with each skill's body, as
// `skill load <path>` prints it: the same renderer, project root and command
// name. A host that mirrors a forge's whole catalog (reliant reading the
// project's pinned forge) reads it in one call rather than one per skill.
func writeSkillsWithBodiesJSON(w interface{ Write([]byte) (int, error) }, skills []skillMeta) error {
	type withBody struct {
		jsonSkill
		Body string `json:"body"`
	}
	root, _ := findProjectRoot()
	out := make([]withBody, 0, len(skills))
	for _, s := range skills {
		body, err := RenderSkillContentAt(root, s.Path, SkillAudienceAll, Name())
		if err != nil {
			return fmt.Errorf("render skill %q: %w", s.Path, err)
		}
		out = append(out, withBody{jsonSkill: toJSONSkill(s), Body: string(body)})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
