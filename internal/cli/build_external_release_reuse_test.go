package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/reliant-labs/forge/internal/buildtarget"
	"github.com/reliant-labs/forge/pkg/release"
)

// fakeReleaseRegistry is a registry that serves manifests by digest and tags
// pointing at them. Every push of the fake build makes a NEW digest: the build
// is not reproducible, which is the case reuse exists for.
type fakeReleaseRegistry struct {
	mu        sync.Mutex
	manifests map[string]bool   // <repo>@<digest>
	tags      map[string]string // <repo>:<tag> → digest
	pushes    int
	retags    []string
	retagErr  error
}

func (r *fakeReleaseRegistry) push(repo, tag string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pushes++
	d := "sha256:" + strings.Repeat(fmt.Sprintf("%x", r.pushes%16), 64)
	r.manifests[repo+"@"+d] = true
	r.tags[repo+":"+tag] = d
}

func (r *fakeReleaseRegistry) resolve(_ context.Context, ref string) (string, []string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i := strings.Index(ref, "@"); i >= 0 {
		if r.manifests[ref] {
			return ref[i+1:], []string{"linux/amd64"}, nil
		}
		return "", nil, errors.New("manifest unknown")
	}
	if d, ok := r.tags[ref]; ok && r.manifests[ref[:strings.LastIndex(ref, ":")]+"@"+d] {
		return d, []string{"linux/amd64"}, nil
	}
	return "", nil, errors.New("manifest unknown")
}

func (r *fakeReleaseRegistry) retag(_ context.Context, src, dst string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retags = append(r.retags, dst+"="+src)
	if r.retagErr != nil {
		return r.retagErr
	}
	r.tags[dst] = src[strings.Index(src, "@")+1:]
	return nil
}

func (r *fakeReleaseRegistry) forget(repo, digest string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.manifests, repo+"@"+digest)
}

// releaseFixture cuts releases of ShellBuilds against a fake shell runner, a
// fake registry and an in-memory ledger: each cut harvests the build state
// exactly as `forge env build --release` does and records it newest first.
type releaseFixture struct {
	t       *testing.T
	projDir string
	h       *inputsHarness
	reg     *fakeReleaseRegistry
	ledger  []release.Release
}

func newReleaseFixture(t *testing.T, names ...string) *releaseFixture {
	t.Helper()
	f := &releaseFixture{t: t, projDir: filepath.Join(t.TempDir(), "proj"), h: newInputsHarness(t, names...),
		reg: &fakeReleaseRegistry{manifests: map[string]bool{}, tags: map[string]string{}}}
	writeInput(t, f.projDir, "src/main.go", "package main")
	writeInput(t, f.projDir, "src/README.md", "docs")
	oldRetag, oldCandidates := externalImageRetag, releaseReuseCandidatesFn
	t.Cleanup(func() { externalImageRetag, releaseReuseCandidatesFn = oldRetag, oldCandidates })
	run := externalShellBuild
	externalShellBuild = func(ctx context.Context, spec buildtarget.Spec) buildtarget.BuildResult {
		res := run(ctx, spec)
		if res.Err == nil {
			f.reg.push(imageRepository(spec.Image), spec.Tag)
		}
		return res
	}
	externalImageDigestResolver = f.reg.resolve
	externalImageRetag = f.reg.retag
	releaseReuseCandidatesFn = func(context.Context, string, string) ([]release.Release, error) {
		return append([]release.Release(nil), f.ledger...), nil
	}
	return f
}

// svc is the api ShellBuild as a release's render produces it: the release
// tag and the checkout's path are composed into the command, as
// `TAG=${tag}` and `${file.workdir()}` compose them.
func (f *releaseFixture) svc(version string, inputs ...string) WorkloadEntity {
	w := inputsSvc("api", inputs...)
	w.Build.Shell.Cmd = "TAG=" + version + " " + f.projDir + "/scripts/build.sh"
	return w
}

// cut builds version and records the release the build state harvests.
func (f *releaseFixture) cut(version string, svc WorkloadEntity, opts buildOptions) buildResult {
	f.t.Helper()
	opts.env, opts.release = "prod", version
	r := f.h.build(f.t, f.projDir, opts, svc)["api"]
	if r.err != nil {
		f.t.Fatalf("cut %s: %v", version, r.err)
	}
	rel := release.Release{Version: version, Artifacts: harvestReleaseArtifacts(f.projDir, "prod")}
	if err := rel.Validate(); err != nil {
		f.t.Fatalf("cut %s harvested an invalid release: %v", version, err)
	}
	f.ledger = append([]release.Release{rel}, f.ledger...)
	return r
}

func (f *releaseFixture) runs() int32 { return f.h.runs["api"].Load() }

func (f *releaseFixture) artifact(version string) release.Artifact {
	for _, r := range f.ledger {
		if r.Version == version {
			return r.Artifacts["reg.example/api"]
		}
	}
	f.t.Fatalf("no release %s", version)
	return release.Artifact{}
}

