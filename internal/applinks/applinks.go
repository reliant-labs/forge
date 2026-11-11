// Package applinks renders the two association files that bind a mobile app to
// the https hosts of its universal links / App Links:
//
//	/.well-known/apple-app-site-association   (iOS, Associated Domains)
//	/.well-known/assetlinks.json              (Android, Digital Asset Links)
//
// The inputs are facts about the BINARY (its team, bundle id, package and the
// certificates that sign it), so the files are per environment even though a
// promoted site release is byte-identical: they are rendered at deploy, never
// baked into the build. See docs/proposals/mobile-native-auth/universal-links.md.
//
// Both files must be served at the host root over https with a valid
// certificate, as `Content-Type: application/json`, and with NO redirect.
package applinks

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strings"
)

// Paths of the two files under the site root, and the one content type both
// must be served with. The AASA has no extension, so a static host cannot infer
// it: the deploy target has to set it (see the deploy/managed-identity/mobile
// skill).
const (
	AASAPath         = ".well-known/apple-app-site-association"
	AssetLinksPath   = ".well-known/assetlinks.json"
	ContentType      = "application/json"
	maxAASABytes     = 128 << 10
	debugFingerprint = "debug"
)

// Callback paths the apps claim: the sign-in redirect and the post-logout
// redirect, the same under every scheme.
var callbackPaths = []string{"/oauthredirect", "/signedout"}

// App is one mobile app associated with a host.
type App struct {
	// Frontend is the forge frontend's name, for messages.
	Frontend string
	// TeamID and BundleID make the iOS app id `<TeamID>.<BundleID>`. Empty
	// TeamID or BundleID: the app has no iOS side.
	TeamID   string
	BundleID string
	// Package and Fingerprints make the Android entry. Fingerprints are the
	// SHA-256 colon-hex fingerprints of every certificate that signs an
	// installable build of the env. Empty Package or none: no Android side.
	Package      string
	Fingerprints []string
}

// IOS reports whether the app has an iOS association.
func (a App) IOS() bool { return a.TeamID != "" && a.BundleID != "" }

// Android reports whether the app has an Android association.
func (a App) Android() bool { return a.Package != "" && len(a.Fingerprints) > 0 }

// AppID is the Apple app id the AASA lists.
func (a App) AppID() string { return a.TeamID + "." + a.BundleID }

// Files are the rendered association files, keyed by site-relative path
// ([AASAPath], [AssetLinksPath]). A file no app contributes to is absent.
type Files map[string][]byte

// Render renders the files for apps, which must all be associated with the
// SAME host set (one site). Order of apps does not matter: the output is
// sorted and de-duplicated, so a re-render is byte-identical.
func Render(apps []App) (Files, error) {
	files := Files{}
	if aasa, err := RenderAASA(apps); err != nil {
		return nil, err
	} else if aasa != nil {
		files[AASAPath] = aasa
	}
	if links, err := RenderAssetLinks(apps); err != nil {
		return nil, err
	} else if links != nil {
		files[AssetLinksPath] = links
	}
	return files, nil
}

type aasaComponent struct {
	Path string `json:"/"`
}

type aasaDetail struct {
	AppIDs     []string        `json:"appIDs"`
	Components []aasaComponent `json:"components"`
}

type aasaDoc struct {
	AppLinks       aasaApplinks       `json:"applinks"`
	WebCredentials aasaWebCredentials `json:"webcredentials"`
}

type aasaApplinks struct {
	Details []aasaDetail `json:"details"`
}

type aasaWebCredentials struct {
	Apps []string `json:"apps"`
}

