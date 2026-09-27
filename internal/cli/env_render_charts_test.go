package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/kclplugin"
)

// fakeHelmOnPath installs a `helm` that renders a fixed chart: a Deployment
// (and a post-install hook Job, which deploy drops) for `template
// --skip-crds`, and one chart-owned CRD for `template --include-crds`. It
// records its argv so a test can assert the declared values reached it.
// Returns the argv log path.
func fakeHelmOnPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "helm.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> ` + logPath + `
case " $* " in
*' --include-crds '*)
cat <<'EOF'
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: gitrepositories.source.toolkit.fluxcd.io
spec:
  group: source.toolkit.fluxcd.io
EOF
;;
*)
cat <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: source-controller
  namespace: flux-system
---
apiVersion: batch/v1
kind: Job
metadata:
  name: post-install-check
  namespace: flux-system
  annotations:
    helm.sh/hook: post-install
EOF
;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "helm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// writeChartRenderProject writes a prod env with one app workload and one
// declared forge.HelmChart (flux), the shape of control-plane's prod.
func writeChartRenderProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("forge.yaml", "name: charttest\nmodule_path: github.com/example/charttest\nversion: \"0.1.0\"\n")
	write("deploy/kcl/kcl.mod", "[package]\nname = \"charttest-deploy\"\nedition = \"v0.11.0\"\nversion = \"0.0.1\"\n\n[dependencies]\nforge = { path = \""+forgeModuleRoot(t)+"\" }\n")
	write("deploy/kcl/prod/main.k", `import forge

_bundle = forge.Bundle {
    project = "charttest"
    cluster_target = forge.ClusterTarget {
        cluster = "gke_example_us-central1_prod"
        namespace = "charttest-prod"
        registry = "reg.example.com"
    }
    services = [forge.RenderedWorkload {
        name = "api"
        image = "charttest"
        deploy = forge.K8sCluster {cluster = "gke_example_us-central1_prod", namespace = "charttest-prod", registry = "reg.example.com"}
    }]
    helm_charts = [forge.HelmChart {
        name = "flux"
        oci = "oci://ghcr.io/fluxcd-community/charts/flux2"
        version = "2.14.1"
        namespace = "flux-system"
        values = {sourceController.create = True}
    }]
}

output = forge.render(_bundle)
manifests = forge.render_manifests(_bundle, option("image_tag") or "latest", forge.image_digests(), False)
`)
	return dir
}

