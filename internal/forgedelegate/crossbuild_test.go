package forgedelegate

import (
	"os"
	"os/exec"
	"testing"
)

// TestBuildsOnTargetsWithoutFlock pins the lock files' build constraints:
// the package (and pkg/hostslot under it) must compile where x/sys/unix has
// no flock(2), falling back to lock_other.go, as well as where it has one.
// wasm is not a target: the package runs other programs (go install, exec of
// the pinned forge), which no wasm port can.
func TestBuildsOnTargetsWithoutFlock(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiles the package with go vet once per target; runs in task test")
	}
	for _, target := range [][2]string{
		{"linux", "amd64"}, {"darwin", "arm64"}, {"windows", "amd64"},
		{"aix", "ppc64"}, {"plan9", "amd64"},
	} {
		t.Run(target[0]+"/"+target[1], func(t *testing.T) {
			t.Parallel()
			cmd := exec.Command("go", "vet", ".")
			cmd.Env = append(os.Environ(), "GOOS="+target[0], "GOARCH="+target[1], "CGO_ENABLED=0")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("go vet for %s/%s: %v\n%s", target[0], target[1], err, out)
			}
		})
	}
}
