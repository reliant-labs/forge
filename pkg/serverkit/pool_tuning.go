package serverkit

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ApplyDBPoolTuning applies the four sql.DB pool knobs from t to db.
// A nil db or zero-value field is a no-op for the corresponding
// setting — Go's default is left in place.
//
// The function is exported so projects that dial their own connection
// outside the AutoMigrate hook (e.g. for an out-of-band health check)
// can apply the same tuning without re-implementing the four-line
// loop.
func ApplyDBPoolTuning(db *sql.DB, t DBPoolTuning) {
	if db == nil {
		return
	}
	if t.MaxOpenConns > 0 {
		db.SetMaxOpenConns(t.MaxOpenConns)
	}
	if t.MaxIdleConns > 0 {
		db.SetMaxIdleConns(t.MaxIdleConns)
	}
	if t.ConnMaxIdleTime > 0 {
		db.SetConnMaxIdleTime(t.ConnMaxIdleTime)
	}
	if t.ConnMaxLifetime > 0 {
		db.SetConnMaxLifetime(t.ConnMaxLifetime)
	}
}

// idleWarmInterval is how often KeepIdleWarm tops a pool up.
var idleWarmInterval = 10 * time.Second

// minIdleFor is t.MinIdleConns capped at what the pool keeps: database/sql
// closes an idle connection past MaxIdleConns (2 when unset) as soon as it
// is returned, so warming more would dial connections only to drop them.
func minIdleFor(t DBPoolTuning) int {
	n := t.MinIdleConns
	maxIdle := t.MaxIdleConns
	if maxIdle <= 0 {
		maxIdle = 2
	}
	n = min(n, maxIdle)
	if t.MaxOpenConns > 0 {
		n = min(n, t.MaxOpenConns)
	}
	return max(n, 0)
}

// KeepIdleWarm holds db at n dialled, idle connections until db is closed.
// OpenDB runs it for a DBPoolTuning.MinIdleConns; a project that opens its
// own pool calls it in a goroutine (n at most the pool's MaxIdleConns).
//
// database/sql has no minimum-idle setting: it dials a connection when a
// caller finds none idle, and that caller waits for the dial (TCP, TLS and
// auth, through a pooler) — measured at 85-336ms per connection on a
// request that fanned out ten queries at once. Connections leave the idle
// set without anyone asking for fewer: ConnMaxLifetime retires them, a burst
// past MaxIdleConns closes the excess, and a fresh process starts with none.
// So this tops the idle set back up off the request path, by checking
// connections out — the pool hands back idle ones first and dials the rest —
// and returning them, which leaves them idle. It never takes a connection a
// request could need: it asks only for what fits beside those in use.
func KeepIdleWarm(db *sql.DB, n int) {
	if db == nil || n <= 0 {
		return
	}
	ticker := time.NewTicker(idleWarmInterval)
	defer ticker.Stop()
	for warmIdle(db, n) {
		<-ticker.C
	}
}

// warmIdle tops db up to n idle connections, best-effort. It reports false
// once db is closed.
func warmIdle(db *sql.DB, n int) bool {
	st := db.Stats()
	if st.Idle >= n {
		return true
	}
	want := n
	if st.MaxOpenConnections > 0 && st.InUse+want > st.MaxOpenConnections {
		want = st.MaxOpenConnections - st.InUse
	}
	if want <= st.Idle {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conns := make([]*sql.Conn, 0, want)
	defer func() {
		for _, c := range conns {
			_ = c.Close() // back to the pool, idle
		}
	}()
	for range want {
		c, err := db.Conn(ctx)
		if err != nil {
			// database/sql's closed-pool error is unexported; its text is
			// the only way to tell it from a dial that failed this time.
			return !errors.Is(err, sql.ErrConnDone) && !strings.Contains(err.Error(), "database is closed")
		}
		conns = append(conns, c)
	}
	return true
}
