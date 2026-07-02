// Package cli — `forge features` cobra command.
//
// `forge features` prints the RESOLVED feature graph for the current
// project: every feature, whether it is on or off, WHY (derived from
// project shape vs explicitly set in forge.yaml), and its dependency
// edges (other features / shape preconditions it requires). It is the
// human/agent-facing window onto the dependency graph that
// internal/config/feature_graph.go validates at load time — when a
// config loads clean, this command shows the coherent set; the validator
// guarantees what it prints can never contradict itself.
//
// `forge features` covers the whole menu, stable + experimental, with
// the dependency column — including the four opt-in experimental flags.

package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/config"
)

// featureDisplayOrder is the stable print order for `forge features` —
// roughly the codegen → build → deploy → frontend → experimental flow a
// reader thinks in, rather than alphabetical. Any feature not listed
// here (defensive: a future Feature* constant the menu forgot) is
// appended alphabetically so it can never silently vanish from the
// output.
var featureDisplayOrder = []config.FeatureName{
	config.FeatureCodegen,
	config.FeatureORM,
	config.FeatureMigrations,
	config.FeatureContracts,
	config.FeatureDocs,
	config.FeatureFrontend,
	config.FeatureObservability,
	config.FeatureHotReload,
	config.FeatureCI,
	config.FeatureBuild,
	config.FeatureDeploy,
	config.FeaturePacks,
	// Experimental — printed in the same list with an (experimental) tag.
	config.FeatureIngress,
	config.FeatureExternalBuilds,
	config.FeatureOperators,
	config.FeatureStrictWiring,
}

func newFeaturesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "features",
		Short: "Print the resolved feature graph (on/off, why, dependencies)",
		Long: `Print every forge feature for this project: whether it is enabled,
WHY (derived from project shape vs explicitly set in forge.yaml), and
the features / shape preconditions it depends on.

The dependency graph is validated at config-load time — a feature
enabled with a dependency off is a load error. This command shows the
resolved, coherent set: codegen, orm, migrations, frontend, deploy,
ingress, and the rest, with their edges.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := loadProjectStore()
			if err != nil {
				return err
			}
			return printFeatureGraph(cmd, store.Config())
		},
	}
}

// printFeatureGraph renders the resolved feature table to the command's
// stdout. Layout is fixed-column so an agent can grep a feature line and
// read its state/origin/deps positionally.
func printFeatureGraph(cmd *cobra.Command, cfg *config.ProjectConfig) error {
	out := cmd.OutOrStdout()
	resolved := cfg.Features.EffectiveFeatures()

	// Build the print order: the curated order first, then any feature
	// present in `resolved` that the curated list missed, appended
	// alphabetically so the output is exhaustive by construction.
	seen := map[config.FeatureName]bool{}
	order := make([]config.FeatureName, 0, len(resolved))
	for _, n := range featureDisplayOrder {
		if _, ok := resolved[n]; ok {
			order = append(order, n)
			seen[n] = true
		}
	}
	var leftover []config.FeatureName
	for n := range resolved {
		if !seen[n] {
			leftover = append(leftover, n)
		}
	}
	sort.Strings(leftover)
	order = append(order, leftover...)

	_, _ = fmt.Fprintln(out, "Feature graph (resolved):")
	for _, name := range order {
		on := resolved[name]
		marker := "[ ]"
		if on {
			marker = "[x]"
		}
		origin := featureOrigin(cfg, name)
		line := fmt.Sprintf("  %s %-16s %s", marker, name, origin)
		if config.IsExperimentalFeature(name) {
			line += " (experimental)"
		}
		if deps := config.FeatureDependencies(name); len(deps) > 0 {
			line += fmt.Sprintf("  →  requires: %s", strings.Join(deps, ", "))
		}
		_, _ = fmt.Fprintln(out, line)
	}
	return nil
}

// featureOrigin reports whether the feature's resolved state came from an
// explicit forge.yaml value or from shape-derivation. Experimental flags
// are plain bools (no nil "absent" state) so they are always reported as
// the default-off opt-in unless turned on.
func featureOrigin(cfg *config.ProjectConfig, name config.FeatureName) string {
	if config.IsExperimentalFeature(name) {
		if cfg.Features.EffectiveFeatures()[name] {
			return "(explicit)"
		}
		return "(default off)"
	}
	if featureExplicitlySet(cfg.Features, name) {
		return "(explicit)"
	}
	return "(derived)"
}

// featureExplicitlySet reports whether a stable feature carries an
// explicit forge.yaml value (the *bool is non-nil) vs resolving from
// shape-derivation. Mirrors the FeaturesConfig field set 1:1.
func featureExplicitlySet(f config.FeaturesConfig, name config.FeatureName) bool {
	switch name {
	case config.FeatureORM:
		return f.ORM != nil
	case config.FeatureCodegen:
		return f.Codegen != nil
	case config.FeatureMigrations:
		return f.Migrations != nil
	case config.FeatureCI:
		return f.CI != nil
	case config.FeatureBuild:
		return f.Build != nil
	case config.FeatureContracts:
		return f.Contracts != nil
	case config.FeatureDocs:
		return f.Docs != nil
	case config.FeatureFrontend:
		return f.Frontend != nil
	case config.FeatureObservability:
		return f.Observability != nil
	case config.FeatureHotReload:
		return f.HotReload != nil
	case config.FeaturePacks:
		return f.Packs != nil
	case config.FeatureDeploy:
		return f.Deploy != nil
	default:
		return false
	}
}
