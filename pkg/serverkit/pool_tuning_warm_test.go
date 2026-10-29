package serverkit

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// countingDriver dials a fake connection per Open and counts the dials.
type countingDriver struct{ dials atomic.Int64 }

func (d *countingDriver) Open(string) (driver.Conn, error) {
	d.dials.Add(1)
	return fakeConn{}, nil
}

type fakeConn struct{}

func (fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (fakeConn) Close() error                        { return nil }
func (fakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("not supported") }

func init() { sql.Register("serverkit-warm-test", &countingDriver{}) }

func openCounting(t *testing.T) (*sql.DB, *countingDriver) {
	t.Helper()
	db, err := sql.Open("serverkit-warm-test", "")
	if err != nil {
		t.Fatal(err)
	}
	return db, db.Driver().(*countingDriver)
}

// database/sql dials only when a caller finds no idle connection, so that
// caller waits out the dial. warmIdle dials ahead, leaves the connections
// idle, never takes more than fits beside the ones in use, and reports a
// closed pool so KeepIdleWarm stops.
func TestWarmIdle_DialsIdleConnectionsAheadOfUse(t *testing.T) {
	db, d := openCounting(t)
	db.SetMaxOpenConns(6)
	db.SetMaxIdleConns(4)
	before := d.dials.Load()

	if !warmIdle(db, 4) {
		t.Fatal("an open pool reported closed")
	}
	if got := db.Stats().Idle; got != 4 {
		t.Fatalf("idle = %d, want 4", got)
	}
	if got := d.dials.Load() - before; got != 4 {
		t.Fatalf("dials = %d, want 4", got)
	}
	// Already warm: nothing more is dialled.
	warmIdle(db, 4)
	if got := d.dials.Load() - before; got != 4 {
		t.Fatalf("a warm pool dialled again: %d dials", got)
	}

	// Two in use: only what fits beside them is dialled.
	ctx := context.Background()
	a, _ := db.Conn(ctx)
	b, _ := db.Conn(ctx)
	warmIdle(db, 4)
	if got := db.Stats().OpenConnections; got != 6 {
		t.Fatalf("open = %d, want 6 (the cap)", got)
	}
	_ = a.Close()
	_ = b.Close()

	_ = db.Close()
	if warmIdle(db, 4) {
		t.Fatal("a closed pool must stop the warmer")
	}
}

// MinIdleConns is capped at what the pool keeps idle: database/sql closes an
// idle connection past MaxIdleConns (2 when unset) as soon as it returns.
func TestMinIdleFor_CapsAtThePoolsLimits(t *testing.T) {
	for _, tc := range []struct {
		tuning DBPoolTuning
		want   int
	}{
		{DBPoolTuning{}, 0},
		{DBPoolTuning{MinIdleConns: 10}, 2},
		{DBPoolTuning{MinIdleConns: 10, MaxIdleConns: 8}, 8},
		{DBPoolTuning{MinIdleConns: 10, MaxIdleConns: 8, MaxOpenConns: 5}, 5},
		{DBPoolTuning{MinIdleConns: 3, MaxIdleConns: 8}, 3},
	} {
		if got := minIdleFor(tc.tuning); got != tc.want {
			t.Errorf("minIdleFor(%+v) = %d, want %d", tc.tuning, got, tc.want)
		}
	}
}

// OpenDB keeps MinIdleConns dialled from the moment it returns.
func TestOpenDB_KeepsMinIdleConnsWarm(t *testing.T) {
	db, err := OpenDB(context.Background(), "serverkit-warm-test", "",
		DBPoolTuning{MaxOpenConns: 8, MaxIdleConns: 5, MinIdleConns: 3}, ConnectPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	deadline := time.Now().Add(5 * time.Second)
	for db.Stats().Idle < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("idle = %d after 5s, want 3", db.Stats().Idle)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
