package collector

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/reliant-labs/forge/pkg/telemetry"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden.yaml from the current render")

// fragmentDoc is f as a document, in a fixed shape for the goldens.
func fragmentDoc(f Fragment) map[string]any {
	return map[string]any{
		"extensions":              f.Extensions,
		"receivers":               f.Receivers,
		"processors":              f.Processors,
		"exporters":               f.Exporters,
		"pipelines":               f.Pipelines,
		"service_extensions":      f.ServiceExtensions,
		"logs_preserve":           f.Logs.Preserve,
		"logs_processors":         f.Logs.Processors,
		"node_metrics_processors": f.NodeMetricsProcessors,
		"k8sattributes_labels":    f.K8sAttributesLabels,
		"warnings":                f.Warnings,
	}
}

func golden(t *testing.T, name string, doc any) {
	t.Helper()
	got, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", name+".golden.yaml")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run `go test ./pkg/telemetry/collector -update`)", err)
	}
	if string(got) != string(want) {
		t.Errorf("%s differs; run `go test ./pkg/telemetry/collector -update` and review the diff\n--- got\n%s", path, got)
	}
}

func mustBuild(t *testing.T, targets []telemetry.Target, opts Options) Fragment {
	t.Helper()
	f, err := Build(targets, opts)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// One golden per mechanism, so a change to one shows in its own file.
func TestGolden(t *testing.T) {
	noProfiles := func(o Options) Options { o.EnableProfiles = false; return o }
	cases := []struct {
		name    string
		targets []telemetry.Target
		opts    Options
	}{
		{"metrics_cluster_pods", []telemetry.Target{clusterManifests(), clusterWorkload()}, noProfiles(clusterOptions())},
		{"metrics_cluster_secured", []telemetry.Target{clusterSecured()}, noProfiles(clusterOptions())},
		{"metrics_host", []telemetry.Target{hostEngine()}, noProfiles(hostOptions())},
		{"profiles_cluster", []telemetry.Target{clusterManifests(), clusterSecured()}, clusterOptions()},
		{"profiles_host", []telemetry.Target{hostEngine()}, hostOptions()},
		{"logs_cluster", []telemetry.Target{clusterManifests(), clusterSecured()}, noProfiles(clusterOptions())},
		{"logs_host", []telemetry.Target{hostEngine(), hostRenamed()}, noProfiles(hostOptions())},
		{"isolated_addresses", []telemetry.Target{clusterAddress(), clusterManifests()}, func() Options {
			o := clusterOptions()
			o.Discovery = DiscoverAddresses
			return o
		}()},
		{"cluster_all", clusterTargets(), clusterOptions()},
		{"host_all", hostTargets(), hostOptions()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			golden(t, c.name, fragmentDoc(mustBuild(t, c.targets, c.opts)))
		})
	}
}

func TestNetworkPoliciesGolden(t *testing.T) {
	ref := CollectorRef{Namespace: "forge-observability", PodLabels: map[string]string{"app.kubernetes.io/name": "otel-agent"}}
	noNS := clusterSecured()
	noNS.ServiceName, noNS.Namespaces = "openbao-anywhere", nil
	off := clusterManifests()
	off.NetworkPolicy = ptr(false)
	addr := clusterAddress()
	addr.NetworkPolicy = ptr(true)
	policies, skipped := NetworkPolicies([]telemetry.Target{
		clusterManifests(), // auto on Manifests: none
		clusterSecured(),   // explicit true: one
		clusterWorkload(),  // auto on a Workload: none here (WorkloadIngressRule)
		off, noNS, addr,
	}, ref)
	rule, ok := WorkloadIngressRule(clusterWorkload(), ref)
	if !ok {
		t.Fatal("WorkloadIngressRule: an auto Workload target gets the collector rule")
	}
	golden(t, "network_policies", map[string]any{"policies": policies, "skipped": skipped, "workload_rule": rule})
}

func TestWorkloadIngressRule(t *testing.T) {
	ref := CollectorRef{Namespace: "obs"}
	w := clusterWorkload()
	w.NetworkPolicy = ptr(false)
	if _, ok := WorkloadIngressRule(w, ref); ok {
		t.Error("network_policy false still adds a rule")
	}
	w.NetworkPolicy = ptr(true)
	if _, ok := WorkloadIngressRule(w, ref); !ok {
		t.Error("network_policy true on a Workload adds no rule to forge's policy")
	}
	if _, ok := WorkloadIngressRule(clusterManifests(), ref); ok {
		t.Error("a Manifests target got a rule for forge's workload policy")
	}
	w.Metrics = nil
	if _, ok := WorkloadIngressRule(w, ref); ok {
		t.Error("a target that pulls nothing got a rule")
	}
}

