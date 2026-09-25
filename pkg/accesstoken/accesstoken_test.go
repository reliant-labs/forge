package accesstoken

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMint_ProducesTheExactFormat(t *testing.T) {
	for i := 0; i < 100; i++ {
		m, err := Mint()
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if !HasFormat(m.Plaintext) {
			t.Fatalf("minted token %q fails HasFormat", m.DisplayPrefix)
		}
		if m.Hash != Hash(m.Plaintext) || !EqualHash(m.Hash, Hash(m.Plaintext)) {
			t.Fatal("Minted.Hash is not Hash(Plaintext)")
		}
		if !strings.HasPrefix(m.Plaintext, m.DisplayPrefix) || len(m.DisplayPrefix) != len(Prefix)+DisplayPrefixLen {
			t.Fatalf("display prefix %q is not the token's leading %d chars", m.DisplayPrefix, len(Prefix)+DisplayPrefixLen)
		}
	}
}

// TestHasFormat_RejectsEveryRetiredFamily pins that `rlat_` is disjoint from
// every credential family it replaced, and from JWTs. Each was a real minted
// shape; none may pass the pre-filter.
func TestHasFormat_RejectsEveryRetiredFamily(t *testing.T) {
	foreign := map[string]string{
		"daemon/api PAT (rlnt_pat_)":     "rlnt_pat_" + strings.Repeat("A1b2C3", 5),
		"connector credential":           "rlnt_conn_" + strings.Repeat("Z9y8X7", 5),
		"per-user LLM key (base64url)":   "rlnt_" + strings.Repeat("a-_B", 10) + "abc",
		"managed LLM key (hex)":          "rlnt_" + strings.Repeat("0a1b2c3d", 8),
		"legacy rly_ key":                "rly_" + strings.Repeat("0a1b2c3d", 8),
		"port-access token (dpat_)":      "dpat_123e4567-e89b-12d3-a456-426614174000",
		"retired deploy token (rlntci_)": "rlntci_" + strings.Repeat("c", 43),
		"JWT":                            "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.sig",
		"truncated rlat_":                Prefix + strings.Repeat("a", randomBytes-1),
		"padded rlat_":                   Prefix + strings.Repeat("a", randomBytes+1),
		"rlat_ with non-base62 body":     Prefix + strings.Repeat("a", randomBytes-1) + "-",
	}
	for name, tok := range foreign {
		if HasFormat(tok) {
			t.Errorf("HasFormat accepted a %s", name)
		}
	}
}

func TestEqualHash(t *testing.T) {
	a := Hash("x")
	if !EqualHash(a, Hash("x")) || EqualHash(a, Hash("y")) || EqualHash(a, a[:10]) {
		t.Fatal("EqualHash is wrong")
	}
}

func TestNewSet_RefusesUnknownAndEmpty(t *testing.T) {
	if _, err := NewSet(nil); err == nil {
		t.Fatal("empty scope set accepted")
	}
	for _, bad := range []string{"deploy", "deploy:wrIte", "billing:write", "*", "secret:list"} {
		if _, err := NewSet([]string{string(ScopeDeployRead), bad}); err == nil {
			t.Errorf("NewSet accepted unknown scope %q", bad)
		}
	}
	for _, good := range AllScopes {
		if _, err := NewSet([]string{string(good)}); err != nil {
			t.Errorf("NewSet refused known scope %q: %v", good, err)
		}
	}
}

func TestPermits_WriteImpliesReadWithinAProductOnly(t *testing.T) {
	s := SetOf(ScopeDeployWrite, ScopeSecretWrite)
	if !s.Permits(ScopeDeployRead) || !s.Permits(ScopeSecretRead) {
		t.Fatal("write must imply read within its product")
	}
	if s.Permits(ScopeTokenRead) || SetOf(ScopeDeployRead).Permits(ScopeDeployWrite) {
		t.Fatal("an implication crossed products or read implied write")
	}
}

// TestCovers_IsTheNoMintBeyondYourScopesRule covers every scope, including the
// new ones: a holder can grant exactly what it holds.
func TestCovers_IsTheNoMintBeyondYourScopesRule(t *testing.T) {
	for _, held := range AllScopes {
		grantor := SetOf(ScopeTokenWrite, held)
		if !grantor.Covers(SetOf(held)) {
			t.Errorf("a holder of %s cannot grant %s", held, held)
		}
		for _, other := range AllScopes {
			if other == held || other == ScopeTokenWrite {
				continue
			}
			if grantor.Covers(SetOf(other)) {
				t.Errorf("a holder of {token:write, %s} was allowed to grant %s", held, other)
			}
		}
	}
	// Covers uses membership, not Permits: write does not grant read.
	if SetOf(ScopeDeployWrite).Covers(SetOf(ScopeDeployRead)) {
		t.Fatal("Covers routed through Permits")
	}
	if SetOf(ScopeDeployRead).Covers(Set{}) {
		t.Fatal("the empty request was covered")
	}
}

