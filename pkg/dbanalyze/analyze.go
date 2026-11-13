package dbanalyze

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Default size thresholds of Config.
const (
	DefaultSmallTableRows = 10_000
	DefaultLargeTableRows = 1_000_000
)

// Config is what an analysis knows besides the target.
type Config struct {
	// TableRows are size hints for tables the target cannot size: a scratch
	// target has no rows, a dsn target with no statistics no estimates.
	// Keyed by table name ("orders", or "schema.orders" outside public).
	TableRows map[string]int64
	// SmallTableRows: a finding on a table whose size is known and below it
	// is dropped. Zero means DefaultSmallTableRows; negative keeps every
	// finding.
	SmallTableRows int64
	// LargeTableRows: a table at or above it is large. Zero means
	// DefaultLargeTableRows. Read by the query pass.
	LargeTableRows int64
}

func (c Config) smallTableRows() int64 {
	if c.SmallTableRows == 0 {
		return DefaultSmallTableRows
	}
	return c.SmallTableRows
}

// MigrationFile is one migration to analyze.
type MigrationFile struct {
	// File is how the document names it (a project-relative path).
	File    string
	Version uint64
	SQL     string
}

// StatementError is a statement that failed on the target for a reason other
// than its timeout: the analysis cannot continue past it, because every later
// statement assumes it applied.
type StatementError struct {
	File     string
	Line     int
	SQLState string
	Err      error
}

func (e *StatementError) Error() string {
	return fmt.Sprintf("%s:%d: the statement failed on the target: %v", e.File, e.Line, e.Err)
}

func (e *StatementError) Unwrap() error { return e.Err }

// AnalyzeMigrations applies files, in order, statement by statement inside
// the session's transaction, measuring each, and returns what each did and
// the findings. Findings carry SeverityError; a caller grading them (a
// warn-first rollout, a project's dial) adjusts that.
//
// Every relation that exists when it starts is pre-existing; one a statement
// of the run creates is new, and new relations are exempt from every rule:
// locking or rewriting a table nobody reads yet is free.
//
// A statement that cannot run inside a transaction is not executed: its lock
// is reported statically. A statement that times out is a finding and ends
// the run, as does a statement that fails, which is returned as a
// *StatementError (the analysis could not be made). The migrations the run did
// not reach are returned unanalyzed, with the reason.
func AnalyzeMigrations(ctx context.Context, s *Session, info TargetInfo, files []MigrationFile, cfg Config) ([]Migration, []Finding, error) {
	start, err := s.takeSnapshot(ctx)
	if err != nil {
		return nil, nil, err
	}
	a := &analysis{s: s, info: info, cfg: cfg, pre: map[uint32]bool{}}
	for oid := range start.rels {
		a.pre[oid] = true
	}
	migs := make([]Migration, 0, len(files))
	var stopped string
	for _, f := range files {
		if stopped != "" {
			migs = append(migs, Migration{File: f.File, Version: f.Version, Reason: stopped, Statements: []StatementResult{}})
			continue
		}
		m, stop, err := a.migration(ctx, f)
		migs = append(migs, m)
		if err != nil {
			return migs, a.findings, err
		}
		if stop != "" {
			stopped = stop
		}
	}
	return migs, a.findings, nil
}

type analysis struct {
	s        *Session
	info     TargetInfo
	cfg      Config
	pre      map[uint32]bool
	findings []Finding
}

