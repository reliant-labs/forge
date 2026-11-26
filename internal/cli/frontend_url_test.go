package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/pkg/topology"
)

// stubFrontendURLStatus states the control plane's answer to the status read
// a build resolves forge.FrontendURL from.
func stubFrontendURLStatus(t *testing.T, st deploytarget.HostedEnvStatus, err error) *int {
	t.Helper()
	calls := 0
	prev := frontendURLStatusReader
	frontendURLStatusReader = func(context.Context, string, *KCLEntities) (deploytarget.HostedEnvStatus, error) {
		calls++
		return st, err
	}
	t.Cleanup(func() { frontendURLStatusReader = prev })
	return &calls
}

func siteURLFrontend() FrontendEntity {
	return FrontendEntity{Name: "web", EnvVars: []KCLEnvVar{
		{Name: "SITE_URL", FrontendURL: &KCLFrontendURL{Frontend: "web"}},
		{Name: "VITE_FLAG", Value: "on"},
	}}
}

// A live custom domain is the site's origin; until one is live, the
// platform's hostname is. A declared base_path is the mount on either.
func TestHostedFrontendURL(t *testing.T) {
	static := string(deploytarget.HostedTierStatic)
	for _, tc := range []struct {
		name string
		ws   []deploytarget.HostedWorkloadStatus
		base string
		want string
	}{
		{"platform hostname", []deploytarget.HostedWorkloadStatus{{Name: "web", Tier: static, URL: "https://web-acme.sites.example.dev/"}}, "", "https://web-acme.sites.example.dev"},
		{"live domain wins", []deploytarget.HostedWorkloadStatus{{Name: "web", Tier: static, URL: "https://web-acme.sites.example.dev",
			Domains: []topology.CustomDomain{{Domain: "pending.example.com", State: "pending_dns"}, {Domain: "www.example.com", State: "live"}}}}, "", "https://www.example.com"},
		{"pending domain is not the origin", []deploytarget.HostedWorkloadStatus{{Name: "web", Tier: static, URL: "https://web-acme.sites.example.dev",
			Domains: []topology.CustomDomain{{Domain: "www.example.com", State: "issuing"}}}}, "", "https://web-acme.sites.example.dev"},
		{"base path appended", []deploytarget.HostedWorkloadStatus{{Name: "web", Tier: static, URL: "https://web-acme.sites.example.dev"}}, "/docs/", "https://web-acme.sites.example.dev/docs"},
		{"base path not doubled", []deploytarget.HostedWorkloadStatus{{Name: "web", Tier: static, URL: "https://web-acme.sites.example.dev/docs"}}, "/docs", "https://web-acme.sites.example.dev/docs"},
		{"no hostname yet", []deploytarget.HostedWorkloadStatus{{Name: "web", Tier: static}}, "", ""},
		{"a workload of the same name is not the site", []deploytarget.HostedWorkloadStatus{{Name: "web", Tier: string(deploytarget.HostedTierWorkload), URL: "https://api.example.dev"}}, "", ""},
		{"absent", nil, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostedFrontendURL(tc.ws, "web", tc.base); got != tc.want {
				t.Errorf("hostedFrontendURL = %q, want %q", got, tc.want)
			}
		})
	}
}

// The build resolves a kept FrontendURL from the env's live status, once per
// frontend, and leaves every other var as the render wrote it.
func TestResolveBuildFrontendURLs(t *testing.T) {
	calls := stubFrontendURLStatus(t, deploytarget.HostedEnvStatus{Workloads: []deploytarget.HostedWorkloadStatus{
		{Name: "web", Tier: string(deploytarget.HostedTierStatic), URL: "https://web-acme.sites.example.dev"},
	}}, nil)
	f := siteURLFrontend()
	var out bytes.Buffer
	got := resolveBuildFrontendURLs(context.Background(), &out, "prod", &KCLEntities{Frontends: []FrontendEntity{f}}, f)
	if got.EnvVars[0].Value != "https://web-acme.sites.example.dev" || got.EnvVars[1].Value != "on" {
		t.Errorf("resolved env vars = %+v", got.EnvVars)
	}
	if f.EnvVars[0].Value != "" {
		t.Error("resolution wrote through to the rendered entity's env vars")
	}
	if *calls != 1 {
		t.Errorf("status read %d times, want 1", *calls)
	}
	if env := frontendBuildEnv(got); env["SITE_URL"] != "https://web-acme.sites.example.dev" {
		t.Errorf("build env SITE_URL = %q", env["SITE_URL"])
	}
	if !strings.Contains(out.String(), "SITE_URL=https://web-acme.sites.example.dev") {
		t.Errorf("the build does not say what it resolved:\n%s", out.String())
	}
}

