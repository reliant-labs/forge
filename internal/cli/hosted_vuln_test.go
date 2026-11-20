package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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
	"github.com/reliant-labs/forge/internal/vulnscan"
	"github.com/reliant-labs/forge/pkg/release"
)

// fakeVulnService is control-plane's VulnerabilityService and the
// vulnerability half of DeployService/GetCapabilities, speaking Connect's
// JSON binding with the proto's field names spelled literally (a typo in the
// client's wire shapes fails the test rather than moving with it). Every
// other procedure goes to next (a fakeDeployService), when set.
type fakeVulnService struct {
	mu sync.Mutex
	// features is GetCapabilities.vulnerabilityFeatures; noCapabilities
	// answers GetCapabilities Unimplemented (a control plane predating it).
	features       []string
	noCapabilities bool
	// unimplemented answers every VulnerabilityService rpc Unimplemented.
	unimplemented bool
	// refuse answers every read PermissionDenied.
	refuse bool
	// digestOverride is what RecordReleaseSBOM answers as the digest,
	// instead of the one it derives from the bytes.
	digestOverride string
	// records are the RecordReleaseSBOM bodies, in order.
	records []map[string]any
	// releaseVulns is GetReleaseVulnerabilities' answer, per version.
	releaseVulns map[string]map[string]any
	// summary is GetVulnerabilitySummary's environments.
	summary []map[string]any
	// advisory is GetVulnerability's answer, by advisory id.
	advisory map[string]map[string]any
	calls    []string
	next     http.Handler
}

func (f *fakeVulnService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const svc = "/controlplane.v1.VulnerabilityService/"
	if r.URL.Path != "/controlplane.v1.DeployService/GetCapabilities" && !strings.HasPrefix(r.URL.Path, svc) {
		if f.next == nil {
			connectErr(w, http.StatusNotFound, "unimplemented", "no such procedure "+r.URL.Path)
			return
		}
		f.next.ServeHTTP(w, r)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.TrimPrefix(r.URL.Path, "/"))
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/controlplane.v1.DeployService/GetCapabilities" {
		if f.noCapabilities {
			connectErr(w, http.StatusNotFound, "unimplemented", "no such procedure")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"vulnerabilityFeatures": f.features})
		return
	}
	if f.unimplemented {
		connectErr(w, http.StatusNotImplemented, "unimplemented", "not implemented")
		return
	}
	str := func(k string) string { s, _ := body[k].(string); return s }
	switch strings.TrimPrefix(r.URL.Path, svc) {
	case "RecordReleaseSBOM":
		f.records = append(f.records, body)
		gz, err := base64.StdEncoding.DecodeString(str("sbom"))
		if err != nil || str("encoding") != "SBOM_ENCODING_GZIP" {
			connectErr(w, http.StatusBadRequest, "invalid_argument", "sbom is not gzip base64")
			return
		}
		zr, err := gzip.NewReader(bytes.NewReader(gz))
		if err != nil {
			connectErr(w, http.StatusBadRequest, "invalid_argument", "sbom is not gzip")
			return
		}
		doc, _ := io.ReadAll(zr)
		sum := sha256.Sum256(doc)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		if f.digestOverride != "" {
			digest = f.digestOverride
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"digest": digest, "created": true, "scanState": "VULNERABILITY_SCAN_STATE_PENDING", "componentCount": 3})
	case "GetVulnerabilitySummary":
		if f.refuse {
			connectErr(w, http.StatusForbidden, "permission_denied", "missing scope vuln:read")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"environments": f.summary})
	case "GetReleaseVulnerabilities":
		if f.refuse {
			connectErr(w, http.StatusForbidden, "permission_denied", "missing scope vuln:read")
			return
		}
		resp, ok := f.releaseVulns[str("version")]
		if !ok {
			connectErr(w, http.StatusNotFound, "not_found", "no such release")
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	case "GetVulnerability":
		resp, ok := f.advisory[str("advisoryId")]
		if !ok {
			connectErr(w, http.StatusNotFound, "not_found", "no such advisory")
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	default:
		connectErr(w, http.StatusNotFound, "unimplemented", "no such procedure "+r.URL.Path)
	}
}

func (f *fakeVulnService) count(proc string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == proc {
			n++
		}
	}
	return n
}