// migration analyzes one file. stop is non-empty when the run must not go
// on (a statement timed out); err when it could not (a statement failed).
func (a *analysis) migration(ctx context.Context, f MigrationFile) (Migration, string, error) {
	m := Migration{File: f.File, Version: f.Version, Analyzed: true, Statements: []StatementResult{}}
	stmts := SplitStatements(f.SQL)
	// The lock modes this file has acquired so far. A migration file runs
	// as one transaction, so a lock one statement takes is held through
	// every later statement of the file; one an EARLIER file took was
	// released when that file committed, and is not counted against this
	// one (see Lock.HeldBefore).
	fileLocks := map[lockKey]bool{}
	for i, st := range stmts {
		res := StatementResult{Index: st.Index, Line: st.Line, SQL: st.SQL,
			Locks: []Lock{}, Rewrites: []Rewrite{}, Scans: []Scan{}}
		if label := st.NonTransactional(); label != "" {
			res.Reason = label + " cannot run inside a transaction, so it was not executed; its lock is known statically"
			res.Locks = append(res.Locks, st.staticLocks()...)
			m.Statements = append(m.Statements, res)
			continue
		}
		meas, err := a.s.measure(ctx, st.SQL)
		if err != nil {
			return m, "", fmt.Errorf("%s:%d: measure: %w", f.File, st.Line, err)
		}
		res.DurationMS = math.Round(float64(meas.duration.Microseconds())) / 1000
		if meas.err != nil {
			stop, err := a.failed(&m, f, st, res, meas.err, stmts[i+1:])
			return m, stop, err
		}
		res.Executed = true
		a.adoptRebuiltIndexes(&meas)
		for _, k := range meas.acquired {
			fileLocks[k] = true
		}
		a.record(&res, meas)
		a.judge(f.File, st, meas, fileLocks)
		m.Statements = append(m.Statements, res)
	}
	return m, "", nil
}

// failed ends the file at a statement that failed on the target, listing the
// rest of it as unreached. A timeout is a finding and stops the run (stop is
// the reason); any other failure is a *StatementError.
func (a *analysis) failed(m *Migration, f MigrationFile, st Statement, res StatementResult, cause error, rest []Statement) (string, error) {
	res.Error, res.SQLState = cause.Error(), sqlState(cause)
	m.Statements = append(m.Statements, res)
	reason := fmt.Sprintf("not reached: %s:%d failed", f.File, st.Line)
	if res.SQLState == sqlStateQueryCanceled {
		reason = fmt.Sprintf("not reached: %s:%d exceeded statement_timeout", f.File, st.Line)
		a.add(Finding{Rule: RuleStatementTimeout, File: f.File, Line: st.Line,
			Message: "the statement exceeded statement_timeout on the target and was cancelled", Fix: fixStatementTimeout})
	}
	m.Statements = append(m.Statements, unreached(rest, reason)...)
	if res.SQLState == sqlStateQueryCanceled {
		return reason, nil
	}
	msg := cause
	if res.SQLState == sqlStateLockNotAvailable {
		msg = fmt.Errorf("%w (another session holds a lock this statement needs: the target is in use)", cause)
	}
	return reason, &StatementError{File: f.File, Line: st.Line, SQLState: res.SQLState, Err: msg}
}

// adoptRebuiltIndexes recognizes the indexes a statement rebuilt rather than
// built. ALTER COLUMN … TYPE drops every index on the column and recreates it
// under a new OID with the same name: the catalog shows a pre-existing index
// gone and a "new" one on a pre-existing table, which would read as an index
// built without CONCURRENTLY — a fix that does not apply, since the
// statement, not an index build, is what needs changing.
//
// A recreated index is the one it replaces: it is never a build, it is
// pre-existing (for this statement and every later one) when that one was,
// and it counts as a rewrite of it when it was written to new storage, which
// the table-rewrite rule judges with its table — and as nothing at all when
// postgres reused its storage (a binary-compatible type change).
func (a *analysis) adoptRebuiltIndexes(m *measurement) {
	type key struct {
		table uint32
		name  string
	}
	gone := map[key]relation{}
	for _, oid := range m.dropped {
		if r := m.rels[oid]; r.isIndex() {
			gone[key{r.table, r.name}] = r
		}
	}
	if len(gone) == 0 {
		return
	}
	created := m.created[:0:0]
	for _, oid := range m.created {
		r := m.rels[oid]
		old, rebuilt := gone[key{r.table, r.name}]
		if !r.isIndex() || !rebuilt {
			created = append(created, oid)
			continue
		}
		if a.pre[old.oid] {
			a.pre[oid] = true
		}
		if r.relfilenode != old.relfilenode {
			m.rewritten = append(m.rewritten, oid)
		}
	}
	m.created = created
	sort.Slice(m.rewritten, func(i, j int) bool { return m.rewritten[i] < m.rewritten[j] })
}

// unreached lists statements the run did not execute.
func unreached(stmts []Statement, reason string) []StatementResult {
	out := make([]StatementResult, 0, len(stmts))
	for _, st := range stmts {
		out = append(out, StatementResult{Index: st.Index, Line: st.Line, SQL: st.SQL, Reason: reason,
			Locks: []Lock{}, Rewrites: []Rewrite{}, Scans: []Scan{}})
	}
	return out
}

