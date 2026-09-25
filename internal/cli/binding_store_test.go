package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/cluster"
	"github.com/reliant-labs/forge/pkg/release"
)

// ─── Shared fakes ────────────────────────────────────────────────────────────

// mustTime parses an RFC3339 literal for a fixture.
func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad fixture time %q: %v", s, err)
	}
	return ts
}

// ociRelease is a release of shared OCI images, for fixtures.
func ociRelease(version string, images map[string]string) release.Release {
	arts := map[string]release.Artifact{}
	for name, d := range images {
		arts[name] = release.Artifact{Kind: release.KindOCI, Mode: release.ModeShared, Digests: map[string]string{release.SharedVariant: d}}
	}
	return release.Release{Version: version, Artifacts: arts}
}

// memBindingStore is a complete bindingStore that touches no filesystem, and
// applies the same release.Decide rules both real backends apply — so a test
// running a consumer against it exercises real append semantics, not a map.
//
// It is also the proof the seam is real: if a method wanted a projectDir or a
// path, a map could not satisfy it, and neither could the hosted backend.
type memBindingStore struct {
	history map[string][]release.Promotion
	err     error
	n       int
}

// newMemBindingStore seeds each env with ONE current entry. Env and Kind are
// filled in when a fixture leaves them blank.
func newMemBindingStore(current map[string]release.Promotion) *memBindingStore {
	m := &memBindingStore{history: map[string][]release.Promotion{}}
	for env, p := range current {
		p.Env = env
		if p.Kind == "" {
			p.Kind = release.KindPromote
		}
		m.history[env] = []release.Promotion{p}
	}
	return m
}

func (m *memBindingStore) Current(_ context.Context, env string) (release.Promotion, bool, error) {
	if m.err != nil {
		return release.Promotion{}, false, m.err
	}
	h := m.history[env]
	if len(h) == 0 {
		return release.Promotion{}, false, nil
	}
	return h[len(h)-1], true, nil
}

func (m *memBindingStore) Append(_ context.Context, p release.Promotion) (release.Promotion, error) {
	if m.err != nil {
		return release.Promotion{}, m.err
	}
	existing, err := release.Decide(m.history[p.Env], p)
	if err != nil {
		return release.Promotion{}, err
	}
	if existing != nil {
		return *existing, nil
	}
	m.n++
	p.ID = fmt.Sprintf("mem-%d", m.n)
	p.PromotedAt = time.Date(2026, 9, 23, 12, 0, m.n, 0, time.UTC)
	m.history[p.Env] = append(m.history[p.Env], p)
	return p, nil
}

func (m *memBindingStore) Location() string { return "memory://promotions" }

// memReleaseLedger is the release half.
type memReleaseLedger struct {
	releases map[string]release.Release
}

func newMemReleaseLedger(rels ...release.Release) *memReleaseLedger {
	m := &memReleaseLedger{releases: map[string]release.Release{}}
	for _, r := range rels {
		m.releases[r.Version] = r
	}
	return m
}

func (m *memReleaseLedger) Cut(_ context.Context, r release.Release) (bool, error) {
	if old, ok := m.releases[r.Version]; ok {
		return false, release.CheckRecut(old, r)
	}
	m.releases[r.Version] = r
	return true, nil
}

func (m *memReleaseLedger) Get(_ context.Context, v string) (*release.Release, error) {
	r, ok := m.releases[v]
	if !ok {
		return nil, nil
	}
	return &r, nil
}

func (m *memReleaseLedger) List(context.Context) ([]release.Release, error) {
	out := make([]release.Release, 0, len(m.releases))
	for _, r := range m.releases {
		out = append(out, r)
	}
	sortReleasesNewestFirst(out)
	return out, nil
}

func (m *memReleaseLedger) Location() string { return "memory://releases" }

// ─── The file backend ────────────────────────────────────────────────────────

// A project that has never been promoted has no log, and asking about an env
// there is a normal "not bound", not an error.
func TestFileBindingStore_MissingIsUnbound(t *testing.T) {
	store := newFileBindingStore(t.TempDir())
	p, bound, err := store.Current(context.Background(), "prod")
	if err != nil {
		t.Fatalf("missing ledger must not error: %v", err)
	}
	if bound {
		t.Errorf("prod must be unbound in an empty project, got %+v", p)
	}
}