// hostedSBOMCut is a hosted store over fake (with a fakeDeployService behind
// it), whose machine ledger holds one SBOM per named artifact, and the
// release those SBOMs belong to.
func hostedSBOMCut(t *testing.T, fake *fakeVulnService, names ...string) (*hostedStore, release.Release, *bytes.Buffer) {
	t.Helper()
	fake.next = newFakeDeployService(map[string]string{"prod": "env-prod-id"})
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	ep := cloud.Endpoint{Env: "prod", URL: srv.URL, TokenEnv: "T"}
	l := hostedLedger(cloud.NewClient(ep, cloud.Credential{Token: "rlat_test"}), srv.URL, "acme", deploytarget.HostedEnvPersistent)
	hs := l.Bindings.(*hostedStore)
	store, err := ledgerfile.Open(t.TempDir(), "hosted-vuln-test")
	if err != nil {
		t.Fatal(err)
	}
	hs.sboms = func() (sbomBytesReader, error) { return store, nil }
	var warn bytes.Buffer
	hs.warn = &warn
	r := release.Release{Version: "v1.0.0", Artifacts: map[string]release.Artifact{}}
	for i, name := range names {
		digest, err := store.PutSBOM([]byte(`{"bomFormat":"CycloneDX","specVersion":"1.6","serialNumber":"` + name + `"}`))
		if err != nil {
			t.Fatal(err)
		}
		r.Artifacts[name] = release.Artifact{
			Kind: release.KindOCI, Mode: release.ModeShared,
			Digests: map[string]string{release.SharedVariant: sha(string("0123456789"[i]))},
			SBOM: &release.SBOMRef{Digest: digest, Status: release.SBOMStatusOK, Format: release.SBOMFormatCycloneDXJSON,
				Generator: "syft@v1.57.2", Source: release.SBOMSourceRegistry, Referrer: "registry.test/acme/" + name + "@" + sha("f")},
		}
	}
	// One artifact without an SBOM, which is never uploaded.
	r.Artifacts["bare"] = release.Artifact{Kind: release.KindOCI, Mode: release.ModeShared,
		Digests: map[string]string{release.SharedVariant: sha("e")},
		SBOM:    &release.SBOMRef{Status: release.SBOMStatusUnavailable, Reason: "syft found no packages"}}
	return hs, r, &warn
}

func TestHostedCutUploadsEachSBOM(t *testing.T) {
	t.Parallel()
	fake := &fakeVulnService{features: []string{hostedVulnFeatureFindings}}
	hs, r, warn := hostedSBOMCut(t, fake, "api", "worker")
	if _, err := hs.Cut(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if warn.Len() != 0 {
		t.Fatalf("a clean upload printed:\n%s", warn)
	}
	if len(fake.records) != 2 {
		t.Fatalf("RecordReleaseSBOM calls = %d, want 2 (api, worker; never the artifact without an SBOM)", len(fake.records))
	}
	got := fake.records[0]
	ref := r.Artifacts["api"].SBOM
	for k, want := range map[string]string{
		"version": "v1.0.0", "project": "acme", "artifactName": "api", "encoding": "SBOM_ENCODING_GZIP",
		"format": ref.Format, "generator": ref.Generator, "source": ref.Source, "referrer": ref.Referrer,
	} {
		if got[k] != want {
			t.Errorf("request %s = %v, want %q", k, got[k], want)
		}
	}
	if _, ok := got["orgId"]; ok {
		t.Errorf("orgId sent: the org is the credential's, as on DeployService")
	}
	// A re-cut uploads again (the server is idempotent): the backfill path.
	if _, err := hs.Cut(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if len(fake.records) != 4 || warn.Len() != 0 {
		t.Fatalf("re-cut: %d uploads, output %q", len(fake.records), warn)
	}
	if n := fake.count("controlplane.v1.DeployService/GetCapabilities"); n != 1 {
		t.Errorf("GetCapabilities asked %d times, want once per store", n)
	}
}

func TestHostedCutWarnsOnADigestMismatch(t *testing.T) {
	t.Parallel()
	fake := &fakeVulnService{features: []string{hostedVulnFeatureFindings}, digestOverride: sha("9")}
	hs, r, warn := hostedSBOMCut(t, fake, "api")
	if _, err := hs.Cut(context.Background(), r); err != nil {
		t.Fatalf("a digest mismatch failed the cut: %v", err)
	}
	if !strings.Contains(warn.String(), sha("9")) || !strings.Contains(warn.String(), r.Artifacts["api"].SBOM.Digest) {
		t.Fatalf("warning does not name both digests:\n%s", warn)
	}
}

func TestHostedCutNotesAnUnimplementedSBOMStoreOnce(t *testing.T) {
	t.Parallel()
	fake := &fakeVulnService{features: []string{hostedVulnFeatureFindings}, unimplemented: true}
	hs, r, warn := hostedSBOMCut(t, fake, "api", "worker")
	if _, err := hs.Cut(context.Background(), r); err != nil {
		t.Fatalf("Unimplemented failed the cut: %v", err)
	}
	if n := strings.Count(warn.String(), "does not store SBOMs yet"); n != 1 || strings.Count(warn.String(), "\n") != 1 {
		t.Fatalf("want exactly one note, got:\n%s", warn)
	}
	if n := fake.count(procRecordReleaseSBOM); n != 1 {
		t.Errorf("RecordReleaseSBOM called %d times after Unimplemented, want 1", n)
	}
}

func TestHostedCutSkipsTheUploadWithoutTheCapability(t *testing.T) {
	t.Parallel()
	for name, fake := range map[string]*fakeVulnService{
		"features withheld":     {features: []string{}},
		"predates capabilities": {noCapabilities: true},
	} {
		hs, r, warn := hostedSBOMCut(t, fake, "api")
		if _, err := hs.Cut(context.Background(), r); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if n := fake.count(procRecordReleaseSBOM); n != 0 || warn.Len() != 0 {
			t.Errorf("%s: %d uploads, output %q; want none, silently", name, n, warn)
		}
	}
}

func TestHostedVulnPlanLine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		features []string
		want     string
	}{
		{nil, "does not keep SBOMs or findings yet"},
		{[]string{hostedVulnFeatureFindings}, "does not judge vuln_gate at promote yet"},
		{[]string{hostedVulnFeatureFindings, hostedVulnFeatureEnforcesGate}, "judges vuln_gate at promote"},
	} {
		fake := &fakeVulnService{features: tc.features}
		hs, _, _ := hostedSBOMCut(t, fake)
		v := hostedVulnPlan(context.Background(), envLedger{Bindings: hs, Releases: hs})
		var out bytes.Buffer
		renderPlanVulns(&out, v)
		if !strings.HasPrefix(out.String(), "  vulns     ") || !strings.Contains(out.String(), tc.want) {
			t.Errorf("features %v: plan line %q, want %q", tc.features, out.String(), tc.want)
		}
	}
	if v := hostedVulnPlan(context.Background(), envLedger{Bindings: memBindingStoreForVulnPlan{}}); v != nil {
		t.Errorf("a machine-ledger env got a vulns line: %+v", v)
	}
}