// Build is a pure function of its input: the same targets in the same order
// render the same bytes.
func TestDeterministic(t *testing.T) {
	render := func() string {
		b, err := yaml.Marshal(fragmentDoc(mustBuild(t, clusterTargets(), clusterOptions())))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	first := render()
	for range 20 {
		if render() != first {
			t.Fatal("two renders of the same targets differ")
		}
	}
}

func TestNoTargetsRendersNothing(t *testing.T) {
	f := mustBuild(t, nil, clusterOptions())
	if len(f.Receivers)+len(f.Processors)+len(f.Pipelines)+len(f.Extensions)+len(f.Exporters) != 0 || f.Logs.Preserve != nil || len(f.Logs.Processors) != 0 {
		t.Errorf("no targets rendered %+v", f)
	}
}

func TestBuildRefuses(t *testing.T) {
	isolatedNoExporter := clusterOptions()
	isolatedNoExporter.Isolated = nil
	noMount := clusterOptions()
	noMount.MountRoot = ""
	noExporters := clusterOptions()
	noExporters.MetricsExporters = nil
	bad := clusterManifests()
	bad.Metrics[0].Allow = "("
	cases := []struct {
		name    string
		targets []telemetry.Target
		opts    Options
		want    string
	}{
		{"runtime", nil, Options{Runtime: telemetry.RuntimeHosted}, "Options.Runtime"},
		{"discovery", nil, Options{Runtime: telemetry.RuntimeCluster, Discovery: "everything"}, "Options.Discovery"},
		{"isolated", []telemetry.Target{clusterAddress()}, func() Options { o := isolatedNoExporter; o.Discovery = DiscoverAll; return o }(), "Options.Isolated"},
		{"mount root", []telemetry.Target{clusterSecured()}, noMount, "Options.MountRoot"},
		{"exporters", []telemetry.Target{clusterManifests()}, noExporters, "MetricsExporters"},
		{"invalid", []telemetry.Target{bad}, clusterOptions(), "not an RE2 expression"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Build(c.targets, c.opts)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("Build error = %v, want one naming %q", err, c.want)
			}
		})
	}
}

// A `$` in a declaration reaches the component as written: the collector's
// resolver reads `$$` as one `$`.
func TestDollarIsEscaped(t *testing.T) {
	tg := hostEngine()
	tg.Metrics[0].Allow = `foo$|bar`
	f := mustBuild(t, []telemetry.Target{tg}, noProfilesHost())
	b, _ := yaml.Marshal(f.Receivers)
	if !strings.Contains(string(b), `up|(?:foo$$|bar)`) {
		t.Errorf("allow regex not escaped for the resolver:\n%s", b)
	}
}

func noProfilesHost() Options { o := hostOptions(); o.EnableProfiles = false; return o }

func TestFieldPath(t *testing.T) {
	cases := map[string][]string{
		"level":                 {"level"},
		"@level":                {"@level"},
		"record.error_severity": {"record", "error_severity"},
		"t.$date":               {"t", "$date"},
		`["k8s.pod"]`:           {"k8s.pod"},
		`a.["b.c"].d`:           {"a", "b.c", "d"},
	}
	for in, want := range cases {
		got, err := fieldPath(in)
		if err != nil || strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("fieldPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "a..b", "a.", `["x`} {
		if _, err := fieldPath(bad); err == nil {
			t.Errorf("fieldPath(%q) accepted", bad)
		}
	}
}

func TestMergeInto(t *testing.T) {
	f := mustBuild(t, clusterTargets(), clusterOptions())
	cfg := map[string]any{
		"receivers": map[string]any{"otlp": map[string]any{}},
		"service":   map[string]any{"extensions": []any{"health_check"}, "pipelines": map[string]any{"traces": map[string]any{}}},
	}
	if err := f.MergeInto(cfg); err != nil {
		t.Fatal(err)
	}
	svc := cfg["service"].(map[string]any)
	if ext := svc["extensions"].([]any); len(ext) != 1+len(f.ServiceExtensions) || ext[0] != "health_check" {
		t.Errorf("service.extensions = %v", ext)
	}
	if _, ok := svc["pipelines"].(map[string]any)["metrics/telemetry_temporal"]; !ok {
		t.Error("pipelines not merged")
	}
	if err := f.MergeInto(cfg); err == nil || !strings.Contains(err.Error(), "already in the config") {
		t.Errorf("merging twice = %v, want a collision error", err)
	}
}
