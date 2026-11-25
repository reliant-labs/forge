package cli

// The credential-field check of an anonymous deploy, run on this machine.
//
// An unclaimed deploy's static site is scanned by the control plane before it
// goes live (control-plane internal/contentscan), and a page with a credential
// input field — a sign-in form — is refused until the deploy is claimed: an
// anonymous page on a preview domain has no business asking anyone for a
// password. That scan runs on the UPLOAD, so a project whose frontend carries
// sign-in pages (forge's own native auth scaffold does) learned it only after
// a two-minute build, from a message that named no check (new.txt §1 #3).
//
// This is the same check on the frontend's SOURCE, before anything is built,
// in `env deploy --claimable` and in its `--plan-only` preview. It is a SUBSET
// of the platform's, on purpose: this file is open source and the scanner is
// not, so it holds only the unambiguous shapes an honest sign-in form always
// has — an input of type password, or a standard autocomplete token that names
// a credential (WHATWG autofill: current-password, new-password,
// one-time-code, cc-number, cc-csc, cc-exp). Whatever it flags the platform
// refuses too; control-plane's claim_required_test.go pins the same shapes
// (forgeLocalCredentialFields), so a drift fails one side's tests. Whatever it
// misses the platform still catches, after the build, and then forge prints
// the claim link (claimContentRefusal).

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/cloud"
)

var (
	// An input of type password, as HTML, JSX or a bundle writes it:
	// type="password", type: "password", setAttribute("type", "password").
	// The platform's scan reads the same shapes from the built bundle.
	credentialPasswordTypeRe = regexp.MustCompile(`(?i)(?:\btype["']?\s*[:=]\s*\{?\s*["'\x60]password["'\x60]|setAttribute\(\s*["']type["']\s*,\s*["']password["'])`)
	// A field whose autocomplete token names a credential.
	credentialAutocompleteRe = regexp.MustCompile(`(?i)\bautocomplete["']?\s*[:=]\s*\{?\s*["'\x60][^"'\x60]{0,80}?\b(current-password|new-password|one-time-code|cc-number|cc-csc|cc-exp(?:-month|-year)?)\b`)
)

// credentialScanExts are the files a frontend's pages are written in.
var credentialScanExts = map[string]bool{
	".html": true, ".htm": true, ".js": true, ".jsx": true, ".ts": true, ".tsx": true,
	".mjs": true, ".cjs": true, ".mts": true, ".cts": true, ".vue": true, ".svelte": true,
	".astro": true, ".mdx": true,
}

// credentialScanSkipDirs are directories whose files never reach the site, or
// are a previous build's output (which the deploy rebuilds anyway).
var credentialScanSkipDirs = map[string]bool{
	"node_modules": true, "out": true, "dist": true, "build": true, "coverage": true,
	"__tests__": true, "__mocks__": true, "e2e": true, "tests": true, "test": true,
	"cypress": true, "playwright": true, "storybook-static": true, "gen": true,
}

const (
	// credentialScanMaxFileBytes bounds one file: a page is small, and a
	// larger file is a vendored bundle or data, not a form someone wrote.
	credentialScanMaxFileBytes = 2 << 20
	// credentialScanMaxFiles bounds one frontend's walk.
	credentialScanMaxFiles = 20000
)

// credentialField is one credential input field found in a frontend's source.
type credentialField struct {
	Frontend string `json:"frontend"`
	// Path is relative to the project directory, slash-separated.
	Path string `json:"path"`
	Line int    `json:"line"`
	// What the field collects, as the author reads it: "a password field",
	// "an autocomplete=one-time-code field".
	What string `json:"what"`
}

func (f credentialField) String() string {
	return fmt.Sprintf("%s:%d  %s (frontend %q)", f.Path, f.Line, f.What, f.Frontend)
}

// frontendCredentialFields scans the source of every frontend the env serves
// from hosted static hosting. A frontend whose code lives in another
// repository (no directory here) is left to the platform's scan.
func frontendCredentialFields(projectDir string, e *KCLEntities) ([]credentialField, error) {
	if e == nil {
		return nil, nil
	}
	var out []credentialField
	for _, fe := range e.Frontends {
		if !frontendIsHosted(fe) || fe.Path == "" {
			continue
		}
		dir := resolveProjectPath(projectDir, fe.Path)
		found, err := scanCredentialFields(projectDir, dir, fe.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Line < out[j].Line
	})
	return out, nil
}

