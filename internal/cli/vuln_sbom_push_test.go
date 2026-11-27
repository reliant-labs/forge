package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/internal/ledgerfile"
	"github.com/reliant-labs/forge/internal/schemagen"
	"github.com/reliant-labs/forge/pkg/release"
)

// cdxDoc is a CycloneDX document naming component name at version, the shape
// forge's generator writes (the artifact's release key, and the digest syft
// read).
func cdxDoc(name, version, serial string) []byte {
	return []byte(`{"bomFormat":"CycloneDX","specVersion":"1.6","serialNumber":"urn:uuid:` + serial + `","version":1,` +
		`"metadata":{"timestamp":"2026-11-25T18:08:00Z","tools":{"components":[{"type":"application","name":"syft","version":"1.57.2"}]},` +
		`"component":{"type":"container","name":"` + name + `","version":"` + version + `"}},"components":[{"name":"busybox","version":"1.36"}]}`)
}

// pushHarness is a control plane (fake behind a fakeDeployService) holding
// release v1.0.0, cut with no SBOM uploaded, and the machine ledger the
// push reads.
type pushHarness struct {
	t     *testing.T
	fake  *fakeVulnService
	deps  vulnDeps
	store *ledgerfile.Store
	rel   release.Release
}

const (
	pushImage  = "registry.test/acme/api"
	pushSite   = "registry.test/acme/web/static.v1"
	pushMaps   = "web.sourcemaps"
	pushCarry  = "registry.test/acme/otelcol"
	pushNoSBOM = "registry.test/acme/legacy"
)

func newPushHarness(t *testing.T, fake *fakeVulnService) *pushHarness {
	t.Helper()
	h := hostedVulnHarness(t, fake)
	dep := newFakeDeployService(map[string]string{"prod": "env-prod-id"})
	fake.next = dep
	oci := func(c string) release.Artifact {
		return release.Artifact{Kind: release.KindOCI, Mode: release.ModeShared, Digests: map[string]string{release.SharedVariant: sha(c)}}
	}
	rel := release.Release{Version: "v1.0.0", Project: "acme", Artifacts: map[string]release.Artifact{
		pushImage: oci("1"), pushSite: oci("2"), pushMaps: oci("3"), pushCarry: oci("4"), pushNoSBOM: oci("5"),
	}}
	// The release is cut on the control plane without its SBOMs, as an
	// older control plane took it: its document names none.
	srv := httptest.NewServer(dep)
	t.Cleanup(srv.Close)
	cutter := hostedLedger(cloud.NewClient(cloud.Endpoint{Env: "prod", URL: srv.URL}, cloud.Credential{Token: "rlat_test"}), srv.URL, "acme", deploytarget.HostedEnvPersistent)
	cutter.Bindings.(*hostedStore).warn = io.Discard
	if _, err := cutter.Releases.Cut(context.Background(), rel); err != nil {
		t.Fatal(err)
	}
	h.deps.listEnvs = func(string) ([]string, error) { return []string{"prod"}, nil }
	return &pushHarness{t: t, fake: fake, deps: h.deps, store: h.store, rel: rel}
}

// put stores an SBOM in the machine ledger and returns its digest.
func (h *pushHarness) put(doc []byte) string {
	h.t.Helper()
	d, err := h.store.PutSBOM(doc)
	if err != nil {
		h.t.Fatal(err)
	}
	return d
}

// index records refs as the cut's index of v1.0.0.
func (h *pushHarness) index(refs map[string]string) {
	h.t.Helper()
	if err := h.store.UpdateReleaseSBOMIndex("v1.0.0", func(x *ledgerfile.ReleaseSBOMIndex) error {
		env, err := h.deps.resolveEnv(context.Background(), "", "prod")
		if err != nil {
			return err
		}
		client, err := h.deps.hostedClient(context.Background(), env)
		if err != nil {
			return err
		}
		target, err := resolveSBOMTarget(context.Background(), client, env.Endpoint, "acme")
		if err != nil {
			return err
		}
		x.Target = target
		x.Artifacts = map[string]release.SBOMRef{}
		for name, d := range refs {
			x.Artifacts[name] = release.SBOMRef{Digest: d, Status: release.SBOMStatusOK, Format: release.SBOMFormatCycloneDXJSON}
		}
		return nil
	}); err != nil {
		h.t.Fatal(err)
	}
}

