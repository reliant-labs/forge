// Package toolfetch installs non-Go tools forge runs for itself — today syft
// (SBOM generation) and grype (vulnerability scanning) — as pinned upstream
// release binaries, each archive verified against a sha256 compiled into
// forge before a byte of it is extracted.
//
// # Using it
//
//	path, version, err := toolfetch.Ensure(ctx, toolfetch.Syft)
//	if err != nil {
//		// The tool is unavailable: whatever needed it did NOT run. Record
//		// "not scanned" / "no SBOM", never a clean result.
//	}
//	cmd := exec.CommandContext(ctx, path, "scan", ...)
//
// Ensure is idempotent and cheap after the first call: a verified entry in
// the cache is a hit with no network. The first call downloads the archive
// for this GOOS/GOARCH (tens of MB), prints one line to stderr saying so,
// verifies it and installs the binary. version is always the pinned release
// (templates.SyftVersion / templates.GrypeVersion), the string a scan record
// names as its scanner version.
//
// Find reports an installed tool without installing anything or touching the
// network (`forge doctor` uses it). MethodFor reports how this platform
// would get a tool: the verified release, `go install`, or not at all. A Fetcher is the same API with every input injectable — cache
// directory, download base URL, HTTP client, platform, the `go install`
// fallback — for tests and for callers that need their own log writer.
//
// # Errors
//
// Every error from Ensure means the tool is not usable. Two are classifiable
// with errors.Is:
//
//   - ErrUnavailable: this platform has no published binary forge has a
//     checksum for, and there is no Go toolchain to build it with. Retrying
//     will not help.
//   - ErrChecksum: the download did not hash to the pinned value. Nothing was
//     installed; the bytes were corrupted or substituted.
//
// Anything else (no network, an HTTP error, a full disk) is transient.
//
// # Where it lives
//
//	$FORGE_TOOLS_CACHE (default <UserCacheDir>/forge/tools)/release/
//	  <name>@<version>-<goos>-<goarch>/
//	    <name>[.exe]
//	    toolfetch.json        what was installed, from where, and its sha256
//
// It is an entry of forge's shared tool cache (internal/toolcache), so it is
// built in a private staging directory and published by one rename: a
// parallel forge sees no entry or a whole one, never a half-written binary.
// Two processes that miss at once both download, and the loser of the rename
// uses the winner's entry. Nothing is ever rewritten in place; a bump is a
// new entry beside the old.
//
// # Where it downloads from
//
// https://github.com/<repo>/releases/download/<version>/<asset>. FORGE_TOOLS_MIRROR
// replaces https://github.com with a mirror that serves the same paths (an
// Artifactory or Nexus remote of github.com, an internal proxy). A mirror
// cannot substitute bytes: the pinned sha256 is checked either way.
//
// # When there is no binary
//
// A platform the upstream release does not publish (grype has no
// windows/arm64 build) falls back to `go install <GoPackage>@<Version>`,
// which the Go checksum database verifies, when a Go toolchain is on PATH.
// Otherwise Ensure returns ErrUnavailable.
//
// # Per-project versions
//
// A Tool is a plain value, so a caller that wants another version builds its
// own Tool with that version AND that version's checksums. There is
// deliberately no way to name a version without its checksums: a version
// forge has no sha256 for cannot be verified, and this package never
// installs a release binary it did not verify. (forge.yaml has no such
// override yet; the seam is here for when it does.)
//
//forge:lint-disable-next-line forge-exclude-contract-outbound-io: the download is the package's whole job, and every input it reaches out with (BaseURL, Client, GoInstall, LookPath, CacheDir) is already an injectable Fetcher field the tests point at an httptest server, as internal/cigate's Resolver does; a contract.go Service would be one implementation with no substitution point, and a consumer that wants a fake declares its own one-method interface over Ensure
//forge:exclude-contract: a filesystem tool cache with package-level operations, not an injected application component
package toolfetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/atomicfile"
	"github.com/reliant-labs/forge/internal/goexec"
	"github.com/reliant-labs/forge/internal/toolcache"
)

// EnvMirror replaces https://github.com as the base of every release
// download. The mirror must serve GitHub's path layout:
// <mirror>/<owner>/<repo>/releases/download/<tag>/<asset>.
const EnvMirror = "FORGE_TOOLS_MIRROR"

// DefaultBaseURL is where release archives are downloaded from when
// FORGE_TOOLS_MIRROR is unset.
const DefaultBaseURL = "https://github.com"

// maxArchiveBytes bounds a download and an extracted binary. The pinned
// archives are ~30 MB and their binaries ~100 MB; a response that keeps
// going is not one of them.
const maxArchiveBytes = 1 << 30

// manifestName is the file in each entry recording what was installed.
const manifestName = "toolfetch.json"