// Every promote APPENDS one line; the current binding is the last line and
// earlier lines are never rewritten. A backend that rewrote the file (the
// retired env-releases.json behaviour) fails the byte-prefix assertion.
func TestFileBindingStore_AppendsOneLinePerPromotion(t *testing.T) {
	dir := t.TempDir()
	store := newFileBindingStore(dir)
	ctx := context.Background()

	first, err := store.Append(ctx, release.Promotion{Env: "prod", Release: "v1", Kind: release.KindPromote,
		Resolved: map[string]string{"api": sha("a")}})
	if err != nil {
		t.Fatalf("append v1: %v", err)
	}
	if first.ID == "" || first.PromotedAt.IsZero() {
		t.Errorf("the backend must stamp ID and PromotedAt, got %+v", first)
	}
	path := promotionLogPath(dir, "prod")
	afterFirst, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the log must live at .forge/promotions/prod.jsonl: %v", err)
	}

	if _, err := store.Append(ctx, release.Promotion{Env: "prod", Release: "v2", Kind: release.KindPromote,
		Resolved: map[string]string{"api": sha("b")}}); err != nil {
		t.Fatalf("append v2: %v", err)
	}
	afterSecond, _ := os.ReadFile(path)
	if !bytes.HasPrefix(afterSecond, afterFirst) {
		t.Fatalf("the first entry was rewritten, not appended to:\nbefore:\n%s\nafter:\n%s", afterFirst, afterSecond)
	}
	if n := bytes.Count(afterSecond, []byte("\n")); n != 2 {
		t.Fatalf("want exactly 2 lines, got %d:\n%s", n, afterSecond)
	}

	cur, bound, err := store.Current(ctx, "prod")
	if err != nil || !bound || cur.Release != "v2" || cur.Resolved["api"] != sha("b") {
		t.Fatalf("current must be the LAST line (v2), got bound=%v %+v err=%v", bound, cur, err)
	}
	if _, bound, _ := store.Current(ctx, "staging"); bound {
		t.Error("a different env must stay unbound — one log per env")
	}
}

// A retry of the env's current state appends NOTHING and returns the existing
// entry — the CI-retry rule. v1→v2→v1 is still three real moves.
func TestFileBindingStore_RetryIsNoOp(t *testing.T) {
	dir := t.TempDir()
	store := newFileBindingStore(dir)
	ctx := context.Background()
	p := func(v string) release.Promotion {
		return release.Promotion{Env: "prod", Release: v, Kind: release.KindPromote, Resolved: map[string]string{"api": sha("a")}}
	}

	first, _ := store.Append(ctx, p("v1"))
	again, err := store.Append(ctx, p("v1"))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if again.ID != first.ID {
		t.Errorf("a retry must return the EXISTING entry %s, got %s", first.ID, again.ID)
	}
	_, _ = store.Append(ctx, p("v2"))
	_, _ = store.Append(ctx, p("v1"))
	history, _ := store.History("prod")
	if len(history) != 3 {
		t.Fatalf("v1, v1(retry), v2, v1 must record 3 entries, got %d", len(history))
	}
}

// ROLLBACK IS A NEW LINE of kind rollback, and it must target a release the
// env has run. A rollback to a never-promoted release writes nothing.
func TestFileBindingStore_RollbackIsANewLine(t *testing.T) {
	dir := t.TempDir()
	store := newFileBindingStore(dir)
	ctx := context.Background()
	for _, v := range []string{"v1", "v2"} {
		if _, err := store.Append(ctx, release.Promotion{Env: "prod", Release: v, Kind: release.KindPromote,
			Resolved: map[string]string{"api": sha(v[1:])}}); err != nil {
			t.Fatalf("append %s: %v", v, err)
		}
	}

	_, err := store.Append(ctx, release.Promotion{Env: "prod", Release: "v9", Kind: release.KindRollback})
	if !errors.Is(err, release.ErrNeverPromoted) {
		t.Fatalf("rollback to a never-run release must be ErrNeverPromoted, got %v", err)
	}

	rb, err := store.Append(ctx, release.Promotion{Env: "prod", Release: "v1", Kind: release.KindRollback,
		Resolved: map[string]string{"api": sha("1")}, Note: "5xx spike"})
	if err != nil {
		t.Fatalf("rollback to v1: %v", err)
	}
	history, _ := store.History("prod")
	if len(history) != 3 {
		t.Fatalf("want 3 lines (v1, v2, rollback→v1) — the refused rollback must write nothing; got %d", len(history))
	}
	last := history[2]
	if last.Kind != release.KindRollback || last.Release != "v1" || last.Note != "5xx spike" || last.ID != rb.ID {
		t.Errorf("the rollback must be recorded as its own entry of kind rollback, got %+v", last)
	}
	if history[0].Kind != release.KindPromote || history[0].Release != "v1" {
		t.Errorf("the original v1 promotion must be untouched, got %+v", history[0])
	}
}

