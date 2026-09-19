package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

// THE FEATURE GATE, at the generator rather than at the template.
//
// Rendering a template proves the YAML is right; only this proves the file is
// not written. Those are different failures, and the one that matters to a
// user who never asked for reconciliation is the second: an hourly scheduled
// job calling `forge reconcile` in a project whose reconcile loop is not wired
// fails every hour, forever, and a repository with a permanently red scheduled
// workflow is one where people stop reading workflow failures at all.

func ciGenerator(t *testing.T, features config.FeaturesConfig) (*ProjectGenerator, string) {
	t.Helper()
	dir := t.TempDir()
	return &ProjectGenerator{
		Name:       "myapp",
		Path:       dir,
		ModulePath: "github.com/example/myapp",
		Kind:       "service",
		Features:   features,
	}, dir
}

func TestCIFiles_ReconcileWorkflowIsOffByDefault(t *testing.T) {
	g, dir := ciGenerator(t, config.FeaturesConfig{})
	if err := g.generateCIFiles(); err != nil {
		t.Fatalf("generateCIFiles: %v", err)
	}

	reconcile := filepath.Join(dir, ".github", "workflows", "reconcile.yml")
	if _, err := os.Stat(reconcile); !os.IsNotExist(err) {
		t.Errorf("reconcile.yml was written for a project that never enabled the feature (err=%v); "+
			"everything in forge is opt-in and à la carte", err)
	}

	// The rest of CI is unaffected — the gate removes one workflow, it does
	// not suppress the scaffold.
	for _, want := range []string{"ci.yml", "build-images.yml", "deploy.yml"} {
		if _, err := os.Stat(filepath.Join(dir, ".github", "workflows", want)); err != nil {
			t.Errorf("%s is missing: %v", want, err)
		}
	}

	// And the build-images workflow carries no cut-release job, for the same
	// reason: it would call a control plane this project has not declared.
	images := readCIFile(t, filepath.Join(dir, ".github", "workflows", "build-images.yml"))
	if strings.Contains(images, "cut-release:") {
		t.Error("build-images.yml has a cut-release job with the feature off")
	}
}

func TestCIFiles_ReconcileWorkflowWhenEnabled(t *testing.T) {
	g, dir := ciGenerator(t, config.FeaturesConfig{
		Experimental: config.ExperimentalConfig{Reconcile: true},
	})
	if err := g.generateCIFiles(); err != nil {
		t.Fatalf("generateCIFiles: %v", err)
	}

	reconcile := readCIFile(t, filepath.Join(dir, ".github", "workflows", "reconcile.yml"))
	for _, want := range []string{"forge reconcile", "schedule:", "workflow_dispatch:"} {
		if !strings.Contains(reconcile, want) {
			t.Errorf("reconcile.yml missing %q", want)
		}
	}

	// The same gate turns on the CI-to-promote hop. Both talk to a control
	// plane, so one flag is the honest granularity — a project with the
	// reconcile loop wired is a project with somewhere to cut releases.
	images := readCIFile(t, filepath.Join(dir, ".github", "workflows", "build-images.yml"))
	for _, want := range []string{
		"cut-release:",
		"/controlplane.v1.DeployService/CutRelease",
		"/controlplane.v1.DeployService/Promote",
	} {
		if !strings.Contains(images, want) {
			t.Errorf("build-images.yml missing %q with the feature on", want)
		}
	}
}

// A CLI or library project gets neither, whatever the flag says: there is no
// Dockerfile to build an image from and nothing to deploy, so a reconcile
// workflow would observe an empty set.
func TestCIFiles_NonServiceKindsGetNoReconcileWorkflow(t *testing.T) {
	g, dir := ciGenerator(t, config.FeaturesConfig{
		Experimental: config.ExperimentalConfig{Reconcile: true},
	})
	g.Kind = "cli"
	if err := g.generateCIFiles(); err != nil {
		t.Fatalf("generateCIFiles: %v", err)
	}

	for _, absent := range []string{"reconcile.yml", "build-images.yml", "deploy.yml"} {
		if _, err := os.Stat(filepath.Join(dir, ".github", "workflows", absent)); !os.IsNotExist(err) {
			t.Errorf("%s was written for a CLI project (err=%v)", absent, err)
		}
	}
}

func readCIFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
