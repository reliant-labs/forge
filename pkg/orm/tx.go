package orm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uptrace/bun"

	"github.com/reliant-labs/forge/pkg/svcerr"
)

// Context-carried transactions.
//
// RunTx opens a transaction and hands fn a context.Context that CARRIES it.
// Every query issued with that ctx through the same database — a generated
// delegate (db.GetJobByID(ctx, s.deps.DB, id)), a pkg/crud.Repo method, a
// store method, raw SQL through Exec/Query/QueryRow or db.Bun() — runs inside
// the transaction, although the handle it was given is still the plain
// *Client. Nothing threads a tx handle through signatures, so a service is
// written once and composes into any caller's transaction.
//
// The default is SERIALIZABLE, retried on serialization failure and deadlock
// (SQLSTATE 40001 / 40P01). That pairing is what makes the ordinary
// read-check-write shape correct with no explicit locking:
//
//	err := s.deps.DB.RunTx(ctx, func(ctx context.Context) error {
//	    inv, err := db.GetInvoiceByID(ctx, s.deps.DB, id)
//	    if err != nil {
//	        return err
//	    }
//	    if inv.BalanceCents < amount {
//	        return svcerr.FailedPrecondition("payment exceeds the balance")
//	    }
//	    inv.PaidCents += amount
//	    return db.UpdateInvoiceMasked(ctx, s.deps.DB, inv, []string{"paid_cents"})
//	})
//
// Two concurrent payments both read the same balance; postgres detects that
// the two transactions cannot be ordered serially, aborts one with 40001, and
// RunTx re-runs it from the top — where it reads the COMMITTED balance and
// takes the branch that is now correct. No SELECT … FOR UPDATE, no per-entity
// lock helper, and no way to forget one on a new code path.
//
// # The contract fn must honor
//
// fn MAY RUN MORE THAN ONCE. A retry rolls back everything the failed attempt
// did in the database and calls fn again from the start, so fn must have no
// effect outside the database: no email, no publish, no HTTP call, no
// mutation of state that outlives the call. Register such effects with
// [AfterCommit]; they run once, after the outermost commit, and a rolled-back
// or retried attempt discards them.
//
// The transaction is one database connection. Do not use the ctx handed to fn
// from several goroutines at once.

// Isolation selects a transaction's isolation level.
//
// The zero value is IsolationDefault, which means SERIALIZABLE — deliberately,
// so the weaker choice is always the one spelled out at the call site, and a
// zero-valued TxOptions can never silently drop a guarantee.
type Isolation int

const (
	// IsolationDefault is SERIALIZABLE. Correct for anything that reads and
	// then writes based on what it read.
	IsolationDefault Isolation = iota

	// IsolationSerializable is the explicit spelling of the default, for a
	// call site where serializability is load-bearing and should be stated
	// rather than inherited.
	IsolationSerializable

	// IsolationRepeatableRead is postgres snapshot isolation: every
	// statement sees the snapshot taken at the transaction's first query,
	// and a write to a row changed since then fails with 40001 (retried).
	// It does NOT detect write skew — two transactions each reading what the
	// other writes — which SERIALIZABLE does.
	IsolationRepeatableRead

	// IsolationReadCommitted is postgres's own default. Each STATEMENT sees
	// rows committed before it began, so two reads in one transaction may
	// disagree and read-then-write has no protection against a lost update.
	// It takes no predicate locks and cannot raise 40001, which makes it a
	// strict win for a single-statement transaction and a correctness
	// decision — one that needs a reason at the call site — for anything
	// longer.
	IsolationReadCommitted
)

// sqlLevel maps an Isolation onto database/sql's constants.
func (i Isolation) sqlLevel() sql.IsolationLevel {
	switch i {
	case IsolationReadCommitted:
		return sql.LevelReadCommitted
	case IsolationRepeatableRead:
		return sql.LevelRepeatableRead
	default:
		return sql.LevelSerializable
	}
}

