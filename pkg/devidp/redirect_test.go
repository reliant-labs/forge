package devidp

import (
	"path"
	"strings"
	"testing"
)

// matchesLikeZitadel approximates Zitadel's devMode glob matching (which
// uses github.com/bmatcuk/doublestar under zitadel/oidc's
// checkURIAgainstRedirects) closely enough for this package's own
// composition to be exercised: a single `*` matches any run of characters
// EXCEPT `/`, exactly [path.Match]'s semantics for '/'-separated patterns.
func matchesLikeZitadel(t *testing.T, pattern, uri string) bool {
	t.Helper()
	ok, err := path.Match(pattern, uri)
	if err != nil {
		t.Fatalf("bad pattern %q: %v", pattern, err)
	}
	return ok
}

func TestRedirectGlob_RootMounted(t *testing.T) {
	got := RedirectGlob("http://localhost:*", "", "/auth/callback")
	want := "http://localhost:*/auth/callback"
	if got != want {
		t.Fatalf("RedirectGlob = %q, want %q", got, want)
	}
}

// THE BUG. Before this fix, the pattern registered with Zitadel was always
// "http://localhost:*/auth/callback" — no base path — so it rejected the
// callback of any frontend mounted under a prefix, even though the origin
// and port were otherwise a perfect match. This reproduces that failure
// against the OLD (unfixed) composition to document exactly what broke,
// and pins the FIX against the new one.
func TestRedirectGlob_BasePathMatchesRealCallback(t *testing.T) {
	const realCallback = "http://localhost:3002/internal/auth/callback"

	oldPattern := "http://localhost:*/auth/callback" // pre-fix: no base path
	if matchesLikeZitadel(t, oldPattern, realCallback) {
		t.Fatalf("the pre-fix pattern %q unexpectedly matched a base-pathed callback %q; "+
			"the bug this test documents did not reproduce", oldPattern, realCallback)
	}

	fixed := RedirectGlob("http://localhost:*", "/internal", "/auth/callback")
	if !matchesLikeZitadel(t, fixed, realCallback) {
		t.Fatalf("fixed pattern %q does not match the real callback %q", fixed, realCallback)
	}
}

// The glob still spans ONLY the port — it must not swallow the base path
// segment boundary and start matching arbitrary paths.
func TestRedirectGlob_StillRejectsWrongPath(t *testing.T) {
	pattern := RedirectGlob("http://localhost:*", "/internal", "/auth/callback")

	cases := []struct {
		name string
		uri  string
		want bool
	}{
		{"same base path, different port", "http://localhost:9999/internal/auth/callback", true},
		{"wrong host", "http://evil.example.com/internal/auth/callback", false},
		{"wrong base path", "http://localhost:3002/admin/auth/callback", false},
		{"no base path at all", "http://localhost:3002/auth/callback", false},
		{"extra path segment smuggled after callback", "http://localhost:3002/internal/auth/callback/../../secret", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesLikeZitadel(t, pattern, tc.uri); got != tc.want {
				t.Errorf("match(%q, %q) = %v, want %v", pattern, tc.uri, got, tc.want)
			}
		})
	}
}

// A base_path with a trailing slash (which forge.yaml's own validation
// rejects, but a hand-edited config.k could still carry) must not produce
// a doubled slash ahead of the callback path.
func TestRedirectGlob_TrimsTrailingSlashOnBasePath(t *testing.T) {
	got := RedirectGlob("http://localhost:*", "/internal/", "/auth/callback")
	if strings.Contains(got, "//auth") {
		t.Fatalf("RedirectGlob produced a doubled slash: %q", got)
	}
	want := "http://localhost:*/internal/auth/callback"
	if got != want {
		t.Fatalf("RedirectGlob = %q, want %q", got, want)
	}
}