// A build never fails for want of a hostname: the variable is present but
// empty (so a shell value cannot leak in), and the build says why and what
// carries it next.
func TestResolveBuildFrontendURLsWithoutAHostname(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   deploytarget.HostedEnvStatus
		err  error
		want string
	}{
		{"never deployed", deploytarget.HostedEnvStatus{}, deploytarget.ErrHostedEnvironmentNotFound, "env prod has never been deployed"},
		{"control plane unreadable", deploytarget.HostedEnvStatus{}, errors.New("not signed in"), "not signed in"},
		{"no hostname yet", deploytarget.HostedEnvStatus{Workloads: []deploytarget.HostedWorkloadStatus{{Name: "web", Tier: string(deploytarget.HostedTierStatic)}}}, nil, "frontend web has no hostname in env prod yet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubFrontendURLStatus(t, tc.st, tc.err)
			f := siteURLFrontend()
			var out bytes.Buffer
			got := resolveBuildFrontendURLs(context.Background(), &out, "prod", nil, f)
			env := frontendBuildEnv(got)
			if v, ok := env["SITE_URL"]; !ok || v != "" {
				t.Errorf("build env SITE_URL = %q (present %v), want present and empty", v, ok)
			}
			if !strings.Contains(out.String(), tc.want) || !strings.Contains(out.String(), "build the next release") {
				t.Errorf("message does not name %q and the remedy:\n%s", tc.want, out.String())
			}
		})
	}
}

// A frontend with no kept FrontendURL never reads the control plane.
func TestResolveBuildFrontendURLsReadsNothingWithoutAReference(t *testing.T) {
	calls := stubFrontendURLStatus(t, deploytarget.HostedEnvStatus{}, nil)
	f := FrontendEntity{Name: "web", EnvVars: []KCLEnvVar{{Name: "SITE_URL", Value: "https://example.com"}}}
	got := resolveBuildFrontendURLs(context.Background(), &bytes.Buffer{}, "prod", nil, f)
	if *calls != 0 || got.EnvVars[0].Value != "https://example.com" {
		t.Errorf("calls = %d, vars = %+v", *calls, got.EnvVars)
	}
}

const hostedFrontendURLBundle = `    project = "acme"
    env = "prod"
    control_plane = forge.ControlPlane { endpoint = "https://cp.example"}
    frontends = [forge.Frontend {
        name = "web"
        image = "web"
        path = "."
        type = "vite"
        public_dir = "dist"
        env_vars = [forge.EnvVar {name = "SITE_URL", frontend_url = forge.FrontendURL {frontend = "web"}}]
        runtime = forge.OnHosted {}
    }]`

// TestFrontendURLRendersToTheHostedBuildEnv: through the real KCL module, a
// forge.FrontendURL to a hosted site is kept by the render and resolved by the
// release build into the site's build-time env, from the env's live status.
func TestFrontendURLRendersToTheHostedBuildEnv(t *testing.T) {
	if testing.Short() {
		t.Skip("renders KCL; skipped in -short")
	}
	dir := workloadURLProject(t, "prod", hostedFrontendURLBundle)
	t.Chdir(dir)
	entities, err := RenderKCL(context.Background(), dir, "prod")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(entities.Frontends) != 1 || len(entities.Frontends[0].EnvVars) != 1 {
		t.Fatalf("frontends = %+v", entities.Frontends)
	}
	if ev := entities.Frontends[0].EnvVars[0]; ev.FrontendURL == nil || ev.FrontendURL.Frontend != "web" || ev.Value != "" {
		t.Fatalf("rendered SITE_URL = %+v, want the reference kept for the build", ev)
	}
	stubFrontendURLStatus(t, deploytarget.HostedEnvStatus{Workloads: []deploytarget.HostedWorkloadStatus{{
		Name: "web", Tier: string(deploytarget.HostedTierStatic), URL: "https://web-acme.sites.example.dev",
		Domains: []topology.CustomDomain{{Domain: "www.example.com", State: "live"}},
	}}}, nil)
	var got []deploytarget.StaticSiteFrontend
	prev := hostedStaticPusher
	hostedStaticPusher = func(_ context.Context, _, _ string, fe deploytarget.StaticSiteFrontend) (string, error) {
		got = append(got, fe)
		return hostedStaticDigest, nil
	}
	t.Cleanup(func() { hostedStaticPusher = prev })
	t.Setenv("FORGE_HOME", t.TempDir())
	if err := buildHostedStaticSites(context.Background(), dir, entities, buildOptions{env: "prod", release: "v0.1.0", pushPlan: pushPlan{push: true, env: "prod"}}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].BuildEnv["SITE_URL"] != "https://www.example.com" {
		t.Fatalf("pushed sites = %+v, want one built with SITE_URL=https://www.example.com", got)
	}
}
