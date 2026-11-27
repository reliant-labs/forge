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
			if got, warnings := hostedFrontendURL(tc.ws, "web", tc.base); got != tc.want || len(warnings) != 0 {
				t.Errorf("hostedFrontendURL = %q %v, want %q and no warning", got, warnings, tc.want)
			}
		})
	}
}

// A platform URL is joined with base_path segment by segment, through
// url.URL, and its query and fragment are dropped with a warning: neither
// belongs in a canonical origin.
func TestHostedFrontendURLPlatformURLShapes(t *testing.T) {
	static := string(deploytarget.HostedTierStatic)
	for _, tc := range []struct {
		name, url, base, want string
		warn                  bool
	}{
		{"path, base appended", "https://acme.sites.example.dev/sites/web/", "/docs", "https://acme.sites.example.dev/sites/web/docs", false},
		{"path already ends with the mount", "https://acme.sites.example.dev/sites/docs", "docs/", "https://acme.sites.example.dev/sites/docs", false},
		{"a suffix that is not a segment is appended", "https://acme.sites.example.dev/mydocs", "/docs", "https://acme.sites.example.dev/mydocs/docs", false},
		{"multi-segment base", "https://acme.sites.example.dev/a/b", "/a/b", "https://acme.sites.example.dev/a/b", false},
		{"escaped segment kept escaped", "https://acme.sites.example.dev/caf%C3%A9", "", "https://acme.sites.example.dev/caf%C3%A9", false},
		{"query dropped", "https://acme.sites.example.dev/?ref=x", "", "https://acme.sites.example.dev", true},
		{"fragment dropped", "https://acme.sites.example.dev/web#top", "/docs", "https://acme.sites.example.dev/web/docs", true},
		{"empty query dropped", "https://acme.sites.example.dev/?", "", "https://acme.sites.example.dev", true},
		{"userinfo refused", "https://user:pw@acme.sites.example.dev/", "", "", true},
		{"not http(s)", "ftp://acme.sites.example.dev/", "", "", true},
		{"relative", "/web", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, warnings := hostedFrontendURL([]deploytarget.HostedWorkloadStatus{{Name: "web", Tier: static, URL: tc.url}}, "web", tc.base)
			if got != tc.want || (len(warnings) > 0) != tc.warn {
				t.Errorf("hostedFrontendURL(%q, base %q) = %q, warnings %v; want %q, warning %v", tc.url, tc.base, got, warnings, tc.want, tc.warn)
			}
		})
	}
}

// A custom domain must be a bare hostname. A malformed one is skipped with a
// warning, for the next live domain or the platform URL: a value like
// "evil.example/x" or "https://www.example.com" must never become the
// canonical origin as written.
func TestHostedFrontendURLSkipsMalformedDomains(t *testing.T) {
	const platform = "https://web-acme.sites.example.dev"
	for _, bad := range []string{
		"https://www.example.com", "www.example.com/path", "www.example.com?x=1", "www.example.com#top",
		"user@www.example.com", "www.example.com:8443", "localhost", "-bad.example.com", "bad-.example.com",
		"www..example.com", "www.exa mple.com", "www.example.com.", strings.Repeat("a", 64) + ".example.com",
	} {
		t.Run(bad, func(t *testing.T) {
			ws := []deploytarget.HostedWorkloadStatus{{Name: "web", Tier: string(deploytarget.HostedTierStatic), URL: platform,
				Domains: []topology.CustomDomain{{Domain: bad, State: "live"}}}}
			got, warnings := hostedFrontendURL(ws, "web", "")
			if got != platform || len(warnings) != 1 || !strings.Contains(warnings[0], "not a bare hostname") {
				t.Errorf("domain %q: got %q, warnings %v; want the platform URL and one warning", bad, got, warnings)
			}
		})
	}
	// The next live domain that is well-formed wins over the platform URL.
	ws := []deploytarget.HostedWorkloadStatus{{Name: "web", Tier: string(deploytarget.HostedTierStatic), URL: platform,
		Domains: []topology.CustomDomain{{Domain: "https://bad.example.com", State: "live"}, {Domain: "WWW.Example.com", State: "live"}}}}
	if got, warnings := hostedFrontendURL(ws, "web", "/docs"); got != "https://www.example.com/docs" || len(warnings) != 1 {
		t.Errorf("got %q, warnings %v; want the second domain, lower-cased, with the base path", got, warnings)
	}
}

// resolveBuildFrontendURLs prints the resolver's warnings into the build.
func TestResolveBuildFrontendURLsPrintsWarnings(t *testing.T) {
	stubFrontendURLStatus(t, deploytarget.HostedEnvStatus{Workloads: []deploytarget.HostedWorkloadStatus{{
		Name: "web", Tier: string(deploytarget.HostedTierStatic), URL: "https://web-acme.sites.example.dev",
		Domains: []topology.CustomDomain{{Domain: "www.example.com/x", State: "live"}},
	}}}, nil)
	var out bytes.Buffer
	got := resolveBuildFrontendURLs(context.Background(), &out, "prod", nil, siteURLFrontend())
	if got.EnvVars[0].Value != "https://web-acme.sites.example.dev" || !strings.Contains(out.String(), `custom domain "www.example.com/x" is not a bare hostname`) {
		t.Errorf("value %q, output:\n%s", got.EnvVars[0].Value, out.String())
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
