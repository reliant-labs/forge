package baseimage

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestMirrorPath_OfficialVsNamespaced(t *testing.T) {
	cases := []struct{ tag, want string }{
		{"alpine:3.21", "library/alpine:3.21"},
		{"golang:1.26-alpine", "library/golang:1.26-alpine"},
		{"ubuntu:24.04", "library/ubuntu:24.04"},
		{"docker/dockerfile:1", "docker/dockerfile:1"},
		{"docker/dockerfile:1.7", "docker/dockerfile:1.7"},
		{"ghcr-owner/repo/img:tag", "ghcr-owner/repo/img:tag"},
	}
	for _, c := range cases {
		if got := MirrorPath(c.tag); got != c.want {
			t.Errorf("MirrorPath(%q): got %q, want %q", c.tag, got, c.want)
		}
	}
}

func TestMirrorRef_DropsTagKeepsDigest(t *testing.T) {
	prefix := "us-docker.pkg.dev/p/dockerhub"
	got := MirrorRef(prefix, "alpine:3.21", "sha256:abc")
	want := "us-docker.pkg.dev/p/dockerhub/library/alpine@sha256:abc"
	if got != want {
		t.Errorf("MirrorRef: got %q, want %q", got, want)
	}
	// Namespaced (frontend) ref keeps its namespace, drops the tag.
	got = MirrorRef(prefix, "docker/dockerfile:1", "sha256:def")
	want = "us-docker.pkg.dev/p/dockerhub/docker/dockerfile@sha256:def"
	if got != want {
		t.Errorf("MirrorRef namespaced: got %q, want %q", got, want)
	}
	// Trailing slash on the prefix is normalized.
	if got := MirrorRef(prefix+"/", "alpine:3.21", "sha256:abc"); got != "us-docker.pkg.dev/p/dockerhub/library/alpine@sha256:abc" {
		t.Errorf("MirrorRef trailing slash: got %q", got)
	}
}

func TestMirrorTagRef(t *testing.T) {
	if got := MirrorTagRef("us-docker.pkg.dev/p/dockerhub", "golang:1.26-alpine"); got != "us-docker.pkg.dev/p/dockerhub/library/golang:1.26-alpine" {
		t.Errorf("MirrorTagRef: got %q", got)
	}
}

func TestSlug_DistinctPerTag(t *testing.T) {
	cases := []struct{ tag, want string }{
		{"alpine:3.21", "ALPINE_3_21"},
		{"golang:1.26-alpine", "GOLANG_1_26_ALPINE"},
		{"golang:1.24-bookworm", "GOLANG_1_24_BOOKWORM"},
		{"node:22-bookworm-slim", "NODE_22_BOOKWORM_SLIM"},
		{"docker/dockerfile:1", "DOCKER_DOCKERFILE_1"},
		{"rust:1.86-bookworm", "RUST_1_86_BOOKWORM"},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		got := Slug(c.tag)
		if got != c.want {
			t.Errorf("Slug(%q): got %q, want %q", c.tag, got, c.want)
		}
		if seen[got] {
			t.Errorf("Slug collision: %q produced already-seen %q", c.tag, got)
		}
		seen[got] = true
	}
}

func TestSlug_LeadingDigitGetsUnderscore(t *testing.T) {
	if got := Slug("3.21"); got != "_3_21" {
		t.Errorf("Slug leading digit: got %q, want _3_21", got)
	}
}

func TestBuildArgName(t *testing.T) {
	if got := BuildArgName("alpine:3.21"); got != "BASE_ALPINE_3_21" {
		t.Errorf("BuildArgName: got %q", got)
	}
}