// RenderAASA renders the apple-app-site-association file, or nil when no app
// has an iOS side. `applinks` makes the callback a universal link outside the
// auth session too (a cold start, a magic link); `webcredentials` is what the
// auth-session callback and password AutoFill are checked against. Declaring
// both keeps either reading of Apple's requirement satisfied.
func RenderAASA(apps []App) ([]byte, error) {
	var ids []string
	for _, a := range apps {
		if a.IOS() {
			ids = append(ids, a.AppID())
		}
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	comps := make([]aasaComponent, len(callbackPaths))
	for i, p := range callbackPaths {
		comps[i] = aasaComponent{Path: p}
	}
	body, err := marshal(aasaDoc{
		AppLinks:       aasaApplinks{Details: []aasaDetail{{AppIDs: ids, Components: comps}}},
		WebCredentials: aasaWebCredentials{Apps: ids},
	})
	if err != nil {
		return nil, err
	}
	if len(body) > maxAASABytes {
		return nil, fmt.Errorf("the apple-app-site-association file is %d bytes; Apple accepts at most %d", len(body), maxAASABytes)
	}
	return body, nil
}

type assetTarget struct {
	Namespace    string   `json:"namespace"`
	PackageName  string   `json:"package_name"`
	Fingerprints []string `json:"sha256_cert_fingerprints"`
}

type assetStatement struct {
	Relation []string    `json:"relation"`
	Target   assetTarget `json:"target"`
}

// RenderAssetLinks renders assetlinks.json, or nil when no app has an Android
// side. One statement per package, listing every fingerprint declared for it.
func RenderAssetLinks(apps []App) ([]byte, error) {
	byPkg := map[string][]string{}
	for _, a := range apps {
		if !a.Android() {
			continue
		}
		for _, fp := range a.Fingerprints {
			n, err := NormalizeFingerprint(fp)
			if err != nil {
				return nil, fmt.Errorf("android package %s: %w", a.Package, err)
			}
			byPkg[a.Package] = append(byPkg[a.Package], n)
		}
	}
	if len(byPkg) == 0 {
		return nil, nil
	}
	pkgs := make([]string, 0, len(byPkg))
	for p := range byPkg {
		pkgs = append(pkgs, p)
	}
	slices.Sort(pkgs)
	stmts := make([]assetStatement, 0, len(pkgs))
	for _, p := range pkgs {
		fps := byPkg[p]
		slices.Sort(fps)
		stmts = append(stmts, assetStatement{
			Relation: []string{"delegate_permission/common.handle_all_urls"},
			Target:   assetTarget{Namespace: "android_app", PackageName: p, Fingerprints: slices.Compact(fps)},
		})
	}
	return marshal(stmts)
}

func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var fingerprintRE = regexp.MustCompile(`^([0-9A-Fa-f]{2}:){31}[0-9A-Fa-f]{2}$`)

// NormalizeFingerprint upper-cases a SHA-256 colon-hex certificate
// fingerprint, which is the form Android compares and keytool prints.
func NormalizeFingerprint(fp string) (string, error) {
	fp = strings.TrimSpace(fp)
	if !fingerprintRE.MatchString(fp) {
		return "", fmt.Errorf("%q is not a SHA-256 certificate fingerprint (32 hex bytes joined by ':')", fp)
	}
	return strings.ToUpper(fp), nil
}

// IsDebugFingerprint reports whether fp is the placeholder for this machine's
// debug keystore, to be resolved by [DebugKeystoreFingerprint].
func IsDebugFingerprint(fp string) bool { return fp == debugFingerprint }

// CommandRunner runs an external command and returns its stdout. It is how a
// test replaces `keytool`.
type CommandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner is the real [CommandRunner].
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

var keytoolSHA256RE = regexp.MustCompile(`SHA-?256:\s*((?:[0-9A-Fa-f]{2}:){31}[0-9A-Fa-f]{2})`)

// DebugKeystoreFingerprint is the SHA-256 fingerprint of the key a prebuild's
// debug build is signed with, read from the keystore the build actually uses
// (`android/app/debug.keystore`, per checkout) with the real `keytool`: forge
// does not re-implement keystore parsing. The standard debug credentials
// (alias androiddebugkey, password android) are the ones Expo and React
// Native generate.
func DebugKeystoreFingerprint(ctx context.Context, run CommandRunner, keystore string) (string, error) {
	out, err := run(ctx, "keytool", "-list", "-v", "-keystore", keystore,
		"-alias", "androiddebugkey", "-storepass", "android")
	if err != nil {
		return "", fmt.Errorf("read the debug keystore fingerprint: %w", err)
	}
	m := keytoolSHA256RE.FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("keytool printed no SHA-256 fingerprint for %s", keystore)
	}
	return strings.ToUpper(string(m[1])), nil
}

// SortApps orders apps by frontend name then package, so callers that gather
// them from maps render deterministically.
func SortApps(apps []App) {
	slices.SortFunc(apps, func(a, b App) int {
		return cmp.Or(cmp.Compare(a.Frontend, b.Frontend), cmp.Compare(a.BundleID, b.BundleID))
	})
}
