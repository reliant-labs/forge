package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/templates"
)

// Proto-first projects declare services only in proto/ (no forge.yaml
// components). CI workflow data must derive HasServices from proto truth —
// the same shape the generate pipeline sees — or the scaffolds silently
// drop buf lint steps and skip proto-breaking.yml.
func TestBuildCIWorkflowData_HasServicesProtoFirst(t *testing.T) {
	root := t.TempDir()
	protoDir := filepath.Join(root, "proto", "services", "widget", "v1")
	if err := os.MkdirAll(protoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	proto := "syntax = \"proto3\";\npackage services.widget.v1;\nservice WidgetService {}\n"
	if err := os.WriteFile(filepath.Join(protoDir, "widget.proto"), []byte(proto), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.ProjectConfig{Name: "proto-first"} // no components declared
	data := buildCIWorkflowData(cfg, root)
	if !data.HasServices {
		t.Fatal("proto-first project (proto service declaration, no forge.yaml components) must set HasServices")
	}
}

func TestBuildCIWorkflowData_NoServicesAnywhere(t *testing.T) {
	cfg := &config.ProjectConfig{Name: "empty"}
	data := buildCIWorkflowData(cfg, t.TempDir())
	if data.HasServices {
		t.Fatal("project with no components and no proto services must leave HasServices false")
	}
}

// The deploy workflow assigns meaning by POSITION: envs[0] auto-deploys
// on every successful image build on main, envs[len-1] carries the
// environment-protection gate and is where a `v*` release tag ships.
// Filesystem discovery returns envs alphabetically, so an unsorted list
// put PROD first: prod auto-deployed on every merge to main with no
// protection, and the release tag shipped to staging forever.
func TestSortByPromotionOrder_ProdIsLast(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"default scaffold, alphabetical", []string{"prod", "staging"}, []string{"staging", "prod"}},
		{"three stages", []string{"preprod", "prod", "staging"}, []string{"staging", "preprod", "prod"}},
		{"bespoke env sorts mid-pipeline", []string{"prod", "sandbox", "staging"}, []string{"staging", "sandbox", "prod"}},
		{"production spelled out", []string{"production", "staging"}, []string{"staging", "production"}},
		{"single env", []string{"prod"}, []string{"prod"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envs := make([]templates.DeployEnv, 0, len(tc.in))
			for _, n := range tc.in {
				envs = append(envs, templates.DeployEnv{Name: n})
			}
			sortByPromotionOrder(envs)
			got := make([]string, 0, len(envs))
			for _, e := range envs {
				got = append(got, e.Name)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("promotion order = %v, want %v", got, tc.want)
			}
		})
	}
}

// End-to-end on the discovery fallback: a project whose only source of
// env truth is deploy/kcl/<env>/main.k must still hand the template a
// promotion-ordered list, so auto-deploy lands on staging and the
// protection gate on prod.
func TestBuildDeployWorkflowData_DiscoveryFallbackOrdersPromotion(t *testing.T) {
	root := t.TempDir()
	for _, env := range []string{"dev", "prod", "staging"} {
		dir := filepath.Join(root, "deploy", "kcl", env)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "main.k"), []byte("manifests = []\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data := buildDeployWorkflowData(&config.ProjectConfig{Name: "demo"}, root)
	if len(data.Environments) != 2 {
		t.Fatalf("want 2 cloud envs (dev is local-only), got %d: %+v", len(data.Environments), data.Environments)
	}
	first, last := data.Environments[0], data.Environments[1]
	if first.Name != "staging" || !first.Auto {
		t.Fatalf("first env must be staging with Auto set, got %+v", first)
	}
	if last.Name != "prod" || !last.Protection {
		t.Fatalf("last env must be prod with Protection set, got %+v", last)
	}
	if first.Protection {
		t.Fatalf("staging must not carry the protection gate: %+v", first)
	}
	if last.Auto {
		t.Fatalf("prod must not auto-deploy on a main merge: %+v", last)
	}
}
