package testdocker

import (
	"reflect"
	"testing"
	"time"
)

func TestLabelArgs(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	want := []string{"--label", "forge.test=openbao", "--label", "forge.test.created=1700000000"}
	if got := labelArgs("openbao", now); !reflect.DeepEqual(got, want) {
		t.Errorf("labelArgs = %q; want %q", got, want)
	}
}

func TestNetworkCreateArgs(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	got := networkCreateArgs("openbao", "bao-public-1", now, []string{"--subnet", "10.20.30.0/24"})
	want := []string{"network", "create",
		"--label", "forge.test=openbao", "--label", "forge.test.created=1700000000",
		"--subnet", "10.20.30.0/24", "bao-public-1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("networkCreateArgs = %q; want %q", got, want)
	}
}

func TestStaleIDsReapsOnlyTheOwnersOldResources(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	old := "1699990000"   // 10000s before now
	fresh := "1699999000" // 1000s before now
	out := "" +
		"aaa\topenbao\t" + old + "\n" + // ours and stale: reaped
		"bbb\topenbao\t" + fresh + "\n" + // ours but live: kept
		"ccc\tclickhouse\t" + old + "\n" + // another suite's: kept
		"ddd\t\t" + old + "\n" + // unlabelled owner: kept
		"eee\topenbao\t\n" + // no created stamp: kept
		"fff\topenbao\tyesterday\n" + // unreadable stamp: kept
		"ggg\topenbao\n" + // malformed line: kept
		"\n" +
		"hhh\topenbao\t" + old + "\n" // ours and stale: reaped
	got := staleIDs(out, "openbao", now, time.Hour)
	if want := []string{"aaa", "hhh"}; !reflect.DeepEqual(got, want) {
		t.Errorf("staleIDs = %q; want %q", got, want)
	}
	if got := staleIDs(out, "openbao", now, 24*time.Hour); len(got) != 0 {
		t.Errorf("staleIDs with a day's grace = %q; want none", got)
	}
}

func TestOwnerFilter(t *testing.T) {
	if got, want := ownerFilter("openbao"), "label=forge.test=openbao"; got != want {
		t.Errorf("ownerFilter = %q; want %q", got, want)
	}
}
