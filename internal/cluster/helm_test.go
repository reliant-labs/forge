package cluster

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- pure helpers -----------------------------------------------------------

// TestStampAppLabel_OverridesEveryDoc proves the helm-as-a-RENDERER
// bridge: every manifest a chart renders is FORCED to
// `app.kubernetes.io/name = <name>` so the SAME exclusive --target axis
// (SelectManifestsByGroup) selects the WHOLE chart as one group — even the
// chart's sub-components that label THEMSELVES (cert-manager's webhook /
// cainjector), which would otherwise be dropped by --target=cert-manager.
func TestStampAppLabel_OverridesEveryDoc(t *testing.T) {
	in := `apiVersion: v1
kind: ServiceAccount
metadata:
  name: cert-manager
  namespace: cert-manager
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cert-manager-webhook
  namespace: cert-manager
  labels:
    app.kubernetes.io/name: webhook
spec: {}`

	got := stampAppLabel(in, "cert-manager")

	// The chart's own sub-component label ("webhook") MUST be overridden
	// to the chart name, or --target=cert-manager would drop the webhook.
	if strings.Contains(got, `app.kubernetes.io/name: webhook`) {
		t.Errorf("chart sub-component label must be overridden to the chart name:\n%s", got)
	}

	// Round-trip through SelectManifestsByGroup: a --target=cert-manager
	// deploy must KEEP EVERY doc — the whole chart selects as one group.
	kept := SelectManifestsByGroup(got, []string{"cert-manager"})
	if !strings.Contains(kept, "kind: ServiceAccount") || !strings.Contains(kept, "cert-manager-webhook") {
		t.Errorf("every chart doc must be selected by --target=cert-manager:\n%s", kept)
	}
}

// TestPartitionCRDs_SplitsCRDsFromRest pins the CRD/rest split the
// ordered apply depends on.
func TestPartitionCRDs_SplitsCRDsFromRest(t *testing.T) {
	in := `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: certificates.cert-manager.io
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cert-manager
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: issuers.cert-manager.io`

	crds, rest := partitionCRDs(in)
	if strings.Count(crds, "kind: CustomResourceDefinition") != 2 {
		t.Errorf("expected 2 CRDs in crds partition:\n%s", crds)
	}
	if strings.Contains(crds, "kind: Deployment") {
		t.Errorf("Deployment leaked into crds partition:\n%s", crds)
	}
	if !strings.Contains(rest, "kind: Deployment") {
		t.Errorf("Deployment missing from rest partition:\n%s", rest)
	}

	names := crdNames(in)
	want := []string{"certificates.cert-manager.io", "issuers.cert-manager.io"}
	if len(names) != len(want) {
		t.Fatalf("crdNames = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("crdNames[%d] = %q, want %q", i, names[i], want[i])
		}
	}
}

// TestSelectHelmChartsByGroup_UniformExclusiveRule proves a chart obeys the
// SAME uniform exclusive --target rule as any manifest (a chart's Name is
// its group): EVERY chart on a bare deploy (no targets — the full
// declarative reconcile), EXACTLY the named chart when targeted, and NO
// chart when the target names something else (its manifests are selected by
// SelectManifestsByGroup instead, not here).
func TestSelectHelmChartsByGroup_UniformExclusiveRule(t *testing.T) {
	charts := []HelmChartSpec{
		{Name: "cert-manager"},
		{Name: "envoy-gateway"},
	}

	// No targets → ALL charts (bare `forge deploy <env>` reconciles every
	// declared platform dep — the uniform "no target = everything" rule).
	sel := selectHelmChartsByGroup(charts, nil)
	if len(sel) != 2 {
		t.Errorf("empty targets must select EVERY chart, got %v", sel)
	}

	// --target=cert-manager → exactly that chart.
	sel = selectHelmChartsByGroup(charts, []string{"cert-manager"})
	if len(sel) != 1 || sel[0].Name != "cert-manager" {
		t.Errorf("expected only cert-manager selected, got %v", sel)
	}

	// Mixed: chart + app name → only the chart is selected here (the app
	// name selects manifests via SelectManifestsByGroup, not charts).
	sel = selectHelmChartsByGroup(charts, []string{"envoy-gateway", "api"})
	if len(sel) != 1 || sel[0].Name != "envoy-gateway" {
		t.Errorf("expected only envoy-gateway selected, got %v", sel)
	}

	// A target naming no chart → no chart rendered.
	sel = selectHelmChartsByGroup(charts, []string{"api"})
	if len(sel) != 0 {
		t.Errorf("a target naming no chart must select no chart, got %v", sel)
	}
}

