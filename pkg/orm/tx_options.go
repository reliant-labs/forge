package orm

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// Isolation selects a transaction's isolation level.
//
// It has no default. The zero value is UNSET, and every transaction opener
// rejects it: SERIALIZABLE and READ COMMITTED make opposite correctness
// arguments (the database proves serializability, versus a lock or a single
// statement does), so whichever one a blank field silently meant would be
// wrong for the code written for the other. Spell the level with
// [Serializable], [RepeatableRead] or [ReadCommitted].
type Isolation int

const (
	// IsolationSerializable is SERIALIZABLE: postgres aborts with 40001 any
	// transaction it cannot order serially against the others, which is what
	// makes read-check-write correct with no locks — provided the aborted
	// transaction is retried. See [Serializable].
	IsolationSerializable Isolation = iota + 1

	// IsolationRepeatableRead is postgres snapshot isolation: every
	// statement sees the snapshot taken at the transaction's first query,
	// and a write to a row changed since then fails with 40001. It does NOT
	// detect write skew — two transactions each reading what the other
	// writes — which SERIALIZABLE does. See [RepeatableRead].
	IsolationRepeatableRead

	// IsolationReadCommitted is postgres's own default. Each STATEMENT sees
	// rows committed before it began, so two reads in one transaction may
	// disagree and read-then-write has no protection against a lost update
	// unless a lock (SELECT … FOR UPDATE, pg_advisory_xact_lock) provides it.
	// It takes no predicate locks and cannot raise 40001. See [ReadCommitted].
	IsolationReadCommitted
)

// sqlLevel maps an Isolation onto database/sql's constants. Only a validated
// level reaches it.
func (i Isolation) sqlLevel() sql.IsolationLevel {
	switch i {
	case IsolationReadCommitted:
		return sql.LevelReadCommitted
	case IsolationRepeatableRead:
		return sql.LevelRepeatableRead
	case IsolationSerializable:
		return sql.LevelSerializable
	}
	panic(fmt.Sprintf("orm: unvalidated isolation %d", int(i)))
}

// String names the isolation level as postgres spells it.
func (i Isolation) String() string {
	switch i {
	case IsolationReadCommitted:
		return "read committed"
	case IsolationRepeatableRead:
		return "repeatable read"
	case IsolationSerializable:
		return "serializable"
	case 0:
		return "unset"
	}
	return fmt.Sprintf("Isolation(%d)", int(i))
}

// TxOptions is a transaction's policy: its isolation level, its access mode
// and how many times it may run. Every opener takes one — RunTx,
// RunTransaction and BeginDetached — and every field that decides behaviour
// must be set: the zero value is rejected.
//
// Name one of the three levels and adjust it, rather than writing the
// struct out:
//
//	db.RunTx(ctx, orm.Serializable, fn)                     // retried up to DefaultTxMaxAttempts
//	db.RunTx(ctx, orm.ReadCommitted, fn)                    // one attempt
//	db.RunTx(ctx, orm.ReadCommitted.WithMaxAttempts(3), fn) // retried on deadlock (40P01)
//	tx, err := db.BeginDetached(ctx, orm.ReadCommitted)
//
// The level and the retry budget are stated together on purpose: retry is
// what makes SERIALIZABLE correct (an aborted transaction that is not re-run
// is a failed request), and what READ COMMITTED needs almost never.
type TxOptions struct {
	// Isolation selects the isolation level. Required.
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

	// MaxAttempts bounds how many times fn runs: 1 is no retry, N re-runs fn
	// from the top on 40001 / 40P01 until N attempts have failed. Required
	// (at least 1). BeginDetached and BeginTx hand the transaction to the
	// caller, so they cannot retry and require exactly 1.
	MaxAttempts int
}

// DefaultTxMaxAttempts is the attempt budget [Serializable] and
// [RepeatableRead] carry: one try plus six retries.
//
// Sized from reliant's measurement on a single hot row under SERIALIZABLE
// (5 writes per writer): with 3 retries the failure rate was 0% at 4
// concurrent writers, 5% at 8 and 8.8% at 16; at 32 writers every write
// completed within one further attempt. Six retries covers 32-way contention
// with margin, and because a transaction that does not conflict never sleeps,
// the extra headroom costs nothing on the uncontended path.
const DefaultTxMaxAttempts = 7

