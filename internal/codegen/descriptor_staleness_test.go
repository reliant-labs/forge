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
		write(ForgeDescriptor{Sources: DescriptorSources(dir)})
		got, err := loadDescriptor(dir)
		if err != nil {
			t.Fatalf("a descriptor matching the protos must load: %v", err)
		}
		if got == nil {
			t.Fatal("expected a descriptor")
		}
	})

	t.Run("proto edited after extraction is refused", func(t *testing.T) {
		write(ForgeDescriptor{Sources: DescriptorSources(dir)})
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
		if !strings.Contains(err.Error(), "proto/services/widget/v1/widget.proto") {
			t.Errorf("the refusal must name the proto that changed; got: %v", err)
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

// The stamp must answer "was this extracted from THESE protos?" — not "have
// the files been touched?". A checkout, rebase or `cp -R` rewrites mtimes
// without changing meaning and must not invalidate a good descriptor.
func TestDescriptorSources_ContentNotMtime(t *testing.T) {
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
	if changed, stamped := StaleSources(b, DescriptorSources(a)); !stamped || len(changed) != 0 {
		t.Errorf("identical proto content in two trees must read as current — otherwise every clone is stale: %v", changed)
	}
	c := mk("service S { rpc X(Y) returns (Z); }\n")
	changed, stamped := StaleSources(c, DescriptorSources(a))
	if !stamped || len(changed) != 1 || changed[0] != "proto/svc/v1/a.proto" {
		t.Errorf("different proto content must be reported, by path, or the guard cannot fire: %v", changed)
	}
	if got := DescriptorSources(t.TempDir()); got != nil {
		t.Errorf("a project with no protos has nothing to fingerprint; got %v", got)
	}
	if _, stamped := StaleSources(a, nil); stamped {
		t.Error("an unstamped descriptor is not enforceable")
	}
}

// Adding or removing a proto changes the stamp the same as editing one.
func TestStaleSources_AddedAndRemovedProtos(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(dir, "proto", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.proto", "a")
	write("b.proto", "b")
	stamp := DescriptorSources(dir)
	write("c.proto", "c")
	if changed, _ := StaleSources(dir, stamp); len(changed) != 1 || changed[0] != "proto/c.proto" {
		t.Errorf("an added proto: %v", changed)
	}
	if err := os.Remove(filepath.Join(dir, "proto", "b.proto")); err != nil {
		t.Fatal(err)
	}
	if changed, _ := StaleSources(dir, stamp); len(changed) != 2 {
		t.Errorf("an added and a removed proto: %v", changed)
	}
}