// push runs `forge vuln sbom push --release v1.0.0 --json` with extra flags
// and returns the validated document and the exit code.
func (h *pushHarness) push(sboms ...string) (vulnSBOMPushDocument, int) {
	h.t.Helper()
	var out bytes.Buffer
	err := runVulnSBOMPush(context.Background(), &out, h.deps, vulnSBOMPushOptions{release: "v1.0.0", sboms: sboms, asJSON: true})
	code := 0
	if err != nil {
		code = 1
		var ec *exitCodeError
		if errors.As(err, &ec) {
			code = ec.code
		}
	}
	var doc vulnSBOMPushDocument
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		h.t.Fatalf("not a vuln-sbom-push document: %v\n%s", err, out.String())
	}
	var schema bytes.Buffer
	if err := runProjectSchema(&schema, "vuln-sbom-push"); err != nil {
		h.t.Fatal(err)
	}
	compiled, err := schemagen.Compile(schema.Bytes())
	if err != nil {
		h.t.Fatal(err)
	}
	inst, err := schemagen.DecodeJSON(out.Bytes())
	if err != nil {
		h.t.Fatal(err)
	}
	if errs := compiled.Validate(inst); len(errs) > 0 {
		h.t.Fatalf("document does not validate against forge project schema vuln-sbom-push: %v\n%s", errs, out.String())
	}
	if doc.ExitCode != code {
		h.t.Fatalf("document exit_code %d, command exited %d", doc.ExitCode, code)
	}
	return doc, code
}

func statusOf(doc vulnSBOMPushDocument, name string) sbomPushArtifact {
	for _, a := range doc.Artifacts {
		if a.Artifact == name {
			return a
		}
	}
	return sbomPushArtifact{}
}

// The index a cut wrote names every SBOM; a push sends each once, and a
// second push sends them again and is told they are already recorded.
func TestVulnSBOMPushIsIdempotent(t *testing.T) {
	t.Parallel()
	h := newPushHarness(t, &fakeVulnService{features: []string{hostedVulnFeatureSBOMUpload}, idempotent: true})
	api := h.put(cdxDoc(pushImage, sha("1"), "a"))
	site := h.put(cdxDoc(pushSite, sha("9"), "s"))
	carry := h.put(cdxDoc(pushCarry, sha("4"), "c"))
	h.index(map[string]string{pushImage: api, pushSite: site, pushCarry: carry})

	doc, code := h.push()
	if code != exitUndetermined || doc.Summary.Sent != 3 || doc.Summary.NotApplicable != 1 || doc.Summary.Missing != 1 {
		t.Fatalf("first push: exit %d, summary %+v; want 3 sent, the source maps not applicable, legacy missing (exit 2)", code, doc.Summary)
	}
	if a := statusOf(doc, pushImage); a.Status != sbomPushSent || a.From != sbomFromIndex || a.SBOMDigest != api {
		t.Errorf("api: %+v", a)
	}
	if a := statusOf(doc, pushNoSBOM); !a.NeedsOverride || a.Status != sbomPushMissing {
		t.Errorf("legacy: %+v; want missing, needs an override", a)
	}
	if a := statusOf(doc, pushMaps); a.Status != sbomPushNotApplicable || !strings.Contains(a.Reason, "source maps") {
		t.Errorf("source maps: %+v", a)
	}
	if doc.Endpoint == "" || doc.Env != "prod" || doc.Schema != vulnSBOMPushSchema {
		t.Errorf("document header: %+v", doc)
	}

	doc, _ = h.push()
	if doc.Summary.Already != 3 || doc.Summary.Sent != 0 {
		t.Fatalf("second push: summary %+v; want the three already recorded", doc.Summary)
	}
	if n := h.fake.count(procRecordReleaseSBOM); n != 6 {
		t.Errorf("RecordReleaseSBOM called %d times, want 6 (every SBOM, twice)", n)
	}
	// The index records the upload on this endpoint as done.
	x, err := h.store.ReleaseSBOMIndex("v1.0.0")
	if err != nil || x == nil {
		t.Fatal(x, err)
	}
	for ep, up := range x.Uploads {
		if up.State != ledgerfile.SBOMUploadDone || up.Artifacts[pushImage] != sbomPushAlready {
			t.Errorf("upload on %s: %+v", ep, up)
		}
	}
}

