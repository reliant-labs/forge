package migrationlint

import (
	"strings"
	"testing"
)

func TestBlockingPrimaryKeyBuild(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
		line      int // zero means no finding
	}{
		{"separate ALTER statements", "ALTER TABLE jobs DROP CONSTRAINT jobs_pkey;\nALTER TABLE jobs ADD PRIMARY KEY (job_id, generation);", 2},
		{"combined ALTER with a named constraint", `-- change the job key
ALTER TABLE IF EXISTS ONLY public.jobs
    DROP CONSTRAINT jobs_pkey,
    /* build */ ADD CONSTRAINT "New Key" PRIMARY KEY (job_id, generation);`, 4},
		{"add without dropping an old key", "ALTER TABLE jobs ADD PRIMARY KEY (id);", 1},
		{"attach a prebuilt index", "ALTER TABLE jobs DROP CONSTRAINT jobs_pkey, ADD PRIMARY KEY USING INDEX idx_jobs;", 0},
		{"new empty table", "CREATE TABLE jobs (id int);\nALTER TABLE jobs ADD PRIMARY KEY (id);", 0},
		{"table potentially pre-exists", "CREATE TABLE IF NOT EXISTS jobs (id int);\nALTER TABLE jobs ADD PRIMARY KEY (id);", 2},
		{"table filled in this migration", "CREATE TABLE jobs (id int);\nINSERT INTO jobs VALUES (1);\nALTER TABLE jobs ADD PRIMARY KEY (id);", 3},
		{"words inside a string", "ALTER TABLE jobs ADD COLUMN note text DEFAULT 'a, ADD PRIMARY KEY (id)';", 0},
		{"words inside a comment", "ALTER TABLE jobs /* ADD PRIMARY KEY (id) */ ADD COLUMN note text;", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const file = "20261126000001_change_key.up.sql"
			var got []Finding
			for _, f := range lintStatements(file, tc.sql, DefaultConfig()) {
				if f.Rule == "blocking-primary-key-build" {
					got = append(got, f)
				}
			}
			if tc.line == 0 {
				if len(got) != 0 {
					t.Fatalf("unexpected finding: %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].File != file || got[0].Line != tc.line || got[0].Severity != SeverityWarn {
				t.Fatalf("want a warning at %s:%d, got %+v", file, tc.line, got)
			}
			if !strings.Contains(got[0].Message, "ACCESS EXCLUSIVE") {
				t.Errorf("message does not name the lock: %s", got[0].Message)
			}
			hint := RemediationFor(got[0].Rule)
			if !strings.Contains(hint, "CREATE UNIQUE INDEX CONCURRENTLY") || !strings.Contains(hint, "ADD PRIMARY KEY USING INDEX") || !strings.Contains(hint, "NOT NULL") {
				t.Errorf("recipe is incomplete: %s", hint)
			}
		})
	}
}
