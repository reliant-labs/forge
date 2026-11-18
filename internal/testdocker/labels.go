package testdocker

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The labels a smoke test puts on every docker network and container it
// creates. OwnerLabel names the suite that made it ("openbao"), CreatedLabel
// when (unix seconds).
//
// WHY. A test removes what it made in t.Cleanup, which never runs when the test
// binary is killed (a -timeout panic, ^C, a process-group kill), and `docker
// network rm` fails while a container is still attached. The leftovers piled
// up on shared machines as unlabelled `bao-smoke-*` networks that nothing could
// tell apart from another agent's (forge#2283). With these labels ReapStale
// removes exactly a suite's own leftovers and nothing else.
const (
	OwnerLabel   = "forge.test"
	CreatedLabel = "forge.test.created"
)

// Labels is the `docker run` / `docker network create` flags that mark a
// resource as owner's, created now.
func Labels(owner string) []string {
	return labelArgs(owner, time.Now())
}

func labelArgs(owner string, now time.Time) []string {
	return []string{
		"--label", OwnerLabel + "=" + owner,
		"--label", CreatedLabel + "=" + strconv.FormatInt(now.Unix(), 10),
	}
}

// Network creates the docker network name, labelled as owner's (Labels), with
// extraArgs (such as "--subnet", cidr) passed to `docker network create`. Its
// cleanup removes the containers on it that carry owner's label, then the
// network; a cleanup failure is logged, not failed, because ReapStale gets
// whatever is left on a later run.
//
// It returns the create error rather than failing, so the caller decides
// whether a network docker refused (a subnet already in use) is a failure or a
// skip.
func Network(t testing.TB, owner, name string, extraArgs ...string) error {
	t.Helper()
	mustOwner(t, owner)
	if out, err := exec.Command("docker", networkCreateArgs(owner, name, time.Now(), extraArgs)...).CombinedOutput(); err != nil {
		return fmt.Errorf("docker network create %s: %w\n%s", name, err, out)
	}
	t.Cleanup(func() {
		out, err := exec.Command("docker", "ps", "-aq", "--filter", "network="+name, "--filter", ownerFilter(owner)).Output()
		if err != nil {
			t.Logf("testdocker: listing the containers on network %s: %v", name, err)
		}
		if ids := strings.Fields(string(out)); len(ids) > 0 {
			if out, err := exec.Command("docker", append([]string{"rm", "-fv"}, ids...)...).CombinedOutput(); err != nil {
				t.Logf("testdocker: removing the containers on network %s: %v\n%s", name, err, out)
			}
		}
		if out, err := exec.Command("docker", "network", "rm", name).CombinedOutput(); err != nil {
			t.Logf("testdocker: removing network %s (ReapStale removes it on a later run): %v\n%s", name, err, out)
		}
	})
	return nil
}

func networkCreateArgs(owner, name string, now time.Time, extraArgs []string) []string {
	args := append([]string{"network", "create"}, labelArgs(owner, now)...)
	args = append(args, extraArgs...)
	return append(args, name)
}

// ReapStale removes owner's containers and networks created more than
// olderThan ago: what an earlier run of the suite left behind when it was
// killed before its cleanup ran. Containers go first, so their networks are
// free to remove. A resource without owner's label, or without a readable
// CreatedLabel, is never touched: the docker daemon is shared with other
// agents and their tests. Failures are logged, not failed; reaping is
// housekeeping, not the test.
//
// olderThan must exceed the longest a run of the suite can take, or one run
// reaps another's live containers.
func ReapStale(t testing.TB, owner string, olderThan time.Duration) {
	t.Helper()
	mustOwner(t, owner)
	format := `{{.ID}}	{{.Label "` + OwnerLabel + `"}}	{{.Label "` + CreatedLabel + `"}}`
	now := time.Now()
	for _, kind := range []struct{ list, rm []string }{
		{[]string{"ps", "-a"}, []string{"rm", "-fv"}},
		{[]string{"network", "ls"}, []string{"network", "rm"}},
	} {
		out, err := exec.Command("docker", append(kind.list, "--filter", ownerFilter(owner), "--format", format)...).Output()
		if err != nil {
			t.Logf("testdocker: docker %s: %v", strings.Join(kind.list, " "), err)
			continue
		}
		ids := staleIDs(string(out), owner, now, olderThan)
		if len(ids) == 0 {
			continue
		}
		if out, err := exec.Command("docker", append(kind.rm, ids...)...).CombinedOutput(); err != nil {
			t.Logf("testdocker: reaping %s's stale resources (docker %s): %v\n%s", owner, strings.Join(kind.rm, " "), err, out)
			continue
		}
		t.Logf("testdocker: reaped %d stale %s resource(s) older than %s: %s", len(ids), owner, olderThan, strings.Join(ids, " "))
	}
}

// staleIDs reads `ID<TAB>owner<TAB>created` lines and returns the IDs labelled
// as owner's whose created stamp is more than olderThan before now. The docker
// filter already selected owner's resources; the owner column is checked again
// so a filter that matched more than it should cannot widen the reap.
func staleIDs(out, owner string, now time.Time, olderThan time.Duration) []string {
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), "\t")
		if len(f) != 3 || f[0] == "" || f[1] != owner {
			continue
		}
		created, err := strconv.ParseInt(strings.TrimSpace(f[2]), 10, 64)
		if err != nil || created <= 0 {
			continue
		}
		if now.Sub(time.Unix(created, 0)) > olderThan {
			ids = append(ids, f[0])
		}
	}
	return ids
}

func ownerFilter(owner string) string { return "label=" + OwnerLabel + "=" + owner }

// mustOwner refuses an empty owner: it would label nothing as anyone's, and a
// reap by it must not be guessed at.
func mustOwner(t testing.TB, owner string) {
	t.Helper()
	if strings.TrimSpace(owner) == "" || strings.ContainsAny(owner, "=\t\n") {
		t.Fatalf("testdocker: owner %q must be a non-empty label value without '=', tabs or newlines", owner)
	}
}
