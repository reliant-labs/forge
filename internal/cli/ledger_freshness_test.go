package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/cluster"
)

// Tests for verify's stale-ledger check. They use REAL git: an "origin" bare
// repository and a clone, because the property under test is a fact about
// two copies of a committed file, and a fake would only restate the
// implementation's own idea of git.

const (
	digestV1 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	digestV2 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// ledgerRepo is a clone of a bare origin, plus a second clone ("other") that
// plays the teammate who records the next release.
type ledgerRepo struct {
	t      *testing.T
	origin string
	dir    string // the checkout verify runs in
	other  string
}

// ledgerGit runs git hermetically: no global or system config, a fixed identity,
// so a commit works on a box with no user.name set.
func ledgerGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

func newLedgerRepo(t *testing.T) *ledgerRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	r := &ledgerRepo{t: t, origin: filepath.Join(root, "origin.git"), dir: filepath.Join(root, "work"), other: filepath.Join(root, "other")}
	ledgerGit(t, root, "init", "-q", "--bare", "-b", "main", r.origin)
	ledgerGit(t, root, "clone", "-q", r.origin, r.dir)
	if err := os.WriteFile(filepath.Join(r.dir, "forge.yaml"), []byte("name: demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// prod → v1, recorded and pushed: both copies agree.
	writeBinding(t, r.dir, "prod", "v1", map[string]string{"api": digestV1})
	r.commitPush(r.dir, "ledger: v1")
	ledgerGit(t, root, "clone", "-q", r.origin, r.other)
	return r
}

func (r *ledgerRepo) commitPush(dir, msg string) {
	r.t.Helper()
	ledgerGit(r.t, dir, "add", "-A")
	ledgerGit(r.t, dir, "commit", "-q", "-m", msg)
	ledgerGit(r.t, dir, "push", "-q", "origin", "HEAD:main")
}

// teammateRecords plays the release that lands on origin while dir is not
// looking: another checkout promotes prod → v2 and pushes; dir FETCHES (so
// it knows origin moved) but does not merge.
func (r *ledgerRepo) teammateRecords(release, digest string) {
	r.t.Helper()
	ledgerGit(r.t, r.other, "pull", "-q", "origin", "main")
	writeBinding(r.t, r.other, "prod", release, map[string]string{"api": digest})
	r.commitPush(r.other, "ledger: "+release)
	ledgerGit(r.t, r.dir, "fetch", "-q", "origin")
}

func (r *ledgerRepo) verify(lister clusterImageLister, jsonOut bool) (string, error) {
	r.t.Helper()
	r.t.Chdir(r.dir)
	var err error
	out := captureStdout(r.t, func() {
		err = runEnvStatusRelease(context.Background(), "prod", envStatusOptions{
			JSON:     jsonOut,
			Lister:   lister,
			Resolver: stubResolver{target: envTarget{KubeContext: "test-context", Namespace: "test-ns"}},
		})
	})
	return out, err
}

func runningDigest(d string) *stubLister {
	return &stubLister{images: []cluster.WorkloadImage{deployImage("api", "ghcr.io/acme/api@"+d)}}
}

// TestVerify_BehindLedgerIsUndetermined is the incident: prod was released
// to v2 and runs v2, but this checkout never pulled the ledger commit. Without
// the check, verify compares against v1 and reports DRIFT on a deploy that
// went fine. With it, the verdict is exit 2 and says why.
func TestVerify_BehindLedgerIsUndetermined(t *testing.T) {
	r := newLedgerRepo(t)
	r.teammateRecords("v2", digestV2)

	out, err := r.verify(runningDigest(digestV2), false)
	if code := exitCodeOf(t, err); code != exitUndetermined {
		t.Fatalf("a behind ledger must exit %d (could not determine), got %d: %v\n%s", exitUndetermined, code, err, out)
	}
	for _, want := range []string{"BEHIND", "origin/main", "git pull"} {
		if !strings.Contains(out+err.Error(), want) {
			t.Errorf("the report must say %q, got:\n%s\n%v", want, out, err)
		}
	}
}

// The dangerous direction: the new release NEVER deployed, so the cluster
// still runs v1 — which MATCHES the stale v1 this checkout holds. Without the
// check this is a green gate on exactly the failure verify exists to catch.
func TestVerify_BehindLedgerCannotFalselyMatch(t *testing.T) {
	r := newLedgerRepo(t)
	r.teammateRecords("v2", digestV2)

	_, err := r.verify(runningDigest(digestV1), false)
	if code := exitCodeOf(t, err); code != exitUndetermined {
		t.Fatalf("a match against a stale ledger must not pass: got exit %d (%v)", code, err)
	}
}

// Current and ahead are not failures. Ahead is the normal state between
// recording a release locally and merging it.
func TestVerify_CurrentAndAheadLedgerPass(t *testing.T) {
	r := newLedgerRepo(t)
	if _, err := r.verify(runningDigest(digestV1), false); err != nil {
		t.Fatalf("current ledger + matching cluster must pass: %v", err)
	}

	writeBinding(t, r.dir, "prod", "v2", map[string]string{"api": digestV2}) // recorded here, not pushed
	out, err := r.verify(runningDigest(digestV2), true)
	if err != nil {
		t.Fatalf("an AHEAD ledger must not fail verify: %v\n%s", err, out)
	}
	var doc struct {
		OK     bool                   `json:"ok"`
		Ledger map[string]interface{} `json:"ledger"`
	}
	if jerr := json.Unmarshal([]byte(out), &doc); jerr != nil {
		t.Fatalf("decode: %v\n%s", jerr, out)
	}
	if !doc.OK || doc.Ledger["state"] != "ahead" || doc.Ledger["ref"] != "origin/main" {
		t.Fatalf("want ok with ledger.state=ahead ref=origin/main, got ok=%v ledger=%v", doc.OK, doc.Ledger)
	}
}

// Two writers appended to different copies: neither is the ledger.
func TestVerify_DivergedLedgerIsUndetermined(t *testing.T) {
	r := newLedgerRepo(t)
	r.teammateRecords("v2", digestV2)
	writeBinding(t, r.dir, "prod", "v3", map[string]string{"api": digestV2}) // a different v1 → v3 here

	out, err := r.verify(runningDigest(digestV2), true)
	if code := exitCodeOf(t, err); code != exitUndetermined {
		t.Fatalf("a diverged ledger must exit %d, got %d: %v", exitUndetermined, code, err)
	}
	var doc struct {
		OK     bool `json:"ok"`
		Ledger struct {
			State string `json:"state"`
		} `json:"ledger"`
	}
	if jerr := json.Unmarshal([]byte(out), &doc); jerr != nil {
		t.Fatalf("--json must still emit one document: %v\n%s", jerr, out)
	}
	if doc.OK || doc.Ledger.State != "diverged" {
		t.Fatalf("want ok=false ledger.state=diverged, got %+v", doc)
	}
}

// "Never promoted" in a checkout that has not pulled the first promotion is
// stale too: the unbound early return must not skip the check.
func TestVerify_UnboundButBehindIsUndetermined(t *testing.T) {
	r := newLedgerRepo(t)
	ledgerGit(t, r.other, "pull", "-q", "origin", "main")
	writeBinding(t, r.other, "staging", "v1", map[string]string{"api": digestV1})
	r.commitPush(r.other, "ledger: staging v1")
	ledgerGit(t, r.dir, "fetch", "-q", "origin")

	t.Chdir(r.dir)
	var err error
	captureStdout(t, func() {
		err = runEnvStatusRelease(context.Background(), "staging", envStatusOptions{
			Lister:   &stubLister{},
			Resolver: stubResolver{target: envTarget{KubeContext: "c", Namespace: "n"}},
		})
	})
	if code := exitCodeOf(t, err); code != exitUndetermined {
		t.Fatalf("'never promoted' from a behind checkout must exit %d, got %d (%v)", exitUndetermined, code, err)
	}
}

// Outside git there is nothing to compare: unknown, reported, never a failure.
func TestVerify_NoGitIsUnknownNotAFailure(t *testing.T) {
	dir := t.TempDir()
	writeBinding(t, dir, "prod", "v1", map[string]string{"api": digestV1})
	got := newFileBindingStore(dir).LedgerFreshness(context.Background(), "prod")
	if got.State != ledgerFreshnessUnknown || got.State.stale() {
		t.Fatalf("outside git the state must be unknown and not stale, got %+v", got)
	}
	if err := runEnvVerifyInDir(t, dir, "prod", runningDigest(digestV1)); err != nil {
		t.Fatalf("a project outside git must still verify: %v", err)
	}
}

// The hosted store is the ledger; it is not asked.
func TestVerify_HostedStoreHasNoFreshness(t *testing.T) {
	var store bindingStore = &hostedStore{}
	if _, ok := store.(ledgerFreshnessChecker); ok {
		t.Fatal("a control-plane ledger has no copy to be behind and must not implement ledgerFreshnessChecker")
	}
	var file bindingStore = newFileBindingStore(t.TempDir())
	if _, ok := file.(ledgerFreshnessChecker); !ok {
		t.Fatal("the file ledger must implement ledgerFreshnessChecker")
	}
}

func TestLedgerFreshnessJSONRoundTripsAndRejectsUnknown(t *testing.T) {
	for _, s := range []ledgerFreshness{ledgerFreshnessUnknown, ledgerCurrent, ledgerBehind, ledgerAhead, ledgerDiverged} {
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var back ledgerFreshness
		if err := json.Unmarshal(raw, &back); err != nil || back != s {
			t.Errorf("%s did not round-trip: %s → %s (%v)", s, raw, back, err)
		}
	}
	var f ledgerFreshness
	if err := json.Unmarshal([]byte(`"stale-ish"`), &f); err == nil {
		t.Error("an unknown ledger state must be refused, not defaulted")
	}
}

func TestCompareLedgerLogs(t *testing.T) {
	cases := []struct {
		local, upstream []string
		want            ledgerFreshness
	}{
		{nil, nil, ledgerCurrent},
		{[]string{"a"}, []string{"a"}, ledgerCurrent},
		{[]string{"a"}, []string{"a", "b"}, ledgerBehind},
		{nil, []string{"a"}, ledgerBehind},
		{[]string{"a", "b"}, []string{"a"}, ledgerAhead},
		{[]string{"a", "c"}, []string{"a", "b"}, ledgerDiverged},
		{[]string{"x"}, []string{"a", "b"}, ledgerDiverged},
	}
	for _, c := range cases {
		if got := compareLedgerLogs(c.local, c.upstream); got != c.want {
			t.Errorf("compare(%v, %v) = %s, want %s", c.local, c.upstream, got, c.want)
		}
	}
}
