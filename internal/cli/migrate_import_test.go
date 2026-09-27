package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type gooseFile struct {
	Name    string
	Content string
}

func writeGooseSrc(t *testing.T, files []gooseFile) string {
	t.Helper()
	src := t.TempDir()
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(src, f.Name), []byte(f.Content), 0o644); err != nil {
			t.Fatalf("write %s: %v", f.Name, err)
		}
	}
	return src
}

func readMigrationsDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func readMigrateImportFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestMigrateImportRoundtrip(t *testing.T) {
	src := writeGooseSrc(t, []gooseFile{
		{
			Name: "20240501_add_users.sql",
			Content: `-- +goose Up
CREATE TABLE users (id INT PRIMARY KEY);
-- +goose Down
DROP TABLE users;
`,
		},
		{
			Name: "20240502_add_orgs.sql",
			Content: `-- +goose Up
CREATE TABLE orgs (id INT PRIMARY KEY);
-- +goose Down
DROP TABLE orgs;
`,
		},
		{
			Name: "20240503_add_memberships.sql",
			Content: `-- +goose Up
CREATE TABLE memberships (
  user_id INT REFERENCES users(id),
  org_id INT REFERENCES orgs(id)
);
-- +goose Down
DROP TABLE memberships;
`,
		},
	})
	dest := t.TempDir()

	var buf bytes.Buffer
	err := runMigrateImport(migrateImportOptions{
		From:    "goose",
		SrcDir:  src,
		DestDir: dest,
		Stdout:  &buf,
	})
	if err != nil {
		t.Fatalf("runMigrateImport: %v", err)
	}

	got := readMigrationsDir(t, dest)
	want := []string{
		"00001_add_users.up.sql",
		"00002_add_orgs.up.sql",
		"00003_add_memberships.up.sql",
	}
	if len(got) != len(want) {
		t.Fatalf("file count: got %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("file[%d]: got %q, want %q", i, got[i], w)
		}
	}

	up := readMigrateImportFile(t, filepath.Join(dest, "00001_add_users.up.sql"))
	if !strings.Contains(up, "CREATE TABLE users") {
		t.Errorf("up file missing CREATE TABLE: %q", up)
	}
	if strings.Contains(up, "+goose") {
		t.Errorf("up file still has goose markers: %q", up)
	}
	// Forward only: the Down section is dropped, never written, and the drop
	// is reported rather than silent.
	if downs, _ := filepath.Glob(filepath.Join(dest, "*.down.sql")); len(downs) != 0 {
		t.Errorf("import must write no .down.sql (forge rolls forward only), got %v", downs)
	}
	if strings.Contains(up, "DROP TABLE users") {
		t.Errorf("up file must not carry the Down section's SQL: %q", up)
	}
	if !strings.Contains(buf.String(), "Dropped -- +goose Down sections") ||
		!strings.Contains(buf.String(), "20240501_add_users.sql") {
		t.Errorf("expected the dropped Down sections to be reported, got: %q", buf.String())
	}

	if !strings.Contains(buf.String(), "Foreign-key check") {
		t.Errorf("expected FK warning in stdout, got: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "00003_add_memberships.up.sql") {
		t.Errorf("expected memberships flagged in FK warning, got: %q", buf.String())
	}
}

func TestMigrateImportNoTransactionHeader(t *testing.T) {
	src := writeGooseSrc(t, []gooseFile{
		{
			Name: "20240501_create_index.sql",
			Content: `-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY idx_users_email ON users(email);
-- +goose Down
DROP INDEX CONCURRENTLY idx_users_email;
`,
		},
	})
	dest := t.TempDir()

	var buf bytes.Buffer
	if err := runMigrateImport(migrateImportOptions{
		From:    "goose",
		SrcDir:  src,
		DestDir: dest,
		Stdout:  &buf,
	}); err != nil {
		t.Fatalf("runMigrateImport: %v", err)
	}

	up := readMigrateImportFile(t, filepath.Join(dest, "00001_create_index.up.sql"))

	if !strings.Contains(up, "x-no-tx-wrap: true") {
		t.Errorf("up missing x-no-tx-wrap header: %q", up)
	}
	if strings.Contains(up, "+goose NO TRANSACTION") {
		t.Errorf("up still contains goose NO TRANSACTION marker: %q", up)
	}
}

func TestMigrateImportStripsStatementMarkers(t *testing.T) {
	src := writeGooseSrc(t, []gooseFile{
		{
			Name: "20240501_create_fn.sql",
			Content: `-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION foo() RETURNS void AS $$
BEGIN
  RAISE NOTICE 'hi';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin
DROP FUNCTION foo();
-- +goose StatementEnd
`,
		},
	})
	dest := t.TempDir()

	var buf bytes.Buffer
	if err := runMigrateImport(migrateImportOptions{
		From:    "goose",
		SrcDir:  src,
		DestDir: dest,
		Stdout:  &buf,
	}); err != nil {
		t.Fatalf("runMigrateImport: %v", err)
	}

	up := readMigrateImportFile(t, filepath.Join(dest, "00001_create_fn.up.sql"))

	if strings.Contains(up, "StatementBegin") || strings.Contains(up, "StatementEnd") {
		t.Errorf("up still contains Statement markers: %q", up)
	}
	if !strings.Contains(up, "CREATE FUNCTION foo") {
		t.Errorf("up missing CREATE FUNCTION: %q", up)
	}
	if strings.Contains(up, "DROP FUNCTION foo") {
		t.Errorf("the Down section leaked into up: %q", up)
	}
}

func TestMigrateImportRenumbersAfterPacks(t *testing.T) {
	dest := t.TempDir()
	for _, name := range []string{
		"00001_audit_log.up.sql",
		"00002_api_key.up.sql",
		"00003_session.up.sql",
	} {
		if err := os.WriteFile(filepath.Join(dest, name), []byte("-- pack\n"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	src := writeGooseSrc(t, []gooseFile{
		{
			Name: "20240501_add_users.sql",
			Content: `-- +goose Up
CREATE TABLE users (id INT);
-- +goose Down
DROP TABLE users;
`,
		},
		{
			Name: "20240502_add_orgs.sql",
			Content: `-- +goose Up
CREATE TABLE orgs (id INT);
-- +goose Down
DROP TABLE orgs;
`,
		},
	})

	var buf bytes.Buffer
	if err := runMigrateImport(migrateImportOptions{
		From:    "goose",
		SrcDir:  src,
		DestDir: dest,
		Stdout:  &buf,
	}); err != nil {
		t.Fatalf("runMigrateImport: %v", err)
	}

	for _, name := range []string{
		"00004_add_users.up.sql",
		"00005_add_orgs.up.sql",
	} {
		if _, err := os.Stat(filepath.Join(dest, name)); err != nil {
			t.Errorf("expected %s to exist: %v", name, err)
		}
	}

	if got := readMigrateImportFile(t, filepath.Join(dest, "00001_audit_log.up.sql")); got != "-- pack\n" {
		t.Errorf("pack file 00001 was clobbered: %q", got)
	}
}

func TestMigrateImportRefusesOverwriteWithoutForce(t *testing.T) {
	src := writeGooseSrc(t, []gooseFile{
		{
			Name: "20240501_add_users.sql",
			Content: `-- +goose Up
CREATE TABLE users (id INT);
-- +goose Down
DROP TABLE users;
`,
		},
	})
	dest := t.TempDir()

	var buf bytes.Buffer
	if err := runMigrateImport(migrateImportOptions{
		From:    "goose",
		SrcDir:  src,
		DestDir: dest,
		Stdout:  &buf,
	}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "00001_add_users.up.sql")); err != nil {
		t.Fatalf("first run did not write expected file: %v", err)
	}

	buf.Reset()
	err := runMigrateImport(migrateImportOptions{
		From:    "goose",
		SrcDir:  src,
		DestDir: dest,
		Stdout:  &buf,
	})
	if err == nil {
		t.Fatal("expected error on re-import without --force")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error should mention 'already exists', got: %v", err)
	}

	files := readMigrationsDir(t, dest)
	if len(files) != 1 {
		t.Errorf("refused run should not touch disk, got: %v", files)
	}
}

func TestMigrateImportForceOverwrites(t *testing.T) {
	dest := t.TempDir()

	src1 := writeGooseSrc(t, []gooseFile{
		{
			Name: "20240501_add_users.sql",
			Content: `-- +goose Up
CREATE TABLE users (id INT);
-- +goose Down
DROP TABLE users;
`,
		},
	})
	var buf bytes.Buffer
	if err := runMigrateImport(migrateImportOptions{
		From:    "goose",
		SrcDir:  src1,
		DestDir: dest,
		Stdout:  &buf,
	}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	original := readMigrateImportFile(t, filepath.Join(dest, "00001_add_users.up.sql"))
	if !strings.Contains(original, "id INT") {
		t.Fatalf("first run produced unexpected content: %q", original)
	}

	src2 := writeGooseSrc(t, []gooseFile{
		{
			Name: "20240501_add_users.sql",
			Content: `-- +goose Up
CREATE TABLE users (id BIGINT PRIMARY KEY, email TEXT);
-- +goose Down
DROP TABLE users;
`,
		},
	})
	buf.Reset()
	if err := runMigrateImport(migrateImportOptions{
		From:    "goose",
		SrcDir:  src2,
		DestDir: dest,
		Force:   true,
		Stdout:  &buf,
	}); err != nil {
		t.Fatalf("force re-run: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dest, "00001_add_users.up.sql")); !os.IsNotExist(err) {
		t.Errorf("expected old slot 1 to be removed, stat err: %v", err)
	}
	newPath := filepath.Join(dest, "00002_add_users.up.sql")
	updated := readMigrateImportFile(t, newPath)
	if !strings.Contains(updated, "BIGINT") {
		t.Errorf("--force should have re-imported new content at %s, got: %q", newPath, updated)
	}
}

func TestMigrateImportDryRunTouchesNothing(t *testing.T) {
	src := writeGooseSrc(t, []gooseFile{
		{
			Name: "20240501_add_users.sql",
			Content: `-- +goose Up
CREATE TABLE users (id INT);
-- +goose Down
DROP TABLE users;
`,
		},
	})
	dest := t.TempDir()

	var buf bytes.Buffer
	if err := runMigrateImport(migrateImportOptions{
		From:    "goose",
		SrcDir:  src,
		DestDir: dest,
		DryRun:  true,
		Stdout:  &buf,
	}); err != nil {
		t.Fatalf("dry-run: %v", err)
	}

	files := readMigrationsDir(t, dest)
	if len(files) != 0 {
		t.Errorf("dry-run should not write files, got: %v", files)
	}

	out := buf.String()
	if !strings.Contains(out, "Dry run") {
		t.Errorf("expected 'Dry run' in output: %q", out)
	}
	if !strings.Contains(out, "00001_add_users.up.sql") {
		t.Errorf("expected planned filename in dry-run output: %q", out)
	}
}

// A source with no Down section imports cleanly and reports no drop — it is
// already the shape the policy asks for.
func TestMigrateImportWithoutDownReportsNoDrop(t *testing.T) {
	src := writeGooseSrc(t, []gooseFile{
		{
			Name: "20240501_no_down.sql",
			Content: `-- +goose Up
CREATE TABLE users (id INT);
-- +goose Down
-- intentionally empty: roll forward
`,
		},
	})
	dest := t.TempDir()

	var buf bytes.Buffer
	if err := runMigrateImport(migrateImportOptions{
		From:    "goose",
		SrcDir:  src,
		DestDir: dest,
		Stdout:  &buf,
	}); err != nil {
		t.Fatalf("runMigrateImport: %v", err)
	}

	if got := readMigrationsDir(t, dest); len(got) != 1 || got[0] != "00001_no_down.up.sql" {
		t.Fatalf("want only the up migration, got %v", got)
	}
	if strings.Contains(buf.String(), "Dropped") {
		t.Errorf("an empty Down section is not a drop worth reporting: %q", buf.String())
	}
}

func TestMigrateImportSkipsAlreadyConverted(t *testing.T) {
	src := writeGooseSrc(t, []gooseFile{
		{
			Name: "20240501_add_users.sql",
			Content: `-- +goose Up
CREATE TABLE users (id INT);
-- +goose Down
DROP TABLE users;
`,
		},
		{
			Name: "00001_already_converted.up.sql",
			Content: `CREATE TABLE orgs (id INT);
`,
		},
	})
	dest := t.TempDir()

	var buf bytes.Buffer
	if err := runMigrateImport(migrateImportOptions{
		From:    "goose",
		SrcDir:  src,
		DestDir: dest,
		Stdout:  &buf,
	}); err != nil {
		t.Fatalf("runMigrateImport: %v", err)
	}

	files := readMigrationsDir(t, dest)
	if len(files) != 1 {
		t.Errorf("expected 1 output file (only add_users converted), got: %v", files)
	}

	out := buf.String()
	if !strings.Contains(out, "Skipped:") {
		t.Errorf("expected skip notice in output: %q", out)
	}
	if !strings.Contains(out, "no goose markers") {
		t.Errorf("expected 'no goose markers' reason: %q", out)
	}
}

func TestMigrateImportRejectsUnknownFrom(t *testing.T) {
	src := t.TempDir()
	dest := t.TempDir()

	var buf bytes.Buffer
	err := runMigrateImport(migrateImportOptions{
		From:    "dbmate",
		SrcDir:  src,
		DestDir: dest,
		Stdout:  &buf,
	})
	if err == nil {
		t.Fatal("expected error for unsupported --from")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("error should mention 'not supported', got: %v", err)
	}
}

func TestMigrateImportCommandWiring(t *testing.T) {
	cmd := newMigrateCmd()
	if cmd.Name() != "migrate" {
		t.Errorf("expected name 'migrate', got %q", cmd.Name())
	}
	imp := commandName(cmd, "import")
	if imp == nil {
		t.Fatal("expected 'import' subcommand under migrate")
	}
	for _, flag := range []string{"from", "src-dir", "dry-run", "force"} {
		if imp.Flag(flag) == nil {
			t.Errorf("expected --%s flag on migrate import", flag)
		}
	}
}
