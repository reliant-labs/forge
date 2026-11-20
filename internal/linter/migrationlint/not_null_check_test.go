package migrationlint

import (
	"testing"
)

// lintSequence lints files the way LintMigrationsDir does: per file, then the
// cross-file CHECK pass. It returns the set-not-null-without-backfill
// findings left, as "file:line".
func lintSequence(t *testing.T, files ...compatFile) []string {
	t.Helper()
	var findings []Finding
	for _, f := range files {
		findings = append(findings, lintMigrationFile(f.path, f.content, DefaultConfig())...)
	}
	var left []string
	for _, f := range dropCheckProvenSetNotNull(files, findings) {
		if f.Rule == RuleSetNotNullWithoutBackfill {
			left = append(left, f.File)
		}
	}
	return left
}

// TestSetNotNull_ProvenByValidatedCheck pins TL-07: the three-migration path
// `forge db analyze` recommends (CHECK NOT VALID, VALIDATE, SET NOT NULL)
// passes, and only a CHECK that is valid, present, on the same table and
// column proves the SET NOT NULL.
func TestSetNotNull_ProvenByValidatedCheck(t *testing.T) {
	add := compatFile{"1_add.up.sql", `ALTER TABLE users ADD CONSTRAINT users_email_nn CHECK (email IS NOT NULL) NOT VALID;`}
	validate := compatFile{"2_validate.up.sql", `ALTER TABLE users VALIDATE CONSTRAINT users_email_nn;`}
	set := compatFile{"3_set.up.sql", "-- the CHECK proves it\nALTER TABLE users ALTER COLUMN email SET NOT NULL;"}

	cases := []struct {
		name  string
		files []compatFile
		want  int // set-not-null findings left
	}{
		{"the advised three files", []compatFile{add, validate, set}, 0},
		{"a CHECK added valid", []compatFile{{"1.up.sql", `ALTER TABLE users ADD CONSTRAINT nn CHECK ((email IS NOT NULL));`}, set}, 0},
		{"same statement, valid CHECK first", []compatFile{{"1.up.sql", `ALTER TABLE users ADD CONSTRAINT nn CHECK (email IS NOT NULL), ALTER COLUMN email SET NOT NULL;`}}, 0},
		{"never validated", []compatFile{add, set}, 1},
		{"validated then dropped", []compatFile{add, validate, {"3_drop.up.sql", `ALTER TABLE users DROP CONSTRAINT IF EXISTS users_email_nn;`}, {"4_set.up.sql", set.content}}, 1},
		{"another table's CHECK", []compatFile{add, validate, {"3_set.up.sql", `ALTER TABLE accounts ALTER COLUMN email SET NOT NULL;`}}, 1},
		{"another column", []compatFile{add, validate, {"3_set.up.sql", `ALTER TABLE users ALTER COLUMN name SET NOT NULL;`}}, 1},
		{"SET NOT NULL before the validation", []compatFile{add, {"2_set.up.sql", set.content}, {"3_validate.up.sql", validate.content}}, 1},
		{"a CHECK that is not IS NOT NULL", []compatFile{{"1.up.sql", `ALTER TABLE users ADD CONSTRAINT len CHECK (length(email) > 0);`}, set}, 1},
		{"quoted identifiers", []compatFile{{"1.up.sql", `ALTER TABLE "users" ADD CONSTRAINT "nn" CHECK ("email" IS NOT NULL);`}, set}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lintSequence(t, tc.files...); len(got) != tc.want {
				t.Errorf("set-not-null-without-backfill findings = %v, want %d", got, tc.want)
			}
		})
	}
}
