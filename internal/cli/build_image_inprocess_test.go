package cli

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/imagebuild"
)

// scaffoldedDockerfile is a trimmed forge Dockerfile.tmpl render: the builder
// and debug stages are what forge's host build replaces, the production stage
// is what the in-process builder assembles.
const scaffoldedDockerfile = `# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS builder
ARG FORGE_VERSION=dev
WORKDIR /app
COPY . .
RUN CGO_ENABLED=0 go build -o /app/bin/demo ./cmd/demo

FROM alpine:3.21 AS production
RUN apk --no-cache add ca-certificates
WORKDIR /app
COPY --from=builder /app/bin/demo ./demo
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/app/demo"]
CMD ["server"]
`

func TestValidateImageBuilder(t *testing.T) {
	for _, ok := range []string{"", "auto", "docker", "in-process"} {
		if err := validateImageBuilder(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	err := validateImageBuilder("buildah")
	if err == nil || !strings.Contains(err.Error(), "in-process") {
		t.Errorf("err = %v, want one naming the valid values", err)
	}
}

func stubDaemon(t *testing.T, err error) {
	t.Helper()
	prev := dockerDaemonReachable
	dockerDaemonReachable = func(context.Context) error { return err }
	t.Cleanup(func() { dockerDaemonReachable = prev })
}

func TestChooseImageBuilder(t *testing.T) {
	noDaemon := errors.New("cannot reach the Docker daemon: boom\n  hint: start it")
	supported := func() error { return nil }
	unsupported := func() error { return &imagebuild.UnsupportedError{Line: 9, Stage: "production", What: "RUN make"} }

	cases := []struct {
		name    string
		mode    string
		daemon  error
		check   func() error
		wantUse bool
		wantErr string
	}{
		{"auto with a daemon keeps docker", "auto", nil, supported, false, ""},
		{"default is auto", "", nil, supported, false, ""},
		{"auto without a daemon goes in-process", "auto", noDaemon, supported, true, ""},
		{"auto without a daemon and a custom Dockerfile names both ways out", "auto", noDaemon, unsupported, false, "Docker daemon"},
		{"docker never goes in-process", "docker", noDaemon, supported, false, ""},
		{"in-process forced even with a daemon", "in-process", nil, supported, true, ""},
		{"in-process forced on a custom Dockerfile fails with the line", "in-process", nil, unsupported, false, "line 9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubDaemon(t, tc.daemon)
			p := chooseImageBuilder(context.Background(), tc.mode, "Dockerfile", tc.check)
			if tc.wantErr != "" {
				if p.err == nil || !strings.Contains(p.err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", p.err, tc.wantErr)
				}
				return
			}
			if p.err != nil {
				t.Fatal(p.err)
			}
			if p.use != tc.wantUse {
				t.Errorf("use = %v, want %v", p.use, tc.wantUse)
			}
		})
	}
}

// The error for a Dockerfile only a daemon can build must name BOTH remedies
// and the first offending line, so an author in a sandbox and one on a laptop
// each see their way out.
func TestNoDaemonErrorNamesBothRemedies(t *testing.T) {
	stubDaemon(t, errors.New("cannot reach the Docker daemon"))
	p := chooseImageBuilder(context.Background(), "auto", "Dockerfile", func() error {
		return &imagebuild.UnsupportedError{Line: 12, Stage: "production", What: "RUN make"}
	})
	if p.err == nil {
		t.Fatal("expected an error")
	}
	msg := p.err.Error()
	for _, want := range []string{"Docker daemon", "line 12", "RUN make", "DOCKER_HOST", "scaffold"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not mention %q:\n%s", want, msg)
		}
	}
}

