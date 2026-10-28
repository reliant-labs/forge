package imagebuild

import (
	"archive/tar"
	"bytes"
	"context"
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
)

var amd64 = v1.Platform{OS: "linux", Architecture: "amd64"}

// testBase is a stand-in distroless: one layer holding /etc/passwd, an env and
// an entrypoint+cmd of its own that the spec must replace.
func testBase(t *testing.T, arch string) v1.Image {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range []*tar.Header{
		{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5},
	} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := tw.Write([]byte("root\n")); err != nil {
				t.Fatal(err)
			}
		}
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
	cf.OS, cf.Architecture = "linux", arch
	cf.Config = v1.Config{Env: []string{"PATH=/usr/bin", "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt"}, Entrypoint: []string{"/bin/base"}, Cmd: []string{"--base"}}
	img, err = mutate.ConfigFile(img, cf)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// fixture writes a binary and a migrations directory to a temp dir.
func fixture(t *testing.T) (bin, migrations string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "demo")
	if err := os.WriteFile(bin, []byte("\x7fELF-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	migrations = filepath.Join(dir, "db", "migrations")
	if err := os.MkdirAll(migrations, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(migrations, "001_init.up.sql"), []byte("create table t ();"), 0o644); err != nil {
		t.Fatal(err)
	}
	return bin, migrations
}

func testSpec(t *testing.T) Spec {
	bin, migrations := fixture(t)
	return Spec{
		Base: "example/base", BaseImage: testBase(t, "amd64"), Platform: amd64,
		Workdir: "/app", User: "65532:65532",
		Entrypoint: []string{"/app/demo"}, Cmd: []string{"server"},
		Env:      map[string]string{"PATH": "/app:/usr/bin", "APP_MODE": "prod"},
		Binaries: map[string]string{"/app/demo": bin},
		Files:    map[string]string{"/app/db/migrations": migrations},
		Created:  time.Unix(1700000000, 0),
	}
}

type layerFile struct {
	typ   byte
	mode  int64
	uid   int
	mtime int64
	body  string
}

func readLayer(t *testing.T, l v1.Layer) (names []string, files map[string]layerFile) {
	t.Helper()
	rc, err := l.Uncompressed()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	files = map[string]layerFile{}
	tr := tar.NewReader(rc)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		names = append(names, h.Name)
		files[h.Name] = layerFile{typ: h.Typeflag, mode: h.Mode, uid: h.Uid, mtime: h.ModTime.Unix(), body: string(b)}
	}
	return names, files
}

func TestBuildConfig(t *testing.T) {
	res, err := Build(context.Background(), testSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	cf, err := res.Image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	c := cf.Config
	if !reflect.DeepEqual(c.Entrypoint, []string{"/app/demo"}) || !reflect.DeepEqual(c.Cmd, []string{"server"}) {
		t.Errorf("entrypoint/cmd = %v %v", c.Entrypoint, c.Cmd)
	}
	if c.User != "65532:65532" || c.WorkingDir != "/app" {
		t.Errorf("user/workdir = %q %q", c.User, c.WorkingDir)
	}
	// The spec replaces PATH in place, keeps the base's other vars, appends its own.
	wantEnv := []string{"PATH=/app:/usr/bin", "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt", "APP_MODE=prod"}
	if !reflect.DeepEqual(c.Env, wantEnv) {
		t.Errorf("env = %v, want %v", c.Env, wantEnv)
	}
	if cf.OS != "linux" || cf.Architecture != "amd64" || !cf.Created.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("os/arch/created = %s/%s %v", cf.OS, cf.Architecture, cf.Created)
	}
	// The base had no history of its own, so mutate gives its layer an
	// empty entry; the two after it are forge's.
	if h := cf.History; len(h) != 3 || !strings.Contains(h[1].CreatedBy, "binaries /app/demo") || !strings.Contains(h[2].CreatedBy, "files /app/db/migrations") {
		t.Errorf("history = %+v", h)
	}
	if mt, _ := res.Image.MediaType(); mt != types.OCIManifestSchema1 {
		t.Errorf("manifest media type = %s, want the base's", mt)
	}
}

