package hostinfra

// Where the dev IdP's bootstrap PAT lives.
//
// Zitadel writes the service account's personal access token exactly once:
// on FIRST-INSTANCE setup, when it initializes its database. Every later
// boot against that database (`start-from-init` is idempotent) finds the
// instance there and writes nothing. So the PAT belongs to the DATABASE, not
// to the checkout whose `forge env up` happened to initialize it.
//
// Writing it into that checkout's .forge/idp/pat.txt lost it whenever the
// database outlived or was shared beyond the checkout: a second clone, or a
// fresh one, whose IdP database lives on a postgres server another checkout
// runs (control-plane's compose postgres, for one). Its `env up` booted the
// IdP against the already-initialized database, no token was written, and
// sign-in failed with "read the personal access token from
// <this checkout>/.forge/idp/pat.txt: no such file or directory" while the
// token sat in another directory nothing pointed at.
//
// So a PAT declared at the default path is stored beside the infrastructure
// it belongs to: a per-machine file keyed by the database's identity
// (~/.forge/idp/<host>_<port>_<database>/pat.txt), the only place Zitadel is
// told to write it. The checkout path stays: it is refreshed from the shared
// copy for anything that reads it, and it is the fallback for a database an
// older forge initialized, whose PAT is still only there. A PAT read from
// that fallback is published to the shared file only once it has
// authenticated against this IdP (see PublishShared), so a token left over
// from some earlier database can never be handed to every other checkout.
//
// A project that declares its own idp_pat_path keeps exactly that path, for
// writing and reading: the declaration is the author's.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"

	"github.com/reliant-labs/forge/internal/atomicfile"
)

// defaultIDPPATPath is the HostInfra schema's idp_pat_path default.
var defaultIDPPATPath = filepath.Join(".forge", "idp", "pat.txt")

// idpHomeDir resolves the user's home for the shared PAT store. Swappable so
// a test never touches the real home directory.
var idpHomeDir = os.UserHomeDir

// IDPPATLocations are the places the dev IdP's bootstrap PAT is written to
// and looked for.
type IDPPATLocations struct {
	// Shared is the per-machine file keyed by the IdP database's identity.
	// Empty when the spec declares its own idp_pat_path, names no database
	// or port, or there is no home directory.
	Shared string
	// Checkout is the declared idp_pat_path resolved against the project.
	Checkout string
}

// IDPPATLocations resolves where this spec's PAT is written and looked for.
func (s Spec) IDPPATLocations(projectDir string) IDPPATLocations {
	return IDPPATLocations{Shared: s.sharedPATPath(), Checkout: s.PATPath(projectDir)}
}

// sharedPATPath is ~/.forge/idp/<host>_<port>_<database>/pat.txt for a spec
// that declares the default idp_pat_path, else "".
func (s Spec) sharedPATPath() string {
	if s.IDPPATPath != "" && filepath.Clean(s.IDPPATPath) != defaultIDPPATPath {
		return ""
	}
	if s.IDPDatabase == "" || s.IDPDatabasePort <= 0 {
		return ""
	}
	home, err := idpHomeDir()
	if err != nil || home == "" {
		return ""
	}
	// The host is the one zitadelEnv dials: localhost.
	key := "localhost_" + strconv.Itoa(s.IDPDatabasePort) + "_" + unsafePathChars.ReplaceAllString(s.IDPDatabase, "_")
	return filepath.Join(home, ".forge", "idp", key, "pat.txt")
}

var unsafePathChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// WritePath is where Zitadel is told to write the PAT on first-instance
// setup (ZITADEL_FIRSTINSTANCE_PATPATH).
func (l IDPPATLocations) WritePath() string {
	if l.Shared != "" {
		return l.Shared
	}
	return l.Checkout
}

// Find returns the PAT to authenticate with: the shared file when it exists,
// else the checkout's, else "". The shared file wins because it is the one
// Zitadel writes, so it is never older than the database it is keyed by.
func (l IDPPATLocations) Find() string {
	for _, p := range []string{l.Shared, l.Checkout} {
		if p == "" {
			continue
		}
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Size() > 0 {
			return p
		}
	}
	return ""
}

// Searched lists the paths Find looks at, for a message naming them.
func (l IDPPATLocations) Searched() []string {
	var out []string
	for _, p := range []string{l.Shared, l.Checkout} {
		if p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// PublishShared copies the PAT at src into the shared file, so every other
// checkout whose IdP uses this database finds it. Call it only with a PAT
// that has just authenticated against this IdP: the shared file is trusted
// by every checkout. A no-op when there is no shared location, or src is
// already it.
func (l IDPPATLocations) PublishShared(src string) error {
	if l.Shared == "" || src == "" || filepath.Clean(src) == filepath.Clean(l.Shared) {
		return nil
	}
	return copyPAT(src, l.Shared)
}

// MirrorToCheckout refreshes the checkout's copy from the shared file, so a
// tool or script that reads the declared .forge/idp/pat.txt keeps finding
// the current token. A no-op when there is no shared file.
func (l IDPPATLocations) MirrorToCheckout() error {
	if l.Shared == "" || l.Checkout == "" || filepath.Clean(l.Shared) == filepath.Clean(l.Checkout) {
		return nil
	}
	if _, err := os.Stat(l.Shared); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return copyPAT(l.Shared, l.Checkout)
}

// copyPAT writes src's content to dst (owner-only, atomically) unless dst
// already holds it.
func copyPAT(src, dst string) error {
	b, err := os.ReadFile(src) // #nosec G304 -- forge's own PAT locations
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	if cur, err := os.ReadFile(dst); err == nil && string(cur) == string(b) { // #nosec G304 -- forge's own PAT locations
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(dst), err)
	}
	if err := atomicfile.WriteFile(dst, b, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return nil
}