func TestProjectImageArtifacts(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"demo", "worker"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// "api-server" is a workload whose output name differs from its cmd dir:
	// the scaffolded Dockerfile compiles it to /app/bin/api, the host build
	// writes bin/api-server, and both spellings must resolve to that file.
	if err := os.WriteFile(filepath.Join(dir, "api-server"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	targets := []goBuildTarget{
		{cmd: "./cmd/demo", outputName: "demo"},
		{cmd: "./cmd/worker", outputName: "worker"},
		{cmd: "./cmd/api", outputName: "api-server"},
		{cmd: "./cmd/missing", outputName: "missing"},
	}
	got := projectImageArtifacts([]byte(scaffoldedDockerfile), dir, projectImageBinaries(targets))
	want := map[string]map[string]string{"builder": {
		"/app/bin/demo":       filepath.Join(dir, "demo"),
		"/app/bin/worker":     filepath.Join(dir, "worker"),
		"/app/bin/api":        filepath.Join(dir, "api-server"),
		"/app/bin/api-server": filepath.Join(dir, "api-server"),
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("artifacts = %v, want %v", got, want)
	}
}

func TestBuilderStageNames(t *testing.T) {
	got := builderStageNames([]byte("FROM a AS builder\nFROM b as debug\nFROM c AS production\n"))
	if !reflect.DeepEqual(got, []string{"builder", "debug"}) {
		t.Errorf("stages = %v", got)
	}
}

// projectImageInProcess is exercised end to end against an in-memory registry:
// base image pulled from it, binaries layered on, result pushed to it, and the
// buildResult carries the digest the build state / release ledger record.
func TestProjectImageInProcessEndToEnd(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	// A base the Dockerfile's FROM resolves to.
	base := fixtureBase(t)
	if err := imagebuild.Push(context.Background(), base, []string{host + "/library/alpine:3.21"}, nil); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	t.Chdir(dir)
	df := strings.ReplaceAll(scaffoldedDockerfile, "alpine:3.21", host+"/library/alpine:3.21")
	if err := os.WriteFile("Dockerfile", []byte(df), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll("bin", 0o755); err != nil {
		t.Fatal(err)
	}
	writeELF(t, filepath.Join("bin", "demo"), "amd64")

	stubDaemon(t, errors.New("cannot reach the Docker daemon"))
	tags := imageTagSet(host+"/acme/demo", "v1.2.3", true, true)
	guard := projectImageGuard{outputDir: "bin", binaryNames: []string{"demo"}, binaries: map[string]string{"demo": "demo"}, envLabel: "prod"}

	res, handled := projectImageInProcess(context.Background(), &config.ProjectConfig{Name: "demo"}, tags, "amd64", versionInfo{version: "v1.2.3"}, guard)
	if !handled {
		t.Fatal("no daemon and a scaffolded Dockerfile: the in-process builder must handle it")
	}
	if res.err != nil {
		t.Fatalf("build: %v", res.err)
	}
	if res.name != "demo (docker)" || res.kind != "docker" || res.image != "demo" || res.repository != host+"/acme/demo" || res.tag != "v1.2.3" {
		t.Errorf("result identity = %+v", res)
	}
	if !strings.HasPrefix(res.digest, "sha256:") || !reflect.DeepEqual(res.platforms, []string{"linux/amd64"}) {
		t.Errorf("digest/platforms = %q %v", res.digest, res.platforms)
	}

	ref, err := name.ParseReference(host + "/acme/demo:v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	desc, err := remote.Head(ref)
	if err != nil {
		t.Fatalf("image not in the registry: %v", err)
	}
	if desc.Digest.String() != res.digest {
		t.Errorf("registry holds %s, build recorded %s", desc.Digest, res.digest)
	}
	img, err := remote.Image(ref)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cf.Config.Entrypoint, []string{"/app/demo"}) || cf.Config.User != "65532:65532" {
		t.Errorf("config = %+v", cf.Config)
	}
}

func TestProjectImageInProcessDefersToDockerWithADaemon(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("Dockerfile", []byte(scaffoldedDockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	stubDaemon(t, nil)
	_, handled := projectImageInProcess(context.Background(), &config.ProjectConfig{Name: "demo"},
		imageTagSet("registry.example/acme/demo", "v1", true, true), "amd64", versionInfo{},
		projectImageGuard{outputDir: "bin", binaryNames: []string{"demo"}, binaries: map[string]string{"demo": "demo"}})
	if handled {
		t.Error("with a reachable daemon, auto must leave the build to docker")
	}
}

func TestProjectImageInProcessRefusesWhenNothingIsPushed(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("Dockerfile", []byte(scaffoldedDockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll("bin", 0o755); err != nil {
		t.Fatal(err)
	}
	writeELF(t, filepath.Join("bin", "demo"), "amd64")
	stubDaemon(t, errors.New("cannot reach the Docker daemon"))
	res, handled := projectImageInProcess(context.Background(), &config.ProjectConfig{Name: "demo"},
		imageTagSet("demo", "v1", false, false), "amd64", versionInfo{},
		projectImageGuard{outputDir: "bin", binaryNames: []string{"demo"}, binaries: map[string]string{"demo": "demo"}})
	if !handled || res.err == nil || !strings.Contains(res.err.Error(), "--push") {
		t.Fatalf("handled=%v err=%v: an image with no daemon and no registry has nowhere to live", handled, res.err)
	}
}

func TestProjectImageInProcessRejectsAWrongArchBinary(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("Dockerfile", []byte(scaffoldedDockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll("bin", 0o755); err != nil {
		t.Fatal(err)
	}
	writeELF(t, filepath.Join("bin", "demo"), "arm64")
	stubDaemon(t, errors.New("cannot reach the Docker daemon"))
	res, handled := projectImageInProcess(context.Background(), &config.ProjectConfig{Name: "demo"},
		imageTagSet("registry.example/acme/demo", "v1", true, true), "amd64", versionInfo{},
		projectImageGuard{outputDir: "bin", binaryNames: []string{"demo"}, binaries: map[string]string{"demo": "demo"}, envLabel: "prod"})
	if !handled || res.err == nil || !strings.Contains(res.err.Error(), "linux/amd64") {
		t.Fatalf("handled=%v err=%v: the same arch guard the docker path runs must run here", handled, res.err)
	}
}

func TestRequireDaemonForDockerOnlyImage(t *testing.T) {
	stubDaemon(t, nil)
	if err := requireDaemonForDockerOnlyImage(context.Background(), "service gw", "gw/Dockerfile"); err != nil {
		t.Fatalf("with a daemon: %v", err)
	}
	stubDaemon(t, errors.New("cannot reach the Docker daemon: unix:///nonexistent"))
	err := requireDaemonForDockerOnlyImage(context.Background(), "service gw", "gw/Dockerfile")
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

var _ = v1.Platform{}
