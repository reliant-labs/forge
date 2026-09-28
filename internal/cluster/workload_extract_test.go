package cluster

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// workloadRecords is an `output` with three Cluster workload records across
// TWO (cluster, namespace) sets, interleaved with support objects, plus the
// env NetworkPolicy bundle. `migrate` gates `api` (before), which is a
// relation only a SET render can resolve.
const workloadRecords = `output:
  network_policy:
    egress_ports: [443]
    telemetry_namespace: ""
    ingress_namespace: ""
  manifests:
  - apiVersion: v1
    kind: Namespace
    metadata:
      name: acme-dev
  - apiVersion: forge.dev/v1alpha1
    kind: Workload
    metadata:
      name: api
      namespace: acme-dev
      labels:
        app.kubernetes.io/name: api
        app.kubernetes.io/part-of: acme
        forge.dev/cluster: k3d-acme
        forge.dev/env: dev
    spec:
      kind: service
      image: localhost:5050/acme:dev
      args: ["api"]
      ports: [{name: http, port: 8080}]
  - apiVersion: v1
    kind: ConfigMap
    metadata:
      name: acme-config
      namespace: acme-dev
  - apiVersion: forge.dev/v1alpha1
    kind: Workload
    metadata:
      name: search
      namespace: acme-search
      labels:
        app.kubernetes.io/name: search
        app.kubernetes.io/part-of: acme
        forge.dev/cluster: k3d-acme-daemon
        forge.dev/env: dev
    spec:
      kind: worker
      image: localhost:5050/acme:dev
      args: ["search"]
  - apiVersion: forge.dev/v1alpha1
    kind: Workload
    metadata:
      name: migrate
      namespace: acme-dev
      labels:
        app.kubernetes.io/name: migrate
        app.kubernetes.io/part-of: acme
        forge.dev/cluster: k3d-acme
        forge.dev/env: dev
    spec:
      kind: job
      image: localhost:5050/acme:dev
      args: ["db", "migrate", "up"]
      before: ["api"]
`

