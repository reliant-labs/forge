package devidp

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The frontend-elsewhere recipe (skill frontend/runtime-config): a frontend at
// https://app.example.com signs in through an API at https://api.example.com.
// The browser sends that POST as same-site, which the guard refuses unless the
// frontend's origin is the trusted app origin, so the recipe sets idp_login_uri
// (IdentityApplication.login_uri) to it. Without it, sign-in is a 403.
func TestCrossOriginProtectionAdmitsTheDeclaredExternalFrontend(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	post := func(appBase string) int {
		req := httptest.NewRequest(http.MethodPost, "https://api.example.com/auth/login", nil)
		req.Header.Set("Sec-Fetch-Site", "same-site")
		req.Header.Set("Origin", "https://app.example.com")
		rec := httptest.NewRecorder()
		NewCrossOriginProtection(appBase).Handler(ok).ServeHTTP(rec, req)
		return rec.Code
	}
	if got := post("https://app.example.com/auth/sign-in"); got != http.StatusNoContent {
		t.Fatalf("sign-in from the declared frontend origin = %d, want it admitted", got)
	}
	if got := post("https://api.example.com/auth/sign-in"); got != http.StatusForbidden {
		t.Fatalf("sign-in from an undeclared sibling origin = %d, want 403", got)
	}
}