// scanCredentialFields walks one frontend's directory. A missing directory is
// no finding: the build reports it.
func scanCredentialFields(projectDir, dir, frontend string) ([]credentialField, error) {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var out []credentialField
	files := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				return nil
			}
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != dir && (strings.HasPrefix(name, ".") || credentialScanSkipDirs[name]) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !credentialScanExts[strings.ToLower(filepath.Ext(name))] || isTestSourceFile(name) {
			return nil
		}
		if files++; files > credentialScanMaxFiles {
			return filepath.SkipAll
		}
		info, err := d.Info()
		if err != nil || info.Size() > credentialScanMaxFileBytes {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, rerr := filepath.Rel(projectDir, path)
		if rerr != nil {
			rel = path
		}
		for _, f := range credentialFieldsIn(data) {
			f.Frontend, f.Path = frontend, filepath.ToSlash(rel)
			out = append(out, f)
		}
		return nil
	})
	return out, err
}

// isTestSourceFile reports a test, story or type-declaration file: it is in
// the source tree but never in the site.
func isTestSourceFile(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".d.ts") {
		return true
	}
	for _, marker := range []string{".test.", ".spec.", ".stories.", ".story."} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// credentialFieldsIn finds the credential fields in one file, one per line.
func credentialFieldsIn(data []byte) []credentialField {
	lower := bytes.ToLower(data)
	if !bytes.Contains(lower, []byte("password")) && !bytes.Contains(lower, []byte("autocomplete")) {
		return nil
	}
	var out []credentialField
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), credentialScanMaxFileBytes)
	for n := 1; sc.Scan(); n++ {
		line := sc.Bytes()
		switch {
		case credentialPasswordTypeRe.Match(line):
			out = append(out, credentialField{Line: n, What: "a password field"})
		default:
			if m := credentialAutocompleteRe.FindSubmatch(line); m != nil {
				out = append(out, credentialField{Line: n, What: fmt.Sprintf("an autocomplete=%s field", strings.ToLower(string(m[1])))})
			}
		}
	}
	return out
}

// credentialFieldsShown bounds how many fields a refusal lists.
const credentialFieldsShown = 8

// claimableCredentialFieldsRefusal is the local refusal: what was found,
// what claiming unlocks, and the ways forward, best first. claimURL is this
// machine's existing claimable deploy of the env, "" when it has none (then
// claiming means signing up, which `forge login` does).
func claimableCredentialFieldsRefusal(env string, fields []credentialField, claimURL string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "env %q can't be deployed anonymously: its frontend has pages with credential input fields "+
		"(sign-in or password forms). Nothing was built or created.\n", env)
	for i, f := range fields {
		if i == credentialFieldsShown {
			fmt.Fprintf(&b, "  … and %d more\n", len(fields)-i)
			break
		}
		fmt.Fprintf(&b, "  %s\n", f)
	}
	b.WriteString(claimToUnlockNote + "\n")
	next := claimRequiredNext(env)
	fmt.Fprintf(&b, "  or remove those pages and deploy anonymously: %s\n", next[1])
	b.WriteString("    (a project made with `project new --auth-mode none` has none)\n")
	if claimURL != "" {
		fmt.Fprintf(&b, "  Claim:  %s  (this machine's anonymous deploy of %s)\n", claimURL, env)
		fmt.Fprintf(&b, "  fix: give the claim link to your user; once claimed, they sign in on this machine and deploy: %s", next[0])
	} else {
		fmt.Fprintf(&b, "  fix: deploy it to your own account (free to sign up): %s", next[0])
	}
	return &claimableRefusal{
		exit: exitRefused, code: cloud.CodeFailedPrecondition, reason: cloud.ReasonClaimableContentRefused,
		msg: b.String(), next: next,
	}
}

// claimToUnlockNote is the one sentence that says what claiming unlocks, in
// every place a claim-required refusal is explained.
const claimToUnlockNote = "  Claim to unlock: pages with sign-in forms can't be served anonymously."

// claimRequiredNext are the commands past a claim-required refusal, best
// first: deploy as an account (after claiming, the claimer's), then deploy
// anonymously once the pages are gone.
func claimRequiredNext(env string) []string {
	return []string{loginThenDeploy(env), fmt.Sprintf("%s env deploy %s --claimable", Name(), env)}
}
