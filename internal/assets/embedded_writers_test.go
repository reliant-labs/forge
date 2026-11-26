package assets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteTemplate(t *testing.T) {
	t.Run("copies the raw template into a created directory", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "nested", "dir", "air.toml")
		if err := WriteTemplate("air.toml.tmpl", dest); err != nil {
			t.Fatalf("WriteTemplate: %v", err)
		}
		got, err := os.ReadFile(dest)
		if err != nil {
			t.Fatalf("read written template: %v", err)
		}
		// WriteTemplate does not render, so the placeholder survives verbatim.
		if !strings.Contains(string(got), "{{.Name}}") {
			t.Errorf("raw template lost its {{.Name}} placeholder:\n%s", got)
		}
	})

	t.Run("unknown template is an error and writes nothing", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "out.txt")
		if err := WriteTemplate("does-not-exist.tmpl", dest); err == nil {
			t.Fatal("expected an error for an unknown template")
		}
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Errorf("destination must not exist after a failed write, stat err = %v", err)
		}
	})
}

func TestWriteTemplateWithData(t *testing.T) {
	t.Run("renders the template data into the file", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "sub", "air.toml")
		data := struct{ Name string }{Name: "my-svc"}
		if err := WriteTemplateWithData("air.toml.tmpl", dest, data); err != nil {
			t.Fatalf("WriteTemplateWithData: %v", err)
		}
		got, err := os.ReadFile(dest)
		if err != nil {
			t.Fatalf("read rendered template: %v", err)
		}
		s := string(got)
		if strings.Contains(s, "{{") {
			t.Errorf("rendered output still holds template actions:\n%s", s)
		}
		if !strings.Contains(s, "./cmd/my-svc") {
			t.Errorf("rendered output missing substituted name, got:\n%s", s)
		}
	})

	t.Run("unknown template is an error and writes nothing", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "out.txt")
		if err := WriteTemplateWithData("does-not-exist.tmpl", dest, nil); err == nil {
			t.Fatal("expected an error for an unknown template")
		}
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Errorf("destination must not exist after a failed render, stat err = %v", err)
		}
	})
}

func TestIsVendoredProtoRelPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"forge proto", ForgeProtoVendorRelPath, true},
		{"validate proto", ValidateProtoVendorRelPath, true},
		{"literal forge path", "proto/forge/v1/forge.proto", true},
		{"a project's own proto", "proto/services/users/v1/users.proto", false},
		{"empty", "", false},
		{"backslash form is not the slash-separated contract", `proto\forge\v1\forge.proto`, false},
		{"prefix only", "proto/forge/v1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsVendoredProtoRelPath(tt.path); got != tt.want {
				t.Errorf("IsVendoredProtoRelPath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestVendoredProtoRelPathsAreEmbeddedFiles(t *testing.T) {
	// Every vendored path, minus the project-relative "proto/" prefix the
	// scaffold adds, must be a file go:embed actually bundles.
	for _, rel := range VendoredProtoRelPaths {
		if _, err := EmbeddedFiles.ReadFile(rel); err != nil {
			t.Errorf("vendored path %q is not in the embedded FS: %v", rel, err)
		}
	}
}

func TestGetValidateProto(t *testing.T) {
	got, err := GetValidateProto()
	if err != nil {
		t.Fatalf("GetValidateProto: %v", err)
	}
	if !strings.Contains(string(got), "package buf.validate;") {
		t.Errorf("embedded validate.proto does not declare package buf.validate")
	}
}

func TestWriteValidateProto(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proto", "buf", "validate")
	if err := WriteValidateProto(dir); err != nil {
		t.Fatalf("WriteValidateProto: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "validate.proto"))
	if err != nil {
		t.Fatalf("read written validate.proto: %v", err)
	}
	want, err := GetValidateProto()
	if err != nil {
		t.Fatalf("GetValidateProto: %v", err)
	}
	if string(got) != string(want) {
		t.Error("validate.proto must be vendored byte-for-byte; go_package is left untouched")
	}
}

func TestWriteForgeV1Proto_NoDestination(t *testing.T) {
	// A regular file where the destination directory should be makes MkdirAll fail.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteForgeV1Proto(filepath.Join(blocker, "sub")); err == nil {
		t.Fatal("expected an error when the destination directory cannot be created")
	}
}

func TestGoPackageOptionRE_RewritesAnyDeclaredValue(t *testing.T) {
	// The rewrite must not depend on the embedded literal: a stale value
	// pointing at a nonexistent module is replaced structurally.
	in := "syntax = \"proto3\";\noption go_package = \"github.com/reliant-labs/forge/gen/forge/v1;forgev1\";\nmessage M {}\n"
	got := goPackageOptionRE.ReplaceAllString(in, forgePBGoPackage)
	if !strings.Contains(got, forgePBGoPackage) {
		t.Errorf("go_package not rewritten:\n%s", got)
	}
	if strings.Contains(got, "gen/forge/v1") {
		t.Errorf("stale go_package survived:\n%s", got)
	}
	if !strings.Contains(got, "message M {}") {
		t.Errorf("rewrite clobbered unrelated lines:\n%s", got)
	}
}

func TestNormalizeVendoredProto(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "drops go_package line",
			in:   "syntax = \"proto3\";\noption go_package = \"a/b;c\";\nmessage M {}\n",
			want: "syntax = \"proto3\";\nmessage M {}",
		},
		{
			name: "drops indented go_package line",
			in:   "a\n  option go_package = \"x\";\nb",
			want: "a\nb",
		},
		{
			name: "normalizes CRLF",
			in:   "a\r\nb\r\n",
			want: "a\nb",
		},
		{
			name: "trims trailing whitespace per line",
			in:   "a \t\nb  \n",
			want: "a\nb",
		},
		{
			name: "drops trailing blank lines but keeps interior ones",
			in:   "a\n\nb\n\n\n",
			want: "a\n\nb",
		},
		{
			name: "empty stays empty",
			in:   "",
			want: "",
		},
		{
			name: "only blanks collapses to empty",
			in:   "\n\n \n",
			want: "",
		},
		{
			name: "other options are kept",
			in:   "option java_package = \"x\";\n",
			want: "option java_package = \"x\";",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeVendoredProto(tt.in); got != tt.want {
				t.Errorf("NormalizeVendoredProto(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNormalizeVendoredProto_WrittenCopyMatchesEmbedded(t *testing.T) {
	// The property forge generate and `lint --vendored-protos` both rely on:
	// the copy WriteForgeV1Proto writes (go_package rewritten) normalizes
	// equal to the embedded original.
	dir := t.TempDir()
	if err := WriteForgeV1Proto(dir); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(filepath.Join(dir, "forge.proto"))
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := GetForgeV1Proto()
	if err != nil {
		t.Fatal(err)
	}
	if NormalizeVendoredProto(string(written)) != NormalizeVendoredProto(string(embedded)) {
		t.Error("written forge.proto diverges from the embedded copy beyond go_package")
	}
}
