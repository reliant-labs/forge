package codemod

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testdata/testscapture/<case>/in is a project's files before the codemod;
// want/ holds every file the codemod must rewrite, as it must leave it, and
// next.txt the steps it must report (one per line, in order). A file in in/
// with no want/ twin must come out byte-identical.
//
//   - v180-service: `forge project new demo --service users --frontend web`
//     and `forge scaffold frontend admin --kind vite-spa` by forge v1.8.0,
//     with the pin moved to v1.9.0 (as `forge project upgrade` leaves it
//     before its playbooks run). Its want/ Taskfile.yml, .air.toml, ci.yml,
//     e2e.yml and Vitest configs are the SAME project scaffolded by this
//     forge, copied verbatim: the codemod moves a v1.8 scaffold to exactly
//     what a new one writes.
//   - v180-cli: `forge project new democli --kind cli`, both sides.
//   - goldens: internal/templates' goldens at v1.8.0 (in) and now (want,
//     read from internal/templates/testdata/golden through links.txt), with
//     ci.test.coverage set: the coverage step follows `task coverage` onto
//     coverage.html, and forge.yaml keeps the legacy key. A template change
//     this codemod does not make (ci.yml's gate-tools comment) is carried
//     into in/ as well, so the comparison stays about the codemod.
//   - dockerfile-pgo: today's Dockerfile golden without its cover stages.
//   - flag-edits: the v1.8 Taskfile with flags added to two lanes; they stay.
//   - customised: a Taskfile, Dockerfile and .air.toml a project edited, so
//     nothing is written and every edit is a step.
//
// Regenerate a want/ file only from a real scaffold, never from the
// codemod's own output: it is what makes the comparison mean anything.
func TestTestsCaptureGoldens(t *testing.T) {
	cases, err := os.ReadDir(filepath.Join("testdata", "testscapture"))
	if err != nil {
		t.Fatal(err)
	}
	c, ok := Lookup("tests-capture")
	if !ok {
		t.Fatal("tests-capture is not registered")
	}
	for _, tc := range cases {
		t.Run(tc.Name(), func(t *testing.T) {
			root := filepath.Join("testdata", "testscapture", tc.Name())
			dir := t.TempDir()
			in := copyFixtureTree(t, filepath.Join(root, "in"), dir)

			rep, err := c.Run(dir, Options{Write: true})
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{}
			_ = filepath.WalkDir(filepath.Join(root, "want"), func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				rel, _ := filepath.Rel(filepath.Join(root, "want"), p)
				b, _ := os.ReadFile(p)
				want[filepath.ToSlash(rel)] = string(b)
				return nil
			})
			// links.txt: "<rel> <path>" — a want file read from where it lives
			// (internal/templates' goldens), so it cannot go stale.
			if b, err := os.ReadFile(filepath.Join(root, "links.txt")); err == nil {
				for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
					rel, target, _ := strings.Cut(l, " ")
					want[rel] = readFile(t, root, target)
				}
			}
			for rel, before := range in {
				got := readFile(t, dir, rel)
				exp, rewritten := want[rel]
				if !rewritten {
					exp = before
				}
				if got != exp {
					t.Errorf("%s:\n%s", rel, tcUnifiedDiff(rel, exp, got))
				}
			}
			for rel := range want {
				if _, ok := in[rel]; !ok {
					t.Errorf("want/%s has no in/ twin", rel)
				}
			}
			wantNext := ""
			if b, err := os.ReadFile(filepath.Join(root, "next.txt")); err == nil {
				wantNext = string(b)
			}
			if got := tcNextText(rep.Next); got != wantNext {
				t.Errorf("Next:\n%s", tcUnifiedDiff("next.txt", wantNext, got))
			}
			if (len(want) > 0) != rep.Changed() {
				t.Errorf("Changed() = %v with %d rewritten file(s); rewrites: %q", rep.Changed(), len(want), rep.Rewrites)
			}

			// Idempotent: a second run over its own output rewrites nothing.
			again, err := c.Run(dir, Options{Write: false})
			if err != nil {
				t.Fatal(err)
			}
			if again.Changed() || again.Diff != "" {
				t.Errorf("second run rewrites %q:\n%s", again.Rewrites, again.Diff)
			}
			// …and finds nothing new to ask for: a step its own output raises
			// is an edit it does not recognise as done.
			asked := map[string]bool{}
			for _, n := range rep.Next {
				asked[n] = true
			}
			for _, n := range again.Next {
				if !asked[n] {
					t.Errorf("second run asks for a new step: %s", n)
				}
			}
		})
	}
}

