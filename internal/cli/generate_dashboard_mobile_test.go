package cli

import (
	"reflect"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

func TestFrontendIdentitiesExcludeNativeAliases(t *testing.T) {
	for _, kind := range []string{"rn", "react-native", "expo", " RN ", " React-Native ", " EXPO "} {
		t.Run(kind, func(t *testing.T) {
			for _, source := range []*config.GitSource{nil, {Repo: "example.com/mobile"}} {
				names, services := frontendIdentities("sample", []FrontendEntity{
					{Name: "mobile", Type: kind, Source: source},
					{Name: "web", Type: "vite"},
					{Name: "web", Type: "vite-spa"},
					{Name: "site", Type: "nextjs"},
				})
				if !reflect.DeepEqual(names, []string{"web", "site"}) || !reflect.DeepEqual(services, []string{"sample-site", "sample-web"}) {
					t.Fatalf("browser identities: names=%v services=%v", names, services)
				}
				names, services = frontendIdentities("sample", []FrontendEntity{{Name: "mobile", Type: kind, Source: source}})
				if len(names)+len(services) != 0 {
					t.Fatalf("native-only identities: %v %v", names, services)
				}
			}
		})
	}
}