// A control plane that says which SBOMs it records is not sent them again;
// one that holds a different SBOM keeps it, which is reported, not a failure.
func TestVulnSBOMPushReadsTheControlPlanesRecordAndReportsDiffers(t *testing.T) {
	t.Parallel()
	fake := &fakeVulnService{features: []string{hostedVulnFeatureFindings}, idempotent: true}
	h := newPushHarness(t, fake)
	api := h.put(cdxDoc(pushImage, sha("1"), "a"))
	carry := h.put(cdxDoc(pushCarry, sha("4"), "c"))
	site := h.put(cdxDoc(pushSite, sha("9"), "s"))
	legacy := h.put(cdxDoc(pushNoSBOM, sha("5"), "l"))
	h.index(map[string]string{pushCarry: carry, pushSite: site, pushNoSBOM: legacy})
	fake.releaseVulns = map[string]map[string]any{"v1.0.0": {"releaseVersion": "v1.0.0", "artifacts": []map[string]any{
		{"artifactName": pushImage, "sbomDigest": api},
	}}}
	// The control plane already holds another SBOM for the static site.
	fake.held = map[string]string{"v1.0.0\x00" + pushSite: sha("d")}

	doc, code := h.push()
	if code != 0 {
		t.Fatalf("exit %d: %+v", code, doc)
	}
	if a := statusOf(doc, pushImage); a.Status != sbomPushAlready || a.From != sbomFromControlPlane || a.RecordedDigest != api {
		t.Errorf("api: %+v; want already recorded, from the control plane's record", a)
	}
	if a := statusOf(doc, pushSite); a.Status != sbomPushDiffers || a.SBOMDigest != site {
		t.Errorf("site: %+v; want differs", a)
	}
	for _, rec := range fake.records {
		if rec["artifactName"] == pushImage {
			t.Errorf("api was sent although the control plane records its SBOM")
		}
	}
	x, err := h.store.ReleaseSBOMIndex("v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	for _, up := range x.Uploads {
		if up.State != ledgerfile.SBOMUploadDone || up.Artifacts[pushSite] != sbomPushDiffers {
			t.Fatalf("binding conflict must be terminal, not retried: %+v", up)
		}
	}
}

