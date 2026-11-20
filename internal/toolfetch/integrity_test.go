package toolfetch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installed installs the fake tool into a fresh cache and returns the
// fetcher, the release server and the installed entry.
func installed(t *testing.T) (*Fetcher, *release, Tool, Installed) {
	t.Helper()
	archive := tarGz(t, "faketool", fakeBinary)
	tool := fakeTool(archive, nil)
	rel := newRelease(t)
	rel.serve(tool, "linux", "amd64", archive)
	f := rel.fetcher(t, "linux")
	in, err := f.Ensure(context.Background(), tool)
	if err != nil {
		t.Fatal(err)
	}
	return f, rel, tool, in
}

// assertRefusedThenReinstalled: Find refuses the tampered entry, and Ensure
// downloads again and serves a clean, trusted entry at the same path.
func assertRefusedThenReinstalled(t *testing.T, f *Fetcher, rel *release, tool Tool, before Installed) {
	t.Helper()
	if _, ok := f.Find(tool); ok {
		t.Fatal("Find trusted a tampered entry")
	}
	fixed, err := f.Ensure(context.Background(), tool)
	if err != nil {
		t.Fatal(err)
	}
	if rel.hits.Load() != 2 {
		t.Fatalf("Ensure made %d requests; want a reinstall (2)", rel.hits.Load())
	}
	got, err := os.ReadFile(fixed.Path)
	if err != nil || string(got) != fakeBinary || fixed.Source != SourceRelease || fixed.Path != before.Path {
		t.Fatalf("repair = %q, %+v, %v", got, fixed, err)
	}
	info, err := os.Lstat(fixed.Path)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("repaired binary is not a regular file: %v, %v", info, err)
	}
	if hostEnforcesModes() && info.Mode().Perm() != 0o555 {
		t.Errorf("repaired binary mode %v, want 0555", info.Mode().Perm())
	}
	if hostEnforcesModes() {
		if di, _ := os.Lstat(filepath.Dir(fixed.Path)); di.Mode().Perm()&0o022 != 0 {
			t.Errorf("repaired entry mode %v is writable by others", di.Mode().Perm())
		}
	}
}

