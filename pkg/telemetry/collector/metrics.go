package collector

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/reliant-labs/forge/pkg/telemetry"
)

// prometheus is t's prometheus receiver: one scrape job per MetricsScrape.
func (b *builder) prometheus(t telemetry.Target) (map[string]any, error) {
	jobs := make([]any, 0, len(t.Metrics))
	for i, m := range t.Metrics {
		suffix := m.Name
		if suffix == "" {
			suffix = strconv.Itoa(i)
		}
		job := map[string]any{
			// The job name only has to be unique; service.name comes from
			// the target's transform.
			"job_name":        lit("telemetry/" + t.ServiceName + "/" + suffix),
			"scrape_interval": m.Interval,
			"scrape_timeout":  m.Timeout,
			"metrics_path":    lit(m.Path),
			"scheme":          m.Scheme,
			// Prometheus 3 refuses a response whose Content-Type it cannot
			// read; many exporters send none.
			"fallback_scrape_protocol": "PrometheusText0.0.4",
			"sample_limit":             m.SampleLimit,
			"label_limit":              m.LabelLimit,
			"label_value_length_limit": m.LabelValueLengthLimit,
		}
		if len(m.Params) > 0 {
			params := map[string]any{}
			for _, k := range sortedKeys(m.Params) {
				params[lit(k)] = strs(lits(m.Params[k])...)
			}
			job["params"] = params
		}
		if m.BearerToken != nil {
			file, err := b.mountFile(t, fmt.Sprintf("metrics[%d].bearer_token", i), m.BearerToken)
			if err != nil {
				return nil, err
			}
			job["authorization"] = map[string]any{"type": "Bearer", "credentials_file": file}
		}
		if m.TLS != nil {
			tls := map[string]any{}
			if m.TLS.InsecureSkipVerify {
				tls["insecure_skip_verify"] = true
			}
			if m.TLS.ServerName != "" {
				tls["server_name"] = lit(m.TLS.ServerName)
			}
			if m.TLS.CA != nil {
				file, err := b.mountFile(t, fmt.Sprintf("metrics[%d].tls.ca", i), m.TLS.CA)
				if err != nil {
					return nil, err
				}
				tls["ca_file"] = file
			}
			job["tls_config"] = tls
		}
		if b.pods(t) {
			job["kubernetes_sd_configs"] = []any{b.podSD(t)}
			job["relabel_configs"] = []any{
				// A pod that is not running has no address to scrape.
				map[string]any{"source_labels": strs("__meta_kubernetes_pod_phase"), "regex": "Running", "action": "keep"},
				// One target per pod: the declared container port.
				map[string]any{"source_labels": strs("__meta_kubernetes_pod_container_port_number"), "regex": strconv.Itoa(m.Port), "action": "keep"},
				map[string]any{"source_labels": strs("__meta_kubernetes_pod_name"), "target_label": "pod", "action": "replace"},
				map[string]any{"source_labels": strs("__meta_kubernetes_namespace"), "target_label": "namespace", "action": "replace"},
			}
		} else {
			job["static_configs"] = []any{map[string]any{"targets": strs(lit(net.JoinHostPort(t.Address, strconv.Itoa(m.Port))))}}
		}
		if mr := metricRelabels(m); len(mr) > 0 {
			job["metric_relabel_configs"] = mr
		}
		jobs = append(jobs, job)
	}
	return map[string]any{"config": map[string]any{"scrape_configs": jobs}}, nil
}

// podSD is t's Kubernetes pod discovery: its namespaces, its selector
// (server-side), and on a per-node collector, its node.
func (b *builder) podSD(t telemetry.Target) map[string]any {
	sel := map[string]any{"role": "pod", "label": labelSelector(t.Selector)}
	if b.opts.Discovery == DiscoverNodePods {
		sel["field"] = "spec.nodeName=${env:" + b.opts.NodeNameEnv + "}"
	}
	sd := map[string]any{"role": "pod", "selectors": []any{sel}}
	if len(t.Namespaces) > 0 {
		sd["namespaces"] = map[string]any{"names": strs(t.Namespaces...)}
	}
	return sd
}

