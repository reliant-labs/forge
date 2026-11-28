package testdocker

import (
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// logTB is a testing.TB whose Fatalf records and ends the goroutine, and
// whose Logf and Cleanup are captured, so helpers that fail or log can be
// observed instead of failing the real test.
type logTB struct {
	testing.TB
	logs     []string
	fatal    string
	cleanups []func()
}

func (l *logTB) Helper()                 {}
func (l *logTB) Logf(f string, a ...any) { l.logs = append(l.logs, fmt.Sprintf(f, a...)) }
func (l *logTB) Cleanup(fn func())       { l.cleanups = append(l.cleanups, fn) }
func (l *logTB) runCleanups() {
	for i := len(l.cleanups) - 1; i >= 0; i-- {
		l.cleanups[i]()
	}
}

// runFatal runs fn in its own goroutine so a Fatalf (modelled as Goexit)
// ends only that goroutine, and reports the Fatalf message.
func runFatal(l *logTB, fn func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	<-done
}

func (l *logTB) Fatalf(f string, a ...any) {
	l.fatal = fmt.Sprintf(f, a...)
	runtime.Goexit()
}

func TestPublish(t *testing.T) {
	got := Publish(8200)
	want := []string{"-p", "127.0.0.1::8200"}
	if !slices.Equal(got, want) {
		t.Errorf("Publish(8200) = %v, want %v", got, want)
	}
}

func TestPublishedPort(t *testing.T) {
	t.Run("reads the port docker picked", func(t *testing.T) {
		f := installFakeDocker(t, map[string]fakeReply{
			"port": {Stdout: "127.0.0.1:49153\n[::1]:49153\n"},
		})
		if got := PublishedPort(t, "bao-1", 8200); got != 49153 {
			t.Errorf("PublishedPort = %d, want 49153", got)
		}
		if c := f.find(t, "port"); !slices.Equal(c, []string{"port", "bao-1", "8200/tcp"}) {
			t.Errorf("docker was called as %v, want port bao-1 8200/tcp", c)
		}
	})

	t.Run("docker failure fails the test with its output", func(t *testing.T) {
		installFakeDocker(t, map[string]fakeReply{
			"port": {Stderr: "Error: No such container: gone\n", Exit: 1},
		})
		l := &logTB{TB: t}
		runFatal(l, func() { PublishedPort(l, "gone", 8200) })
		for _, want := range []string{"docker port gone 8200", "No such container: gone"} {
			if !strings.Contains(l.fatal, want) {
				t.Errorf("fatal %q does not mention %q", l.fatal, want)
			}
		}
	})

	t.Run("an answer with no port fails the test", func(t *testing.T) {
		installFakeDocker(t, map[string]fakeReply{"port": {Stdout: "\n"}})
		l := &logTB{TB: t}
		runFatal(l, func() { PublishedPort(l, "c", 80) })
		if !strings.Contains(l.fatal, "no published port") {
			t.Errorf("fatal = %q, want it to say no port was published", l.fatal)
		}
	})
}

func TestNetwork(t *testing.T) {
	t.Run("creates a labelled network and cleanup removes the owner's containers then the network", func(t *testing.T) {
		f := installFakeDocker(t, map[string]fakeReply{
			"ps": {Stdout: "id1\nid2\n"},
		})
		l := &logTB{TB: t}
		if err := Network(l, "openbao", "bao-net", "--subnet", "10.9.0.0/24"); err != nil {
			t.Fatalf("Network: %v", err)
		}
		create := f.find(t, "network", "create")
		if create == nil {
			t.Fatal("docker network create was never run")
		}
		joined := strings.Join(create, " ")
		for _, want := range []string{"--label forge.test=openbao", "--label forge.test.created=", "--subnet 10.9.0.0/24"} {
			if !strings.Contains(joined, want) {
				t.Errorf("network create argv %q missing %q", joined, want)
			}
		}
		if create[len(create)-1] != "bao-net" {
			t.Errorf("network name must be last, argv = %v", create)
		}

		l.runCleanups()
		ps := f.find(t, "ps", "-aq")
		if ps == nil || !slices.Contains(ps, "network=bao-net") || !slices.Contains(ps, "label=forge.test=openbao") {
			t.Errorf("cleanup must list only the owner's containers on the network, ps argv = %v", ps)
		}
		if rm := f.find(t, "rm", "-fv"); !slices.Equal(rm, []string{"rm", "-fv", "id1", "id2"}) {
			t.Errorf("cleanup container removal = %v", rm)
		}
		if rm := f.find(t, "network", "rm"); !slices.Equal(rm, []string{"network", "rm", "bao-net"}) {
			t.Errorf("cleanup network removal = %v", rm)
		}
		if len(l.logs) != 0 {
			t.Errorf("a clean teardown must log nothing, got %v", l.logs)
		}
	})

	t.Run("no containers means no container removal", func(t *testing.T) {
		f := installFakeDocker(t, nil)
		l := &logTB{TB: t}
		if err := Network(l, "openbao", "n"); err != nil {
			t.Fatal(err)
		}
		l.runCleanups()
		if rm := f.find(t, "rm"); rm != nil {
			t.Errorf("rm ran with no containers: %v", rm)
		}
		if f.find(t, "network", "rm", "n") == nil {
			t.Error("the network itself must still be removed")
		}
	})

	t.Run("a create failure is returned, not fatal, and registers no cleanup", func(t *testing.T) {
		installFakeDocker(t, map[string]fakeReply{
			"network create": {Stderr: "Pool overlaps with other one on this address space\n", Exit: 1},
		})
		l := &logTB{TB: t}
		err := Network(l, "openbao", "n", "--subnet", "10.0.0.0/8")
		if err == nil {
			t.Fatal("want the create error back")
		}
		for _, want := range []string{"docker network create n", "Pool overlaps"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
		if l.fatal != "" || len(l.cleanups) != 0 {
			t.Errorf("fatal = %q, cleanups = %d; want neither", l.fatal, len(l.cleanups))
		}
	})

	t.Run("cleanup failures are logged, never fatal", func(t *testing.T) {
		installFakeDocker(t, map[string]fakeReply{
			"ps":         {Stdout: "id1\n"},
			"rm":         {Stderr: "cannot remove\n", Exit: 1},
			"network rm": {Stderr: "network has active endpoints\n", Exit: 1},
		})
		l := &logTB{TB: t}
		if err := Network(l, "openbao", "n"); err != nil {
			t.Fatal(err)
		}
		l.runCleanups()
		all := strings.Join(l.logs, "\n")
		for _, want := range []string{"removing the containers on network n", "removing network n", "active endpoints"} {
			if !strings.Contains(all, want) {
				t.Errorf("logs %q missing %q", all, want)
			}
		}
		if l.fatal != "" {
			t.Errorf("cleanup failure was fatal: %s", l.fatal)
		}
	})

	t.Run("a failing container listing is logged and the network is still removed", func(t *testing.T) {
		f := installFakeDocker(t, map[string]fakeReply{"ps": {Exit: 1}})
		l := &logTB{TB: t}
		if err := Network(l, "openbao", "n"); err != nil {
			t.Fatal(err)
		}
		l.runCleanups()
		if !strings.Contains(strings.Join(l.logs, "\n"), "listing the containers on network n") {
			t.Errorf("logs = %v, want the listing failure", l.logs)
		}
		if f.find(t, "network", "rm", "n") == nil {
			t.Error("the network must still be removed after a failed listing")
		}
	})

	t.Run("an invalid owner is fatal before docker runs", func(t *testing.T) {
		f := installFakeDocker(t, nil)
		l := &logTB{TB: t}
		runFatal(l, func() { _ = Network(l, "", "n") })
		if !strings.Contains(l.fatal, "owner") {
			t.Errorf("fatal = %q, want an owner complaint", l.fatal)
		}
		if c := f.calls(t); len(c) != 0 {
			t.Errorf("docker ran for an invalid owner: %v", c)
		}
	})
}

func TestReapStale(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano)
	fresh := time.Now().UTC().Format(time.RFC3339Nano)
	row := func(id, owner, created string) string {
		return strings.Join([]string{id, owner, created, "", ""}, "\t") + "\n"
	}

	t.Run("removes only the owner's stale containers and networks, containers first", func(t *testing.T) {
		f := installFakeDocker(t, map[string]fakeReply{
			"ps":         {Stdout: row("c-old", "openbao", old) + row("c-new", "openbao", fresh) + row("c-other", "other", old)},
			"network ls": {Stdout: row("n-old", "openbao", old) + row("n-unlabelled", "openbao", "")},
		})
		l := &logTB{TB: t}
		ReapStale(l, "openbao", time.Minute)

		if rm := f.find(t, "rm", "-fv"); !slices.Equal(rm, []string{"rm", "-fv", "c-old"}) {
			t.Errorf("container reap = %v, want only c-old", rm)
		}
		if rm := f.find(t, "network", "rm"); !slices.Equal(rm, []string{"network", "rm", "n-old"}) {
			t.Errorf("network reap = %v, want only n-old", rm)
		}
		calls := f.calls(t)
		idx := func(prefix ...string) int {
			for i, c := range calls {
				if len(c) >= len(prefix) && slices.Equal(c[:len(prefix)], prefix) {
					return i
				}
			}
			return -1
		}
		if idx("rm", "-fv") > idx("network", "rm") {
			t.Errorf("networks were removed before containers: %v", calls)
		}
		ls := f.find(t, "ps", "-a")
		if !slices.Contains(ls, "label=forge.test=openbao") {
			t.Errorf("listing must be filtered to the owner, argv = %v", ls)
		}
		if got := strings.Join(l.logs, "\n"); !strings.Contains(got, "reaped 1 stale openbao resource(s)") {
			t.Errorf("logs = %q, want a reap summary", got)
		}
	})

	t.Run("nothing stale runs no removal", func(t *testing.T) {
		f := installFakeDocker(t, map[string]fakeReply{
			"ps": {Stdout: row("c-new", "openbao", fresh)},
		})
		ReapStale(&logTB{TB: t}, "openbao", time.Minute)
		if f.find(t, "rm") != nil || f.find(t, "network", "rm") != nil {
			t.Errorf("a removal ran though nothing was stale: %v", f.calls(t))
		}
	})

	t.Run("a failing listing is logged and the other kind is still reaped", func(t *testing.T) {
		f := installFakeDocker(t, map[string]fakeReply{
			"ps":         {Exit: 1},
			"network ls": {Stdout: row("n-old", "openbao", old)},
		})
		l := &logTB{TB: t}
		ReapStale(l, "openbao", time.Minute)
		if !strings.Contains(strings.Join(l.logs, "\n"), "docker ps -a") {
			t.Errorf("logs = %v, want the ps failure", l.logs)
		}
		if f.find(t, "network", "rm", "n-old") == nil {
			t.Error("networks must still be reaped after a container listing failure")
		}
	})

	t.Run("a failing removal is logged, not fatal", func(t *testing.T) {
		installFakeDocker(t, map[string]fakeReply{
			"ps": {Stdout: row("c-old", "openbao", old)},
			"rm": {Stderr: "device busy\n", Exit: 1},
		})
		l := &logTB{TB: t}
		ReapStale(l, "openbao", time.Minute)
		got := strings.Join(l.logs, "\n")
		if !strings.Contains(got, "reaping openbao's stale resources") || !strings.Contains(got, "device busy") {
			t.Errorf("logs = %q, want the removal failure with docker's output", got)
		}
		if strings.Contains(got, "reaped 1") {
			t.Errorf("a failed removal must not be reported as a reap: %q", got)
		}
	})

	t.Run("an invalid owner is fatal before docker runs", func(t *testing.T) {
		f := installFakeDocker(t, nil)
		l := &logTB{TB: t}
		runFatal(l, func() { ReapStale(l, "a=b", time.Minute) })
		if l.fatal == "" {
			t.Error("want a fatal for an owner containing '='")
		}
		if c := f.calls(t); len(c) != 0 {
			t.Errorf("docker ran for an invalid owner: %v", c)
		}
	})
}

func TestMustOwner(t *testing.T) {
	for _, tc := range []struct {
		owner string
		fatal bool
	}{
		{"openbao", false},
		{"clickhouse-smoke", false},
		{"", true},
		{"   ", true},
		{"a=b", true},
		{"a\tb", true},
		{"a\nb", true},
	} {
		t.Run(fmt.Sprintf("%q", tc.owner), func(t *testing.T) {
			l := &logTB{TB: t}
			runFatal(l, func() { mustOwner(l, tc.owner) })
			if (l.fatal != "") != tc.fatal {
				t.Errorf("mustOwner(%q) fatal = %q, want fatal=%v", tc.owner, l.fatal, tc.fatal)
			}
		})
	}
}

func TestSelf(t *testing.T) {
	o := self()
	if o.pid <= 0 {
		t.Errorf("self().pid = %d, want a real pid", o.pid)
	}
}

func TestLabels(t *testing.T) {
	got := Labels("openbao")
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--label forge.test=openbao") || !strings.Contains(joined, "--label forge.test.created=") {
		t.Errorf("Labels = %v, want owner and created labels", got)
	}
	if !strings.Contains(joined, "forge.test.pid=") {
		t.Errorf("Labels = %v, want this process's pid label", got)
	}
}

func TestTestTimeout(t *testing.T) {
	// Under `go test` the binary always has a -timeout (default 10m), so the
	// helper must report it; the flag default is positive.
	if d := testTimeout(); d <= 0 {
		t.Errorf("testTimeout() = %v under go test, want the binary's positive -timeout", d)
	}
}
