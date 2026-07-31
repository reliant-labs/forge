package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// payloadProject lays out the minimum shape CheckPayloadLimits reads:
// a cmd/<bin>/cmd/serve.go composition root and, optionally, an
// internal/handlers file that proves Connect handlers are mounted.
func payloadProject(t *testing.T, serveBody string, withHandlers bool) string {
	t.Helper()
	dir := t.TempDir()

	cmdDir := filepath.Join(dir, "cmd", "app", "cmd")
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cmdDir, "serve.go"), []byte(serveBody), 0o600); err != nil {
		t.Fatal(err)
	}

	if withHandlers {
		hDir := filepath.Join(dir, "internal", "handlers", "svc")
		if err := os.MkdirAll(hDir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "package svc\n\nfunc Mount(opts ...connect.HandlerOption) {}\n"
		if err := os.WriteFile(filepath.Join(hDir, "service.go"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestCheckPayloadLimits(t *testing.T) {
	// The shape forge emits today: the cap is read off a config struct
	// this file never assigns, so connect receives 0 = unlimited.
	unassignedField := `package cmd

func Serve() {
	skCfg := serverkit.Config{
		Addr:        ":8080",
		ServiceName: "app",
	}
	opts := []connect.HandlerOption{
		connect.WithReadMaxBytes(skCfg.ReadMaxBytes),
		connect.WithSendMaxBytes(skCfg.SendMaxBytes),
	}
	_ = opts
}
`

	assignedInLiteral := `package cmd

func Serve() {
	skCfg := serverkit.Config{
		Addr:         ":8080",
		ReadMaxBytes: 4 << 20,
		SendMaxBytes: 4 << 20,
	}
	opts := []connect.HandlerOption{
		connect.WithReadMaxBytes(skCfg.ReadMaxBytes),
		connect.WithSendMaxBytes(skCfg.SendMaxBytes),
	}
	_ = opts
}
`

	assignedBySelector := `package cmd

func Serve() {
	var skCfg serverkit.Config
	skCfg.ReadMaxBytes = 4 << 20
	skCfg.SendMaxBytes = 4 << 20
	opts := []connect.HandlerOption{
		connect.WithReadMaxBytes(skCfg.ReadMaxBytes),
		connect.WithSendMaxBytes(skCfg.SendMaxBytes),
	}
	_ = opts
}
`

	// The shape forge emits for a project whose config proto predates the
	// read_max_bytes / send_max_bytes fields: nothing assigns the field, but
	// the config is normalized before it is read, so the caps are 4 MiB. A
	// FAIL here would be a false positive on correct, protected code.
	normalizedBeforeRead := `package cmd

func Serve() {
	skCfg, err := projectServerkitConfig(cfg)
	_ = err
	opts := []connect.HandlerOption{
		connect.WithReadMaxBytes(skCfg.ReadMaxBytes),
		connect.WithSendMaxBytes(skCfg.SendMaxBytes),
	}
	_ = opts
}

func projectServerkitConfig(cfg *config.Config) (serverkit.Config, error) {
	skCfg := serverkit.Config{Addr: ":8080"}
	skCfg.Normalize()
	return skCfg, nil
}
`

	literalZero := `package cmd

func Serve() {
	opts := []connect.HandlerOption{
		connect.WithReadMaxBytes(0),
	}
	_ = opts
}
`

	noCapsAtAll := `package cmd

func Serve() {
	opts := []connect.HandlerOption{
		connect.WithInterceptors(),
	}
	_ = opts
}
`

	tests := []struct {
		name         string
		serve        string
		withHandlers bool
		want         Status
		evidence     string
	}{
		{
			name:         "cap read from a never-assigned field is unlimited",
			serve:        unassignedField,
			withHandlers: true,
			want:         StatusFail,
			evidence:     "ReadMaxBytes is never assigned",
		},
		{
			name:         "cap set in the composite literal passes",
			serve:        assignedInLiteral,
			withHandlers: true,
			want:         StatusPass,
		},
		{
			name:         "cap set by selector assignment passes",
			serve:        assignedBySelector,
			withHandlers: true,
			want:         StatusPass,
		},
		{
			name:         "cap read from a normalized config passes",
			serve:        normalizedBeforeRead,
			withHandlers: true,
			want:         StatusPass,
		},
		{
			name:         "literal zero is unlimited",
			serve:        literalZero,
			withHandlers: true,
			want:         StatusFail,
			evidence:     "WithReadMaxBytes(0)",
		},
		{
			name:         "no caps with Connect handlers mounted fails",
			serve:        noCapsAtAll,
			withHandlers: true,
			want:         StatusFail,
		},
		{
			name:         "no caps and no Connect handlers skips",
			serve:        noCapsAtAll,
			withHandlers: false,
			want:         StatusSkip,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := payloadProject(t, tt.serve, tt.withHandlers)
			env := &Environment{ProjectName: "test", ProjectDir: dir}
			got := CheckPayloadLimits(context.Background(), env)
			if got.Status != tt.want {
				t.Fatalf("status = %q, want %q (msg=%s ev=%s)", got.Status, tt.want, got.Message, got.Evidence)
			}
			if tt.evidence != "" && !strings.Contains(got.Evidence, tt.evidence) {
				t.Errorf("evidence %q does not mention %q", got.Evidence, tt.evidence)
			}
		})
	}
}

// A project with no cmd/ tree (a library) must skip, not fail.
func TestCheckPayloadLimitsSkipsWithoutCmd(t *testing.T) {
	env := &Environment{ProjectName: "test", ProjectDir: t.TempDir()}
	if got := CheckPayloadLimits(context.Background(), env); got.Status != StatusSkip {
		t.Fatalf("status = %q, want %q", got.Status, StatusSkip)
	}
}

// _test.go files under cmd/ must not be read: a test may legitimately
// exercise a zero cap.
func TestCheckPayloadLimitsIgnoresTestFiles(t *testing.T) {
	dir := payloadProject(t, "package cmd\n", true)
	body := "package cmd\n\nfunc x() { connect.WithReadMaxBytes(0) }\n"
	if err := os.WriteFile(filepath.Join(dir, "cmd", "app", "cmd", "serve_test.go"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got := CheckPayloadLimits(context.Background(), &Environment{ProjectDir: dir})
	if strings.Contains(got.Evidence, "serve_test.go") {
		t.Fatalf("check sourced a finding from a _test.go file: %s", got.Evidence)
	}
	// With the test file ignored, the only remaining shape is "Connect
	// handlers mounted, no caps anywhere".
	if got.Status != StatusFail {
		t.Fatalf("status = %q, want %q (msg=%s)", got.Status, StatusFail, got.Message)
	}
}
