package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"debug/elf"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/imagebuild"
)

// writeELF writes the smallest file debug/elf accepts as a 64-bit little-endian
// Linux executable for goarch. The image-binary guard reads only the header, so
// a real compile (seconds, a toolchain) would add nothing to these tests.
func writeELF(t *testing.T, path, goarch string) {
	t.Helper()
	machine, ok := goarchToELFMachine(goarch)
	if !ok {
		t.Fatalf("no ELF machine for %q", goarch)
	}
	var b bytes.Buffer
	hdr := elf.Header64{
		Ident: [elf.EI_NIDENT]byte{0x7f, 'E', 'L', 'F', byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT), byte(elf.ELFOSABI_NONE)},
		Type:  uint16(elf.ET_EXEC), Machine: uint16(machine), Version: uint32(elf.EV_CURRENT),
		Ehsize: 64, Phentsize: 56, Shentsize: 64,
	}
	if err := binary.Write(&b, binary.LittleEndian, &hdr); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}
}

// fixtureBase is a stand-in distroless/static: one layer, an /etc/passwd.
func fixtureBase(t *testing.T) v1.Image {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("root\n")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(empty.Image, static.NewLayer(buf.Bytes(), types.OCILayer))
	if err != nil {
		t.Fatal(err)
	}
	img = mutate.MediaType(img, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, types.OCIConfigJSON)
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cf = cf.DeepCopy()
	cf.OS, cf.Architecture = "linux", "amd64"
	cf.Config = v1.Config{Env: []string{"PATH=/usr/bin"}}
	if img, err = mutate.ConfigFile(img, cf); err != nil {
		t.Fatal(err)
	}
	return img
}

func stubDaemon(t *testing.T, err error) {
	t.Helper()
	prev := dockerDaemonReachable
	dockerDaemonReachable = func(context.Context) error { return err }
	t.Cleanup(func() { dockerDaemonReachable = prev })
}

