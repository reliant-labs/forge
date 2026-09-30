// ingress and operators GRADUATED out of experimental. This pins the
// user-visible consequence: a project that uses them gets no warning.
//
// The line this removes, printed by control-plane on every single forge
// invocation:
//
//	warning: experimental: ingress, external_builds, operators (--silence-experimental to hide)
//
// It was a warning about the project's OWN production configuration —
// ingress is how a deployed service is reachable at all — offering a flag
// to hide it as the remedy. A warning whose only available response is to
// silence it is not protecting anyone; it is training the reader to pass
// --silence-experimental, which then hides the warnings that do matter.
package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

// TestGraduatedFeaturesEmitNoExperimentalWarning is the headline: the
// exact control-plane feature set produces no warning at all.
func TestGraduatedFeaturesEmitNoExperimentalWarning(t *testing.T) {
	yes := true
	f := config.FeaturesConfig{Ingress: &yes, Operators: &yes}

	if names := f.EnabledExperimentalFeatures(); len(names) != 0 {
		t.Fatalf("ingress/operators still count as experimental: %v — graduating them means "+
			"a project using them is not warned about its own production config", names)
	}

	var buf bytes.Buffer
	experimentalWarningEmitted.Store(false)
	t.Cleanup(func() { experimentalWarningEmitted.Store(false) })
	emitExperimentalWarning(&buf, f.EnabledExperimentalFeatures())
	if got := buf.String(); got != "" {
		t.Fatalf("a project with ingress + operators on still warns: %q", got)
	}
}

// TestStillExperimentalFeaturesStillWarn is the boundary. This change
// retires three features from the experimental tier, not the tier itself:
// strict_wiring and reconcile are genuinely unsettled and must keep their
// warning, or graduating would have quietly disabled the mechanism.
func TestStillExperimentalFeaturesStillWarn(t *testing.T) {
	f := config.FeaturesConfig{
		Experimental: config.ExperimentalConfig{Reconcile: true},
	}
	names := f.EnabledExperimentalFeatures()
	if len(names) != 1 || names[0] != config.FeatureReconcile {
		t.Fatalf("reconcile must still be experimental; got %v", names)
	}

	var buf bytes.Buffer
	experimentalWarningEmitted.Store(false)
	t.Cleanup(func() { experimentalWarningEmitted.Store(false) })
	emitExperimentalWarning(&buf, names)
	if !strings.Contains(buf.String(), "reconcile") {
		t.Fatalf("a genuinely experimental feature stopped warning: %q", buf.String())
	}
}

// TestExternalBuildsIsGoneEntirely pins the delete. It is not in the
// experimental list, and it is not a stable feature either: the flag had
// already been reduced to an inert key no code path consulted, so there was
// nothing to graduate.
func TestExternalBuildsIsGoneEntirely(t *testing.T) {
	var f config.FeaturesConfig
	for name := range f.EffectiveFeatures() {
		if name == "external_builds" {
			t.Fatal("external_builds is still a feature; it gated nothing and was deleted, not graduated")
		}
	}
	for _, name := range config.ExperimentalFeatureNames {
		if name == "external_builds" {
			t.Fatal("external_builds is still listed as experimental")
		}
	}
}

// TestGraduatedFeaturesAreStillOptIn guards the thing graduation must NOT
// change. Both features derive to false: neither follows from a project's
// shape, so the stable-flag default of "absent means enabled" would turn on
// Gateway API codegen and CRD generation for every project that never asked.
// Graduating changes the spelling and the warning, not the behaviour.
func TestGraduatedFeaturesAreStillOptIn(t *testing.T) {
	var f config.FeaturesConfig
	if f.IngressEnabled() {
		t.Error("ingress defaults ON — graduating must not enable it for projects that never opted in")
	}
	if f.OperatorsEnabled() {
		t.Error("operators defaults ON — graduating must not enable it for projects that never opted in")
	}
}
