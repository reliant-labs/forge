package templates_test

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/templates"
)

func TestIngressTemplatesEmbedded(t *testing.T) {
	cat := templates.IngressTemplates()
	// The local ingress install is Envoy Gateway: VERSION pins the
	// gateway-helm chart + Gateway API CRD versions, gatewayclass.yaml
	// is the vendored `eg` GatewayClass forge applies after the helm
	// install. See internal/cli/dev_cluster_ingress.go.
	for _, name := range []string{
		"envoy/VERSION",
		"envoy/gatewayclass.yaml",
	} {
		b, err := cat.Get(name)
		if err != nil {
			t.Errorf("Get(%q): %v", name, err)
			continue
		}
		if len(b) == 0 {
			t.Errorf("Get(%q) returned empty bytes", name)
		}
	}
	// VERSION carries both pin lines for cluster-up to read.
	b, _ := cat.Get("envoy/VERSION")
	for _, want := range []string{"envoy_gateway=", "gateway_api="} {
		if !strings.Contains(string(b), want) {
			t.Errorf("VERSION missing %q in:\n%s", want, b)
		}
	}
}
