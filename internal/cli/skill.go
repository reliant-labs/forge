package cli

import (
	"fmt"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/templates"
)

// forgeCmdRE matches "forge" used as a CLI command — i.e. followed by a space.
// Duplicated from generator/project.go to avoid a cross-package dependency.
var forgeCmdRE = regexp.MustCompile(`\bforge( )`)

// skillMeta holds parsed YAML frontmatter from a SKILL.md file.
type skillMeta struct {
	Path        string // e.g. "db", "frontend/state", "debug/investigate"
	Name        string
	Description string
}

func newSkillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Manage Forge skills — conventions and playbooks for LLM agents",
	}
	cmd.AddCommand(newSkillListCmd())
	cmd.AddCommand(newSkillLoadCmd())
	return cmd
}

func newSkillListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List available skills",
		RunE: func(cmd *cobra.Command, args []string) error {
			skills, err := listSkills()
			if err != nil {
				return err
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "PATH\tNAME\tDESCRIPTION")
			for _, s := range skills {
				fmt.Fprintf(w, "%s\t%s\t%s\n", s.Path, s.Name, s.Description)
			}
			return w.Flush()
		},
	}
}

func newSkillLoadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "load <name>",
		Short: "Print a skill's content to stdout",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			// Allow both "db" and "forge/db"
			name = strings.TrimPrefix(name, "forge/")

			// "forge" itself is the root skill at skills/forge/SKILL.md
			var templatePath string
			if name == "forge" {
				templatePath = path.Join("skills", "forge", "SKILL.md")
			} else {
				templatePath = path.Join("skills", "forge", name, "SKILL.md")
			}
			content, err := templates.ProjectTemplates.Get(templatePath)
			if err != nil {
				return fmt.Errorf("skill %q not found", name)
			}

			// Rewrite CLI command references if running under a different binary name.
			cliName := CLIName()
			if cliName != "forge" {
				content = forgeCmdRE.ReplaceAll(content, []byte(cliName+"$1"))
			}

			_, err = os.Stdout.Write(content)
			return err
		},
	}
}

// listSkills returns all available skills, sorted alphabetically by path.
func listSkills() ([]skillMeta, error) {
	files, err := templates.ProjectTemplates.List("skills")
	if err != nil {
		return nil, fmt.Errorf("list skill templates: %w", err)
	}

	var skills []skillMeta
	for _, rel := range files {
		// Skip non-SKILL.md files (e.g. README.md)
		if !strings.HasSuffix(rel, "/SKILL.md") && rel != "SKILL.md" {
			continue
		}

		// rel is like "forge/SKILL.md", "forge/db/SKILL.md", or
		// "forge/frontend/state/SKILL.md". Strip to get the skill path.
		skillPath := strings.TrimPrefix(rel, "forge/")
		skillPath = strings.TrimSuffix(skillPath, "/SKILL.md")
		skillPath = strings.TrimSuffix(skillPath, "SKILL.md")
		skillPath = strings.TrimSuffix(skillPath, "/")
		if skillPath == "" {
			skillPath = "forge"
		}

		// Read and parse frontmatter
		content, err := templates.ProjectTemplates.Get(path.Join("skills", rel))
		if err != nil {
			continue
		}

		meta := parseFrontmatter(content)
		meta.Path = skillPath

		skills = append(skills, meta)
	}

	sort.Slice(skills, func(i, j int) bool {
		return skills[i].Path < skills[j].Path
	})

	return skills, nil
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