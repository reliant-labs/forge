package selfupdate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoverMoveRejectsUntrustedPaths(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"other install", "outside aside", "outside staged", "wrong aside", "wrong staged", "relative install", "unclean staged", "symlink aside", "directory staged"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			exe := filepath.Join(t.TempDir(), "app.exe")
			outside := installed(t, []byte("untouched"))
			j := moveJournal{Path: exe, Aside: asideName(exe, 1), Staged: filepath.Join(filepath.Dir(exe), ".app.exe.new-123")}
			for _, p := range []string{j.Aside, j.Staged} {
				if err := os.WriteFile(p, []byte("untouched"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			switch name {
			case "other install":
				j.Path = outside
			case "outside aside":
				j.Aside = outside
			case "outside staged":
				j.Staged = outside
			case "wrong aside":
				j.Aside = j.Staged
			case "wrong staged":
				j.Staged = j.Aside
			case "relative install":
				j.Path = "app.exe"
			case "unclean staged":
				j.Staged = filepath.Dir(exe) + string(os.PathSeparator) + "." + string(os.PathSeparator) + filepath.Base(j.Staged)
			case "symlink aside":
				if err := os.Remove(j.Aside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, j.Aside); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			case "directory staged":
				if err := os.Remove(j.Staged); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(j.Staged, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			data, err := json.Marshal(j)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(journalPath(dir), data, 0o600); err != nil {
				t.Fatal(err)
			}
			f := swapFS{rename: func(string, string) error { t.Fatal("invalid journal renamed a file"); return nil }, remove: os.Remove}
			if _, err := recoverMove(f, dir, exe); err == nil {
				t.Fatal("accepted untrusted journal")
			}
			if got := string(readFile(t, outside)); got != "untouched" {
				t.Fatalf("outside file changed: %q", got)
			}
			if _, err := os.Lstat(j.Staged); err != nil {
				t.Fatalf("staged operand was removed: %v", err)
			}
			if _, err := os.Stat(journalPath(dir)); err != nil {
				t.Fatal("invalid journal was removed")
			}
		})
	}
}