// TestEnvRender_IncludesHelmChartObjects is the F5 regression: `forge env
// render` printed 0 objects for every declared HelmChart while `forge env
// deploy` applied them, so render was not a preview of deploy.
//
// It drives the real command against a project declaring a chart, with a fake
// helm, and asserts the chart's objects are in the stream — labelled as chart
// output, attributed to the env's cluster, after the SAME post-hook drop and
// CRD merge a deploy performs (proof that it went through the deploy's render
// function rather than a second model of it).
//
// Mutation that fails it: drop the renderEnvCharts call in renderEnvTo.
func TestEnvRender_IncludesHelmChartObjects(t *testing.T) {
	kclplugin.Register()
	helmLog := fakeHelmOnPath(t)
	dir := writeChartRenderProject(t)

	stdout, stderr, err := runRenderCapturingProcessStdout(t, dir, "prod")
	if err != nil {
		t.Fatalf("forge env render prod: %v\nstderr:\n%s", err, stderr)
	}
	docs := assertYAMLManifestStream(t, stdout)

	have := map[string]bool{}
	for _, d := range docs {
		have[d.Kind+"/"+d.Metadata.Name] = true
	}
	for _, want := range []string{
		"Deployment/api",               // the app, from KCL
		"Deployment/source-controller", // the chart's templated output
		"Namespace/flux-system",        // synthesized, as deploy does
		"CustomResourceDefinition/gitrepositories.source.toolkit.fluxcd.io", // chart-owned CRD
	} {
		if !have[want] {
			t.Errorf("render stream is missing %s — a deploy applies it. kinds/names: %v", want, have)
		}
	}
	// Deploy drops post-install hooks; a render that kept them would not be
	// the deploy's stream.
	if have["Job/post-install-check"] {
		t.Errorf("render kept a post-install hook Job that deploy drops — it is not rendering through the deploy's chart path")
	}

	// Labelled and attributed.
	if !strings.Contains(stdout, "# source: helm chart flux") {
		t.Errorf("chart documents must be labelled `# source: helm chart flux`:\n%s", stdout)
	}
	idx := strings.Index(stdout, "name: source-controller")
	head := stdout[:idx]
	if last := strings.LastIndex(head, "# cluster: "); last < 0 || !strings.HasPrefix(head[last:], "# cluster: gke_example_us-central1_prod") {
		t.Errorf("the chart Deployment must be attributed to the env's cluster:\n%s", stdout)
	}
	if !strings.Contains(stderr, "chart:     flux") {
		t.Errorf("summary should report the rendered chart, got stderr:\n%s", stderr)
	}

	// The declared values reached helm.
	argv, rerr := os.ReadFile(helmLog)
	if rerr != nil {
		t.Fatalf("fake helm never ran: %v", rerr)
	}
	if !strings.Contains(string(argv), "--values") || !strings.Contains(string(argv), "--version 2.14.1") {
		t.Errorf("helm was not invoked with the declared version and values:\n%s", argv)
	}
}

// TestEnvRender_NoChartsSaysWhatItLeftOut: --no-charts is the offline escape
// hatch, and a render that silently dropped the charts would recreate the
// defect. The summary must name what was left out, and helm must not run.
func TestEnvRender_NoChartsSaysWhatItLeftOut(t *testing.T) {
	kclplugin.Register()
	helmLog := fakeHelmOnPath(t)
	dir := writeChartRenderProject(t)

	stdout, stderr, err := runRenderCapturingProcessStdout(t, dir, "prod", "--no-charts")
	if err != nil {
		t.Fatalf("forge env render prod --no-charts: %v\nstderr:\n%s", err, stderr)
	}
	if strings.Contains(stdout, "source-controller") {
		t.Errorf("--no-charts must not render chart objects:\n%s", stdout)
	}
	if !strings.Contains(stderr, "NOT RENDERED (--no-charts): helm chart(s) flux") {
		t.Errorf("summary must name the charts --no-charts left out, got stderr:\n%s", stderr)
	}
	if _, serr := os.Stat(helmLog); serr == nil {
		t.Errorf("--no-charts must not invoke helm")
	}
}

// TestEnvRender_TargetSelectsOnlyThatChart: --target applies the deploy's
// selection rule to charts too — a service target renders no chart, a chart
// target renders only that chart.
func TestEnvRender_TargetSelectsOnlyThatChart(t *testing.T) {
	kclplugin.Register()
	fakeHelmOnPath(t)
	dir := writeChartRenderProject(t)

	stdout, stderr, err := runRenderCapturingProcessStdout(t, dir, "prod", "--target", "api")
	if err != nil {
		t.Fatalf("render --target api: %v\nstderr:\n%s", err, stderr)
	}
	if strings.Contains(stdout, "source-controller") {
		t.Errorf("--target api must not render the flux chart:\n%s", stdout)
	}

	stdout, stderr, err = runRenderCapturingProcessStdout(t, dir, "prod", "--target", "flux", "--list")
	if err != nil {
		t.Fatalf("render --target flux: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "source-controller") || !strings.Contains(stdout, "(helm chart flux)") {
		t.Errorf("--target flux --list must list the chart's objects, marked as chart output:\n%s", stdout)
	}
	if strings.Contains(stdout, "Deployment  charttest-prod  api") {
		t.Errorf("--target flux must not list the app:\n%s", stdout)
	}
}
