package cli

import (
	"strings"
	"testing"
	"time"
)

func runMarker(proj, env, svc, run string) []string {
	return append(markerProj(proj, env, svc), forgeUpRunVar+"="+run)
}

// TestRunMarkedRoots pins the shutdown sweep's selection: a process this run
// started is found wherever it now sits in the process table, and nothing
// another run, project or env started — nor this command's own lineage — ever
// is.
//
//	1    init
//	├─ 100 forge env up         (this command)
//	│  └─ 110 air               (run R, api)
//	│     └─ 111 server         (run R, api)    ← inside 110's tree: not a root
//	├─ 200 server               (run R, api)    ← detached: reparented to init
//	│  └─ 201 worker child      (run R, api)
//	├─ 300 server               (run OTHER, api) ← another run of the same stack
//	├─ 400 server               (run R, other project)
//	└─ 500 server               (run R, other env)
func TestRunMarkedRoots(t *testing.T) {
	f := fakeFacts{
		env: map[int][]string{
			110: runMarker(testProj, "dev", "api", "R"),
			111: runMarker(testProj, "dev", "api", "R"),
			200: runMarker(testProj, "dev", "api", "R"),
			201: runMarker(testProj, "dev", "api", "R"),
			300: runMarker(testProj, "dev", "api", "OTHER"),
			400: runMarker("projB", "dev", "api", "R"),
			500: runMarker(testProj, "staging", "api", "R"),
		},
		ppid: map[int]int{100: 1, 110: 100, 111: 110, 200: 1, 201: 200, 300: 1, 400: 1, 500: 1},
	}
	got := runMarkedRoots(testProj, "dev", "R", allPIDs(f), f, lineageOf(f, 100))
	if want := []int{110, 200}; !equalInts(got, want) {
		t.Fatalf("roots = %v, want %v: the runner and the process that left its tree", got, want)
	}

	// The command's own lineage is never selected, even wearing the marker
	// (a forge run from a shell inside its own stack).
	f.env[100] = runMarker(testProj, "dev", "api", "R")
	got = runMarkedRoots(testProj, "dev", "R", allPIDs(f), f, lineageOf(f, 100))
	if want := []int{110, 200}; !equalInts(got, want) {
		t.Errorf("with a marked lineage: roots = %v, want %v", got, want)
	}

	if got := runMarkedRoots(testProj, "dev", "", allPIDs(f), f, lineageOf(f, 100)); len(got) != 0 {
		t.Errorf("empty run id selected %v, want nothing", got)
	}
	if got := runMarkedRoots("", "dev", "R", allPIDs(f), f, lineageOf(f, 100)); len(got) != 0 {
		t.Errorf("empty project id selected %v, want nothing", got)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestParseAirStopDrain(t *testing.T) {
	cases := []struct {
		name string
		toml string
		want time.Duration
	}{
		{"duration string", "[build]\nsend_interrupt = true\nkill_delay = \"5s\"\n", 5 * time.Second},
		{"bare integer is milliseconds, as air reads it", "[build]\nsend_interrupt = true\nkill_delay = 1500\n", 1500 * time.Millisecond},
		{"no interrupt: air kills at once", "[build]\nsend_interrupt = false\nkill_delay = \"20s\"\n", 0},
		{"no kill_delay", "[build]\nsend_interrupt = true\n", 0},
		{"unparseable", "[build\n", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseAirStopDrain([]byte(tc.toml)); got != tc.want {
				t.Errorf("parseAirStopDrain = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestRequireStopGrace pins how a runner's declared drain sets the shutdown
// grace: never below the default, the longest drain plus a margin, capped,
// and a drain past the cap reported where the user will see it.
func TestRequireStopGrace(t *testing.T) {
	p := newProcRegistry(testProj, t.TempDir(), "dev")
	out := captureStdout(t, func() {
		p.requireStopGrace("api", 5*time.Second)
	})
	if p.stopGrace != 0 || out != "" {
		t.Errorf("a drain inside the default grace changed it to %s (output %q)", p.stopGrace, out)
	}

	p.requireStopGrace("api", 12*time.Second)
	p.requireStopGrace("worker", 8*time.Second)
	if want := 12*time.Second + stopGraceMargin; p.stopGrace != want || !strings.Contains(p.graceFor, "api") {
		t.Errorf("grace = %s for %q, want %s for api", p.stopGrace, p.graceFor, want)
	}

	out = captureStdout(t, func() {
		p.requireStopGrace("admin", time.Minute)
	})
	if p.stopGrace != maxStopGrace {
		t.Errorf("grace = %s, want the cap %s", p.stopGrace, maxStopGrace)
	}
	if !strings.Contains(out, "admin") || !strings.Contains(out, "SIGKILL") {
		t.Errorf("a drain past the cap was not reported: %q", out)
	}
}
