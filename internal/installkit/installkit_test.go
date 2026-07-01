package installkit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestRenderPathTemplate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		data map[string]any
		want string
		err  bool
	}{
		{"plain_string_short_circuits", "pkg/middleware/auth/handler.go", nil, "pkg/middleware/auth/handler.go", false},
		{"empty_short_circuits", "", nil, "", false},
		{"single_token", "frontends/{{.FrontendName}}/pages/billing.tsx", map[string]any{"FrontendName": "web"}, "frontends/web/pages/billing.tsx", false},
		{"two_tokens", "pkg/{{.Service}}/{{.Name}}.go", map[string]any{"Service": "billing", "Name": "store"}, "pkg/billing/store.go", false},
		{"unparseable_template", "frontends/{{.FrontendName", map[string]any{"FrontendName": "web"}, "", true},
		{"missing_field_renders_empty_token", "pkg/{{.MissingField}}/handler.go", map[string]any{}, "pkg/<no value>/handler.go", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := RenderPathTemplate(tc.in, tc.data)
			if (err != nil) != tc.err {
				t.Fatalf("err = %v, wantErr = %v", err, tc.err)
			}
			if !tc.err && got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidSlug(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"jwt-auth":      true,
		"my_pack":       true,
		"auth123":       true,
		"stripe":        true,
		"clerk-webhook": true,

		"":                    false,
		"-leading-hyphen":     false,
		"_leading-underscore": false,
		"UPPERCASE":           false,
		"Has-Caps":            false,
		"has spaces":          false,
		"has/slash":           false,
		"has.dot":             false,
		"has!bang":            false,
	}
	for name, want := range cases {
		name := name
		want := want
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := ValidSlug(name); got != want {
				t.Errorf("ValidSlug(%q) = %v, want %v", name, got, want)
			}
		})
	}
}

func TestIsProtoFile(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"proto/audit/v1/audit.proto":      true,
		"audit.proto":                     true,
		"pkg/middleware/auth/handler.go":  false,
		"proto/audit/v1/audit.proto.tmpl": false, // .tmpl suffix wins
		"frontends/web/pages/page.tsx":    false,
		"":                                false,
		"db/migrations/00001_init.up.sql": false,
	}
	for in, want := range cases {
		in := in
		want := want
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			if got := IsProtoFile(in); got != want {
				t.Errorf("IsProtoFile(%q) = %v, want %v", in, got, want)
			}
		})
	}
}

func TestFirstByteIndex(t *testing.T) {
	t.Parallel()
	cases := []struct {
		s    string
		c    byte
		want int
	}{
		{"hello\nworld", '\n', 5},
		{"no-newline-here", '\n', -1},
		{"", '\n', -1},
		{"\nstart", '\n', 0},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.s, func(t *testing.T) {
			t.Parallel()
			if got := FirstByteIndex(tc.s, tc.c); got != tc.want {
				t.Errorf("FirstByteIndex(%q,%q) = %d, want %d", tc.s, tc.c, got, tc.want)
			}
		})
	}
}

// TestRenderAndWriteOverwritePolicies covers the three policy variants
// against an in-memory templates FS and a temp project dir. Each test
// pre-creates the target file (when applicable) so we can assert Skipped
// vs Wrote without overloading the harness.
func TestRenderAndWriteOverwritePolicies(t *testing.T) {
	t.Parallel()
	const tmplName = "hello.txt.tmpl"
	const tmplBody = "hello {{.Name}}\n"
	const basePath = "templates"

	fsys := fstest.MapFS{
		filepath.ToSlash(filepath.Join(basePath, tmplName)): {Data: []byte(tmplBody)},
	}

	cases := []struct {
		name      string
		policy    OverwritePolicy
		preExist  bool
		wantWrote bool
		wantSkip  bool
		wantBody  string
	}{
		{"always_no_existing_writes", Always, false, true, false, "hello world\n"},
		{"always_overwrites_existing", Always, true, true, false, "hello world\n"},
		{"once_no_existing_writes", OnceSkip, false, true, false, "hello world\n"},
		{"once_skips_existing", OnceSkip, true, false, true, "pre-existing\n"},
		{"never_no_existing_writes", NeverSkip, false, true, false, "hello world\n"},
		{"never_skips_existing", NeverSkip, true, false, true, "pre-existing\n"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			projectDir := t.TempDir()
			outRel := "out/hello.txt"
			target := filepath.Join(projectDir, outRel)
			if tc.preExist {
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(target, []byte("pre-existing\n"), 0o644); err != nil {
					t.Fatalf("write pre-existing: %v", err)
				}
			}

			var logs []string
			opts := WriteOpts{
				OverwritePolicy: tc.policy,
				LogFunc: func(format string, args ...any) {
					logs = append(logs, format)
				},
			}
			outcome, err := RenderAndWrite(fsys, basePath, tmplName, outRel, projectDir, map[string]any{"Name": "world"}, opts)
			if err != nil {
				t.Fatalf("RenderAndWrite: %v", err)
			}
			if outcome.Wrote != tc.wantWrote {
				t.Errorf("Wrote = %v, want %v", outcome.Wrote, tc.wantWrote)
			}
			if outcome.Skipped != tc.wantSkip {
				t.Errorf("Skipped = %v, want %v", outcome.Skipped, tc.wantSkip)
			}
			if outcome.ResolvedOutput != outRel {
				t.Errorf("ResolvedOutput = %q, want %q", outcome.ResolvedOutput, outRel)
			}
			if outcome.AbsTarget != target {
				t.Errorf("AbsTarget = %q, want %q", outcome.AbsTarget, target)
			}
			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatalf("read target: %v", err)
			}
			if string(got) != tc.wantBody {
				t.Errorf("body = %q, want %q", got, tc.wantBody)
			}
			// LogFunc should fire on writes but not on skips.
			if tc.wantWrote {
				if len(logs) != 1 || !strings.Contains(logs[0], "Created") {
					t.Errorf("expected 'Created' log on write, got %v", logs)
				}
			}
			if tc.wantSkip && len(logs) != 0 {
				t.Errorf("expected no logs on skip, got %v", logs)
			}
		})
	}
}

