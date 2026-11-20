package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/reliant-labs/forge/internal/sbom"
	"github.com/reliant-labs/forge/pkg/release"
	"github.com/reliant-labs/forge/pkg/testkit"
)

const (
	recordCycloneDX = `{"bomFormat":"CycloneDX","specVersion":"1.5","metadata":{"tools":{"components":[{"name":"trivy","version":"0.50.0"}]}},"components":[]}`
	recordSPDX      = `{"spdxVersion":"SPDX-2.3","SPDXID":"SPDXRef-DOCUMENT","creationInfo":{"creators":["Tool: syft-1.57.2"]},"packages":[]}`
)

// sbomRecordTest is one record test's surroundings: a fake control plane, a
// fake syft, a private ledger home, and the options for two workloads.
type sbomRecordTest struct {
	cp   *fakeRecordCP
	syft *cutSyft
	home string
	sc   recordScope
}

func newSBOMRecordTest(t *testing.T) *sbomRecordTest {
	t.Helper()
	rt := &sbomRecordTest{cp: newFakeRecordCP(), syft: &cutSyft{}, home: t.TempDir()}
	useRecordLedger(t, hostedRecordTarget(rt.cp))
	useFakeSyft(t, rt.syft, "")
	useLedgerHome(t, rt.home)
	rt.sc = recordScopeFor(t, githubEnv)
	return rt
}

