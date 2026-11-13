// File: internal/cli/db_analyze.go
//
// `forge db analyze` — what a migration measurably does on a real postgres:
// which locks it takes, whether it rewrites a table, whether a constraint
// validation scans one, how long it runs. The mechanism (the rolled-back
// session, the measurement, the rules, the document) is forge/pkg/dbanalyze;
// this file decides WHICH migrations, on WHICH database, and grades the
// findings like `forge lint`'s. Design: docs/proposals/db-analyze.md.
//
// It needs postgres, so it is never in a fast PR tier: it runs in a review
// lane, a full gate, or on demand.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/lint"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/migrationgit"
	"github.com/reliant-labs/forge/pkg/dbanalyze"
	"github.com/reliant-labs/forge/pkg/migratekit/compat"
	"github.com/reliant-labs/forge/pkg/pgtest"
)

// defaultAnalyzeDSNEnv is the variable a dsn target's connection string is
// read from when neither --dsn-env nor database.analyze.target.dsn_env names
// another.
const defaultAnalyzeDSNEnv = "FORGE_ANALYZE_DATABASE_URL"

type dbAnalyzeOptions struct {
	target string
	dsnEnv string
	base   string
	migDir string
	json   bool
}

func newDBAnalyzeCommand() *cobra.Command {
	var opts dbAnalyzeOptions
	cmd := &cobra.Command{
		Use:   "analyze",
		Short: "Measure what pending migrations do on a real postgres: locks, rewrites, scans, time",
		Long: `Apply migrations statement by statement inside ONE transaction that is always
rolled back, and measure each statement on the target: the locks it took
(pg_locks), the tables and indexes it rewrote (pg_class.relfilenode), the
tables it scanned (pg_stat_xact_user_tables), and how long it ran.

Which migrations:
  scratch (default)  the migrations added or changed since --base (default: the
                     merge-base with the remote default branch), on an embedded
                     postgres migrated with the unchanged ones
  dsn                every migration the target's schema_migrations lacks, on a
                     database you provide — typically an anonymized prod snapshot

The target is never changed. A statement that cannot run inside a transaction
(CREATE INDEX CONCURRENTLY) is not executed; its lock is reported statically.
A dsn target that is a non-development environment's declared database is
refused: point it at a snapshot, not production. The DSN is read from an
environment variable (database.analyze.target.dsn_env, default
` + defaultAnalyzeDSNEnv + `), never from forge.yaml.

Rules (lint.rules sets their severity; each warns before it gates):
  ` + dbanalyze.RuleTableRewrite + `         a pre-existing table or its indexes rewritten
  ` + dbanalyze.RuleAccessExclusiveLock + ` ACCESS EXCLUSIVE held across a scan or rewrite
  ` + dbanalyze.RuleValidationScan + `        a full scan under a lock that blocks writes
  ` + dbanalyze.RuleBlockingIndexBuild + `   an index built without CONCURRENTLY
  ` + dbanalyze.RuleStatementTimeout + `     a statement exceeded statement_timeout

Exit codes: 0 nothing gating; 1 a gating finding; 2 could not analyze (no
postgres, target ahead of the tree, refused DSN, a statement that failed).
An empty analysis is reported as scanned 0, with the reason.

Load the db/analyze skill for configuration and how to read the document.

Examples:
  forge db analyze
  forge db analyze --json
  FORGE_ANALYZE_DATABASE_URL=postgres://... forge db analyze --target dsn`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectDir := dbProjectRoot()
			cfg, err := loadProjectConfig()
			if err != nil && !errors.Is(err, ErrProjectConfigNotFound) {
				return err
			}
			opts.migDir = resolveMigrationsDir(opts.migDir)
			doc := runDBAnalyze(cmd.Context(), projectDir, cfg, opts, defaultAnalyzeDeps())
			return reportDBAnalyze(cmd.OutOrStdout(), doc, opts.json)
		},
	}
	cmd.Flags().StringVar(&opts.target, "target", "", "Where to analyze: scratch (an embedded postgres) or dsn (default: database.analyze.target.kind, else scratch)")
	cmd.Flags().StringVar(&opts.dsnEnv, "dsn-env", "", "Environment variable holding a dsn target's connection string (default: database.analyze.target.dsn_env, else "+defaultAnalyzeDSNEnv+")")
	cmd.Flags().StringVar(&opts.base, "base", "", "Revision a scratch target's migrations are split at (default: the merge-base with the remote default branch)")
	cmd.Flags().StringVar(&opts.migDir, "dir", "", migrationsDirFlagUsage)
	cmd.Flags().BoolVar(&opts.json, "json", false, "Print the forge.db-analyze/v1 document")
	return cmd
}

