package kclrender

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/devstack"
	"github.com/reliant-labs/forge/pkg/release"
)

func TestWithDevStackDArgs(t *testing.T) {
	prev := devstack.Active()
	t.Cleanup(func() { devstack.SetActive(prev) })

	t.Run("the default stack adds nothing and returns the caller's slice", func(t *testing.T) {
		devstack.SetActive(devstack.Options{})
		in := []string{"env=dev"}
		got := withDevStackDArgs(in)
		if !reflect.DeepEqual(got, in) {
			t.Errorf("got %v", got)
		}
	})

	t.Run("worktree and branch are bound, quoted, after the caller's own", func(t *testing.T) {
		devstack.SetActive(devstack.Options{Worktree: "w-123", Branch: "feat-x"})
		got := withDevStackDArgs([]string{"env=dev"})
		want := []string{"env=dev", `worktree="w-123"`, `branch="feat-x"`}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("a binding the caller made is never overridden", func(t *testing.T) {
		devstack.SetActive(devstack.Options{Worktree: "w-123", Branch: "feat-x"})
		got := withDevStackDArgs([]string{`worktree="explicit"`})
		want := []string{`worktree="explicit"`, `branch="feat-x"`}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("the caller's slice is copied, not appended to in place", func(t *testing.T) {
		devstack.SetActive(devstack.Options{Branch: "b"})
		backing := make([]string, 1, 8) // spare capacity: an in-place append would write into it
		backing[0] = "env=dev"
		got := withDevStackDArgs(backing)
		if len(got) != 2 {
			t.Fatalf("got %v", got)
		}
		if extra := backing[:2]; extra[1] != "" {
			t.Errorf("the caller's backing array was written to: %v", extra)
		}
	})

	t.Run("an all-digit worktree stays a string", func(t *testing.T) {
		devstack.SetActive(devstack.Options{Worktree: "12345"})
		if got := withDevStackDArgs(nil); !reflect.DeepEqual(got, []string{`worktree="12345"`}) {
			t.Errorf("got %v: an unquoted value would be an int in KCL", got)
		}
	})
}

func TestAbsPaths(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	work, source, err := absPaths("proj", filepath.Join("proj", "deploy", "kcl", "dev"))
	if err != nil {
		t.Fatal(err)
	}
	if work != filepath.Join(wd, "proj") || source != filepath.Join(wd, "proj", "deploy", "kcl", "dev") {
		t.Errorf("(%q, %q): both must resolve against the process cwd, not against each other", work, source)
	}
	abs := t.TempDir()
	work, source, err = absPaths(abs, filepath.Join(abs, "main.k"))
	if err != nil || work != abs || source != filepath.Join(abs, "main.k") {
		t.Errorf("absolute inputs must pass through: (%q, %q, %v)", work, source, err)
	}
}

func TestKpmRunSource(t *testing.T) {
	work := filepath.Join(t.TempDir(), "My Landing")
	if err := os.MkdirAll(filepath.Join(work, "deploy", "kcl"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Run("a plain path goes through as a slash path", func(t *testing.T) {
		plain := filepath.Join(t.TempDir(), "proj", "main.k")
		if got := kpmRunSource(filepath.Dir(plain), plain); got != filepath.ToSlash(plain) {
			t.Errorf("got %q, want %q", got, filepath.ToSlash(plain))
		}
	})
	t.Run("a space in the project path is kept out of the URL by going relative", func(t *testing.T) {
		src := filepath.Join(work, "deploy", "kcl")
		if got := kpmRunSource(work, src); got != "deploy/kcl" {
			t.Errorf("got %q, want the workDir-relative deploy/kcl", got)
		}
	})
	t.Run("the work dir itself or a source outside it stays absolute", func(t *testing.T) {
		if got := kpmRunSource(work, work); got != filepath.ToSlash(work) {
			t.Errorf("source == workDir: got %q", got)
		}
		outside := filepath.Join(t.TempDir(), "other dir", "x.k")
		if got := kpmRunSource(work, outside); got != filepath.ToSlash(outside) {
			t.Errorf("source outside workDir: got %q", got)
		}
	})
	t.Run("a relative path that would itself need escaping stays absolute", func(t *testing.T) {
		src := filepath.Join(work, "deploy kcl", "main.k")
		if got := kpmRunSource(work, src); got != filepath.ToSlash(src) {
			t.Errorf("got %q: a relative path with a space is no better than the absolute one", got)
		}
	})
}

func TestKpmSourceURL(t *testing.T) {
	p := filepath.Join("a", "b", "c.k")
	if got := kpmSourceURL(p); got != "a/b/c.k" {
		t.Errorf("kpmSourceURL(%q) = %q", p, got)
	}
}

func TestChdir(t *testing.T) {
	start, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(start) })

	dir := t.TempDir()
	restore, err := chdir(dir)
	if err != nil {
		t.Fatalf("chdir: %v", err)
	}
	if now, _ := os.Getwd(); evalSyms(t, now) != evalSyms(t, dir) {
		t.Errorf("cwd = %q, want %q", now, dir)
	}
	restore()
	if now, _ := os.Getwd(); now != start {
		t.Errorf("cwd after restore = %q, want %q", now, start)
	}

	if _, err := chdir(filepath.Join(dir, "does-not-exist")); err == nil || !strings.Contains(err.Error(), "enter ") {
		t.Errorf("chdir into a missing directory = %v", err)
	}
	if now, _ := os.Getwd(); now != start {
		t.Errorf("a failed chdir moved the process to %q", now)
	}
}

func evalSyms(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestForgeModuleArg_RefusesAnUnmigratedProject(t *testing.T) {
	t.Run("a kcl.mod that still declares the forge dependency", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "deploy", "kcl"), 0o755); err != nil {
			t.Fatal(err)
		}
		mod := "[package]\nname = \"p\"\n\n[dependencies]\nforge = { path = \"../../.forge-kcl\" }\n"
		if err := os.WriteFile(filepath.Join(dir, "deploy", "kcl", "kcl.mod"), []byte(mod), 0o644); err != nil {
			t.Fatal(err)
		}
		arg, err := forgeModuleArg(dir)
		if err == nil || arg != "" {
			t.Fatalf("forgeModuleArg = (%q, %v), want a refusal", arg, err)
		}
		if !strings.Contains(err.Error(), "kcl.mod") {
			t.Errorf("error %q must name the file", err)
		}
	})

	t.Run("a clean project gets the embedded forge module", func(t *testing.T) {
		arg, err := forgeModuleArg(t.TempDir())
		if err != nil {
			t.Fatalf("forgeModuleArg: %v", err)
		}
		if !strings.HasPrefix(arg, "forge=") {
			t.Errorf("arg = %q, want a forge=<dir> external package", arg)
		}
	})
}

func TestReadPreviewMarker(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := ReadPreviewMarker(dir); ok || err != nil {
		t.Errorf("no marker = (ok=%v, %v), want neither", ok, err)
	}
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(dir, PreviewMarkerFile), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"of":"preview","expires_at":"2026-11-15T12:00:00Z","source":"pr-7"}`)
	m, ok, err := ReadPreviewMarker(dir)
	if err != nil || !ok || m != (PreviewMarker{Of: "preview", ExpiresAt: "2026-11-15T12:00:00Z", Source: "pr-7"}) {
		t.Errorf("ReadPreviewMarker = (%+v, %v, %v)", m, ok, err)
	}
	write(`{nope`)
	if _, ok, err := ReadPreviewMarker(dir); ok || err == nil || !strings.Contains(err.Error(), PreviewMarkerFile) {
		t.Errorf("a corrupt marker = (ok=%v, %v), want an error naming the file", ok, err)
	}
	// A directory where the marker file should be is an unreadable marker, not an absent one.
	other := t.TempDir()
	if err := os.Mkdir(filepath.Join(other, PreviewMarkerFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ReadPreviewMarker(other); ok || err == nil || errors.Is(err, os.ErrNotExist) {
		t.Errorf("an unreadable marker = (ok=%v, %v)", ok, err)
	}
}

func TestSnapshotPinsWithoutASnapshot(t *testing.T) {
	if pins, ok := SnapshotPins(context.Background()); ok || pins != nil {
		t.Errorf("SnapshotPins on a bare context = (%v, %v), want nothing bound", pins, ok)
	}
}

func TestWithSnapshot_RefusesAnInvalidSnapshot(t *testing.T) {
	ctx := context.Background()
	got, err := WithSnapshot(ctx, release.Snapshot{})
	if err == nil {
		t.Fatal("an empty snapshot must be refused")
	}
	if got != ctx {
		t.Error("a refused snapshot must leave the context untouched")
	}
	if _, bound := SnapshotPins(got); bound {
		t.Error("nothing may be bound for a refused snapshot")
	}
	if _, err := SnapshotDArgs(release.Snapshot{}, "cluster"); err == nil {
		t.Error("SnapshotDArgs must validate its snapshot too")
	}
}