// For a release cut before the index existed, an image's SBOM is found by
// its metadata (component name = the artifact, version = its digest). A
// static site's records a lockfile digest: its candidates are listed, and
// --sbom names the one to send.
func TestVulnSBOMPushMetadataFallbackAndOverride(t *testing.T) {
	t.Parallel()
	h := newPushHarness(t, &fakeVulnService{features: []string{hostedVulnFeatureSBOMUpload}, idempotent: true})
	api := h.put(cdxDoc(pushImage, sha("1"), "a"))
	h.put(cdxDoc(pushImage, sha("8"), "older-image")) // another build of api: not this release's
	carry := h.put(cdxDoc(pushCarry, sha("4"), "c"))
	lock := h.put(cdxDoc(pushSite, sha("e"), "lockfile"))
	legacy := h.put(cdxDoc(pushNoSBOM, sha("5"), "l"))

	doc, code := h.push()
	if code != exitUndetermined {
		t.Fatalf("exit %d, want 2 (the site needs an override)", code)
	}
	for name, want := range map[string]string{pushImage: api, pushCarry: carry, pushNoSBOM: legacy} {
		if a := statusOf(doc, name); a.Status != sbomPushSent || a.From != sbomFromMetadata || a.SBOMDigest != want {
			t.Errorf("%s: %+v; want sent %s, found by metadata", name, a, want)
		}
	}
	site := statusOf(doc, pushSite)
	if site.Status != sbomPushMissing || !site.NeedsOverride || len(site.Candidates) != 1 || site.Candidates[0] != lock ||
		!strings.Contains(site.Reason, "--sbom "+pushSite+"=") {
		t.Fatalf("site: %+v; want missing with its lockfile SBOM as the candidate", site)
	}

	// A typo'd override that names another artifact's SBOM is refused.
	doc, code = h.push(pushSite + "=" + api)
	if a := statusOf(doc, pushSite); code != exitUndetermined || a.Status != sbomPushMissing || !strings.Contains(a.Reason, "describes") {
		t.Fatalf("wrong override: exit %d, %+v", code, a)
	}
	// One not in this ledger is missing, never sent.
	doc, _ = h.push(pushSite + "=" + sha("7"))
	if a := statusOf(doc, pushSite); a.Status != sbomPushMissing || !strings.Contains(a.Reason, "not found") {
		t.Fatalf("absent override: %+v", a)
	}

	doc, code = h.push(pushSite + "=" + lock)
	if code != 0 {
		t.Fatalf("with the override: exit %d, %+v", code, doc)
	}
	if a := statusOf(doc, pushSite); a.Status != sbomPushSent || a.From != sbomFromOverride || a.SBOMDigest != lock {
		t.Errorf("site with --sbom: %+v", a)
	}
	if a := statusOf(doc, pushImage); a.Status != sbomPushAlready || a.From != sbomFromIndex {
		t.Errorf("api after the first push: %+v; want already recorded, now from the index the first push wrote", a)
	}
}

// A control plane that does not take SBOMs, or answers Unimplemented, is a
// refusal (exit 4): nothing is sent, and the release stays pending.
func TestVulnSBOMPushRefusals(t *testing.T) {
	t.Parallel()
	for name, fake := range map[string]*fakeVulnService{
		"no capability": {features: []string{}},
		"unimplemented": {features: []string{hostedVulnFeatureSBOMUpload}, unimplemented: true},
	} {
		h := newPushHarness(t, fake)
		h.index(map[string]string{pushImage: h.put(cdxDoc(pushImage, sha("1"), "a"))})
		doc, code := h.push()
		if code != exitRefused || !strings.Contains(doc.Error, "does not") {
			t.Errorf("%s: exit %d, error %q; want 4", name, code, doc.Error)
		}
		if name == "unimplemented" {
			x, _ := h.store.ReleaseSBOMIndex("v1.0.0")
			for _, up := range x.Uploads {
				if up.State != ledgerfile.SBOMUploadPending {
					t.Errorf("%s: upload state %+v; want pending", name, up)
				}
			}
		}
	}
	// No env keeps its ledger on a control plane.
	h := newPushHarness(t, &fakeVulnService{features: []string{hostedVulnFeatureSBOMUpload}})
	h.deps.resolveEnv = func(context.Context, string, string) (vulnEnv, error) { return vulnEnv{Name: "prod"}, nil }
	if _, code := h.push(); code != exitRefused {
		t.Errorf("no control-plane env: exit %d, want 4", code)
	}
}