// labelSelector is sel as a Kubernetes equality selector, keys sorted.
func labelSelector(sel map[string]string) string {
	parts := make([]string, 0, len(sel))
	for _, k := range sortedKeys(sel) {
		parts = append(parts, k+"="+sel[k])
	}
	return strings.Join(parts, ",")
}

// metricRelabels keep `up` and the allowed names, drop the denied ones, and
// remove the dropped labels. Prometheus anchors each regex.
func metricRelabels(m telemetry.MetricsScrape) []any {
	var out []any
	if m.Allow != "" {
		out = append(out, map[string]any{"source_labels": strs("__name__"), "regex": lit("up|(?:" + m.Allow + ")"), "action": "keep"})
	}
	if m.Deny != "" {
		out = append(out, map[string]any{"source_labels": strs("__name__"), "regex": lit(m.Deny), "action": "drop"})
	}
	if len(m.DropLabels) > 0 {
		quoted := make([]string, len(m.DropLabels))
		for i, l := range m.DropLabels {
			quoted[i] = regexp.QuoteMeta(l)
		}
		sort.Strings(quoted)
		out = append(out, map[string]any{"regex": lit(strings.Join(quoted, "|")), "action": "labeldrop"})
	}
	return out
}

// checks renders t's health checks: a tcpcheck or httpcheck receiver per
// check, through a receiver_creator when t's pods are discovered. It returns
// the receivers' names.
func (b *builder) checks(t telemetry.Target) []string {
	if len(t.Checks) == 0 {
		return nil
	}
	svc := t.ServiceName
	if b.pods(t) {
		b.observerUsed = true
		templates := map[string]any{}
		for i, c := range t.Checks {
			kind := c.Kind + "check"
			templates[kind+"/"+checkName(c, i)] = map[string]any{
				"rule":   lit(podPortRule(t, c.Port)),
				"config": checkConfig(c, "`endpoint`"),
			}
		}
		name := "receiver_creator/telemetry_" + svc + "_checks"
		b.f.Receivers[name] = map[string]any{"watch_observers": strs(ObserverExtension), "receivers": templates}
		return []string{name}
	}
	var names []string
	for i, c := range t.Checks {
		name := c.Kind + "check/telemetry_" + svc + "_" + checkName(c, i)
		b.f.Receivers[name] = checkConfig(c, lit(net.JoinHostPort(t.Address, strconv.Itoa(c.Port))))
		names = append(names, name)
	}
	return names
}

func checkName(c telemetry.HealthCheck, i int) string {
	if c.Name != "" {
		return c.Name
	}
	return strconv.Itoa(i)
}

// checkConfig is a check's receiver config against hostport (host:port, or
// receiver_creator's `endpoint`).
func checkConfig(c telemetry.HealthCheck, hostport string) map[string]any {
	endpoint := hostport
	target := map[string]any{"endpoint": endpoint}
	if c.Kind == "http" {
		target = map[string]any{"endpoint": c.Scheme + "://" + hostport + lit(c.Path), "method": "GET"}
	}
	return map[string]any{"collection_interval": c.Interval, "targets": []any{target}}
}

// podPortRule is receiver_creator's rule for port endpoints of t's pods on
// port: in its namespaces, carrying every selector label.
func podPortRule(t telemetry.Target, port int) string {
	parts := []string{`type == "port"`}
	if len(t.Namespaces) > 0 {
		quoted := make([]string, len(t.Namespaces))
		for i, n := range t.Namespaces {
			quoted[i] = strconv.Quote(n)
		}
		parts = append(parts, "pod.namespace in ["+strings.Join(quoted, ", ")+"]")
	}
	for _, k := range sortedKeys(t.Selector) {
		parts = append(parts, fmt.Sprintf("pod.labels[%s] == %s", strconv.Quote(k), strconv.Quote(t.Selector[k])))
	}
	parts = append(parts, "port == "+strconv.Itoa(port))
	return strings.Join(parts, " && ")
}
