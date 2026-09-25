package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/pkg/release"
)

// fakeDeployService is an httptest control plane speaking Connect's JSON
// binding for the six ledger RPCs plus ListEnvironments, with the SERVER's
// semantics: CutRelease is idempotent on version and refuses a different
// artifact set (AlreadyExists); Promote/Rollback freeze the pin set from the
// release the server holds and apply release.Decide; responses use proto3 JSON
// names and enum value names.
//
// It is a FAKE of control-plane's handlers, not a mirror of forge's client:
// the request fields it reads are the proto's (environmentId, version,
// artifacts[].variant …), spelled literally here so a typo in the client's
// wire structs fails the test rather than moving with it.
type fakeDeployService struct {
	mu         sync.Mutex
	envs       map[string]string // name → id
	releases   map[string]wireRelease
	promotions map[string][]wirePromotion // env id → oldest first
	calls      []string
	auth       []string

	// The hosted deploy half (EnsureEnvironment / EnsureDeployment /
	// PublishDeploymentConfig / GetStatus). deployments is env id → name →
	// the stored deployment; bodies records every request body in order so
	// a test can assert the wire document.
	deployments map[string]map[string]*fakeDeployment
	bodies      []fakeBody
	// onSecret, when set, sees every SecretStoreService/SetSecret body.
	onSecret func(body map[string]any)
	// imagePushBase overrides the environment reads' imagePushBase; see
	// pushBase.
	imagePushBase string
}

type fakeDeployment struct {
	ID        string
	Tier      string
	Spec      map[string]any
	Published bool
}

type fakeBody struct {
	Path string
	Body map[string]any
}

// pushBase is the imagePushBase every environment read reports: the org's
// admitted registry subtree. The default covers writeHostedProject's
// localhost:5051/acme images; "-" reports none. Called under f.mu.
func (f *fakeDeployService) pushBase() string {
	switch f.imagePushBase {
	case "":
		return "localhost:5051/acme"
	case "-":
		return ""
	default:
		return f.imagePushBase
	}
}

func newFakeDeployService(envs map[string]string) *fakeDeployService {
	return &fakeDeployService{envs: envs, releases: map[string]wireRelease{}, promotions: map[string][]wirePromotion{}}
}

func connectErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": msg})
}

