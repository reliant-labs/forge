// Tests for `forge project rescaffold` (rescaffold.go).
//
// The defect these pin: every scaffolded workflow said "`forge generate`
// re-creates it only if you delete it first", and that was false. The
// scaffold-once birth ledger (.forge/scaffolded.json) records every file forge
// has written once, so a deletion reads as ownership and generate leaves the
// file gone — while printing "exists — leaving it untouched" about a file that
// is not there. Worse, some scaffold files (e2e.yml, pre-commit.yml,
// .pre-commit-config.yaml) are written only by `forge project new`, so no
// command re-emitted them at all: houndersclub copied them out of a throwaway
// scaffold.
//
// The generate pipeline is stood in for by its CI step, which is the emitter
// that writes .github/workflows — the same function, over the same mapper
// (generator.CIWorkflows). Everything else a rescaffold writes comes from
// renderers that run in-process, so the whole file is pure filesystem work.
//
// Not t.Parallel: the birth ledger's in-memory cache is process-global (see
// newLedgerProject).
package cli

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
)

// scaffoldForRescaffold runs the `forge project new` scaffold lane into a temp
// dir and returns it with its config loaded the way every command loads it.
func scaffoldForRescaffold(t *testing.T, shape func(g *generator.ProjectGenerator)) (string, *config.ProjectConfig) {
	t.Helper()
	dir := t.TempDir()
	checksums.ResetScaffoldLedgerCache()
	t.Cleanup(checksums.ResetScaffoldLedgerCache)
	g := generator.NewProjectGenerator("demo", dir, "github.com/example/demo")
	if shape != nil {
		shape(g)
	}
	captureStdout(t, func() {
		if err := g.Generate(); err != nil {
			t.Fatalf("scaffold: %v", err)
		}
	})
	return dir, loadAdvisoryConfig(t, dir)
}

// ciStepOnly stands in for the generate pipeline (see the file comment).
func ciStepOnly(root string, cfg *config.ProjectConfig) func() error {
	return func() error { return generateCIWorkflows(root, cfg, nil, false) }
}

func readRescaffoldFile(t *testing.T, root, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return b
}

func removeRescaffoldFile(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.Remove(filepath.Join(root, rel)); err != nil {
		t.Fatalf("delete %s: %v", rel, err)
	}
}

func rescaffoldFileExists(root, rel string) bool {
	_, err := os.Stat(filepath.Join(root, rel))
	return err == nil
}

// A deleted workflow stays deleted under `forge generate` — that is the
// ledger doing its job — but generate must SAY so truthfully and name the one
// command that brings it back, and that command must reproduce the scaffold's
// bytes exactly.
func TestRescaffold_DeletedWorkflowComesBackByteIdentical(t *testing.T) {
	root, cfg := scaffoldForRescaffold(t, nil)
	const rel = ".github/workflows/ci.yml"
	born := readRescaffoldFile(t, root, rel)
	removeRescaffoldFile(t, root, rel)

	// Under -v, because the per-path line is routine: it is identical on
	// every run. The EVENT — the moment the file went missing — is
	// reported at default verbosity by reportMissingScaffolds, which names
	// the same command. What must never happen, at any verbosity, is the
	// line claiming the file EXISTS; that is the bug this test was written
	// for and it is asserted below.
	defer setGenerateVerbosity(true)()
	out := captureStdout(t, func() {
		if err := generateCIWorkflows(root, cfg, nil, false); err != nil {
			t.Fatalf("generateCIWorkflows: %v", err)
		}
	})
	if rescaffoldFileExists(root, rel) {
		t.Fatal("generate re-created a scaffold the user deleted — the birth ledger must keep deletions sticky")
	}
	if strings.Contains(out, rel+" exists") {
		t.Errorf("generate reports %s as existing, but it was deleted:\n%s", rel, out)
	}
	if !strings.Contains(out, "project rescaffold "+rel) {
		t.Errorf("generate does not name the command that re-creates %s:\n%s", rel, out)
	}

	var w bytes.Buffer
	if err := rescaffoldPaths(&w, root, cfg, []string{rel}, ciStepOnly(root, cfg)); err != nil {
		t.Fatalf("rescaffold: %v\n%s", err, w.String())
	}
	if !rescaffoldFileExists(root, rel) {
		t.Fatalf("rescaffold did not re-create %s:\n%s", rel, w.String())
	}
	if got := readRescaffoldFile(t, root, rel); !bytes.Equal(got, born) {
		t.Errorf("rescaffolded %s differs from what the scaffold wrote\n--- born ---\n%s\n--- rescaffolded ---\n%s", rel, born, got)
	}
	if !checksums.ScaffoldRecorded(root, rel) {
		t.Errorf("%s is back on disk but not in the birth ledger — a later deletion would be undone by generate", rel)
	}
}