// record writes a measurement into the statement's result, by name.
func (a *analysis) record(res *StatementResult, m measurement) {
	sized := map[uint32]bool{}
	size := func(oid uint32) {
		if t := a.tableOf(m, oid); a.pre[t] {
			sized[t] = true
		}
	}
	for _, k := range m.acquired {
		res.Locks = append(res.Locks, Lock{Relation: m.rels[k.oid].name, Mode: k.mode, New: !a.pre[k.oid]})
		size(k.oid)
	}
	for _, k := range m.held {
		res.Locks = append(res.Locks, Lock{Relation: m.rels[k.oid].name, Mode: k.mode, New: !a.pre[k.oid], HeldBefore: true})
	}
	for _, oid := range m.rewritten {
		r := m.rels[oid]
		rw := Rewrite{Relation: r.name, Kind: "table", New: !a.pre[oid]}
		if r.isIndex() {
			rw.Kind, rw.Table = "index", m.rels[r.table].name
		}
		res.Rewrites = append(res.Rewrites, rw)
		size(oid)
	}
	oids := make([]uint32, 0, len(m.scanned))
	for oid := range m.scanned {
		oids = append(oids, oid)
	}
	sort.Slice(oids, func(i, j int) bool { return oids[i] < oids[j] })
	for _, oid := range oids {
		d := m.scanned[oid]
		res.Scans = append(res.Scans, Scan{Relation: m.rels[oid].name, SeqScan: d[0], SeqTupRead: d[1], New: !a.pre[oid]})
		size(oid)
	}
	for oid := range sized {
		if res.Rows == nil {
			res.Rows = map[string]RowCount{}
		}
		res.Rows[m.rels[oid].name] = a.rows(m.rels[oid])
	}
}

// tableOf is the table an index belongs to, or oid itself.
func (a *analysis) tableOf(m measurement, oid uint32) uint32 {
	if r := m.rels[oid]; r.isIndex() && r.table != 0 {
		return r.table
	}
	return oid
}

// rows sizes a table: the target's own estimate when it has statistics,
// else the configured hint, else unknown.
func (a *analysis) rows(r relation) RowCount {
	if a.info.HasStatistics && r.reltuples >= 0 {
		v := int64(r.reltuples)
		return RowCount{Value: &v, Source: RowsFromReltuples}
	}
	if v, ok := a.hint(r.name); ok {
		return RowCount{Value: &v, Source: RowsFromHints}
	}
	return RowCount{Source: RowsUnknown}
}

func (a *analysis) hint(name string) (int64, bool) {
	if v, ok := a.cfg.TableRows[name]; ok {
		return v, true
	}
	if !strings.Contains(name, ".") {
		v, ok := a.cfg.TableRows["public."+name]
		return v, ok
	}
	return 0, false
}

