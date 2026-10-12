package migratekit

import (
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

// TestDiffAdoptionIsSetMembershipNotAHighWaterMark: goose applies out of
// order, so a database can hold the NEWEST listed version and still lack an
// older one. A comparison of maxima would adopt it; set membership must not.
func TestDiffAdoptionIsSetMembershipNotAHighWaterMark(t *testing.T) {
	declared := []int64{10, 20, 30}
	cases := []struct {
		name                string
		applied             []int64
		missing, unexpected []int64
	}{
		{name: "exactly the history", applied: []int64{30, 10, 20}},
		{name: "newest applied, a middle one skipped", applied: []int64{10, 30}, missing: []int64{20}},
		{name: "behind", applied: []int64{10}, missing: []int64{20, 30}},
		{name: "nothing applied", missing: []int64{10, 20, 30}},
		{name: "a version the baseline does not list", applied: []int64{10, 20, 25, 30}, unexpected: []int64{25}},
		{name: "both", applied: []int64{10, 40}, missing: []int64{20, 30}, unexpected: []int64{40}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			applied := map[int64]bool{}
			for _, v := range tc.applied {
				applied[v] = true
			}
			missing, unexpected := diffAdoption(declared, applied)
			if !reflect.DeepEqual(missing, tc.missing) || !reflect.DeepEqual(unexpected, tc.unexpected) {
				t.Errorf("diff = missing %v, unexpected %v; want missing %v, unexpected %v", missing, unexpected, tc.missing, tc.unexpected)
			}
		})
	}
}

// TestAdoptionErrorIsTheRunbook: the refusal is read by whoever is holding a
// stalled rollout or a desktop app that will not start, so it must say what
// is wrong in each direction and how to fix it — and stay readable when a
// database is behind by an entire history.
func TestAdoptionErrorIsTheRunbook(t *testing.T) {
	behind := make([]int64, 30)
	for i := range behind {
		behind[i] = int64(i + 1)
	}
	err := &AdoptionError{
		Tool: "goose", Table: gooseTable, Baseline: "20260201000000_baseline.up.sql", Declared: 40,
		Missing: behind, Unexpected: []int64{99},
	}
	msg := err.Error()
	for _, want := range []string{
		"20260201000000_baseline.up.sql",
		"NOT applied 30 of the 40 goose migration(s)",
		"run the last release that still migrates with goose",
		"… and 18 more", // 30 missing, 12 printed
		"HAS applied 1 goose migration(s) the baseline does not list: 99",
		"DELETE FROM goose_db_version WHERE version_id IN",
		"NOTHING WAS APPLIED",
		"db/deploy-migrations",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("AdoptionError does not say %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, " 13,") {
		t.Errorf("AdoptionError printed past its cap:\n%s", msg)
	}
}

// TestOpenRefusesAnAdoptionAfterTheFirstMigration: a database another tool
// migrated has run none of this project's migrations, so only the first one
// can already be reflected in it. A declaration further down would record the
// earlier ones as applied on a database that never ran them. Refused at Open,
// before any database is touched, on every database — so it fails the first
// test run.
func TestOpenRefusesAnAdoptionAfterTheFirstMigration(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/20260101000000_first.up.sql":    {Data: []byte("CREATE TABLE a (id INT);")},
		"migrations/20260201000000_baseline.up.sql": {Data: []byte("-- forge:adopts goose 1\nCREATE TABLE b (id INT);")},
	}
	_, err := Open(Options{FS: fsys, Dir: "migrations", DSN: "postgres://unused@127.0.0.1:1/x"})
	if err == nil || !strings.Contains(err.Error(), "only the FIRST migration") || !strings.Contains(err.Error(), "20260201000000_baseline.up.sql") {
		t.Fatalf("Open err = %v; want a refusal naming the misplaced baseline", err)
	}
}
