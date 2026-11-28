package testcapture

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/testkit"
)

// testdata/go-test.jsonl is a real `go test -json ./...` stream (Go 1.27) of
// two packages: one with a passing, a skipped, a failing (with a failing and
// a passing subtest) and a failing parallel test, one with a passing test
// that prints to stdout. Times are zeroed.
func readFixture(t *testing.T) []Event {
	t.Helper()
	f, err := os.Open("testdata/go-test.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var evs []Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		evs = append(evs, e)
	}
	return evs
}

func renderAll(evs []Event, verbose bool) string {
	var b strings.Builder
	r := NewRenderer(&b, verbose)
	for _, e := range evs {
		r.Event(e)
	}
	r.Close()
	return b.String()
}

// What `go test ./...` printed for the same packages, package by package
// (its last line, the overall FAIL, is cmd/go's own and is in no stream),
// then the one line forge adds: the count of skipped tests.
func TestRendererPrintsWhatGoTestPrints(t *testing.T) {
	t.Parallel()
	want := "ok  \texample.com/x/b\t(cached)\n" +
		"--- FAIL: TestFail (0.00s)\n" +
		"    --- FAIL: TestFail/sub (0.00s)\n" +
		"        a_test.go:5: boom\n" +
		"    a_test.go:5: parent log\n" +
		"--- FAIL: TestPar (0.00s)\n" +
		"    a_test.go:6: par\n" +
		"FAIL\n" +
		"FAIL\texample.com/x/a\t0.00s\n" +
		"skipped: 1 test. Add -v to see which and why.\n"
	if got := renderAll(readFixture(t), false); got != want {
		t.Fatalf("rendered:\n%s\nwant:\n%s", got, want)
	}
}

func TestRendererVerbosePrintsEveryOutput(t *testing.T) {
	t.Parallel()
	evs := readFixture(t)
	var want strings.Builder
	for _, e := range evs {
		want.WriteString(e.Output)
	}
	want.WriteString("skipped: 1 test.\n")
	if got := renderAll(evs, true); got != want.String() {
		t.Fatalf("verbose:\n%s\nwant:\n%s", got, want.String())
	}
}

// A binary that dies mid-test (a panic, a timeout) leaves tests with no
// result: their output is what explains it, so it is printed.
func TestRendererPrintsTestsACrashLeftRunning(t *testing.T) {
	t.Parallel()
	pkg := "example.com/x/c"
	evs := []Event{
		{Action: "run", Package: pkg, Test: "TestHang"},
		{Action: "output", Package: pkg, Test: "TestHang", Output: "=== RUN   TestHang\n"},
		{Action: "output", Package: pkg, Test: "TestHang", Output: "    c_test.go:9: waiting\n"},
		{Action: "output", Package: pkg, Output: "panic: test timed out after 1s\n"},
		{Action: "fail", Package: pkg},
		{Action: "run", Package: pkg, Test: "TestCut"},
		{Action: "output", Package: pkg, Test: "TestCut", Output: "    c_test.go:12: the stream ended here\n"},
	}
	want := "panic: test timed out after 1s\n" +
		"    c_test.go:9: waiting\n" +
		"    c_test.go:12: the stream ended here\n"
	if got := renderAll(evs, false); got != want {
		t.Fatalf("rendered:\n%q\nwant:\n%q", got, want)
	}
}

func TestRendererBoundsAPassingTestsBuffer(t *testing.T) {
	t.Parallel()
	pkg := "example.com/x/d"
	line := strings.Repeat("x", 1023) + "\n"
	evs := []Event{{Action: "run", Package: pkg, Test: "TestBig"}}
	for range 2*maxTestBuffer/len(line) + 1 {
		evs = append(evs, Event{Action: "output", Package: pkg, Test: "TestBig", Output: line})
	}
	evs = append(evs, Event{Action: "output", Package: pkg, Test: "TestBig", Output: "--- FAIL: TestBig (0.00s)\n"},
		Event{Action: "fail", Package: pkg, Test: "TestBig"})
	got := renderAll(evs, false)
	if !strings.HasPrefix(got, "--- FAIL: TestBig (0.00s)\n") {
		t.Fatalf("the result line is not first: %.80q", got)
	}
	if len(got) > maxTestBuffer+1024 || !strings.Contains(got, "more bytes of output not shown here") {
		t.Fatalf("rendered %d bytes without saying what was left out", len(got))
	}
}

