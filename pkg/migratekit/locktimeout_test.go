package migratekit

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/golang-migrate/migrate/v4/database"

	"github.com/reliant-labs/forge/pkg/migratekit/compat"
)

func TestParseLimits(t *testing.T) {
	l, err := ParseLimits("", "", 0)
	if err != nil || l != (Limits{}) {
		t.Fatalf("empty flags: %+v, %v, want the zero Limits", l, err)
	}
	l, err = ParseLimits("30s", "2h", 3)
	if err != nil || l.LockTimeout != 30*time.Second || l.StatementTimeout != 2*time.Hour || l.LockAttempts != 3 {
		t.Fatalf("got %+v, %v", l, err)
	}
	l, err = ParseLimits("off", "0", 1)
	if err != nil || l.LockTimeout != Off || l.StatementTimeout != Off {
		t.Fatalf("off: %+v, %v", l, err)
	}
	for _, c := range []struct {
		lock, stm string
		attempts  int
		want      string
	}{
		{"soon", "", 0, "lock timeout"},
		{"", "10", 0, "statement timeout"},
		{"", "", -1, "negative"},
	} {
		if _, err := ParseLimits(c.lock, c.stm, c.attempts); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ParseLimits(%q, %q, %d) = %v, want an error containing %q", c.lock, c.stm, c.attempts, err, c.want)
		}
	}
}

func TestLimitsDefaultsAndFileOverrides(t *testing.T) {
	r := Limits{}.resolved()
	if r.LockTimeout != DefaultLockTimeout || r.StatementTimeout != DefaultStatementTimeout ||
		r.LockAttempts != DefaultLockAttempts || r.RetryBackoff != DefaultRetryBackoff {
		t.Fatalf("zero Limits resolved to %+v, want the defaults", r)
	}
	if got := (Limits{LockTimeout: Off}).resolved().LockTimeout; got != Off {
		t.Errorf("Off resolved to %v", got)
	}

	sec := func(n int) *time.Duration { d := time.Duration(n) * time.Second; return &d }
	off := new(time.Duration)
	for _, c := range []struct {
		name     string
		file     compat.Timeouts
		lock, st time.Duration
	}{
		{"no directive keeps the run's", compat.Timeouts{}, 5 * time.Second, 30 * time.Minute},
		{"lock directive", compat.Timeouts{Lock: sec(60)}, time.Minute, 30 * time.Minute},
		{"statement directive", compat.Timeouts{Statement: sec(7200)}, 5 * time.Second, 2 * time.Hour},
		{"off disables", compat.Timeouts{Lock: off, Statement: off}, Off, Off},
	} {
		lock, st := r.forFile(c.file)
		if lock != c.lock || st != c.st {
			t.Errorf("%s: lock=%v statement=%v, want %v and %v", c.name, lock, st, c.lock, c.st)
		}
	}
}

func TestTimeoutSetting(t *testing.T) {
	for d, want := range map[time.Duration]string{Off: "0", 0: "0", 5 * time.Second: "5000ms", 1500 * time.Microsecond: "1ms", 30 * time.Minute: "1800000ms"} {
		if got := timeoutSetting(d); got != want {
			t.Errorf("timeoutSetting(%v) = %q, want %q", d, got, want)
		}
	}
}

// golang-migrate's database.Error has no Unwrap, so a lock timeout is found
// through its OrigErr; only SQLSTATE 55P03 counts.
func TestIsLockTimeout(t *testing.T) {
	lock := &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"}
	for name, c := range map[string]struct {
		err  error
		want bool
	}{
		"bare":                   {lock, true},
		"wrapped by migrate":     {database.Error{OrigErr: lock, Err: "migration failed"}, true},
		"pointer wrapped":        {&database.Error{OrigErr: lock}, true},
		"joined":                 {errors.Join(errors.New("x"), database.Error{OrigErr: lock}), true},
		"statement timeout":      {database.Error{OrigErr: &pgconn.PgError{Code: "57014"}}, false},
		"deadlock":               {&pgconn.PgError{Code: "40P01"}, false},
		"not postgres":           {errors.New("lock timeout"), false},
		"database error no orig": {database.Error{Err: "x"}, false},
		"nil":                    {nil, false},
	} {
		if got := isLockTimeout(c.err); got != c.want {
			t.Errorf("%s: isLockTimeout = %v, want %v", name, got, c.want)
		}
	}
}

func TestLockTimeoutErrorSaysWhatToDo(t *testing.T) {
	err := &LockTimeoutError{Version: 7, Name: "7_x.up.sql", Attempts: 5, Timeout: 5 * time.Second,
		Leftover: []string{"public.ix"}, Err: &pgconn.PgError{Code: "55P03", Message: "lock timeout"}}
	msg := err.Error()
	for _, want := range []string{"7_x.up.sql", "5 attempt", "5s", "schema is unchanged", "public.ix", compat.LockTimeoutDirective, "pg_stat_activity"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "55P03" {
		t.Error("the postgres error is not reachable through Unwrap")
	}
}

func TestNamesIdent(t *testing.T) {
	body := []byte("CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS \"Items_V\" ON items (v);")
	for name, want := range map[string]bool{"Items_V": true, "items_v": true, "items": true, "Items_V2": false, "tems_v": false} {
		if got := namesIdent(body, name); got != want {
			t.Errorf("namesIdent(%q) = %v, want %v", name, got, want)
		}
	}
	if !commitRe.Match([]byte("UPDATE t SET a=1;\nCOMMIT;\n")) || commitRe.Match([]byte("SELECT 'commit;'")) {
		t.Error("commitRe misreads an explicit COMMIT")
	}
}