// A line that does not validate poisons the log: its LAST line cannot be
// trusted as current if an earlier one is unreadable.
func TestFileBindingStore_CorruptLineIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := promotionLogPath(dir, "prod")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte(`{"env":"prod","release":"v1","kind":"sideways","resolved":{}}`+"\n"), 0o644)
	if _, _, err := newFileBindingStore(dir).Current(context.Background(), "prod"); err == nil {
		t.Fatal("an unknown promotion kind must be refused, not read as a promote")
	}
}

// The retired env-releases.json is never read as a ledger, and its presence
// is an ERROR — silently reading "never promoted" would make the next deploy
// ship mutable tags.
func TestFileBindingStore_LegacyFileIsRefused(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, ".forge"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, legacyEnvReleasesRel), []byte(`{"bindings":{}}`), 0o644)
	_, _, err := newFileBindingStore(dir).Current(context.Background(), "prod")
	if err == nil || !strings.Contains(err.Error(), "forge release convert-ledger") {
		t.Fatalf("want an error naming the conversion, got %v", err)
	}
}

// The one-time conversion carries each env's digests over unchanged, stamps
// the now-required artifact kinds, and removes the legacy file.
func TestConvertLegacyLedger(t *testing.T) {
	dir := t.TempDir()
	rels := filepath.Join(dir, releasesDirRel)
	_ = os.MkdirAll(rels, 0o755)
	_ = os.WriteFile(filepath.Join(rels, "v1.5.18.json"), []byte(`{
  "release": "v1.5.18", "git": {"commit": "3dc9b81", "dirty": true}, "created_at": "2026-09-19T01:27:23Z",
  "artifacts": {
    "control-plane": {"mode": "shared", "digests": {"*": "`+sha("a")+`"}},
    "reliant-web": {"mode": "source", "source": {"repo": "github.com/reliant-labs/reliant", "ref": "v1.7.15", "subdir": "web", "commit": "d5f6184"}}
  }
}`), 0o644)
	_ = os.WriteFile(filepath.Join(dir, legacyEnvReleasesRel), []byte(`{"bindings": {
  "prod": {"release": "v1.5.18", "resolved": {"control-plane": "`+sha("a")+`"},
           "sources": {"reliant-web": {"repo": "github.com/reliant-labs/reliant", "ref": "v1.7.15", "subdir": "web", "commit": "d5f6184"}},
           "promoted_at": "2026-09-19T01:27:23Z"}
}}`), 0o644)

	if _, err := convertLegacyLedger(dir); err != nil {
		t.Fatalf("convert: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, legacyEnvReleasesRel)); !os.IsNotExist(err) {
		t.Error("the legacy file must be removed after conversion")
	}
	rel, err := ReadRelease(dir, "v1.5.18")
	if err != nil || rel == nil {
		t.Fatalf("the converted release must validate: %v", err)
	}
	if rel.Artifacts["control-plane"].Kind != release.KindOCI || rel.Artifacts["reliant-web"].Kind != release.KindGit {
		t.Errorf("kinds not stamped: %+v", rel.Artifacts)
	}
	cur, bound, err := newFileBindingStore(dir).Current(context.Background(), "prod")
	if err != nil || !bound {
		t.Fatalf("prod must be bound after conversion: bound=%v err=%v", bound, err)
	}
	if cur.Release != "v1.5.18" || cur.Resolved["control-plane"] != sha("a") ||
		cur.Sources["reliant-web"].Commit != "d5f6184" || !cur.PromotedAt.Equal(mustTime(t, "2026-09-19T01:27:23Z")) {
		t.Errorf("the binding must carry over unchanged, got %+v", cur)
	}
	// Idempotent: a second run finds nothing to do.
	if done, err := convertLegacyLedger(dir); err != nil || len(done) != 0 {
		t.Errorf("second conversion must be a no-op, got %v %v", done, err)
	}
}

func TestFileBindingStore_LocationIsThePromotionsDir(t *testing.T) {
	dir := t.TempDir()
	if got, want := newFileBindingStore(dir).Location(), filepath.Join(dir, ".forge", "promotions"); got != want {
		t.Errorf("Location() = %q, want %q", got, want)
	}
}

// A release version can be re-cut only with the same bytes.
func TestWriteRelease_Immutable(t *testing.T) {
	dir := t.TempDir()
	r := ociRelease("v1", map[string]string{"api": sha("a")})
	if err := WriteRelease(dir, r); err != nil {
		t.Fatalf("cut: %v", err)
	}
	if err := WriteRelease(dir, r); err != nil {
		t.Fatalf("an identical re-cut is a retry, not an error: %v", err)
	}
	if err := WriteRelease(dir, ociRelease("v1", map[string]string{"api": sha("b")})); !errors.Is(err, release.ErrReleaseConflict) {
		t.Fatalf("a different artifact set under v1 must be ErrReleaseConflict, got %v", err)
	}
	got, _ := ReadRelease(dir, "v1")
	if got.Artifacts["api"].Digests[release.SharedVariant] != sha("a") {
		t.Error("a refused re-cut must not have overwritten the release")
	}
}

// ─── Selection ───────────────────────────────────────────────────────────────

// An env declaring forge.ControlPlane gets the HOSTED ledger, labelled with the
// endpoint URL; an env declaring none gets the project's files. Mutation: make
// ledgerForEntities always return fileLedger and the first half fails.
func TestLedgerForEntities_SelectsHostedWhenControlPlaneDeclared(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FORGE_TEST_CP_TOKEN", "rlat_test")

	hosted, err := ledgerForEntities("prod", &KCLEntities{ControlPlane: &ControlPlaneEntity{
		Type: "control_plane", Endpoint: "https://cp.example.com/", TokenEnv: "FORGE_TEST_CP_TOKEN",
	}}, dir)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if !hosted.Hosted {
		t.Fatal("an env declaring forge.ControlPlane must use the hosted ledger")
	}
	if _, isHosted := hosted.Bindings.(*hostedStore); !isHosted {
		t.Errorf("bindings = %T, want *hostedStore", hosted.Bindings)
	}
	if got := hosted.Bindings.Location(); got != "https://cp.example.com" {
		t.Errorf("a hosted ledger's label must be the endpoint URL, got %q", got)
	}

	local, err := ledgerForEntities("dev", &KCLEntities{}, dir)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if local.Hosted {
		t.Fatal("an env declaring no control plane must use the file ledger")
	}
	if _, isFile := local.Bindings.(fileBindingStore); !isFile {
		t.Errorf("bindings = %T, want fileBindingStore", local.Bindings)
	}
}

// bindingStoreFor reads the env's rendered KCL: the same declaration selects
// hosted end to end.
func TestBindingStoreFor_ReadsTheEnvDeclaration(t *testing.T) {
	dir := t.TempDir()
	declareEnvDir(t, dir, "prod")
	t.Setenv("FORGE_TEST_CP_TOKEN", "rlat_test")
	t.Setenv("FORGE_KCL_RENDER_FIXTURE", writeKCLFixture(t,
		`{"control_plane":{"type":"control_plane","endpoint":"https://cp.example.com","token_env":"FORGE_TEST_CP_TOKEN"}}`))

	store, err := bindingStoreFor(context.Background(), dir, "prod")
	if err != nil {
		t.Fatalf("bindingStoreFor: %v", err)
	}
	if _, ok := store.(*hostedStore); !ok {
		t.Fatalf("bindingStoreFor = %T, want *hostedStore for an env declaring forge.ControlPlane", store)
	}
}

// A render failure for a DECLARED env is an error, never a silent fallback to
// files — for a hosted env that fallback reads "never promoted".
func TestBindingStoreFor_RenderFailureIsNotAFallback(t *testing.T) {
	dir := t.TempDir()
	declareEnvDir(t, dir, "prod")
	t.Setenv("FORGE_KCL_RENDER_FIXTURE", filepath.Join(dir, "does-not-exist.json"))
	if _, err := bindingStoreFor(context.Background(), dir, "prod"); err == nil {
		t.Fatal("an unrenderable env must not silently get the file ledger")
	}
}

// ─── Consumers against a non-file backend ────────────────────────────────────

// `forge env verify` runs end to end with its ledger served from memory, and
// reaches the real DRIFT verdict.
func TestRunEnvVerify_AgainstNonFileBackend(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	store := newMemBindingStore(map[string]release.Promotion{
		"prod": {Release: "v1.5.13", Resolved: map[string]string{"control-plane": sha("a")},
			PromotedAt: mustTime(t, "2026-09-11T12:00:00Z")},
	})
	lister := &stubLister{images: []cluster.WorkloadImage{{
		Kind: "Deployment", Name: "control-plane", Container: "app",
		Image: "ghcr.io/acme/control-plane@" + sha("b"),
	}}}

	err := runEnvVerify(context.Background(), "prod", envVerifyOptions{
		Lister:   lister,
		Resolver: stubResolver{target: envTarget{KubeContext: "test-context", Namespace: "test-ns"}},
		Bindings: store,
	})
	if got := exitCodeOf(t, err); got != 1 {
		t.Fatalf("drift against a memory-backed ledger must exit 1, got %d (err: %v)", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, promotionsDirRel)); !os.IsNotExist(err) {
		t.Errorf("no promotion log should exist; the binding came from memory (stat err: %v)", err)
	}
}

func TestRunEnvVerify_UnboundAgainstNonFileBackend(t *testing.T) {
	t.Chdir(t.TempDir())
	err := runEnvVerify(context.Background(), "prod", envVerifyOptions{
		Lister:   &stubLister{},
		Resolver: stubResolver{target: envTarget{KubeContext: "test-context", Namespace: "test-ns"}},
		Bindings: newMemBindingStore(nil),
	})
	if err != nil {
		t.Fatalf("an unbound env is not a failure, got: %v", err)
	}
}

// The deploy digest resolver: a bound promotion overrides the build state.
func TestResolveDeployDigests_AgainstNonFileBackend(t *testing.T) {
	dir := t.TempDir()
	if err := WriteBuildState(dir, "prod", BuildState{
		Image: "control-plane", Tag: "old", Pushed: true, PushedAt: nowRFC3339(), Digest: sha("0"),
	}); err != nil {
		t.Fatalf("write build state: %v", err)
	}
	store := newMemBindingStore(map[string]release.Promotion{
		"prod": {Release: "v1.4.0", Resolved: map[string]string{"control-plane": sha("a")}},
	})
	digests, boundRel, err := resolveDeployDigests(context.Background(), dir, "prod", false, store)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if boundRel != "v1.4.0" || digests["control-plane"] != sha("a") {
		t.Errorf("release must override build state: rel=%q digests=%v", boundRel, digests)
	}
}

// runPromote with no injected seams resolves the env's DECLARED backend — for
// a project with no KCL, the files — and appends there.
func TestRunPromote_AppendsThroughTheDeclaredStore(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := WriteRelease(dir, ociRelease("v1.4.0", map[string]string{"control-plane": sha("a")})); err != nil {
		t.Fatalf("write release: %v", err)
	}
	captureStdout(t, func() {
		if err := runPromote(context.Background(), "v1.4.0", "staging", promoteOptions{ProjectDir: dir, Git: allCommitsPresent()}); err != nil {
			t.Errorf("promote: %v", err)
		}
	})
	cur, bound, err := newFileBindingStore(dir).Current(context.Background(), "staging")
	if err != nil || !bound || cur.Release != "v1.4.0" || cur.Kind != release.KindPromote {
		t.Fatalf("promote must append through the store, got bound=%v %+v err=%v", bound, cur, err)
	}
}

// `forge env promote --rollback` records a rollback entry, and refuses one to a
// release the env never ran — end to end through the command's run function.
func TestRunPromote_Rollback(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	for _, v := range []string{"v1", "v2", "v3"} {
		if err := WriteRelease(dir, ociRelease(v, map[string]string{"api": sha(v[1:])})); err != nil {
			t.Fatal(err)
		}
	}
	run := func(v string, kind release.PromotionKind) error {
		var err error
		captureStdout(t, func() {
			err = runPromote(context.Background(), v, "prod", promoteOptions{ProjectDir: dir, Kind: kind, Note: "why", Git: allCommitsPresent()})
		})
		return err
	}
	if err := run("v1", release.KindPromote); err != nil {
		t.Fatal(err)
	}
	if err := run("v2", release.KindPromote); err != nil {
		t.Fatal(err)
	}
	if err := run("v3", release.KindRollback); !errors.Is(err, release.ErrNeverPromoted) {
		t.Fatalf("rollback to never-run v3 must be refused with ErrNeverPromoted, got %v", err)
	}
	if err := run("v1", release.KindRollback); err != nil {
		t.Fatalf("rollback to v1: %v", err)
	}
	history, _ := newFileBindingStore(dir).History("prod")
	got := make([]string, 0, len(history))
	for _, p := range history {
		got = append(got, p.Release+":"+string(p.Kind))
	}
	if s := strings.Join(got, ","); s != "v1:promote,v2:promote,v1:rollback" {
		t.Errorf("ledger = %s, want v1:promote,v2:promote,v1:rollback", s)
	}
}