// The three isolation levels, each with the retry budget it needs. Treat them
// as constants: derive a variant with WithMaxAttempts / WithReadOnly, never by
// assigning to them.
var (
	// Serializable is SERIALIZABLE, retried on 40001 / 40P01 up to
	// DefaultTxMaxAttempts. Correct for read-check-write with NO locks: the
	// database aborts the loser of a race and the retry re-reads committed
	// state. Do not take a lock inside it (forgeconv-tx-lock-under-
	// serializable): the waiter wakes into a stale snapshot and fails 40001.
	Serializable = TxOptions{Isolation: IsolationSerializable, MaxAttempts: DefaultTxMaxAttempts}

	// RepeatableRead is REPEATABLE READ (snapshot isolation), retried on
	// 40001 / 40P01 up to DefaultTxMaxAttempts. A consistent snapshot for a
	// multi-statement read or report; it does not prevent write skew.
	RepeatableRead = TxOptions{Isolation: IsolationRepeatableRead, MaxAttempts: DefaultTxMaxAttempts}

	// ReadCommitted is READ COMMITTED, one attempt. Correct for a single
	// statement, or for read-check-write whose correctness comes from a lock
	// it takes (SELECT … FOR UPDATE, pg_advisory_xact_lock). It never raises
	// 40001, so it has nothing to retry unless it can deadlock; then say so
	// with WithMaxAttempts.
	ReadCommitted = TxOptions{Isolation: IsolationReadCommitted, MaxAttempts: 1}
)

// WithMaxAttempts returns o with the attempt budget n (1 = no retry).
func (o TxOptions) WithMaxAttempts(n int) TxOptions {
	o.MaxAttempts = n
	return o
}

// WithReadOnly returns o as a READ ONLY transaction (DEFERRABLE under
// SERIALIZABLE; see TxOptions.ReadOnly).
func (o TxOptions) WithReadOnly() TxOptions {
	o.ReadOnly = true
	return o
}

// String describes the policy: "serializable, 7 attempts".
func (o TxOptions) String() string {
	var b strings.Builder
	b.WriteString(o.Isolation.String())
	if o.ReadOnly {
		b.WriteString(" read only")
		if o.Isolation == IsolationSerializable {
			b.WriteString(" deferrable")
		}
	}
	if o.MaxAttempts == 1 {
		b.WriteString(", 1 attempt")
	} else {
		fmt.Fprintf(&b, ", %d attempts", o.MaxAttempts)
	}
	return b.String()
}

// ErrInvalidTxOptions is returned by a transaction opener handed a TxOptions
// that does not say what it means: an unset (or unknown) isolation level, an
// unset attempt budget, or a retry budget on an opener that cannot retry. The
// error names the call site. Nothing is begun.
var ErrInvalidTxOptions = errors.New("orm: invalid transaction options")

// validate rejects options that leave a decision to a default. op names the
// opener ("RunTx", "BeginDetached", …) for the error; detached is true for
// an opener that hands the transaction to the caller and so cannot retry.
func (o TxOptions) validate(op string, detached bool) error {
	var problem string
	switch {
	case o.Isolation == 0:
		problem = "names no isolation level — pass orm.Serializable or orm.ReadCommitted (see orm.TxOptions)"
	case o.Isolation < IsolationSerializable || o.Isolation > IsolationReadCommitted:
		problem = fmt.Sprintf("names an unknown isolation level %d", int(o.Isolation))
	case o.MaxAttempts < 1:
		problem = fmt.Sprintf("sets no attempt budget (MaxAttempts %d) — use orm.%s, or .WithMaxAttempts(n) with n ≥ 1", o.MaxAttempts, levelVar(o.Isolation))
	case detached && o.MaxAttempts != 1:
		problem = fmt.Sprintf("asks for %d attempts, but the caller owns this transaction and %s cannot retry it — pass orm.%s.WithMaxAttempts(1)", o.MaxAttempts, op, levelVar(o.Isolation))
	default:
		return nil
	}
	return fmt.Errorf("%w: %s at %s %s", ErrInvalidTxOptions, op, callSite(), problem)
}

// levelVar is the package-level TxOptions var that spells level.
func levelVar(level Isolation) string {
	switch level {
	case IsolationReadCommitted:
		return "ReadCommitted"
	case IsolationRepeatableRead:
		return "RepeatableRead"
	}
	return "Serializable"
}

// ormDir is this package's source directory, for callSite.
var ormDir = func() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(file)
}()

// callSite returns file:line of the first caller outside this package's
// non-test sources — the line that opened the transaction.
func callSite() string {
	pcs := make([]uintptr, 32)
	n := runtime.Callers(3, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if filepath.Dir(f.File) != ormDir || strings.HasSuffix(f.File, "_test.go") {
			return fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		if !more {
			return "an unknown call site"
		}
	}
}