// analyzeDeps are the seams a test replaces: the environment, and what a
// deploy env declares as its database (which renders KCL).
type analyzeDeps struct {
	getenv      func(string) string
	declaredDSN func(ctx context.Context, projectDir, env string) string
}

func defaultAnalyzeDeps() analyzeDeps {
	return analyzeDeps{getenv: os.Getenv, declaredDSN: declaredEnvDSN}
}

// runDBAnalyze builds the document. It never fails on its own: a failure is
// the document's Error, exit 2.
func runDBAnalyze(ctx context.Context, projectDir string, cfg *config.ProjectConfig, opts dbAnalyzeOptions, deps analyzeDeps) *dbanalyze.Document {
	doc := dbanalyze.NewDocument()
	fail := func(format string, args ...any) *dbanalyze.Document {
		doc.Error = fmt.Sprintf(format, args...)
		doc.Finalize()
		return doc
	}
	var acfg config.AnalyzeConfig
	if cfg != nil {
		acfg = cfg.Database.Analyze
	}
	kind := strings.TrimSpace(opts.target)
	if kind == "" {
		kind = acfg.Target.EffectiveKind()
	}
	switch kind {
	case config.AnalyzeTargetScratch, config.AnalyzeTargetDSN:
	default:
		return fail("--target %q is not scratch or dsn", kind)
	}
	doc.Target.Kind = dbanalyze.TargetKind(kind)
	stmtTimeout, lockTimeout, err := acfg.Target.Timeouts()
	if err != nil {
		return fail("%v", err)
	}

	if h, ok := projectMigrationHook(opts.migDir); ok {
		return fail("this project's migrations are applied by %s (database.migrations.tool): forge db analyze reads forge-migrator migrations (<version>_<name>.up.sql) only", h.Tool)
	}
	if info, err := os.Stat(opts.migDir); err != nil || !info.IsDir() {
		return fail("migrations directory %s does not exist", opts.migDir)
	}
	ms, err := projectMigrations(opts.migDir)
	if err != nil {
		return fail("read migrations: %v", err)
	}
	files := make([]dbanalyze.MigrationFile, 0, len(ms))
	for _, m := range ms {
		path := filepath.Join(opts.migDir, m.Name)
		data, err := os.ReadFile(path) // #nosec G304 -- the project's own migration
		if err != nil {
			return fail("read %s: %v", path, err)
		}
		files = append(files, dbanalyze.MigrationFile{File: analyzeRelPath(projectDir, path), Version: uint64(m.Version), SQL: string(data)})
	}

	var (
		analyzed []dbanalyze.MigrationFile
		dsn      string
	)
	switch kind {
	case config.AnalyzeTargetScratch:
		setup, changed, base, err := splitAtBase(projectDir, opts.migDir, opts.base, files)
		if err != nil {
			return fail("%v", err)
		}
		doc.Base, analyzed = base, changed
		if len(analyzed) == 0 {
			doc.Summary.Reason = fmt.Sprintf("no migration in %s was added or changed since %s", analyzeRelPath(projectDir, opts.migDir), base)
			doc.Finalize()
			return doc
		}
		if v := cfgPostgresVersion(cfg); v != "" && deps.getenv(pgtest.EnvVersion) == "" {
			// pgtest reads database.postgres_version from the forge.yaml
			// above the CWD; -C may name another project.
			_ = os.Setenv(pgtest.EnvVersion, v)
		}
		var cleanup func()
		dsn, cleanup, err = dbanalyze.PrepareScratch(ctx, setup)
		if err != nil {
			return fail("could not start a scratch postgres migrated to the base: %v", err)
		}
		defer cleanup()
	case config.AnalyzeTargetDSN:
		env := firstNonEmptyStr(opts.dsnEnv, acfg.Target.DSNEnv, defaultAnalyzeDSNEnv)
		dsn = strings.TrimSpace(deps.getenv(env))
		if dsn == "" {
			return fail("a dsn target reads its connection string from $%s, which is unset: export it (a snapshot of production, never production itself)", env)
		}
		if err := refuseDeployedAnalyzeDSN(ctx, projectDir, dsn, deps.declaredDSN); err != nil {
			return fail("%v", err)
		}
	}
	doc.Target.Identity = databaseIdentity(dsn)

	s, err := dbanalyze.Open(ctx, dsn, dbanalyze.SessionOptions{StatementTimeout: stmtTimeout, LockTimeout: lockTimeout})
	if err != nil {
		return fail("connect to the %s target %s: %v", kind, doc.Target.Identity, err)
	}
	defer func() { _ = s.Close() }()
	info, err := s.Info(ctx, doc.Target.Kind, doc.Target.Identity)
	if err != nil {
		return fail("%v", err)
	}
	doc.Target = info

	if kind == config.AnalyzeTargetDSN {
		st, err := s.MigrationState(ctx)
		if err != nil {
			return fail("%v", err)
		}
		analyzed, err = pendingOnTarget(ms, files, st)
		if err != nil {
			return fail("%v", err)
		}
		if len(analyzed) == 0 {
			doc.Summary.Reason = fmt.Sprintf("the target has applied every migration in %s (version %d)", analyzeRelPath(projectDir, opts.migDir), st.Version)
			doc.Finalize()
			return doc
		}
	}

	migs, findings, err := dbanalyze.AnalyzeMigrations(ctx, s, info, analyzed, dbanalyze.Config{
		TableRows: acfg.TableRows, SmallTableRows: acfg.SmallTableRows, LargeTableRows: acfg.LargeTableRows,
	})
	doc.Migrations = append(doc.Migrations, migs...)
	doc.Findings = gradeAnalyzeFindings(cfg, findings)
	if err != nil {
		return fail("could not analyze past %v", err)
	}
	doc.Finalize()
	return doc
}

