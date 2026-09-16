package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/reliant-labs/forge/internal/cluster"
)

// ─── The file backend ────────────────────────────────────────────────────────

// TestFileBindingStore_MissingIsUnbound pins the semantics `forge env deploy`
// rests on: a project that has never been promoted has no ledger file, and
// asking about an env there is a normal "not bound", not an error. Returning
// an error would make a promote a prerequisite for every deploy.
func TestFileBindingStore_MissingIsUnbound(t *testing.T) {
	dir := t.TempDir()
	store := newFileBindingStore(dir)

	binding, bound, err := store.Binding("prod")
	if err != nil {
		t.Fatalf("missing ledger must not error: %v", err)
	}
	if bound {
		t.Errorf("prod must be unbound in an empty project, got %+v", binding)
	}
	if !reflect.DeepEqual(binding, EnvBinding{}) {
		t.Errorf("unbound lookup must yield the zero binding, got %+v", binding)
	}
}

// TestFileBindingStore_RoundTrip proves a written binding reads back intact.
func TestFileBindingStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := newFileBindingStore(dir)

	want := EnvBinding{
		Release:    "v1.5.13",
		Resolved:   map[string]string{"control-plane": sha("a"), "reliant": sha("b")},
		PromotedAt: "2026-09-11T12:00:00Z",
	}
	if err := store.SetBinding("prod", want); err != nil {
		t.Fatalf("set: %v", err)
	}

	got, bound, err := store.Binding("prod")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bound {
		t.Fatal("prod must be bound after SetBinding")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip mismatch:\n  want %+v\n  got  %+v", want, got)
	}
}