// e2e.yml, pre-commit.yml and .pre-commit-config.yaml are the files houndersclub
// had to copy from a throwaway scaffold: nothing but `forge project new` ever
// wrote them. The devcontainer and bootstrap script are the same class.
func TestRescaffold_ReemitsFilesOnlyProjectNewWrites(t *testing.T) {
	// A service scaffold carries the generated e2e/ suite, which is what
	// makes e2e.yml part of the project (see generator.CIWorkflows).
	root, cfg := scaffoldForRescaffold(t, func(g *generator.ProjectGenerator) { g.ServiceName = "item" })
	paths := []string{
		".github/workflows/e2e.yml",
		".github/workflows/pre-commit.yml",
		".pre-commit-config.yaml",
		".devcontainer/devcontainer.json",
		"scripts/bootstrap.sh",
	}
	born := map[string][]byte{}
	for _, rel := range paths {
		born[rel] = readRescaffoldFile(t, root, rel)
		removeRescaffoldFile(t, root, rel)
	}

	var w bytes.Buffer
	if err := rescaffoldPaths(&w, root, cfg, paths, ciStepOnly(root, cfg)); err != nil {
		t.Fatalf("rescaffold: %v\n%s", err, w.String())
	}
	for _, rel := range paths {
		if !rescaffoldFileExists(root, rel) {
			t.Errorf("%s was not re-created:\n%s", rel, w.String())
			continue
		}
		if got := readRescaffoldFile(t, root, rel); !bytes.Equal(got, born[rel]) {
			t.Errorf("rescaffolded %s differs from what the scaffold wrote\n--- born ---\n%s\n--- rescaffolded ---\n%s", rel, born[rel], got)
		}
	}
	if info, err := os.Stat(filepath.Join(root, "scripts/bootstrap.sh")); err == nil && info.Mode().Perm()&0o100 == 0 {
		t.Errorf("scripts/bootstrap.sh came back without its executable bit (mode %v)", info.Mode().Perm())
	}
}

// generator.CIWorkflows decides which workflows a project HAS. A project with
// no e2e suite has no e2e.yml, and rescaffold must not smuggle one in from a
// scaffold-time render — it must say why there is nothing to re-create.
func TestRescaffold_WorkflowTheProjectDoesNotHave(t *testing.T) {
	root, cfg := scaffoldForRescaffold(t, nil)
	const rel = ".github/workflows/e2e.yml"
	if rescaffoldFileExists(root, rel) {
		t.Fatalf("precondition: the default scaffold has no e2e suite, so no %s", rel)
	}

	var w bytes.Buffer
	err := rescaffoldPaths(&w, root, cfg, []string{rel}, ciStepOnly(root, cfg))
	if err == nil {
		t.Fatalf("rescaffold of a workflow this project does not have succeeded:\n%s", w.String())
	}
	if rescaffoldFileExists(root, rel) {
		t.Errorf("rescaffold wrote %s for a project with no e2e suite", rel)
	}
	if !strings.Contains(err.Error(), "e2e") {
		t.Errorf("the refusal must say what the workflow needs, got: %v", err)
	}
}