type extractedObj struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				InitContainers []struct {
					Name string `json:"name"`
				} `json:"initContainers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

func extractObjs(t *testing.T, in string) []extractedObj {
	t.Helper()
	got, err := ExtractManifests([]byte(in))
	if err != nil {
		t.Fatalf("ExtractManifests: %v", err)
	}
	var out []extractedObj
	for _, doc := range splitDocs(got) {
		var o extractedObj
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
			t.Fatalf("parse %q: %v", doc, err)
		}
		out = append(out, o)
	}
	return out
}

// TestExtractManifests_RendersWorkloadRecordsPerSet: records are grouped by
// (forge.dev/cluster, namespace) and each group is ONE RenderWorkloads call.
// The proof is the `before` gate: `migrate` becomes an initContainer on
// `api`'s Deployment, which a per-record render cannot produce (it never sees
// both). No record survives into the applied stream, and every object carries
// its group's cluster routing label.
func TestExtractManifests_RendersWorkloadRecordsPerSet(t *testing.T) {
	objs := extractObjs(t, workloadRecords)
	var apiDeploy *extractedObj
	for i, o := range objs {
		if o.Kind == "Workload" {
			t.Errorf("a Workload record reached the applied stream: %s", o.Metadata.Name)
		}
		if o.Kind == "Deployment" && o.Metadata.Name == "api" {
			apiDeploy = &objs[i]
		}
	}
	if apiDeploy == nil {
		t.Fatalf("no api Deployment rendered; kinds: %v", kinds(objs))
	}
	var gated bool
	for _, ic := range apiDeploy.Spec.Template.Spec.InitContainers {
		if strings.Contains(ic.Name, "migrate") {
			gated = true
		}
	}
	if !gated {
		t.Errorf("api is not gated by migrate: the records were not rendered as one set (initContainers %+v)", apiDeploy.Spec.Template.Spec.InitContainers)
	}
	for _, o := range objs {
		switch o.Metadata.Namespace {
		case "acme-dev":
			if o.Kind != "ConfigMap" && o.Metadata.Labels[ClusterRoutingLabel] != "k3d-acme" {
				t.Errorf("%s/%s in acme-dev routes to %q, want k3d-acme", o.Kind, o.Metadata.Name, o.Metadata.Labels[ClusterRoutingLabel])
			}
		case "acme-search":
			if o.Metadata.Labels[ClusterRoutingLabel] != "k3d-acme-daemon" {
				t.Errorf("%s/%s in acme-search routes to %q, want k3d-acme-daemon", o.Kind, o.Metadata.Name, o.Metadata.Labels[ClusterRoutingLabel])
			}
		}
	}
}

// TestExtractManifests_KeepsStreamOrder: a group's objects are spliced in at
// its first record's position, so support objects declared before it still
// apply before it and the ConfigMap between two records of one group stays
// where it was relative to the group.
func TestExtractManifests_KeepsStreamOrder(t *testing.T) {
	objs := extractObjs(t, workloadRecords)
	ks := kinds(objs)
	if ks[0] != "Namespace" {
		t.Errorf("first object = %s, want the Namespace declared first", ks[0])
	}
	firstAPI, cm := -1, -1
	for i, o := range objs {
		if o.Metadata.Name == "api" && firstAPI < 0 {
			firstAPI = i
		}
		if o.Kind == "ConfigMap" {
			cm = i
		}
	}
	if !(firstAPI > 0 && firstAPI < cm) {
		t.Errorf("group objects not at the first record's position: api at %d, ConfigMap at %d (%v)", firstAPI, cm, ks)
	}
}

// TestExtractManifests_EnvNetworkPolicyPerGroup: output.network_policy is the
// env bundle, passed to every group's render, so each namespace the env
// deploys into gets its default-deny.
func TestExtractManifests_EnvNetworkPolicyPerGroup(t *testing.T) {
	objs := extractObjs(t, workloadRecords)
	byNS := map[string]int{}
	for _, o := range objs {
		if o.Kind == "NetworkPolicy" && o.Metadata.Labels[deployLabelName] == "" {
			byNS[o.Metadata.Namespace]++
		}
	}
	if byNS["acme-dev"] == 0 || byNS["acme-search"] == 0 {
		t.Errorf("env NetworkPolicy bundle per namespace = %v, want both acme-dev and acme-search", byNS)
	}
}

// TestExtractManifests_OneEntrypoint: a top-level `manifests` is refused, and
// a render with no `output` is refused.
func TestExtractManifests_OneEntrypoint(t *testing.T) {
	if _, err := ExtractManifests([]byte("manifests: []\noutput: {manifests: []}\n")); err == nil || !strings.Contains(err.Error(), "forge.render(bundle)") {
		t.Errorf("top-level manifests: err = %v, want a refusal naming forge.render(bundle)", err)
	}
	if _, err := ExtractManifests([]byte("other: 1\n")); err == nil {
		t.Error("no output: want an error")
	}
	got, err := ExtractManifests([]byte("output:\n  workloads: []\n"))
	if err != nil || strings.TrimSpace(got) != "" {
		t.Errorf("output with no manifests: got %q, %v; want an empty stream", got, err)
	}
}

// TestExtractManifests_RefusesUnknownSpecField: records decode strictly.
func TestExtractManifests_RefusesUnknownSpecField(t *testing.T) {
	in := strings.Replace(workloadRecords, "      kind: worker\n", "      kind: worker\n      network: public\n", 1)
	if _, err := ExtractManifests([]byte(in)); err == nil || !strings.Contains(err.Error(), "network") {
		t.Errorf("err = %v, want a strict-decode refusal naming the unknown field", err)
	}
}

const deployLabelName = "app.kubernetes.io/name"

func kinds(objs []extractedObj) []string {
	out := make([]string, len(objs))
	for i, o := range objs {
		out[i] = o.Kind
	}
	return out
}
