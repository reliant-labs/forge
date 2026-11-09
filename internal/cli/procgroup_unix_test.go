//go:build !windows

package cli

import (
	"syscall"
	"testing"
	"time"
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

// killRecorder is a fake kill(2) that records each target.
type killRecorder struct{ sent []int }

func (k *killRecorder) kill(target int, _ syscall.Signal) error {
	k.sent = append(k.sent, target)
	return nil
}

// stampsAt is a stamp reader over a fixed table: a pid absent from it is gone.
func stampsAt(stamps map[int]procStamp) func(int) (procStamp, bool) {
	return func(pid int) (procStamp, bool) {
		st, ok := stamps[pid]
		return st, ok
	}
}

// airTree is the snapshot of Air's shape, every process started before the
// mark (1000): the runner 110 leads group 110; its server 120 leads group 120,
// with the server's child 121 in it.
func airTree() map[int]procRow {
	return map[int]procRow{
		100: {1, 100},
		110: {100, 110},
		120: {110, 120},
		121: {120, 120},
	}
}

func noLineage(int) bool { return false }

func TestSignalTreeTargets_SignalsGroupsWhoseLeadersReverify(t *testing.T) {
	table := airTree()
	v := stampVerifier{mark: 1000, ok: true, read: stampsAt(map[int]procStamp{
		110: {pgid: 110, start: 10},
		120: {pgid: 120, start: 20},
		121: {pgid: 120, start: 30},
	})}
	var k killRecorder
	targets := treeSignalTargets(110, table, noLineage, noLineage)
	if err := signalTreeTargets(targets, table, v, noLineage, k.kill, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if want := []int{-110, 110, 120, -120, 121}; !equalInts(k.sent, want) {
		t.Errorf("sent = %v, want %v", k.sent, want)
	}
}

// TestSignalTreeTargets_LeaderExitedAndGroupNumberReused is the #1979 review
// risk: between the snapshot and the kill, the server (120) and its child
// exited, its group emptied, and pid 120 now belongs to an unrelated process
// that leads its own group 120. Neither the stranger nor its group may be
// signalled.
func TestSignalTreeTargets_LeaderExitedAndGroupNumberReused(t *testing.T) {
	table := airTree()
	v := stampVerifier{mark: 1000, ok: true, read: stampsAt(map[int]procStamp{
		110: {pgid: 110, start: 10},
		120: {pgid: 120, start: 1500}, // a stranger: same pid and group, started after the mark
	})}
	var k killRecorder
	targets := treeSignalTargets(110, table, noLineage, noLineage)
	if err := signalTreeTargets(targets, table, v, noLineage, k.kill, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	for _, target := range k.sent {
		if target == 120 || target == -120 || target == 121 {
			t.Errorf("signalled %d: pid 120 and group 120 now belong to an unrelated process (sent %v)", target, k.sent)
		}
	}
	if want := []int{-110, 110}; !equalInts(k.sent, want) {
		t.Errorf("sent = %v, want only the still-verified runner and its group %v", k.sent, want)
	}
}

// TestSignalTreeTargets_LeaderGoneMembersRemain: the server (120) exited but
// its child (121), still in group 120, lives on. The group is not signalled —
// its leader cannot vouch for it — but the child is, as itself.
func TestSignalTreeTargets_LeaderGoneMembersRemain(t *testing.T) {
	table := airTree()
	table[130] = procRow{1, 120} // an orphan of the group, outside the tree
	v := stampVerifier{mark: 1000, ok: true, read: stampsAt(map[int]procStamp{
		110: {pgid: 110, start: 10},
		121: {pgid: 120, start: 30},
		130: {pgid: 120, start: 40},
	})}
	var k killRecorder
	targets := treeSignalTargets(110, table, noLineage, noLineage)
	if err := signalTreeTargets(targets, table, v, noLineage, k.kill, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if want := []int{-110, 110, 121, 130}; !equalInts(k.sent, want) {
		t.Errorf("sent = %v, want %v", k.sent, want)
	}
}

// TestSignalTreeTargets_MemberLeftOrReplaced: in the leader-gone fallback a
// member is signalled only while it is the snapshot's process AND still in
// the group.
func TestSignalTreeTargets_MemberLeftOrReplaced(t *testing.T) {
	table := map[int]procRow{120: {1, 120}, 121: {120, 120}, 122: {120, 120}}
	v := stampVerifier{mark: 1000, ok: true, read: stampsAt(map[int]procStamp{
		121: {pgid: 777, start: 30},   // moved to another group
		122: {pgid: 120, start: 1200}, // pid reused after the snapshot
	})}
	var k killRecorder
	if err := signalTreeTargets([]int{-120}, table, v, noLineage, k.kill, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if len(k.sent) != 0 {
		t.Errorf("sent %v, want nothing", k.sent)
	}
}

// TestSignalTreeTargets_Unverifiable: where no stamp can be read, pids are
// signalled as listed (what a pid kill always was) and no group ever is.
func TestSignalTreeTargets_Unverifiable(t *testing.T) {
	table := airTree()
	var k killRecorder
	targets := treeSignalTargets(110, table, noLineage, noLineage)
	if err := signalTreeTargets(targets, table, stampVerifier{}, noLineage, k.kill, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if want := []int{110, 120, 121}; !equalInts(k.sent, want) {
		t.Errorf("sent = %v, want %v", k.sent, want)
	}
}

// TestReadProcStampSelf pins the real reader and mark against this process:
// its group is its own, and it started before a mark read now.
func TestReadProcStampSelf(t *testing.T) {
	mark, ok := startMark()
	if !ok {
		t.Skip("no start-time source on this platform: killProcessTree never signals a group here")
	}
	st, ok := readProcStamp(syscall.Getpid())
	if !ok {
		t.Fatal("cannot read this process's stamp")
	}
	pgid, _ := syscall.Getpgid(0)
	if st.pgid != pgid {
		t.Errorf("pgid = %d, want %d", st.pgid, pgid)
	}
	// On Linux a process younger than linuxMarkSlack cannot yet verify (it is
	// refused, never wrongly accepted); this one only gets older.
	if !pollUntil(2*time.Second, func() bool {
		mark, _ = startMark()
		return st.start < mark
	}) {
		t.Errorf("this process started at %d, not before the mark %d", st.start, mark)
	}
}

func TestParseProcStatStamp(t *testing.T) {
	// pid (comm with ") (" in it) state ppid pgrp ... field 22 = starttime.
	stat := "4242 (weird ) (name) S 1 4240 4240 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000 10 18446744073709551615\n"
	st, ok := parseProcStatStamp([]byte(stat))
	if !ok || st.pgid != 4240 || st.start != 987654*10_000 {
		t.Errorf("stamp = %+v ok=%v, want pgid 4240 start %d", st, ok, 987654*10_000)
	}
	if _, ok := parseProcStatStamp([]byte("4242 (short) S 1 2")); ok {
		t.Error("a truncated stat parsed")
	}
	mark, ok := parseUptimeMark([]byte("350735.47 2000000.00\n"))
	if !ok || mark != 350735470000-linuxMarkSlack {
		t.Errorf("mark = %d ok=%v", mark, ok)
	}
}
