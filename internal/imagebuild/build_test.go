package imagebuild

import (
	"archive/tar"
	"context"
	"errors"
	"io"
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
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// The Dockerfile forge scaffolds (internal/templates/project/Dockerfile.tmpl),
// reduced to its stage shape. The builder stage is what `forge build` replaces
// with a host `go build`; the production stage is what this package assembles.
const scaffolded = `# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS builder
ARG FORGE_VERSION=dev
WORKDIR /app
COPY . .
RUN CGO_ENABLED=0 go build -o /app/bin/app ./cmd/app

FROM alpine:3.21 AS debug
RUN apk --no-cache add ca-certificates
ENTRYPOINT ["dlv"]

FROM alpine:3.21 AS production
RUN apk --no-cache add ca-certificates
WORKDIR /app
COPY --from=builder /app/bin/app ./app
COPY --from=builder /app/bin/worker ./worker
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://localhost:8080/healthz || exit 1
ENTRYPOINT ["/app/app"]
CMD ["server"]
`

var linuxAMD64 = v1.Platform{OS: "linux", Architecture: "amd64"}

// alpineLike is a stand-in base: a CA bundle, /etc/passwd, a PATH, and a
// base-set CMD that the stage's ENTRYPOINT must reset.
func alpineLike(t *testing.T) v1.Image {
	t.Helper()
	var buf strings.Builder
	tw := tar.NewWriter(&buf)
	for _, h := range []struct {
		name string
		dir  bool
		body string
	}{
		{"etc/", true, ""},
		{"etc/ssl/", true, ""},
		{"etc/ssl/certs/", true, ""},
		{"etc/ssl/certs/ca-certificates.crt", false, "-----BEGIN CERTIFICATE-----"},
		{"etc/passwd", false, "root:x:0:0:root:/root:/bin/sh\n"},
		{"app/", true, ""},
	} {
		hdr := &tar.Header{Name: h.name, Mode: 0o755, Typeflag: tar.TypeReg, Size: int64(len(h.body))}
		if h.dir {
			hdr.Typeflag, hdr.Size = tar.TypeDir, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, h.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	layer := static.NewLayer([]byte(buf.String()), types.OCILayer)
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatal(err)
	}
	img = mutate.MediaType(img, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, types.OCIConfigJSON)
	img, err = mutate.ConfigFile(img, &v1.ConfigFile{
		OS: "linux", Architecture: "amd64",
		RootFS:  v1.RootFS{Type: "layers", DiffIDs: mustDiffIDs(t, img)},
		History: []v1.History{{CreatedBy: "ADD alpine-minirootfs.tar.gz / # buildkit"}},
		Config: v1.Config{
			Env: []string{"PATH=/usr/local/sbin:/usr/bin"},
			Cmd: []string{"/bin/sh"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func mustDiffIDs(t *testing.T, img v1.Image) []v1.Hash {
	t.Helper()
	ls, err := img.Layers()
	if err != nil {
		t.Fatal(err)
	}
	var out []v1.Hash
	for _, l := range ls {
		d, err := l.DiffID()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func projectWithBinaries(t *testing.T) (ctxDir string, artifacts map[string]map[string]string) {
	t.Helper()
	ctxDir = t.TempDir()
	bin := filepath.Join(ctxDir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"app": "\x7fELF-app", "worker": "\x7fELF-worker"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return ctxDir, map[string]map[string]string{"builder": {
		"/app/bin/app":    filepath.Join(bin, "app"),
		"/app/bin/worker": filepath.Join(bin, "worker"),
	}}
}

func buildScaffolded(t *testing.T) *Result {
	t.Helper()
	ctxDir, artifacts := projectWithBinaries(t)
	res, err := Build(context.Background(), Options{
		Dockerfile: []byte(scaffolded), Target: "production", Platform: linuxAMD64,
		ContextDir: ctxDir, Artifacts: artifacts, Base: alpineLike(t),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(res.Cleanup)
	return res
}

func TestScaffoldedProductionStageConfig(t *testing.T) {
	res := buildScaffolded(t)
	cf, err := res.Image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	c := cf.Config
	if !reflect.DeepEqual(c.Entrypoint, []string{"/app/app"}) {
		t.Errorf("Entrypoint = %v", c.Entrypoint)
	}
	if !reflect.DeepEqual(c.Cmd, []string{"server"}) {
		t.Errorf("Cmd = %v, want [server] (the base's /bin/sh must not survive)", c.Cmd)
	}
	if c.User != "65532:65532" {
		t.Errorf("User = %q", c.User)
	}
	if c.WorkingDir != "/app" {
		t.Errorf("WorkingDir = %q", c.WorkingDir)
	}
	if _, ok := c.ExposedPorts["8080/tcp"]; !ok {
		t.Errorf("ExposedPorts = %v", c.ExposedPorts)
	}
	if len(c.Env) != 1 || c.Env[0] != "PATH=/usr/local/sbin:/usr/bin" {
		t.Errorf("Env = %v (the base's PATH must be inherited)", c.Env)
	}
	hc := c.Healthcheck
	if hc == nil || hc.Interval != 10*time.Second || hc.Timeout != 3*time.Second || hc.StartPeriod != 5*time.Second || hc.Retries != 3 {
		t.Fatalf("Healthcheck = %+v", hc)
	}
	if !reflect.DeepEqual(hc.Test, []string{"CMD-SHELL", "wget -qO- http://localhost:8080/healthz || exit 1"}) {
		t.Errorf("Healthcheck.Test = %q", hc.Test)
	}
	if cf.OS != "linux" || cf.Architecture != "amd64" {
		t.Errorf("platform = %s/%s", cf.OS, cf.Architecture)
	}
}

func TestScaffoldedProductionStageLayers(t *testing.T) {
	res := buildScaffolded(t)
	layers, err := res.Image.Layers()
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 3 {
		t.Fatalf("layers = %d, want base + one per COPY (app, worker)", len(layers))
	}
	files := map[string]*tar.Header{}
	bodies := map[string]string{}
	for _, l := range layers[1:] {
		rc, err := l.Uncompressed()
		if err != nil {
			t.Fatal(err)
		}
		tr := tar.NewReader(rc)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			files[h.Name] = h
			b, _ := io.ReadAll(tr)
			bodies[h.Name] = string(b)
		}
		_ = rc.Close()
	}
	for name, body := range map[string]string{"app/app": "\x7fELF-app", "app/worker": "\x7fELF-worker"} {
		h, ok := files[name]
		if !ok {
			t.Fatalf("layer has no %s; has %v", name, keys(files))
		}
		if bodies[name] != body {
			t.Errorf("%s body = %q", name, bodies[name])
		}
		if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
			t.Errorf("%s owner = %d:%d %q:%q, want root with no names", name, h.Uid, h.Gid, h.Uname, h.Gname)
		}
		if h.Mode&0o111 == 0 {
			t.Errorf("%s mode = %o, lost its executable bit", name, h.Mode)
		}
		if !h.ModTime.Equal(time.Unix(0, 0)) {
			t.Errorf("%s mtime = %v, want the epoch", name, h.ModTime)
		}
	}
	// /app already exists in the base, so no directory entry may be added for it.
	if _, ok := files["app"]; ok {
		t.Error("layer re-creates /app, which the base image already has")
	}
}

func keys(m map[string]*tar.Header) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestWorkdirOfMissingDirectoryIsCreated(t *testing.T) {
	ctxDir, artifacts := projectWithBinaries(t)
	// builder must be an earlier stage for the COPY to be legal.
	df := "FROM golang AS builder\nRUN true\nFROM alpine\nWORKDIR /srv/app\nCOPY --from=builder /app/bin/app ./app\n"
	res, err := Build(context.Background(), Options{
		Dockerfile: []byte(df), Platform: linuxAMD64, ContextDir: ctxDir, Artifacts: artifacts, Base: alpineLike(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Cleanup()
	layers, _ := res.Image.Layers()
	rc, _ := layers[len(layers)-1].Uncompressed()
	defer rc.Close()
	tr := tar.NewReader(rc)
	var got []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		got = append(got, h.Name)
	}
	want := []string{"srv/", "srv/app/", "srv/app/app"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("layer entries = %v, want %v", got, want)
	}
}

func TestHistoryHasAnEntryPerInstruction(t *testing.T) {
	res := buildScaffolded(t)
	cf, err := res.Image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range cf.History[1:] { // [0] is the base's own
		got = append(got, h.CreatedBy)
	}
	for _, want := range []string{"WORKDIR /app", "USER 65532:65532", "EXPOSE 8080", "ENTRYPOINT [\"/app/app\"]", "CMD [\"server\"]"} {
		found := false
		for _, g := range got {
			if g == want {
				found = true
			}
		}
		if !found {
			t.Errorf("history has no %q; has %q", want, got)
		}
	}
	empty, layered := 0, 0
	for _, h := range cf.History {
		if h.EmptyLayer {
			empty++
		} else {
			layered++
		}
	}
	if layers, _ := res.Image.Layers(); layered != len(layers) {
		t.Errorf("%d non-empty history entries for %d layers", layered, len(layers))
	}
}

func TestDigestIsReproducible(t *testing.T) {
	ctxDir, artifacts := projectWithBinaries(t)
	build := func() string {
		res, err := Build(context.Background(), Options{
			Dockerfile: []byte(scaffolded), Target: "production", Platform: linuxAMD64,
			ContextDir: ctxDir, Artifacts: artifacts, Base: alpineLike(t),
		})
		if err != nil {
			t.Fatal(err)
		}
		defer res.Cleanup()
		return res.Digest.String()
	}
	a := build()
	time.Sleep(1100 * time.Millisecond) // a wall-clock leak would show across a second
	if b := build(); a != b {
		t.Fatalf("same inputs, different digests:\n  %s\n  %s", a, b)
	}

	// And it moves when the binary does.
	if err := os.WriteFile(filepath.Join(ctxDir, "bin", "app"), []byte("changed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if c := build(); c == a {
		t.Fatal("digest did not change when the binary changed")
	}
}

func TestPushAndPullBackThroughRegistry(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	res := buildScaffolded(t)
	refs := []string{host + "/acme/app:latest", host + "/acme/app:v1.2.3"}
	if err := Push(context.Background(), res.Image, refs, nil); err != nil {
		t.Fatalf("Push: %v", err)
	}

	for _, r := range refs {
		ref, err := name.ParseReference(r)
		if err != nil {
			t.Fatal(err)
		}
		desc, err := remote.Head(ref)
		if err != nil {
			t.Fatalf("HEAD %s: %v", r, err)
		}
		if desc.Digest != res.Digest {
			t.Errorf("%s pushed as %s, built as %s", r, desc.Digest, res.Digest)
		}
	}
	ref, _ := name.ParseReference(refs[0])
	pulled, err := remote.Image(ref)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := pulled.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cf.Config.Entrypoint, []string{"/app/app"}) {
		t.Errorf("pulled Entrypoint = %v", cf.Config.Entrypoint)
	}
}

func TestBuildPullsBaseFromRegistryForPlatform(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	if err := Push(context.Background(), alpineLike(t), []string{host + "/library/alpine:3.21"}, nil); err != nil {
		t.Fatal(err)
	}
	ctxDir, artifacts := projectWithBinaries(t)
	df := strings.ReplaceAll(scaffolded, "alpine:3.21", host+"/library/alpine:3.21")
	res, err := Build(context.Background(), Options{
		Dockerfile: []byte(df), Target: "production", Platform: linuxAMD64, ContextDir: ctxDir, Artifacts: artifacts,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer res.Cleanup()
	if !strings.Contains(res.Base, host+"/library/alpine:3.21 (sha256:") {
		t.Errorf("Base = %q, want the ref and its resolved digest", res.Base)
	}
	if _, err := Build(context.Background(), Options{
		Dockerfile: []byte(df), Target: "production", Platform: v1.Platform{OS: "linux", Architecture: "arm64"},
		ContextDir: ctxDir, Artifacts: artifacts,
	}); err == nil || !strings.Contains(err.Error(), "linux/arm64") {
		t.Errorf("an amd64-only base for an arm64 image must fail naming the platform, got %v", err)
	}
}

func TestBuildArgOverridesFROM(t *testing.T) {
	df := "ARG BASE=alpine:3.21\nFROM ${BASE}\nUSER 1\n"
	if err := Check([]byte(df), "", map[string]string{"BASE": "ghcr.io/x/y:1"}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestStageEvaluation(t *testing.T) {
	cases := []struct {
		name    string
		df      string
		args    map[string]string
		wantErr string
		check   func(t *testing.T, c v1.Config)
	}{
		{
			name: "ENV and LABEL with expansion",
			df:   "FROM x\nARG V=1.2\nENV A=b C=\"d e\" VER=$V\nLABEL org.x=\"${A}-1\"\n",
			check: func(t *testing.T, c v1.Config) {
				if !reflect.DeepEqual(c.Env, []string{"A=b", "C=d e", "VER=1.2"}) {
					t.Errorf("Env = %v", c.Env)
				}
				if c.Labels["org.x"] != "b-1" {
					t.Errorf("Labels = %v", c.Labels)
				}
			},
		},
		{
			name: "build arg beats ARG default",
			df:   "FROM x\nARG V=1.2\nENV VER=$V\n",
			args: map[string]string{"V": "9"},
			check: func(t *testing.T, c v1.Config) {
				if !reflect.DeepEqual(c.Env, []string{"VER=9"}) {
					t.Errorf("Env = %v", c.Env)
				}
			},
		},
		{
			name: "shell-form ENTRYPOINT runs through /bin/sh -c",
			df:   "FROM x\nENTRYPOINT /app/run --flag\n",
			check: func(t *testing.T, c v1.Config) {
				if !reflect.DeepEqual(c.Entrypoint, []string{"/bin/sh", "-c", "/app/run --flag"}) {
					t.Errorf("Entrypoint = %v", c.Entrypoint)
				}
			},
		},
		{
			name: "exec-form is not variable-expanded",
			df:   "FROM x\nENV A=1\nCMD [\"echo\", \"$A\"]\n",
			check: func(t *testing.T, c v1.Config) {
				if !reflect.DeepEqual(c.Cmd, []string{"echo", "$A"}) {
					t.Errorf("Cmd = %v", c.Cmd)
				}
			},
		},
		{
			name: "EXPOSE range and protocol",
			df:   "FROM x\nEXPOSE 80 9000-9001/udp\n",
			check: func(t *testing.T, c v1.Config) {
				for _, k := range []string{"80/tcp", "9000/udp", "9001/udp"} {
					if _, ok := c.ExposedPorts[k]; !ok {
						t.Errorf("missing %s in %v", k, c.ExposedPorts)
					}
				}
			},
		},
		{name: "HEALTHCHECK NONE", df: "FROM x\nHEALTHCHECK NONE\n", check: func(t *testing.T, c v1.Config) {
			if c.Healthcheck == nil || !reflect.DeepEqual(c.Healthcheck.Test, []string{"NONE"}) {
				t.Errorf("Healthcheck = %+v", c.Healthcheck)
			}
		}},
		{name: "line continuation with interleaved comment", df: "FROM x\nENV A=1 \\\n  # comment\n  B=2\n", check: func(t *testing.T, c v1.Config) {
			if !reflect.DeepEqual(c.Env, []string{"A=1", "B=2"}) {
				t.Errorf("Env = %v", c.Env)
			}
		}},
		{name: "RUN that executes code", df: "FROM x\nRUN make all\n", wantErr: "line 2"},
		{name: "RUN apk add something else", df: "FROM x\nRUN apk add curl\n", wantErr: "line 2"},
		{name: "ADD", df: "FROM x\nADD a /b\n", wantErr: "ADD is not supported"},
		{name: "ONBUILD", df: "FROM x\nONBUILD RUN x\n", wantErr: "ONBUILD"},
		{name: "SHELL", df: "FROM x\nSHELL [\"/bin/bash\",\"-c\"]\n", wantErr: "SHELL"},
		{name: "VOLUME", df: "FROM x\nVOLUME /data\n", wantErr: "VOLUME"},
		{name: "heredoc", df: "FROM x\nCOPY <<EOF /a\nhi\nEOF\n", wantErr: "heredoc"},
		{name: "COPY --chown", df: "FROM x\nCOPY --chown=1:1 a /b\n", wantErr: "--chown"},
		{name: "COPY --from an image", df: "FROM x\nCOPY --from=busybox /bin/sh /sh\n", wantErr: "not a stage"},
		{name: "mount flag on RUN", df: "FROM x\nRUN --mount=type=cache,target=/c go build\n", wantErr: "RUN executes code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			df, err := parseDockerfile([]byte(tc.df))
			if err == nil {
				var st *stage
				if st, err = df.selectStage(""); err == nil {
					var ev *evaluated
					ev, err = evaluateStage(df, st, v1.Config{}, tc.args, df.globalVars(tc.args))
					if err == nil && tc.check != nil {
						tc.check(t, ev.config)
					}
				}
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var ue *UnsupportedError
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
			if !errors.As(err, &ue) {
				t.Errorf("err is %T, want *UnsupportedError so the caller can offer docker", err)
			}
		})
	}
}

func TestCheckRejectsWhatOnlyADaemonCanBuild(t *testing.T) {
	artifacts := map[string]map[string]string{"builder": {"/app/bin/app": "/x"}}
	if err := Check([]byte(scaffolded), "production", nil, map[string]map[string]string{"builder": {
		"/app/bin/app": "/x", "/app/bin/worker": "/y",
	}}); err != nil {
		t.Fatalf("scaffolded production stage must be accepted: %v", err)
	}
	// A COPY --from path the host build did not produce.
	err := Check([]byte(scaffolded), "production", nil, artifacts)
	if err == nil || !strings.Contains(err.Error(), "/app/bin/worker") {
		t.Fatalf("err = %v, want it to name the file the host build does not produce", err)
	}
	// The default target is the LAST stage, as in docker build.
	if err := Check([]byte("FROM a\nRUN make\nFROM b\nUSER 1\n"), "", nil, nil); err != nil {
		t.Errorf("last stage is clean, earlier stages are not built: %v", err)
	}
	if err := Check([]byte("FROM a\nUSER 1\nFROM b\nRUN make\n"), "", nil, nil); err == nil {
		t.Error("last stage runs code and must be refused")
	}
}

func TestCARequirementIsCheckedAgainstBase(t *testing.T) {
	// A base with no CA bundle: `apk add ca-certificates` cannot be satisfied.
	bare, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{OS: "linux", Architecture: "amd64", RootFS: v1.RootFS{Type: "layers"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Build(context.Background(), Options{
		Dockerfile: []byte("FROM scratchy\nRUN apk --no-cache add ca-certificates\n"),
		Platform:   linuxAMD64, Base: bare,
	})
	if err == nil || !strings.Contains(err.Error(), "ca-certificates") {
		t.Fatalf("err = %v, want a refusal naming ca-certificates", err)
	}
}

func TestContextCopyHonoursDockerignore(t *testing.T) {
	ctxDir := t.TempDir()
	for p, body := range map[string]string{
		"db/migrations/001.sql": "create table a();",
		"db/migrations/skip.md": "ignored",
		"db/keep.txt":           "k",
	} {
		full := filepath.Join(ctxDir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ctxDir, ".dockerignore"), []byte("**/*.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Build(context.Background(), Options{
		Dockerfile: []byte("FROM x\nWORKDIR /app\nCOPY db/migrations ./db/migrations\n"),
		Platform:   linuxAMD64, ContextDir: ctxDir, Base: alpineLike(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Cleanup()
	layers, _ := res.Image.Layers()
	rc, _ := layers[len(layers)-1].Uncompressed()
	defer rc.Close()
	tr := tar.NewReader(rc)
	var got []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		got = append(got, h.Name)
	}
	want := []string{"app/db/", "app/db/migrations/", "app/db/migrations/001.sql"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("entries = %v, want %v", got, want)
	}
}

// A COPY that fails after an earlier COPY already wrote its layer must not
// leave that layer's temp file behind.
func TestFailedBuildLeavesNoTempLayers(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	ctxDir, artifacts := projectWithBinaries(t)
	df := "FROM golang AS builder\nRUN true\nFROM alpine\nCOPY --from=builder /app/bin/app /app\nCOPY nope /nope\n"
	if _, err := Build(context.Background(), Options{
		Dockerfile: []byte(df), Platform: linuxAMD64, ContextDir: ctxDir, Artifacts: artifacts, Base: alpineLike(t),
	}); err == nil {
		t.Fatal("the second COPY names a missing file")
	}
	left, _ := filepath.Glob(filepath.Join(tmp, "forge-layer-*"))
	if len(left) != 0 {
		t.Errorf("temp layers left behind: %v", left)
	}
}

func TestMissingContextFileIsAnError(t *testing.T) {
	_, err := Build(context.Background(), Options{
		Dockerfile: []byte("FROM x\nCOPY nope /nope\n"), Platform: linuxAMD64, ContextDir: t.TempDir(), Base: alpineLike(t),
	})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
}