// String names the isolation level as postgres spells it.
func (i Isolation) String() string {
	switch i {
	case IsolationReadCommitted:
		return "read committed"
	case IsolationRepeatableRead:
		return "repeatable read"
	default:
		return "serializable"
	}
}

// TxOptions configures RunTxWithOptions. The zero value is a read-write
// SERIALIZABLE transaction with the default retry budget — what RunTx uses.
type TxOptions struct {
	// Isolation selects the isolation level. Zero value = SERIALIZABLE.
	Isolation Isolation

	// ReadOnly runs the transaction READ ONLY, and under SERIALIZABLE also
	// DEFERRABLE.
	//
	// That second half is the point. A SERIALIZABLE read takes predicate
	// locks over whatever it touched (the whole relation, for a read no index
	// serves), and those locks are what make two writers on unrelated rows
	// abort each other. A SERIALIZABLE READ ONLY DEFERRABLE transaction takes
	// NO predicate locks and can never fail with 40001: it waits at its first
	// statement for a snapshot guaranteed free of later conflicts, then runs
	// outside the serialization graph entirely — neither suffering conflicts
	// nor causing them for concurrent writers.
	//
	// The cost is that wait, which can briefly delay a read on a busy
	// database. A write inside a read-only transaction is rejected by
	// postgres outright, so a mislabelled path fails loudly.
	ReadOnly bool

	// MaxAttempts bounds how many times fn runs. Zero (or negative) means the
	// default, DefaultTxMaxAttempts; 1 disables retry — useful for measuring
	// contention rather than surviving it, never for a production writer.
	MaxAttempts int
}

// DefaultTxMaxAttempts is the attempt budget a zero TxOptions gets: one try
// plus six retries.
//
// Sized from reliant's measurement on a single hot row under SERIALIZABLE
// (5 writes per writer): with 3 retries the failure rate was 0% at 4
// concurrent writers, 5% at 8 and 8.8% at 16; at 32 writers every write
// completed within one further attempt. Six retries covers 32-way contention
// with margin, and because a transaction that does not conflict never sleeps,
// the extra headroom costs nothing on the uncontended path.
const DefaultTxMaxAttempts = 7

const (
	txBaseRetryDelay = 50 * time.Millisecond
	txMaxRetryDelay  = 1 * time.Second
)

func (o TxOptions) maxAttempts() int {
	if o.MaxAttempts <= 0 {
		return DefaultTxMaxAttempts
	}
	return o.MaxAttempts
}

// ErrNoTransaction is returned by AfterCommit when ctx carries no active
// transaction to defer the callback to.
var ErrNoTransaction = errors.New("orm: no active transaction in context")

// txState is one transaction attempt: the open transaction, the identity of
// the pool it belongs to, and the callbacks to run if — and only if — THIS
// attempt commits. A retried attempt gets a fresh txState, so a callback
// registered by an attempt that was rolled back dies with it.
type txState struct {
	// db is the pool the transaction was opened on. It is the identity the
	// executor resolution checks: a transaction carried in ctx is used only
	// for queries against the SAME pool, so a transaction on database A can
	// never be handed a statement meant for database B.
	db *sql.DB
	tx bun.Tx

	// outer is the context RunTx/BeginTx was called with — the caller's
	// context, without this transaction in it. After-commit callbacks
	// receive it, so a callback that queries the database runs against the
	// pool rather than a transaction that has already ended.
	outer context.Context

	mu          sync.Mutex
	afterCommit []func(context.Context)

	// done flips once the transaction commits or rolls back. A ctx that
	// still carries a finished transaction — captured by a closure, or
	// leaked past RunTx — resolves to the plain pool again instead of
	// failing every query with sql.ErrTxDone.
	done atomic.Bool
}

func (s *txState) active() bool { return s != nil && !s.done.Load() }

// addAfterCommit queues fn to run if this attempt commits.
func (s *txState) addAfterCommit(fn func(context.Context)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.afterCommit = append(s.afterCommit, fn)
}