// A new entrypoint drops the base's CMD; a nil Cmd does not resurrect it.
func TestEntrypointClearsTheBaseCmd(t *testing.T) {
	s := testSpec(t)
	s.Cmd = nil
	res, err := Build(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	cf, _ := res.Image.ConfigFile()
	if cf.Config.Cmd != nil {
		t.Errorf("cmd = %v, want none", cf.Config.Cmd)
	}
}

func TestBuildLayers(t *testing.T) {
	res, err := Build(context.Background(), testSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	layers, err := res.Image.Layers()
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 3 {
		t.Fatalf("%d layers, want base + binaries + files", len(layers))
	}
	names, files := readLayer(t, layers[1])
	if !reflect.DeepEqual(names, []string{"app/", "app/demo"}) {
		t.Errorf("binaries layer = %v", names)
	}
	if f := files["app/demo"]; f.mode != 0o755 || f.uid != 0 || f.mtime != 1700000000 || f.body != "\x7fELF-binary" {
		t.Errorf("binary entry = %+v", f)
	}
	names, files = readLayer(t, layers[2])
	want := []string{"app/", "app/db/", "app/db/migrations/", "app/db/migrations/001_init.up.sql"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("files layer = %v, want %v", names, want)
	}
	if f := files["app/db/migrations/001_init.up.sql"]; f.mode != 0o644 || f.body != "create table t ();" {
		t.Errorf("file entry = %+v", f)
	}
}

func TestDigestIsReproducible(t *testing.T) {
	s := testSpec(t)
	a, err := Build(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Build(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest != b.Digest {
		t.Errorf("two builds of the same inputs: %s vs %s", a.Digest, b.Digest)
	}
	if err := os.WriteFile(s.Binaries["/app/demo"], []byte("\x7fELF-other!"), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := Build(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if c.Digest == a.Digest {
		t.Error("a changed binary must change the digest")
	}
}

func TestMissingFileIsAnError(t *testing.T) {
	s := testSpec(t)
	s.Files = map[string]string{"/app/conf": filepath.Join(t.TempDir(), "nope")}
	_, err := Build(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), "does not exist") || !strings.Contains(err.Error(), "/app/conf") {
		t.Errorf("err = %v", err)
	}
}

func TestBaseOfTheWrongPlatformIsRefused(t *testing.T) {
	s := testSpec(t)
	s.BaseImage = testBase(t, "arm64")
	if _, err := Build(context.Background(), s); err == nil || !strings.Contains(err.Error(), "linux/arm64") {
		t.Errorf("err = %v", err)
	}
}

func TestScratchBase(t *testing.T) {
	s := testSpec(t)
	s.Base, s.BaseImage, s.Files = "scratch", nil, nil
	res, err := Build(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	layers, _ := res.Image.Layers()
	if len(layers) != 1 || res.Base != "scratch" {
		t.Errorf("%d layers, base %q", len(layers), res.Base)
	}
}

// The base is pulled from a registry for the platform, the result pushed to
// one and pulled back with the digest Build reported; an unpinned base is
// reported with what it resolved to.
func TestPullBuildPushThroughARegistry(t *testing.T) {
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	ctx := context.Background()
	if err := Push(ctx, testBase(t, "amd64"), []string{host + "/base:1"}, nil); err != nil {
		t.Fatal(err)
	}

	s := testSpec(t)
	s.Base, s.BaseImage = host+"/base:1", nil
	res, err := Build(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Base, "resolved to sha256:") {
		t.Errorf("base label = %q, want the resolved digest", res.Base)
	}
	if err := Push(ctx, res.Image, []string{host + "/app:latest", host + "/app:v1"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"latest", "v1"} {
		ref, _ := name.ParseReference(host + "/app:" + tag)
		desc, err := remote.Head(ref)
		if err != nil {
			t.Fatal(err)
		}
		if desc.Digest != res.Digest {
			t.Errorf(":%s holds %s, Build reported %s", tag, desc.Digest, res.Digest)
		}
	}

	// A digest-pinned base is reported as written.
	d, _ := testBase(t, "amd64").Digest()
	s.Base = host + "/base@" + d.String()
	if res, err = Build(ctx, s); err != nil {
		t.Fatal(err)
	} else if res.Base != s.Base {
		t.Errorf("pinned base label = %q", res.Base)
	}
}

func TestWriteTarballIsLoadable(t *testing.T) {
	res, err := Build(context.Background(), testSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteTarball(&buf, res.Image, []string{"demo:latest", "demo:v1"}); err != nil {
		t.Fatal(err)
	}
	tag, _ := name.NewTag("demo:v1")
	img, err := tarball.Image(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf.Bytes())), nil }, &tag)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cf.Config.Entrypoint, []string{"/app/demo"}) {
		t.Errorf("loaded entrypoint = %v", cf.Config.Entrypoint)
	}
}