// memBindingStoreForVulnPlan is any binding store that is not a control plane.
type memBindingStoreForVulnPlan struct{ bindingStore }

// ─── forge vuln on a control-plane env ──────────────────────────────────────

// hostedVulnHarness is a vulnHarness whose env records on fake.
func hostedVulnHarness(t *testing.T, fake *fakeVulnService) *vulnHarness {
	t.Helper()
	h := newVulnHarness(t)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	h.env.Endpoint = cloud.DefaultEndpoint
	h.env.Project = "acme"
	h.deps.hostedClient = func(context.Context, vulnEnv) (cloudCaller, error) {
		return cloud.NewClient(cloud.Endpoint{Env: "prod", URL: srv.URL}, cloud.Credential{Token: "rlat_test"}), nil
	}
	return h
}

// hostedFindings is a control plane holding release v1.0.0 in prod: api
// scanned with a fixable critical (blocking) and a low; web never given an
// SBOM.
func hostedFindings(sbomDigest string) *fakeVulnService {
	firstSeen := vulnTestNow.Add(-10 * 24 * time.Hour)
	return &fakeVulnService{
		features: []string{hostedVulnFeatureFindings},
		summary:  []map[string]any{{"environmentId": "env-prod", "environmentName": "prod", "releaseVersion": "v1.0.0"}},
		releaseVulns: map[string]map[string]any{"v1.0.0": {
			"releaseVersion": "v1.0.0",
			"artifacts": []map[string]any{
				{"artifactName": "api", "sbomDigest": sbomDigest, "referrer": "registry.test/acme/api@" + sha("f"), "scanState": "VULNERABILITY_SCAN_STATE_SCANNED", "lastScannedAt": vulnTestNow.Add(-time.Hour)},
				{"artifactName": "web", "scanState": "VULNERABILITY_SCAN_STATE_NO_SBOM"},
			},
			"findings": []map[string]any{
				{
					"advisory":      map[string]any{"id": "CVE-2026-1111", "aliases": []string{"GHSA-aaaa-bbbb-cccc"}, "severity": "VULNERABILITY_SEVERITY_CRITICAL", "cvssScore": 9.8, "kev": false},
					"package":       map[string]any{"name": "golang.org/x/net", "version": "v0.17.0", "type": "go-module", "purl": "pkg:golang/golang.org/x/net@v0.17.0"},
					"fixedVersions": []string{"v0.23.0"}, "fixState": "VULNERABILITY_FIX_STATE_FIXED",
					"artifactNames": []string{"api"}, "firstSeenAt": firstSeen,
				},
				{
					"advisory":      map[string]any{"id": "CVE-2026-2222", "severity": "VULNERABILITY_SEVERITY_LOW"},
					"package":       map[string]any{"name": "busybox", "version": "1.36", "type": "apk", "purl": "pkg:apk/alpine/busybox@1.36"},
					"fixState":      "VULNERABILITY_FIX_STATE_NOT_FIXED",
					"artifactNames": []string{"api"},
				},
			},
			"database": map[string]any{"scanner": "grype", "scannerVersion": "v0.122.1", "schema": "v6.1.10", "builtAt": vulnTestNow.Add(-6 * time.Hour)},
		}},
		advisory: map[string]map[string]any{"CVE-2026-1111": {
			"advisory":     map[string]any{"id": "CVE-2026-1111", "summary": "HTTP/2 rapid reset", "references": []string{"https://nvd.nist.gov/vuln/detail/CVE-2026-1111"}},
			"observations": []map[string]any{{"environmentId": "env-prod", "purl": "pkg:golang/golang.org/x/net@v0.17.0", "firstSeenAt": firstSeen, "firstReleaseVersion": "v0.9.0"}},
		}},
	}
}

