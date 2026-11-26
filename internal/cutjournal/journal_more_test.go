package cutjournal

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/errdef"
)

func TestImageRef(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		name string
		img  Image
		want string
	}{
		{"tagged", Image{Repository: "reg/api", Tag: "v1", Digest: digest}, "reg/api:v1@" + digest},
		{"reused under another tag has no tag", Image{Repository: "reg/api", Digest: digest}, "reg/api@" + digest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.img.Ref(); got != tt.want {
				t.Errorf("Ref = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPushedUnder(t *testing.T) {
	j := sampleJournal()
	if !j.PushedUnder("v1.2.3-rc.1") {
		t.Error("the journal pushed images under its own version")
	}
	if j.PushedUnder("v9") {
		t.Error("no image was pushed under v9")
	}
	for i := range j.Images {
		j.Images[i].Tag = ""
	}
	if j.PushedUnder("v1.2.3-rc.1") {
		t.Error("reused images carry no tag, so a retry cannot collide with them")
	}
}

func TestEncodeRefusesAnInvalidJournal(t *testing.T) {
	bad := sampleJournal()
	bad.Commit = ""
	if _, err := Encode(bad); err == nil || !strings.Contains(err.Error(), "version, env and commit are required") {
		t.Errorf("Encode without a commit = %v", err)
	}
	noImages := sampleJournal()
	noImages.Images = nil
	if _, err := Encode(noImages); err == nil || !strings.Contains(err.Error(), "records no image") {
		t.Errorf("Encode without images = %v", err)
	}
	badDigest := sampleJournal()
	badDigest.Images[0].Digest = "md5:abc"
	if _, err := Encode(badDigest); err == nil || !strings.Contains(err.Error(), "incomplete image") {
		t.Errorf("Encode with a non-sha256 digest = %v", err)
	}
}

func TestEncodeStampsTheSchema(t *testing.T) {
	j := sampleJournal()
	j.Schema = "something-else"
	raw, err := Encode(j)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(raw)
	if err != nil {
		t.Fatalf("a journal Encode wrote must Decode: %v", err)
	}
	if got.Schema != Schema {
		t.Errorf("Schema = %q, want %q", got.Schema, Schema)
	}
}

func TestDecodeRefusals(t *testing.T) {
	good, err := Encode(sampleJournal())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		raw  []byte
		want string
	}{
		{"not json", []byte("{nope"), "cut journal:"},
		{"another schema", bytes.Replace(good, []byte(Schema), []byte("forge.cut/v0"), 1), `this forge reads`},
		{"a structurally invalid journal", bytes.Replace(good, []byte(`"env":"prod"`), []byte(`"env":""`), 1), "required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Decode(tt.raw); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Decode = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestMismatchesIsEmptyForTheSameCut(t *testing.T) {
	j := sampleJournal()
	want := Expect{Project: "shop", Env: "prod", Version: j.Version, ForgeVersion: j.ForgeVersion, Commit: j.Commit, Tree: j.Tree}
	if got := j.Mismatches(want); len(got) != 0 {
		t.Errorf("Mismatches = %v, want none", got)
	}
}

func TestPlainHTTPRegistry(t *testing.T) {
	tests := map[string]bool{
		"localhost:5000":       true,
		"localhost":            true,
		"reg.localhost:5001":   true,
		"127.0.0.1:5000":       true,
		"host.k3d.internal:80": true,
		"ghcr.io":              false,
		"registry.example:443": false,
		"notlocalhost:5000":    false,
	}
	for host, want := range tests {
		if got := plainHTTPRegistry(host); got != want {
			t.Errorf("plainHTTPRegistry(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestOpen(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())

	repo, err := Open("localhost:5000/team/app")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !repo.PlainHTTP {
		t.Error("a loopback registry is reached over plain HTTP")
	}
	if repo.Reference.Registry != "localhost:5000" || repo.Reference.Repository != "team/app" {
		t.Errorf("reference = %+v", repo.Reference)
	}
	if repo.Client == nil {
		t.Error("the repository must carry an authenticated client")
	}

	remote, err := Open("ghcr.io/acme/app")
	if err != nil {
		t.Fatal(err)
	}
	if remote.PlainHTTP {
		t.Error("a public registry must be reached over TLS")
	}

	if _, err := Open("not a repository"); err == nil || !strings.Contains(err.Error(), `cut journal repository "not a repository"`) {
		t.Errorf("Open of a bad name = %v", err)
	}
}

// faultyTarget wraps an in-memory store and fails the operation it is told to.
type faultyTarget struct {
	*memory.Store
	pushErr    func(desc ocispec.Descriptor) error
	tagErr     error
	resolveErr error
	fetchErr   error
	corrupt    bool // Fetch returns bytes that do not match their descriptor
}

func (f *faultyTarget) Push(ctx context.Context, d ocispec.Descriptor, r io.Reader) error {
	if f.pushErr != nil {
		if err := f.pushErr(d); err != nil {
			return err
		}
	}
	return f.Store.Push(ctx, d, r)
}

func (f *faultyTarget) Tag(ctx context.Context, d ocispec.Descriptor, ref string) error {
	if f.tagErr != nil {
		return f.tagErr
	}
	return f.Store.Tag(ctx, d, ref)
}

func (f *faultyTarget) Resolve(ctx context.Context, ref string) (ocispec.Descriptor, error) {
	if f.resolveErr != nil {
		return ocispec.Descriptor{}, f.resolveErr
	}
	return f.Store.Resolve(ctx, ref)
}

func (f *faultyTarget) Fetch(ctx context.Context, d ocispec.Descriptor) (io.ReadCloser, error) {
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	rc, err := f.Store.Fetch(ctx, d)
	if err != nil || !f.corrupt {
		return rc, err
	}
	_ = rc.Close()
	return io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("x"), int(d.Size)))), nil
}

func TestPush_Failures(t *testing.T) {
	ctx := context.Background()

	t.Run("an invalid journal never reaches the registry", func(t *testing.T) {
		bad := sampleJournal()
		bad.Images = nil
		ft := &faultyTarget{Store: memory.New(), pushErr: func(ocispec.Descriptor) error { t.Error("pushed an invalid journal"); return nil }}
		if _, err := Push(ctx, ft, "v1.forge-cut", bad); err == nil {
			t.Error("want a validation error")
		}
	})

	t.Run("a blob push failure is named by media type", func(t *testing.T) {
		ft := &faultyTarget{Store: memory.New(), pushErr: func(d ocispec.Descriptor) error {
			if d.MediaType == LayerMediaType {
				return errors.New("quota exceeded")
			}
			return nil
		}}
		_, err := Push(ctx, ft, "v1.forge-cut", sampleJournal())
		if err == nil || !strings.Contains(err.Error(), "push cut journal "+LayerMediaType) || !strings.Contains(err.Error(), "quota exceeded") {
			t.Errorf("Push = %v", err)
		}
		if _, rerr := ft.Store.Resolve(ctx, "v1.forge-cut"); rerr == nil {
			t.Error("a tag must never name an incomplete artifact")
		}
	})

	t.Run("blobs that already exist are not an error", func(t *testing.T) {
		ft := &faultyTarget{Store: memory.New()}
		first, err := Push(ctx, ft, "v1.forge-cut", sampleJournal())
		if err != nil {
			t.Fatal(err)
		}
		ft.pushErr = func(ocispec.Descriptor) error { return errdef.ErrAlreadyExists }
		again, err := Push(ctx, ft, "v1.forge-cut", sampleJournal())
		if err != nil || again != first {
			t.Errorf("Push over existing blobs = (%s, %v), want the same digest %s", again, err, first)
		}
	})

	t.Run("a tag failure is reported with the tag", func(t *testing.T) {
		ft := &faultyTarget{Store: memory.New(), tagErr: errors.New("denied")}
		_, err := Push(ctx, ft, "v1.forge-cut", sampleJournal())
		if err == nil || !strings.Contains(err.Error(), "tag cut journal v1.forge-cut") || !strings.Contains(err.Error(), "denied") {
			t.Errorf("Push = %v", err)
		}
	})
}

func TestFetch_Failures(t *testing.T) {
	ctx := context.Background()
	pushed := func(t *testing.T) *faultyTarget {
		t.Helper()
		ft := &faultyTarget{Store: memory.New()}
		if _, err := Push(ctx, ft, "v1.forge-cut", sampleJournal()); err != nil {
			t.Fatal(err)
		}
		return ft
	}

	t.Run("a registry error other than not-found is wrapped", func(t *testing.T) {
		ft := pushed(t)
		ft.resolveErr = errors.New("503 service unavailable")
		_, _, err := Fetch(ctx, ft, "v1.forge-cut")
		if err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "resolve cut journal v1.forge-cut") {
			t.Errorf("Fetch = %v, want a wrapped resolve failure that is not ErrNotFound", err)
		}
	})

	t.Run("an unreadable blob is wrapped", func(t *testing.T) {
		ft := pushed(t)
		ft.fetchErr = errors.New("connection reset")
		_, _, err := Fetch(ctx, ft, "v1.forge-cut")
		if err == nil || !strings.Contains(err.Error(), "read cut journal v1.forge-cut") || !strings.Contains(err.Error(), "connection reset") {
			t.Errorf("Fetch = %v", err)
		}
	})

	t.Run("bytes that do not match their digest are refused", func(t *testing.T) {
		ft := pushed(t)
		ft.corrupt = true
		_, _, err := Fetch(ctx, ft, "v1.forge-cut")
		if err == nil || !strings.Contains(err.Error(), "read cut journal v1.forge-cut") {
			t.Errorf("Fetch of tampered content = %v, want a verification failure", err)
		}
	})

	t.Run("a journal with a path escaping its directory is refused after the round trip", func(t *testing.T) {
		ft := &faultyTarget{Store: memory.New()}
		bad := sampleJournal()
		bad.Outputs = []File{{Name: "../../etc/passwd", Data: []byte("x")}}
		if _, err := Push(ctx, ft, "v3.forge-cut", bad); err == nil {
			t.Fatal("Push must not write a journal that Decode would refuse")
		}
	})

	t.Run("an oversized descriptor is refused before it is fetched", func(t *testing.T) {
		_, err := readVerified(ctx, memory.New(), ocispec.Descriptor{Digest: digestOf([]byte("x")), Size: maxJournalBytes + 1})
		if err == nil || !strings.Contains(err.Error(), "over the") {
			t.Errorf("readVerified = %v, want an oversize refusal", err)
		}
	})
}

func digestOf(b []byte) digest.Digest { return digest.FromBytes(b) }
