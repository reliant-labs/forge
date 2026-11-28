package cli

import "github.com/reliant-labs/forge/internal/skillcatalog"

// The skill catalog — listing, resolving and rendering skills — lives in
// internal/skillcatalog, so the public pkg/skills can serve it without
// linking this package (the whole CLI). These aliases and forwarders keep
// every name this package and forge/cli already use.

type (
	// SkillScope identifies where a skill was discovered from.
	SkillScope = skillcatalog.SkillScope
	// SkillEmit declares which audience(s) a skill is authored for.
	SkillEmit = skillcatalog.SkillEmit
	// SkillRelevance classifies WHEN a skill is worth surfacing.
	SkillRelevance = skillcatalog.SkillRelevance
	// SkillApplies says where a skill is useful.
	SkillApplies = skillcatalog.SkillApplies
	// SkillAudience names the consumer of a skill emission.
	SkillAudience = skillcatalog.SkillAudience
	// SkillMetaPublic is the cross-package view of a skill returned from
	// [ListSkillsAt].
	SkillMetaPublic = skillcatalog.SkillMetaPublic
	// SkillListOptions tunes [ListSkillsAtWithOptions].
	SkillListOptions = skillcatalog.SkillListOptions

	skillMeta = skillcatalog.SkillMeta
)

// The catalog's constants, under the names this package has always used.
const (
	SkillScopeForge   = skillcatalog.SkillScopeForge
	SkillScopeProject = skillcatalog.SkillScopeProject
	SkillScopeUser    = skillcatalog.SkillScopeUser

	SkillEmitForge   = skillcatalog.SkillEmitForge
	SkillEmitGeneral = skillcatalog.SkillEmitGeneral
	SkillEmitBoth    = skillcatalog.SkillEmitBoth

	SkillRelevanceMigration = skillcatalog.SkillRelevanceMigration
	SkillAppliesAny         = skillcatalog.SkillAppliesAny

	SkillAudienceAll     = skillcatalog.SkillAudienceAll
	SkillAudienceGeneral = skillcatalog.SkillAudienceGeneral
	SkillAudienceForge   = skillcatalog.SkillAudienceForge
)

// ListSkillsAt lists every skill for projectRoot (skillcatalog.ListSkillsAt).
func ListSkillsAt(projectRoot string) ([]SkillMetaPublic, error) {
	return skillcatalog.ListSkillsAt(projectRoot)
}

// ListSkillsAtWithOptions is [ListSkillsAt] with explicit listing options.
func ListSkillsAtWithOptions(projectRoot string, opts SkillListOptions) ([]SkillMetaPublic, error) {
	return skillcatalog.ListSkillsAtWithOptions(projectRoot, opts)
}

// ResolveSkillContentAt returns a skill's body with the version-skew advisory
// (skillcatalog.ResolveSkillContentAt).
func ResolveSkillContentAt(projectRoot, skillPath string) ([]byte, SkillScope, error) {
	return skillcatalog.ResolveSkillContentAt(projectRoot, skillPath)
}

// RenderSkillContentAt returns a skill exactly as `forge skill load` prints it
// (skillcatalog.RenderSkillContentAt).
func RenderSkillContentAt(projectRoot, skillPath string, audience SkillAudience, cliName string) ([]byte, error) {
	return skillcatalog.RenderSkillContentAt(projectRoot, skillPath, audience, cliName)
}

// RenderSkillForAudience filters a skill body for audience
// (skillcatalog.RenderSkillForAudience).
func RenderSkillForAudience(body []byte, audience SkillAudience) []byte {
	return skillcatalog.RenderSkillForAudience(body, audience)
}

// forgeModulePath is forge's Go module path (skillcatalog.ForgeModulePath).
const forgeModulePath = skillcatalog.ForgeModulePath

func listSkillsAt(projectRoot string) ([]skillMeta, error) { return skillcatalog.List(projectRoot) }
func listForgeShippedSkills() ([]skillMeta, error)         { return skillcatalog.ListShipped() }
func loadForgeShippedSkill(skillPath string) ([]byte, error) {
	return skillcatalog.LoadShipped(skillPath)
}
func parseFrontmatter(content []byte) skillMeta { return skillcatalog.ParseFrontmatter(content) }
func filterDefaultRelevance(skills []skillMeta) []skillMeta {
	return skillcatalog.FilterDefaultRelevance(skills)
}
func rewriteForgeCommands(content []byte, cliName string) []byte {
	return skillcatalog.RewriteForgeCommands(content, cliName)
}
func expandBenefitDirectives(body []byte) ([]byte, error) {
	return skillcatalog.ExpandBenefitDirectives(body)
}
func runningForgeVersion() string { return skillcatalog.RunningForgeVersion() }
func isForgeVersionSkew(binaryVersion, projectVersion string) bool {
	return skillcatalog.IsForgeVersionSkew(binaryVersion, projectVersion)
}
func insertAfterFrontmatter(content, chunk []byte) []byte {
	return skillcatalog.InsertAfterFrontmatter(content, chunk)
}
