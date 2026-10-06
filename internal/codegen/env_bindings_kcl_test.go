package codegen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// There is no env-level runtime, so a workload `forge scaffold` declares runs
// nowhere until each env binds it. AppendEnvBinding adds that one line, the
// way the env binds its other workloads, and never rewrites anything.
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
	// A deployed env as scaffolded today: hosted on the control plane.
	write("prod", strings.ReplaceAll(strings.ReplaceAll(list, "_on_host_job(", "_hosted("), "_on_host(", "_hosted("))
	// A deployed env scaffolded before hosting was the default: every
	// workload on a cluster the project operates.
	write("legacy", strings.ReplaceAll(strings.ReplaceAll(list, "_on_host_job(", "_on_cluster("), "_on_host(", "_on_cluster("))

	for _, tc := range []struct {
		env, kind, name, wantBinder string
	}{
		{"dev", WorkloadKindWorker, "mailer", "_on_host"},
		{"dev", WorkloadKindOperator, "reaper", "_on_k3d"},
		{"prod", WorkloadKindWorker, "mailer", "_hosted"},
		{"prod", WorkloadKindJob, "backfill", "_hosted"},
		// The platform refuses these: a cluster you operate is the one
		// binding that can run them.
		{"prod", WorkloadKindCron, "sweeper", "_on_cluster"},
		{"prod", WorkloadKindOperator, "reaper", "_on_cluster"},
		{"prod", WorkloadKindTool, "ctl", "_build_only"},
		// A pre-hosting env keeps its shape: a new service lands beside its
		// siblings on the cluster, never on a control plane it may not have.
		{"legacy", WorkloadKindService, "billing", "_on_cluster"},
	} {
		binder, applied, err := AppendEnvBinding(dir, tc.env, tc.kind, tc.name)
		if err != nil || !applied {
			t.Fatalf("%s %s: applied=%v err=%v", tc.env, tc.name, applied, err)
		}
		if binder != tc.wantBinder {
			t.Errorf("%s %s (%s): binder = %s, want %s", tc.env, tc.name, tc.kind, binder, tc.wantBinder)
		}
		b, _ := os.ReadFile(filepath.Join(dir, "deploy", "kcl", tc.env, "main.k"))
		if want := "    " + tc.wantBinder + "(wl." + tc.name + ")\n]"; !strings.Contains(string(b), want) {
			t.Errorf("%s: want %q at the end of the list:\n%s", tc.env, want, b)
		}
		if !strings.HasSuffix(string(b), "]\n\noutput = 1\n") {
			t.Errorf("%s: content after the list was disturbed:\n%s", tc.env, b)
		}
	}

	// Already bound (by an earlier run, or by hand, under any binder): no-op.
	before, _ := os.ReadFile(dev)
	if _, applied, err := AppendEnvBinding(dir, "dev", WorkloadKindService, "item"); applied || err != nil {
		t.Errorf("rebinding an already-bound workload: applied=%v err=%v", applied, err)
	}
	if after, _ := os.ReadFile(dev); string(after) != string(before) {
		t.Errorf("a no-op append wrote the file")
	}

	// Restructured past recognition: no write, the caller prints the line.
	write("staging", "_workloads = [w for w in wl.ALL]\n")
	if _, applied, err := AppendEnvBinding(dir, "staging", WorkloadKindService, "item"); applied || err != nil {
		t.Errorf("no recognisable list: applied=%v err=%v", applied, err)
	}
}

// A hosted-admissible workload follows the env's migrate job, because it
// reads the database migrate migrates. A hosted env that put one cron on its
// own cluster still binds a new service hosted; a cluster env that rebound
// one service to the control plane still binds the next one on its cluster.
func TestEnvBinderIn_FollowsMigrate(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"hosted env with a cron on a cluster", "    _hosted(wl.migrate)\n    _on_cluster(wl.sweeper)\n    _on_cluster(wl.reaper)\n", "_hosted"},
		{"cluster env with one hosted service", "    _on_cluster(wl.migrate)\n    _hosted(wl.api)\n", "_on_cluster"},
		{"no migrate, a hosted binding", "    _on_cluster(wl.sweeper)\n    _hosted(wl.api)\n", "_hosted"},
		{"no migrate, cluster only", "    _on_cluster(wl.api)\n", "_on_cluster"},
		{"empty list", "", "_hosted"},
		{"migrate bound by a custom binder", "    _on_gpu(wl.migrate)\n    _on_cluster(wl.api)\n", "_on_cluster"},
	} {
		if got := envBinderIn(tc.body, "prod", WorkloadKindService); got != tc.want {
			t.Errorf("%s: binder = %s, want %s", tc.name, got, tc.want)
		}
	}
	// A refused kind never follows migrate onto the control plane.
	if got := envBinderIn("    _hosted(wl.migrate)\n", "prod", WorkloadKindCron); got != "_on_cluster" {
		t.Errorf("cron in a hosted env: binder = %s, want _on_cluster", got)
	}
}

// A fresh scaffold's deployed envs are hosted: every kind the Restricted
// profile admits binds `_hosted`, and only the kinds it refuses bind to a
// cluster you operate. The refusal list must agree with the profile the
// deploy skill documents (cron, operator).
func TestEnvBinder_HostedByDefault(t *testing.T) {
	for kind, want := range map[string]string{
		WorkloadKindService:  "_hosted",
		WorkloadKindWorker:   "_hosted",
		WorkloadKindJob:      "_hosted",
		WorkloadKindCron:     "_on_cluster",
		WorkloadKindOperator: "_on_cluster",
		WorkloadKindTool:     "_build_only",
	} {
		for _, env := range []string{"staging", "prod", "preview"} {
			if got := EnvBinder(env, kind); got != want {
				t.Errorf("EnvBinder(%s, %s) = %s, want %s", env, kind, got, want)
			}
		}
		if (HostedRefusal(kind) != "") != (want == "_on_cluster") {
			t.Errorf("HostedRefusal(%s) = %q disagrees with its binder %s", kind, HostedRefusal(kind), want)
		}
	}
}

func TestEnvDeclaresNoCluster(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "deploy", "kcl", "prod", "main.k")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	for body, want := range map[string]bool{
		"# example:\n#     _cluster = forge.ClusterTarget {...}\n_cluster = None\n": true,
		"_cluster = forge.ClusterTarget {cluster = \"gke\", namespace = \"x\"}\n":   false,
		"# _cluster = None\n_workloads = []\n":                                      false,
	} {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := EnvDeclaresNoCluster(dir, "prod"); got != want {
			t.Errorf("EnvDeclaresNoCluster(%q) = %v, want %v", body, got, want)
		}
	}
	if EnvDeclaresNoCluster(dir, "missing") {
		t.Error("a missing env declares nothing")
	}
}
