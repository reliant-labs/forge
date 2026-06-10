// Copyright (c) 2025 Reliant Labs
package cli

import (
	internalcli "github.com/reliant-labs/forge/internal/cli"
)

// Skill is the public metadata view of a forge skill, suitable for
// out-of-process consumers (e.g. reliant surfacing forge skills natively).
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
	// (methodology that applies to any project), or "both" (mixed
	// content, with framework-specific portions inside @forge-only
	// blocks the renderer strips for general audiences). Consumers use
	// this to decide whether to surface a skill outside a forge project.
	Emit string

	// SkillForgeVersion is the forge version whose embedded templates the
	// skill content comes from — i.e. the forge module linked into THIS
	// process, not the project's pin. Empty for project/user-scope skills.
	SkillForgeVersion string
	// ProjectForgeVersion is the forge_version pinned in the project's
	// forge.yaml ("" when projectRoot was empty, no pin is declared, or the
	// file could not be read).
	ProjectForgeVersion string
	// VersionSkew is true when the serving forge version and the project's
	// pin are both real release versions and differ. Harness consumers
	// should surface skewed skills with caution: the guidance may describe
	// a different forge version than the one that generated the project.
	VersionSkew bool
}

// ListSkills returns every available forge skill for projectRoot — merging
// forge-shipped, project (.forge/skills under projectRoot), and user-global
// (~/.forge/skills) sources. On path collision, precedence is user > project
// > forge.
//
// An empty projectRoot skips the project source. The result is sorted by
// Path. Returns an error only if the embedded forge-shipped skills cannot be
// enumerated; missing disk sources are silently skipped.
func ListSkills(projectRoot string) ([]Skill, error) {
	metas, err := internalcli.ListSkillsAt(projectRoot)
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
			SkillForgeVersion:   m.SkillForgeVersion,
			ProjectForgeVersion: m.ProjectForgeVersion,
			VersionSkew:         m.VersionSkew,
		})
	}
	return out, nil
}

// LoadSkill returns the SKILL.md body for skillPath under projectRoot,
// honoring the same user > project > forge precedence as ListSkills.
// Returns an error if the skill is not found.
//
// Version-skew advisory: when the skill is forge-shipped and the forge
// version serving it differs from the project's pinned forge_version
// (see Skill.VersionSkew), a one-line "Note: this guidance is from
// forge <X>; this project pins forge <Y>..." advisory is inserted after
// the YAML frontmatter so downstream readers see the skew inline.
func LoadSkill(projectRoot, skillPath string) ([]byte, error) {
	body, _, err := internalcli.ResolveSkillContentAt(projectRoot, skillPath)
	return body, err
}

// LoadSkillForAudience is like [LoadSkill] but filters the body for the
// given audience. Pass "general" to strip `<!-- @forge-only:start/end -->`
// blocks (e.g. when surfacing a skill outside a forge project). Pass
// "forge" or "" to keep the full body. Reliant and other harness shims
// should use this when a project lacks forge.yaml so emit:both skills
// surface without forge-specific tooling instructions.
func LoadSkillForAudience(projectRoot, skillPath, audience string) ([]byte, error) {
	body, err := LoadSkill(projectRoot, skillPath)
	if err != nil {
		return nil, err
	}
	return internalcli.RenderSkillForAudience(body, internalcli.SkillAudience(audience)), nil
}