func TestVulnListReadsTheControlPlane(t *testing.T) {
	t.Parallel()
	fake := hostedFindings("sha256:" + strings.Repeat("a", 64))
	h := hostedVulnHarness(t, fake)
	out, stderr, code := h.exec("list", "prod", "--json")
	if code != exitWrong {
		t.Fatalf("exit %d, want 1 (a fixable critical blocks)\n%s\n%s", code, out, stderr)
	}
	doc := decodeVulnDoc(t, out)
	if doc.Env != "prod" || doc.Release != "v1.0.0" {
		t.Errorf("doc head = %s %s", doc.Env, doc.Release)
	}
	if doc.Coverage.Artifacts != 2 || doc.Coverage.WithSBOM != 1 || doc.Coverage.Scanned != 1 || len(doc.Coverage.Unscanned) != 1 {
		t.Errorf("coverage = %+v", doc.Coverage)
	}
	if doc.Gate.Verdict != vulnscan.VerdictBlock || strings.Join(doc.Gate.Blocking, ",") != "CVE-2026-1111" {
		t.Errorf("gate = %+v", doc.Gate)
	}
	if doc.DB.Version != "v0.122.1" || doc.DB.Schema != "v6.1.10" || doc.DB.Stale {
		t.Errorf("db = %+v", doc.DB)
	}
	if doc.Counts.Critical != 1 || doc.Counts.Low != 1 || len(doc.Findings) != 2 {
		t.Fatalf("counts %+v, findings %d", doc.Counts, len(doc.Findings))
	}
	f := doc.Findings[0]
	if f.ID != "CVE-2026-1111" || f.Severity != vulnscan.SeverityCritical || !f.Fixable() || strings.Join(f.FixedIn, ",") != "v0.23.0" ||
		f.Package.PURL != "pkg:golang/golang.org/x/net@v0.17.0" || f.CVSS == nil || *f.CVSS != 9.8 ||
		!f.FirstSeen.Equal(vulnTestNow.Add(-10*24*time.Hour)) || f.Action != vulnscan.ActionBlock || strings.Join(f.Artifacts, ",") != "api" {
		t.Errorf("finding = %+v", f)
	}
	if h.scans != 0 {
		t.Errorf("scanned on this machine: the control plane's findings are the record")
	}
	text, _, _ := h.exec("list", "prod")
	for _, want := range []string{"vuln_gate (audit): BLOCK: CVE-2026-1111", "not scanned: web (no SBOM recorded on the control plane)",
		"→ https://forge.reliantlabs.io/forge/p/acme/vulnerabilities"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(text), "/forge/p/acme/vulnerabilities") {
		t.Errorf("text does not end with the console link:\n%s", text)
	}
	// scan --deployed reads the same record.
	if out, _, code := h.exec("scan", "--deployed", "prod", "--json"); code != exitWrong || decodeVulnDoc(t, out).Release != "v1.0.0" {
		t.Errorf("scan --deployed: exit %d", code)
	}
	if h.scans != 0 || len(h.installs) != 0 {
		t.Errorf("scan --deployed scanned (%d) or installed (%v) on this machine", h.scans, h.installs)
	}
}

