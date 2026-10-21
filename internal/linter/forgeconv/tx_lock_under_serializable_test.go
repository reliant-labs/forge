package forgeconv

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/linter/suppress"
)

const txLockSrc = `package store

import (
	"context"

	"github.com/reliant-labs/forge/pkg/orm"
)

const lockQuery = "SELECT id FROM jobs WHERE id = $1 FOR UPDATE"

func a(ctx context.Context, db orm.Context) error {
	return db.RunTx(ctx, orm.Serializable, func(ctx context.Context) error { // FOR UPDATE literal
		_, err := db.Exec(ctx, "SELECT 1 FROM jobs WHERE id = $1 FOR UPDATE", 1)
		return err
	})
}

func b(ctx context.Context, db orm.Context) error {
	return db.RunTx(ctx, orm.Serializable.WithMaxAttempts(3), func(ctx context.Context) error { // a const
		_, err := db.Exec(ctx, lockQuery, 1)
		return err
	})
}

func c(ctx context.Context, db orm.Context) error {
	return db.RunTx(ctx, orm.ReadCommitted, func(ctx context.Context) error { // RC: the lock is the point
		_, err := db.Exec(ctx, lockQuery, 1)
		return err
	})
}

func d(ctx context.Context, db orm.Context) error {
	tx, err := db.BeginDetached(ctx, orm.Serializable.WithMaxAttempts(1)) // advisory lock later
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", 7)
	return err
}

func e(ctx context.Context, db orm.Context) error {
	tx, err := db.BeginDetached(ctx, orm.ReadCommitted)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", 7)
	return err
}

func f(ctx context.Context, db orm.Context) error {
	return db.RunTransaction(ctx, orm.TxOptions{Isolation: orm.RepeatableRead, MaxAttempts: 2}, func(tx orm.Context) error { // LOCK TABLE
		_, err := tx.Exec(ctx, "LOCK TABLE jobs IN EXCLUSIVE MODE")
		return err
	})
}

func g(ctx context.Context, db orm.Context) error {
	return db.RunTx(ctx, orm.Serializable, lockedBody(db)) // not a func literal or a package func: not judged
}

func lockedBody(db orm.Context) func(context.Context) error { return nil }

func h(ctx context.Context, db orm.Context) error {
	return db.RunTx(ctx, orm.Serializable, bunLocked) // package func with .For("UPDATE")
}

func bunLocked(ctx context.Context) error {
	var q interface{ For(string) any }
	q.For("UPDATE")
	return nil
}

func i(ctx context.Context, db orm.Context) error {
	return db.RunTx(ctx, orm.Serializable, func(ctx context.Context) error { // no lock
		_, err := db.Exec(ctx, "UPDATE jobs SET state = 'done' WHERE id = $1", 1)
		return err
	})
}

func j(ctx context.Context, db orm.Context) error {
	return db.RunTx(ctx, orm.Serializable.WithReadOnly(), func(ctx context.Context) error { // read only: not judged
		_, err := db.Exec(ctx, lockQuery, 1)
		return err
	})
}

func k(ctx context.Context, db orm.Context) error {
	// forge:lint-disable-next-line forgeconv-tx-lock-under-serializable: the retry is the point here
	return db.RunTx(ctx, orm.Serializable, func(ctx context.Context) error {
		_, err := db.Exec(ctx, lockQuery, 1)
		return err
	})
}
`

func TestLintTxLockUnderSerializable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "store.go"), []byte(txLockSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, strict := range []bool{false, true} {
		res, err := LintTxLockUnderSerializable(dir, LintOptions{Strict: strict})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, f := range res.Findings {
			want := SeverityWarning
			if strict {
				want = SeverityError
			}
			if f.Severity != want {
				t.Errorf("strict=%v: %s:%d is %s, want %s", strict, f.File, f.Line, f.Severity, want)
			}
			if !strings.Contains(f.Remediation, "orm.ReadCommitted") {
				t.Errorf("remediation names no fix: %q", f.Remediation)
			}
			got = append(got, fmt.Sprintf("%s:%d %s", f.File, f.Line, between(f.Message, "— ", " at ")))
		}
		// k is found here and suppressed by the shared suppression pass
		// `forge lint` runs over every Go finding.
		want := []string{
			"store.go:12 SELECT … FOR UPDATE",
			"store.go:19 SELECT … FOR UPDATE",
			"store.go:33 pg_advisory_xact_lock",
			"store.go:53 LOCK TABLE",
			"store.go:66 .For(\"UPDATE\")",
			"store.go:91 SELECT … FOR UPDATE",
		}
		if !slices.Equal(got, want) {
			t.Errorf("strict=%v findings:\n  %s\nwant:\n  %s", strict, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
		}
		applied := suppress.Apply(txLockSrc, res.Findings)
		if len(applied.Kept) != len(want)-1 || len(applied.Violations) != 0 {
			t.Errorf("strict=%v: the reasoned disable above k must suppress exactly its finding: kept %d, violations %v", strict, len(applied.Kept), applied.Violations)
		}
	}
}

func between(s, from, to string) string {
	_, after, ok := strings.Cut(s, from)
	if !ok {
		return s
	}
	before, _, _ := strings.Cut(after, to)
	return before
}
