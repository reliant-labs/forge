package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

// TestIsFeatureEnabled_NilConfig locks in the permissive default
// when no forge.yaml is loaded — required so commands run outside a
// project don't error on a missing config.
func TestIsFeatureEnabled_NilConfig(t *testing.T) {
	if !isFeatureEnabled(nil, config.FeatureDeploy) {
		t.Error("isFeatureEnabled(nil, deploy) = false, want true")
	}
}

// TestIsFeatureEnabled_DefaultsTrue covers the absent-features-block
// case for a real config — every feature accessor must report enabled.
func TestIsFeatureEnabled_DefaultsTrue(t *testing.T) {
	cfg := &config.ProjectConfig{Name: "t", ModulePath: "x/t"}
	for _, name := range []string{
		config.FeatureDeploy, config.FeatureBuild, config.FeatureFrontend,
		config.FeaturePacks, config.FeatureStarters, config.FeatureCI,
		config.FeatureDocs, config.FeatureObservability,
	} {
		if !isFeatureEnabled(cfg, name) {
			t.Errorf("isFeatureEnabled(<no-features>, %q) = false, want true", name)
		}
	}
}

// TestIsFeatureEnabled_ExplicitFalse covers the opt-out path for each
// new feature added by the features-block work.
func TestIsFeatureEnabled_ExplicitFalse(t *testing.T) {
	off := false
	cfg := &config.ProjectConfig{
		Features: config.FeaturesConfig{
			Deploy:   &off,
			Build:    &off,
			Packs:    &off,
			Starters: &off,
		},
	}
	for _, name := range []string{
		config.FeatureDeploy, config.FeatureBuild,
		config.FeaturePacks, config.FeatureStarters,
	} {
		if isFeatureEnabled(cfg, name) {
			t.Errorf("isFeatureEnabled(<%s=false>, %q) = true, want false", name, name)
		}
	}
	// Features the test didn't touch must remain enabled (nil-default).
	if !isFeatureEnabled(cfg, config.FeatureCI) {
		t.Error("isFeatureEnabled(<deploy=false>, ci) flipped — only Deploy was set")
	}
}

// TestIsFeatureEnabled_UnknownNamePermissive asserts the
// "additive-extension" rule: an unknown feature name returns true
// (enabled) rather than erroring, so a new gate site added in a
// downstream forge doesn't crash older configs that don't yet know
// the constant.
func TestIsFeatureEnabled_UnknownNamePermissive(t *testing.T) {
	cfg := &config.ProjectConfig{}
	if !isFeatureEnabled(cfg, "made-up-feature") {
		t.Error("isFeatureEnabled(cfg, unknown) = false, want true (additive-extension)")
	}
}

// TestDisabledFeatureError_Wording locks in the user-visible string
// the CLI emits when a direct cobra command is invoked against a
// project that disabled the feature. The exact spelling is the
// public contract — both for humans grepping logs and for
// sub-agents matching against the canonical "feature 'X' is
// disabled in forge.yaml" idiom.
func TestDisabledFeatureError_Wording(t *testing.T) {
	for _, name := range []string{
		config.FeatureDeploy, config.FeatureBuild,
		config.FeaturePacks, config.FeatureStarters,
		config.FeatureFrontend, config.FeatureCI,
		config.FeatureDocs, config.FeatureObservability,
	} {
		err := config.DisabledFeatureError(name)
		if err == nil {
			t.Errorf("DisabledFeatureError(%q) = nil", name)
			continue
		}
		got := err.Error()
		if !strings.Contains(got, "feature '"+name+"' is disabled in forge.yaml") {
			t.Errorf("DisabledFeatureError(%q) = %q, missing canonical prefix", name, got)
		}
		if !strings.Contains(got, "features."+name+": true") {
			t.Errorf("DisabledFeatureError(%q) = %q, missing fix-up hint", name, got)
		}
	}
}

// TestRequireFeature_NoProject covers the no-forge.yaml path: the
// helper must surface ErrProjectConfigNotFound so the cobra command
// shows the existing "not in a forge project" message rather than
// a confusing "feature disabled" string.
func TestRequireFeature_NoProject(t *testing.T) {
	// chdir to a temp dir without a forge.yaml so loadProjectConfig
	// walks up and never finds one.
	t.Chdir(t.TempDir())
	_, err := requireFeature(config.FeatureDeploy)
	if !errors.Is(err, ErrProjectConfigNotFound) {
		t.Errorf("requireFeature outside project: got %v, want ErrProjectConfigNotFound", err)
	}
}