func (f *fakeDeployService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.URL.Path)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.bodies = append(f.bodies, fakeBody{Path: r.URL.Path, Body: body})
	if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
		connectErr(w, http.StatusUnsupportedMediaType, "invalid_argument", "connect json requires POST + application/json")
		return
	}
	str := func(k string) string { s, _ := body[k].(string); return s }
	envName := func(id string) string {
		for n, i := range f.envs {
			if i == id {
				return n
			}
		}
		return ""
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/controlplane.v1.DeployService/ListEnvironments":
		var out []map[string]string
		for n, id := range f.envs {
			if strings.Contains(n, str("search")) {
				out = append(out, map[string]string{"id": id, "name": n, "imagePushBase": f.pushBase()})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"environments": out})

	case "/controlplane.v1.SecretStoreService/SetSecret":
		if envName(str("environmentId")) == "" {
			connectErr(w, http.StatusNotFound, "not_found", "no such environment")
			return
		}
		if f.onSecret != nil {
			f.onSecret(body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"version": 1})

	case "/controlplane.v1.DeployService/EnsureEnvironment":
		spec, _ := body["spec"].(map[string]any)
		name, _ := spec["name"].(string)
		id, ok := f.envs[name]
		if !ok {
			id = "env-" + name + "-id"
			f.envs[name] = id
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"environment": map[string]string{"id": id, "name": name, "namespace": "env-" + id, "imagePushBase": f.pushBase()},
			"created":     !ok,
		})

	case "/controlplane.v1.DeployService/EnsureDeployment":
		envID, name := str("environmentId"), str("name")
		if envName(envID) == "" {
			connectErr(w, http.StatusNotFound, "not_found", "no such environment")
			return
		}
		if f.deployments == nil {
			f.deployments = map[string]map[string]*fakeDeployment{}
		}
		if f.deployments[envID] == nil {
			f.deployments[envID] = map[string]*fakeDeployment{}
		}
		spec, _ := body["spec"].(map[string]any)
		d, ok := f.deployments[envID][name]
		if !ok {
			d = &fakeDeployment{ID: "dep-" + name}
			f.deployments[envID][name] = d
		}
		d.Tier, d.Spec = str("tier"), spec
		_ = json.NewEncoder(w).Encode(map[string]any{
			"deployment": map[string]any{"id": d.ID, "name": name}, "created": !ok, "updated": ok,
		})

	case "/controlplane.v1.DeployService/PublishDeploymentConfig":
		for _, byName := range f.deployments {
			for _, d := range byName {
				if d.ID == str("deploymentId") {
					d.Published = true
					_ = json.NewEncoder(w).Encode(map[string]any{"digest": "sha256:cfg", "reference": "reg/cfg@sha256:cfg"})
					return
				}
			}
		}
		connectErr(w, http.StatusNotFound, "not_found", "no such deployment")

	case "/controlplane.v1.DeployService/GetStatus":
		// A published backend is READY on the digest its spec pins — the
		// control plane's observer confirmed what was published.
		envID := str("environmentId")
		var out []map[string]any
		for name, d := range f.deployments[envID] {
			img, _ := d.Spec["image"].(string)
			digest := ""
			if i := strings.LastIndex(img, "@"); i >= 0 {
				digest = img[i+1:]
			}
			// A static site's release digest is its spec.liveDigest.
			if live, ok := d.Spec["liveDigest"].(string); ok {
				digest = live
			}
			state := "DEPLOY_OBSERVED_STATE_PENDING"
			if d.Published {
				state = "DEPLOY_OBSERVED_STATE_READY"
			}
			out = append(out, map[string]any{
				"deployment": map[string]any{"id": d.ID, "name": name, "tier": d.Tier, "observed": map[string]any{
					"state": state, "imageDigest": digest, "url": "https://" + name + "-acme.reliantapps.dev"}},
				"verdict": "DEPLOY_VERDICT_CONVERGING", "verdictReason": "within the stability window", "desiredDigest": digest,
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"deployments": out, "environmentVerdict": "DEPLOY_VERDICT_CONVERGING", "reconcilePolicy": "DEPLOY_RECONCILE_POLICY_OBSERVE",
		})

	case "/controlplane.v1.DeployService/CutRelease":
		var req struct {
			Version   string         `json:"version"`
			Artifacts []wireArtifact `json:"artifacts"`
			GitCommit string         `json:"gitCommit"`
		}
		_ = json.Unmarshal(raw, &req)
		want := wireRelease{ID: "rel-" + req.Version, Version: req.Version, GitCommit: req.GitCommit,
			Artifacts: req.Artifacts, CreatedAt: time.Date(2026, 9, 23, 0, len(f.releases), 0, 0, time.UTC)}
		wantRel, err := releaseFromWire(want)
		if err != nil {
			connectErr(w, http.StatusBadRequest, "invalid_argument", err.Error())
			return
		}
		if old, ok := f.releases[req.Version]; ok {
			oldRel, _ := releaseFromWire(old)
			if release.CheckRecut(oldRel, wantRel) != nil {
				connectErr(w, http.StatusConflict, "already_exists", "that version has already been cut")
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"release": old, "created": false})
			return
		}
		f.releases[req.Version] = want
		_ = json.NewEncoder(w).Encode(map[string]any{"release": want, "created": true})

	case "/controlplane.v1.DeployService/GetRelease":
		rel, ok := f.releases[str("version")]
		if !ok {
			connectErr(w, http.StatusNotFound, "not_found", "release or environment")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"release": rel})

	case "/controlplane.v1.DeployService/ListReleases":
		var out []wireRelease
		for _, rel := range f.releases {
			out = append(out, rel)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"releases": out})

	case "/controlplane.v1.DeployService/Promote", "/controlplane.v1.DeployService/Rollback":
		envID, version := str("environmentId"), str("version")
		rel, ok := f.releases[version]
		if !ok {
			connectErr(w, http.StatusBadRequest, "failed_precondition", "that version has not been built")
			return
		}
		kind := release.KindPromote
		if strings.HasSuffix(r.URL.Path, "/Rollback") {
			kind = release.KindRollback
		}
		relDomain, _ := releaseFromWire(rel)
		var history []release.Promotion
		for _, p := range f.promotions[envID] {
			k, _ := promotionKindFromWire(p.Kind)
			history = append(history, release.Promotion{Env: envID, Release: p.ReleaseVersion, Kind: k})
		}
		existing, err := release.Decide(history, release.Promotion{Env: envID, Release: version, Kind: kind})
		if errors.Is(err, release.ErrNeverPromoted) {
			connectErr(w, http.StatusBadRequest, "failed_precondition",
				"that version has never run in this environment; promote it instead of rolling back to it")
			return
		}
		if existing != nil {
			list := f.promotions[envID]
			_ = json.NewEncoder(w).Encode(map[string]any{"promotion": list[len(list)-1]})
			return
		}
		wk := wireKindPromote
		if kind == release.KindRollback {
			wk = wireKindRollback
		}
		p := wirePromotion{
			ID: fmt.Sprintf("promo-%d", len(f.promotions[envID])+1), EnvironmentID: envID, ReleaseID: rel.ID,
			ReleaseVersion: version, Kind: wk, ResolvedArtifacts: relDomain.SharedDigests(),
			PromotedByActor: str("promotedByActor"), Note: str("note"),
			CreatedAt: time.Date(2026, 9, 23, 1, len(f.promotions[envID]), 0, 0, time.UTC),
		}
		if fromID := str("fromEnvironmentId"); fromID != "" {
			p.FromEnvironmentID = fromID
		}
		f.promotions[envID] = append(f.promotions[envID], p)
		_ = envName
		_ = json.NewEncoder(w).Encode(map[string]any{"promotion": p})

	case "/controlplane.v1.DeployService/ListPromotions":
		list := f.promotions[str("environmentId")]
		out := make([]wirePromotion, 0, len(list))
		for i := len(list) - 1; i >= 0; i-- { // newest first
			out = append(out, list[i])
		}
		if lim, ok := body["limit"].(float64); ok && int(lim) < len(out) {
			out = out[:int(lim)]
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"promotions": out})

	default:
		connectErr(w, http.StatusNotFound, "unimplemented", "no such procedure "+r.URL.Path)
	}
}

