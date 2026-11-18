package cli

// THE CODE SOURCE: which repository, which directory of it and which commit a
// release or a deploy came from, and the Reliant project that ran it. Sent to
// the control plane with every release cut (`forge env build --release`, a
// composed release), every `forge release record` and every deploy attempt,
// so an app can name its code: the app → code half of the code↔app link
// (control-plane docs/design/unified-shell-and-projects.md, §8.1 and D3; the
// control plane stores it in hosted_project_sources).
//
// THE WIRE SHAPE is controlplane.v1.DeployCodeSource, the optional field
// `code_source` on CutReleaseRequest and on OpenDeployAttemptRequest:
//
//	message DeployCodeSource {
//	  string remote = 1;             // "host/owner/repo"; "" when there is no shareable remote
//	  string subpath = 2;            // forge.yaml's directory in the repo, slash-separated; "" at the root
//	  string commit = 3;             // full object id; "" when unknown
//	  string reliant_project_id = 4; // RELIANT_PROJECT_ID, when a Reliant chat ran forge
//	}
//
// SENT AHEAD OF THE SERVER. A control plane that predates the field drops it
// (Connect's JSON codec discards unknown fields) and records everything else
// as before; nothing is negotiated and nothing waits on it.
//
// IT NEVER FAILS ANYTHING. A directory that is not a git checkout, a
// repository with no commits, no origin, a remote that is a local path: each
// sends what is known and leaves the rest empty. A dirty tree sends its HEAD,
// the commit the change was made on; the release's provenance already says
// it was dirty.
//
// NOTHING LEAVES THAT NAMES THIS MACHINE. The remote is canonicalized
// (release.CanonicalRepo: no scheme, no credentials, no ".git") and then
// dropped unless it reads as a hosted repository, so neither a token in a URL
// nor a home-directory path reaches the ledger.

import (
	"context"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/reliant-labs/forge/pkg/release"
)

// reliantProjectIDEnv names the Reliant project a chat runs in. The Reliant
// daemon sets it next to RELIANT_FORGE_ORG in a chat's environment.
const reliantProjectIDEnv = "RELIANT_PROJECT_ID"

// codeSourceTimeout bounds the git reads. A var so tests can shrink it.
var codeSourceTimeout = 5 * time.Second

// codeSource is one DeployCodeSource. The zero value is "nothing known".
type codeSource struct {
	Remote           string
	Subpath          string
	Commit           string
	ReliantProjectID string
}

// wire is the proto3 JSON of the source, or nil when it says nothing — so a
// request from outside any checkout and any chat is the request it was.
func (s codeSource) wire() map[string]any {
	out := map[string]any{}
	putNonEmptyAny(out, "remote", s.Remote)
	putNonEmptyAny(out, "subpath", s.Subpath)
	putNonEmptyAny(out, "commit", s.Commit)
	putNonEmptyAny(out, "reliantProjectId", s.ReliantProjectID)
	if len(out) == 0 {
		return nil
	}
	return out
}

func putNonEmptyAny(m map[string]any, key, value string) {
	if value != "" {
		m[key] = value
	}
}

// forRelease is the source a cut of r sends: the release's own commit when it
// names one (`release record --commit` from CI, or what was built), else the
// checkout's HEAD; and the checkout's remote, else the one r's provenance
// recorded.
func (s codeSource) forRelease(r release.Release) codeSource {
	commit := r.Git.Commit
	if r.Provenance != nil && r.Provenance.Commit != "" {
		commit = r.Provenance.Commit
	}
	if commit = strings.ToLower(strings.TrimSpace(commit)); release.ValidObjectID(commit) {
		s.Commit = commit
	}
	if s.Remote == "" && r.Provenance != nil {
		s.Remote = shareableRemote(r.Provenance.Repo)
	}
	return s
}

// codeSourceProbe captures the code source of one checkout, once per process,
// on first use: a command that never cuts or deploys runs no git for it.
type codeSourceProbe struct {
	dir     string
	getenv  func(string) string
	capture func(ctx context.Context, dir string, getenv func(string) string) codeSource

	once sync.Once
	src  codeSource
}

func newCodeSourceProbe(dir string, getenv func(string) string) *codeSourceProbe {
	return &codeSourceProbe{dir: dir, getenv: getenv, capture: captureCodeSource}
}

// get is the captured source. A nil probe — a ledger opened with no checkout
// behind it — knows nothing.
func (p *codeSourceProbe) get(ctx context.Context) codeSource {
	if p == nil {
		return codeSource{}
	}
	p.once.Do(func() { p.src = p.capture(ctx, p.dir, p.getenv) })
	return p.src
}

// attachCodeSource makes a hosted ledger send the code source of the checkout
// at projectDir with what it records. Any other ledger is returned unchanged:
// a file ledger is on this machine and needs no link back to it.
func attachCodeSource(l envLedger, projectDir string, getenv func(string) string) envLedger {
	if hs, ok := l.Bindings.(*hostedStore); ok && hs.source == nil && projectDir != "" {
		hs.source = newCodeSourceProbe(projectDir, getenv)
	}
	return l
}

// captureCodeSource reads dir's code source. Every git failure is an empty
// field, never an error: see the file comment.
func captureCodeSource(ctx context.Context, dir string, getenv func(string) string) codeSource {
	var s codeSource
	if getenv != nil {
		s.ReliantProjectID = reliantProjectID(getenv(reliantProjectIDEnv))
	}
	ctx, cancel := context.WithTimeout(ctx, codeSourceTimeout)
	defer cancel()
	// --show-prefix is dir's path below the top of its checkout, with a
	// trailing slash, "" at the top, and fails outside a checkout — which
	// ends the capture, since nothing else git could say would apply.
	prefix, err := codeSourceGit(ctx, dir, "rev-parse", "--show-prefix")
	if err != nil {
		return s
	}
	s.Subpath = strings.Trim(prefix, "/")
	var wg sync.WaitGroup
	wg.Go(func() {
		if c, err := codeSourceGit(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err == nil && release.ValidObjectID(c) {
			s.Commit = c
		}
	})
	wg.Go(func() {
		if url, err := codeSourceGit(ctx, dir, "remote", "get-url", "origin"); err == nil {
			s.Remote = shareableRemote(url)
		}
	})
	wg.Wait()
	return s
}

func codeSourceGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// shareableHost is a repository host as a remote names one: a DNS name or an
// address. A single letter (a Windows drive) is not one.
var shareableHost = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*[a-z0-9]$`)

// shareableRemote is url canonicalized to "host/owner/repo", or "" when it
// does not name a hosted repository: a local path, a file:// URL, anything
// still carrying a credential, query or fragment after canonicalization.
func shareableRemote(url string) string {
	canon := release.CanonicalRepo(url)
	if canon == "" || strings.ContainsAny(canon, "@?#\\ \t\r\n") {
		return ""
	}
	host, path, ok := strings.Cut(canon, "/")
	if !ok || !shareableHost.MatchString(host) || strings.Trim(path, "/") == "" {
		return ""
	}
	return canon
}

// reliantProjectIDPattern bounds RELIANT_PROJECT_ID to an opaque id: the
// variable comes from the environment, and a malformed one is dropped rather
// than sent.
var reliantProjectIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func reliantProjectID(v string) string {
	v = strings.TrimSpace(v)
	if !reliantProjectIDPattern.MatchString(v) {
		return ""
	}
	return v
}
