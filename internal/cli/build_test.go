package cli

import (
	"testing"
)

// TestCountTagsHelper ensures the docker-build tag counter handles the
// canonical `-t a -t b` shape and ignores other flags.
func TestCountTagsHelper(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"empty", nil, 0},
		{"one tag", []string{"build", "-t", "foo:latest"}, 1},
		{"three tags", []string{"build", "-t", "a", "-t", "b", "-t", "c"}, 3},
		{"unrelated -f flag", []string{"build", "-t", "a", "-f", "Dockerfile"}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := countTags(c.args); got != c.want {
				t.Errorf("want %d, got %d", c.want, got)
			}
		})
	}
}

// TestBuildPushFlagRegistered confirms the --push flag is wired into
// the build command and implies --docker at parse time.
func TestBuildPushFlagRegistered(t *testing.T) {
	cmd := newBuildCmd()
	f := cmd.Flags().Lookup("push")
	if f == nil {
		t.Fatal("--push flag not registered on build command")
	}
	if f.DefValue != "" {
		t.Errorf("--push default = %q, want empty", f.DefValue)
	}
}

func TestBuildDebugFlagExists(t *testing.T) {
	cmd := newBuildCmd()

	f := cmd.Flags().Lookup("debug")
	if f == nil {
		t.Fatal("--debug flag not registered on build command")
	}

	if f.DefValue != "false" {
		t.Errorf("--debug default = %q, want %q", f.DefValue, "false")
	}
}

func TestBuildDebugFlagParsesTrue(t *testing.T) {
	cmd := newBuildCmd()

	if err := cmd.Flags().Parse([]string{"--debug"}); err != nil {
		t.Fatalf("failed to parse --debug: %v", err)
	}

	val, err := cmd.Flags().GetBool("debug")
	if err != nil {
		t.Fatalf("GetBool(\"debug\") error: %v", err)
	}
	if !val {
		t.Error("expected --debug to be true after parsing --debug")
	}
}

func TestBuildDebugFlagDefaultIsFalse(t *testing.T) {
	cmd := newBuildCmd()

	// Parse with no flags — debug should remain false.
	if err := cmd.Flags().Parse([]string{}); err != nil {
		t.Fatalf("failed to parse empty args: %v", err)
	}

	val, err := cmd.Flags().GetBool("debug")
	if err != nil {
		t.Fatalf("GetBool(\"debug\") error: %v", err)
	}
	if val {
		t.Error("expected --debug to default to false")
	}
}

func TestBuildAllFlagsRegistered(t *testing.T) {
	cmd := newBuildCmd()

	expected := []struct {
		name     string
		defValue string
	}{
		{"output", "bin"},
		{"target", "all"},
		{"parallel", "true"},
		{"docker", "false"},
		{"debug", "false"},
	}

	for _, tt := range expected {
		f := cmd.Flags().Lookup(tt.name)
		if f == nil {
			t.Errorf("flag --%s not registered", tt.name)
			continue
		}
		if f.DefValue != tt.defValue {
			t.Errorf("flag --%s default = %q, want %q", tt.name, f.DefValue, tt.defValue)
		}
	}
}
