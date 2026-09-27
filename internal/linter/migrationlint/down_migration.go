package migrationlint

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// RuleNoDownMigration is the rule ID for a down migration: a `*.down.sql`
// file, or a goose `-- +goose Down` section with SQL in it.
//
// Forge's policy is roll forward, never roll back. A down migration claims
// to undo a release and cannot: by the time anyone would run it, the release
// has written rows in the new shape, other services have read them, and jobs
// have acted on them. The script was written before any of that happened, it
// is untested in the state it would run in, and the moment it is needed is
// mid-incident. So forge writes none, runs none, and flags any that appear.
const RuleNoDownMigration = "no-down-migration"

// NoDownMigrationRemediation is the fix text for a no-down-migration finding.
const NoDownMigrationRemediation = `delete the down migration — forge never runs one. Recover from a bad migration with a new forward migration that repairs the schema from the state it is actually in, and write migrations expand-then-contract (add, backfill, switch readers, drop in a later release) so the previous release keeps working against the new schema. Down files that predate this rule can be grandfathered with database.migration_safety.down_files_allowed_until: <version> in forge.yaml`

// gooseDownRe matches the goose Down marker line.
var gooseDownRe = regexp.MustCompile(`^\s*--\s*\+goose\s+Down\b`)

// gooseMarkerRe matches any goose directive line (StatementBegin/End, Up,
// Down, NO TRANSACTION) — directives are not SQL, so a Down section holding
// only directives is empty.
var gooseMarkerRe = regexp.MustCompile(`^\s*--\s*\+goose\b`)

// leadingVersionRe pulls the numeric version prefix off a migration filename
// or a configured threshold ("00092_drop_x.down.sql", "00092", "92").
var leadingVersionRe = regexp.MustCompile(`^(\d+)`)

// lintDownMigrations reports every down migration among files.
//
// files are the non-`.up.sql` SQL files of a migrations directory. A
// `*.down.sql` is a down migration by its name, whatever it holds — an empty
// one still tells golang-migrate there is a way back. A goose one-file
// migration is one only if its Down section carries SQL: an absent or empty
// Down is exactly what the policy asks for.
//
// Down migrations at or below allowedUntil are grandfathered: they fold into
// ONE warning, so a project that predates the rule sees a single line
// pointing at the cleanup instead of ninety, and an error never fires for
// history nobody can rewrite. Anything newer is an error per file.
func lintDownMigrations(files []string, allowedUntil string) ([]Finding, error) {
	threshold, hasThreshold, err := parseDownThreshold(allowedUntil)
	if err != nil {
		return nil, err
	}

	var findings []Finding
	var grandfathered []string
	for _, file := range files {
		line, isDown, err := downMigrationLine(file)
		if err != nil {
			return nil, err
		}
		if !isDown {
			continue
		}
		if hasThreshold {
			if version, ok := migrationVersion(filepath.Base(file)); ok && version <= threshold {
				grandfathered = append(grandfathered, file)
				continue
			}
		}
		findings = append(findings, Finding{
			File:     file,
			Line:     line,
			Rule:     RuleNoDownMigration,
			Severity: SeverityError,
			Message:  "down migration: forge rolls forward only and never runs down SQL — delete it and recover from a bad migration with a new forward migration",
		})
	}

	if len(grandfathered) > 0 {
		findings = append(findings, Finding{
			File:     grandfathered[len(grandfathered)-1],
			Line:     1,
			Rule:     RuleNoDownMigration,
			Severity: SeverityWarn,
			Message: fmt.Sprintf("%d grandfathered down migration(s) at or below version %s (down_files_allowed_until); forge never runs them — delete them when convenient",
				len(grandfathered), allowedUntil),
		})
	}
	return findings, nil
}

// downMigrationLine reports whether file is a down migration and the line to
// point at: line 1 of a `*.down.sql`, or the goose Down marker's line.
func downMigrationLine(file string) (int, bool, error) {
	if strings.HasSuffix(file, ".down.sql") {
		return 1, true, nil
	}
	f, err := os.Open(file)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	lineNo, downLine := 0, 0
	for scanner.Scan() {
		lineNo++
		text := scanner.Text()
		if downLine == 0 {
			if gooseDownRe.MatchString(text) {
				downLine = lineNo
			}
			continue
		}
		if gooseMarkerRe.MatchString(text) {
			continue
		}
		if strings.TrimSpace(stripSQLComments(text)) != "" {
			return downLine, true, nil
		}
	}
	return 0, false, scanner.Err()
}

// parseDownThreshold reads down_files_allowed_until. It accepts the bare
// version ("00092", "92") or a migration filename stem ("00092_drop_x"), since
// both are what an author naturally copies out of the directory listing.
func parseDownThreshold(value string) (uint64, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false, nil
	}
	version, ok := migrationVersion(value)
	if !ok {
		return 0, false, fmt.Errorf("database.migration_safety.down_files_allowed_until: %q is not a migration version (want the numeric prefix of a migration file, e.g. \"00092\")", value)
	}
	return version, true, nil
}

// DownFilesBaseline returns the version prefix of the newest down migration
// in dir (the value to record as down_files_allowed_until so the history a
// project already has is grandfathered), or "" when dir holds none. The
// prefix is returned exactly as written ("00090"), so the recorded line reads
// like the filenames beside it.
func DownFilesBaseline(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var best uint64
	var bestPrefix string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") || strings.HasSuffix(name, ".up.sql") {
			continue
		}
		_, isDown, err := downMigrationLine(filepath.Join(dir, name))
		if err != nil {
			return "", err
		}
		if !isDown {
			continue
		}
		version, ok := migrationVersion(name)
		if !ok {
			continue
		}
		if bestPrefix == "" || version > best {
			best, bestPrefix = version, leadingVersionRe.FindString(name)
		}
	}
	return bestPrefix, nil
}

// migrationVersion parses the leading numeric version of a migration name.
func migrationVersion(name string) (uint64, bool) {
	m := leadingVersionRe.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