// A cut to a control plane that does not take SBOMs records the release
// pending; the next cut to one that does re-sends it first, then its own.
func TestHostedCutDrainsPendingReleasesFirst(t *testing.T) {
	t.Parallel()
	fake := &fakeVulnService{features: []string{}}
	hs, r, warn := hostedSBOMCut(t, fake, "api")
	if _, err := hs.Cut(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if fake.count(procRecordReleaseSBOM) != 0 || !strings.Contains(warn.String(), "re-sent on the next cut") {
		t.Fatalf("first cut: uploads %d, output %q", fake.count(procRecordReleaseSBOM), warn)
	}
	// The control plane is upgraded; the next cut (a new store: a new
	// process) is v1.1.0.
	fake.mu.Lock()
	fake.features = []string{hostedVulnFeatureSBOMUpload}
	fake.mu.Unlock()
	hs.vulnCaps = nil
	warn.Reset()
	next := r
	next.Version = "v1.1.0"
	if _, err := hs.Cut(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	if warn.Len() != 0 {
		t.Errorf("a clean drain printed:\n%s", warn)
	}
	var versions []string
	for _, rec := range fake.records {
		versions = append(versions, rec["version"].(string)+"/"+rec["artifactName"].(string))
	}
	if strings.Join(versions, ",") != "v1.0.0/api,v1.1.0/api" {
		t.Fatalf("uploads %v; want the pending v1.0.0 first, then v1.1.0", versions)
	}
	store, _ := hs.sboms()
	if p, _, _ := store.PendingSBOMUploads(hs.endpoint); len(p) != 0 {
		t.Errorf("still pending after the drain: %+v", p)
	}
	// A third cut has nothing left to drain.
	third := r
	third.Version = "v1.2.0"
	if _, err := hs.Cut(context.Background(), third); err != nil {
		t.Fatal(err)
	}
	if n := fake.count(procRecordReleaseSBOM); n != 3 {
		t.Errorf("RecordReleaseSBOM calls = %d, want 3", n)
	}
}

// An Unimplemented answer keeps its one line, now naming the re-send, and
// records the release pending.
func TestHostedCutUnimplementedRecordsPending(t *testing.T) {
	t.Parallel()
	fake := &fakeVulnService{features: []string{hostedVulnFeatureFindings}, unimplemented: true}
	hs, r, warn := hostedSBOMCut(t, fake, "api", "worker")
	if _, err := hs.Cut(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warn.String(), "forge vuln sbom push --release v1.0.0") || strings.Count(warn.String(), "\n") != 1 {
		t.Fatalf("want one line naming the re-send:\n%s", warn)
	}
	store, _ := hs.sboms()
	p, _, err := store.PendingSBOMUploads(hs.endpoint)
	if err != nil || len(p) != 1 || p[0].Uploads[hs.endpoint].Attempts != 1 {
		t.Fatalf("pending = %+v, %v", p, err)
	}
}

func TestCycloneDXComponent(t *testing.T) {
	t.Parallel()
	c, ok := cycloneDXComponent(cdxDoc("img", sha("1"), "x"))
	if !ok || c.Name != "img" || c.Version != sha("1") {
		t.Fatalf("%+v %v", c, ok)
	}
	// Truncated after metadata: still read.
	full := cdxDoc("img", sha("1"), "x")
	cut := full[:bytes.Index(full, []byte(`"components":[{"name":"busybox"`))+5]
	if c, ok := cycloneDXComponent(cut); !ok || c.Name != "img" {
		t.Fatalf("truncated: %+v %v", c, ok)
	}
	for _, bad := range []string{``, `[]`, `{"bomFormat":"CycloneDX"}`, `{"metadata":{}}`} {
		if _, ok := cycloneDXComponent([]byte(bad)); ok {
			t.Errorf("%q read as having a component", bad)
		}
	}
}

func TestParseSBOMOverrides(t *testing.T) {
	t.Parallel()
	got, err := parseSBOMOverrides([]string{"a/b=" + sha("1"), "c=" + sha("2")})
	if err != nil || got["a/b"] != sha("1") || got["c"] != sha("2") {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range [][]string{{"nodigest"}, {"=x"}, {"a="}, {"a=" + sha("1"), "a=" + sha("2")}} {
		if _, err := parseSBOMOverrides(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

// The push's pre-read of which SBOMs the control plane records
// (GetReleaseVulnerabilities) needs vuln:read. A session credential's token
// does not carry it, and the control plane's refusal ("missing vuln:read in
// target organization") names no scope to elevate for, so the push declares
// it: the session is exchanged for vuln:read before the read, and the upload
// goes out on the session's own token. On cicd the undeclared read was
// refused, every SBOM was sent blind, and the push said so in a note.
func TestVulnSBOMPushElevatesForVulnReadBeforeThePreRead(t *testing.T) {
	t.Parallel()
	fake := &fakeVulnService{features: []string{hostedVulnFeatureFindings}, idempotent: true}
	h := newPushHarness(t, fake)
	api := h.put(cdxDoc(pushImage, sha("1"), "a"))
	carry := h.put(cdxDoc(pushCarry, sha("4"), "c"))
	site := h.put(cdxDoc(pushSite, sha("9"), "s"))
	legacy := h.put(cdxDoc(pushNoSBOM, sha("5"), "l"))
	h.index(map[string]string{pushCarry: carry, pushSite: site, pushNoSBOM: legacy})
	fake.releaseVulns = map[string]map[string]any{"v1.0.0": {"releaseVersion": "v1.0.0", "artifacts": []map[string]any{
		{"artifactName": pushImage, "sbomDigest": api},
	}}}

	var (
		mu                      sync.Mutex
		exchanges               []string
		readBearers, sendBearer []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proc := strings.TrimPrefix(r.URL.Path, "/")
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch proc {
		case "controlplane.v1.AccessTokenService/ExchangeToken":
			var req struct {
				Scopes []string `json:"scopes"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			exchanges = append(exchanges, bearer+" "+strings.Join(req.Scopes, ","))
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"secret": "rlat_vuln_read",
				"token":  map[string]any{"expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
			})
			return
		case procGetReleaseVulnerabilities:
			mu.Lock()
			readBearers = append(readBearers, bearer)
			mu.Unlock()
			if bearer != "rlat_vuln_read" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"code":"permission_denied","message":"missing vuln:read in target organization"}`))
				return
			}
		case procRecordReleaseSBOM:
			mu.Lock()
			sendBearer = append(sendBearer, bearer)
			mu.Unlock()
		}
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	h.deps.hostedClient = func(context.Context, vulnEnv) (cloudCaller, error) {
		// A session, as the credential helper or `forge login` hands it
		// over: no scope declared here; the push must declare its own.
		return cloud.NewClient(cloud.Endpoint{Env: "prod", URL: srv.URL}, cloud.Credential{Token: "rlat_session", Source: cloud.SourceLogin}), nil
	}

	doc, code := h.push()
	if code != 0 {
		t.Fatalf("exit %d: %+v", code, doc)
	}
	for _, n := range doc.Notes {
		if strings.Contains(n, "could not read which SBOMs") {
			t.Errorf("the pre-read was refused: %s", n)
		}
	}
	if a := statusOf(doc, pushImage); a.Status != sbomPushAlready || a.From != sbomFromControlPlane {
		t.Errorf("api: %+v; want already recorded, read from the control plane", a)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(exchanges) != 1 || exchanges[0] != "rlat_session vuln:read" {
		t.Errorf("exchanges %v; want the session exchanged once for vuln:read", exchanges)
	}
	if len(readBearers) != 1 || readBearers[0] != "rlat_vuln_read" {
		t.Errorf("GetReleaseVulnerabilities bearers %v; want one read on the vuln:read token", readBearers)
	}
	if len(sendBearer) != 3 {
		t.Errorf("RecordReleaseSBOM calls %d, want 3 (carry, site, legacy)", len(sendBearer))
	}
	for _, b := range sendBearer {
		if b != "rlat_session" {
			t.Errorf("an upload went out on %q, want the session's own token", b)
		}
	}
}