// TestTestsCaptureDryRunShowsTheDiffAndWritesNothing: --dry-run's report is
// the diff a write would make, and the tree is untouched.
func TestTestsCaptureDryRunShowsTheDiffAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	in := copyFixtureTree(t, filepath.Join("testdata", "testscapture", "v180-service", "in"), dir)
	c, _ := Lookup("tests-capture")
	rep, err := c.Run(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for rel, before := range in {
		if got := readFile(t, dir, rel); got != before {
			t.Errorf("dry run wrote %s", rel)
		}
	}
	for _, want := range []string{
		"--- a/Taskfile.yml\n+++ b/Taskfile.yml\n",
		"+        {{.SLOT}} {{if .RUN}}{{.FORGE}} test capture --run '{{.RUN}}' --lane unit -- {{end}}go test {{.GOTESTRACE}} -count=1\n",
		"--- a/.github/workflows/release.yml\n",
		"+FROM alpine:3.21 AS cover\n",
	} {
		if !strings.Contains(rep.Diff, want) {
			t.Errorf("dry-run diff lacks %q", want)
		}
	}
}

// TestTestsCaptureOldPinKeepsTheReleaseGate: a project pinned to a forge
// without `forge test report` keeps verify-test-run, whose step would
// otherwise fail every release, and is told to move the pin.
func TestTestsCaptureOldPinKeepsTheReleaseGate(t *testing.T) {
	dir := t.TempDir()
	in := copyFixtureTree(t, filepath.Join("testdata", "testscapture", "v180-service", "in"), dir)
	fy := strings.Replace(in["forge.yaml"], "forge_version: v1.9.0", "forge_version: v1.8.0", 1)
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte(fy), 0o644); err != nil {
		t.Fatal(err)
	}
	c, _ := Lookup("tests-capture")
	rep, err := c.Run(dir, Options{Write: true})
	if err != nil {
		t.Fatal(err)
	}
	rel := ".github/workflows/release.yml"
	if got := readFile(t, dir, rel); got != in[rel] {
		t.Errorf("%s was rewritten under a v1.8.0 pin", rel)
	}
	if !strings.Contains(tcNextText(rep.Next), "pins forge v1.8.0, which has no `forge test report`") {
		t.Errorf("Next does not name the pin:\n%s", tcNextText(rep.Next))
	}
	// The lanes still move: they degrade to today's command under an older forge.
	if !strings.Contains(readFile(t, dir, "Taskfile.yml"), "test capture") {
		t.Error("Taskfile lanes were not rewritten")
	}
}

func tcNextText(next []string) string {
	if len(next) == 0 {
		return ""
	}
	return strings.Join(next, "\n") + "\n"
}

