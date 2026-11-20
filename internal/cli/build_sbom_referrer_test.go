package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/reliant-labs/forge/internal/sbom"
	"github.com/reliant-labs/forge/internal/sbomoci"
	"github.com/reliant-labs/forge/internal/sbomoci/ocitest"
	"github.com/reliant-labs/forge/pkg/release"
)

// attachCall is one attach the hook asked for.
type attachCall struct {
	subject string
	opts    sbomoci.AttachOptions
}

// useReferrerRegistry points the referrer hook at the real sbomoci.Attach
// with an anonymous credential (an ocitest.Registry asks for none), and
// records each call. It mutates package vars: the test must not be parallel.
func useReferrerRegistry(t *testing.T) *[]attachCall {
	t.Helper()
	prevAttach, prevCred := sbomReferrerAttach, sbomRegistryCredential
	var (
		mu    sync.Mutex
		calls []attachCall
	)
	sbomReferrerAttach = func(ctx context.Context, subject string, data []byte, opts sbomoci.AttachOptions) (string, error) {
		mu.Lock()
		calls = append(calls, attachCall{subject, opts})
		mu.Unlock()
		opts.Registry.Credential = func(context.Context, string) (auth.Credential, error) { return auth.EmptyCredential, nil }
		return sbomoci.Attach(ctx, subject, data, opts)
	}
	sbomRegistryCredential = func(string, string, *KCLEntities, string) (auth.CredentialFunc, error) { return nil, nil }
	t.Cleanup(func() { sbomReferrerAttach, sbomRegistryCredential = prevAttach, prevCred })
	return &calls
}

// generatedFor is the hook input for an image name whose SBOM is data.
func generatedFor(dir, name string, art release.Artifact, data []byte) sbomGenerated {
	art.SBOM = &release.SBOMRef{Digest: sbom.Digest(data), Format: release.SBOMFormatCycloneDXJSON,
		Generator: "syft@v1.57.2", Source: release.SBOMSourceRegistry, Status: release.SBOMStatusOK}
	return sbomGenerated{ProjectDir: dir, Env: "prod", Version: "v2.0.0", Name: name, Artifact: art, Bytes: data, Built: true,
		CutAt: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)}
}

func splitSubject(subject string) (name, digest string) {
	name, digest, _ = strings.Cut(subject, "@")
	return name, digest
}

func TestAttachSBOMReferrer_AttachesBesideTheImage(t *testing.T) {
	calls := useReferrerRegistry(t)
	f := ocitest.New(t, nil)
	name, digest := splitSubject(f.PutSubject("acme/api", "v1"))
	data := []byte(`{"bomFormat":"CycloneDX","specVersion":"1.6"}`)
	in := generatedFor(t.TempDir(), name, sbomTestImage(digest), data)

	ref, err := attachSBOMReferrer(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ref.Referrer, name+"@sha256:") || ref.Digest != in.Artifact.SBOM.Digest ||
		ref.Status != release.SBOMStatusOK || ref.Reason != "" {
		t.Fatalf("ref = %+v", ref)
	}
	if len(*calls) != 1 {
		t.Fatalf("attach called %d times", len(*calls))
	}
	c := (*calls)[0]
	if c.subject != name+"@"+digest || !c.opts.Created.Equal(in.CutAt) || c.opts.Release != "v2.0.0" || c.opts.Generator != "syft@v1.57.2" {
		t.Errorf("attach call = %+v", c)
	}
	got, source, err := sbomoci.FetchWith(context.Background(), nil, release.Artifact{SBOM: &ref}, "",
		sbomoci.RegistryOptions{Credential: func(context.Context, string) (auth.Credential, error) { return auth.EmptyCredential, nil }})
	if err != nil || source != sbomoci.SourceReferrer || !bytes.Equal(got, data) {
		t.Errorf("the recorded referrer does not read back: (%q, %q, %v)", got, source, err)
	}
}

