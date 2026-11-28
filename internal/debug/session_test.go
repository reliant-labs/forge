package debug_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/debug"
)

func TestSessionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	svc := debug.New(debug.Deps{})

	got, err := svc.LoadSession(dir)
	if err != nil || got != nil {
		t.Fatalf("LoadSession with no file = (%+v, %v), want (nil, nil)", got, err)
	}

	want := &debug.SessionInfo{
		Type: "delve", Addr: "127.0.0.1:2345", PID: 10, DlvPID: 11, Binary: "svc",
		TmpDir: "/tmp/x", Owned: true, Connected: false, Resumed: true,
		BreakpointIDs: []int{1, 4}, LogPath: "/tmp/x/dlv.log",
		Started: time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC),
	}
	if err := svc.SaveSession(dir, want); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".forge", "debug-session.json")); err != nil {
		t.Fatalf("the session must live in .forge/debug-session.json: %v", err)
	}
	got, err = svc.LoadSession(dir)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}

	// Saving again replaces the previous session.
	next := &debug.SessionInfo{Type: "delve", Addr: "127.0.0.1:9999", PID: 99}
	if err := svc.SaveSession(dir, next); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.LoadSession(dir); got == nil || got.Addr != "127.0.0.1:9999" || len(got.BreakpointIDs) != 0 {
		t.Errorf("a second save must replace the first, got %+v", got)
	}

	if err := svc.ClearSession(dir); err != nil {
		t.Fatalf("ClearSession: %v", err)
	}
	if got, err := svc.LoadSession(dir); err != nil || got != nil {
		t.Errorf("after Clear, LoadSession = (%+v, %v), want (nil, nil)", got, err)
	}
	if err := svc.ClearSession(dir); err != nil {
		t.Errorf("clearing a session that is already gone must succeed, got %v", err)
	}
}

func TestLoadSession_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".forge", "debug-session.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := debug.New(debug.Deps{}).LoadSession(dir)
	if err == nil || !strings.Contains(err.Error(), "parsing session file") {
		t.Fatalf("LoadSession = (%+v, %v), want a parse error", got, err)
	}
	if got != nil {
		t.Errorf("a corrupt session must not yield a value, got %+v", got)
	}
}

func TestLoadSession_UnreadableFile(t *testing.T) {
	dir := t.TempDir()
	// A directory where the session file should be: reading it fails with
	// something other than "does not exist".
	if err := os.MkdirAll(filepath.Join(dir, ".forge", "debug-session.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := debug.New(debug.Deps{}).LoadSession(dir)
	if err == nil || !strings.Contains(err.Error(), "reading session file") {
		t.Fatalf("LoadSession = %v, want a read error", err)
	}
}

func TestSaveSession_DirectoryCannotBeCreated(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := debug.New(debug.Deps{}).SaveSession(blocker, &debug.SessionInfo{})
	if err == nil || !strings.Contains(err.Error(), "creating session directory") {
		t.Fatalf("SaveSession under a regular file = %v, want a directory error", err)
	}
}

func TestClearSession_RemovalFailure(t *testing.T) {
	dir := t.TempDir()
	// A non-empty directory in place of the file: os.Remove refuses it.
	sub := filepath.Join(dir, ".forge", "debug-session.json")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := debug.New(debug.Deps{}).ClearSession(dir)
	if err == nil || !strings.Contains(err.Error(), "removing session file") {
		t.Fatalf("ClearSession = %v, want a removal error", err)
	}
}