// finish marks the transaction ended and hands back the callbacks that
// should run: all of them on commit, none on rollback.
func (s *txState) finish(committed bool) []func(context.Context) {
	s.done.Store(true)
	s.mu.Lock()
	defer s.mu.Unlock()
	callbacks := s.afterCommit
	s.afterCommit = nil
	if !committed {
		return nil
	}
	return callbacks
}

// txKey is the context key for the transaction carried for ONE pool. Keying
// by pool rather than using a single slot is what lets transactions on two
// databases nest without the inner one shadowing the outer: each database
// finds its own.
type txKey struct{ db *sql.DB }

// innermostTxKey names the most recently opened transaction in ctx, whatever
// its pool — the one AfterCommit attaches to.
type innermostTxKey struct{}

// withTx returns ctx carrying st.
func withTx(ctx context.Context, st *txState) context.Context {
	ctx = context.WithValue(ctx, txKey{db: st.db}, st)
	return context.WithValue(ctx, innermostTxKey{}, st)
}

// ambientTx returns the still-active transaction ctx carries for pool db, or
// nil when there is none.
func ambientTx(ctx context.Context, db *sql.DB) *txState {
	if ctx == nil || db == nil {
		return nil
	}
	st, _ := ctx.Value(txKey{db: db}).(*txState)
	if !st.active() {
		return nil
	}
	return st
}

// AfterCommit defers fn until the transaction carried by ctx commits.
//
// fn runs exactly once, after the OUTERMOST commit: a RunTx that joined an
// enclosing transaction does not commit anything itself, so callbacks it
// registers wait for the owner. If the attempt rolls back — fn returned an
// error, or postgres aborted it and RunTx is retrying — the callbacks
// registered by that attempt are discarded; the retry registers its own.
//
// This is the place for every effect fn must not perform itself because fn
// can run more than once: sending an email, publishing an event, enqueueing
// a job, calling another service.
//
// fn receives the context RunTx was called with, which does not carry the
// finished transaction. It runs synchronously, in registration order, before
// RunTx returns. It cannot fail the transaction — that has already committed
// — so it returns nothing; handle or log its own errors.
//
// With nested transactions on DIFFERENT databases, fn attaches to the
// innermost one and runs when that one commits.
//
// Returns ErrNoTransaction when ctx carries no active transaction.
func AfterCommit(ctx context.Context, fn func(context.Context)) error {
	st, _ := ctx.Value(innermostTxKey{}).(*txState)
	if !st.active() {
		return ErrNoTransaction
	}
	st.addAfterCommit(fn)
	return nil
}

// ─── RunTx on *Client ─────────────────────────────────────────────────────

// RunTx runs fn in a SERIALIZABLE transaction carried by the ctx fn receives,
// retrying on serialization failure and deadlock. fn MAY RUN MORE THAN ONCE;
// see the package documentation in tx.go and AfterCommit.
//
// If ctx already carries an active transaction on this database, RunTx JOINS
// it: fn runs inside the enclosing transaction, and committing, rolling back
// and retrying stay with whoever opened it. That is what lets a service wrap
// its own work in RunTx and still compose into a caller's larger one.
//
// fn's error is returned verbatim, so svcerr sentinels and domain errors
// survive. A transaction still conflicting after its last attempt returns a
// svcerr.Aborted error (the driver error is kept as its cause).
func (c *Client) RunTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return c.RunTxWithOptions(ctx, TxOptions{}, fn)
}

// RunTxReadOnly is RunTx for a transaction that only reads: SERIALIZABLE
// READ ONLY DEFERRABLE. See TxOptions.ReadOnly for why that matters.
func (c *Client) RunTxReadOnly(ctx context.Context, fn func(ctx context.Context) error) error {
	return c.RunTxWithOptions(ctx, TxOptions{ReadOnly: true}, fn)
}

