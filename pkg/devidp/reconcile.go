package devidp

import (
	"context"
	"fmt"
)

// Spec is what one dev-IdP convergence run declares: the project and
// application names to register, and the redirect URI PATTERN
// (RedirectGlob) the SPA should be reachable at.
type Spec struct {
	// Project is the Zitadel project name. Its generated id becomes the
	// token audience.
	Project string
	// Frontend is the SPA application name, registered inside Project.
	Frontend string
	// RedirectURIs / PostLogoutURIs are the patterns (or literal URIs, for
	// a non-dev issuer) registered with the issuer. Build these with
	// RedirectGlob for the scaffolded dev IdP.
	RedirectURIs   []string
	PostLogoutURIs []string

	// LoginURI is the app's OWN sign-in route. Set it to run the API-only
	// sign-in flow: the issuer then redirects /oauth/v2/authorize INTO the
	// app instead of rendering its own login pages, and the app drives the
	// flow through CreateSession / FinalizeAuthRequest (see login.go).
	//
	// EMPTY (the default) keeps the issuer-hosted login UI, which is the
	// standard OIDC redirect flow and the right choice for a project that
	// does not want to own a credential form.
	//
	// Setting this is what makes the issuer mint v2 auth requests, which
	// FinalizeAuthRequest requires — so it is not cosmetic. Reconcile
	// converges it in BOTH directions: a project that stops setting it
	// goes back to the issuer-hosted UI rather than silently keeping the
	// last value someone configured.
	LoginURI string
}

// Reconcile converges one application against client and returns the
// Identity the run produced: the client id and audience Zitadel
// generated, paired with the issuer/JWKS URL browserOrigin implies.
//
// It ADOPTS an existing project/application rather than creating a second
// one (EnsureProject / EnsureSPAApplication both read before they write),
// which is what makes this idempotent — the property every consumer of
// this function's caller (a deploy-time Job, run on every deploy) depends
// on.
func Reconcile(ctx context.Context, client *Client, spec Spec, browserOrigin string) (Identity, error) {
	if err := client.WaitReady(ctx); err != nil {
		return Identity{}, fmt.Errorf("identity provider at %s is not answering: %w", client.Base, err)
	}

	org, err := client.DefaultOrg(ctx)
	if err != nil {
		return Identity{}, fmt.Errorf("resolve default organization: %w", err)
	}
	client.OrgID = org.ID

	project, err := client.EnsureProject(ctx, spec.Project)
	if err != nil {
		return Identity{}, fmt.Errorf("register project %q: %w", spec.Project, err)
	}

	app, err := client.EnsureSPAApplication(ctx, project.ID, spec.Frontend, spec.RedirectURIs, spec.PostLogoutURIs)
	if err != nil {
		return Identity{}, fmt.Errorf("register application %q: %w", spec.Frontend, err)
	}

	// Converge WHICH login UI the issuer sends users to. Declared state,
	// both ways: an empty LoginURI restores the issuer-hosted pages, so
	// this cannot drift into a configuration nobody chose.
	if err := client.SetLoginUI(ctx, spec.LoginURI); err != nil {
		return Identity{}, err
	}

	return Identity{
		ClientID: app.ClientID,
		Audience: project.ID,
		Issuer:   Issuer(browserOrigin),
		JWKSURL:  JWKSURL(browserOrigin),
	}, nil
}