func TestReleaseBuild_ReusesThePreviousReleasesImageWhenInputsAreUnchanged(t *testing.T) {
	f := newReleaseFixture(t, "api")

	// The first release has nothing to compare with: it runs and records
	// what the next one compares against.
	r1 := f.cut("v1", f.svc("v1", "**/*.go"), buildOptions{})
	if r1.upToDate || f.runs() != 1 {
		t.Fatalf("first release: upToDate=%v runs=%d, want a real run", r1.upToDate, f.runs())
	}
	v1 := f.artifact("v1")
	if !release.ValidDigest(v1.InputsDigest) || v1.CarriedFrom != "" {
		t.Fatalf("first release's artifact = %+v, want an inputs digest and no carry", v1)
	}

	// Unchanged inputs, a new tag in the command: no build, the same
	// digest, and the new release's tag points at it.
	r2 := f.cut("v2", f.svc("v2", "**/*.go"), buildOptions{})
	if !r2.upToDate || f.runs() != 1 {
		t.Fatalf("unchanged release: upToDate=%v runs=%d, want the image reused and no build", r2.upToDate, f.runs())
	}
	v1Digest := v1.Digests[release.SharedVariant]
	if r2.digest != v1Digest || r2.repository != "reg.example/api" {
		t.Fatalf("reused result = %s@%s, want reg.example/api@%s", r2.repository, r2.digest, v1Digest)
	}
	if got, _, err := f.reg.resolve(context.Background(), "reg.example/api:v2"); err != nil || got != v1Digest {
		t.Fatalf("reg.example/api:v2 = %s (%v), want it tagged onto %s", got, err, v1Digest)
	}
	v2 := f.artifact("v2")
	if v2.Digests[release.SharedVariant] != v1Digest || v2.CarriedFrom != "v1" || v2.InputsDigest != v1.InputsDigest {
		t.Fatalf("second release's artifact = %+v, want v1's digest carried from v1 with the same inputs digest", v2)
	}
	st, err := buildtarget.ReadState(f.projDir, "prod", "api")
	if err != nil || st == nil || st.Tag != "v2" || st.Digest != v1Digest || st.CarriedFrom != "v1" {
		t.Fatalf("build state after a reuse = %+v (%v), want tag v2 on v1's digest", st, err)
	}

	// An undeclared file is not an input.
	writeInput(t, f.projDir, "src/README.md", "edited docs")
	if r := f.cut("v3", f.svc("v3", "**/*.go"), buildOptions{}); !r.upToDate || f.runs() != 1 {
		t.Fatalf("an undeclared edit rebuilt (runs=%d)", f.runs())
	}
}

func TestReleaseBuild_RebuildsWhenItCannotProveReuse(t *testing.T) {
	cases := []struct {
		name string
		// prepare runs after a first release v1 is cut and returns the
		// workload v2 builds.
		prepare func(f *releaseFixture) WorkloadEntity
		opts    buildOptions
	}{
		{name: "a declared input changed", prepare: func(f *releaseFixture) WorkloadEntity {
			writeInput(f.t, f.projDir, "src/main.go", "package main // edited")
			return f.svc("v2", "**/*.go")
		}},
		{name: "the command changed", prepare: func(f *releaseFixture) WorkloadEntity {
			w := f.svc("v2", "**/*.go")
			w.Build.Shell.Cmd += " --no-cache"
			return w
		}},
		{name: "the env changed", prepare: func(f *releaseFixture) WorkloadEntity {
			w := f.svc("v2", "**/*.go")
			w.Build.Shell.Env = map[string]string{"MODE": "debug"}
			return w
		}},
		{name: "the registry no longer has the digest", prepare: func(f *releaseFixture) WorkloadEntity {
			f.reg.forget("reg.example/api", f.artifact("v1").Digests[release.SharedVariant])
			return f.svc("v2", "**/*.go")
		}},
		{name: "the release tag cannot be pointed at it", prepare: func(f *releaseFixture) WorkloadEntity {
			f.reg.retagErr = errors.New("denied")
			return f.svc("v2", "**/*.go")
		}},
		{name: "the previous release recorded no inputs digest", prepare: func(f *releaseFixture) WorkloadEntity {
			old := f.ledger[0].Artifacts["reg.example/api"]
			old.InputsDigest = ""
			f.ledger[0].Artifacts["reg.example/api"] = old
			return f.svc("v2", "**/*.go")
		}},
		{name: "no previous release", prepare: func(f *releaseFixture) WorkloadEntity {
			f.ledger = nil
			return f.svc("v2", "**/*.go")
		}},
		{name: "--rebuild", opts: buildOptions{rebuild: true}, prepare: func(f *releaseFixture) WorkloadEntity {
			return f.svc("v2", "**/*.go")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReleaseFixture(t, "api")
			v1 := f.cut("v1", f.svc("v1", "**/*.go"), buildOptions{})
			r := f.cut("v2", tc.prepare(f), tc.opts)
			if r.upToDate || f.runs() != 2 {
				t.Fatalf("upToDate=%v runs=%d, want the command run", r.upToDate, f.runs())
			}
			if r.digest == "" || r.digest == v1.digest {
				t.Fatalf("rebuilt digest = %q, want the new build's (not v1's %s)", r.digest, v1.digest)
			}
			if v2 := f.artifact("v2"); v2.CarriedFrom != "" || !release.ValidDigest(v2.InputsDigest) {
				t.Fatalf("rebuilt release's artifact = %+v, want no carry and a fresh inputs digest", v2)
			}
		})
	}
}

