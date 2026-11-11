package release

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// https universal-link / App Link callbacks for a native application
// (docs/proposals/mobile-native-auth/universal-links.md).
//
// A native application may declare AssociatedDomains: the bare hostnames the
// app is associated with (Apple Associated Domains, Android App Links). Its
// callbacks are then https URLs on the first domain, tried BEFORE the
// private-use `<scheme>:/oauthredirect` pair, which stays registered as the
// fallback (iOS below 17.4, an unverified Android link, a domain-less build).

// The paths of the https callbacks, and of the association files. The callback
// paths are the same under every scheme, so the app's one
// `app/oauthredirect.tsx` route serves both forms. They are private on purpose:
// the private-use callback's own path constants belong to the native-app
// derivation, and this file only adds the https pair.
const (
	appLinkRedirectPath  = "/oauthredirect"
	appLinkSignedOutPath = "/signedout"
)

// associatedDomainRE is a bare, lowercase DNS hostname with at least two
// labels and an alphabetic top-level label. It deliberately refuses a scheme,
// port, path, wildcard and an IP literal: a wildcard cannot serve
// `webcredentials`, and an association file is fetched from the exact host.
var associatedDomainRE = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// ValidateAssociatedDomain checks one entry of
// [ShapeIdentityApplication.AssociatedDomains].
func ValidateAssociatedDomain(domain string) error {
	if len(domain) > 253 || !associatedDomainRE.MatchString(domain) {
		return fmt.Errorf("associated domain %q must be a bare lowercase hostname such as app.acme.example: no scheme, port, path, wildcard or IP address", domain)
	}
	return nil
}

// ValidateAssociatedDomains checks a native application's associated domains
// against its redirect URIs. An empty list is valid and means no association.
//
//   - every domain is a bare hostname, listed once;
//   - the host of the FIRST https redirect must be one of the domains, and so
//     must the host of every other https redirect, because an https callback
//     on a host the app is not associated with can never reach it;
//   - with domains set there must be at least one https redirect: the domains
//     would otherwise claim an association nothing signs in through.
//     redirectURIs may be empty only when the redirect is derived (see
//     [PrependAppLinkRedirect]), so callers pass the DECLARED list and check
//     only when it is non-empty; see [ValidateAssociatedDomainsDeclared].
func ValidateAssociatedDomains(domains, redirectURIs []string) error {
	if len(domains) == 0 {
		return nil
	}
	seen := map[string]bool{}
	for _, d := range domains {
		if err := ValidateAssociatedDomain(d); err != nil {
			return err
		}
		if seen[d] {
			return fmt.Errorf("associated domain %q is listed twice", d)
		}
		seen[d] = true
	}
	https := 0
	for _, raw := range redirectURIs {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" {
			continue
		}
		https++
		if !seen[strings.ToLower(u.Hostname())] {
			return fmt.Errorf("https redirect %q is on host %q, which is not in associated_domains %v: the app is associated only with those hosts", raw, u.Hostname(), domains)
		}
	}
	if len(redirectURIs) > 0 && https == 0 {
		return fmt.Errorf("associated_domains %v are set but no redirect_uris entry is an https URL on one of them: add https://%s%s, or omit redirect_uris to have it derived", domains, domains[0], appLinkRedirectPath)
	}
	return nil
}

// PrependAppLinkRedirect is the ONE place an https redirect is put ahead of a
// native application's private-use callbacks: the first associated domain's
// `https://<domain>/oauthredirect`, unless redirectURIs already carries it.
// It returns redirectURIs unchanged when there are no domains, and never
// reorders or drops an entry, so a declared list keeps its order.
//
// The IdP accepts every registered entry and the client picks by platform, so
// the private-use entry that follows is what an iOS below 17.4, an
// unverified Android link and a domain-less build sign in with.
func PrependAppLinkRedirect(domains, redirectURIs []string) []string {
	if len(domains) == 0 {
		return redirectURIs
	}
	link := "https://" + domains[0] + appLinkRedirectPath
	if slices.Contains(redirectURIs, link) {
		return redirectURIs
	}
	return append([]string{link}, redirectURIs...)
}
