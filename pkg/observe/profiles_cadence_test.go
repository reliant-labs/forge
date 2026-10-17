package observe

import (
	"bytes"
	"context"
	"fmt"
	"runtime/pprof"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pprofile"
)

func TestHeapCadenceOptInAndDefault(t *testing.T) {
	start := time.Date(2026, 10, 15, 17, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		interval time.Duration
		want     int
	}{{0, 60}, {-1, 60}, {15 * time.Minute, 4}, {5 * time.Minute, 12}} {
		p := &profiler{heapInterval: tc.interval}
		count := 0
		for minute := 0; minute < 60; minute++ {
			now := start.Add(time.Duration(minute) * time.Minute)
			if p.heapDue(now) {
				count++
			}
			if tc.interval > 0 && p.heapDue(now) {
				t.Fatal("retry recaptured heap")
			}
		}
		if count != tc.want {
			t.Fatalf("interval %s: %d snapshots/hour want %d", tc.interval, count, tc.want)
		}
	}
	p := &profiler{heapInterval: 15 * time.Minute}
	if !p.heapDue(start) || !p.heapDue(start.Add(time.Hour)) || p.heapDue(start.Add(time.Hour)) {
		t.Fatal("outage caused catch-up burst")
	}
}

func TestCyclesDoNotRecaptureHeapAfterFailedExportWhenOptedIn(t *testing.T) {
	var cpu bytes.Buffer
	if err := pprof.StartCPUProfile(&cpu); err != nil {
		t.Skip(err)
	}
	defer pprof.StopCPUProfile()
	for _, tc := range []struct {
		interval time.Duration
		heaps    int
	}{{0, 2}, {15 * time.Minute, 1}} {
		p := testProfiler(func(context.Context) error { return nil }, nil)
		p.heapInterval = tc.interval
		heaps, goroutines := 0, 0
		p.send = func(_ context.Context, pd pprofile.Profiles) error {
			profs := pd.ResourceProfiles().At(0).ScopeProfiles().At(0).Profiles()
			for i := 0; i < profs.Len(); i++ {
				switch pd.Dictionary().StringTable().At(int(profs.At(i).SampleType().TypeStrindex())) {
				case "alloc_space":
					heaps++
				case "goroutine":
					goroutines++
				}
			}
			return fmt.Errorf("ambiguous collector failure")
		}
		for i := 0; i < 2; i++ {
			if err := p.safeCycle(context.Background()); err == nil {
				t.Fatal("expected export failure")
			}
		}
		if heaps != tc.heaps || goroutines != 2 {
			t.Fatalf("interval %s sent %d heaps/%d goroutines, want %d/2", tc.interval, heaps, goroutines, tc.heaps)
		}
	}
}
