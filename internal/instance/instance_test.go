package instance

import (
	"path/filepath"
	"testing"
)

func TestSanitize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"feat/up-generate", "feat-up-generate"},
		{"Feat/Up_Generate", "feat-up-generate"},
		{"  spaces  ", "spaces"},
		{"chore/bump-forge-93067142", "chore-bump-forge-9306714"}, // bounded to 24 chars
		{"---weird---", "weird"},
		{"!!!", ""},
		{"UPPER", "upper"},
		{"a.b.c", "a-b-c"},
		{"", ""},
	}
	for _, c := range cases {
		if got := Sanitize(c.in); got != c.want {
			t.Errorf("Sanitize(%q) = %q, want %q", c.in, got, c.want)
		}
		// Sanitize is idempotent: re-sanitizing a sanitized value is a no-op.
		if got := Sanitize(Sanitize(c.in)); got != c.want {
			t.Errorf("Sanitize not idempotent for %q", c.in)
		}
	}
}

func TestInstanceDArgs(t *testing.T) {
	// Default instance pushes NO options — the byte-identical render contract.
	if d := (Instance{}).DArgs(); d != nil {
		t.Errorf("default instance must emit no -D args, got %v", d)
	}
	// A named instance pushes a QUOTED string name + a bare int index.
	got := Instance{Name: "feat-x", Index: 3}.DArgs()
	want := []string{`instance="feat-x"`, "instance_index=3"}
	if len(got) != len(want) {
		t.Fatalf("DArgs len = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("DArgs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// An all-digit instance name must stay a str (quoted), never coerced to
	// int — same contract as renderDArgs' image_tag quoting.
	if d := (Instance{Name: "12345", Index: 1}).DArgs(); d[0] != `instance="12345"` {
		t.Errorf("all-digit name must be quoted str, got %q", d[0])
	}
}

func TestPortStorePath(t *testing.T) {
	dir := "/proj"
	// Default instance keeps the historical path (byte-identical dev loop).
	if p := PortStorePath(dir, "dev", Instance{}); p != filepath.Join(dir, ".forge", "ports-dev.json") {
		t.Errorf("default store path = %q", p)
	}
	// Named instance gets its own file so blocks never collide.
	if p := PortStorePath(dir, "dev", Instance{Name: "wt2", Index: 2}); p != filepath.Join(dir, ".forge", "ports-dev-wt2.json") {
		t.Errorf("named store path = %q", p)
	}
}

func TestActiveDefaultIsByteIdentical(t *testing.T) {
	// The process-global default (never SetActive'd) emits no args, so any
	// command that doesn't opt into instances renders exactly as before.
	SetActive(Instance{})
	if d := ActiveDArgs(); d != nil {
		t.Errorf("unset active instance must emit no -D args, got %v", d)
	}
	SetActive(Instance{Name: "x", Index: 1})
	if d := ActiveDArgs(); len(d) != 2 {
		t.Errorf("active named instance must emit 2 -D args, got %v", d)
	}
	SetActive(Instance{}) // reset for other tests
}