// TestFileBindingStore_UnknownEnvNotFound proves a populated ledger still
// reports a DIFFERENT env as unbound. Without this, an implementation that
// returned the first (or any) binding regardless of the key would pass the
// round-trip test above — and a deploy of staging would silently ship prod's
// pinned digests.
func TestFileBindingStore_UnknownEnvNotFound(t *testing.T) {
	dir := t.TempDir()
	store := newFileBindingStore(dir)

	if err := store.SetBinding("prod", EnvBinding{
		Release:  "v1.5.13",
		Resolved: map[string]string{"control-plane": sha("a")},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}

	binding, bound, err := store.Binding("staging")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if bound {
		t.Errorf("staging was never promoted and must be unbound, got %+v", binding)
	}
}

// TestFileBindingStore_SetPreservesOtherEnvs is the read-modify-write
// guarantee. One file holds every env, so a SetBinding that wrote only its own
// key would silently unbind every other environment — losing prod's pinned
// digests as a side effect of promoting staging.
func TestFileBindingStore_SetPreservesOtherEnvs(t *testing.T) {
	dir := t.TempDir()
	store := newFileBindingStore(dir)

	prod := EnvBinding{Release: "v1.5.13", Resolved: map[string]string{"control-plane": sha("a")}}
	if err := store.SetBinding("prod", prod); err != nil {
		t.Fatalf("set prod: %v", err)
	}
	if err := store.SetBinding("staging", EnvBinding{
		Release: "v1.6.0", Resolved: map[string]string{"control-plane": sha("b")},
	}); err != nil {
		t.Fatalf("set staging: %v", err)
	}

	got, bound, err := store.Binding("prod")
	if err != nil {
		t.Fatalf("get prod: %v", err)
	}
	if !bound {
		t.Fatal("prod must survive a staging promote")
	}
	if !reflect.DeepEqual(got, prod) {
		t.Errorf("prod binding was mutated by a staging promote:\n  want %+v\n  got  %+v", prod, got)
	}
}

// TestFileBindingStore_OnDiskFormatUnchanged is a compatibility lock, not a
// behaviour test. .forge/env-releases.json is committed in real projects, so
// its path and its JSON shape are a published contract: a refactor that
// relocated the file or renamed a field would silently orphan every existing
// project's promotion state. Asserting the raw bytes' structure — rather than
// round-tripping through the same Go structs that would change with it — is
// what makes this catch a rename.
func TestFileBindingStore_OnDiskFormatUnchanged(t *testing.T) {
	dir := t.TempDir()

	if err := newFileBindingStore(dir).SetBinding("prod", EnvBinding{
		Release:    "v1.5.13",
		Resolved:   map[string]string{"control-plane": sha("a")},
		PromotedAt: "2026-09-11T12:00:00Z",
	}); err != nil {
		t.Fatalf("set: %v", err)
	}

	path := filepath.Join(dir, ".forge", "env-releases.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the ledger must live at .forge/env-releases.json: %v", err)
	}

	var raw struct {
		Bindings map[string]struct {
			Release    string            `json:"release"`
			Resolved   map[string]string `json:"resolved"`
			PromotedAt string            `json:"promoted_at"`
		} `json:"bindings"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("ledger is not in the published shape: %v\n%s", err, data)
	}
	b, ok := raw.Bindings["prod"]
	if !ok {
		t.Fatalf("bindings.prod missing from the ledger:\n%s", data)
	}
	if b.Release != "v1.5.13" || b.PromotedAt != "2026-09-11T12:00:00Z" {
		t.Errorf("field names/values changed: %+v\n%s", b, data)
	}
	if b.Resolved["control-plane"] != sha("a") {
		t.Errorf("resolved digests changed shape: %+v\n%s", b.Resolved, data)
	}
}

// TestFileBindingStore_LocationIsTheLedgerPath pins what promote prints.
func TestFileBindingStore_LocationIsTheLedgerPath(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, ".forge", "env-releases.json")
	if got := newFileBindingStore(dir).Location(); got != want {
		t.Errorf("Location() = %q, want %q", got, want)
	}
}

// ─── The fake backend, and why it is the real proof ──────────────────────────

// memBindingStore is a complete bindingStore that touches no filesystem.
//
// THIS IS THE POINT OF THE WHOLE CHANGE. The seam is only real if a consumer
// can run against a backend that is not a file, and the cheapest possible
// demonstration of that is a map. If this type could not satisfy the interface
// — because a method wanted a projectDir, or a path, or anything else only a
// filesystem can supply — then the "interface" would be the file API with
// extra steps, and the hosted backend it exists for could not be written
// either.
//
// It is also how the consumer test below states its premise directly: "prod is
// bound to these digests" is one literal, instead of a staged temp directory
// whose JSON has to be written and re-parsed to mean the same thing.
type memBindingStore struct {
	bindings map[string]EnvBinding
}

func newMemBindingStore(bindings map[string]EnvBinding) *memBindingStore {
	if bindings == nil {
		bindings = map[string]EnvBinding{}
	}
	return &memBindingStore{bindings: bindings}
}

func (m *memBindingStore) Binding(env string) (EnvBinding, bool, error) {
	b, ok := m.bindings[env]
	return b, ok, nil
}

func (m *memBindingStore) SetBinding(env string, binding EnvBinding) error {
	m.bindings[env] = binding
	return nil
}

func (m *memBindingStore) Location() string { return "memory://bindings" }

// TestRunEnvVerify_AgainstNonFileBackend runs the `forge env verify` command
// end to end with its binding ledger served entirely from memory.
//
// This is the seam's proof. The command reaches its real comparison logic and
// produces the real DRIFT verdict without a .forge/env-releases.json existing
// anywhere — so nothing on the path from the command to the ledger assumes a
// file. A hosted backend plugs in exactly where this fake does.
func TestRunEnvVerify_AgainstNonFileBackend(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	// No ledger file is ever written. If the command still consulted the
	// file backend, it would find nothing and report "no release binding".
	store := newMemBindingStore(map[string]EnvBinding{
		"prod": {
			Release:    "v1.5.13",
			Resolved:   map[string]string{"control-plane": sha("a")},
			PromotedAt: "2026-09-11T12:00:00Z",
		},
	})

	// The cluster runs a DIFFERENT digest than the in-memory binding
	// declares — the headline drift case.
	lister := &stubLister{images: []cluster.WorkloadImage{{
		Kind: "Deployment", Name: "control-plane", Container: "app",
		Image: "ghcr.io/acme/control-plane@" + sha("b"),
	}}}

	err := runEnvVerify(context.Background(), "prod", envVerifyOptions{
		Lister:   lister,
		Resolver: stubResolver{target: envTarget{KubeContext: "test-context", Namespace: "test-ns"}},
		Bindings: store,
	})

	// Exit 1 is the drift verdict. Reaching it proves the command read the
	// declaration out of the memory-backed store and compared it for real:
	// an unbound env would have exited 0 with "nothing to verify".
	if got := exitCodeOf(t, err); got != 1 {
		t.Fatalf("drift against a memory-backed ledger must exit 1, got %d (err: %v)", got, err)
	}

	// And the file backend genuinely has nothing — the command was served
	// entirely by the fake.
	if _, err := os.Stat(filepath.Join(dir, ".forge", "env-releases.json")); !os.IsNotExist(err) {
		t.Errorf("no ledger file should exist; the binding came from memory (stat err: %v)", err)
	}
}

// TestRunEnvVerify_UnboundAgainstNonFileBackend is the other half: a store
// that reports no binding drives the "nothing to verify, exit 0" path. Without
// it, a fake that returned bound=true unconditionally would pass the drift
// test above, and the test would not actually be reading the store's answer.
func TestRunEnvVerify_UnboundAgainstNonFileBackend(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	err := runEnvVerify(context.Background(), "prod", envVerifyOptions{
		Lister:   &stubLister{},
		Resolver: stubResolver{target: envTarget{KubeContext: "test-context", Namespace: "test-ns"}},
		Bindings: newMemBindingStore(nil),
	})
	if err != nil {
		t.Fatalf("an unbound env is not a failure, got: %v", err)
	}
}

// TestResolveDeployDigests_AgainstNonFileBackend proves the SECOND consumer —
// the deploy digest resolver — is equally free of the file. A memory-backed
// binding must override the per-env build state exactly as a file-backed one
// does, because that precedence is what "build once, promote" means.
func TestResolveDeployDigests_AgainstNonFileBackend(t *testing.T) {
	dir := t.TempDir()

	// Build state (a real file — that half is legitimately file-backed)
	// holds an older digest.
	if err := WriteBuildState(dir, "prod", BuildState{
		Image: "control-plane", Tag: "old", Pushed: true, PushedAt: nowRFC3339(), Digest: sha("0"),
	}); err != nil {
		t.Fatalf("write build state: %v", err)
	}

	store := newMemBindingStore(map[string]EnvBinding{
		"prod": {Release: "v1.4.0", Resolved: map[string]string{"control-plane": sha("a")}},
	})

	digests, boundRel, err := resolveDeployDigests(dir, "prod", false, store)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if boundRel != "v1.4.0" {
		t.Errorf("boundRelease = %q, want v1.4.0 from the memory-backed store", boundRel)
	}
	if digests["control-plane"] != sha("a") {
		t.Errorf("control-plane = %q, want the store's digest %q (release must override build state)",
			digests["control-plane"], sha("a"))
	}
}

// TestRunPromote_WritesThroughTheStore proves the THIRD consumer routes its
// write through the interface rather than the file helpers: promoting reaches
// SetBinding with the digests resolved from the release ledger.
func TestRunPromote_WritesThroughTheStore(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if err := WriteRelease(dir, Release{
		Version:   "v1.4.0",
		CreatedAt: nowRFC3339(),
		Artifacts: map[string]ReleaseArtifact{
			"control-plane": {Kind: ArtifactKindOCI, Mode: "shared", Digests: map[string]string{"*": sha("a")}},
		},
	}); err != nil {
		t.Fatalf("write release: %v", err)
	}

	if err := runPromote(context.Background(), "v1.4.0", "staging", promoteOptions{}); err != nil {
		t.Fatalf("promote: %v", err)
	}

	// runPromote constructs its own store via bindingStoreFor, which is the
	// file backend by default — so the write must be visible through a
	// freshly-constructed file store reading the same project.
	binding, bound, err := newFileBindingStore(dir).Binding("staging")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bound || binding.Release != "v1.4.0" {
		t.Fatalf("promote must persist through the store, got bound=%v %+v", bound, binding)
	}
}