// splitAtBase splits a scratch target's migrations at the base revision:
// the files unchanged since it are setup, the ones added or changed are
// analyzed. A changed file is analyzed after every unchanged one, in version
// order (an edited merged migration is itself a defect; see db/versioning).
func splitAtBase(projectDir, migDir, base string, files []dbanalyze.MigrationFile) (setup, changed []dbanalyze.MigrationFile, label string, err error) {
	root := migrationgit.RepoRoot(projectDir)
	var sha string
	if base != "" {
		s, ok := migrationgit.ResolveCommit(root, base)
		if !ok {
			return nil, nil, "", fmt.Errorf("--base %q is not a commit in this repository", base)
		}
		sha, label = s, base
	} else {
		s, ref, ok := migrationgit.MergeBase(root)
		if !ok {
			return nil, nil, "", fmt.Errorf("cannot find the merge-base with the remote default branch (no git repository, no default branch, or a shallow clone): pass --base <rev>")
		}
		sha, label = s, strings.TrimPrefix(ref, "refs/remotes/")
	}
	label += "@" + shortSHA(sha)
	atBase, ok := migrationgit.FilesAt(root, migDir, sha)
	if !ok {
		return nil, nil, "", fmt.Errorf("cannot read %s at %s", migDir, label)
	}
	diff, ok := migrationgit.ChangedSince(root, migDir, sha)
	if !ok {
		return nil, nil, "", fmt.Errorf("cannot diff %s against %s", migDir, label)
	}
	present := map[string]bool{}
	for _, n := range atBase {
		present[n] = true
	}
	for _, f := range files {
		name := filepath.Base(f.File)
		if present[name] && !diff[name] {
			setup = append(setup, f)
		} else {
			changed = append(changed, f)
		}
	}
	return setup, changed, label, nil
}

