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
			var warnings []string
			u, warnings = hostedFrontendURL(st.Workloads, target, frontendBasePath(e, target))
			for _, msg := range warnings {
				_, _ = fmt.Fprintf(w, "[build] %s: ⚠️  %s: %s\n", f.Name, ev.Name, msg)
			}
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
// live custom domain that is a valid bare hostname, else the platform's URL,
// joined with basePath. It returns "" when the frontend has neither, and a
// warning for each value it had to skip or trim, which the build prints.
//
// Both are values another system wrote, and the result becomes the site's
// canonical origin: a wrong one hands the site's ranking to another URL. So
// every URL is built through url.URL, never by concatenating strings.
func hostedFrontendURL(ws []deploytarget.HostedWorkloadStatus, name, basePath string) (string, []string) {
	var warnings []string
	for _, w := range ws {
		if w.Name != name || deploytarget.HostedTier(w.Tier) != deploytarget.HostedTierStatic {
			continue
		}
		var site *url.URL
		for _, d := range w.Domains {
			if d.State != "live" || d.Domain == "" {
				continue
			}
			if !isBareHostname(d.Domain) {
				warnings = append(warnings, fmt.Sprintf("custom domain %q is not a bare hostname; using the next live domain or the platform URL instead", d.Domain))
				continue
			}
			site = &url.URL{Scheme: "https", Host: strings.ToLower(d.Domain)}
			break
		}
		if site == nil {
			u, err := url.Parse(w.URL)
			if err != nil || u.Host == "" || u.User != nil || u.Opaque != "" || (u.Scheme != "https" && u.Scheme != "http") {
				if w.URL != "" {
					warnings = append(warnings, fmt.Sprintf("platform URL %q is not an absolute http(s) URL; leaving the origin empty", w.URL))
				}
				return "", warnings
			}
			if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
				warnings = append(warnings, fmt.Sprintf("platform URL %q carries a query or fragment, which a site origin cannot; dropped it", w.URL))
			}
			site = &url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}
		}
		return joinBasePath(site, basePath).String(), warnings
	}
	return "", warnings
}

// joinBasePath appends the frontend's base_path to site's path, segment by
// segment, unless site's path already ends with those segments (a platform
// URL that names the mount). The result has no trailing slash.
func joinBasePath(site *url.URL, basePath string) *url.URL {
	segments := func(p string) []string {
		var out []string
		for _, s := range strings.Split(p, "/") {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	have, base := segments(site.Path), segments(basePath)
	if len(base) > 0 && (len(have) < len(base) || !slices.Equal(have[len(have)-len(base):], base)) {
		have = append(have, base...)
	}
	out := *site
	out.Path, out.RawPath = "", ""
	if len(have) > 0 {
		// Decoded segments; String() escapes them.
		out.Path = "/" + strings.Join(have, "/")
	}
	return &out
}

// isBareHostname reports whether s is a hostname alone, as a custom domain
// must be: no scheme, userinfo, port, path, query or fragment, and only
// DNS labels (letters, digits, hyphens; 1-63 characters, not starting or
// ending with a hyphen) separated by dots, with at least two labels.
func isBareHostname(s string) bool {
	if len(s) > 253 {
		return false
	}
	labels := strings.Split(s, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, c := range l {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
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
