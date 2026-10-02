package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// registryVolume is the registry's data volume as k3d creates it: ANONYMOUS
// (registry:2 declares VOLUME /var/lib/registry, and k3d mounts nothing over
// it), so its name is a bare hex id.
var registryVolume = strings.Repeat("ab", 32)

// fakeRegistryHost emulates the docker CLI and the registry HTTP API for one
// full RegistryGC pass: a running k3d registry holding one live tag and one
// expired untagged manifest, so the plan is non-empty and apply reaches the
// stop / helper / DELETE / garbage-collect / restart sequence.
//
// Only docker and the registry API are faked. Inventory parsing, the
// protected set, the graph plan and the maintenance sequence are the
// production code.
type fakeRegistryHost struct {
	t        *testing.T
	registry string
	port     string

	mu      sync.Mutex
	running map[string]bool
	calls   [][]string
	// fail makes the Nth matching call fail: key is a call prefix (joined
	// args), value is how many matching calls fail before it succeeds.
	fail map[string]int
	// failAlways makes every matching call fail.
	failAlways map[string]bool
	// startDoesNotRun makes `docker start` exit 0 while the container stays
	// stopped, as a registry that crashes on start does.
	startDoesNotRun bool

	tags, revisions string
}

func newFakeRegistryHost(t *testing.T) *fakeRegistryHost {
	t.Helper()
	now := time.Now()
	live, orphan := testDigest("a"), testDigest("b")
	manifests := map[string]string{live: singleArch(1, 2), orphan: singleArch(3, 4)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v2/" {
			return
		}
		digest := strings.TrimPrefix(req.URL.Path, "/v2/app/manifests/")
		body, ok := manifests[digest]
		switch {
		case !ok:
			http.NotFound(w, req)
		case req.Method == http.MethodDelete:
			w.WriteHeader(http.StatusAccepted)
		default:
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRegistryHost{
		t: t, registry: "k3d-test-registry", port: port,
		running: map[string]bool{"k3d-test-registry": true},
		fail:    map[string]int{}, failAlways: map[string]bool{},
	}
	const prefix = "/var/lib/registry/docker/registry/v2/repositories/app/_manifests/"
	f.tags = fmt.Sprintf("%d\t%stags/dev/current/link\t%s\n", now.Unix(), prefix, live)
	old := now.Add(-30 * 24 * time.Hour).Unix()
	f.revisions = fmt.Sprintf("%d\t%srevisions/sha256/%s/link\t%s\n%d\t%srevisions/sha256/%s/link\t%s\n",
		now.Unix(), prefix, strings.TrimPrefix(live, "sha256:"), live,
		old, prefix, strings.TrimPrefix(orphan, "sha256:"), orphan)
	return f
}

func (f *fakeRegistryHost) reg() Registry {
	return Registry{Container: f.registry, Repositories: []string{"app"}, Aliases: []string{"localhost:" + f.port}}
}

// helperName and gcName are the maintenance containers RegistryGC starts.
func (f *fakeRegistryHost) helperName() string { return f.registry + "-forge-retention" }
func (f *fakeRegistryHost) gcName() string     { return f.registry + "-forge-gc" }

func (f *fakeRegistryHost) command(_ context.Context, name string, args ...string) ([]byte, error) {
	if name != "docker" {
		return nil, fmt.Errorf("unexpected command %s %v", name, args)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), args...))
	joined := strings.Join(args, " ")
	for prefix := range f.failAlways {
		if strings.HasPrefix(joined, prefix) {
			return nil, fmt.Errorf("injected failure: docker %s", joined)
		}
	}
	for prefix, n := range f.fail {
		if n > 0 && strings.HasPrefix(joined, prefix) {
			f.fail[prefix] = n - 1
			return nil, fmt.Errorf("injected failure: docker %s", joined)
		}
	}
	switch {
	case args[0] == "inspect" && len(args) == 2:
		return f.inspect(args[1])
	case joined == "ps -aq --filter label=k3d.role=server", joined == "ps -aq":
		return nil, nil
	case args[0] == "ps":
		// ps [-q|-aq] --filter name=^/<name>$
		filter := args[len(args)-1]
		target := strings.TrimSuffix(strings.TrimPrefix(filter, "name=^/"), "$")
		if f.running[target] {
			return []byte(target + "-id\n"), nil
		}
		return nil, nil
	case args[0] == "exec" && strings.Contains(joined, "_manifests/tags/"):
		return []byte(f.tags), nil
	case args[0] == "exec" && strings.Contains(joined, "_manifests/revisions/"):
		return []byte(f.revisions), nil
	case args[0] == "stop":
		f.running[args[len(args)-1]] = false
		return nil, nil
	case args[0] == "start":
		if !f.startDoesNotRun {
			f.running[args[1]] = true
		}
		return nil, nil
	case args[0] == "run":
		for i, a := range args {
			if a == "--name" {
				if args[1] == "-d" {
					f.running[args[i+1]] = true
				}
				return []byte("ok"), nil
			}
		}
	}
	return nil, fmt.Errorf("unexpected docker %v", args)
}