// RunTxWithOptions is RunTx with explicit options.
//
// When ctx already carries an active transaction on this database, opts are
// IGNORED and fn joins it: the enclosing transaction exists and its mode
// cannot change mid-flight. That is safe in the direction that matters — a
// read-only call joining a read-write transaction just reads — and the
// reverse, a write joining a read-only transaction, is rejected by postgres
// at the write, which is exactly where the mistake is.
func (c *Client) RunTxWithOptions(ctx context.Context, opts TxOptions, fn func(ctx context.Context) error) error {
	if ambientTx(ctx, c.bun.DB) != nil {
		return fn(ctx)
	}

	maxAttempts := opts.maxAttempts()
	for attempt := 1; ; attempt++ {
		err := c.runTxAttempt(ctx, opts, fn)
		if err == nil {
			return nil
		}
		if !isRetryableTxError(err) {
			return err
		}
		if attempt >= maxAttempts {
			return svcerr.WithCause(
				svcerr.Aborted("the operation conflicted with concurrent changes; retry it"),
				fmt.Errorf("orm: transaction still conflicting after %d attempts: %w", attempt, err))
		}
		if err := sleepCtx(ctx, txRetryDelay(attempt)); err != nil {
			return err
		}
	}
}

// runTxAttempt runs ONE attempt: begin, fn, commit — rolling back if fn fails
// or panics.
func (c *Client) runTxAttempt(ctx context.Context, opts TxOptions, fn func(ctx context.Context) error) (err error) {
	tx, err := c.beginTx(ctx, opts.Isolation.sqlLevel(), opts.ReadOnly)
	if err != nil {
		return NewTransactionError("begin", err)
	}

	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := fn(withTx(ctx, tx.state)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return NewTransactionError("commit", err)
	}
	committed = true
	return nil
}

// beginTx opens a transaction at level, READ ONLY when asked — and
// DEFERRABLE when it is both read-only and SERIALIZABLE, which is the
// combination where DEFERRABLE buys "no predicate locks, cannot be aborted"
// (see TxOptions.ReadOnly). database/sql has no DEFERRABLE flag, so it is set
// as the transaction's first statement.
func (c *Client) beginTx(ctx context.Context, level sql.IsolationLevel, readOnly bool) (*Tx, error) {
	tx, err := c.BeginTx(ctx, &sql.TxOptions{Isolation: level, ReadOnly: readOnly})
	if err != nil {
		return nil, err
	}
	if readOnly && level == sql.LevelSerializable {
		if _, err := tx.tx.ExecContext(ctx, "SET TRANSACTION READ ONLY DEFERRABLE"); err != nil {
			_ = tx.Rollback()
			return nil, fmt.Errorf("set transaction read only deferrable: %w", err)
		}
	}
	return tx, nil
}

// ─── RunTx on *Tx: always a join ───────────────────────────────────────────

// RunTx on a *Tx runs fn inside THIS transaction, with a ctx that carries it,
// so code written against the plain client (generated delegates, stores,
// pkg/crud) participates. It never begins, commits, retries or rolls back:
// the owner of the *Tx does. This is how code holding an explicit handle from
// RunTransaction calls a service written with RunTx.
func (t *Tx) RunTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if t.state == nil {
		return fn(ctx)
	}
	return fn(withTx(ctx, t.state))
}

// RunTxReadOnly on a *Tx is a join; see RunTx. The read-only request is
// ignored because the transaction's mode is already fixed.
func (t *Tx) RunTxReadOnly(ctx context.Context, fn func(ctx context.Context) error) error {
	return t.RunTx(ctx, fn)
}

// RunTxWithOptions on a *Tx is a join; opts are ignored. See RunTx.
func (t *Tx) RunTxWithOptions(ctx context.Context, _ TxOptions, fn func(ctx context.Context) error) error {
	return t.RunTx(ctx, fn)
}

// ─── retry policy ──────────────────────────────────────────────────────────

