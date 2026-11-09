//go:build !windows

package cli

import (
	"encoding/binary"
	"strconv"
	"syscall"
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

func TestParseProcStatStamp(t *testing.T) {
	// pid (comm with ") (" in it) state ppid pgrp ... field 22 = starttime.
	stat := "4242 (weird ) (name) S 1 4240 4240 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000 10 18446744073709551615\n"
	st, ok := parseProcStatStamp([]byte(stat))
	if !ok || st.pgid != 4240 || st.start != 987654 {
		t.Errorf("stamp = %+v ok=%v, want pgid 4240 start 987654 ticks", st, ok)
	}
	if _, ok := parseProcStatStamp([]byte("4242 (short) S 1 2")); ok {
		t.Error("a truncated stat parsed")
	}
}

// TestLinuxStartMark pins the mark in clock ticks at the USER_HZ values Linux
// ships (100 on x86 and arm64, 250/300/1000 on other configurations), and the
// property the mark exists for: a process that starts at or after the uptime
// read — its starttime is floor(start·hz) — never verifies as older than the
// snapshot, while one started a second earlier does.
func TestLinuxStartMark(t *testing.T) {
	const uptime = "350735.47 2000000.00\n"
	const readCS = 35073547 // the uptime read, in centiseconds since boot
	for _, hz := range []int64{100, 250, 300, 1000} {
		t.Run(strconv.FormatInt(hz, 10)+"Hz", func(t *testing.T) {
			mark, ok := linuxStartMark([]byte(uptime), hz)
			if want := readCS*hz/100 - 1; !ok || mark != want {
				t.Fatalf("mark = %d ok=%v, want %d", mark, ok, want)
			}
			// starttime of a process started at a time given in microseconds
			// since boot, as the kernel reports it: floor(seconds·hz).
			ticksAt := func(us int64) int64 { return us * hz / 1_000_000 }
			readUS := int64(readCS) * 10_000
			v := stampVerifier{mark: mark, ok: true}
			for _, after := range []int64{0, 1, 3_333, 10_000, 1_000_000} {
				v.read = stampsAt(map[int]procStamp{120: {pgid: 120, start: ticksAt(readUS + after)}})
				if v.samePID(120) || v.leads(120) {
					t.Errorf("a process started %dµs after the mark verified as older than the snapshot", after)
				}
			}
			v.read = stampsAt(map[int]procStamp{120: {pgid: 120, start: ticksAt(readUS - 1_000_000)}})
			if !v.samePID(120) || !v.leads(120) {
				t.Error("a process started a second before the mark did not verify")
			}
		})
	}
	if _, ok := linuxStartMark([]byte("garbage"), 100); ok {
		t.Error("an unparseable uptime produced a mark")
	}
	if mark, ok := linuxStartMark([]byte("12.5 1.0"), 100); !ok || mark != 1250-1 {
		t.Errorf("single-digit centiseconds: mark = %d ok=%v, want 1249", mark, ok)
	}
}

// TestLinuxTickRateUnavailable: with no tick rate there is no mark, and with
// no mark no process group is ever signalled — forge does not guess USER_HZ.
func TestLinuxTickRateUnavailable(t *testing.T) {
	mark, ok := linuxStartMark([]byte("350735.47 2000000.00\n"), 0)
	if ok {
		t.Fatalf("a mark (%d) without a tick rate", mark)
	}
	table := airTree()
	v := stampVerifier{mark: mark, ok: ok, read: stampsAt(map[int]procStamp{
		110: {pgid: 110, start: 1}, 120: {pgid: 120, start: 1}, 121: {pgid: 120, start: 1},
	})}
	var k killRecorder
	if err := signalTreeTargets(treeSignalTargets(110, table, noLineage, noLineage), table, v, noLineage, k.kill, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	for _, target := range k.sent {
		if target < 0 {
			t.Errorf("signalled group %d without a tick rate (sent %v)", -target, k.sent)
		}
	}
}

func TestClockTicksFromAuxv(t *testing.T) {
	cases := []struct {
		name string
		auxv [][2]uintptr
		hz   int64
		ok   bool
	}{
		{"present", [][2]uintptr{{6, 4096}, {atClkTck, 250}, {25, 0xdead}}, 250, true},
		{"absent", [][2]uintptr{{6, 4096}}, 0, false},
		{"zero", [][2]uintptr{{atClkTck, 0}}, 0, false},
	}
	for _, tc := range cases {
		hz, ok := clockTicksFromAuxv(tc.auxv)
		if hz != tc.hz || ok != tc.ok {
			t.Errorf("%s: got %d,%v want %d,%v", tc.name, hz, ok, tc.hz, tc.ok)
		}
	}
}

// TestParseAuxv decodes /proc/self/auxv in both word sizes and byte orders,
// stopping at AT_NULL.
func TestParseAuxv(t *testing.T) {
	encode := func(size int, order binary.ByteOrder, words ...uint64) []byte {
		b := make([]byte, size*len(words))
		for i, w := range words {
			if size == 4 {
				order.PutUint32(b[i*4:], uint32(w))
			} else {
				order.PutUint64(b[i*8:], w)
			}
		}
		return b
	}
	for _, size := range []int{4, 8} {
		for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
			raw := encode(size, order, 6, 4096, atClkTck, 1000, 0, 0, atClkTck, 7)
			got := parseAuxv(raw, size, order)
			if len(got) != 2 || got[1] != [2]uintptr{atClkTck, 1000} {
				t.Errorf("size %d %v: auxv = %v", size, order, got)
			}
			if hz, ok := clockTicksFromAuxv(got); !ok || hz != 1000 {
				t.Errorf("size %d %v: hz = %d ok=%v", size, order, hz, ok)
			}
		}
	}
}