// --- apply ordering (fake kubectl) ------------------------------------------

// fakeKubectlLog installs a fake `kubectl` on PATH that appends its argv
// (one space-joined line per invocation) to a log file, and returns the
// log path. It is the seam for asserting the ORDER of kubectl calls
// (apply CRDs, wait Established, apply rest) without a live cluster.
func fakeKubectlLog(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "kubectl.log")
	// The fake records argv then exits 0. `kubectl wait` and `kubectl
	// apply -f -` both succeed; apply reads stdin so we drain it.
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + logPath + "\n" +
		"cat > /dev/null 2>/dev/null || true\n" +
		"exit 0\n"
	bin := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// TestApplyCRDsThenRest_OrdersCRDsBeforeRest is the apply-ordering proof:
// a manifest set whose chart resources reference a CRD must apply the CRDs
// FIRST, WAIT until Established, and only THEN apply the rest — so a
// subsequent `forge deploy` (the app) finds the CRDs present. Asserts the
// kubectl invocation order: apply (CRD) → wait Established → apply (rest).
func TestApplyCRDsThenRest_OrdersCRDsBeforeRest(t *testing.T) {
	logPath := fakeKubectlLog(t)

	extraCRDs := `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: gateways.gateway.networking.k8s.io`

	// The chart's --skip-crds controllers (a Deployment that needs the CRD).
	rest := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: envoy-gateway
  namespace: envoy-gateway-system
spec: {}`

	if err := applyCRDsThenRest(context.Background(), "k3d-test", extraCRDs, rest); err != nil {
		t.Fatalf("applyCRDsThenRest: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read kubectl log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	// Find the index of: the wait-Established call, and the apply that
	// carries the Deployment (rest). The CRD apply must precede the wait,
	// and the wait must precede the rest apply.
	waitIdx, restApplyIdx, crdApplyIdx := -1, -1, -1
	for i, l := range lines {
		switch {
		case strings.Contains(l, "wait") && strings.Contains(l, "condition=Established"):
			waitIdx = i
		case strings.Contains(l, "apply"):
			// Distinguish CRD apply from rest apply by which came before/after
			// the wait. The first apply is CRDs; a later apply is rest.
			if crdApplyIdx == -1 {
				crdApplyIdx = i
			} else {
				restApplyIdx = i
			}
		}
	}

	if crdApplyIdx == -1 {
		t.Fatalf("expected a CRD apply; kubectl log:\n%s", string(data))
	}
	if waitIdx == -1 {
		t.Fatalf("expected a `kubectl wait --for=condition=Established`; kubectl log:\n%s", string(data))
	}
	if restApplyIdx == -1 {
		t.Fatalf("expected a second apply for the rest; kubectl log:\n%s", string(data))
	}
	if !(crdApplyIdx < waitIdx && waitIdx < restApplyIdx) {
		t.Errorf("apply order must be CRDs → wait Established → rest; got crdApply=%d wait=%d restApply=%d\nlog:\n%s",
			crdApplyIdx, waitIdx, restApplyIdx, string(data))
	}

	// The wait must name the forge-supplied CRD.
	if !strings.Contains(lines[waitIdx], "gateways.gateway.networking.k8s.io") {
		t.Errorf("wait should name the CRD; got %q", lines[waitIdx])
	}
}

// TestApplyCRDsThenRest_NoCRDsSingleApply confirms the degenerate case: a
// stream with no CRDs (and no forge-supplied CRDs) does a plain apply with
// no Established wait — byte-identical to a normal apply.
func TestApplyCRDsThenRest_NoCRDsSingleApply(t *testing.T) {
	logPath := fakeKubectlLog(t)

	rest := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: app
spec: {}`

	if err := applyCRDsThenRest(context.Background(), "k3d-test", "", rest); err != nil {
		t.Fatalf("applyCRDsThenRest: %v", err)
	}
	data, _ := os.ReadFile(logPath)
	if strings.Contains(string(data), "condition=Established") {
		t.Errorf("no-CRD apply must not wait on Established:\n%s", string(data))
	}
}
