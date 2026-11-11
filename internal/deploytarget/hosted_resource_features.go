package deploytarget

import (
	"context"
	"fmt"
	"slices"
)

// ResourceFeatureStaticSiteWellKnown is advertised in GetCapabilities.resourceFeatures
// only when the platform accepts StaticSite.spec.wellKnown AND serves it after
// every sync. It is not a bundle-document feature: it gates the CR payload.
const ResourceFeatureStaticSiteWellKnown = "static-site-well-known"

// RequiredHostedResourceFeatures lists capabilities a group's CR payloads
// require. Empty legacy groups make no capability request.
func RequiredHostedResourceFeatures(group ServiceGroup) []string {
	for _, s := range group.Services {
		if s.Hosted != nil && s.Hosted.Static != nil && len(s.Hosted.Static.WellKnown) > 0 {
			return []string{ResourceFeatureStaticSiteWellKnown}
		}
	}
	return nil
}

// RequireHostedResourceFeatures refuses unsupported CR payloads BEFORE any
// publish, seal or promotion. Missing fields, an old RPC, and network errors
// never authorize dropping verification files or publishing an unrecognized
// spec. Empty required leaves every legacy deployment unchanged.
func RequireHostedResourceFeatures(ctx context.Context, c HostedCaller, required []string) error {
	if len(required) == 0 {
		return nil
	}
	var caps struct {
		Features []string `json:"resourceFeatures"`
	}
	if err := c.Call(ctx, ProcDeployCapabilities, map[string]any{}, &caps); err != nil {
		return fmt.Errorf("hosted resource capabilities %v required before publishing: upgrade the control plane to support StaticSite.spec.wellKnown: %w", required, err)
	}
	for _, f := range required {
		if !slices.Contains(caps.Features, f) {
			return fmt.Errorf("control plane does not advertise resourceFeatures %q: upgrade it before publishing StaticSite.spec.wellKnown; association files cannot be silently omitted", f)
		}
	}
	return nil
}
