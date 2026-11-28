package cli

import (
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestCovDAgoString(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "moments ago"},
		{59 * time.Second, "moments ago"},
		{time.Minute, "1m ago"},
		{59*time.Minute + 59*time.Second, "59m ago"},
		{time.Hour, "1h ago"},
		{47 * time.Hour, "47h ago"},
		{48 * time.Hour, "2d ago"},
		{100 * 24 * time.Hour, "100d ago"},
	}
	for _, c := range cases {
		if got := agoString(c.d); got != c.want {
			t.Errorf("agoString(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestCovDFingerprintKeyPath(t *testing.T) {
	// Built from t.TempDir so both are absolute on every OS: a slash-rooted
	// literal has no drive letter on Windows and would not count as absolute.
	base := t.TempDir()
	root := filepath.Join(base, "proj")
	abs := filepath.Join(base, "opt", "tool", "x")
	if !filepath.IsAbs(abs) {
		t.Fatalf("fixture %q must be absolute", abs)
	}
	cases := []struct {
		name, key string
		wantPath  string
		wantFile  bool
	}{
		{"a plain project file", "proto/a.proto", filepath.Join(root, "proto", "a.proto"), true},
		{"a directory marker", "gen/", "", false},
		{"an external absolute file", "ext:" + filepath.ToSlash(abs), abs, true},
		{"an external file inside the project", "ext:tools/x", filepath.Join(root, "tools", "x"), true},
		{"a go.work inside the project", "gowork:go.work", filepath.Join(root, "go.work"), true},
		{"a go env file outside the project", "goenv:" + filepath.ToSlash(abs), abs, true},
	}
	for _, c := range cases {
		p, isFile := fingerprintKeyPath(root, c.key)
		if p != c.wantPath || isFile != c.wantFile {
			t.Errorf("%s: fingerprintKeyPath(%q) = (%q, %v), want (%q, %v)", c.name, c.key, p, isFile, c.wantPath, c.wantFile)
		}
	}
}

func TestCovDProtoKindToString(t *testing.T) {
	cases := map[protoreflect.Kind]string{
		protoreflect.BoolKind:     "bool",
		protoreflect.Int32Kind:    "int32",
		protoreflect.Sint32Kind:   "sint32",
		protoreflect.Sfixed32Kind: "sfixed32",
		protoreflect.Uint32Kind:   "uint32",
		protoreflect.Fixed32Kind:  "fixed32",
		protoreflect.Int64Kind:    "int64",
		protoreflect.Sint64Kind:   "sint64",
		protoreflect.Sfixed64Kind: "sfixed64",
		protoreflect.Uint64Kind:   "uint64",
		protoreflect.Fixed64Kind:  "fixed64",
		protoreflect.FloatKind:    "float",
		protoreflect.DoubleKind:   "double",
		protoreflect.StringKind:   "string",
		protoreflect.BytesKind:    "bytes",
		protoreflect.MessageKind:  "message",
		protoreflect.EnumKind:     "enum",
		protoreflect.GroupKind:    "string", // anything unmodelled reads as a string
	}
	for kind, want := range cases {
		if got := protoKindToString(kind); got != want {
			t.Errorf("protoKindToString(%v) = %q, want %q", kind, got, want)
		}
	}
}