var (
	// ErrUnavailable means the tool cannot be installed on this platform at
	// all: no published binary forge has a checksum for, and no Go toolchain.
	ErrUnavailable = errors.New("tool unavailable on this platform")
	// ErrChecksum means a downloaded archive did not match its pinned sha256.
	ErrChecksum = errors.New("checksum mismatch")
)

// Tool is a pinned upstream release of one binary.
type Tool struct {
	// Name is the binary's name (without .exe) and the cache entry's prefix.
	Name string
	// Version is the release tag, "v1.57.2". Asset names carry it without
	// the leading "v", as goreleaser writes them.
	Version string
	// Repo is the GitHub owner/repo the release is published under.
	Repo string
	// GoPackage is the main package `go install` builds when the platform
	// has no published binary.
	GoPackage string
	// Checksums maps a release asset's file name to its sha256, copied from
	// the release's checksums file. A platform whose asset is absent has no
	// verified binary.
	Checksums map[string]string
}

// Asset is the release archive name for goos/goarch, in goreleaser's
// <name>_<version>_<os>_<arch>.<ext> layout (.zip on Windows).
func (t Tool) Asset(goos, goarch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("%s_%s_%s_%s%s", t.Name, strings.TrimPrefix(t.Version, "v"), goos, goarch, ext)
}

// Binary is the executable's file name on goos.
func (t Tool) Binary(goos string) string {
	if goos == "windows" {
		return t.Name + ".exe"
	}
	return t.Name
}

// checksum returns the pinned sha256 of goos/goarch's asset, if any.
func (t Tool) checksum(goos, goarch string) (asset, sum string, ok bool) {
	asset = t.Asset(goos, goarch)
	sum, ok = t.Checksums[asset]
	return asset, sum, ok
}

// Source says how an installed entry was produced.
type Source string

const (
	// SourceRelease is a release archive verified against its pinned sha256.
	SourceRelease Source = "release"
	// SourceGoInstall is a `go install` build, verified by the Go checksum
	// database.
	SourceGoInstall Source = "go-install"
)

// Method is how a platform gets a tool.
type Method string

const (
	// MethodRelease downloads the verified release binary.
	MethodRelease Method = "release"
	// MethodGoInstall builds it with `go install` (no published binary).
	MethodGoInstall Method = "go-install"
	// MethodUnavailable means neither is possible.
	MethodUnavailable Method = "unavailable"
)

// Installed is a usable tool on disk.
type Installed struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// Path is the executable's absolute path.
	Path   string `json:"-"`
	Source Source `json:"source"`
	// Platform is <goos>/<goarch>.
	Platform string `json:"platform"`
	// URL and ArchiveSHA256 are set for SourceRelease: what was downloaded
	// and the pinned hash it matched.
	URL           string `json:"url,omitempty"`
	ArchiveSHA256 string `json:"archive_sha256,omitempty"`
	// GoPackage is the `go install` spec for SourceGoInstall.
	GoPackage string `json:"go_package,omitempty"`
}

// Fetcher installs tools. The zero value is ready to use and resolves every
// field from the environment; tests set the fields instead.
type Fetcher struct {
	// CacheDir is the directory entries live in. "" is the release kind of
	// forge's shared tool cache ($FORGE_TOOLS_CACHE/release).
	CacheDir string
	// BaseURL replaces https://github.com. "" is $FORGE_TOOLS_MIRROR, else
	// DefaultBaseURL.
	BaseURL string
	// Client downloads archives. nil is a client with a 10-minute timeout
	// that honours the standard proxy environment.
	Client *http.Client
	// GOOS and GOARCH are the platform to install for. "" is this process's.
	GOOS, GOARCH string
	// LookPath finds the Go toolchain for the fallback. nil is exec.LookPath.
	LookPath func(file string) (string, error)
	// GoInstall runs `go install spec` with GOBIN=gobin. nil runs the real
	// toolchain.
	GoInstall func(ctx context.Context, spec, gobin string) error
	// Log receives the one-line notice of a download. nil is os.Stderr.
	Log io.Writer
}

// Ensure returns the path and pinned version of a usable t, installing it
// on first use with the default Fetcher. See the package doc.
func Ensure(ctx context.Context, t Tool) (path, version string, err error) {
	in, err := (&Fetcher{}).Ensure(ctx, t)
	if err != nil {
		return "", "", err
	}
	return in.Path, in.Version, nil
}

// Find reports t when it is installed in the default cache, without
// installing anything or using the network.
func Find(t Tool) (Installed, bool) { return (&Fetcher{}).Find(t) }

// MethodFor reports how the default Fetcher would get t on this platform.
func MethodFor(t Tool) Method { return (&Fetcher{}).Method(t) }

