package accesstoken

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func hostGrant(now time.Time, scopes ...Scope) Grant {
	exp := now.Add(90 * 24 * time.Hour)
	return Grant{OrgID: "host-org", Name: "host", Scopes: SetOf(scopes...), ExpiresAt: &exp}
}

func TestHostCredentialShape(t *testing.T) {
	now := time.Now()
	for _, scopes := range [][]Scope{
		{ScopeEndTenantsManage},
		{ScopeEndTenantsDelete},
		{ScopeConnectionsConnect},
		{ScopeEndTenantsManage, ScopeConnectionsConnect},
		HostCredentialScopes,
	} {
		if err := hostGrant(now, scopes...).Validate(now); err != nil {
			t.Errorf("host credential %v refused: %v", scopes, err)
		}
	}
	atCap := now.Add(MaxHostCredentialTTL)
	over := now.Add(MaxHostCredentialTTL + time.Second)
	past := now.Add(-time.Hour)
	cases := map[string]func(*Grant){
		"mixed with deploy":      func(g *Grant) { g.Scopes[ScopeDeployRead] = struct{}{} },
		"mixed with app:oauth":   func(g *Grant) { g.Scopes[ScopeAppOAuth] = struct{}{} },
		"mixed with mcp:oauth":   func(g *Grant) { g.Scopes[ScopeMCPOAuth] = struct{}{} },
		"mixed with events:read": func(g *Grant) { g.Scopes[ScopeEventsRead] = struct{}{} },
		"mixed with registry":    func(g *Grant) { g.Scopes[ScopeSourcemapsWrite] = struct{}{} },
		"acting user":            func(g *Grant) { g.ActingUserID = "u" },
		"daemon binding":         func(g *Grant) { g.Resource = &Resource{Kind: ResourceDaemon, ID: "d"} },
		"environment binding":    func(g *Grant) { g.Resource = &Resource{Kind: ResourceEnvironment, ID: "e"} },
		"client id":              func(g *Grant) { g.ClientID = "c" },
		"audience":               func(g *Grant) { g.Audience = "https://api.example.com" },
		"app grant":              func(g *Grant) { g.AppGrantID = "ag" },
		"ephemeral":              func(g *Grant) { g.Ephemeral = true; g.Resource = &Resource{Kind: ResourceDaemon, ID: "d"} },
		"no expiry":              func(g *Grant) { g.ExpiresAt = nil },
		"expiry in the past":     func(g *Grant) { g.ExpiresAt = &past },
		"expiry beyond the cap":  func(g *Grant) { g.ExpiresAt = &over },
	}
	for name, mutate := range cases {
		g := hostGrant(now, ScopeEndTenantsManage)
		mutate(&g)
		if err := g.Validate(now); !errors.Is(err, ErrInvalidGrant) {
			t.Errorf("%s: Validate = %v, want ErrInvalidGrant", name, err)
		}
	}
	g := hostGrant(now, ScopeEndTenantsManage)
	g.ExpiresAt = &atCap
	if err := g.Validate(now); err != nil {
		t.Errorf("an expiry exactly at the cap was refused: %v", err)
	}
	if MaxHostCredentialTTL != 365*24*time.Hour {
		t.Errorf("MaxHostCredentialTTL = %s", MaxHostCredentialTTL)
	}
}

// TestHostCredential_TwoLiveTokensCoexist: C1's overlapping rotation is the
// minter's; nothing here may stop two live host tokens of one org.
func TestHostCredential_TwoLiveTokensCoexist(t *testing.T) {
	now := time.Now()
	a, b := hostGrant(now, HostCredentialScopes...), hostGrant(now, HostCredentialScopes...)
	if a.Validate(now) != nil || b.Validate(now) != nil {
		t.Fatal("a host grant minted twice for one org was refused")
	}
}

func TestHostScopes_ClassAndFlags(t *testing.T) {
	for _, s := range HostCredentialScopes {
		if !IsHostCredentialScope(s) || ClassOf(s) != ClassHost {
			t.Errorf("%s is not classified host", s)
		}
		if !NeverOnDaemon(s) || Elevatable(s) || RequiresActingUser(s) || FederationOf(s) != FederationNone {
			t.Errorf("%s: must be never-on-daemon, not elevatable, not acting-user, not federatable", s)
		}
		if len(BundlesOf(s)) != 0 {
			t.Errorf("%s is in bundles %v", s, BundlesOf(s))
		}
		for _, protected := range []bool{false, true} {
			if Federatable(s, protected) || slices.Contains(FederatableScopes(protected), s) {
				t.Errorf("%s is federatable (protected=%v)", s, protected)
			}
		}
		for _, b := range AllBundles {
			if ExpandBundle(b).Has(s) {
				t.Errorf("ExpandBundle(%s) holds host scope %s", b, s)
			}
			if r, _ := ResolveStored([]string{BundleRef(b)}); r.Has(s) {
				t.Errorf("ResolveStored(%s) holds host scope %s", BundleRef(b), s)
			}
		}
	}
	if IsHostCredentialScope(ScopeAppOAuth) || IsHostCredentialScope(ScopeEventsRead) || IsHostCredentialScope(ScopeDeployWrite) {
		t.Error("IsHostCredentialScope accepted a non-host scope")
	}
	// events:read: inert, in no bundle, never on a federated identity.
	if len(BundlesOf(ScopeEventsRead)) != 0 || FederationOf(ScopeEventsRead) != FederationNone || Federatable(ScopeEventsRead, true) {
		t.Error("events:read must join no bundle and no federation")
	}
}

// TestWildcardNeverResolvesToWholeCredentialScopes: no wildcard shape —
// org automation, a person's, a daemon's — resolves to a host scope or
// app:oauth.
func TestWildcardNeverResolvesToWholeCredentialScopes(t *testing.T) {
	shapes := map[string]Grant{
		"org automation": {},
		"person":         {ActingUserID: "u"},
		"daemon":         {ActingUserID: "u", Resource: &Resource{Kind: ResourceDaemon, ID: "d"}},
		"environment":    {Resource: &Resource{Kind: ResourceEnvironment, ID: "e"}},
	}
	for name, g := range shapes {
		got := g.Admissible()
		for _, s := range append(slices.Clone(HostCredentialScopes), ScopeAppOAuth) {
			if got.Has(s) {
				t.Errorf("%s wildcard resolved to %s", name, s)
			}
		}
	}
}
