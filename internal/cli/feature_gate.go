// Package cli — feature-gating helpers shared across cobra commands.
//
// `forge.yaml` exposes a `features:` block (see config.FeaturesConfig)
// that gates major subsystems — deploy, build, frontend, packs,
// starters, ci, docs, observability, ... — so projects can opt out of
// surface they don't use. Two modes are supported:
//
//   - requireFeature is the strict gate: a direct cobra subcommand
//     (e.g. `forge deploy`, `forge build`) returns
//     config.DisabledFeatureError when the relevant feature is off.
//     The error format is centralised so sub-agents and humans
//     grepping for the "feature 'X' is disabled" string find one
//     authoritative spelling.
//
//   - skipFeature is the orchestrator gate: when `forge up` is driving
//     several phases, a disabled phase logs a one-line skip and the
//     orchestrator continues with whatever remaining phases are
//     enabled. Returns false when the feature is off so the caller can
//     branch around the phase without surfacing an error to the user.
//
// Both helpers tolerate `cfg == nil` (project missing or unreadable)
// by treating it as "feature enabled" — the canonical "no forge.yaml,
// no opinion" behaviour every existing direct-invoke gate already
// uses. The `loadAndCheckFeature` helper is the one-liner most call
// sites need: load the project config, return the canonical disabled
// error if the feature is off, otherwise return the loaded config.

package cli

import (
	"fmt"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/projectstore"
)

// featureCheck is the per-feature predicate signature. Each Feature*
// constant in package config has a paired FeaturesConfig.<Name>Enabled
// method; this type lets callers pass the method by reference without
// importing the FeaturesConfig type into the call site.
type featureCheck func(config.FeaturesConfig) bool

// featureChecks maps every config.Feature* constant to its
// FeaturesConfig accessor. Used by requireFeature so call sites pass
// just the feature name and the helper knows which accessor to invoke
// — keeps the name-to-accessor mapping in one place (mismatch is a
// compile-time error rather than a runtime mis-spelling). Experimental
// features share this map; the default-OFF semantics come from the
// underlying accessor (e.g. IngressEnabled() reads Experimental.Ingress).
var featureChecks = map[string]featureCheck{
	config.FeatureORM:            func(f config.FeaturesConfig) bool { return f.ORMEnabled() },
	config.FeatureCodegen:        func(f config.FeaturesConfig) bool { return f.CodegenEnabled() },
	config.FeatureMigrations:     func(f config.FeaturesConfig) bool { return f.MigrationsEnabled() },
	config.FeatureCI:             func(f config.FeaturesConfig) bool { return f.CIEnabled() },
	config.FeatureBuild:          func(f config.FeaturesConfig) bool { return f.BuildEnabled() },
	config.FeatureContracts:      func(f config.FeaturesConfig) bool { return f.ContractsEnabled() },
	config.FeatureDocs:           func(f config.FeaturesConfig) bool { return f.DocsEnabled() },
	config.FeatureFrontend:       func(f config.FeaturesConfig) bool { return f.FrontendEnabled() },
	config.FeatureObservability:  func(f config.FeaturesConfig) bool { return f.ObservabilityEnabled() },
	config.FeatureHotReload:      func(f config.FeaturesConfig) bool { return f.HotReloadEnabled() },
	config.FeaturePacks:          func(f config.FeaturesConfig) bool { return f.PacksEnabled() },
	config.FeatureStarters:       func(f config.FeaturesConfig) bool { return f.StartersEnabled() },
	config.FeatureDeploy:         func(f config.FeaturesConfig) bool { return f.DeployEnabled() },
	config.FeatureIngress:        func(f config.FeaturesConfig) bool { return f.IngressEnabled() },
	config.FeatureExternalBuilds: func(f config.FeaturesConfig) bool { return f.ExternalBuildsEnabled() },
	config.FeatureOperators:      func(f config.FeaturesConfig) bool { return f.OperatorsEnabled() },
	config.FeatureStrictWiring:   func(f config.FeaturesConfig) bool { return f.StrictWiringEnabled() },
}

// isFeatureEnabled reports whether a named feature is enabled in cfg.
// A nil cfg (project missing) is treated as "enabled" so callers that
// don't bother loading config get the historical permissive default.
// An unknown feature name returns true with no error — keeps adding a
// new gate site backwards-compatible across forge versions that
// haven't yet registered the constant in featureChecks.
func isFeatureEnabled(store projectstore.ProjectStore, name string) bool {
	if store == nil {
		return true
	}
	check, ok := featureChecks[name]
	if !ok {
		return true
	}
	return check(store.Features())
}

// requireFeature is the strict gate for direct cobra subcommands. It
// loads the project config and returns config.DisabledFeatureError
// when the named feature is off. Returns the loaded config on the
// happy path so the caller can hold on to it without a second read.
//
// Use from the top of a cobra RunE when the subcommand has no useful
// fallback (e.g. `forge deploy` against a project with
// features.deploy: false). Don't use from orchestrators — see
// skipFeature for the orchestrator shape.
func requireFeature(name string) (projectstore.ProjectStore, error) {
	store, err := loadProjectStore()
	if err != nil {
		return nil, err
	}
	if !isFeatureEnabled(store, name) {
		return nil, config.DisabledFeatureError(name)
	}
	return store, nil
}

// skipFeature is the orchestrator gate. Returns true when the
// orchestrator SHOULD skip the phase, false when the phase should
// run. When skipping, emits a one-line log so the user can see WHY
// the phase was elided.
//
// Used by `forge up` to elide build/deploy/frontend phases against
// projects that have those features turned off. Unlike requireFeature
// this never errors — the orchestrator wants to finish whatever
// remaining phases are enabled.
//
// Experimental features get a distinct skip message — the historical
// "disabled in forge.yaml" wording implies the user opted out, which
// is misleading for default-off opt-in features the user never
// touched.
func skipFeature(store projectstore.ProjectStore, name, phase string) bool {
	if isFeatureEnabled(store, name) {
		return false
	}
	if config.IsExperimentalFeature(name) {
		fmt.Printf("[%s] feature '%s' is experimental and not opted in (set features.experimental.%s: true) — skipping\n",
			phase, name, name)
		return true
	}
	fmt.Printf("[%s] feature '%s' is disabled in forge.yaml — skipping\n", phase, name)
	return true
}