func (f *Fetcher) goos() string {
	if f.GOOS != "" {
		return f.GOOS
	}
	return runtime.GOOS
}

func (f *Fetcher) goarch() string {
	if f.GOARCH != "" {
		return f.GOARCH
	}
	return runtime.GOARCH
}

func (f *Fetcher) platform() string { return f.goos() + "/" + f.goarch() }

func (f *Fetcher) cacheDir() (string, error) {
	if f.CacheDir != "" {
		return f.CacheDir, nil
	}
	return toolcache.KindDir(toolcache.KindRelease)
}

func (f *Fetcher) baseURL() string {
	if f.BaseURL != "" {
		return strings.TrimRight(f.BaseURL, "/")
	}
	if m := strings.TrimSpace(os.Getenv(EnvMirror)); m != "" {
		return strings.TrimRight(m, "/")
	}
	return DefaultBaseURL
}

func (f *Fetcher) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

func (f *Fetcher) log() io.Writer {
	if f.Log != nil {
		return f.Log
	}
	return os.Stderr
}

func (f *Fetcher) hasGo() bool {
	look := f.LookPath
	if look == nil {
		look = exec.LookPath
	}
	_, err := look("go")
	return err == nil
}

// URL is where t's archive for the Fetcher's platform is downloaded from.
func (f *Fetcher) URL(t Tool) string {
	return fmt.Sprintf("%s/%s/releases/download/%s/%s", f.baseURL(), t.Repo, t.Version, t.Asset(f.goos(), f.goarch()))
}

// Method reports how the Fetcher's platform gets t.
func (f *Fetcher) Method(t Tool) Method {
	if _, _, ok := t.checksum(f.goos(), f.goarch()); ok {
		return MethodRelease
	}
	if t.GoPackage != "" && f.hasGo() {
		return MethodGoInstall
	}
	return MethodUnavailable
}

// key is t's cache entry name. The platform is part of it because it
// decides the bytes, and a FORGE_TOOLS_CACHE can be shared.
func (f *Fetcher) key(t Tool) string {
	return fmt.Sprintf("%s@%s-%s-%s", t.Name, t.Version, f.goos(), f.goarch())
}

// Find reports t when its entry is installed and usable. No network.
func (f *Fetcher) Find(t Tool) (Installed, bool) {
	dir, err := f.cacheDir()
	if err != nil {
		return Installed{}, false
	}
	return f.ready(t, filepath.Join(dir, f.key(t)))
}

// ready decides whether the entry at dir is a usable t: the manifest names
// t at its version on this platform, a release entry carries exactly the
// pinned checksum, and the binary is an executable regular file. The entry
// is published by rename only after verification, so presence of a
// matching manifest means the bytes beside it were verified.
func (f *Fetcher) ready(t Tool, dir string) (Installed, bool) {
	data, err := os.ReadFile(filepath.Join(dir, manifestName)) // #nosec G304 -- forge's own cache entry
	if err != nil {
		return Installed{}, false
	}
	var in Installed
	if json.Unmarshal(data, &in) != nil || in.Name != t.Name || in.Version != t.Version || in.Platform != f.platform() {
		return Installed{}, false
	}
	switch in.Source {
	case SourceRelease:
		if _, sum, ok := t.checksum(f.goos(), f.goarch()); !ok || in.ArchiveSHA256 != sum {
			return Installed{}, false
		}
	case SourceGoInstall:
	default:
		return Installed{}, false
	}
	bin := filepath.Join(dir, t.Binary(f.goos()))
	info, err := os.Stat(bin)
	if err != nil || !info.Mode().IsRegular() || (f.goos() != "windows" && info.Mode().Perm()&0o111 == 0) {
		return Installed{}, false
	}
	in.Path = bin
	return in, true
}

// Ensure returns a usable t, installing it when its entry is missing or
// unusable. See the package doc for the error contract.
func (f *Fetcher) Ensure(ctx context.Context, t Tool) (Installed, error) {
	if t.Name == "" || t.Version == "" {
		return Installed{}, fmt.Errorf("toolfetch: tool needs a name and a version, got %q@%q", t.Name, t.Version)
	}
	kindDir, err := f.cacheDir()
	if err != nil {
		return Installed{}, fmt.Errorf("%s %s: %w", t.Name, t.Version, err)
	}
	if in, ok := f.ready(t, filepath.Join(kindDir, f.key(t))); ok {
		return in, nil
	}

	var build func(ctx context.Context, stage string) error
	switch f.Method(t) {
	case MethodRelease:
		build = func(ctx context.Context, stage string) error { return f.installRelease(ctx, t, stage) }
	case MethodGoInstall:
		build = func(ctx context.Context, stage string) error { return f.installGo(ctx, t, stage) }
	default:
		return Installed{}, fmt.Errorf(
			"%s %s: no published binary for %s that forge has a checksum for, and no Go toolchain on PATH to build %s@%s with: %w\n"+
				"  anything that needs %s reports \"not scanned\", never clean — install Go, or run forge on a platform %s publishes",
			t.Name, t.Version, f.platform(), t.GoPackage, t.Version, ErrUnavailable, t.Name, t.Name)
	}

	readyFn := func(dir string) bool { _, ok := f.ready(t, dir); return ok }
	dir, err := toolcache.Ensure(ctx, kindDir, f.key(t), readyFn, build)
	if err != nil {
		return Installed{}, err
	}
	in, ok := f.ready(t, dir)
	if !ok {
		return Installed{}, fmt.Errorf("%s %s: the installed entry %s is not usable", t.Name, t.Version, dir)
	}
	return in, nil
}

