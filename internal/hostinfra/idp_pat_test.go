package hostinfra

import (
	"os"
	"path/filepath"
	"testing"
)

func withIDPHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	prev := idpHomeDir
	idpHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { idpHomeDir = prev })
	return home
}

// Zitadel writes the bootstrap PAT once, when it initializes its database, so
// the PAT belongs to the database: at the default idp_pat_path it lives in a
// per-machine file keyed by that database, which every checkout whose IdP
// uses it finds. A declared idp_pat_path is the author's and stays the only
// location.
func TestIDPPATLocations(t *testing.T) {
	home := withIDPHome(t)
	project := filepath.Join(t.TempDir(), "clone")
	checkout := filepath.Join(project, ".forge", "idp", "pat.txt")
	shared := filepath.Join(home, ".forge", "idp", "localhost_5434_zitadel_idp", "pat.txt")

	for _, tc := range []struct {
		name         string
		spec         Spec
		wantShared   string
		wantCheckout string
	}{
		{"unset path", Spec{IDPDatabase: "zitadel_idp", IDPDatabasePort: 5434}, shared, checkout},
		{"the schema default", Spec{IDPPATPath: ".forge/idp/pat.txt", IDPDatabase: "zitadel_idp", IDPDatabasePort: 5434}, shared, checkout},
		{"a declared path is the only one", Spec{IDPPATPath: "secrets/pat.txt", IDPDatabase: "zitadel_idp", IDPDatabasePort: 5434}, "", filepath.Join(project, "secrets", "pat.txt")},
		{"no database port: no identity to key by", Spec{IDPDatabase: "zitadel_idp"}, "", checkout},
		{"unsafe characters in the database name", Spec{IDPDatabase: "a/b c", IDPDatabasePort: 1}, filepath.Join(home, ".forge", "idp", "localhost_1_a_b_c", "pat.txt"), checkout},
	} {
		got := tc.spec.IDPPATLocations(project)
		if got.Shared != tc.wantShared || got.Checkout != tc.wantCheckout {
			t.Errorf("%s: got %+v, want shared %q checkout %q", tc.name, got, tc.wantShared, tc.wantCheckout)
		}
		want := tc.wantShared
		if want == "" {
			want = tc.wantCheckout
		}
		if got.WritePath() != want {
			t.Errorf("%s: WritePath() = %q, want %q (Zitadel writes beside the database when it can)", tc.name, got.WritePath(), want)
		}
	}
}

func TestIDPPATLocations_FindPublishMirror(t *testing.T) {
	dir := t.TempDir()
	loc := IDPPATLocations{Shared: filepath.Join(dir, "home", "pat.txt"), Checkout: filepath.Join(dir, "clone", "pat.txt")}
	write := func(p, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(p string) string {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	if got := loc.Find(); got != "" {
		t.Fatalf("Find() with neither file = %q, want empty", got)
	}
	if err := loc.MirrorToCheckout(); err != nil {
		t.Fatalf("mirroring a missing shared file is a no-op: %v", err)
	}

	// An older forge's database: the token is only in the checkout.
	write(loc.Checkout, "old-forge-token")
	if got := loc.Find(); got != loc.Checkout {
		t.Fatalf("Find() = %q, want the checkout fallback", got)
	}
	if err := loc.PublishShared(loc.Checkout); err != nil {
		t.Fatal(err)
	}
	if read(loc.Shared) != "old-forge-token" {
		t.Fatal("PublishShared did not copy the token")
	}
	if st, err := os.Stat(loc.Shared); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("the shared token must be owner-only: %v %v", st.Mode(), err)
	}

	// The shared file is what Zitadel writes: it wins, and the checkout copy follows it.
	write(loc.Shared, "current-token")
	if got := loc.Find(); got != loc.Shared {
		t.Fatalf("Find() = %q, want the shared file", got)
	}
	if err := loc.MirrorToCheckout(); err != nil {
		t.Fatal(err)
	}
	if read(loc.Checkout) != "current-token" {
		t.Error("MirrorToCheckout did not refresh the checkout copy")
	}

	// An empty file is not a token.
	write(loc.Shared, "")
	if got := loc.Find(); got != loc.Checkout {
		t.Errorf("Find() with an empty shared file = %q, want the checkout copy", got)
	}
}
