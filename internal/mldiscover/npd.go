package mldiscover

import (
	"encoding/json"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// NPDDefaultPrometheusPort is node-problem-detector's --prometheus-port
// default (kubernetes/node-problem-detector README, checked 2026-11-26).
// Its --prometheus-address defaults to 127.0.0.1, so the endpoint is only
// reachable through the pod network when a chart binds 0.0.0.0 (the
// deliveryhero chart does).
const NPDDefaultPrometheusPort = 20257

// NPD monitor kinds, from the flag that names the file.
const (
	NPDSystemLogMonitor    = "system-log-monitor"
	NPDCustomPluginMonitor = "custom-plugin-monitor"
	NPDSystemStatsMonitor  = "system-stats-monitor"
)

// NPDMonitorRef is one monitor config file NPD's args name.
type NPDMonitorRef struct {
	// Type is one of the NPD*Monitor constants.
	Type string
	// Path is the file path in the container.
	Path string
}

// NPDMonitorArgs reads the monitor config paths from node-problem-detector's
// command and args: --config.system-log-monitor, --config.custom-plugin-monitor,
// --config.system-stats-monitor (and the deprecated --system-log-monitors /
// --custom-plugin-monitors), comma-separated, `=` or a separate value.
func NPDMonitorArgs(args []string) []NPDMonitorRef {
	flags := map[string]string{
		"--config.system-log-monitor":    NPDSystemLogMonitor,
		"--system-log-monitors":          NPDSystemLogMonitor,
		"--config.custom-plugin-monitor": NPDCustomPluginMonitor,
		"--custom-plugin-monitors":       NPDCustomPluginMonitor,
		"--config.system-stats-monitor":  NPDSystemStatsMonitor,
	}
	var out []NPDMonitorRef
	fields := argFields(args)
	for i := 0; i < len(fields); i++ {
		name, val, hasVal := strings.Cut(fields[i], "=")
		typ, ok := flags[longFlag(name)]
		if !ok {
			continue
		}
		if !hasVal && i+1 < len(fields) {
			i++
			val = fields[i]
		}
		for _, p := range strings.Split(val, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, NPDMonitorRef{Type: typ, Path: p})
			}
		}
	}
	return out
}

// argFields splits a container's command and args into whitespace-separated
// words, so a `sh -c "exec … --flag value"` reads like separate args.
func argFields(args []string) []string {
	var out []string
	for _, a := range args {
		out = append(out, strings.Fields(a)...)
	}
	return out
}

// longFlag writes a Go-style single-dash flag with two dashes.
func longFlag(name string) string {
	if strings.HasPrefix(name, "-") && !strings.HasPrefix(name, "--") {
		return "-" + name
	}
	return name
}

// NPDPrometheusEndpoint reads --prometheus-address and --prometheus-port
// from NPD's args: the port (default NPDDefaultPrometheusPort, 0 means
// disabled) and whether the address is reachable from the pod network
// (not loopback).
func NPDPrometheusEndpoint(args []string) (port int32, reachable bool) {
	port, addr := int32(NPDDefaultPrometheusPort), "127.0.0.1"
	fields := argFields(args)
	for i := 0; i < len(fields); i++ {
		name, val, hasVal := strings.Cut(fields[i], "=")
		name = longFlag(name)
		if name != "--prometheus-port" && name != "--prometheus-address" {
			continue
		}
		if !hasVal && i+1 < len(fields) {
			i++
			val = fields[i]
		}
		if name == "--prometheus-port" {
			if n, err := strconv.Atoi(val); err == nil {
				port = int32(n)
			}
		} else {
			addr = val
		}
	}
	return port, port > 0 && addr != "127.0.0.1" && addr != "localhost" && addr != "::1"
}

// NPDMonitor is what one node-problem-detector monitor config declares.
type NPDMonitor struct {
	// Name is the file's base name without .json (kernel-monitor).
	Name string
	// Source is its "source" (kernel-monitor, custom-plugin-monitor).
	Source string
	// Plugin is "kmsg", "journald", "filelog" or "custom".
	Plugin string
	// Conditions are the condition types it declares or sets.
	Conditions []string
	// Reasons are its rules' reasons.
	Reasons []string
	// GPURule is set when a rule turns NVIDIA Xid / NVRM kernel lines (or a
	// GPU health check script) into a problem.
	GPURule bool
}

var gpuPatternRE = regexp.MustCompile(`(?i)NVRM|\bXid\b|nvidia-smi|dcgmi|nvidia.*(gpu|fallen off)`)

// ParseNPDMonitor reads one NPD monitor config (system log or custom
// plugin JSON). ok is false when the text is not one.
func ParseNPDMonitor(name, text string) (NPDMonitor, bool) {
	var doc struct {
		Plugin     string `json:"plugin"`
		Source     string `json:"source"`
		Conditions []struct {
			Type   string `json:"type"`
			Reason string `json:"reason"`
		} `json:"conditions"`
		Rules []struct {
			Type      string `json:"type"`
			Condition string `json:"condition"`
			Reason    string `json:"reason"`
			Pattern   string `json:"pattern"`
			Path      string `json:"path"`
			Args      []any  `json:"args"`
		} `json:"rules"`
		PluginConfig map[string]any `json:"pluginConfig"`
	}
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		return NPDMonitor{}, false
	}
	if doc.Plugin == "" && doc.Source == "" && len(doc.Rules) == 0 && len(doc.Conditions) == 0 {
		return NPDMonitor{}, false
	}
	m := NPDMonitor{Name: strings.TrimSuffix(path.Base(name), ".json"), Source: doc.Source, Plugin: doc.Plugin}
	conds, reasons := map[string]bool{}, map[string]bool{}
	for _, c := range doc.Conditions {
		if c.Type != "" {
			conds[c.Type] = true
		}
	}
	for _, r := range doc.Rules {
		if r.Condition != "" {
			conds[r.Condition] = true
		}
		if r.Reason != "" {
			reasons[r.Reason] = true
		}
		text := r.Pattern + " " + r.Path + " " + r.Reason + " " + r.Condition
		for _, a := range r.Args {
			if s, ok := a.(string); ok {
				text += " " + s
			}
		}
		if gpuPatternRE.MatchString(text) || strings.Contains(strings.ToLower(r.Condition), "gpu") {
			m.GPURule = true
		}
	}
	m.Conditions = sortedKeys(conds)
	m.Reasons = sortedKeys(reasons)
	return m, true
}

// npdBuiltinPrefix is where node-problem-detector's image ships its monitor
// configs. None of the built-in configs matches an NVIDIA Xid line
// (kubernetes/node-problem-detector config/, checked 2026-11-26).
const npdBuiltinPrefix = "/config/"
