package templates_test

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/templates"
)

func TestIngressTemplatesEmbedded(t *testing.T) {
	cat := templates.IngressTemplates()
	for _, name := range []string{
		"traefik/VERSION",
		"traefik/traefik.yaml",
		"traefik/gatewayclass.yaml",
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
	b, _ := cat.Get("traefik/VERSION")
	for _, want := range []string{"traefik=", "gateway_api="} {
		if !strings.Contains(string(b), want) {
			t.Errorf("VERSION missing %q in:\n%s", want, b)
		}
	}
}