func (f *fakeDeployService) callCount(proc string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == "/"+proc {
			n++
		}
	}
	return n
}

func newHostedTestStore(t *testing.T, fake *fakeDeployService) (*hostedStore, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	ep := cloud.Endpoint{Env: "prod", URL: srv.URL, TokenEnv: "T"}
	l := hostedLedger(cloud.NewClient(ep, cloud.Credential{Token: "rlat_test"}), srv.URL)
	return l.Bindings.(*hostedStore), srv
}

// The hosted store against an httptest Connect server: cut, promote, retry,
// rollback (refused, then accepted), and Current — with the pin set coming
// from the SERVER, not from what the client sent.
func TestHostedStore_LedgerRoundTrip(t *testing.T) {
	fake := newFakeDeployService(map[string]string{"prod": "env-prod-uuid", "prod-eu": "env-prod-eu"})
	store, _ := newHostedTestStore(t, fake)
	ctx := context.Background()

	if _, bound, err := store.Current(ctx, "prod"); err != nil || bound {
		t.Fatalf("a fresh env must be unbound: bound=%v err=%v", bound, err)
	}

	v1 := ociRelease("v1", map[string]string{"api": sha("1")})
	v1.Artifacts["web"] = release.Artifact{Kind: release.KindGit, Mode: release.ModeSource,
		Source: &release.Source{Repo: "github.com/acme/web", Ref: "v1", Subdir: "web", Commit: "abc"}}
	if created, err := store.Cut(ctx, v1); err != nil || !created {
		t.Fatalf("cut v1: created=%v err=%v", created, err)
	}
	if created, err := store.Cut(ctx, v1); err != nil || created {
		t.Fatalf("an identical re-cut is a retry: created=%v err=%v", created, err)
	}
	if _, err := store.Cut(ctx, ociRelease("v1", map[string]string{"api": sha("9")})); !errors.Is(err, release.ErrReleaseConflict) {
		t.Fatalf("a different set under v1 must be ErrReleaseConflict, got %v", err)
	}
	for _, v := range []string{"v2", "v3"} {
		if _, err := store.Cut(ctx, ociRelease(v, map[string]string{"api": sha(v[1:])})); err != nil {
			t.Fatal(err)
		}
	}

	// Round trip through GetRelease: the source pin and kinds survive the
	// one-row-per-(name, variant) wire shape.
	got, err := store.Get(ctx, "v1")
	if err != nil || got == nil {
		t.Fatalf("get v1: %v", err)
	}
	if !got.SameContent(v1) {
		t.Errorf("release did not round-trip:\n got %+v\nwant %+v", got.Artifacts, v1.Artifacts)
	}
	if missing, err := store.Get(ctx, "v404"); err != nil || missing != nil {
		t.Errorf("a never-cut version is (nil, nil), got %v %v", missing, err)
	}

	// The client sends a BOGUS Resolved map; the server must ignore it and
	// freeze from the release it holds.
	p, err := store.Append(ctx, release.Promotion{Env: "prod", Release: "v1", Kind: release.KindPromote,
		Resolved: map[string]string{"api": sha("f")}})
	if err != nil {
		t.Fatalf("promote v1: %v", err)
	}
	if p.Resolved["api"] != sha("1") || p.Env != "prod" || p.Kind != release.KindPromote || p.Release != "v1" {
		t.Errorf("promotion must carry the server-frozen pin and forge names, got %+v", p)
	}
	again, err := store.Append(ctx, release.Promotion{Env: "prod", Release: "v1", Kind: release.KindPromote})
	if err != nil || again.ID != p.ID {
		t.Fatalf("a retry must return the existing entry %s, got %+v %v", p.ID, again, err)
	}
	if _, err := store.Append(ctx, release.Promotion{Env: "prod", Release: "v2", Kind: release.KindPromote}); err != nil {
		t.Fatal(err)
	}
	// v3 was CUT but never ran in prod: a rollback to it is refused.
	if _, err := store.Append(ctx, release.Promotion{Env: "prod", Release: "v3", Kind: release.KindRollback}); !errors.Is(err, release.ErrNeverPromoted) {
		t.Fatalf("rollback to a never-run release must be ErrNeverPromoted, got %v", err)
	}
	rb, err := store.Append(ctx, release.Promotion{Env: "prod", Release: "v1", Kind: release.KindRollback, Note: "5xx"})
	if err != nil || rb.Kind != release.KindRollback {
		t.Fatalf("rollback to v1: %+v %v", rb, err)
	}

	cur, bound, err := store.Current(ctx, "prod")
	if err != nil || !bound || cur.Release != "v1" || cur.Kind != release.KindRollback || cur.Resolved["api"] != sha("1") {
		t.Fatalf("current must be the rollback to v1, got bound=%v %+v err=%v", bound, cur, err)
	}
	// Exactly one env lookup per env name: the id is cached per process.
	if n := fake.callCount("controlplane.v1.DeployService/ListEnvironments"); n != 1 {
		t.Errorf("ListEnvironments called %d times, want 1 (cached)", n)
	}
	for _, a := range fake.auth {
		if a != "Bearer rlat_test" {
			t.Fatalf("every call must carry the bearer token, saw %q", a)
		}
	}
	if len(fake.promotions["env-prod-eu"]) != 0 {
		t.Error("prod-eu (a search near-miss) must never be written")
	}
}

