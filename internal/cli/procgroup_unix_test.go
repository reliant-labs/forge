//go:build !windows

package cli

import (
	"testing"
)

func TestParseProcessTable(t *testing.T) {
	got := parseProcessTable("    1     0     1\n  110   100   110\nbogus line\n  111   110   110\n")
	want := map[int]procRow{1: {0, 1}, 110: {100, 110}, 111: {110, 110}}
	if len(got) != len(want) {
		t.Fatalf("table = %v, want %v", got, want)
	}
	for pid, row := range want {
		if got[pid] != row {
			t.Errorf("pid %d = %+v, want %+v", pid, got[pid], row)
		}
	}
}

// TestTreeSignalTargets pins what one tree kill reaches. Air's shape: the
// runner (110) leads its own group; it starts its server (120) in a NEW group
// that the server leads, and the server's own child (121) sits in that group.
// A descendant that only BELONGS to a group (121) never makes its group a
// target on its own; a group whose leader is outside the tree is never
// signalled; the lineage is never signalled.
func TestTreeSignalTargets(t *testing.T) {
	table := map[int]procRow{
		100: {1, 100},   // forge (lineage)
		110: {100, 110}, // air
		120: {110, 120}, // server, its own group
		121: {120, 120}, // the server's child, in the server's group
		130: {110, 999}, // joined a group led by a stranger
		999: {1, 999},
	}
	inLineage := func(pid int) bool { return pid == 100 }
	lineageGroup := func(pgid int) bool { return pgid == 100 }

	got := treeSignalTargets(110, table, inLineage, lineageGroup)
	want := []int{-110, 110, 120, -120, 130, 121}
	if !equalInts(got, want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}

	// A root whose group this command belongs to is signalled alone.
	got = treeSignalTargets(110, map[int]procRow{110: {100, 110}}, inLineage, func(pgid int) bool { return pgid == 110 })
	if want := []int{110}; !equalInts(got, want) {
		t.Errorf("root in the lineage's group: targets = %v, want %v", got, want)
	}

	// No table (ps failed): the root and its group, nothing else.
	got = treeSignalTargets(110, nil, inLineage, lineageGroup)
	if want := []int{-110, 110}; !equalInts(got, want) {
		t.Errorf("without a table: targets = %v, want %v", got, want)
	}
}

func TestParseStopTable(t *testing.T) {
	out := "    1     0     0 Ss   Thu Nov  5 09:35:21 2026\n" +
		"  500     1   501 S+   Sun Nov  8 16:20:17 2026\n" +
		"  501   500   501 Z    Sun Nov  8 16:20:18 2026\n" +
		"  502     1   501 S    garbage\n" +
		"not a row\n"
	got := parseStopTable(out, 501)
	if len(got) != 4 {
		t.Fatalf("rows = %v, want 4", got)
	}
	if !got[1].foreign || got[500].foreign {
		t.Errorf("foreign: init = %v (want true), own process = %v (want false)", got[1].foreign, got[500].foreign)
	}
	if !got[501].exited || got[500].exited {
		t.Errorf("exited: zombie = %v (want true), running = %v (want false)", got[501].exited, got[500].exited)
	}
	if got[500].started.IsZero() || !got[502].started.IsZero() {
		t.Errorf("started: parsed = %v, unparseable = %v (want zero)", got[500].started, got[502].started)
	}
	if got[501].ppid != 500 {
		t.Errorf("ppid = %d, want 500", got[501].ppid)
	}
	if root := parseStopTable(out, 0); root[500].foreign || root[1].foreign {
		t.Error("nothing is foreign to root")
	}
}
