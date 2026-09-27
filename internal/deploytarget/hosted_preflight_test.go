package deploytarget

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

// preflightGroup is hounders' hosted shape as a group: a registry-less
// backend this project builds (it has no release yet, so no digest and no
// registry), a static site and a database, with references between them.
func preflightGroup(backend v1alpha1.SimpleBackendSpec) ServiceGroup {
	return ServiceGroup{
		Env:        "prod",
		ProviderID: HostedProviderID,
		Services: []ResolvedService{
			{Name: "api", Hosted: &HostedWorkload{Tier: HostedTierBackend, Artifact: "hounders", Backend: &backend}},
			{Name: "web", Hosted: &HostedWorkload{Tier: HostedTierStatic, Static: &v1alpha1.StaticSiteSpec{
				RuntimeConfig: map[string]v1alpha1.RuntimeConfigValue{"API_URL": {WorkloadURL: &v1alpha1.WorkloadURLRef{Name: "api"}}},
			}}},
			{Name: "hounders", Hosted: &HostedWorkload{Tier: HostedTierDatabase, Database: &v1alpha1.ManagedDatabaseSpec{StorageGiB: 1}}},
		},
	}
}

func publicBackend() v1alpha1.SimpleBackendSpec {
	return v1alpha1.SimpleBackendSpec{
		Image: "hounders", Ports: []int32{8080}, Network: v1alpha1.NetworkPublic,
		Env: []v1alpha1.EnvVar{
			{Name: "CORS_ORIGINS", WorkloadURL: &v1alpha1.WorkloadURLRef{Name: "web"}},
			{Name: "DATABASE_URL", DatabaseRef: &v1alpha1.DatabaseRef{Name: "hounders"}},
		},
	}
}

// PreflightHosted runs the deploy's own plan with no release: the admitted
// set is every workload, with the backend pinned to the placeholder — never
// to anything a real deploy could mistake for a pin.
func TestPreflightHostedAdmitsWithoutARelease(t *testing.T) {
	items, err := PreflightHosted(preflightGroup(publicBackend()))
	if err != nil {
		t.Fatalf("PreflightHosted refused an admissible env: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("admitted %d workload(s), want 3", len(items))
	}
	for _, it := range items {
		if it.Backend != nil && !strings.HasPrefix(it.Backend.Image, preflightRegistry+"/hounders@"+preflightDigest) {
			t.Errorf("backend image = %q, want the placeholder pin", it.Backend.Image)
		}
	}
}

// Every rule the deploy's plan applies is applied here, and each refusal is
// the plan's own words. The reference checks are new: the platform would
// publish these specs and wait forever.
func TestPreflightHostedRefusesWhatTheDeployWould(t *testing.T) {
	cases := map[string]struct {
		mutate func(*ServiceGroup)
		want   string
	}{
		"off the shape band": {
			func(g *ServiceGroup) {
				// On the 125m grid, off the 4 GiB-per-vCPU band (250m needs 1 GiB).
				g.Services[0].Hosted.Backend.Resources = v1alpha1.Resources{CPURequestMillicores: 250, MemoryRequestBytes: 128 << 20, MemoryLimitBytes: 128 << 20}
			},
			"shape band",
		},
		"databaseRef to no database": {
			func(g *ServiceGroup) { g.Services = g.Services[:2] },
			`databaseRef "hounders"`,
		},
		"workloadURL to a private backend": {
			func(g *ServiceGroup) { g.Services[0].Hosted.Backend.Network = v1alpha1.NetworkPrivate },
			"only a public backend has a URL",
		},
		"workloadURL to an undeclared workload": {
			func(g *ServiceGroup) {
				g.Services[0].Hosted.Backend.Env[0].WorkloadURL.Name = "admin"
			},
			`references workload "admin", which this env does not publish`,
		},
		"invalid spec": {
			func(g *ServiceGroup) { g.Services[0].Hosted.Backend.Ports = []int32{0} },
			"port 0 must be 1-65535",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			g := preflightGroup(publicBackend())
			tc.mutate(&g)
			_, err := PreflightHosted(g)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "refusing to publish anything") {
				t.Fatalf("err = %v, want the plan's refusal naming %q", err, tc.want)
			}
		})
	}
}

// The reference checks run at DEPLOY time too — the preflight is the plan,
// so a release-bound deploy of a dangling reference is refused before any
// RPC, not published to wait forever.
func TestHostedDeployRefusesADanglingReference(t *testing.T) {
	g := preflightGroup(publicBackend())
	g.Services = g.Services[:2] // no database for DATABASE_URL's databaseRef
	g.Hosted = &HostedTarget{Release: "v1",
		Digests:    map[string]string{"hounders": digestA, "web": digestB},
		Registries: map[string]string{"hounders": "ghcr.io/acme"}}
	if _, err := planHosted(g); err == nil || !strings.Contains(err.Error(), `databaseRef "hounders"`) {
		t.Fatalf("planHosted = %v, want the dangling databaseRef refused", err)
	}
}