// An env the control plane has never heard of has never been promoted — a
// read answers unbound and creates nothing. Every WRITE ensures the env by
// name first (ensureHostedEnv), so the first PROMOTE of a fresh hosted env
// works; a ROLLBACK is still refused — by the ledger's never-promoted rule.
// Mutation: dropping the ensure in Append fails the promote half.
func TestHostedStore_UnknownEnvironment(t *testing.T) {
	fake := newFakeDeployService(map[string]string{})
	fake.releases["v1"] = wireRelease{ID: "rel-1", Version: "v1", Artifacts: []wireArtifact{
		{Name: "api", Digest: "sha256:" + strings.Repeat("a", 64), Kind: "oci", Mode: "shared", Variant: release.SharedVariant},
	}}
	store, _ := newHostedTestStore(t, fake)
	if _, bound, err := store.Current(context.Background(), "prod"); err != nil || bound {
		t.Fatalf("read of an unknown env must be unbound, got bound=%v err=%v", bound, err)
	}
	_, err := store.Append(context.Background(), release.Promotion{Env: "staging", Release: "v1", Kind: release.KindRollback})
	if !errors.Is(err, release.ErrNeverPromoted) {
		t.Fatalf("a rollback of a fresh env must be refused as never-promoted, got %v", err)
	}
	p, err := store.Append(context.Background(), release.Promotion{Env: "prod", Release: "v1", Kind: release.KindPromote})
	if err != nil {
		t.Fatalf("first promote of a fresh hosted env: %v", err)
	}
	if p.Release != "v1" || fake.envs["prod"] == "" {
		t.Fatalf("promote did not ensure the env: promotion=%+v envs=%v", p, fake.envs)
	}
	if _, bound, err := store.Current(context.Background(), "prod"); err != nil || !bound {
		t.Fatalf("after the first promote the env must be bound, got bound=%v err=%v", bound, err)
	}
}