// pendingOnTarget is every migration a dsn target has not applied, by the
// version its schema_migrations records. A target ahead of the tree, dirty,
// or at a version no migration carries cannot be analyzed.
func pendingOnTarget(ms []compat.Migration, files []dbanalyze.MigrationFile, st dbanalyze.MigrationState) ([]dbanalyze.MigrationFile, error) {
	switch {
	case st.Dirty:
		return nil, fmt.Errorf("the target's schema is dirty at version %d (a migration failed part-way there): repair it before analyzing", st.Version)
	case !st.Applied:
		return files, nil
	case len(ms) == 0:
		return nil, fmt.Errorf("the target is at version %d and the tree holds no migrations: the target is ahead of the tree", st.Version)
	}
	latest := uint64(ms[len(ms)-1].Version)
	if st.Version > latest {
		return nil, fmt.Errorf("the target is at version %d, ahead of the tree (latest %d): analyze it from a checkout that has its migrations", st.Version, latest)
	}
	carried := false
	for _, m := range ms {
		carried = carried || uint64(m.Version) == st.Version
	}
	if !carried {
		if squashedBy(ms[0], uint(st.Version)) {
			return files[1:], nil
		}
		return nil, fmt.Errorf("the target is at version %d, which no migration in the tree carries", st.Version)
	}
	var out []dbanalyze.MigrationFile
	for _, f := range files {
		if f.Version > st.Version {
			out = append(out, f)
		}
	}
	return out, nil
}

// refuseDeployedAnalyzeDSN refuses a dsn target that is the database a
// non-development deploy env declares: the analysis is rolled back, but a
// rewrite or a scan still runs, under real locks, for as long as
// statement_timeout allows. That is the point on a snapshot and an outage on
// production.
//
// A DSN that does not name a server and a database (a key=value string, a
// bare socket) is refused too: forge cannot tell which database it is.
func refuseDeployedAnalyzeDSN(ctx context.Context, projectDir, dsn string, declared func(context.Context, string, string) string) error {
	id := databaseIdentity(dsn)
	if id == "" {
		return fmt.Errorf("refusing the dsn target %s: it does not name a server and a database (postgres://host:port/name), so forge cannot check that it is not a deployed environment's database", redactDSNForMessage(dsn))
	}
	envs, err := ListEnvs(projectDir)
	if err != nil {
		return err
	}
	for _, env := range envs {
		if seedEnvIsDevIn(projectDir, env) {
			continue
		}
		if d := declared(ctx, projectDir, env); d != "" && databaseIdentity(d) == id {
			return fmt.Errorf("refusing the dsn target %s: it is environment %q's database. forge db analyze runs every pending migration on its target (rolled back, but under real locks and for real time): point it at a snapshot, not at %s", id, env, env)
		}
	}
	return nil
}

// gradeAnalyzeFindings grades the findings as forge lint grades its own: the
// warn-first rollout for the project's pin, then lint.rules.
func gradeAnalyzeFindings(cfg *config.ProjectConfig, fs []dbanalyze.Finding) []dbanalyze.Finding {
	out := make([]dbanalyze.Finding, 0, len(fs))
	for _, f := range fs {
		sev, keep, note := lint.GradeFinding(cfg, f.Rule, f.Severity)
		if !keep {
			continue
		}
		f.Severity, f.Note = sev, note
		out = append(out, f)
	}
	return out
}

