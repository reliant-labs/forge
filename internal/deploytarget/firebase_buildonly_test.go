package deploytarget

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// emittingRunner is a commandRunner test double that records calls (like
// fakeRunner) but, for the build step, MATERIALIZES the frontend's build
// output dir + an index.html — simulating what a real `npm run build`
// would emit. This lets a build-only frontend's output actually exist on
// disk so a sibling FirebaseHosting frontend's assemble (which os.Stats
// the bundle src) succeeds.
type emittingRunner struct {
	calls    []string
	envCalls []map[string]string

	// emitDir is an absolute dir the build step creates an index.html in
	// when the recorded command mentions "npm run build". Empty = emit
	// nothing.
	emitDir string
}

func (r *emittingRunner) record(name string, args []string, env map[string]string) error {
	full := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, full)
	var cp map[string]string
	if env != nil {
		cp = make(map[string]string, len(env))
		for k, v := range env {
			cp[k] = v
		}
	}
	r.envCalls = append(r.envCalls, cp)
	if r.emitDir != "" && strings.Contains(full, "npm run build") {
		if err := os.MkdirAll(r.emitDir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(r.emitDir, "index.html"), []byte("<admin/>"), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (r *emittingRunner) Run(_ context.Context, name string, args ...string) error {
	return r.record(name, args, nil)
}

func (r *emittingRunner) RunWithEnv(_ context.Context, env map[string]string, name string, args ...string) error {
	return r.record(name, args, env)
}

func (r *emittingRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	return nil, r.record(name, args, nil)
}

// TestBuildOnlyThenFirebaseAssembles is the end-to-end contract for
// `deploy = None` ⇒ build-only: a build-only frontend (admin-web, env
// injected) is BUILT first, emitting admin-web/out; then a sibling
// FirebaseHosting frontend bundles admin-web/out into its hosting tree.
// The assemble must succeed (no os.Stat "no such file" error) BECAUSE the
// build-only build ran first and produced the dir.
func TestBuildOnlyThenFirebaseAssembles(t *testing.T) {
	projectDir := t.TempDir()
	staging := filepath.Join(t.TempDir(), "public")

	// The build-only frontend emits its static export here when built.
	emittedOut := filepath.Join(projectDir, "admin-web", "out")

	buildOnly := BuildOnlyFrontend{
		Name:      "admin-web",
		Path:      "admin-web",
		DevRunner: "npm",
		BuildEnv: map[string]string{
			"NEXT_PUBLIC_API_URL": "https://api.staging.example.com",
		},
		PublicDir: "out",
	}

	buildRunner := &emittingRunner{emitDir: emittedOut}
	prov := FirebaseProvider{ProjectDir: projectDir, Runner: buildRunner}

	// 1) Build the build-only frontend FIRST — this materializes
	//    admin-web/out (via the emitting runner's "npm run build").
	if err := prov.BuildOnly(context.Background(), []BuildOnlyFrontend{buildOnly}, false); err != nil {
		t.Fatalf("build-only: %v", err)
	}

	// Assert the build emitted the dir.
	if _, err := os.Stat(emittedOut); err != nil {
		t.Fatalf("build-only did not emit %s: %v", emittedOut, err)
	}

	// Assert env injection: a build call carried NODE_ENV=production +
	// the inline NEXT_PUBLIC_API_URL.
	foundEnv := false
	for _, env := range buildRunner.envCalls {
		if env != nil && env["NODE_ENV"] == "production" && env["NEXT_PUBLIC_API_URL"] == "https://api.staging.example.com" {
			foundEnv = true
		}
	}
	if !foundEnv {
		t.Errorf("expected a build-only call with NODE_ENV=production + NEXT_PUBLIC_API_URL injected; calls=%v envs=%v", buildRunner.calls, buildRunner.envCalls)
	}
	if !strings.Contains(strings.Join(buildRunner.calls, "\n"), "npm install") {
		t.Errorf("expected an npm install call; got %v", buildRunner.calls)
	}

	// 2) Now a sibling FirebaseHosting frontend bundles admin-web/out into
	//    its hosting tree. This frontend's OWN public_dir is reliant-web/dist
	//    (pre-created); the admin-web/out it bundles exists ONLY because the
	//    build-only build ran first.
	writeFile(t, filepath.Join(projectDir, "reliant-web", "dist", "index.html"), "<spa/>")

	fbRunner := &fakeRunner{}
	fbProv := FirebaseProvider{ProjectDir: projectDir, Runner: fbRunner, StagingRoot: staging}
	fe := FirebaseFrontend{
		Name:      "reliant-web",
		Path:      "reliant-web",
		DevRunner: "npm",
		Spec: FirebaseHostingSpec{
			Project:   "reliant-nonprod-490701",
			Site:      "reliant-staging",
			PublicDir: "dist",
			Bundle: []FirebaseBundleSpec{
				// Bundle the build-only frontend's output under /admin.
				{Src: "admin-web/out", Dest: "admin"},
			},
		},
	}
	if err := fbProv.Deploy(context.Background(), ServiceGroup{ProviderID: fbProv.Name(), Frontends: []FirebaseFrontend{fe}}); err != nil {
		// The os.Stat "no such file" failure would surface here if the
		// build-only build hadn't produced admin-web/out first.
		t.Fatalf("firebase deploy (assemble) failed: %v", err)
	}

	// The assembled tree carries the build-only frontend's output under
	// /admin and the firebase frontend's own SPA at root.
	assertFileContains(t, filepath.Join(staging, "index.html"), "<spa/>")
	assertFileContains(t, filepath.Join(staging, "admin", "index.html"), "<admin/>")
}

// TestBuildOnlyDryRun asserts --dry-run prints the build-only plan (install
// + build commands, injected env keys, emitted dir) and runs NOTHING.
func TestBuildOnlyDryRun(t *testing.T) {
	projectDir := t.TempDir()
	fake := &fakeRunner{}
	prov := FirebaseProvider{ProjectDir: projectDir, Runner: fake}

	fe := BuildOnlyFrontend{
		Name:      "admin-web",
		Path:      "admin-web",
		DevRunner: "npm",
		BuildEnv:  map[string]string{"NEXT_PUBLIC_API_URL": "https://api.staging.example.com"},
		PublicDir: "out",
	}

	out := captureStdout(t, func() {
		if err := prov.BuildOnly(context.Background(), []BuildOnlyFrontend{fe}, true); err != nil {
			t.Fatalf("dry-run build-only: %v", err)
		}
	})

	if len(fake.calls) != 0 {
		t.Fatalf("dry-run executed %d command(s); want 0: %v", len(fake.calls), fake.calls)
	}

	for _, want := range []string{
		`build-only plan for frontend "admin-web"`,
		"npm install",
		"npm run build",
		"NEXT_PUBLIC_API_URL=https://api.staging.example.com",
		filepath.Join(projectDir, "admin-web", "out"), // emitted dir
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q\n---\n%s", want, out)
		}
	}
}
