package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"

	"github.com/reliant-labs/forge/internal/deploytarget"
)

// frontendURLStatusReader reads the hosted status a build resolves a
// forge.FrontendURL from. A var so the CLI tests state the control plane's
// answer instead of needing one.
var frontendURLStatusReader hostedStatusReader = readHostedStatusFromDeclaration

// resolveBuildFrontendURLs returns f with every forge.FrontendURL the render
// kept (one naming a hosted frontend: kcl/schema.k FrontendURL) resolved into
// its Value, from the env's live status: the target's first live custom
// domain, else its platform URL, plus the target's base_path.
//
// The value is a fact of the env the release is BUILT for. A hosted release
// is promoted by digest, so every env it reaches serves the same origin in
// its HTML — which is what a canonical link wants.
//
// It never fails the build. An env that has never been deployed, a site that
// has no hostname yet, or a control plane that cannot be read leaves the
// value empty and says so on w: the starter templates omit the tags that need
// an origin, the site audit names them, and the next release carries it.
func resolveBuildFrontendURLs(ctx context.Context, w io.Writer, envName string, e *KCLEntities, f FrontendEntity) FrontendEntity {
	if !slices.ContainsFunc(f.EnvVars, keptFrontendURL) {
		return f
	}
	st, err := frontendURLStatusReader(ctx, envName, e)
	vars := slices.Clone(f.EnvVars)
	for i, ev := range vars {
		if !keptFrontendURL(ev) {
			continue
		}
		target := ev.FrontendURL.Frontend
		var u string
		if err == nil {
			u = hostedFrontendURL(st.Workloads, target, frontendBasePath(e, target))
		}
		if u == "" {
			_, _ = fmt.Fprintf(w, "[build] %s: %s is empty: %s. The build leaves out what needs the site's origin "+
				"(canonical link, og:url, the sitemap); deploy env %s, then build the next release to carry it.\n",
				f.Name, ev.Name, frontendURLGap(err, envName, target), envName)
			continue
		}
		vars[i].Value = u
		_, _ = fmt.Fprintf(w, "[build] %s: %s=%s (forge.FrontendURL %s in env %s)\n", f.Name, ev.Name, u, target, envName)
	}
	f.EnvVars = vars
	return f
}

// keptFrontendURL reports an env var carrying a FrontendURL reference the
// render could not resolve.
func keptFrontendURL(ev KCLEnvVar) bool { return ev.FrontendURL != nil && ev.Value == "" }

// frontendURLGap says why a FrontendURL resolved to nothing.
func frontendURLGap(err error, envName, target string) string {
	switch {
	case errors.Is(err, deploytarget.ErrHostedEnvironmentNotFound):
		return fmt.Sprintf("env %s has never been deployed, so frontend %s has no hostname yet", envName, target)
	case err != nil:
		return fmt.Sprintf("could not read env %s's hostnames from the control plane (%v)", envName, err)
	default:
		return fmt.Sprintf("frontend %s has no hostname in env %s yet", target, envName)
	}
}

// hostedFrontendURL is the public URL of hosted frontend name in ws: its first
// live custom domain, else the platform's URL, with basePath appended unless
// the URL already ends in it. "" when the frontend has neither.
func hostedFrontendURL(ws []deploytarget.HostedWorkloadStatus, name, basePath string) string {
	for _, w := range ws {
		if w.Name != name || deploytarget.HostedTier(w.Tier) != deploytarget.HostedTierStatic {
			continue
		}
		origin := ""
		for _, d := range w.Domains {
			if d.State == "live" && d.Domain != "" {
				origin = "https://" + d.Domain
				break
			}
		}
		if origin == "" {
			u, err := url.Parse(w.URL)
			if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
				return ""
			}
			origin = strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/")
		}
		base := strings.TrimRight(basePath, "/")
		if base != "" && !strings.HasSuffix(origin, base) {
			origin += base
		}
		return origin
	}
	return ""
}

// frontendBasePath is the declared base_path of frontend name, "" for none.
func frontendBasePath(e *KCLEntities, name string) string {
	if e == nil {
		return ""
	}
	for _, f := range e.Frontends {
		if f.Name == name {
			return f.BasePath
		}
	}
	return ""
}
