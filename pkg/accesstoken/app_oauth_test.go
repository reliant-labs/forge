package accesstoken

import (
	"errors"
	"testing"
	"time"
)

func appOAuthGrant() Grant {
	return Grant{OrgID: "o", Name: "app@oauth", Scopes: SetOf(ScopeAppOAuth), ActingUserID: "u",
		ClientID: "app_abc", Audience: "https://api.example.com/app", AppGrantID: "ag_1"}
}

func TestAppOAuth_Classification(t *testing.T) {
	if ClassOf(ScopeAppOAuth) != ClassPersonal {
		t.Errorf("app:oauth is class %q, want personal", ClassOf(ScopeAppOAuth))
	}
	if !IsOAuthClientScope(ScopeAppOAuth) || !RequiresActingUser(ScopeAppOAuth) {
		t.Error("app:oauth must be an OAuth client scope that acts as a person")
	}
	if len(BundlesOf(ScopeAppOAuth)) != 0 {
		t.Errorf("app:oauth is in bundles %v: it is a whole credential", BundlesOf(ScopeAppOAuth))
	}
}

func TestAppOAuthGrantShape(t *testing.T) {
	now := time.Now()
	if err := appOAuthGrant().Validate(now); err != nil {
		t.Fatalf("a well-formed app:oauth grant was refused: %v", err)
	}
	cases := map[string]func(*Grant){
		"no acting user":     func(g *Grant) { g.ActingUserID = "" },
		"no client":          func(g *Grant) { g.ClientID = "" },
		"no audience":        func(g *Grant) { g.Audience = "" },
		"relative audience":  func(g *Grant) { g.Audience = "/app" },
		"audience fragment":  func(g *Grant) { g.Audience = "https://api.example.com/app#x" },
		"no app grant":       func(g *Grant) { g.AppGrantID = "" },
		"blank app grant":    func(g *Grant) { g.AppGrantID = "  " },
		"second scope":       func(g *Grant) { g.Scopes = SetOf(ScopeAppOAuth, ScopeReliantAPI) },
		"with mcp:oauth":     func(g *Grant) { g.Scopes = SetOf(ScopeAppOAuth, ScopeMCPOAuth) },
		"with host scope":    func(g *Grant) { g.Scopes = SetOf(ScopeAppOAuth, ScopeEndTenantsManage) },
		"bound to daemon":    func(g *Grant) { g.Resource = &Resource{Kind: ResourceDaemon, ID: "d"} },
		"bound to env":       func(g *Grant) { g.Resource = &Resource{Kind: ResourceEnvironment, ID: "e"} },
		"ephemeral":          func(g *Grant) { g.Ephemeral = true },
		"escalating grantor": func(g *Grant) { g.GrantorScopes = SetOf(ScopeReliantAPI) },
	}
	for name, mutate := range cases {
		g := appOAuthGrant()
		mutate(&g)
		if err := g.Validate(now); err == nil {
			t.Errorf("%s: Validate accepted it", name)
		} else if name != "escalating grantor" && !errors.Is(err, ErrInvalidGrant) {
			t.Errorf("%s: Validate = %v, want ErrInvalidGrant", name, err)
		}
	}
}

// TestAppGrantID_OnlyOnAppOAuth is N3: the id is required on app:oauth and
// refused on every other shape.
func TestAppGrantID_OnlyOnAppOAuth(t *testing.T) {
	now := time.Now()
	soon := now.Add(time.Hour)
	shapes := map[string]Grant{
		"mcp:oauth":      mcpOAuthGrant(),
		"mcp:forge":      {OrgID: "o", Name: "n", Scopes: SetOf(ScopeMCPForge), ActingUserID: "u", ClientID: "c", Audience: "https://api.example.com/mcp"},
		"host":           {OrgID: "o", Name: "n", Scopes: SetOf(ScopeEndTenantsManage), ExpiresAt: &soon},
		"registry-only":  {OrgID: "o", Name: "n", Scopes: SetOf(ScopeSourcemapsWrite)},
		"plain personal": {OrgID: "o", Name: "n", Scopes: SetOf(ScopeReliantAPI), ActingUserID: "u"},
		"org automation": {OrgID: "o", Name: "n", Scopes: SetOf(ScopeDeployWrite)},
		"daemon":         {OrgID: "o", Name: "n", Scopes: SetOf(ScopeDaemonConnect), ActingUserID: "u", Resource: &Resource{Kind: ResourceDaemon, ID: "d"}},
	}
	for name, g := range shapes {
		if err := g.Validate(now); err != nil {
			t.Errorf("%s: baseline grant refused: %v", name, err)
			continue
		}
		g.AppGrantID = "ag_1"
		if err := g.Validate(now); !errors.Is(err, ErrInvalidGrant) {
			t.Errorf("%s with an app grant id: Validate = %v, want ErrInvalidGrant", name, err)
		}
	}
	g := appOAuthGrant()
	g.AppGrantID = ""
	if err := g.Validate(now); !errors.Is(err, ErrInvalidGrant) {
		t.Errorf("app:oauth without an app grant id: Validate = %v, want ErrInvalidGrant", err)
	}
}

func TestIsAppOAuthFor(t *testing.T) {
	g := appOAuthGrant()
	p := &Principal{Scopes: g.Scopes, ActingUserID: g.ActingUserID, ClientID: g.ClientID, Audience: g.Audience, AppGrantID: g.AppGrantID}
	if !p.IsAppOAuthFor(g.Audience) {
		t.Fatal("the token was refused for its own audience")
	}
	if p.IsMCPOAuthFor(g.Audience) || p.IsMCPForgeFor(g.Audience) {
		t.Error("an app:oauth token passed an MCP check")
	}
	mcp := *p
	mcp.Scopes = SetOf(ScopeMCPOAuth)
	if mcp.IsAppOAuthFor(g.Audience) {
		t.Error("an mcp:oauth token passed the app:oauth check")
	}
	noGrant := *p
	noGrant.AppGrantID = ""
	wild := *p
	wild.AllScopes = true
	bound := *p
	bound.Resource = &Resource{Kind: ResourceDaemon, ID: "d"}
	noUser := *p
	noUser.ActingUserID = ""
	extra := *p
	extra.Scopes = SetOf(ScopeAppOAuth, ScopeReliantAPI)
	for name, q := range map[string]*Principal{"no app grant": &noGrant, "wildcard": &wild, "bound": &bound, "no user": &noUser, "extra scope": &extra, "other resource": p, "nil": nil} {
		if q.IsAppOAuthFor("https://api.example.com/other") || (name != "other resource" && name != "nil" && q.IsAppOAuthFor(g.Audience)) {
			t.Errorf("%s: accepted", name)
		}
	}
	if p.IsAppOAuthFor("") {
		t.Error("accepted for an empty resource")
	}
}