// isRetryableTxError reports whether err — anywhere in its %w chain — is a
// postgres failure that re-running the whole transaction can cure:
//
//   - 40001 serialization_failure: SERIALIZABLE / REPEATABLE READ found the
//     transaction could not be ordered against a concurrent one.
//   - 40P01 deadlock_detected: postgres broke a lock cycle by aborting this
//     transaction.
//
// Deliberately NOT retried: 23505 unique_violation, which is a real answer
// (pkg/crud maps it to AlreadyExists) — and postgres already reports a
// read-then-insert race under SERIALIZABLE as 40001 rather than 23505.
func isRetryableTxError(err error) bool {
	state, _, ok := pgDiagnostic(err)
	return ok && (state == "40001" || state == "40P01")
}

// txRetryDelay is exponential backoff from txBaseRetryDelay, capped at
// txMaxRetryDelay, with ±25% jitter so writers that collided once do not
// collide again in lockstep. attempt is the 1-based attempt that just failed.
func txRetryDelay(attempt int) time.Duration {
	delay := txBaseRetryDelay << min(attempt-1, 10)
	if delay > txMaxRetryDelay {
		delay = txMaxRetryDelay
	}
	return delay - delay/4 + rand.N(delay/2)
}

// sleepCtx waits d, or returns ctx's error if ctx ends first — a request
// that was cancelled must not keep retrying.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ─── executor resolution ───────────────────────────────────────────────────

// ambientResolver is the bun.ConnResolver every Client installs on its
// *bun.DB. Bun consults it for each query built from the *bun.DB (NewSelect,
// NewInsert, NewRaw, …) whose connection was not fixed explicitly, passing
// the ctx the query runs with. It answers with the transaction ctx carries
// for THIS pool — so every generated delegate and pkg/crud.Repo method,
// handed the plain *Client, joins the caller's RunTx without being told.
//
// It returns the *sql.Tx rather than the bun.Tx: bun runs its query hooks
// (tracing) around the statement already, and bun.Tx's own methods would run
// them a second time — the same unwrapping bun applies in baseQuery.setConn.
//
// A query built off an explicit *Tx handle (tx.Bun()) never reaches here:
// bun pins its connection, so an explicit handle always wins.
type ambientResolver struct{ db *sql.DB }

func (r ambientResolver) ResolveConn(ctx context.Context, _ bun.Query) bun.IConn {
	if st := ambientTx(ctx, r.db); st != nil {
		return st.tx.Tx
	}
	return nil
}

func (ambientResolver) Close() error { return nil }

// ambientDB is the bun.IDB Client.Bun() returns: the *bun.DB, with its three
// direct-execution methods made transaction-aware.
//
// Bun's query builders consult ambientResolver on their own, but
// (*bun.DB).ExecContext / QueryContext / QueryRowContext go straight to the
// pool. Left alone, `db.Bun().ExecContext(ctx, …)` inside RunTx would escape
// the transaction — losing atomicity, and blocking forever if it touched a
// row the transaction had locked (two connections, one goroutine: a deadlock
// postgres cannot see). These overrides route them through the bun.Tx so
// hooks and query formatting are unchanged.
type ambientDB struct{ *bun.DB }

func (d ambientDB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if st := ambientTx(ctx, d.DB.DB); st != nil {
		return st.tx.ExecContext(ctx, query, args...)
	}
	return d.DB.ExecContext(ctx, query, args...)
}

func (d ambientDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if st := ambientTx(ctx, d.DB.DB); st != nil {
		return st.tx.QueryContext(ctx, query, args...)
	}
	return d.DB.QueryContext(ctx, query, args...)
}

func (d ambientDB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if st := ambientTx(ctx, d.DB.DB); st != nil {
		return st.tx.QueryRowContext(ctx, query, args...)
	}
	return d.DB.QueryRowContext(ctx, query, args...)
}

var _ bun.IDB = ambientDB{}

// conn returns the executor for raw SQL on c: the transaction ctx carries for
// c's pool, or the pool itself.
func (c *Client) conn(ctx context.Context) bun.IConn {
	if st := ambientTx(ctx, c.bun.DB); st != nil {
		return st.tx.Tx
	}
	return c.bun.DB
}