// modelProject is a project dir (cwd) with a built bin/demo, db/migrations, and
// forge.yaml's image block pointing at base.
func modelProject(t *testing.T, base string, binArch string) *config.ProjectConfig {
	t.Helper()
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("bin", 0o755); err != nil {
		t.Fatal(err)
	}
	writeELF(t, filepath.Join("bin", "demo"), binArch)
	if err := os.MkdirAll(filepath.Join("db", "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("db", "migrations", "001.up.sql"), []byte("select 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &config.ProjectConfig{Name: "demo", Image: config.ImageConfig{
		Base:  base,
		Cmd:   []string{"server"},
		Files: map[string]string{"db/migrations": "/app/db/migrations"},
	}}
}

func modelGuard() projectImageGuard {
	return projectImageGuard{outputDir: "bin", binaryNames: []string{"demo"}, goTargets: []goBuildTarget{{cmd: "./cmd/demo", outputName: "demo"}}, envLabel: "prod"}
}

// The model path end to end against an in-memory registry: base pulled from
// it, binaries and files layered on, result pushed to it, with NO daemon and
// NO Dockerfile, and the buildResult carries the digest the build state and
// the release ledger record.
func TestProjectImageFromModelEndToEnd(t *testing.T) {
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	base := fixtureBase(t)
	if err := imagebuild.Push(context.Background(), base, []string{host + "/distroless/static:nonroot"}, nil); err != nil {
		t.Fatal(err)
	}
	d, _ := base.Digest()
	cfg := modelProject(t, host+"/distroless/static@"+d.String(), "amd64")
	stubDaemon(t, errors.New("cannot reach the Docker daemon"))

	tags := imageTagSet(host+"/acme/demo", "v1.2.3", true, true)
	res := dockerBuildProject(context.Background(), cfg, tags, "amd64", versionInfo{version: "v1.2.3"}, modelGuard())
	if res.err != nil {
		t.Fatalf("build: %v", res.err)
	}
	if res.name != "demo (docker)" || res.kind != "docker" || res.image != "demo" || res.repository != host+"/acme/demo" || res.tag != "v1.2.3" {
		t.Errorf("result identity = %+v", res)
	}
	if !strings.HasPrefix(res.digest, "sha256:") || !reflect.DeepEqual(res.platforms, []string{"linux/amd64"}) {
		t.Errorf("digest/platforms = %q %v", res.digest, res.platforms)
	}

	ref, _ := name.ParseReference(host + "/acme/demo:v1.2.3")
	img, err := remote.Image(ref)
	if err != nil {
		t.Fatalf("image not in the registry: %v", err)
	}
	if got, _ := img.Digest(); got.String() != res.digest {
		t.Errorf("registry holds %s, build recorded %s", got, res.digest)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	c := cf.Config
	if !reflect.DeepEqual(c.Entrypoint, []string{"/app/demo"}) || !reflect.DeepEqual(c.Cmd, []string{"server"}) ||
		c.User != "65532:65532" || c.WorkingDir != "/app" {
		t.Errorf("config = %+v", c)
	}
	layers, _ := img.Layers()
	if len(layers) != 3 {
		t.Errorf("%d layers, want base + binaries + files", len(layers))
	}
}

// A release build pushes only its version tag; :latest never moves.
func TestProjectImageFromModelPushesOnlyThePushTags(t *testing.T) {
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	if err := imagebuild.Push(context.Background(), fixtureBase(t), []string{host + "/base:1"}, nil); err != nil {
		t.Fatal(err)
	}
	cfg := modelProject(t, host+"/base:1", "amd64")
	stubDaemon(t, errors.New("no daemon"))
	res := dockerBuildProject(context.Background(), cfg, imageTagSet(host+"/acme/demo", "v2", true, true), "amd64", versionInfo{}, modelGuard())
	if res.err != nil {
		t.Fatal(res.err)
	}
	latest, _ := name.ParseReference(host + "/acme/demo:latest")
	if _, err := remote.Head(latest); err == nil {
		t.Error("a release-scoped build pushed :latest")
	}
}

func TestProjectImageFromModelLoadsWhenNothingIsPushed(t *testing.T) {
	cfg := modelProject(t, "scratch", "amd64")
	stubDaemon(t, nil)
	var loaded bytes.Buffer
	prev := dockerLoad
	dockerLoad = func(_ context.Context, write func(io.Writer) error) error { return write(&loaded) }
	t.Cleanup(func() { dockerLoad = prev })

	res := dockerBuildProject(context.Background(), cfg, imageTagSet("demo", "v1", false, false), "amd64", versionInfo{}, modelGuard())
	if res.err != nil {
		t.Fatal(res.err)
	}
	if res.digest != "" {
		t.Errorf("a loaded image records no registry digest, got %q", res.digest)
	}
	tag, _ := name.NewTag("demo:v1")
	img, err := tarball.Image(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(loaded.Bytes())), nil }, &tag)
	if err != nil {
		t.Fatalf("docker load got no demo:v1: %v", err)
	}
	if cf, _ := img.ConfigFile(); !reflect.DeepEqual(cf.Config.Entrypoint, []string{"/app/demo"}) {
		t.Errorf("loaded entrypoint = %v", cf.Config.Entrypoint)
	}
}

func TestProjectImageFromModelRefusesWhenItHasNowhereToGo(t *testing.T) {
	cfg := modelProject(t, "scratch", "amd64")
	stubDaemon(t, errors.New("cannot reach the Docker daemon"))
	res := dockerBuildProject(context.Background(), cfg, imageTagSet("demo", "v1", false, false), "amd64", versionInfo{}, modelGuard())
	if res.err == nil || !strings.Contains(res.err.Error(), "--push") {
		t.Fatalf("err = %v: an image with no daemon and no registry has nowhere to live", res.err)
	}
}

func TestProjectImageFromModelRejectsAWrongArchBinary(t *testing.T) {
	cfg := modelProject(t, "scratch", "arm64")
	stubDaemon(t, errors.New("no daemon"))
	res := dockerBuildProject(context.Background(), cfg, imageTagSet("registry.example/acme/demo", "v1", true, true), "amd64", versionInfo{}, modelGuard())
	if res.err == nil || !strings.Contains(res.err.Error(), "linux/amd64") {
		t.Fatalf("err = %v: the same arch guard the docker path runs must run here", res.err)
	}
}

func TestProjectImageSpecDefaults(t *testing.T) {
	cfg := &config.ProjectConfig{Name: "demo", Image: config.ImageConfig{Base: "b", Env: map[string]string{"A": "1"}}}
	targets := []goBuildTarget{{cmd: "./cmd/demo", outputName: "demo"}, {cmd: "./cmd/worker", outputName: "worker"}, {cmd: "./cmd/demo", outputName: "demo"}}
	s, err := projectImageSpec(cfg, targets, "bin", "arm64", time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if s.Workdir != "/app" || s.User != "65532:65532" || !reflect.DeepEqual(s.Entrypoint, []string{"/app/demo"}) || s.Cmd != nil {
		t.Errorf("defaults = workdir %q user %q entrypoint %v cmd %v", s.Workdir, s.User, s.Entrypoint, s.Cmd)
	}
	want := map[string]string{"/app/demo": filepath.Join("bin", "demo"), "/app/worker": filepath.Join("bin", "worker")}
	if !reflect.DeepEqual(s.Binaries, want) {
		t.Errorf("binaries = %v, want %v", s.Binaries, want)
	}
	if !reflect.DeepEqual(s.Platform, v1.Platform{OS: "linux", Architecture: "arm64"}) {
		t.Errorf("platform = %v", s.Platform)
	}
}

func TestPrimaryImageBinary(t *testing.T) {
	for _, tc := range []struct {
		names []string
		want  string
	}{
		{[]string{"worker", "demo"}, "demo"},
		{[]string{"api"}, "api"},
	} {
		got, err := primaryImageBinary("demo", tc.names)
		if err != nil || got != tc.want {
			t.Errorf("primaryImageBinary(%v) = %q, %v", tc.names, got, err)
		}
	}
	if _, err := primaryImageBinary("demo", []string{"api", "worker"}); err == nil || !strings.Contains(err.Error(), "image.entrypoint") {
		t.Errorf("ambiguous binaries: err = %v", err)
	}
}

// Without the image block the project image is docker's, and without a
// daemon it fails once, naming the model as a way out.
func TestDockerfileProjectImageWithoutADaemonNamesTheModel(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("Dockerfile", []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll("bin", 0o755); err != nil {
		t.Fatal(err)
	}
	writeELF(t, filepath.Join("bin", "demo"), "amd64")
	stubDaemon(t, errors.New("cannot reach the Docker daemon"))
	res := dockerBuildProject(context.Background(), &config.ProjectConfig{Name: "demo"}, imageTagSet("r.example/demo", "v1", true, true), "amd64", versionInfo{}, modelGuard())
	if res.err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"Docker daemon", "DOCKER_HOST", "image:", "forge.yaml"} {
		if !strings.Contains(res.err.Error(), want) {
			t.Errorf("error does not mention %q:\n%s", want, res.err)
		}
	}
}

func TestRequireDaemonForDockerImage(t *testing.T) {
	stubDaemon(t, nil)
	if err := requireDaemonForDockerImage(context.Background(), "service gw", "gw/Dockerfile", ""); err != nil {
		t.Fatalf("with a daemon: %v", err)
	}
	stubDaemon(t, errors.New("cannot reach the Docker daemon: unix:///nonexistent"))
	err := requireDaemonForDockerImage(context.Background(), "service gw", "gw/Dockerfile", "")
	if err == nil {
		t.Fatal("without a daemon this must fail before docker does")
	}
	for _, want := range []string{"service gw", "gw/Dockerfile", "DOCKER_HOST", "forge.RemoteBuild"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%s", want, err)
		}
	}
}

func TestDedupeRefs(t *testing.T) {
	got := dedupeRefs([]string{"r/a:latest", "r/a:latest", "r/a:v1", "r/a:latest"})
	if !reflect.DeepEqual(got, []string{"r/a:latest", "r/a:v1"}) {
		t.Errorf("refs = %v", got)
	}
}

func TestWithTrimpathDoesNotMutateTheCallersTargets(t *testing.T) {
	in := []goBuildTarget{{cmd: "./cmd/a", outputName: "a"}}
	out := withTrimpath(in)
	if !out[0].trimpath || in[0].trimpath {
		t.Errorf("out=%v in=%v", out[0].trimpath, in[0].trimpath)
	}
}