func TestDeclared_Validate(t *testing.T) {
	cases := []struct {
		name    string
		d       Declared
		wantErr string // substring; "" means no error
	}{
		{"empty is off", Declared{}, ""},
		{"valid", Declared{MirrorPrefix: "us-docker.pkg.dev/p/dh", Tags: []string{"alpine:3.21", "node:22-alpine"}}, ""},
		{"missing mirror", Declared{Tags: []string{"alpine:3.21"}}, "mirror_prefix is required"},
		{"mirror is url", Declared{MirrorPrefix: "https://x/y", Tags: []string{"alpine:3.21"}}, "must be a bare registry"},
		{"tag no version", Declared{MirrorPrefix: "x/y", Tags: []string{"alpine"}}, "must include an explicit"},
		{"tag digest-pinned", Declared{MirrorPrefix: "x/y", Tags: []string{"alpine@sha256:abc"}}, "must not be digest-pinned"},
		{"duplicate tag", Declared{MirrorPrefix: "x/y", Tags: []string{"alpine:3.21", "alpine:3.21"}}, "duplicate tag"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.d.Validate()
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("want no error, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

// fakeResolver returns a canned digest per ref and records which refs it was
// asked about, so the test can assert resolution goes THROUGH the mirror.
type fakeResolver struct {
	digests map[string]string
	asked   []string
	err     error
}

func (f *fakeResolver) Resolve(_ context.Context, ref string) (string, error) {
	f.asked = append(f.asked, ref)
	if f.err != nil {
		return "", f.err
	}
	d, ok := f.digests[ref]
	if !ok {
		return "", errors.New("no such ref in fake")
	}
	return d, nil
}

func TestRepin_ResolvesThroughMirrorAndBuildsLock(t *testing.T) {
	prefix := "us-docker.pkg.dev/p/dockerhub"
	d := Declared{
		MirrorPrefix: prefix,
		Tags:         []string{"node:22-alpine", "alpine:3.21", "docker/dockerfile:1"},
	}
	fr := &fakeResolver{digests: map[string]string{
		prefix + "/library/alpine:3.21":    "sha256:aaa",
		prefix + "/library/node:22-alpine": "sha256:bbb",
		prefix + "/docker/dockerfile:1":    "sha256:ccc",
	}}
	lk, err := Repin(context.Background(), d, fr)
	if err != nil {
		t.Fatalf("Repin: %v", err)
	}

	// Every resolution must have gone through the mirror prefix — never a
	// bare Docker Hub ref. This is the rate-limit-dodge invariant.
	for _, ref := range fr.asked {
		if !strings.HasPrefix(ref, prefix+"/") {
			t.Errorf("resolved ref %q did NOT go through mirror %q", ref, prefix)
		}
	}

	// Lock entries are tag-sorted and digest-pinned.
	if len(lk.Entries) != 3 {
		t.Fatalf("entries: got %d, want 3", len(lk.Entries))
	}
	e, ok := lk.Entry("alpine:3.21")
	if !ok {
		t.Fatal("missing alpine entry")
	}
	if e.Ref != prefix+"/library/alpine@sha256:aaa" {
		t.Errorf("alpine ref: got %q", e.Ref)
	}
	if e.Arg != "BASE_ALPINE_3_21" {
		t.Errorf("alpine arg: got %q", e.Arg)
	}

	// BuildArgs are KEY=VALUE in tag order.
	args := lk.BuildArgs()
	if len(args) != 3 {
		t.Fatalf("build args: got %d, want 3", len(args))
	}
	if !strings.HasPrefix(args[0], "BASE_ALPINE_3_21=") {
		t.Errorf("first build arg (tag-sorted) should be alpine, got %q", args[0])
	}
}

func TestRepin_FailFastOnUnresolvable(t *testing.T) {
	d := Declared{MirrorPrefix: "m/p", Tags: []string{"alpine:3.21"}}
	fr := &fakeResolver{err: errors.New("registry boom")}
	_, err := Repin(context.Background(), d, fr)
	if err == nil || !strings.Contains(err.Error(), "alpine:3.21") {
		t.Fatalf("want fail-fast naming the tag, got %v", err)
	}
}

func TestLock_RoundTripAndStaleness(t *testing.T) {
	dir := t.TempDir()
	prefix := "m/p"
	d := Declared{MirrorPrefix: prefix, Tags: []string{"alpine:3.21", "node:22-alpine"}}
	fr := &fakeResolver{digests: map[string]string{
		prefix + "/library/alpine:3.21":    "sha256:a",
		prefix + "/library/node:22-alpine": "sha256:n",
	}}
	lk, err := Repin(context.Background(), d, fr)
	if err != nil {
		t.Fatal(err)
	}
	path, err := WriteLock(dir, lk)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "base-images.lock.json" {
		t.Errorf("lock path: got %q", path)
	}

	got, err := ReadLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got.Entries) != 2 || got.MirrorPrefix != prefix {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// Lock matches the declared set it was pinned from.
	if !got.LockMatchesDeclared(d) {
		t.Error("LockMatchesDeclared: want true for the set it was pinned from")
	}
	// Adding a tag makes it stale.
	d2 := Declared{MirrorPrefix: prefix, Tags: append(d.Tags, "ubuntu:24.04")}
	if got.LockMatchesDeclared(d2) {
		t.Error("LockMatchesDeclared: want false after a tag was added")
	}
	// Changing the mirror makes it stale.
	d3 := Declared{MirrorPrefix: "other/mirror", Tags: d.Tags}
	if got.LockMatchesDeclared(d3) {
		t.Error("LockMatchesDeclared: want false after mirror changed")
	}
}

func TestReadLock_MissingIsNil(t *testing.T) {
	lk, err := ReadLock(t.TempDir())
	if err != nil {
		t.Fatalf("missing lock should be (nil,nil), got err %v", err)
	}
	if lk != nil {
		t.Fatalf("missing lock should be nil, got %+v", lk)
	}
}

func TestLock_BuildArgsNilSafe(t *testing.T) {
	var lk *Lock
	if lk.BuildArgs() != nil {
		t.Error("nil lock BuildArgs should be nil")
	}
	if !lk.LockMatchesDeclared(Declared{}) {
		t.Error("nil lock vs empty declared should match (both off)")
	}
}