// A step with no inputs behaves as it always has: every release runs it, and
// its release records no inputs digest.
func TestReleaseBuild_NoDeclaredInputsAlwaysRuns(t *testing.T) {
	f := newReleaseFixture(t, "api")
	f.cut("v1", f.svc("v1"), buildOptions{})
	r := f.cut("v2", f.svc("v2"), buildOptions{})
	if r.upToDate || f.runs() != 2 {
		t.Fatalf("upToDate=%v runs=%d, want every release to run a step without inputs", r.upToDate, f.runs())
	}
	if a := f.artifact("v2"); a.InputsDigest != "" || a.CarriedFrom != "" {
		t.Fatalf("artifact = %+v, want no inputs evidence", a)
	}
	if len(f.reg.retags) != 0 {
		t.Fatalf("retagged %v for a step without inputs", f.reg.retags)
	}
}

// The release fingerprint masks what changes on every cut — the tag and the
// checkout's location — and nothing else.
func TestReleaseInputsFingerprint_MasksTagAndCheckoutLocationOnly(t *testing.T) {
	fingerprint := func(projDir, tag string, mut func(*WorkloadEntity)) string {
		t.Helper()
		writeInput(t, projDir, "src/main.go", "package main")
		w := inputsSvc("api", "**/*.go")
		w.Build.Shell.Cmd = "TAG=" + tag + " " + projDir + "/scripts/build.sh"
		w.Build.Shell.Env = map[string]string{"ROOT": filepath.Dir(projDir) + "/reliant", "VERSION_TAG": tag}
		if mut != nil {
			mut(&w)
		}
		opts := buildOptions{env: "prod", release: tag}
		spec := externalBuildSpec(w, opts, tag, projDir)
		fp, err := releaseInputsFingerprint(w, spec, filepath.Join(projDir, "src"), projDir, opts)
		if err != nil {
			t.Fatal(err)
		}
		return fp.Digest
	}
	a := fingerprint(filepath.Join(t.TempDir(), "proj"), "v1.0.0", nil)
	if b := fingerprint(filepath.Join(t.TempDir(), "elsewhere", "proj"), "v1.0.1", nil); a != b {
		t.Errorf("another checkout location and tag changed the release fingerprint: %s vs %s", a, b)
	}
	if c := fingerprint(filepath.Join(t.TempDir(), "proj"), "v1.0.1", func(w *WorkloadEntity) {
		w.Build.Shell.Cmd += " --platform linux/arm64"
	}); a == c {
		t.Error("a changed command must change the release fingerprint")
	}
	if d := fingerprint(filepath.Join(t.TempDir(), "proj"), "v1.0.1", func(w *WorkloadEntity) {
		w.Image = "reg.example/other"
	}); a == d {
		t.Error("another destination repository must change the release fingerprint")
	}
}

// A hosted ledger carries the inputs evidence on the wire, and a malformed
// value from a server never makes the release unreadable.
func TestHostedWire_CarriesInputsEvidence(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	r := release.Release{Version: "v2", Artifacts: map[string]release.Artifact{"reg.example/api": {
		Kind: release.KindOCI, Mode: release.ModeShared, Digests: map[string]string{release.SharedVariant: pushedDigest},
		InputsDigest: d, CarriedFrom: "v1",
	}}}
	rows := releaseToWire(r)
	back, err := releaseFromWire(wireRelease{Version: "v2", Artifacts: rows})
	if err != nil || back.Artifacts["reg.example/api"].InputsDigest != d || back.Artifacts["reg.example/api"].CarriedFrom != "v1" {
		t.Fatalf("round trip = %+v (%v), want the inputs evidence kept", back.Artifacts, err)
	}
	rows[0].InputsDigest = "not-a-digest"
	back, err = releaseFromWire(wireRelease{Version: "v2", Artifacts: rows})
	if err != nil || back.Artifacts["reg.example/api"].InputsDigest != "" {
		t.Fatalf("malformed inputs digest = %+v (%v), want it dropped and the release readable", back.Artifacts, err)
	}
}
