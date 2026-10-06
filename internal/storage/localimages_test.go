package storage

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type fakeImageHost struct {
	ps, images string
	used       string // output of inspect --format
	removed    []string
	rmiFails   map[string]bool
}

func (f *fakeImageHost) command(_ context.Context, name string, args ...string) ([]byte, error) {
	joined := strings.Join(args, " ")
	switch {
	case name == "docker" && joined == "context inspect":
		return []byte(`[{"Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]`), nil
	case strings.HasPrefix(joined, "ps --format"):
		return []byte(f.ps), nil
	case joined == "ps -aq":
		return []byte("c1\nc2\n"), nil
	case strings.HasPrefix(joined, "inspect --format"):
		return []byte(f.used), nil
	case strings.HasPrefix(joined, "image ls"):
		return []byte(f.images), nil
	case strings.HasPrefix(joined, "rmi "):
		ref := strings.TrimPrefix(joined, "rmi ")
		if f.rmiFails[ref] {
			return nil, fmt.Errorf("image is being used")
		}
		f.removed = append(f.removed, ref)
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected %s %s", name, joined)
}

func imageRow(repo, tag, id string) string {
	return fmt.Sprintf(`{"Repository":%q,"Tag":%q,"ID":%q}`+"\n", repo, tag, id)
}

func newFakeImageHost() *fakeImageHost {
	return &fakeImageHost{
		// One live registry publishing 5051.
		ps: `{"Names":"k3d-live-registry","Ports":"0.0.0.0:5051->5000/tcp"}` + "\n" +
			`{"Names":"web","Ports":"0.0.0.0:8080->80/tcp"}` + "\n",
		used: "sha256:inuse localhost:5061/inuse:1\n",
		images: imageRow("localhost:5061/workspace-base", "t1", "sha256:a") + // dead port
			imageRow("registry.localhost:5061/workspace-base", "t1", "sha256:a") + // same image, second tag
			imageRow("localhost:5051/workspace-base", "t2", "sha256:b") + // live port
			imageRow("k3d-gone-registry:5000/app", "x", "sha256:c") + // no such container
			imageRow("k3d-live-registry:5000/app", "x", "sha256:d") + // live container
			imageRow("localhost:5061/inuse", "1", "sha256:inuse") + // a container uses it
			imageRow("localhost:5061/untagged", "<none>", "sha256:e") +
			imageRow("ghcr.io/org/app", "v1", "sha256:f") + // not a local registry
			imageRow("localhost/app", "dev", "sha256:g") + // no port
			imageRow("localhost:8080/app", "dev", "sha256:h"), // live port of a non-registry container: kept
	}
}

func TestStaleLocalImagesSelectsOnlyDeadRegistryTagsNoContainerUses(t *testing.T) {
	f := newFakeImageHost()
	got, err := (Runner{Command: f.command}).StaleLocalImages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, img := range got {
		refs = append(refs, img.Ref)
	}
	want := "localhost:5061/workspace-base:t1,registry.localhost:5061/workspace-base:t1,k3d-gone-registry:5000/app:x"
	if strings.Join(refs, ",") != want {
		t.Fatalf("stale = %v\nwant %s", refs, want)
	}
}

func TestLocalImagesDryRunRemovesNothingAndApplyUntags(t *testing.T) {
	f := newFakeImageHost()
	var out strings.Builder
	r := Runner{Command: f.command, Out: &out}
	if err := r.LocalImages(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if len(f.removed) != 0 || !strings.Contains(out.String(), "untag stale local-registry image localhost:5061/workspace-base:t1") {
		t.Fatalf("dry run removed=%v out=%s", f.removed, out.String())
	}
	f.rmiFails = map[string]bool{"k3d-gone-registry:5000/app:x": true}
	err := r.LocalImages(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "k3d-gone-registry") {
		t.Fatalf("a refused rmi must be reported: %v", err)
	}
	if len(f.removed) != 2 {
		t.Fatalf("one refusal must not stop the rest: %v", f.removed)
	}
}

func TestStaleLocalImagesKeepsAliasesOfARunningRegisteredRegistry(t *testing.T) {
	f := newFakeImageHost()
	f.images = imageRow("registry.localhost:5000/app", "x", "sha256:z")
	r := Runner{Command: f.command, Policy: Policy{Registries: []Registry{{Container: "k3d-live-registry", Aliases: []string{"registry.localhost:5000"}}}}}
	got, err := r.StaleLocalImages(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("stale = %v, %v", got, err)
	}
}