// judge applies the rules to one executed statement.
func (a *analysis) judge(file string, st Statement, m measurement, fileLocks map[lockKey]bool) {
	// TRUNCATE swaps in empty storage — a "rewrite" that copies nothing —
	// and is the textual migration-safety lane's to judge (destructive).
	if st.first() == "truncate" {
		return
	}
	modes := map[uint32][]string{}
	for k := range fileLocks {
		modes[k.oid] = append(modes[k.oid], k.mode)
	}
	holds := func(oid uint32, pred func(string) bool) bool {
		for _, mode := range modes[oid] {
			if pred(mode) {
				return true
			}
		}
		return false
	}

	rewrittenTables := map[uint32][]string{}
	for _, oid := range m.rewritten {
		t := a.tableOf(m, oid)
		if !a.pre[oid] || !a.pre[t] {
			continue
		}
		if oid != t {
			rewrittenTables[t] = append(rewrittenTables[t], m.rels[oid].name)
		} else if _, ok := rewrittenTables[t]; !ok {
			rewrittenTables[t] = nil
		}
	}
	for _, t := range sortedOIDs(rewrittenTables) {
		name := m.rels[t].name
		msg := fmt.Sprintf("rewrote %s: every row is copied to new storage, and every index rebuilt, under ACCESS EXCLUSIVE", name)
		if a.indexOnlyRewrite(m, t) {
			msg = fmt.Sprintf("rebuilt index(es) %s of %s", strings.Join(rewrittenTables[t], ", "), name)
		}
		a.addSized(m, t, Finding{Rule: RuleTableRewrite, File: file, Line: st.Line, Relation: name, Message: msg, Fix: fixTableRewrite})
	}

	builtOn := map[uint32][]string{}
	for _, oid := range m.created {
		r := m.rels[oid]
		if r.isIndex() && a.pre[r.table] {
			builtOn[r.table] = append(builtOn[r.table], r.name)
		}
	}
	for _, t := range sortedOIDs(builtOn) {
		name := m.rels[t].name
		a.addSized(m, t, Finding{Rule: RuleBlockingIndexBuild, File: file, Line: st.Line, Relation: name,
			Message: fmt.Sprintf("built index %s on %s without CONCURRENTLY: the build holds SHARE (or stronger), which blocks every write to %s until it finishes",
				strings.Join(builtOn[t], ", "), name, name),
			Fix: fixIndexBuild})
	}

	for _, t := range a.preTables(m) {
		name := m.rels[t].name
		_, rewritten := rewrittenTables[t]
		rewritten = rewritten && !a.indexOnlyRewrite(m, t)
		scanned := m.scanned[t][0] > 0
		ael := rewritten || holds(t, func(mode string) bool { return mode == "AccessExclusiveLock" })
		if ael && (scanned || rewritten) {
			what := "scanned every row of"
			if rewritten {
				what = "rewrote"
			}
			a.addSized(m, t, Finding{Rule: RuleAccessExclusiveLock, File: file, Line: st.Line, Relation: name,
				Message: fmt.Sprintf("held ACCESS EXCLUSIVE on %s, which blocks every read and write of it, while the statement %s it", name, what),
				Fix:     fixAccessExclusive})
		}
		if scanned && !rewritten && len(builtOn[t]) == 0 && holds(t, blocksWrites) {
			a.addSized(m, t, Finding{Rule: RuleValidationScan, File: file, Line: st.Line, Relation: name,
				Message: fmt.Sprintf("scanned every row of %s under a lock that blocks writes to it (%s)", name, strongest(modes[t])),
				Fix:     validationFix(st)})
		}
	}
}

// indexOnlyRewrite reports whether t's rewrite in m touched only indexes.
func (a *analysis) indexOnlyRewrite(m measurement, t uint32) bool {
	for _, oid := range m.rewritten {
		if oid == t {
			return false
		}
	}
	return true
}

// preTables are the pre-existing tables the statement scanned or rewrote.
func (a *analysis) preTables(m measurement) []uint32 {
	set := map[uint32]bool{}
	for oid := range m.scanned {
		set[oid] = true
	}
	for _, oid := range m.rewritten {
		set[a.tableOf(m, oid)] = true
	}
	var out []uint32
	for oid := range set {
		if a.pre[oid] && !m.rels[oid].isIndex() {
			out = append(out, oid)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// addSized records f with t's size, dropping it when t is known to be small.
func (a *analysis) addSized(m measurement, t uint32, f Finding) {
	rc := a.rows(m.rels[t])
	if rc.Value != nil {
		if small := a.cfg.smallTableRows(); small > 0 && *rc.Value < small {
			return
		}
		f.Rows = &rc
	}
	a.add(f)
}

func (a *analysis) add(f Finding) {
	f.Severity = SeverityError
	a.findings = append(a.findings, f)
}

func validationFix(st Statement) string {
	switch {
	case st.has("set", "not", "null"):
		return fixValidateNotNull
	case st.has("foreign") || st.has("references"):
		return fixValidateFK
	case st.has("check") || st.has("constraint"):
		return fixValidateCheck
	}
	return fixValidateGeneric
}

func strongest(modes []string) string {
	best := ""
	for _, m := range modes {
		if lockStrength[m] > lockStrength[best] {
			best = m
		}
	}
	return best
}

func sortedOIDs[V any](m map[uint32]V) []uint32 {
	out := make([]uint32, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// IsStatementError reports whether err is a statement that failed on the
// target.
func IsStatementError(err error) bool {
	var se *StatementError
	return errors.As(err, &se)
}
