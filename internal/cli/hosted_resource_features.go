package cli

import (
	"context"

	"gopkg.in/yaml.v3"

	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/pkg/release"
)

// requiredSnapshotResourceFeatures reads the sealed CR inventory rather than
// today's KCL: promotion must validate the exact frozen payload it will apply.
func requiredSnapshotResourceFeatures(resources []release.SnapshotResource) ([]string, error) {
	for _, r := range resources {
		if r.Key.Group != "forge.dev" || r.Key.Kind != "StaticSite" {
			continue
		}
		var obj struct {
			Spec struct {
				WellKnown map[string]string `yaml:"wellKnown"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal(r.Spec, &obj); err != nil {
			return nil, err
		}
		if len(obj.Spec.WellKnown) > 0 {
			return []string{deploytarget.ResourceFeatureStaticSiteWellKnown}, nil
		}
	}
	return nil, nil
}

func requireSnapshotResourceFeatures(ctx context.Context, c cloudCaller, resources []release.SnapshotResource) error {
	required, err := requiredSnapshotResourceFeatures(resources)
	if err != nil {
		return err
	}
	return deploytarget.RequireHostedResourceFeatures(ctx, c, required)
}
