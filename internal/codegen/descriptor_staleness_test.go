package codegen

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The descriptor is a DERIVED CACHE of the protos. A cache that cannot detect
// its own staleness does not degrade — it lies.
//
// Measured before this guard existed: renaming one RPC in the descriptor,
// touching no .proto, made `forge project graph` report an RPC that existed in
// no proto file and omit one that existed in six places. Nothing warned,
// because loading was read-file-and-unmarshal with no validation.
func TestLoadDescriptor_RefusesStaleCache(t *testing.T) {
	dir := t.TempDir()
	protoDir := filepath.Join(dir, "proto", "services", "widget", "v1")
	if err := os.MkdirAll(protoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	proto := "service WidgetService {\n  rpc GetWidget(GetWidgetRequest) returns (GetWidgetResponse);\n}\n"
	if err := os.WriteFile(filepath.Join(protoDir, "widget.proto"), []byte(proto), 0o644); err != nil {
		t.Fatal(err)
	}
	genDir := filepath.Join(dir, "gen")
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(d ForgeDescriptor) {
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(genDir, "forge_descriptor.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("matching hash loads", func(t *testing.T) {
		write(ForgeDescriptor{SourceHash: DescriptorSourceHash(dir)})
		got, err := loadDescriptor(dir)
		if err != nil {
			t.Fatalf("a descriptor matching the protos must load: %v", err)
		}
		if got == nil {
			t.Fatal("expected a descriptor")
		}
	})

	t.Run("proto edited after extraction is refused", func(t *testing.T) {
		write(ForgeDescriptor{SourceHash: DescriptorSourceHash(dir)})
		// The real-world sequence: edit a proto, then run an inspection
		// command before regenerating.
		if err := os.WriteFile(filepath.Join(protoDir, "widget.proto"),
			[]byte(proto+"\nmessage Extra {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := loadDescriptor(dir)
		if err == nil {
			t.Fatal("a descriptor extracted from different protos must be REFUSED, not returned")
		}
		if !strings.Contains(err.Error(), "stale") || !strings.Contains(err.Error(), "forge generate") {
			t.Errorf("the refusal must say it is stale AND name the fix; got: %v", err)
		}
	})

	t.Run("unstamped file is allowed — only a stamped-then-drifted one is refused", func(t *testing.T) {
		// Deliberate scope limit. An unstamped descriptor is either a
		// pre-guard file (the next generate stamps it) or a hand-built
		// fixture with no proto tree to hash — several callers construct one
		// directly. Refusing those would break them for no safety gain: the
		// drift that actually happens is a descriptor stamped from protos
		// that then changed, and that IS refused (see the case above).
		write(ForgeDescriptor{})
		got, err := loadDescriptor(dir)
		if err != nil {
			t.Fatalf("an unstamped descriptor must still load: %v", err)
		}
		if got == nil {
			t.Fatal("expected a descriptor")
		}
	})

	t.Run("missing descriptor is still fine", func(t *testing.T) {
		if err := os.Remove(filepath.Join(genDir, "forge_descriptor.json")); err != nil {
			t.Fatal(err)
		}
		got, err := loadDescriptor(dir)
		if err != nil || got != nil {
			t.Fatalf("absent is a normal state (CLI/library projects); got %v, %v", got, err)
		}
	})
}

// The hash must answer "was this extracted from THESE protos?" — not "have the
// files been touched?". A checkout, rebase or `cp -R` rewrites mtimes without
// changing meaning and must not invalidate a good descriptor.
func TestDescriptorSourceHash_ContentNotMtime(t *testing.T) {
	mk := func(body string) string {
		dir := t.TempDir()
		pd := filepath.Join(dir, "proto", "svc", "v1")
		if err := os.MkdirAll(pd, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pd, "a.proto"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	a, b := mk("service S {}\n"), mk("service S {}\n")
	if DescriptorSourceHash(a) != DescriptorSourceHash(b) {
		t.Error("identical proto content in two trees must hash identically — otherwise every clone reads as stale")
	}
	c := mk("service S { rpc X(Y) returns (Z); }\n")
	if DescriptorSourceHash(a) == DescriptorSourceHash(c) {
		t.Error("different proto content must hash differently, or the guard cannot fire")
	}
	if DescriptorSourceHash(t.TempDir()) != "" {
		t.Error("a project with no protos has nothing to fingerprint; empty means 'do not enforce'")
	}
}