// An unknown promotion kind from the server is refused, never read as a
// forward promote.
func TestPromotionKindFromWire_Closed(t *testing.T) {
	if _, err := promotionKindFromWire("DEPLOY_PROMOTION_KIND_UNSPECIFIED"); err == nil {
		t.Fatal("UNSPECIFIED must be refused")
	}
}

// ─── End to end through the command tree ─────────────────────────────────────

// `forge release cut v1 --env prod` → `forge env promote v1 --to prod` →
// `forge cloud releases prod`, for an env whose KCL declares forge.ControlPlane,
// against the fake control plane. Nothing is written to the project's ledger
// files: the env's declaration routed every write to the control plane.
func TestHostedLedger_CutPromoteListEndToEnd(t *testing.T) {
	fake := newFakeDeployService(map[string]string{"prod": "env-prod-uuid"})
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte("name: demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	declareEnvDir(t, dir, "prod")
	t.Chdir(dir)
	t.Setenv("FORGE_E2E_CP_TOKEN", "rlat_e2e")
	t.Setenv("FORGE_KCL_RENDER_FIXTURE", writeKCLFixture(t, fmt.Sprintf(
		`{"control_plane":{"type":"control_plane","endpoint":%q,"token_env":"FORGE_E2E_CP_TOKEN"},"services":[{"name":"api","image":"api","deploy":{"type":"cluster","cluster":"c","namespace":"n"}}]}`, srv.URL)))
	// The build state an earlier `forge build prod --push` left behind.
	if err := WriteBuildState(dir, "prod", BuildState{
		Image: "api", Tag: "v1", Pushed: true, PushedAt: nowRFC3339(), Digest: sha("1"), Registry: "ghcr.io/acme",
	}); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) (string, error) {
		root := NewRootCmd()
		var buf bytes.Buffer
		root.SetOut(&buf)
		root.SetErr(&buf)
		root.SetArgs(args)
		var err error
		out := captureStdout(t, func() { err = root.Execute() })
		return out + buf.String(), err
	}

	if out, err := run("release", "cut", "v1", "--env", "prod"); err != nil {
		t.Fatalf("forge release cut: %v\n%s", err, out)
	}
	if out, err := run("env", "promote", "v1", "--to", "prod", "--actor", "ci"); err != nil {
		t.Fatalf("forge env promote: %v\n%s", err, out)
	}
	out, err := run("cloud", "releases", "prod", "--json")
	if err != nil {
		t.Fatalf("forge cloud releases: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"version": "v1"`) {
		t.Errorf("cloud releases must list v1, got:\n%s", out)
	}

	if n := fake.callCount(procCutRelease); n != 1 {
		t.Errorf("CutRelease calls = %d, want 1", n)
	}
	if n := fake.callCount(procPromote); n != 1 {
		t.Errorf("Promote calls = %d, want 1", n)
	}
	list := fake.promotions["env-prod-uuid"]
	if len(list) != 1 || list[0].ReleaseVersion != "v1" || list[0].ResolvedArtifacts["api"] != sha("1") || list[0].PromotedByActor != "ci" {
		t.Fatalf("the control plane must hold prod→v1 pinned to the pushed digest, got %+v", list)
	}
	for _, p := range []string{releasesDirRel, promotionsDirRel} {
		if _, err := os.Stat(filepath.Join(dir, p)); !os.IsNotExist(err) {
			t.Errorf("%s must not exist — a hosted env's ledger is the control plane (stat err %v)", p, err)
		}
	}

	// And deploy resolves the digest from the SAME ledger.
	bindings, err := bindingStoreFor(context.Background(), dir, "prod")
	if err != nil {
		t.Fatal(err)
	}
	digests, bound, err := resolveDeployDigests(context.Background(), dir, "prod", false, bindings)
	if err != nil || bound != "v1" || digests["api"] != sha("1") {
		t.Fatalf("deploy must pin from the hosted ledger: rel=%q digests=%v err=%v", bound, digests, err)
	}
}
