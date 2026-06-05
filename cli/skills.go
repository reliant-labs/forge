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
			Path:        m.Path,
			Name:        m.Name,
			Description: m.Description,
			Scope:       string(m.Scope),
			Emit:        string(m.Emit),
		})
	}
	return out, nil
}

// LoadSkill returns the raw SKILL.md body for skillPath under projectRoot,
// honoring the same user > project > forge precedence as ListSkills.
// Returns an error if the skill is not found.
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
