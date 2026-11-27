//go:build linux

package airbuild

import "testing"

func TestParseProcStat(t *testing.T) {
	// The command name is parenthesised and may hold spaces and ')'.
	line := "4242 (we ird) (name)) S 17 4242 4242 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000 10 18446744073709551615"
	got, ok := parseProcStat(line)
	if !ok || got.ppid != 17 || got.start != "987654" {
		t.Fatalf("parseProcStat = %+v, %t; want ppid 17, start 987654", got, ok)
	}
	if _, ok := parseProcStat("4242 (short) S 1"); ok {
		t.Fatal("a truncated stat line parsed")
	}
}