func TestCacheRejectsSubstitution(t *testing.T) {
	t.Parallel()
	t.Run("binary bytes", func(t *testing.T) {
		t.Parallel()
		f, rel, tool, in := installed(t)
		if err := os.Remove(in.Path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(in.Path, []byte("substituted"), 0o555); err != nil {
			t.Fatal(err)
		}
		assertRefusedThenReinstalled(t, f, rel, tool, in)
	})
	t.Run("source", func(t *testing.T) {
		t.Parallel()
		f, rel, tool, in := installed(t)
		in.Source = SourceGoInstall
		in.GoPackage = tool.GoPackage + "@" + tool.Version
		if err := writeManifest(filepath.Dir(in.Path), in); err != nil {
			t.Fatal(err)
		}
		assertRefusedThenReinstalled(t, f, rel, tool, in)
	})
	t.Run("manifest without binary hash", func(t *testing.T) {
		t.Parallel()
		f, rel, tool, in := installed(t)
		in.BinarySHA256 = ""
		if err := writeManifest(filepath.Dir(in.Path), in); err != nil {
			t.Fatal(err)
		}
		assertRefusedThenReinstalled(t, f, rel, tool, in)
	})
}

// The threat model: a cache entry another user could write is refused and
// reinstalled, and so is a link anywhere in it.
func TestCacheRefusesEntriesOthersCouldWrite(t *testing.T) {
	t.Parallel()
	t.Run("group-writable entry", func(t *testing.T) {
		t.Parallel()
		if !hostEnforcesModes() {
			t.Skip("Windows protects the cache by ACL, not mode bits")
		}
		f, rel, tool, in := installed(t)
		if err := os.Chmod(filepath.Dir(in.Path), 0o775); err != nil {
			t.Fatal(err)
		}
		assertRefusedThenReinstalled(t, f, rel, tool, in)
	})
	t.Run("group-writable binary", func(t *testing.T) {
		t.Parallel()
		if !hostEnforcesModes() {
			t.Skip("Windows protects the cache by ACL, not mode bits")
		}
		f, rel, tool, in := installed(t)
		if err := os.Chmod(in.Path, 0o575); err != nil {
			t.Fatal(err)
		}
		assertRefusedThenReinstalled(t, f, rel, tool, in)
	})
	t.Run("owner-writable binary", func(t *testing.T) {
		t.Parallel()
		if !hostEnforcesModes() {
			t.Skip("Windows keeps 0755")
		}
		f, rel, tool, in := installed(t)
		if err := os.Chmod(in.Path, 0o755); err != nil {
			t.Fatal(err)
		}
		assertRefusedThenReinstalled(t, f, rel, tool, in)
	})
	t.Run("symlinked binary", func(t *testing.T) {
		t.Parallel()
		f, rel, tool, in := installed(t)
		// The link points at identical, correctly-moded bytes: only the
		// link itself is wrong, because its target can be anywhere.
		elsewhere := filepath.Join(t.TempDir(), "faketool")
		if err := os.WriteFile(elsewhere, []byte(fakeBinary), 0o555); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(in.Path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, in.Path); err != nil {
			t.Skipf("cannot create a symlink here: %v", err)
		}
		assertRefusedThenReinstalled(t, f, rel, tool, in)
	})
	t.Run("symlinked entry", func(t *testing.T) {
		t.Parallel()
		f, rel, tool, in := installed(t)
		entry := filepath.Dir(in.Path)
		moved := filepath.Join(t.TempDir(), "entry")
		if err := os.Rename(entry, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(moved, entry); err != nil {
			t.Skipf("cannot create a symlink here: %v", err)
		}
		assertRefusedThenReinstalled(t, f, rel, tool, in)
	})
}

// A cache that belongs to another user is refused outright: nothing under it
// is trusted, and nothing is installed into it.
func TestCacheRefusesAForeignOwner(t *testing.T) {
	t.Parallel()
	if !hostEnforcesModes() {
		t.Skip("Windows protects the cache by ACL; forge does not read owners there")
	}
	f, rel, tool, _ := installed(t)
	// Creating files owned by another uid needs root; instead this Fetcher
	// is another user looking at the same cache.
	other := *f
	other.owner = f.owner + 1
	if _, ok := other.Find(tool); ok {
		t.Fatal("Find trusted a cache owned by another user")
	}
	_, err := other.Ensure(context.Background(), tool)
	if !errors.Is(err, ErrUntrustedCache) || !strings.Contains(err.Error(), "owned by uid") {
		t.Fatalf("err = %v, want ErrUntrustedCache naming the owner", err)
	}
	if rel.hits.Load() != 1 {
		t.Error("an untrusted cache was installed into")
	}
}

// Every directory above the cache matters: one others can write lets them
// rename the cache away. A sticky world-writable directory (/tmp) does not.
func TestCacheRefusesAWritableAncestor(t *testing.T) {
	t.Parallel()
	if !hostEnforcesModes() {
		t.Skip("Windows protects the cache by ACL, not mode bits")
	}
	archive := tarGz(t, "faketool", fakeBinary)
	tool := fakeTool(archive, nil)
	rel := newRelease(t)
	rel.serve(tool, "linux", "amd64", archive)
	for _, c := range []struct {
		mode os.FileMode
		ok   bool
	}{{0o777, false}, {0o777 | os.ModeSticky, true}, {0o755, true}} {
		parent := t.TempDir()
		if err := os.Chmod(parent, c.mode); err != nil {
			t.Fatal(err)
		}
		f := rel.fetcher(t, "linux")
		f.CacheDir = filepath.Join(parent, "release")
		_, err := f.Ensure(context.Background(), tool)
		if c.ok && err != nil {
			t.Errorf("parent %v: %v", c.mode, err)
		}
		if !c.ok && (!errors.Is(err, ErrUntrustedCache) || !strings.Contains(err.Error(), parent)) {
			t.Errorf("parent %v: err = %v, want ErrUntrustedCache naming %s", c.mode, err, parent)
		}
		_ = os.Chmod(parent, 0o755)
	}
}

// The cache path may itself be reached through a link (macOS /var, a
// relocated home): it is resolved once, then checked.
func TestCacheRootThroughASymlink(t *testing.T) {
	t.Parallel()
	archive := tarGz(t, "faketool", fakeBinary)
	tool := fakeTool(archive, nil)
	rel := newRelease(t)
	rel.serve(tool, "linux", "amd64", archive)
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "cache")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	f := rel.fetcher(t, "linux")
	f.CacheDir = link
	in, err := f.Ensure(context.Background(), tool)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := filepath.EvalSymlinks(real); !strings.HasPrefix(in.Path, want) {
		t.Errorf("path %s is not under the resolved cache %s", in.Path, want)
	}
}

func TestUnsafeMetadataRejectedBeforeIO(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"name", "version", "os", "arch", "asset"} {
		for _, bad := range []string{"..", "../escape", `..\escape`, "/absolute", `C:drive`, "trailing.", "nul\x00"} {
			t.Run(field+"/"+bad, func(t *testing.T) {
				t.Parallel()
				root := t.TempDir()
				f := testFetcher(filepath.Join(root, "cache"), "", "linux")
				f.LookPath = func(string) (string, error) { t.Fatal("unexpected IO"); return "", nil }
				tool := Tool{Name: "faketool", Version: "v1", Checksums: map[string]string{}}
				switch field {
				case "name":
					tool.Name = bad
				case "version":
					tool.Version = bad
				case "os":
					f.GOOS = bad
				case "arch":
					f.GOARCH = bad
				case "asset":
					tool.Checksums[bad] = "bad"
				}
				if _, err := f.Ensure(context.Background(), tool); err == nil {
					t.Fatal("Ensure accepted unsafe metadata")
				}
				if _, ok := f.Find(tool); ok {
					t.Fatal("Find accepted unsafe metadata")
				}
				entries, err := os.ReadDir(root)
				if err != nil || len(entries) != 0 {
					t.Fatalf("cache touched: %v, %v", entries, err)
				}
			})
		}
	}
}
