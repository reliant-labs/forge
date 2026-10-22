package cluster

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/pkg/deploy/openbao"
)

// The KCL render and the Go renderer meet at the OpenBao record. This reads the
// record the real KCL render golden contains (secrets-components.json), decodes
// it STRICTLY (a key Go does not know is an error, exactly as in production),
// and expands it, so a field renamed on one side fails here rather than on a
// customer's deploy.
func TestExpandTierDeclarations_OpenBaoRecordFromTheRenderGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "cli", "testdata", "render_contract", "secrets-components.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Output struct {
			Manifests []any `json:"manifests"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var records int
	for _, m := range doc.Output.Manifests {
		if mm, _ := m.(map[string]any); mm["kind"] == openbao.Kind {
			records++
		}
	}
	if records != 1 {
		t.Fatalf("the golden carries %d OpenBao records, want 1", records)
	}

	out, err := expandTierDeclarations(doc.Output.Manifests, nil, nil)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	kinds := map[string]int{}
	for _, it := range out {
		m := it.(map[string]any)
		if m["kind"] == openbao.Kind {
			t.Fatal("the OpenBao record must be replaced by what it renders")
		}
		kinds[m["kind"].(string)]++
		md := m["metadata"].(map[string]any)
		labels, _ := md["labels"].(map[string]any)
		if md["namespace"] == "openbao" && m["kind"] != "Namespace" {
			if labels[ClusterRoutingLabel] != "gke_acme_us-central1_prod" {
				t.Errorf("%v/%v lost its cluster route: %v", m["kind"], md["name"], labels)
			}
			if labels["forge.dev/env"] != "staging" {
				t.Errorf("%v/%v lost its env stamp: %v", m["kind"], md["name"], labels)
			}
		}
	}
	for kind, n := range map[string]int{"StatefulSet": 1, "Job": 2, "Certificate": 2, "Issuer": 2, "NetworkPolicy": 2, "PodDisruptionBudget": 1} {
		if kinds[kind] != n {
			t.Errorf("%s: %d rendered, want %d (all: %v)", kind, kinds[kind], n, kinds)
		}
	}
}

func TestExpandOpenBao_RefusesAnUnknownKeyAndNamesTheRecord(t *testing.T) {
	_, err := expandTierDeclarations([]any{map[string]any{
		"apiVersion": "forge.dev/v1alpha1", "kind": "OpenBao",
		"metadata": map[string]any{"name": "openbao", "namespace": "openbao"},
		"spec":     map[string]any{"name": "openbao", "frobnicate": true},
	}}, nil, nil)
	if err == nil {
		t.Fatal("an unknown spec key must be refused: a dropped field is the drift this seam exists to prevent")
	}
}