// copyFixtureTree copies src into dst and returns every file's content by
// slash-separated relative path.
func copyFixtureTree(t *testing.T, src, dst string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(b)
		return os.WriteFile(to, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestTestsCaptureBlocksAreTodaysTemplates holds every fragment the codemod
// writes to the text today's templates render, so a template change that
// does not reach the codemod fails here rather than in a project upgraded to
// a shape no scaffold has.
func TestTestsCaptureBlocksAreTodaysTemplates(t *testing.T) {
	golden := func(name string) string {
		return readFile(t, filepath.Join("..", "templates", "testdata", "golden"), name)
	}
	tmpl := func(rel string) string { return readFile(t, filepath.Join("..", "templates"), rel) }
	sources := map[string]string{
		"Taskfile.yml.golden": golden("Taskfile.yml.golden"),
		"ci_full.yml.golden":  golden("ci_full.yml.golden"),
		"e2e.yml.golden":      golden("e2e.yml.golden"),
		"Dockerfile.golden":   golden("Dockerfile.golden"),
		"air.toml.tmpl":       tmpl("project/air.toml.tmpl"),
		"release.yml.tmpl":    tmpl("ci/github/release.yml.tmpl"),
		"ci.yml.tmpl":         tmpl("ci/github/ci.yml.tmpl"),
		"e2e.yml.tmpl":        tmpl("ci/github/e2e.yml.tmpl"),
		"vitest.config.ts":    tmpl("frontend/nextjs/vitest.config.ts"),
		"vite.config.ts.tmpl": tmpl("frontend/vite-spa/vite.config.ts.tmpl"),
		"cli Taskfile":        readFile(t, filepath.Join("testdata", "testscapture", "v180-cli", "want"), "Taskfile.yml"),
	}
	for block, source := range map[string]string{
		"taskfile_vars_service":                    "Taskfile.yml.golden",
		"taskfile_unit_comment_service":            "Taskfile.yml.golden",
		"taskfile_frontend_new":                    "Taskfile.yml.golden",
		"taskfile_integration_comment_new_service": "Taskfile.yml.golden",
		"taskfile_e2e_comment":                     "Taskfile.yml.golden",
		"taskfile_e2e_vars":                        "Taskfile.yml.golden",
		"taskfile_e2e_collect_cmds":                "Taskfile.yml.golden",
		"taskfile_e2e_collect_task":                "Taskfile.yml.golden",
		"taskfile_test_report":                     "Taskfile.yml.golden",
		"taskfile_coverage_new_service":            "Taskfile.yml.golden",
		"taskfile_vars_cli":                        "cli Taskfile",
		"taskfile_unit_comment_cli":                "cli Taskfile",
		"taskfile_integration_comment_cli":         "cli Taskfile",
		"taskfile_coverage_new_cli":                "cli Taskfile",
		"ci_report":                                "ci_full.yml.golden",
		"ci_coverage_new":                          "ci_full.yml.golden",
		"ci_k3d_comment_new":                       "ci.yml.tmpl",
		"e2e_report":                               "e2e.yml.golden",
		"e2e_k3d_comment":                          "e2e.yml.tmpl",
		"release_test_new":                         "release.yml.tmpl",
		"air_comment":                              "air.toml.tmpl",
		"air_windows_comment":                      "air.toml.tmpl",
		"dockerfile_cover_builder_comment":         "Dockerfile.golden",
		"dockerfile_cover_comment":                 "Dockerfile.golden",
		"vitest_next_const":                        "vitest.config.ts",
		"vitest_next_spread":                       "vitest.config.ts",
		"vitest_vite_const":                        "vite.config.ts.tmpl",
		"vitest_vite_comment":                      "vite.config.ts.tmpl",
		"vitest_vite_spread":                       "vite.config.ts.tmpl",
	} {
		if !strings.Contains(sources[source], tcBlock(block)) {
			t.Errorf("testscapture/%s.txt is not in %s: copy it from there again", block, source)
		}
	}
	// The cli fixture is a real scaffold of this forge; hold it to the
	// template's own lane line so it cannot go stale either.
	if !strings.Contains(tmpl("project/Taskfile.cli.yml.tmpl"), "--lane unit -- {{end}}`}}go test {{`{{.GOTESTRACE}}`}} -count=1") {
		t.Error("Taskfile.cli.yml.tmpl's unit lane moved: re-scaffold testdata/testscapture/v180-cli/want")
	}
}

// TestTestsCaptureK3DLanesCollectFromTheE2EEnv: a job that deploys the e2e
// env runs its lane with ENV=e2e, in both scaffolded spellings (e2e.yml's
// `run: task test:e2e`, ci.yml's `run: |` block), and gets its Report step.
func TestTestsCaptureK3DLanesCollectFromTheE2EEnv(t *testing.T) {
	taskfile := readFile(t, filepath.Join("testdata", "testscapture", "v180-service", "in"), "Taskfile.yml")
	e2e := "jobs:\n  k3d:\n    steps:\n      - name: Deploy\n        run: forge env deploy e2e --yes\n\n" +
		"      - name: Run E2E tests\n        run: task test:e2e\n        env:\n          E2E: \"true\"\n\n" +
		"      - name: Dump logs on failure\n        if: failure()\n        run: kubectl get pods -A\n"
	ci := "jobs:\n  e2e:\n    steps:\n      - name: Deploy and test\n" + tcBlock("ci_k3d_comment_old") +
		"        run: |\n          forge env deploy e2e --yes\n          task test:e2e\n      - name: Upload test artifacts\n        if: failure()\n"
	dir := writeTree(t, map[string]string{
		"Taskfile.yml": taskfile, tcE2EWorkflow: e2e, tcCIWorkflow: ci,
		"forge.yaml": "name: demo\nforge_version: v1.9.0\n",
	})
	c, _ := Lookup("tests-capture")
	if _, err := c.Run(dir, Options{Write: true}); err != nil {
		t.Fatal(err)
	}
	wantE2E := "jobs:\n  k3d:\n    steps:\n      - name: Deploy\n        run: forge env deploy e2e --yes\n\n" +
		"      - name: Run E2E tests\n" + tcBlock("e2e_k3d_comment") + "        run: task test:e2e ENV=e2e\n        env:\n          E2E: \"true\"\n\n" +
		tcBlock("e2e_report") + "\n" +
		"      - name: Dump logs on failure\n        if: failure()\n        run: kubectl get pods -A\n"
	if got := readFile(t, dir, tcE2EWorkflow); got != wantE2E {
		t.Errorf("e2e.yml:\n%s", tcUnifiedDiff(tcE2EWorkflow, wantE2E, got))
	}
	wantCI := "jobs:\n  e2e:\n    steps:\n      - name: Deploy and test\n" + tcBlock("ci_k3d_comment_new") +
		"        run: |\n          forge env deploy e2e --yes\n          task test:e2e ENV=e2e\n" + tcBlock("ci_report") +
		"      - name: Upload test artifacts\n        if: failure()\n"
	if got := readFile(t, dir, tcCIWorkflow); got != wantCI {
		t.Errorf("ci.yml:\n%s", tcUnifiedDiff(tcCIWorkflow, wantCI, got))
	}
}
