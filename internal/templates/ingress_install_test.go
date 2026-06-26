package templates_test

import (
	"bytes"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/reliant-labs/forge/internal/templates"
)

// TestEnvoyGatewayClassVendored guards the vendored `eg` GatewayClass
// forge applies after the Envoy Gateway helm install at `forge cluster
// up` / `forge up --env=<env>` time (internal/cli/dev_cluster_ingress.go).
// It must name the `eg` class and the Envoy Gateway controllerName — the
// SINGLE Gateway API controller every forge env (local k3d AND cloud)
// uses, referenced by the schema-default `gateway_class_name = "eg"`.
func TestEnvoyGatewayClassVendored(t *testing.T) {
	raw, err := templates.IngressTemplates().Get("envoy/gatewayclass.yaml")
	if err != nil {
		t.Fatalf("Get(envoy/gatewayclass.yaml): %v", err)
	}
	docs := parseDocs(t, raw)
	gc := findResource(t, docs, "GatewayClass", "eg")

	spec, _ := gc["spec"].(map[string]any)
	controllerName, _ := spec["controllerName"].(string)
	if controllerName != "gateway.envoyproxy.io/gatewayclass-controller" {
		t.Errorf("GatewayClass eg controllerName = %q, want gateway.envoyproxy.io/gatewayclass-controller", controllerName)
	}

	apiVersion, _ := gc["apiVersion"].(string)
	if !strings.HasPrefix(apiVersion, "gateway.networking.k8s.io/") {
		t.Errorf("GatewayClass apiVersion = %q, want gateway.networking.k8s.io/*", apiVersion)
	}
}

// parseDocs decodes a multi-doc YAML stream into a slice of maps.
func parseDocs(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var docs []map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("yaml parse: %v", err)
		}
		if doc != nil {
			docs = append(docs, doc)
		}
	}
	return docs
}

// findResource returns the YAML doc whose kind+metadata.name match.
func findResource(t *testing.T, docs []map[string]any, kind, name string) map[string]any {
	t.Helper()
	for _, d := range docs {
		k, _ := d["kind"].(string)
		if k != kind {
			continue
		}
		md, _ := d["metadata"].(map[string]any)
		n, _ := md["name"].(string)
		if n == name {
			return d
		}
	}
	t.Fatalf("could not find %s/%s in gatewayclass.yaml", kind, name)
	return nil
}
