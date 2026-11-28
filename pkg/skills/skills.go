// Package skills lists and renders forge's skills — the conventions and
// playbooks `forge skill list` and `forge skill load` print — for a host that
// surfaces them itself, such as an agent harness.
//
// It is the light entry point. forge/cli offers the same calls, but importing
// it links the whole forge CLI (KCL, docker, the cloud SDKs, k8s: about 2,150
// packages) into every binary and test binary above the import. This package
// links the embedded skill files and the code that renders them, and nothing
// heavier; TestImportsStayLight holds that line.
package skills

import (
	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/skillcatalog"
)

// Skill is the public metadata view of a forge skill.
type Skill struct {
	// Path is the skill identifier (e.g. "db", "frontend/state").
	Path string
	// Name is the human-readable name from the SKILL.md frontmatter.
	Name string
	// Description is the one-line summary from the SKILL.md frontmatter.
	Description string
	// Scope is where the skill was discovered: "forge", "project", or "user".
	Scope string
	// Emit declares which audience the skill targets — "forge" (framework
	// skills, default when frontmatter omits the field), "general"
	// (methodology that applies to any project), or "both" (mixed content,
	// with framework-specific portions inside @forge-only blocks the renderer
	// strips for general audiences). Consumers use this to decide whether to
	// surface a skill outside a forge project.
	Emit string
	// Relevance classifies when the skill is worth surfacing: "" (always —
	// the default) or "migration" (a one-time upgrade-transition playbook).
	// ListSkills excludes relevance=migration skills; ListSkillsWithOptions
	// can opt them back in.
	Relevance string
	// Version is the release that introduced the breaking change this
	// migration skill covers, passed through verbatim from the `version:`
	// frontmatter. A project needs the migration when its pin is BELOW this
	// version and it still exhibits the old shape; the authoritative
	// project-aware gate is `forge project upgrade list`. Empty when
	// undeclared and for non-migration skills.
	Version string

	// SkillForgeVersion is the forge version whose embedded templates the
	// skill content comes from — the forge module linked into THIS process,
	// not the project's pin. Empty for project/user-scope skills.
	SkillForgeVersion string
	// ProjectForgeVersion is the forge_version pinned in the project's
	// forge.yaml ("" when projectRoot was empty, no pin is declared, or the
	// file could not be read).
	ProjectForgeVersion string
	// VersionSkew is true when the serving forge version and the project's
	// pin are both real release versions and differ: the guidance may
	// describe a different forge than the one that generated the project.
	VersionSkew bool
}

// ListSkills returns every available forge skill for projectRoot, merging
// forge-shipped, project (.forge/skills under projectRoot) and user-global
// (~/.forge/skills) sources. On a path collision, user > project > forge.
//
// An empty projectRoot skips the project source. The result is sorted by
// Path. It returns an error only if the embedded skills cannot be
// enumerated; missing disk sources are skipped.
//
// One-time migration skills (Relevance == "migration") are excluded; use
// [ListSkillsWithOptions] to opt them in. They stay loadable by exact path.
func ListSkills(projectRoot string) ([]Skill, error) {
	return ListSkillsWithOptions(projectRoot, ListSkillsOptions{})
}

// ListSkillsOptions tunes [ListSkillsWithOptions]. The zero value matches
// [ListSkills].
type ListSkillsOptions struct {
	// IncludeMigrationSkills opts relevance=migration skills back into the
	// listing (e.g. for an upgrade-assistant surface).
	IncludeMigrationSkills bool
}

// ListSkillsWithOptions is [ListSkills] with explicit listing options.
func ListSkillsWithOptions(projectRoot string, opts ListSkillsOptions) ([]Skill, error) {
	metas, err := skillcatalog.ListSkillsAtWithOptions(projectRoot, skillcatalog.SkillListOptions{
		IncludeMigrations: opts.IncludeMigrationSkills,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Skill, 0, len(metas))
	for _, m := range metas {
		out = append(out, Skill{
			Path:                m.Path,
			Name:                m.Name,
			Description:         m.Description,
			Scope:               string(m.Scope),
			Emit:                string(m.Emit),
			Relevance:           string(m.Relevance),
			Version:             m.Version,
			SkillForgeVersion:   m.SkillForgeVersion,
			ProjectForgeVersion: m.ProjectForgeVersion,
			VersionSkew:         m.VersionSkew,
		})
	}
	return out, nil
}

// LoadSkill returns the SKILL.md body for skillPath under projectRoot, with
// the same user > project > forge precedence as [ListSkills], and an error
// if the skill is not found. When a forge-shipped skill is served by a forge
// version other than the project's pin, a one-line advisory follows the
// frontmatter.
//
// A harness that injects a skill into an agent's context wants [RenderSkill],
// which also rewrites command names to the mount that serves them.
func LoadSkill(projectRoot, skillPath string) ([]byte, error) {
	body, _, err := skillcatalog.ResolveSkillContentAt(projectRoot, skillPath)
	return body, err
}

// LoadSkillForAudience is [LoadSkill] with the body filtered for audience:
// "general" strips `<!-- @forge-only:start/end -->` blocks (for a project
// with no forge.yaml); "forge" or "" keeps the full body.
func LoadSkillForAudience(projectRoot, skillPath, audience string) ([]byte, error) {
	body, err := LoadSkill(projectRoot, skillPath)
	if err != nil {
		return nil, err
	}
	return skillcatalog.RenderSkillForAudience(body, skillcatalog.SkillAudience(audience)), nil
}

// RenderSkillOptions tunes [RenderSkill]. The zero value renders exactly what
// `forge skill load` prints from THIS process.
type RenderSkillOptions struct {
	// Audience filters the body the same way as [LoadSkillForAudience].
	Audience string
	// CLIName is the command a reader types to invoke forge — "forge" for the
	// standalone binary, "reliant forge" when forge is mounted inside
	// reliant. Every bare `forge ` command reference in the body is rewritten
	// to it. "" means the name this process reports for itself.
	CLIName string
}

// RenderSkill returns skillPath's content exactly as `<cli> skill load
// <skillPath>` prints it: the resolution and version-skew advisory of
// [LoadSkill], plus the command-name rewrite the CLI applies, so every
// command in the skill is copy-pasteable under the mount that serves it.
// `skill load` calls the same renderer, so the two cannot drift.
//
// A "forge/" prefix on skillPath is accepted, matching `skill load`.
func RenderSkill(projectRoot, skillPath string, opts RenderSkillOptions) ([]byte, error) {
	cliName := opts.CLIName
	if cliName == "" {
		cliName = cmdutil.Name()
	}
	return skillcatalog.RenderSkillContentAt(projectRoot, skillPath, skillcatalog.SkillAudience(opts.Audience), cliName)
}