// slowSkipEvents is the -json stream of a test that testkit.Slow skipped
// under -short: its message is test output, then the skip.
func slowSkipEvents(pkg, test string) []Event {
	return []Event{
		{Action: "run", Package: pkg, Test: test},
		{Action: "output", Package: pkg, Test: test, Output: "=== RUN   " + test + "\n"},
		{Action: "output", Package: pkg, Test: test, Output: "    a_test.go:9: slow test, skipped under -short: boots postgres; runs in the full tier (go test without -short, task test)\n"},
		{Action: "output", Package: pkg, Test: test, Output: "--- SKIP: " + test + " (0.00s)\n"},
		{Action: "skip", Package: pkg, Test: test},
	}
}

// A -short run whose slow tests all skipped prints the same "ok" as one
// where they passed; the closing line is what tells them apart.
func TestRendererCountsSlowSkips(t *testing.T) {
	t.Parallel()
	pkg := "example.com/x/s"
	var evs []Event
	evs = append(evs, slowSkipEvents(pkg, "TestSlowA")...)
	evs = append(evs, slowSkipEvents(pkg, "TestSlowB/sub")...)
	evs = append(evs,
		Event{Action: "run", Package: pkg, Test: "TestOther"},
		Event{Action: "output", Package: pkg, Test: "TestOther", Output: "    a_test.go:20: needs docker\n"},
		Event{Action: "skip", Package: pkg, Test: "TestOther"},
		// A passing test that merely mentions the phrase is not a skip.
		Event{Action: "run", Package: pkg, Test: "TestPass"},
		Event{Action: "output", Package: pkg, Test: "TestPass", Output: "    a_test.go:30: slow test, skipped under -short is what Slow says\n"},
		Event{Action: "pass", Package: pkg, Test: "TestPass"},
		Event{Action: "output", Package: pkg, Output: "ok  \t" + pkg + "\t0.01s\n"},
		Event{Action: "pass", Package: pkg},
	)
	want := "ok  \t" + pkg + "\t0.01s\n" +
		"skipped: 2 tests marked testkit.Slow, 1 other. Slow tests run in the full tier (go test without -short, task test). Add -v to see which and why.\n"
	if got := renderAll(evs, false); got != want {
		t.Fatalf("rendered:\n%q\nwant:\n%q", got, want)
	}

	one := renderAll(append(slowSkipEvents(pkg, "TestSlow"), Event{Action: "pass", Package: pkg}), true)
	if !strings.HasSuffix(one, "skipped: 1 test marked testkit.Slow. Slow tests run in the full tier (go test without -short, task test).\n") {
		t.Fatalf("verbose rendering of one slow skip ends:\n%q", one)
	}
}

// A stream in which nothing skipped renders exactly what `go test` prints.
func TestRendererAddsNothingWhenNothingSkipped(t *testing.T) {
	t.Parallel()
	pkg := "example.com/x/p"
	evs := []Event{
		{Action: "run", Package: pkg, Test: "TestA"},
		{Action: "pass", Package: pkg, Test: "TestA"},
		{Action: "output", Package: pkg, Output: "ok  \t" + pkg + "\t(cached)\n"},
		{Action: "pass", Package: pkg},
	}
	if got, want := renderAll(evs, false), "ok  \t"+pkg+"\t(cached)\n"; got != want {
		t.Fatalf("rendered %q, want %q", got, want)
	}
}

// slowSkipHelperEnv makes TestSlowSkipHelper call testkit.Slow.
const slowSkipHelperEnv = "FORGE_TESTCAPTURE_SLOW_HELPER"

func TestSlowSkipHelper(t *testing.T) {
	if os.Getenv(slowSkipHelperEnv) == "" {
		return
	}
	testkit.Slow(t, "the marker test's helper")
}

// TestSlowSkipMarkerMatchesTestkit holds SlowSkipMarker to the message the
// real testkit.Slow skips with: it re-runs this test binary under -short
// (no build, no go command) and reads the helper's skip line.
func TestSlowSkipMarkerMatchesTestkit(t *testing.T) {
	t.Parallel()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSlowSkipHelper$", "-test.short", "-test.v")
	cmd.Env = append(os.Environ(), slowSkipHelperEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "--- SKIP: TestSlowSkipHelper") || !strings.Contains(string(out), SlowSkipMarker) {
		t.Fatalf("testkit.Slow's skip output does not carry %q, so the renderer would not count it:\n%s", SlowSkipMarker, out)
	}
}