// installRelease downloads t's archive into stage, verifies it against the
// pinned sha256, extracts the binary and writes the manifest.
func (f *Fetcher) installRelease(ctx context.Context, t Tool, stage string) error {
	asset, want, _ := t.checksum(f.goos(), f.goarch())
	url := f.URL(t)
	fmt.Fprintf(f.log(), "fetching %s %s for %s (%s, verified against forge's pinned sha256)\n", t.Name, t.Version, f.platform(), url)

	archive := filepath.Join(stage, asset)
	got, err := f.download(ctx, url, archive)
	if err != nil {
		return fmt.Errorf("%s %s: %w", t.Name, t.Version, err)
	}
	if got != want {
		return fmt.Errorf("%s %s: %w for %s\n  expected %s\n  got      %s\n"+
			"  the download was corrupted or substituted; forge will not run an unverified binary",
			t.Name, t.Version, ErrChecksum, asset, want, got)
	}
	bin := t.Binary(f.goos())
	if err := extractMember(archive, asset, bin, filepath.Join(stage, bin)); err != nil {
		return fmt.Errorf("%s %s: %w", t.Name, t.Version, err)
	}
	if err := os.Remove(archive); err != nil {
		return err
	}
	return writeManifest(stage, Installed{
		Name: t.Name, Version: t.Version, Source: SourceRelease, Platform: f.platform(),
		URL: url, ArchiveSHA256: want,
	})
}

// download writes url's body to dest and returns its sha256.
func (f *Fetcher) download(ctx context.Context, url, dest string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build request for %s: %w", url, err)
	}
	resp, err := f.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		hint := ""
		if f.baseURL() != DefaultBaseURL {
			hint = fmt.Sprintf(" (from the mirror %s=%s)", EnvMirror, f.baseURL())
		}
		return "", fmt.Errorf("download %s: HTTP %d%s", url, resp.StatusCode, hint)
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- a staging path forge chose
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, hash), io.LimitReader(resp.Body, maxArchiveBytes+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	if n > maxArchiveBytes {
		return "", fmt.Errorf("download %s: larger than %d bytes, which no pinned archive is", url, maxArchiveBytes)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// installGo builds t with `go install` into stage.
func (f *Fetcher) installGo(ctx context.Context, t Tool, stage string) error {
	spec := t.GoPackage + "@" + t.Version
	fmt.Fprintf(f.log(), "building %s %s for %s with go install %s (no published binary for this platform)\n", t.Name, t.Version, f.platform(), spec)
	install := f.GoInstall
	if install == nil {
		install = goInstall
	}
	if err := install(ctx, spec, stage); err != nil {
		return fmt.Errorf("%s %s: go install %s: %w", t.Name, t.Version, spec, err)
	}
	if _, err := os.Stat(filepath.Join(stage, t.Binary(f.goos()))); err != nil {
		return fmt.Errorf("%s %s: go install %s produced no %s: %w", t.Name, t.Version, spec, t.Binary(f.goos()), err)
	}
	return writeManifest(stage, Installed{
		Name: t.Name, Version: t.Version, Source: SourceGoInstall, Platform: f.platform(), GoPackage: spec,
	})
}

// goInstall runs the real `go install spec` into gobin, outside any module
// (spec@version ignores the current one anyway) and with cgo off, as the
// upstream release builds are.
func goInstall(ctx context.Context, spec, gobin string) error {
	cmd := exec.CommandContext(ctx, "go", "install", spec) // #nosec G204 -- a pinned package path and version
	cmd.Dir = gobin
	cmd.Env = goexec.Env("GOBIN="+gobin, "GOWORK=off", "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func writeManifest(stage string, in Installed) error {
	data, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(filepath.Join(stage, manifestName), append(data, '\n'), 0o644) // #nosec G306 -- a shared cache may serve several users
}