func TestVulnShowReadsTheAdvisoryFromTheControlPlane(t *testing.T) {
	t.Parallel()
	h := hostedVulnHarness(t, hostedFindings("sha256:"+strings.Repeat("a", 64)))
	out, _, code := h.exec("show", "prod", "GHSA-aaaa-bbbb-cccc")
	if code != exitWrong {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, want := range []string{"CVE-2026-1111", "golang.org/x/net v0.17.0", "fixed in v0.23.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	out, _, _ = h.exec("show", "prod", "CVE-2026-1111")
	for _, want := range []string{"HTTP/2 rapid reset", "https://nvd.nist.gov/vuln/detail/CVE-2026-1111", "first seen 2026-11-10 (release v0.9.0)"} {
		if !strings.Contains(out, want) {
			t.Errorf("show by id lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "CVE-2026-2222") {
		t.Errorf("show listed an unrelated finding:\n%s", out)
	}
}

func TestVulnOnAControlPlaneWithoutVulnReadIsRefused(t *testing.T) {
	t.Parallel()
	fake := hostedFindings("")
	fake.refuse = true
	h := hostedVulnHarness(t, fake)
	for _, args := range [][]string{{"list", "prod"}, {"show", "prod", "CVE-1"}, {"sbom", "prod"}, {"list", "prod", "--release", "v1.0.0"}} {
		_, stderr, code := h.exec(args...)
		if code != exitRefused || !strings.Contains(stderr, "vuln:read") {
			t.Errorf("%v: exit %d, want 4\n%s", args, code, stderr)
		}
	}
}

func TestVulnOnAControlPlaneThatServesNoFindingsIsUndetermined(t *testing.T) {
	t.Parallel()
	for name, fake := range map[string]*fakeVulnService{
		"capability withheld": {features: []string{}},
		"predates it":         {noCapabilities: true},
		"rpc unimplemented":   {features: []string{hostedVulnFeatureFindings}, unimplemented: true},
	} {
		h := hostedVulnHarness(t, fake)
		for _, args := range [][]string{{"list", "prod"}, {"show", "prod", "CVE-1"}, {"sbom", "prod"}, {"scan", "--deployed", "prod"}} {
			_, stderr, code := h.exec(args...)
			if code != exitUndetermined || !strings.Contains(stderr, hostedVulnFindingsNotServedMsg) {
				t.Errorf("%s %v: exit %d\n%s", name, args, code, stderr)
			}
		}
	}
}

func TestVulnSBOMOnAControlPlaneReadsThisMachinesCopy(t *testing.T) {
	t.Parallel()
	h := newVulnHarness(t)
	data := []byte(`{"bomFormat":"CycloneDX","specVersion":"1.6","components":[]}`)
	digest, err := h.store.PutSBOM(data)
	if err != nil {
		t.Fatal(err)
	}
	fake := hostedFindings(digest)
	h2 := hostedVulnHarness(t, fake)
	h2.store = h.store
	h2.deps.machineLedger = func(string) (*ledgerfile.Store, error) { return h.store, nil }
	out, stderr, code := h2.exec("sbom", "prod")
	if code != 0 || out != string(data) {
		t.Fatalf("exit %d\n%s\n%s", code, out, stderr)
	}
	// An SBOM this machine has no copy of names its referrer and exits 2.
	h3 := hostedVulnHarness(t, hostedFindings("sha256:"+strings.Repeat("d", 64)))
	_, stderr, code = h3.exec("sbom", "prod", "--artifact", "api")
	if code != exitUndetermined || !strings.Contains(stderr, "no copy") || !strings.Contains(stderr, "registry.test/acme/api@") {
		t.Fatalf("missing copy: exit %d\n%s", code, stderr)
	}
}

// The findings seam the promote check reads (design §1.5): one env's findings
// of one release, as the document's artifacts.
func TestHostedVulnSourceReleaseScans(t *testing.T) {
	t.Parallel()
	fake := hostedFindings("sha256:" + strings.Repeat("a", 64))
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	src := hostedVulnSource{env: "prod", reader: hostedVulnReader{
		client:  cloud.NewClient(cloud.Endpoint{Env: "prod", URL: srv.URL}, cloud.Credential{Token: "rlat_test"}),
		project: "acme",
	}}
	scans, _, err := src.releaseScans(context.Background(), &release.Release{Version: "v1.0.0"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(scans) != 2 || scans[0].Latest == nil || len(scans[0].Latest.Findings) != 2 || scans[1].Latest != nil {
		t.Fatalf("scans = %+v", scans)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var envApplied bool
	for _, c := range fake.calls {
		envApplied = envApplied || c == "controlplane.v1.VulnerabilityService/GetVulnerabilitySummary"
	}
	if !envApplied {
		t.Errorf("the env's id was never resolved, so its exemptions and first-seen times were not applied")
	}
}