// reportDBAnalyze prints the document and returns the error carrying its
// exit code.
func reportDBAnalyze(w io.Writer, doc *dbanalyze.Document, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(doc); err != nil {
			return err
		}
	} else {
		writeDBAnalyzeText(w, doc)
	}
	switch doc.ExitCode {
	case dbanalyze.ExitOK:
		return nil
	case dbanalyze.ExitFindings:
		n := 0
		for _, f := range doc.Findings {
			if f.Severity == dbanalyze.SeverityError {
				n++
			}
		}
		return exitCodeError{code: dbanalyze.ExitFindings, msg: fmt.Sprintf("db analyze: %d finding(s) gate", n)}
	default:
		return exitCodeError{code: doc.ExitCode, msg: "db analyze could not analyze: " + doc.Error}
	}
}

func writeDBAnalyzeText(w io.Writer, doc *dbanalyze.Document) {
	t := doc.Target
	fmt.Fprintf(w, "forge db analyze — %s target", t.Kind)
	if t.Identity != "" {
		fmt.Fprintf(w, " %s", t.Identity)
	}
	if t.ServerVersion != "" {
		fmt.Fprintf(w, " (postgres %s)", t.ServerVersion)
	}
	if doc.Base != "" {
		fmt.Fprintf(w, ", base %s", doc.Base)
	}
	fmt.Fprintln(w)
	for _, m := range doc.Migrations {
		fmt.Fprintf(w, "\n%s\n", m.File)
		if !m.Analyzed {
			fmt.Fprintf(w, "  not analyzed: %s\n", m.Reason)
			continue
		}
		for _, st := range m.Statements {
			fmt.Fprintf(w, "  line %d: %s\n", st.Line, oneLine(st.SQL, 72))
			switch {
			case st.Error != "":
				fmt.Fprintf(w, "    failed: %s\n", st.Error)
			case !st.Executed:
				fmt.Fprintf(w, "    not executed: %s\n", st.Reason)
			default:
				fmt.Fprintf(w, "    %.1fms\n", st.DurationMS)
			}
			var locks, held []string
			for _, l := range st.Locks {
				switch {
				case l.New:
				case l.HeldBefore:
					held = append(held, l.Relation+" "+l.Mode)
				default:
					locks = append(locks, l.Relation+" "+l.Mode)
				}
			}
			if len(locks) > 0 {
				fmt.Fprintf(w, "    locks:    %s\n", strings.Join(locks, ", "))
			}
			if len(held) > 0 {
				fmt.Fprintf(w, "    held:     %s (taken by an earlier statement)\n", strings.Join(held, ", "))
			}
			for _, rw := range st.Rewrites {
				if !rw.New {
					fmt.Fprintf(w, "    rewrote:  %s (%s)\n", rw.Relation, rw.Kind)
				}
			}
			for _, sc := range st.Scans {
				if !sc.New {
					fmt.Fprintf(w, "    scanned:  %s (%d seq scan(s), %d rows)\n", sc.Relation, sc.SeqScan, sc.SeqTupRead)
				}
			}
		}
	}
	if len(doc.Findings) > 0 {
		fmt.Fprintln(w, "\nfindings:")
		for _, f := range doc.Findings {
			fmt.Fprintf(w, "  %s %s:%d [%s] %s\n", f.Severity, f.File, f.Line, f.Rule, f.Message)
			if f.Fix != "" {
				fmt.Fprintf(w, "      fix: %s\n", f.Fix)
			}
			if f.Note != "" {
				fmt.Fprintf(w, "      %s\n", f.Note)
			}
		}
	}
	s := doc.Summary
	fmt.Fprintf(w, "\n%d migration(s) analyzed, %d statement(s) executed", s.MigrationsScanned, s.StatementsExecuted)
	if s.Reason != "" {
		fmt.Fprintf(w, " — %s", s.Reason)
	}
	fmt.Fprintln(w)
}

// analyzeRelPath names path relative to the project, slash-separated.
func analyzeRelPath(projectDir, path string) string {
	if rel, err := filepath.Rel(projectDir, path); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(path)
}

func cfgPostgresVersion(cfg *config.ProjectConfig) string {
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.Database.PostgresVersion)
}
