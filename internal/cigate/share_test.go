package cigate

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// sharingSlot is a fakeSlot that gives its holder a CPU share of 3, as
// *hostslot.Slot does through CPUEnv.
type sharingSlot struct{ fakeSlot }

func (s *sharingSlot) CPUEnv(env []string) []string {
	for _, kv := range env {
		if strings.HasPrefix(kv, "GOMAXPROCS=") {
			return nil
		}
	}
	return []string{"GOMAXPROCS=3"}
}

// A heavy check's command runs on its slot's CPU share, unless the gate's
// environment already sets GOMAXPROCS; a light check holds no slot and gets
// no share.
func TestScheduler_HeavyChecksRunOnTheSlotsShare(t *testing.T) {
	for _, tc := range []struct {
		name      string
		env       []string
		heavyWant string
	}{
		{"share exported", []string{"PATH=/nonexistent"}, "GOMAXPROCS=3"},
		{"caller's GOMAXPROCS kept", []string{"PATH=/nonexistent", "GOMAXPROCS=7"}, "GOMAXPROCS=7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRecorder()
			slots := &fakeSlots{sem: make(chan struct{}, 1)}
			take := func(ctx context.Context) (HeldSlot, error) {
				select {
				case slots.sem <- struct{}{}:
					return &sharingSlot{fakeSlot{s: slots}}, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			heavy := check("heavy", "heavy")
			heavy.Heavy = true
			rep := runChecks(t, []Check{heavy, check("light", "light")}, Options{Exec: r.run, Jobs: 2, Slot: take, Env: tc.env})
			if rep.Failed() {
				t.Fatalf("rows: %+v", rep.Results)
			}
			if got := gomaxprocs(r.env["heavy"]); len(got) != 1 || got[0] != tc.heavyWant {
				t.Errorf("heavy check's GOMAXPROCS = %v, want [%s]", got, tc.heavyWant)
			}
			if got := gomaxprocs(r.env["light"]); slices.Contains(got, "GOMAXPROCS=3") {
				t.Errorf("light check got a slot's share: %v", got)
			}
		})
	}
}

func gomaxprocs(env []string) []string {
	var out []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "GOMAXPROCS=") {
			out = append(out, kv)
		}
	}
	return out
}
