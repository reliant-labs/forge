package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/buildtarget"
	"github.com/reliant-labs/forge/pkg/release"
	"github.com/reliant-labs/forge/pkg/testkit"
)

// covDBackfillProject is a git project named "shop" whose first commit carries
// one migration; it returns the dir and that commit.
func covDBackfillProject(t *testing.T) (dir, commit string) {
	t.Helper()
	testkit.Slow(t, "execs git over a temp repository")
	dir = t.TempDir()
	migGit(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte("name: shop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeMigFile(t, dir, "00001_init.up.sql")
	migGit(t, dir, "add", "-A")
	migGit(t, dir, "commit", "-q", "-m", "one")
	commit = migGit(t, dir, "rev-parse", "HEAD")
	prev := ledgerHome
	ledgerHome = func() (string, error) { return filepath.Join(dir, ".ledger-home"), nil }
	t.Cleanup(func() { ledgerHome = prev })
	return dir, commit
}

func covDCutRelease(t *testing.T, dir, version, commit string, m *release.MigrationManifest) {
	t.Helper()
	ledger, err := machineLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	rel := relWithMigrations(version, m)
	if commit != "" {
		rel.SetProvenance(release.Provenance{Repo: "github.com/acme/shop", Commit: commit})
	}
	if _, err := ledger.Releases.Cut(context.Background(), rel); err != nil {
		t.Fatalf("cut %s: %v", version, err)
	}
}

func TestCovDRunBackfillMigrations_RecordsAndSkips(t *testing.T) {
	dir, commit := covDBackfillProject(t)
	covDCutRelease(t, dir, "v1", commit, nil)
	covDCutRelease(t, dir, "v2", commit, migrationManifestOf("00009_already.up.sql"))
	covDCutRelease(t, dir, "v3", "", nil)

	var out bytes.Buffer
	err := runBackfillMigrations(context.Background(), &out, dir, "dev", "", false)
	var exit exitCodeError
	if !errors.As(err, &exit) || exit.code != exitWrong || !strings.Contains(exit.msg, "1 release(s) were not recorded: v3") {
		t.Fatalf("err = %#v, want exit 1 naming v3", err)
	}
	o := out.String()
	for _, want := range []string{
		"[backfill] v1:",
		"github.com/acme/shop/db/migrations @ " + shortSHA(commit) + ": 1 file(s), head 00001_init.up.sql",
		"[backfill] v3: NOT RECORDED: it records no source commit",
		"[backfill] recorded 1 manifest(s) in ",
		"deploy/kcl/dev did not render",
	} {
		if !strings.Contains(o, want) {
			t.Errorf("output lacks %q:\n%s", want, o)
		}
	}
	if strings.Contains(o, "[backfill] v2") {
		t.Errorf("a release that already records a manifest is skipped silently:\n%s", o)
	}

	ledger, err := machineLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	v1, err := ledger.Releases.Get(context.Background(), "v1")
	if err != nil || v1 == nil || v1.Migrations == nil || len(v1.Migrations.Sets) != 1 || strings.Join(v1.Migrations.Sets[0].Files, ",") != "00001_init.up.sql" {
		t.Fatalf("v1 after backfill = %+v, %v", v1, err)
	}
	v2, _ := ledger.Releases.Get(context.Background(), "v2")
	if v2.Migrations == nil || v2.Migrations.Sets[0].Files[0] != "00009_already.up.sql" {
		t.Errorf("an existing manifest must never be rewritten: %+v", v2.Migrations)
	}
}

func TestCovDRunBackfillMigrations_DryRunRecordsNothing(t *testing.T) {
	dir, commit := covDBackfillProject(t)
	covDCutRelease(t, dir, "v1", commit, nil)
	var out bytes.Buffer
	if err := runBackfillMigrations(context.Background(), &out, dir, "dev", "", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "would record (dry run) 1 manifest(s)") {
		t.Errorf("output:\n%s", out.String())
	}
	ledger, _ := machineLedger(dir)
	if v1, _ := ledger.Releases.Get(context.Background(), "v1"); v1.Migrations != nil {
		t.Errorf("a dry run recorded %+v", v1.Migrations)
	}
}

func TestCovDRunBackfillMigrations_SinceBoundsTheWalk(t *testing.T) {
	dir, commit := covDBackfillProject(t)
	covDCutRelease(t, dir, "v1", commit, nil)

	err := runBackfillMigrations(context.Background(), covDDiscard{}, dir, "dev", "v-nope", false)
	var exit exitCodeError
	if !errors.As(err, &exit) || exit.code != exitWrong || !strings.Contains(exit.msg, "--since v-nope: no such release") {
		t.Fatalf("err = %#v", err)
	}
	var out bytes.Buffer
	if err := runBackfillMigrations(context.Background(), &out, dir, "dev", "v1", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "[backfill] v1:") {
		t.Errorf("--since naming the only release still backfills it:\n%s", out.String())
	}
}

type covDDiscard struct{}

func (covDDiscard) Write(p []byte) (int, error) { return len(p), nil }

func TestCovDReconstructReleaseMigrations_Refusals(t *testing.T) {
	dir, commit := covDBackfillProject(t)
	ctx := context.Background()
	cases := []struct {
		name  string
		rel   release.Release
		notes string
		want  string
	}{
		{"no commit", release.Release{Version: "v"}, "", "records no source commit"},
		{"commit missing from this checkout", func() release.Release {
			r := release.Release{Version: "v"}
			r.SetProvenance(release.Provenance{Repo: "r", Commit: strings.Repeat("c", 40)})
			return r
		}(), "", "is not in this checkout"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := reconstructReleaseMigrations(ctx, dir, c.rel, nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}

	t.Run("a dirty cut is noted and read from its commit", func(t *testing.T) {
		r := release.Release{Version: "v"}
		r.SetProvenance(release.Provenance{Repo: "github.com/acme/shop", Commit: commit, Dirty: true})
		m, notes, err := reconstructReleaseMigrations(ctx, dir, r, nil)
		if err != nil || m == nil {
			t.Fatalf("m=%v err=%v", m, err)
		}
		if len(notes) != 1 || !strings.Contains(notes[0], "DIRTY") {
			t.Errorf("notes = %v", notes)
		}
	})

	t.Run("a commit with no migrations directory", func(t *testing.T) {
		migGit(t, dir, "rm", "-rq", "db")
		migGit(t, dir, "commit", "-q", "-m", "drop migrations")
		bare := migGit(t, dir, "rev-parse", "HEAD")
		r := release.Release{Version: "v"}
		r.SetProvenance(release.Provenance{Repo: "github.com/acme/shop", Commit: bare})
		if _, _, err := reconstructReleaseMigrations(ctx, dir, r, nil); err == nil || !strings.Contains(err.Error(), "carries no migrations directory at its commit") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("a sibling whose pin cannot be read from go.mod", func(t *testing.T) {
		sib := t.TempDir()
		if err := os.WriteFile(filepath.Join(sib, "forge.yaml"), []byte("name: sib\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		writeMigFile(t, sib, "00001_sib.up.sql")
		r := release.Release{Version: "v"}
		r.SetProvenance(release.Provenance{Repo: "github.com/acme/shop", Commit: commit})
		_, _, err := reconstructReleaseMigrations(ctx, dir, r, []*buildtarget.Source{{Dir: sib, Repo: "github.com/acme/sib", Module: "example.com/sib", ModuleDir: "."}})
		if err == nil || !strings.Contains(err.Error(), "read go.mod at "+shortSHA(commit)) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestCovDIsForgeCheckoutWithMigrations(t *testing.T) {
	if isForgeCheckoutWithMigrations("") {
		t.Error("an empty dir is not a checkout")
	}
	plain := t.TempDir()
	writeMigFile(t, plain, "00001.up.sql")
	if isForgeCheckoutWithMigrations(plain) {
		t.Error("migrations without a forge.yaml is not a forge checkout")
	}
	if err := os.WriteFile(filepath.Join(plain, "forge.yaml"), []byte("name: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isForgeCheckoutWithMigrations(plain) {
		t.Error("forge.yaml plus a migrations dir is a forge checkout with migrations")
	}
	bare := t.TempDir()
	if err := os.WriteFile(filepath.Join(bare, "forge.yaml"), []byte("name: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if isForgeCheckoutWithMigrations(bare) {
		t.Error("a forge project with no migrations directory has none to pin")
	}
}

func TestCovDProjectGoModPath(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "services", "api")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := projectGoModPath(root, root); got != "go.mod" {
		t.Errorf("root project = %q", got)
	}
	if got := projectGoModPath(root, sub); got != "services/api/go.mod" {
		t.Errorf("nested project = %q, want the slash form git show takes", got)
	}
	if got := projectGoModPath(sub, root); got != "go.mod" {
		t.Errorf("a project outside the repo root falls back to go.mod, got %q", got)
	}
}

func TestCovDResolvedPath(t *testing.T) {
	dir := t.TempDir()
	if got := resolvedPath(filepath.Join(dir, "missing")); got != filepath.Join(dir, "missing") {
		t.Errorf("an unresolvable path is returned as given, got %q", got)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolvedPath(dir); got != real {
		t.Errorf("resolvedPath = %q, want %q", got, real)
	}
}

func TestCovDEnvReleasesBackfillCmd_WiresFlags(t *testing.T) {
	cmd := newEnvReleasesBackfillMigrationsCmd()
	if f := cmd.Flags().Lookup("since"); f == nil || f.DefValue != "" {
		t.Errorf("--since: %+v", f)
	}
	if f := cmd.Flags().Lookup("dry-run"); f == nil || f.DefValue != "false" {
		t.Errorf("--dry-run: %+v", f)
	}
	cmd.SetArgs(nil)
	cmd.SetOut(covDDiscard{})
	cmd.SetErr(covDDiscard{})
	if err := cmd.Execute(); err == nil {
		t.Error("an env argument is required")
	}
}