func (f *fakeRegistryHost) inspect(name string) ([]byte, error) {
	if name != f.registry && name != f.helperName() {
		return nil, fmt.Errorf("no such container %s", name)
	}
	info := map[string]any{
		"Image":  "sha256:" + strings.Repeat("e", 64),
		"State":  map[string]any{"Running": f.running[name]},
		"Config": map[string]any{"Cmd": []string{"/etc/docker/registry/config.yml"}},
		"Mounts": []map[string]any{{
			"Type": "volume", "Name": registryVolume, "Destination": "/var/lib/registry", "RW": true,
			"Source": "/var/lib/docker/volumes/" + registryVolume + "/_data",
		}},
		"NetworkSettings": map[string]any{
			"Ports":    map[string]any{"5000/tcp": []map[string]string{{"HostPort": f.port}}},
			"Networks": map[string]any{"k3d-test": map[string]any{}},
		},
	}
	b, err := json.Marshal([]any{info})
	return b, err
}

// runs returns every `docker run` invocation, in order.
func (f *fakeRegistryHost) runs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if len(c) > 0 && c[0] == "run" {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeRegistryHost) called(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(strings.Join(c, " "), prefix) {
			n++
		}
	}
	return n
}

func (f *fakeRegistryHost) isRunning(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running[name]
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// TestRegistryGCNeverAsksDockerToRemoveTheRegistryVolume is the H1 regression.
// `docker run --rm --volumes-from <registry>` makes the helper a co-owner of
// the registry's ANONYMOUS data volume, and `--rm` removes anonymous volumes
// on exit (moby removeMountPoints skips only mounts with an explicit source,
// and ignores "in use"). Only the registry container's own reference kept
// every pushed image alive; removing that container during a pass — k3d
// registry delete, a recreate, an agent's reset — let the helper's exit
// delete the store.
//
// A volume mounted BY NAME has an explicit source and is never removed by
// --rm, so that is the only form a maintenance container may use.
func TestRegistryGCNeverAsksDockerToRemoveTheRegistryVolume(t *testing.T) {
	f := newFakeRegistryHost(t)
	r := Runner{Policy: DefaultPolicy(), Command: f.command}
	if err := r.RegistryGC(context.Background(), f.reg(), true); err != nil {
		t.Fatalf("RegistryGC: %v", err)
	}
	runs := f.runs()
	if len(runs) != 2 {
		t.Fatalf("want the retention helper and the garbage-collect pass, got %d runs: %v", len(runs), runs)
	}
	for _, args := range runs {
		if hasArg(args, "--volumes-from") {
			t.Errorf("maintenance container inherits volumes from the registry: %v", args)
			if hasArg(args, "--rm") {
				t.Errorf("--rm with --volumes-from: removing this container removes the registry's anonymous volume")
			}
		}
		if !hasArg(args, "type=volume,src="+registryVolume+",dst=/var/lib/registry") {
			t.Errorf("registry data volume is not mounted by name: %v", args)
		}
	}
	if !f.isRunning(f.registry) {
		t.Fatal("registry was not restarted")
	}
}

// TestMaintenanceMountsReproduceEveryMountByName pins the translation from the
// registry's inspected mounts to the helper's: every mount keeps its
// destination and read-only bit, and every source is explicit.
func TestMaintenanceMountsReproduceEveryMountByName(t *testing.T) {
	got, err := maintenanceMounts(containerInfo{Mounts: []containerMount{
		{Type: "volume", Name: registryVolume, Destination: "/var/lib/registry", RW: true},
		{Type: "bind", Source: "/host/config.yml", Destination: "/etc/docker/registry/config.yml", RW: false},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--mount", "type=volume,src=" + registryVolume + ",dst=/var/lib/registry",
		"--mount", "type=bind,src=/host/config.yml,dst=/etc/docker/registry/config.yml,readonly",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("mounts = %v, want %v", got, want)
	}
	for _, bad := range []containerMount{
		{Type: "volume", Destination: "/var/lib/registry", RW: true},
		{Type: "tmpfs", Destination: "/tmp", RW: true},
		{Type: "bind", Destination: "/etc/x", RW: true},
		{Type: "bind", Source: "/a,b", Destination: "/etc/x", RW: true},
	} {
		if _, err := maintenanceMounts(containerInfo{Mounts: []containerMount{bad}}); err == nil {
			t.Errorf("mount %+v cannot be reproduced by name and must be refused", bad)
		}
	}
}