// Rescaffold restores ABSENT files only. Overwriting a present one would
// discard the user's edits to a file forge promised never to touch.
func TestRescaffold_RefusesAPresentFile(t *testing.T) {
	root, cfg := scaffoldForRescaffold(t, nil)
	const rel = ".pre-commit-config.yaml"
	edited := []byte("# mine\n")
	if err := os.WriteFile(filepath.Join(root, rel), edited, 0o644); err != nil {
		t.Fatal(err)
	}
	var w bytes.Buffer
	err := rescaffoldPaths(&w, root, cfg, []string{rel}, ciStepOnly(root, cfg))
	if err == nil {
		t.Fatalf("rescaffold over a present file succeeded:\n%s", w.String())
	}
	if got := readRescaffoldFile(t, root, rel); !bytes.Equal(got, edited) {
		t.Errorf("rescaffold rewrote a present file the user owns:\n%s", got)
	}
	// The pre-commit config is not upgrade-managed, so pointing at
	// `upgrade --force` would send the user into upgrade's refusal.
	if strings.Contains(err.Error(), "upgrade --force") {
		t.Errorf("remedy names `upgrade --force` for a path upgrade does not manage: %v", err)
	}

	// Taskfile.yml IS upgrade-managed: there the diff-first adopt is the
	// better remedy and must be the one named.
	err = rescaffoldPaths(&w, root, cfg, []string{"Taskfile.yml"}, ciStepOnly(root, cfg))
	if err == nil || !strings.Contains(err.Error(), "upgrade --force Taskfile.yml") {
		t.Errorf("remedy for an upgrade-managed present file should name `upgrade --force`, got: %v", err)
	}
}

// A deleted service proto took the service with it; the refusal names the
// verb that adds a service rather than a generic "not scaffolded".
func TestRescaffold_ServiceProtoNamesScaffoldService(t *testing.T) {
	root, cfg := scaffoldForRescaffold(t, func(g *generator.ProjectGenerator) { g.ServiceName = "item" })
	const rel = "proto/services/item/v1/item.proto"
	removeRescaffoldFile(t, root, rel)
	var w bytes.Buffer
	err := rescaffoldPaths(&w, root, cfg, []string{rel}, ciStepOnly(root, cfg))
	if err == nil || !strings.Contains(err.Error(), "scaffold service item") {
		t.Fatalf("want a refusal naming `scaffold service item`, got: %v", err)
	}
}

// "Can forge re-emit this file?" must be YES for every file the scaffold
// writes — not for a hand-kept list of the ones that were measured missing.
// Each file is deleted alone and rescaffolded, and must come back
// byte-identical.
func TestRescaffold_EveryScaffoldedFileIsReemittable(t *testing.T) {
	if testing.Short() {
		t.Skip("one scaffold render per file; the targeted rescaffold tests above run in -short")
	}
	// Not reproducible by construction, and not a scaffold's content:
	//   .forge/            forge's own state, including the ledger itself
	//   .reliant/project.json  stamps created_at at birth
	//   proto/services/<svc>/v1/<svc>.proto  IS the service — deleting it
	//                      deletes the service; `forge scaffold service`
	//                      re-creates one (TestRescaffold_ServiceProtoNamesScaffoldService)
	skip := func(rel string) bool {
		if _, ok := serviceProtoOf(rel); ok {
			return true
		}
		return strings.HasPrefix(rel, ".forge/") || rel == ".reliant/project.json"
	}
	for name, shape := range map[string]func(*generator.ProjectGenerator){
		"default service": nil,
		"service with frontend": func(g *generator.ProjectGenerator) {
			g.ServiceName = "item"
			g.FrontendName = "web"
		},
		"cli":     func(g *generator.ProjectGenerator) { g.Kind = config.ProjectKindCLI },
		"library": func(g *generator.ProjectGenerator) { g.Kind = config.ProjectKindLibrary },
	} {
		t.Run(name, func(t *testing.T) {
			root, cfg := scaffoldForRescaffold(t, shape)
			var files []string
			_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
				if err != nil || !d.Type().IsRegular() {
					return err
				}
				rel, _ := filepath.Rel(root, p)
				if rel = filepath.ToSlash(rel); !skip(rel) {
					files = append(files, rel)
				}
				return nil
			})
			sort.Strings(files)
			for _, rel := range files {
				full := filepath.Join(root, rel)
				born := readRescaffoldFile(t, root, rel)
				info, _ := os.Stat(full)
				removeRescaffoldFile(t, root, rel)
				var w bytes.Buffer
				err := rescaffoldPaths(&w, root, cfg, []string{rel}, ciStepOnly(root, cfg))
				got, _ := os.ReadFile(full)
				switch {
				case err != nil:
					t.Errorf("%s: forge cannot re-emit it: %v", rel, err)
				case !bytes.Equal(got, born):
					t.Errorf("%s: rescaffolded bytes differ from the scaffold's", rel)
				}
				// Put the birth bytes back whatever happened, so one gap
				// cannot cascade into the files checked after it.
				_ = os.WriteFile(full, born, info.Mode().Perm())
			}
		})
	}
}
