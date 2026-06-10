package doctor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeChecksums writes a structured .forge/checksums.json into dir.
func writeChecksums(t *testing.T, dir string, files map[string]map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".forge"), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"forge_version": "test", "files": files})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".forge", "checksums.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckForkedFiles(t *testing.T) {
	tests := []struct {
		name       string
		files      map[string]map[string]any // nil → no checksums.json at all
		wantStatus Status
		wantInMsg  string
		wantInEvid []string
	}{
		{
			name:       "no checksums file — pass",
			files:      nil,
			wantStatus: StatusPass,
			wantInMsg:  "no forked generated files",
		},
		{
			name: "no forks — pass",
			files: map[string]map[string]any{
				"pkg/app/wire_gen.go": {"hash": "abc", "tier": 1},
			},
			wantStatus: StatusPass,
			wantInMsg:  "no forked generated files",
		},
		{
			name: "tier-1 forks — warn with paths and reconcile hint",
			files: map[string]map[string]any{
				"pkg/app/wire_gen.go":  {"hash": "abc", "tier": 1, "forked": true},
				"pkg/app/bootstrap.go": {"hash": "def", "tier": 1, "forked": true},
				"pkg/app/app_gen.go":   {"hash": "ghi", "tier": 1},
			},
			wantStatus: StatusWarn,
			wantInMsg:  "2 forked generated file(s)",
			wantInEvid: []string{"pkg/app/bootstrap.go", "pkg/app/wire_gen.go"},
		},
		{
			name: "tier-2 fork is ownership transfer, not a regen loss — pass",
			files: map[string]map[string]any{
				"handlers/echo/handlers_crud_gen_test.go": {"hash": "abc", "tier": 2, "forked": true},
			},
			wantStatus: StatusPass,
			wantInMsg:  "no forked generated files",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.files != nil {
				writeChecksums(t, dir, tt.files)
			}
			env := &Environment{ProjectDir: dir}
			res := CheckForkedFiles(context.Background(), env)
			if res.Status != tt.wantStatus {
				t.Errorf("status = %s, want %s (message: %s)", res.Status, tt.wantStatus, res.Message)
			}
			if !strings.Contains(res.Message, tt.wantInMsg) {
				t.Errorf("message %q missing %q", res.Message, tt.wantInMsg)
			}
			for _, want := range tt.wantInEvid {
				if !strings.Contains(res.Evidence, want) {
					t.Errorf("evidence %q missing %q", res.Evidence, want)
				}
			}
		})
	}
}
