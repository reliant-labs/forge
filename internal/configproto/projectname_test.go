package configproto

import (
	"path/filepath"
	"testing"
)

func TestProjectName(t *testing.T) {
	tests := []struct {
		name       string
		forgeYAML  string
		modulePath string
		want       string
	}{
		{"forge.yaml name wins over the module", "name: ledger\n", "github.com/acme/billing-api", "ledger"},
		{"last module element", "", "github.com/acme/billing-api", "billing-api"},
		{"major-version suffix is skipped", "", "github.com/acme/billing-api/v2", "billing-api"},
		{"several suffix-looking elements are all skipped", "", "github.com/acme/v3/v2", "acme"},
		{"v is not a major version", "", "github.com/acme/v", "v"},
		{"vX is not a major version", "", "github.com/acme/vault", "vault"},
		{"a trailing slash is ignored", "", "github.com/acme/billing/", "billing"},
		{"a bare module path", "", "svc", "svc"},
		{"only version elements falls back to the whole path", "", "v2/v3", "v2/v3"},
		{"no module path falls back to the directory name", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "checkout-dir")
			if tt.forgeYAML != "" {
				write(t, dir, "forge.yaml", tt.forgeYAML)
			} else {
				write(t, dir, "placeholder", "")
			}
			want := tt.want
			if want == "" {
				want = "checkout-dir"
			}
			if got := ProjectName(dir, tt.modulePath); got != want {
				t.Errorf("ProjectName(%q) = %q, want %q", tt.modulePath, got, want)
			}
		})
	}
}

func TestModulePath(t *testing.T) {
	tests := []struct {
		name  string
		gomod string // "" = no go.mod at all
		want  string
	}{
		{"plain", "module github.com/acme/x\n\ngo 1.25\n", "github.com/acme/x"},
		{"quoted", "module \"github.com/acme/x\"\n", "github.com/acme/x"},
		{"indented with a comment before", "// header\n\n  module github.com/acme/x  \n", "github.com/acme/x"},
		{"CRLF line endings", "module github.com/acme/x\r\ngo 1.25\r\n", "github.com/acme/x"},
		{"no module directive", "go 1.25\n", ""},
		{"no go.mod", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.gomod != "" {
				write(t, dir, "go.mod", tt.gomod)
			}
			if got := ModulePath(dir); got != tt.want {
				t.Errorf("ModulePath = %q, want %q", got, tt.want)
			}
		})
	}
}
