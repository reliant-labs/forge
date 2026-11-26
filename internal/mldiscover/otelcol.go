package mldiscover

import (
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

// Settings a collector's ComponentConfig carries.
const (
	settingK8sObjectsEvents = "k8sobjects.events=true"
	settingFilelogPods      = "filelog.container-logs=true"
)

// CollectorConfig is what an OpenTelemetry Collector config declares. Each
// list is sorted and unique.
type CollectorConfig struct {
	// Receivers are the receiver ids the pipelines use (every declared
	// receiver when the config has no pipelines), by type: "otlp/grpc" is
	// "otlp".
	Receivers  []string
	Processors []string
	Exporters  []string
	// Pipelines are the pipeline types: traces, metrics, logs, profiles.
	Pipelines []string
	// Settings are facts about receivers: k8sobjects.events=true when a
	// k8sobjects receiver pulls or watches events, filelog.container-logs=true
	// when a filelog receiver reads /var/log/pods.
	Settings []string
}

// ParseCollectorConfig reads an OTel Collector config (YAML or JSON). ok is
// false when it is not one (no receivers or service). Values the collector
// redacted ("REDACTED") are ignored.
func ParseCollectorConfig(text string) (CollectorConfig, bool) {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil || doc == nil {
		return CollectorConfig{}, false
	}
	return collectorConfigFrom(doc)
}

// CollectorConfigFromObject reads the config of an OpenTelemetryCollector
// custom resource's spec.config: a YAML string (v1alpha1) or an object
// (v1beta1).
func CollectorConfigFromObject(v any) (CollectorConfig, bool) {
	switch t := v.(type) {
	case string:
		return ParseCollectorConfig(t)
	case map[string]any:
		return collectorConfigFrom(t)
	}
	return CollectorConfig{}, false
}

func collectorConfigFrom(doc map[string]any) (CollectorConfig, bool) {
	receivers, _ := doc["receivers"].(map[string]any)
	service, _ := doc["service"].(map[string]any)
	if receivers == nil && service == nil {
		return CollectorConfig{}, false
	}
	rec, proc, exp, pipes := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	used := map[string]bool{}
	pipelines, _ := service["pipelines"].(map[string]any)
	for name, p := range pipelines {
		typ, _, _ := strings.Cut(name, "/")
		pipes[typ] = true
		pm, _ := p.(map[string]any)
		for _, kv := range []struct {
			key string
			set map[string]bool
		}{{"receivers", rec}, {"processors", proc}, {"exporters", exp}} {
			for _, id := range stringList(pm[kv.key]) {
				if kv.key == "receivers" {
					used[id] = true
				}
				t, _, _ := strings.Cut(id, "/")
				kv.set[t] = true
			}
		}
	}
	var settings []string
	for id, cfg := range receivers {
		if len(pipelines) > 0 && !used[id] {
			continue
		}
		t, _, _ := strings.Cut(id, "/")
		rec[t] = true
		cm, _ := cfg.(map[string]any)
		switch t {
		case "k8sobjects":
			objs, _ := cm["objects"].([]any)
			for _, o := range objs {
				if om, ok := o.(map[string]any); ok && om["name"] == "events" {
					settings = append(settings, settingK8sObjectsEvents)
				}
			}
		case "filelog":
			for _, inc := range stringList(cm["include"]) {
				if strings.Contains(inc, "/var/log/pods") || strings.Contains(inc, "/var/log/containers") {
					settings = append(settings, settingFilelogPods)
				}
			}
		}
	}
	return CollectorConfig{
		Receivers: sortedKeys(rec), Processors: sortedKeys(proc), Exporters: sortedKeys(exp),
		Pipelines: sortedKeys(pipes), Settings: uniqueSortedNonNil(settings),
	}, true
}

func stringList(v any) []string {
	switch t := v.(type) {
	case []any:
		var out []string
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return t
	case string:
		return []string{t}
	}
	return nil
}

func uniqueSortedNonNil(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	return uniqueSorted(in)
}

// alloyBlockRE matches an Alloy component block header: `pyroscope.ebpf "default" {`.
var alloyBlockRE = regexp.MustCompile(`(?m)^\s*([a-z][a-z0-9_]*(?:\.[a-z0-9_]+)+)\s+"[^"]*"\s*\{`)

// AlloyConfig is the component blocks a Grafana Alloy config declares,
// split as a collector's are.
type AlloyConfig struct {
	Receivers, Processors, Exporters []string
}

// ParseAlloyConfig reads the component names out of an Alloy (River)
// config. ok is false when it declares none.
func ParseAlloyConfig(text string) (AlloyConfig, bool) {
	rec, proc, exp := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, m := range alloyBlockRE.FindAllStringSubmatch(text, -1) {
		name := m[1]
		switch {
		case strings.HasPrefix(name, "otelcol.processor."), strings.HasPrefix(name, "loki.process"),
			strings.HasPrefix(name, "discovery.relabel"), strings.HasPrefix(name, "prometheus.relabel"):
			proc[name] = true
		case strings.HasPrefix(name, "otelcol.exporter."), name == "loki.write", name == "prometheus.remote_write",
			name == "pyroscope.write":
			exp[name] = true
		case strings.HasPrefix(name, "otelcol.receiver."), strings.HasPrefix(name, "pyroscope."),
			strings.HasPrefix(name, "loki.source."), name == "prometheus.scrape", strings.HasPrefix(name, "prometheus.exporter."),
			strings.HasPrefix(name, "beyla."), name == "faro.receiver":
			rec[name] = true
		}
	}
	if len(rec)+len(proc)+len(exp) == 0 {
		return AlloyConfig{}, false
	}
	return AlloyConfig{Receivers: sortedKeys(rec), Processors: sortedKeys(proc), Exporters: sortedKeys(exp)}, true
}

// collectorProvides reports which telemetry signals a collector (or Alloy)
// config provides.
func collectorProvides(cfg *ComponentConfig) map[string]bool {
	out := map[string]bool{}
	if cfg == nil {
		return out
	}
	for _, r := range cfg.Receivers {
		switch r {
		case "otlp", "otelcol.receiver.otlp", "datadog-otlp":
			out[SignalOTLPReceiver] = true
		case "k8s_events", "loki.source.kubernetes_events", "datadog-k8s-events":
			out[SignalK8sEvents] = true
		case "kubeletstats":
			out[SignalKubeletStats] = true
		case "loki.source.kubernetes", "loki.source.podlogs", "otelcol.receiver.filelog", "datadog-container-logs":
			out[SignalContainerLogs] = true
		case "profiling", "pyroscope.ebpf", "pyroscope.java", "otelcol.receiver.profiling":
			out[SignalContinuousProfiling] = true
		}
	}
	for _, s := range cfg.Settings {
		switch s {
		case settingK8sObjectsEvents:
			out[SignalK8sEvents] = true
		case settingFilelogPods:
			out[SignalContainerLogs] = true
		}
	}
	return out
}