func (rt *sbomRecordTest) store(t *testing.T) interface {
	GetSBOM(string) ([]byte, error)
} {
	t.Helper()
	s, err := openRecordSBOMStore(rt.sc.projectDir, rt.sc.project)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func twoWorkloads(o releaseRecordOptions) releaseRecordOptions {
	o.env, o.version = "prod", "v1.4.0"
	o.workloads = []string{"api", "web"}
	o.images = []string{"ghcr.io/acme/api@" + recordDigestA, "ghcr.io/acme/web@" + recordDigestB}
	return o
}

func writeSBOMFile(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func sbomEntry(t *testing.T, res releaseRecordResult, workload string) recordSBOMEntry {
	t.Helper()
	for _, e := range res.SBOMs {
		if e.Workload == workload {
			return e
		}
	}
	t.Fatalf("no SBOM entry for %s in %+v", workload, res.SBOMs)
	return recordSBOMEntry{}
}

// The zero-flag path: each --image is described from its registry by digest,
// under the repository's name, and the bytes land in the machine ledger.
func TestReleaseRecordSBOM_GeneratedFromEachImageByDefault(t *testing.T) {
	rt := newSBOMRecordTest(t)
	res, err := recordReleaseIn(context.Background(), twoWorkloads(releaseRecordOptions{}), rt.sc, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	calls := rt.syft.calls()
	sort.Strings(calls)
	if got, want := strings.Join(calls, " "),
		"registry:ghcr.io/acme/api@"+recordDigestA+" registry:ghcr.io/acme/web@"+recordDigestB; got != want {
		t.Errorf("syft read %q, want %q", got, want)
	}
	for _, w := range []string{"api", "web"} {
		e := sbomEntry(t, res, w)
		if e.Status != release.SBOMStatusOK || e.Source != release.SBOMSourceRegistry || e.Format != sbom.Format || e.Reason != "" {
			t.Errorf("%s: %+v", w, e)
		}
		data, err := rt.store(t).GetSBOM(e.Digest)
		if err != nil || !strings.Contains(string(data), "ghcr.io/acme/"+w) {
			t.Errorf("%s: the machine ledger does not hold SBOM %s: %v", w, e.Digest, err)
		}
	}
	if len(rt.cp.called("CutRelease")) != 1 {
		t.Error("the record was not cut")
	}
}

// A registry read that fails warns and records unavailable; the record
// itself succeeds.
func TestReleaseRecordSBOM_FailureWarnsAndStillRecords(t *testing.T) {
	rt := newSBOMRecordTest(t)
	rt.syft.fail = map[string]error{"registry": errors.New("GET https://ghcr.io/v2/: 401 Unauthorized")}
	var warn bytes.Buffer
	res, err := recordReleaseIn(context.Background(), twoWorkloads(releaseRecordOptions{}), rt.sc, &warn)
	if err != nil {
		t.Fatalf("a failed SBOM failed the record: %v", err)
	}
	if !res.ReleaseCreated || !res.Promoted {
		t.Errorf("record: %+v", res)
	}
	e := sbomEntry(t, res, "api")
	if e.Status != release.SBOMStatusUnavailable || e.Digest != "" || !strings.Contains(e.Reason, "401") {
		t.Errorf("api: %+v", e)
	}
	if !strings.Contains(warn.String(), "release v1.4.0 recorded without an SBOM for api: ") {
		t.Errorf("warning: %q", warn.String())
	}
}

// --sbom records the pipeline's own document verbatim, CycloneDX or SPDX,
// with the format it states and the digest of the bytes as given; syft runs
// only for the workload with none.
func TestReleaseRecordSBOM_ProvidedCycloneDXAndSPDX(t *testing.T) {
	rt := newSBOMRecordTest(t)
	cdx := writeSBOMFile(t, "api.cdx.json", recordCycloneDX)
	spdx := writeSBOMFile(t, "worker.spdx.json", recordSPDX)
	o := twoWorkloads(releaseRecordOptions{sbom: recordSBOMOptions{specs: []string{"api=" + cdx, "worker=" + spdx}}})
	o.workloads = append(o.workloads, "worker")
	o.images = append(o.images, "ghcr.io/acme/worker@"+sha("w"))

	res, err := recordReleaseIn(context.Background(), o, rt.sc, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	for w, want := range map[string]struct{ body, format string }{
		"api":    {recordCycloneDX, "cyclonedx-json@1.5"},
		"worker": {recordSPDX, "spdx-json@2.3"},
	} {
		e := sbomEntry(t, res, w)
		if e.Status != release.SBOMStatusOK || e.Source != release.SBOMSourceCIProvided || e.Format != want.format ||
			e.Digest != sbom.Digest([]byte(want.body)) {
			t.Errorf("%s: %+v", w, e)
		}
		if data, err := rt.store(t).GetSBOM(e.Digest); err != nil || string(data) != want.body {
			t.Errorf("%s: stored %q, %v; want the file's bytes", w, data, err)
		}
	}
	if e := sbomEntry(t, res, "web"); e.Source != release.SBOMSourceRegistry {
		t.Errorf("web, with no --sbom, was not generated: %+v", e)
	}
	if calls := rt.syft.calls(); len(calls) != 1 || !strings.Contains(calls[0], "ghcr.io/acme/web") {
		t.Errorf("syft ran for %v, want only web", calls)
	}
}

// Mistakes in the flags are refused before anything is cut.
func TestReleaseRecordSBOM_FlagMistakesRefuseTheRecord(t *testing.T) {
	cdx := writeSBOMFile(t, "api.cdx.json", recordCycloneDX)
	notSBOM := writeSBOMFile(t, "package.json", `{"name":"app"}`)
	for name, tc := range map[string]struct {
		sbom recordSBOMOptions
		want string
	}{
		"unknown workload": {recordSBOMOptions{specs: []string{"apii=" + cdx}}, `workload "apii", which this record does not: it names api, web`},
		"twice":            {recordSBOMOptions{specs: []string{"api=" + cdx, "api=" + cdx}}, "twice"},
		"no path":          {recordSBOMOptions{specs: []string{"api"}}, "<workload>=<path>"},
		"missing file":     {recordSBOMOptions{specs: []string{"api=" + cdx + ".nope"}}, "no such file"},
		"not an SBOM":      {recordSBOMOptions{specs: []string{"api=" + notSBOM}}, "not an SBOM forge can record"},
		"no-sbom and sbom": {recordSBOMOptions{noSBOM: true, specs: []string{"api=" + cdx}}, "cannot be combined"},
		"source w/ images": {recordSBOMOptions{source: t.TempDir()}, "version-only"},
	} {
		t.Run(name, func(t *testing.T) {
			rt := newSBOMRecordTest(t)
			_, err := recordReleaseIn(context.Background(), twoWorkloads(releaseRecordOptions{sbom: tc.sbom}), rt.sc, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want one saying %q", err, tc.want)
			}
			if n := len(rt.cp.called("CutRelease")); n != 0 {
				t.Errorf("a refused record cut %d release(s)", n)
			}
		})
	}
}

// --no-sbom records every workload as skipped, says so, and runs no syft.
func TestReleaseRecordSBOM_NoSBOM(t *testing.T) {
	rt := newSBOMRecordTest(t)
	var warn bytes.Buffer
	res, err := recordReleaseIn(context.Background(), twoWorkloads(releaseRecordOptions{sbom: recordSBOMOptions{noSBOM: true}}), rt.sc, &warn)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	for _, w := range []string{"api", "web"} {
		if e := sbomEntry(t, res, w); e.Status != release.SBOMStatusUnavailable || e.Reason != "skipped: --no-sbom" {
			t.Errorf("%s: %+v", w, e)
		}
	}
	if n := len(rt.syft.calls()); n != 0 {
		t.Errorf("syft ran %d time(s) under --no-sbom", n)
	}
	if !strings.Contains(warn.String(), "--no-sbom") {
		t.Errorf("--no-sbom did not warn: %q", warn.String())
	}
}

// --sbom-source on a version-only record: the source tree at the record's
// commit becomes one git artifact named after the project, described by a
// directory scan.
func TestReleaseRecordSBOM_SourceTreeOfAVersionOnlyRecord(t *testing.T) {
	rt := newSBOMRecordTest(t)
	top := t.TempDir()
	tree := filepath.Join(top, "services", "api")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	prevGit := recordSourceGit
	recordSourceGit = func(dir string, args ...string) string {
		switch strings.Join(args, " ") {
		case "rev-parse --show-toplevel":
			return top
		case "remote get-url origin":
			return "git@github.com:Acme/Shop.git"
		}
		return ""
	}
	t.Cleanup(func() { recordSourceGit = prevGit })

	o := releaseRecordOptions{env: "prod", version: "v1.4.0", sbom: recordSBOMOptions{source: tree}}
	res, err := recordReleaseIn(context.Background(), o, rt.sc, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	e := sbomEntry(t, res, "shop")
	if e.Status != release.SBOMStatusOK || e.Source != release.SBOMSourceLockfile {
		t.Errorf("shop: %+v", e)
	}
	if calls := rt.syft.calls(); len(calls) != 1 || calls[0] != "dir:"+tree {
		t.Errorf("syft read %v, want a dir scan of %s", calls, tree)
	}
	arts := rt.cp.called("CutRelease")[0]["artifacts"].([]any)
	if len(arts) != 1 {
		t.Fatalf("CutRelease artifacts = %v, want the source tree", arts)
	}
	a := arts[0].(map[string]any)
	src, _ := a["source"].(map[string]any)
	if a["name"] != "shop" || a["kind"] != string(release.KindGit) || src["repo"] != "github.com/Acme/Shop" ||
		src["ref"] != recordCommitA || src["commit"] != recordCommitA || src["subdir"] != "services/api" {
		t.Errorf("source artifact = %v", a)
	}

	// A retry is the same release; one with no origin to name the source by
	// stays version-only and warns.
	if again, err := recordReleaseIn(context.Background(), o, rt.sc, &bytes.Buffer{}); err != nil || again.ReleaseCreated {
		t.Errorf("retry: %+v %v", again, err)
	}
	recordSourceGit = func(string, ...string) string { return "" }
	var warn bytes.Buffer
	o.version = "v1.5.0"
	res, err = recordReleaseIn(context.Background(), o, rt.sc, &warn)
	if err != nil || len(res.SBOMs) != 0 || !strings.Contains(warn.String(), "recorded version-only, without an SBOM") {
		t.Errorf("no checkout: %+v %v %q", res, err, warn.String())
	}
}

// The post-generate hooks run for record-made SBOMs too, generated and
// provided alike: Name is the workload, the repository is Artifact.URI.
func TestReleaseRecordSBOM_RunsThePostGenerateHooks(t *testing.T) {
	rt := newSBOMRecordTest(t)
	prev := sbomPostGenerate
	t.Cleanup(func() { sbomPostGenerate = prev })
	var seen sync.Map
	sbomPostGenerate = []sbomPostGenerateHook{func(_ context.Context, in sbomGenerated) (release.SBOMRef, error) {
		seen.Store(in.Name, in.Artifact.URI+" "+in.Version+" "+in.Env)
		ref := *in.Artifact.SBOM
		ref.Referrer = in.Artifact.URI + "@" + sha("referrer")
		return ref, nil
	}}
	cdx := writeSBOMFile(t, "api.cdx.json", recordCycloneDX)
	o := twoWorkloads(releaseRecordOptions{sbom: recordSBOMOptions{specs: []string{"api=" + cdx}}})
	res, err := recordReleaseIn(context.Background(), o, rt.sc, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	for _, w := range []string{"api", "web"} {
		if got, _ := seen.Load(w); got != "ghcr.io/acme/"+w+" v1.4.0 prod" {
			t.Errorf("%s: the hook saw %v", w, got)
		}
		if e := sbomEntry(t, res, w); e.Status != release.SBOMStatusOK {
			t.Errorf("%s: %+v", w, e)
		}
	}
}

// On the machine ledger the release itself carries each SBOM ref, and
// --json lists them under sboms.
func TestReleaseRecordSBOM_MachineLedgerAndJSON(t *testing.T) {
	newSBOMRecordTest(t)
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "forge.yaml"), []byte("name: shop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ledgerSelectionEnv, string(ledgerSelectMachine))
	useRecordLedger(t, defaultRecordLedger)
	sc := recordScope{projectDir: projectDir, project: "shop", getenv: func(string) string { return "" }}
	o := twoWorkloads(releaseRecordOptions{commit: recordCommitA})

	res, err := recordReleaseIn(context.Background(), o, sc, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	l, err := machineLedger(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := l.Releases.Get(context.Background(), "v1.4.0")
	if err != nil || rel == nil {
		t.Fatalf("release: %v", err)
	}
	if ref := rel.Artifacts["api"].SBOM; ref == nil || ref.Digest != sbomEntry(t, res, "api").Digest || ref.Source != release.SBOMSourceRegistry {
		t.Errorf("recorded api SBOM %+v", ref)
	}

	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		SBOMs []map[string]any `json:"sboms"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.SBOMs) != 2 {
		t.Fatalf("--json sboms: %s", raw)
	}
	first := doc.SBOMs[0]
	for _, key := range []string{"workload", "digest", "status", "source"} {
		if _, ok := first[key]; !ok {
			t.Errorf("--json sboms[0] has no %q: %v", key, first)
		}
	}
	if _, ok := first["reason"]; ok {
		t.Errorf("an SBOM that was recorded carries a reason: %v", first)
	}
	var out bytes.Buffer
	printReleaseRecord(&out, res)
	if !strings.Contains(out.String(), "sbom     api") {
		t.Errorf("text output lists no SBOM:\n%s", out.String())
	}
}

// The real git: the source artifact names the checkout's origin and the
// tree's directory within it.
func TestRecordSourceArtifact_RealGit(t *testing.T) {
	testkit.Slow(t, "runs git init, remote and rev-parse")
	top := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "https://github.com/acme/shop.git"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = top
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	tree := filepath.Join(top, "web")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	art, err := recordSourceArtifact(tree, recordCommitA)
	if err != nil {
		t.Fatal(err)
	}
	if err := art.Validate("shop"); err != nil {
		t.Fatalf("the source artifact does not validate: %v", err)
	}
	if art.Source.Repo != "github.com/acme/shop" || art.Source.Subdir != "web" || art.Source.Commit != recordCommitA {
		t.Errorf("source = %+v", art.Source)
	}
	if _, err := recordSourceArtifact(tree, ""); err == nil || !strings.Contains(err.Error(), "--commit") {
		t.Errorf("no commit: %v", err)
	}
}

// Registry reads go through the cut's credential path: the record's env and
// its declaration decide whether forge's own control-plane identity is
// offered for the image's registry.
func TestReleaseRecordSBOM_RegistryReadsUseTheCutsCredentialPath(t *testing.T) {
	rt := newSBOMRecordTest(t)
	prev := sbomRegistryCredential
	t.Cleanup(func() { sbomRegistryCredential = prev })
	declared := &KCLEntities{}
	var mu sync.Mutex
	var asked []string
	sbomRegistryCredential = func(projectDir, env string, e *KCLEntities, host string) (auth.CredentialFunc, error) {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, env+" "+host+" "+strconv.FormatBool(e == declared))
		return nil, nil
	}
	artifacts := map[string]release.Artifact{"api": {Kind: release.KindOCI, Mode: release.ModeShared,
		Digests: map[string]string{release.SharedVariant: recordDigestA}, URI: "registry.example.com/acme/api"}}
	recordReleaseSBOMs(context.Background(), recordSBOMRun{projectDir: rt.sc.projectDir, project: "shop", env: "prod",
		version: "v1.4.0", declared: declared}, artifacts, &bytes.Buffer{})
	if len(asked) != 1 || asked[0] != "prod registry.example.com true" {
		t.Errorf("credential asked for %v, want prod's declaration for registry.example.com", asked)
	}
	if ref := artifacts["api"].SBOM; ref == nil || ref.Status != release.SBOMStatusOK {
		t.Errorf("SBOM %+v", ref)
	}
}