func TestAttachSBOMReferrer_LeavesAloneWhatItDoesNotAttach(t *testing.T) {
	calls := useReferrerRegistry(t)
	f := ocitest.New(t, nil)
	name, digest := splitSubject(f.PutSubject("acme/api", "v1"))
	data := []byte(`{"bomFormat":"CycloneDX"}`)

	off := t.TempDir()
	if err := os.WriteFile(filepath.Join(off, "forge.yaml"), []byte("name: shop\nvuln:\n  attach_sbom: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	variant := release.Artifact{Kind: release.KindOCI, Mode: release.ModeVariant, Digests: map[string]string{"prod": digest}}
	file := release.Artifact{Kind: release.KindFile, Version: "v2.0.0", Integrity: sha("f")}
	for label, in := range map[string]sbomGenerated{
		"vuln.attach_sbom: false": generatedFor(off, name, sbomTestImage(digest), data),
		"a variant image":         generatedFor(t.TempDir(), name, variant, data),
		"a file artifact":         generatedFor(t.TempDir(), "bin/api", file, data),
	} {
		ref, err := attachSBOMReferrer(context.Background(), in)
		if err != nil || ref != *in.Artifact.SBOM {
			t.Errorf("%s: (%+v, %v), want the ref unchanged", label, ref, err)
		}
	}
	if len(*calls) != 0 || len(f.Requests()) != 0 {
		t.Errorf("attached anyway: %d call(s), %d request(s)", len(*calls), len(f.Requests()))
	}
}

func TestAttachSBOMReferrer_BundleRepositoryIsSkippedWithAReason(t *testing.T) {
	useReferrerRegistry(t)
	f := ocitest.New(t, nil)
	name, digest := splitSubject(f.PutSubject("org/proj/bundle.v1/prod", "v1"))
	in := generatedFor(t.TempDir(), name, sbomTestImage(digest), []byte(`{"bomFormat":"CycloneDX"}`))

	ref, err := attachSBOMReferrer(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Status != release.SBOMStatusOK || ref.Referrer != "" || !strings.Contains(ref.Reason, "bundle") || ref.Digest != in.Artifact.SBOM.Digest {
		t.Errorf("ref = %+v, want status ok, no referrer, the skip reason", ref)
	}
}

// Through the seam: the cut records the referrer, and an attach that fails
// warns and leaves the SBOM recorded without one.
func TestGenerateReleaseSBOMs_AttachesReferrers(t *testing.T) {
	dir := newLedgerTestProject(t, "sbomref")
	useFakeSyft(t, &cutSyft{}, "")
	useReferrerRegistry(t)
	f := ocitest.New(t, func(r *ocitest.Registry) { r.RefuseDelete = true })
	name, digest := splitSubject(f.PutSubject("acme/api", "v1"))
	missing := f.Host() + "/acme/gone"
	_, pgDigest := splitSubject(f.PutSubject("library/postgres", "16"))
	postgres := f.Host() + "/library/postgres"
	artifacts := map[string]release.Artifact{name: sbomTestImage(digest), missing: sbomTestImage(sha("9")), postgres: sbomTestImage(pgDigest)}
	var warn bytes.Buffer
	generateReleaseSBOMs(context.Background(), sbomCut{ProjectDir: dir, Env: "prod", Version: "v3.0.0",
		CreatedAt: time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC),
		Built:     map[string]bool{name: true, missing: true}}, artifacts, &warn)

	got := artifacts[name].SBOM
	if got == nil || got.Status != release.SBOMStatusOK || !strings.HasPrefix(got.Referrer, name+"@sha256:") {
		t.Fatalf("%s: SBOM %+v", name, got)
	}
	lost := artifacts[missing].SBOM
	if lost == nil || lost.Status != release.SBOMStatusOK || !lost.HasSBOM() || lost.Referrer != "" {
		t.Errorf("%s: SBOM %+v, want recorded without a referrer", missing, lost)
	}
	if !strings.Contains(warn.String(), "not attached beside the image") || !strings.Contains(warn.String(), missing) {
		t.Errorf("the failed attach was not warned:\n%s", warn.String())
	}
	// A pinned third-party image: recorded, said why, and never warned.
	pg := artifacts[postgres].SBOM
	if pg == nil || pg.Status != release.SBOMStatusOK || pg.Referrer != "" ||
		pg.Reason != "referrer not attached: "+postgres+"@"+pgDigest+" is not pushed by this project" {
		t.Errorf("%s: SBOM %+v", postgres, pg)
	}
	if strings.Contains(warn.String(), "postgres") {
		t.Errorf("a third-party image was warned about:\n%s", warn.String())
	}
	if err := (release.Release{Version: "v3.0.0", Artifacts: artifacts}).Validate(); err != nil {
		t.Errorf("the recorded artifacts no longer validate: %v", err)
	}
	// The ledger copy is there whatever the registry did.
	store := testStore(t, dir)
	for _, n := range []string{name, missing} {
		if _, err := store.GetSBOM(artifacts[n].SBOM.Digest); err != nil {
			t.Errorf("%s: ledger copy: %v", n, err)
		}
	}
}

// Only a subject this project pushed gets an attach attempt.
func TestAttachSBOMReferrer_OnlyWhereTheProjectPushed(t *testing.T) {
	calls := useReferrerRegistry(t)
	prevName := hostedProjectName
	hostedProjectName = func() string { return "shop" }
	t.Cleanup(func() { hostedProjectName = prevName })
	f := ocitest.New(t, nil)
	data := []byte(`{"bomFormat":"CycloneDX"}`)
	hosted := &KCLEntities{ControlPlane: &ControlPlaneEntity{RegistryHost: f.Host()}}
	base := platformPushBase(hosted)
	if base == "" {
		t.Fatal("no push base for the test control plane")
	}
	builds := func(spec string) *KCLEntities {
		w := WorkloadEntity{Name: "api", Image: "api"}
		w.Spec.Image = spec
		return &KCLEntities{Workloads: []WorkloadEntity{w}}
	}
	pinned := func(spec string) *KCLEntities {
		w := WorkloadEntity{Name: "db"} // forge does not build it
		w.Spec.Image = spec
		return &KCLEntities{Workloads: []WorkloadEntity{w}}
	}
	for _, tc := range []struct {
		label    string
		repo     string
		entities func(name string) *KCLEntities
		attached bool
	}{
		{"under the platform push base", strings.TrimPrefix(base, f.Host()+"/") + "/api", func(string) *KCLEntities { return hosted }, true},
		{"the repository a built workload resolves to", "acme/api", func(name string) *KCLEntities { return builds(name + ":v1") }, true},
		{"a pinned third-party image", "library/postgres", func(name string) *KCLEntities { return pinned(name + ":16") }, false},
		{"no declaration at all", "vendor/agent", func(string) *KCLEntities { return nil }, false},
	} {
		name, digest := splitSubject(f.PutSubject(tc.repo, "v1"))
		in := generatedFor(t.TempDir(), name, sbomTestImage(digest), data)
		in.Built, in.Entities = false, tc.entities(name)
		before := len(*calls)

		ref, err := attachSBOMReferrer(context.Background(), in)
		if err != nil {
			t.Fatalf("%s: %v", tc.label, err)
		}
		if got := len(*calls) > before; got != tc.attached {
			t.Errorf("%s: attach attempted = %v, want %v", tc.label, got, tc.attached)
		}
		if tc.attached && !strings.HasPrefix(ref.Referrer, name+"@sha256:") {
			t.Errorf("%s: ref = %+v, want a referrer", tc.label, ref)
		}
		if !tc.attached && (ref.Referrer != "" || ref.Status != release.SBOMStatusOK ||
			ref.Reason != "referrer not attached: "+name+"@"+digest+" is not pushed by this project") {
			t.Errorf("%s: ref = %+v", tc.label, ref)
		}
	}
}

func TestRegistryCredentialFor_PlatformHostTakesTheControlPlaneIdentity(t *testing.T) {
	const host = "registry.example.test"
	e := &KCLEntities{ControlPlane: &ControlPlaneEntity{RegistryHost: host}}
	dir := t.TempDir()
	key := dir + "\x00prod"
	bundleRegistryAuths.Store(key, bundleRegistryAuth{Host: host, credential: func(context.Context) (string, error) { return "rlat_test", nil }})
	t.Cleanup(func() { bundleRegistryAuths.Delete(key) })

	cred, err := registryCredentialFor(dir, "prod", e, host)
	if err != nil || cred == nil {
		t.Fatalf("platform host: (%v, %v), want the control-plane credential", cred, err)
	}
	if c, err := cred(context.Background(), host); err != nil || c.Password != "rlat_test" {
		t.Errorf("credential = (%+v, %v)", c, err)
	}
	for _, other := range []string{"ghcr.io", "localhost:5000", "127.0.0.1:5000"} {
		if cred, err := registryCredentialFor(dir, "prod", e, other); err != nil || cred != nil {
			t.Errorf("%s: (%v, %v), want the docker store (nil)", other, cred != nil, err)
		}
	}
	if cred, err := registryCredentialFor(dir, "prod", nil, host); err != nil || cred != nil {
		t.Errorf("no control plane: (%v, %v), want the docker store (nil)", cred != nil, err)
	}
}