func validGrant(now time.Time) Grant {
	exp := now.Add(time.Hour)
	return Grant{
		OrgID:        "org",
		Name:         "laptop daemon",
		Scopes:       SetOf(ScopeDaemonConnect),
		ActingUserID: "user",
		Resource:     &Resource{Kind: ResourceDaemon, ID: "d1"},
		Ephemeral:    true,
		ExpiresAt:    &exp,
	}
}

func TestGrant_Validate(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Minute)

	if err := validGrant(now).Validate(now); err != nil {
		t.Fatalf("a valid bound, ephemeral, expiring daemon grant was refused: %v", err)
	}

	cases := map[string]struct {
		mutate func(*Grant)
		want   error
	}{
		"no org":                         {func(g *Grant) { g.OrgID = "" }, ErrInvalidGrant},
		"no name":                        {func(g *Grant) { g.Name = " " }, ErrInvalidGrant},
		"no scopes":                      {func(g *Grant) { g.Scopes = Set{} }, ErrInvalidGrant},
		"user scope with no acting user": {func(g *Grant) { g.ActingUserID = "" }, ErrInvalidGrant},
		"ephemeral unbound":              {func(g *Grant) { g.Resource = nil }, ErrInvalidGrant},
		"ephemeral never expires":        {func(g *Grant) { g.ExpiresAt = nil }, ErrInvalidGrant},
		"expiry in the past":             {func(g *Grant) { g.ExpiresAt = &past }, ErrInvalidGrant},
		"wrong binding kind": {func(g *Grant) {
			g.Resource = &Resource{Kind: ResourcePort, ID: PortResourceID("d1", 80)}
		}, ErrInvalidGrant},
		"unknown binding kind": {func(g *Grant) { g.Resource = &Resource{Kind: "bucket", ID: "b"} }, ErrInvalidGrant},
		"binding with no id":   {func(g *Grant) { g.Resource = &Resource{Kind: ResourceDaemon} }, ErrInvalidGrant},
		"unbindable scope bound": {func(g *Grant) {
			g.Scopes = SetOf(ScopeDeployRead)
		}, ErrInvalidGrant},
		"port scope unbound": {func(g *Grant) {
			g.Scopes, g.Resource, g.Ephemeral = SetOf(ScopeProxyPort), nil, false
		}, ErrInvalidGrant},
		"connector scope unbound": {func(g *Grant) {
			g.Scopes, g.Resource, g.Ephemeral = SetOf(ScopeMCPConnector), nil, false
		}, ErrInvalidGrant},
		"grantor escalates": {func(g *Grant) {
			g.GrantorScopes = SetOf(ScopeTokenWrite, ScopeLLMInvoke)
		}, ErrEscalation},
		// "A PAT cannot mint a PAT" is this rule: a daemon credential without
		// token:write cannot grant its own scope, because Covers requires
		// holding every requested scope and the handler requires token:write.
		"grantor without the scope": {func(g *Grant) {
			g.GrantorScopes = SetOf(ScopeLLMInvoke)
		}, ErrEscalation},
	}
	for name, tc := range cases {
		g := validGrant(now)
		tc.mutate(&g)
		err := g.Validate(now)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}

	// Positive rows for each new scope's legitimate shape.
	ok := map[string]Grant{
		"org CI token, no user, unbound": {OrgID: "o", Name: "ci", Scopes: SetOf(ScopeDeployWrite, ScopeSecretWrite)},
		"LLM key acting as a user":       {OrgID: "o", Name: "laptop", Scopes: SetOf(ScopeLLMInvoke), ActingUserID: "u"},
		"CLI token":                      {OrgID: "o", Name: "cli", Scopes: SetOf(ScopeReliantAPI), ActingUserID: "u"},
		"unbound daemon registration":    {OrgID: "o", Name: "reg", Scopes: SetOf(ScopeDaemonConnect), ActingUserID: "u"},
		"port share link": {OrgID: "o", Name: "share", Scopes: SetOf(ScopeProxyPort),
			Resource: &Resource{Kind: ResourcePort, ID: PortResourceID("d", 3000)}},
		"connector credential": {OrgID: "o", Name: "claude", Scopes: SetOf(ScopeMCPConnector), ActingUserID: "u",
			Resource: &Resource{Kind: ResourceConnector, ID: "g1"}},
		"grantor narrows": {OrgID: "o", Name: "n", Scopes: SetOf(ScopeDeployRead),
			GrantorScopes: SetOf(ScopeTokenWrite, ScopeDeployRead, ScopeDeployWrite)},
	}
	for name, g := range ok {
		if err := g.Validate(now); err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
	}
}

func TestPrincipal_MayActOn(t *testing.T) {
	bound := &Principal{Resource: &Resource{Kind: ResourceDaemon, ID: "d1"}}
	if !bound.MayActOn(ResourceDaemon, "d1") || bound.MayActOn(ResourceDaemon, "d2") || bound.MayActOn(ResourcePort, "d1") {
		t.Fatal("a bound principal escaped its binding")
	}
	if !(&Principal{}).MayActOn(ResourceDaemon, "any") {
		t.Fatal("an unbound principal was confined")
	}
	var nilP *Principal
	if nilP.MayActOn(ResourceDaemon, "d1") {
		t.Fatal("nil principal authorized")
	}
}