// TestRenderAndWriteRendersOutputPathTemplate confirms that the output
// path itself is processed as a Go template before the join.
func TestRenderAndWriteRendersOutputPathTemplate(t *testing.T) {
	t.Parallel()
	const tmplName = "page.tsx.tmpl"
	const basePath = "templates"
	fsys := fstest.MapFS{
		filepath.ToSlash(filepath.Join(basePath, tmplName)): {Data: []byte("export const name = '{{.FrontendName}}';\n")},
	}
	projectDir := t.TempDir()

	outcome, err := RenderAndWrite(
		fsys, basePath, tmplName,
		"frontends/{{.FrontendName}}/src/page.tsx",
		projectDir,
		map[string]any{"FrontendName": "web"},
		WriteOpts{OverwritePolicy: Always},
	)
	if err != nil {
		t.Fatalf("RenderAndWrite: %v", err)
	}
	if outcome.ResolvedOutput != "frontends/web/src/page.tsx" {
		t.Errorf("ResolvedOutput = %q, want frontends/web/src/page.tsx", outcome.ResolvedOutput)
	}
	body, err := os.ReadFile(filepath.Join(projectDir, "frontends/web/src/page.tsx"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != "export const name = 'web';\n" {
		t.Errorf("body = %q, want 'export const name = \\'web\\';\\n'", body)
	}
}

// TestRenderAndWriteNilLogFunc confirms LogFunc=nil is treated as "log
// nothing" — callers that don't want progress chatter (audit, validation
// passes) shouldn't crash.
func TestRenderAndWriteNilLogFunc(t *testing.T) {
	t.Parallel()
	const tmplName = "hello.txt.tmpl"
	const basePath = "templates"
	fsys := fstest.MapFS{
		filepath.ToSlash(filepath.Join(basePath, tmplName)): {Data: []byte("hello\n")},
	}
	projectDir := t.TempDir()
	if _, err := RenderAndWrite(fsys, basePath, tmplName, "out.txt", projectDir, nil, WriteOpts{}); err != nil {
		t.Fatalf("RenderAndWrite: %v", err)
	}
}

// TestRenderAndWriteCustomFileModes confirms DirMode and FileMode are
// honoured, falling back to 0755 / 0644 when zero.
func TestRenderAndWriteCustomFileModes(t *testing.T) {
	t.Parallel()
	const tmplName = "x.txt"
	const basePath = "templates"
	fsys := fstest.MapFS{
		filepath.ToSlash(filepath.Join(basePath, tmplName)): {Data: []byte("body\n")},
	}
	projectDir := t.TempDir()
	_, err := RenderAndWrite(fsys, basePath, tmplName, "sub/x.txt", projectDir, nil, WriteOpts{
		FileMode: 0o600,
		DirMode:  0o700,
	})
	if err != nil {
		t.Fatalf("RenderAndWrite: %v", err)
	}
	st, err := os.Stat(filepath.Join(projectDir, "sub", "x.txt"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %o, want 0600", got)
	}
	dst, err := os.Stat(filepath.Join(projectDir, "sub"))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := dst.Mode().Perm(); got != 0o700 {
		t.Errorf("dir mode = %o, want 0700", got)
	}
}
