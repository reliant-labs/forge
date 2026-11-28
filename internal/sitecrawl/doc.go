// Package sitecrawl gathers the facts pkg/siteaudit judges, for `forge site
// audit`: it crawls a static site (a build directory served in-process, or a
// live URL fetched through pkg/netguard) and returns a [siteaudit.Input].
//
// It is the CLI's crawler, not the control plane's. The two share the rules
// (pkg/siteaudit) and the parsers the rules export (ParseRobots,
// ParseSitemap, DiscoverLinks, ExtractHead); the control plane's site auditor
// owns its own fetch policy (probe signing, a shared link cache, plan caps).
//
// What a crawl does, in order:
//
//   - fetches /robots.txt at the site's host and obeys it for
//     [UserAgentToken] (a group naming no agent of ours falls back to "*"),
//     so the audit sees what a well-behaved crawler sees; the site's root is
//     always fetched;
//   - fetches the sitemaps robots.txt names on the site's host, else
//     <base>/sitemap.xml (kept only when it answers), decompressing gzip and
//     following a sitemap index one level;
//   - walks the site breadth first from its root and the sitemap URLs,
//     staying on the site's host and under its base path, up to MaxPages
//     pages; every other internal target it saw (an asset, a page past the
//     cap) gets a status check, as does each page's og:image (which is also
//     tested for forge's placeholder image);
//   - checks external links only when asked (CheckExternal), capped;
//   - probes a path that cannot exist, for soft-404 detection;
//   - renders up to MaxRender pages with a [Renderer] when one is given.
//
// Redirects are never followed implicitly: a 3xx is recorded as the page's
// status, and a same-site Location is crawled as a page of its own.
//
// [DirHandler] serves a build directory the way Reliant hosted static
// hosting does (the control plane's staticorigin): a 404.html declares real
// 404s, else a route-shaped path falls back to the mount's index.html with
// 200; /x is x, x.html or x/index.html; /x/ redirects to /x when only x.html
// exists. The site's _headers and _redirects are not applied.
package sitecrawl
