package sbom

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDefaultCacheDir(t *testing.T) {
	t.Run("the environment variable wins and is trimmed", func(t *testing.T) {
		t.Setenv(CacheDirEnv, "  /srv/sbom-cache \n")
		got, err := DefaultCacheDir()
		if err != nil || got != "/srv/sbom-cache" {
			t.Errorf("DefaultCacheDir = (%q, %v), want /srv/sbom-cache", got, err)
		}
	})

	t.Run("otherwise it sits beside forge's other machine caches", func(t *testing.T) {
		t.Setenv(CacheDirEnv, "   ")
		base, err := os.UserCacheDir()
		if err != nil {
			t.Skipf("no user cache dir on this host: %v", err)
		}
		got, err := DefaultCacheDir()
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(base, "forge", "sbom"); got != want {
			t.Errorf("DefaultCacheDir = %q, want %q", got, want)
		}
	})

	t.Run("no cache directory is an error that says how to choose one", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("UserCacheDir fails from unset variables only on linux")
		}
		t.Setenv(CacheDirEnv, "")
		t.Setenv("XDG_CACHE_HOME", "")
		t.Setenv("HOME", "")
		_, err := DefaultCacheDir()
		if err == nil || !strings.Contains(err.Error(), CacheDirEnv) {
			t.Errorf("DefaultCacheDir = %v, want an error naming %s", err, CacheDirEnv)
		}
	})
}

func TestCache_PutRejectsWhatItCannotStore(t *testing.T) {
	doc := []byte(`{"bomFormat":"CycloneDX","specVersion":"1.6"}`)
	key := CacheKey{Generator: "syft@v1.57.2", Subject: imageDigest, SourceName: "api"}

	t.Run("an unkeyable subject is silently not cached", func(t *testing.T) {
		c := &Cache{Dir: t.TempDir()}
		bad := key
		bad.Subject = "not-a-digest"
		if err := c.Put(bad, "image", doc); err != nil {
			t.Errorf("Put = %v, want a silent no-op", err)
		}
		if _, _, ok := c.Get(bad); ok {
			t.Error("an unkeyable subject was served from the cache")
		}
		if entries, _ := os.ReadDir(c.Dir); len(entries) != 0 {
			t.Errorf("an unkeyable Put wrote %d entries", len(entries))
		}
	})

	t.Run("an empty directory or generator means no cache", func(t *testing.T) {
		if err := (&Cache{}).Put(key, "image", doc); err != nil {
			t.Errorf("Put into a cache with no Dir = %v", err)
		}
		noGen := key
		noGen.Generator = ""
		if err := (&Cache{Dir: t.TempDir()}).Put(noGen, "image", doc); err != nil {
			t.Errorf("Put with no generator = %v", err)
		}
	})

	t.Run("a cache directory that is a file is an error", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := (&Cache{Dir: blocker}).Put(key, "image", doc); err == nil {
			t.Error("Put under a regular file must fail rather than pretend to cache")
		}
	})
}

func TestCache_GetRefusesEntriesThatDoNotMatch(t *testing.T) {
	c := &Cache{Dir: t.TempDir()}
	key := CacheKey{Generator: "syft@v1.57.2", Subject: imageDigest, SourceName: "api", SourceVersion: "v1"}
	doc := []byte(`{"bomFormat":"CycloneDX","specVersion":"1.6"}`)
	if err := c.Put(key, "image", doc); err != nil {
		t.Fatal(err)
	}
	data, source, ok := c.Get(key)
	if !ok || source != "image" || string(data) != string(doc) {
		t.Fatalf("Get = (%q, %q, %v), want the stored entry", data, source, ok)
	}

	stem, _ := c.stem(key)
	for name, tamper := range map[string]func(){
		"unreadable metadata": func() { _ = os.WriteFile(stem+".json", []byte("{nope"), 0o600) },
		"metadata with no source": func() {
			_ = os.WriteFile(stem+".json", []byte(`{"format":"`+Format+`","source_name":"api","source_version":"v1"}`), 0o600)
		},
		"a missing document": func() { _ = os.Remove(stem + ".cdx.json") },
	} {
		t.Run(name, func(t *testing.T) {
			_ = c.Put(key, "image", doc) // restore a good entry
			tamper()
			if _, _, ok := c.Get(key); ok {
				t.Error("a damaged entry was served")
			}
		})
	}

	t.Run("a document that is not CycloneDX is a miss even with a matching digest", func(t *testing.T) {
		junk := []byte(`{"hello":"world"}`)
		_ = c.Put(key, "image", junk) // the digest beside it is honest
		if _, _, ok := c.Get(key); ok {
			t.Error("a non-CycloneDX document was served")
		}
	})

	t.Run("another name or version is another entry", func(t *testing.T) {
		_ = c.Put(key, "image", doc)
		other := key
		other.SourceVersion = "v2"
		if _, _, ok := c.Get(other); ok {
			t.Error("an entry was served for a different source version")
		}
	})
}

func TestSafeSegment(t *testing.T) {
	tests := map[string]string{
		"syft@v1.57.2":     "syft@v1.57.2",
		"linux/arm64":      "linux_arm64",
		"../../etc":        ".._.._etc",
		"a b:c":            "a_b_c",
		"under_score-dot.": "under_score-dot.",
	}
	for in, want := range tests {
		if got := safeSegment(in); got != want {
			t.Errorf("safeSegment(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWriteAtomic(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "doc.json")
	if err := writeAtomic(target, []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(target, []byte("v2")); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "v2" {
		t.Errorf("content = %q, want v2", b)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("a temp file was left behind: %v", entries)
	}

	if err := writeAtomic(filepath.Join(dir, "missing", "doc.json"), []byte("x")); err == nil {
		t.Error("writing into a directory that does not exist must fail")
	}

	// Renaming a file over a directory fails, and must clean up its temp file.
	sub := filepath.Join(dir, "occupied")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(sub, []byte("x")); err == nil {
		t.Error("renaming over a non-empty directory must fail")
	}
	for _, e := range mustReadDir(t, dir) {
		if strings.Contains(e, ".tmp-") {
			t.Errorf("temp file %q left behind after a failed rename", e)
		}
	}
}

func mustReadDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}

func TestExecRunner_Run(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	ctx := context.Background()

	out, err := execRunner{}.Run(ctx, sh, []string{"-c", `printf '%s|%s' "$SYFT_CHECK_FOR_APP_UPDATE" "$EXTRA"`}, []string{"EXTRA=from-env"})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "false|from-env" {
		t.Errorf("stdout = %q, want the update check switched off and the child env applied", out)
	}

	_, err = execRunner{}.Run(ctx, sh, []string{"-c", "echo 'registry denied' >&2; exit 3"}, nil)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || !strings.Contains(err.Error(), "registry denied") {
		t.Errorf("err = %v, want an ExitError carrying syft's stderr", err)
	}

	_, err = execRunner{}.Run(ctx, sh, []string{"-c", "exit 4"}, nil)
	if !errors.As(err, &exitErr) || err.Error() != exitErr.Error() {
		t.Errorf("err = %v, want the bare exit error when stderr is empty", err)
	}

	if _, err := (execRunner{}).Run(ctx, filepath.Join(t.TempDir(), "no-syft"), nil, nil); err == nil {
		t.Error("a missing binary must fail")
	}
}
