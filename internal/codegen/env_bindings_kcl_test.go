package codegen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// There is no env-level runtime, so a workload `forge scaffold` declares runs
// nowhere until each env binds it. AppendEnvBinding adds that one line, the
// way the env binds a sibling of the same kind, and never rewrites anything.
func TestAppendEnvBinding(t *testing.T) {
	dir := t.TempDir()
	write := func(env, body string) string {
		p := filepath.Join(dir, "deploy", "kcl", env, "main.k")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	list := "_workloads = [\n    _on_host_job(wl.migrate)\n    _on_host(wl.item)\n]\n\noutput = 1\n"
	dev := write("dev", list)
	prod := write("prod", strings.ReplaceAll(strings.ReplaceAll(list, "_on_host_job", "_on_cluster"), "_on_host(", "_on_cluster("))

	for _, tc := range []struct {
		env, kind, name, want string
	}{
		{"dev", WorkloadKindWorker, "mailer", "    _on_host(wl.mailer)\n]"},
		{"dev", WorkloadKindOperator, "reaper", "    _on_k3d(wl.reaper)\n]"},
		{"prod", WorkloadKindWorker, "mailer", "    _on_cluster(wl.mailer)\n]"},
		{"prod", WorkloadKindTool, "ctl", "    _build_only(wl.ctl)\n]"},
	} {
		applied, err := AppendEnvBinding(dir, tc.env, tc.kind, tc.name)
		if err != nil || !applied {
			t.Fatalf("%s %s: applied=%v err=%v", tc.env, tc.name, applied, err)
		}
		b, _ := os.ReadFile(filepath.Join(dir, "deploy", "kcl", tc.env, "main.k"))
		if !strings.Contains(string(b), tc.want) {
			t.Errorf("%s: want %q at the end of the list:\n%s", tc.env, tc.want, b)
		}
		if !strings.HasSuffix(string(b), "]\n\noutput = 1\n") {
			t.Errorf("%s: content after the list was disturbed:\n%s", tc.env, b)
		}
	}

	// Already bound (by an earlier run, or by hand, under any binder): no-op.
	before, _ := os.ReadFile(dev)
	if applied, err := AppendEnvBinding(dir, "dev", WorkloadKindService, "item"); applied || err != nil {
		t.Errorf("rebinding an already-bound workload: applied=%v err=%v", applied, err)
	}
	if after, _ := os.ReadFile(dev); string(after) != string(before) {
		t.Errorf("a no-op append wrote the file")
	}

	// Restructured past recognition: no write, the caller prints the line.
	write("staging", "_workloads = [w for w in wl.ALL]\n")
	if applied, err := AppendEnvBinding(dir, "staging", WorkloadKindService, "item"); applied || err != nil {
		t.Errorf("no recognisable list: applied=%v err=%v", applied, err)
	}
	_ = prod
}
