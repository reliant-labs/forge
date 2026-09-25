package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/reliant-labs/forge/internal/buildtarget"
	"github.com/reliant-labs/forge/internal/deploytarget"
)

// hostedStaticPusher builds, packs and pushes one hosted StaticSite and
// returns the release digest. A var so the CLI tests state the registry's
// answer (an in-memory push) instead of needing a registry.
var hostedStaticPusher = func(ctx context.Context, projectDir, repository string, fe deploytarget.StaticSiteFrontend) (string, error) {
	tree, err := deploytarget.BuildStaticSiteTree(ctx, projectDir, fe)
	if err != nil {
		return "", err
	}
	layer, err := deploytarget.PackStaticSiteTree(tree)
	if err != nil {
		return "", err
	}
	return deploytarget.PushStaticSiteArtifact(ctx, repository, layer)
}

// buildHostedStaticSites is the BUILD half of a hosted StaticSite: for an env
// whose Bundle declares control_plane, every forge.StaticSite frontend is
// built, assembled, and pushed as an OCI release artifact to
// `<push>/static.v1/<frontend>` — where `<push>` is the org's image push base,
// the one registry subtree the control plane admits this org's artifacts from.
//
// The digest is recorded as ordinary build state under the frontend's name,
// with the registry set to `<push>/static.v1`, so the existing harvest turns
// it into a shared OCI release artifact whose URI + name is the real
// repository (and `forge release verify` can fetch its manifest).
//
// It runs only with --push: a site release that is not registry-addressable
// cannot be pinned, promoted or pulled by the platform.
func buildHostedStaticSites(ctx context.Context, projectDir string, entities *KCLEntities, opts buildOptions) error {
	if entities == nil || entities.ControlPlane == nil {
		return nil
	}
	var sites []FrontendEntity
	for _, f := range entities.Frontends {
		if f.Deploy == nil || f.Deploy.Type != frontendDeployStaticSite || f.Deploy.StaticSite == nil {
			continue
		}
		if opts.buildTarget != "" && opts.buildTarget != "all" && opts.buildTarget != f.Name {
			continue
		}
		sites = append(sites, f)
	}
	if len(sites) == 0 {
		return nil
	}
	if opts.pushRegistry == "" {
		names := make([]string, 0, len(sites))
		for _, f := range sites {
			names = append(names, f.Name)
		}
		return fmt.Errorf("env %q is hosted and declares forge.StaticSite frontend(s) %s: a hosted site ships as an OCI release artifact, "+
			"so the build needs --push <image push base> (the registry subtree the control plane admits this org's artifacts from)",
			opts.env, strings.Join(names, ", "))
	}
	if err := resolveFrontendEntitySources(ctx, projectDir, entities); err != nil {
		return err
	}
	runtimeConfigs, err := renderFrontendRuntimeDocs(projectDir, opts.env)
	if err != nil {
		return err
	}
	registry := strings.TrimSuffix(opts.pushRegistry, "/") + "/" + deploytarget.StaticSiteRepositorySegment
	for _, f := range sites {
		if err := checkDeployableFrontendMock(f); err != nil {
			return err
		}
		fe := frontendToStaticSite(f)
		fe.RuntimeConfigJS = runtimeConfigs[f.Name]
		repository := deploytarget.HostedStaticRepository(opts.pushRegistry, f.Name)
		fmt.Printf("[build] %s: hosted static site → %s\n", f.Name, repository)
		digest, err := hostedStaticPusher(ctx, projectDir, repository, fe)
		if err != nil {
			return fmt.Errorf("hosted static site %s: %w", f.Name, err)
		}
		state := buildtarget.State{
			Service:  f.Name,
			Image:    f.Name,
			Tag:      digest,
			Registry: registry,
			PushedAt: nowRFC3339(),
			Digest:   digest,
		}
		if err := buildtarget.WriteState(projectDir, opts.env, state); err != nil {
			return fmt.Errorf("hosted static site %s: record build state: %w", f.Name, err)
		}
		fmt.Printf("[build]   %s release %s\n", f.Name, digest)
	}
	return nil
}
