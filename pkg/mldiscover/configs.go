package mldiscover

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// CRD of the OpenTelemetry Operator's collector resource.
const (
	CRDOTelCollector       = "opentelemetrycollectors.opentelemetry.io"
	CRDOTelInstrumentation = "instrumentations.opentelemetry.io"
	CRDClusterPolicy       = "clusterpolicies.nvidia.com"
)

// ComponentConfig.Settings keys the coverage reads.
const (
	SettingKubernetesAttribution = "kubernetes-attribution"
	SettingGPURules              = "gpu-rules"
)

// dcgmBuiltinDir is where the dcgm-exporter image ships its counters files.
const dcgmBuiltinDir = "/etc/dcgm-exporter/"

// podTemplate returns the pod spec and labels of a component's workload:
// its DaemonSet, Deployment or StatefulSet template, else the spec of a pod
// it owns.
func podTemplate(s *Snapshot, c *Component) (*corev1.PodSpec, map[string]string) {
	kind, name, ok := strings.Cut(c.Workload, "/")
	if !ok {
		return nil, nil
	}
	switch kind {
	case WorkloadDaemonSet:
		for i := range s.DaemonSets {
			w := &s.DaemonSets[i]
			if w.Namespace == c.Namespace && w.Name == name {
				return &w.Spec.Template.Spec, w.Spec.Template.Labels
			}
		}
	case WorkloadDeployment:
		for i := range s.Deployments {
			w := &s.Deployments[i]
			if w.Namespace == c.Namespace && w.Name == name {
				return &w.Spec.Template.Spec, w.Spec.Template.Labels
			}
		}
	case WorkloadStatefulSet:
		for i := range s.StatefulSets {
			w := &s.StatefulSets[i]
			if w.Namespace == c.Namespace && w.Name == name {
				return &w.Spec.Template.Spec, w.Spec.Template.Labels
			}
		}
	}
	for _, p := range componentPods(s, c) {
		return &p.Spec, p.Labels
	}
	return nil, nil
}

// componentPods returns the active pods a component's workload owns,
// sorted by namespace/name.
func componentPods(s *Snapshot, c *Component) []*corev1.Pod {
	kind, name, ok := strings.Cut(c.Workload, "/")
	if !ok {
		return nil
	}
	var out []*corev1.Pod
	for i := range s.Pods {
		p := &s.Pods[i]
		if p.Namespace != c.Namespace || !PodActive(p) {
			continue
		}
		if o := PodOwner(s, p); o.Kind == kind && o.Name == name {
			out = append(out, p)
		}
	}
	sortPods(out)
	return out
}

// mainContainer picks the container a signature matched: the first whose
// image or name contains hint, else the first.
func mainContainer(spec *corev1.PodSpec, hints ...string) *corev1.Container {
	if spec == nil || len(spec.Containers) == 0 {
		return nil
	}
	for _, h := range hints {
		for i := range spec.Containers {
			c := &spec.Containers[i]
			if strings.Contains(strings.ToLower(c.Image), h) || strings.Contains(c.Name, h) {
				return c
			}
		}
	}
	return &spec.Containers[0]
}

// configMapVolumes lists the ConfigMaps a pod spec mounts (directly or
// through a projected volume), sorted and unique.
func configMapVolumes(spec *corev1.PodSpec) []string {
	set := map[string]bool{}
	for _, v := range spec.Volumes {
		if v.ConfigMap != nil && v.ConfigMap.Name != "" {
			set[v.ConfigMap.Name] = true
		}
		if v.Projected != nil {
			for _, src := range v.Projected.Sources {
				if src.ConfigMap != nil && src.ConfigMap.Name != "" {
					set[src.ConfigMap.Name] = true
				}
			}
		}
	}
	return sortedKeys(set)
}

// resolveFile maps a file path in a container to the ConfigMap and key it
// is mounted from. ok is false when no ConfigMap volume covers it.
func resolveFile(spec *corev1.PodSpec, c *corev1.Container, file string) (cm, key string, ok bool) {
	if spec == nil || c == nil || file == "" {
		return "", "", false
	}
	for _, vm := range c.VolumeMounts {
		mp := strings.TrimSuffix(vm.MountPath, "/")
		var rel string
		switch {
		case vm.SubPath != "" && file == mp:
			rel = vm.SubPath
		case vm.SubPath == "" && strings.HasPrefix(file, mp+"/"):
			rel = strings.TrimPrefix(file, mp+"/")
		default:
			continue
		}
		for _, v := range spec.Volumes {
			if v.Name != vm.Name {
				continue
			}
			if v.ConfigMap != nil {
				return v.ConfigMap.Name, keyFor(v.ConfigMap.Items, rel), true
			}
			if v.Projected != nil {
				for _, src := range v.Projected.Sources {
					if src.ConfigMap == nil {
						continue
					}
					k := keyFor(src.ConfigMap.Items, rel)
					if len(src.ConfigMap.Items) == 0 || k != rel || hasItemPath(src.ConfigMap.Items, rel) {
						return src.ConfigMap.Name, k, true
					}
				}
			}
		}
	}
	return "", "", false
}

func keyFor(items []corev1.KeyToPath, rel string) string {
	for _, it := range items {
		if it.Path == rel {
			return it.Key
		}
	}
	return rel
}

func hasItemPath(items []corev1.KeyToPath, rel string) bool {
	for _, it := range items {
		if it.Path == rel {
			return true
		}
	}
	return false
}

// readMounted returns the content of a file a container reads from a
// mounted ConfigMap discovery read, and where it came from.
func readMounted(s *Snapshot, c *Component, spec *corev1.PodSpec, ctr *corev1.Container, file string) (text, source string, ok bool) {
	cmName, key, ok := resolveFile(spec, ctr, file)
	if !ok {
		return "", "", false
	}
	cm := configMap(s, c.Namespace, cmName)
	if cm == nil {
		return "", "", false
	}
	text, ok = cm.Data[key]
	return text, cmSource(c.Namespace, cmName, key), ok
}

// configMap returns the snapshot's copy of a ConfigMap.
func configMap(s *Snapshot, namespace, name string) *corev1.ConfigMap {
	for i := range s.ConfigMaps {
		cm := &s.ConfigMaps[i]
		if cm.Namespace == namespace && cm.Name == name {
			return cm
		}
	}
	return nil
}

// cmKeys lists a ConfigMap's data keys, sorted.
func cmKeys(cm *corev1.ConfigMap) []string {
	keys := make([]string, 0, len(cm.Data))
	for k := range cm.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func cmSource(ns, name, key string) string {
	return fmt.Sprintf("ConfigMap/%s/%s key %s", ns, name, key)
}

// attachConfigs fills Component.Config for every component whose
// configuration discovery can read.
func (d *discoverer) attachConfigs() {
	for i := range d.comps {
		c := &d.comps[i]
		if c.Status != StatusPresent {
			continue
		}
		switch c.Kind {
		case KindDCGMExporter:
			c.Config = dcgmConfig(d.s, c)
		case KindNodeProblemDetector:
			c.Config = npdConfig(d.s, c)
		case KindOTelCollector, KindOTelEBPFProfiler:
			c.Config = collectorConfig(d.s, c)
		case KindAlloy:
			c.Config = alloyConfig(d.s, c)
		case KindDatadogAgent:
			c.Config = datadogConfig(d.s, c)
		}
	}
}

// --- DCGM exporter -----------------------------------------------------

// probesFor returns the successful probes of a component's pods.
func probesFor(s *Snapshot, c *Component) []MetricsProbe {
	pods := map[string]bool{}
	for _, p := range componentPods(s, c) {
		pods[p.Name] = true
	}
	var out []MetricsProbe
	for _, p := range s.Probes {
		if p.Kind != c.Kind || p.Namespace != c.Namespace || p.Error != "" {
			continue
		}
		_, name, _ := strings.Cut(c.Workload, "/")
		if pods[p.Pod] || (len(pods) == 0 && name != "" && strings.HasPrefix(p.Pod, name+"-")) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pod < out[j].Pod })
	return out
}

func dcgmConfig(s *Snapshot, c *Component) *ComponentConfig {
	spec, _ := podTemplate(s, c)
	ctr := mainContainer(spec, "dcgm-exporter", "exporter")
	attr, attrBy := dcgmAttribution(s, c, ctr, spec)
	cfg := dcgmFieldSource(s, c, spec, ctr)
	if cfg.DeterminedBy == DeterminedByConfig {
		// A config file's enrichment beats a missing flag.
		if a, by := dcgmConfigAttribution(s, c, spec, ctr); by != "" && attrBy != DeterminedByProbe && attrBy != DeterminedByArgs {
			attr, attrBy = a, by
		}
	}
	if attr != "" {
		cfg.Settings = append(cfg.Settings, SettingKubernetesAttribution+"="+attr, SettingKubernetesAttribution+".determined-by="+attrBy)
	}
	if ctr != nil {
		if v, ok := envValue(ctr, "DCGM_EXPORTER_INTERVAL"); ok && v != "" {
			cfg.Settings = append(cfg.Settings, "interval="+v+"ms")
		}
	}
	sort.Strings(cfg.Settings)
	return cfg
}

// dcgmFieldSource establishes the exporter's field set, strongest source
// first: probe, config, version-default, presence.
func dcgmFieldSource(s *Snapshot, c *Component, spec *corev1.PodSpec, ctr *corev1.Container) *ComponentConfig {
	if cfg := dcgmFromProbe(s, c); cfg != nil {
		return cfg
	}
	file := dcgmCountersFile(ctr)
	r := dcgmReader{s: s, c: c, spec: spec, ctr: ctr}
	if file != "" {
		if cmName, key, ok := resolveFile(spec, ctr, file); ok {
			if cfg := r.csv(cmName, key, ""); cfg != nil {
				return cfg
			}
		}
	}
	if c.Via == ViaGPUOperator {
		for _, name := range clusterPolicyMetricsConfigs(s) {
			if cfg := r.csv(name, "dcgm-metrics.csv", " (ClusterPolicy spec.dcgmExporter.config)"); cfg != nil {
				return cfg
			}
		}
	}
	if cfg, builtin := r.exporterConfig(); cfg != nil {
		return cfg
	} else if builtin != "" {
		file = builtin
	}
	if file == "" {
		if cfg := r.anyCSV(); cfg != nil {
			return cfg
		}
	}
	return dcgmDefault(s, c, file)
}

func dcgmFromProbe(s *Snapshot, c *Component) *ComponentConfig {
	probes := probesFor(s, c)
	if len(probes) == 0 {
		return nil
	}
	set := map[string]bool{}
	for _, p := range probes {
		for _, f := range p.Families {
			if dcgmFieldRE.MatchString(f) {
				set[f] = true
			}
		}
	}
	p := probes[0]
	return &ComponentConfig{DeterminedBy: DeterminedByProbe, Fields: sortedKeys(set),
		Source: fmt.Sprintf("probe %s/%s:%d%s", p.Namespace, p.Pod, p.Port, p.Path)}
}

// dcgmCountersFile is the counters file the exporter is told to read
// (-f/--collectors beats DCGM_EXPORTER_COLLECTORS).
func dcgmCountersFile(ctr *corev1.Container) string {
	if ctr == nil {
		return ""
	}
	if v, ok := FlagValue(containerArgs(ctr), "f", "collectors"); ok && v != "" {
		return v
	}
	if v, ok := envValue(ctr, "DCGM_EXPORTER_COLLECTORS"); ok {
		return v
	}
	return ""
}

// dcgmReader reads an exporter's mounted ConfigMaps.
type dcgmReader struct {
	s    *Snapshot
	c    *Component
	spec *corev1.PodSpec
	ctr  *corev1.Container
}

func (r dcgmReader) csv(name, key, why string) *ComponentConfig {
	cm := configMap(r.s, r.c.Namespace, name)
	if cm == nil {
		return nil
	}
	text, ok := cm.Data[key]
	if !ok {
		return nil
	}
	return &ComponentConfig{DeterminedBy: DeterminedByConfig, Fields: ParseDCGMCounters(text), Source: cmSource(r.c.Namespace, name, key) + why}
}

// mounted lists (ConfigMap, key) pairs of the exporter's mounted
// ConfigMaps that discovery read.
func (r dcgmReader) mounted() [][2]string {
	if r.spec == nil {
		return nil
	}
	var out [][2]string
	for _, name := range configMapVolumes(r.spec) {
		if cm := configMap(r.s, r.c.Namespace, name); cm != nil {
			for _, key := range cmKeys(cm) {
				out = append(out, [2]string{name, key})
			}
		}
	}
	return out
}

// exporterConfig reads a mounted dcgm-exporter config file (the 4.9
// chart's config.yaml): its fields and the counters files it names. When
// it names only a built-in file, that path is returned instead.
func (r dcgmReader) exporterConfig() (*ComponentConfig, string) {
	builtin := ""
	for _, nk := range r.mounted() {
		ec, ok := ParseDCGMExporterConfig(configMap(r.s, r.c.Namespace, nk[0]).Data[nk[1]])
		if !ok {
			continue
		}
		fields := setOf(ec.Fields)
		src := cmSource(r.c.Namespace, nk[0], nk[1])
		for _, f := range ec.Files {
			cmName, k, ok := resolveFile(r.spec, r.ctr, f)
			if !ok {
				if strings.HasPrefix(f, dcgmBuiltinDir) && len(ec.Fields) == 0 {
					builtin = f
				}
				continue
			}
			if sub := r.csv(cmName, k, ""); sub != nil {
				for _, x := range sub.Fields {
					fields[x] = true
				}
				src += ", " + sub.Source
			}
		}
		if len(fields) > 0 {
			return &ComponentConfig{DeterminedBy: DeterminedByConfig, Fields: sortedKeys(fields), Source: src}, ""
		}
	}
	return nil, builtin
}

// anyCSV reads the first mounted *.csv key that holds DCGM fields.
func (r dcgmReader) anyCSV() *ComponentConfig {
	for _, nk := range r.mounted() {
		if !strings.HasSuffix(nk[1], ".csv") {
			continue
		}
		if f := ParseDCGMCounters(configMap(r.s, r.c.Namespace, nk[0]).Data[nk[1]]); len(f) > 0 {
			return &ComponentConfig{DeterminedBy: DeterminedByConfig, Fields: f, Source: cmSource(r.c.Namespace, nk[0], nk[1])}
		}
	}
	return nil
}

// dcgmDefault is the field set when no config is readable: GKE's fixed
// managed set, else the built-in counters file of the found release.
func dcgmDefault(s *Snapshot, c *Component, file string) *ComponentConfig {
	if c.Via == ViaGKEManaged {
		return &ComponentConfig{DeterminedBy: DeterminedByVersionDefault, Fields: append([]string(nil), GKEManagedDCGMFields...), Source: GKEManagedDCGMSource}
	}
	version := c.Version
	if version == "" && c.Via == ViaGPUOperator {
		version = clusterPolicyExporterVersion(s)
	}
	if file == "" {
		file = dcgmBuiltinDir + DCGMDefaultCounters
		if c.Via == ViaGPUOperator && operatorUsesDCP(s) {
			file = dcgmBuiltinDir + DCGMDCPIncluded
		}
	}
	derived := c.Via == ViaGPUOperator && c.Workload == "" && version != ""
	if strings.HasPrefix(file, dcgmBuiltinDir) && (isDCGMExporterImage(s, c) || derived) {
		if fields, tag, ok := DCGMDefaultFields(version, path.Base(file)); ok {
			return &ComponentConfig{DeterminedBy: DeterminedByVersionDefault, Fields: fields,
				Source: fmt.Sprintf("dcgm-exporter %s etc/%s (default of %s)", tag, path.Base(file), version)}
		}
	}
	src := ""
	if !strings.HasPrefix(file, dcgmBuiltinDir) {
		src = "counters file " + file + " is not in a ConfigMap discovery read"
	}
	return &ComponentConfig{DeterminedBy: DeterminedByPresence, Source: src}
}

// isDCGMExporterImage reports whether the component runs NVIDIA's
// dcgm-exporter image (not a look-alike such as fake-gpu-operator's
// status exporter, whose field set no default describes).
func isDCGMExporterImage(s *Snapshot, c *Component) bool {
	spec, _ := podTemplate(s, c)
	if spec == nil {
		return false
	}
	for _, ctr := range spec.Containers {
		if strings.Contains(imageRepo(ctr.Image), "dcgm-exporter") {
			return true
		}
	}
	return false
}

func dcgmAttribution(s *Snapshot, c *Component, ctr *corev1.Container, spec *corev1.PodSpec) (string, string) {
	if probes := probesFor(s, c); len(probes) > 0 {
		for _, k := range probes[0].LabelKeys {
			if k == "pod" || k == "exported_pod" {
				return "true", DeterminedByProbe
			}
		}
		return "false", DeterminedByProbe
	}
	if v, ok := dcgmAttributionArgs(ctr); ok {
		return v, DeterminedByArgs
	}
	if c.Via == ViaGKEManaged {
		return "unknown", DeterminedByPresence
	}
	if c.Via == ViaGPUOperator {
		// Every GPU Operator release's exporter asset sets
		// DCGM_EXPORTER_KUBERNETES=true.
		return "true", DeterminedByVersionDefault
	}
	if ctr != nil && isDCGMExporterImage(s, c) {
		if _, ok := envValue(ctr, "DCGM_EXPORTER_CONFIG_FILE"); !ok {
			// The exporter's --kubernetes defaults to false.
			return "false", DeterminedByVersionDefault
		}
	}
	return "unknown", DeterminedByPresence
}

// dcgmAttributionArgs reads DCGM_EXPORTER_KUBERNETES or --kubernetes.
func dcgmAttributionArgs(ctr *corev1.Container) (string, bool) {
	if ctr == nil {
		return "", false
	}
	if v, ok := envValue(ctr, "DCGM_EXPORTER_KUBERNETES"); ok && v != "" {
		if b, ok := parseBool(v); ok {
			return strconv.FormatBool(b), true
		}
	}
	v, ok := FlagValue(containerArgs(ctr), "k", "kubernetes")
	if !ok {
		return "", false
	}
	if v == "" {
		return "true", true
	}
	if b, ok := parseBool(v); ok {
		return strconv.FormatBool(b), true
	}
	return "", false
}

func dcgmConfigAttribution(s *Snapshot, c *Component, spec *corev1.PodSpec, ctr *corev1.Container) (string, string) {
	if spec == nil {
		return "", ""
	}
	for _, name := range configMapVolumes(spec) {
		cm := configMap(s, c.Namespace, name)
		if cm == nil {
			continue
		}
		for _, key := range cmKeys(cm) {
			if ec, ok := ParseDCGMExporterConfig(cm.Data[key]); ok && ec.Kubernetes != "" {
				switch ec.Kubernetes {
				case "auto", "true", "on", "enabled":
					return "true", DeterminedByConfig
				default:
					return "false", DeterminedByConfig
				}
			}
		}
	}
	return "", ""
}

// clusterPolicyMetricsConfigs returns the ClusterPolicies'
// spec.dcgmExporter.config.name values, sorted.
func clusterPolicyMetricsConfigs(s *Snapshot) []string {
	set := map[string]bool{}
	for _, obj := range s.CustomObjects[CRDClusterPolicy] {
		spec, _ := obj["spec"].(map[string]any)
		ex, _ := spec["dcgmExporter"].(map[string]any)
		conf, _ := ex["config"].(map[string]any)
		if name, _ := conf["name"].(string); name != "" && name != "REDACTED" {
			set[name] = true
		}
	}
	return sortedKeys(set)
}

func clusterPolicyExporterVersion(s *Snapshot) string {
	for _, obj := range s.CustomObjects[CRDClusterPolicy] {
		spec, _ := obj["spec"].(map[string]any)
		ex, _ := spec["dcgmExporter"].(map[string]any)
		if v, _ := ex["version"].(string); v != "" {
			return v
		}
	}
	return ""
}

// operatorUsesDCP reports whether the GPU Operator found configures its
// exporter with dcp-metrics-included.csv: every release from v24.6 through
// v26.3 does; v26.7 switched to default-counters.csv (the
// state-dcgm-exporter DaemonSet assets at those tags, checked 2026-11-26).
func operatorUsesDCP(s *Snapshot) bool {
	for i := range s.Deployments {
		dp := &s.Deployments[i]
		for _, ctr := range dp.Spec.Template.Spec.Containers {
			if !glob("*/gpu-operator", imageRepo(ctr.Image)) {
				continue
			}
			m := exporterVersionRE.FindStringSubmatch(strings.TrimPrefix(ImageTag(ctr.Image), "v"))
			if m == nil {
				return true
			}
			major, _ := strconv.Atoi(m[1])
			minor, _ := strconv.Atoi(m[2])
			return major < 26 || (major == 26 && minor < 7)
		}
	}
	return true
}

// --- node-problem-detector ---------------------------------------------

func npdConfig(s *Snapshot, c *Component) *ComponentConfig {
	spec, _ := podTemplate(s, c)
	ctr := mainContainer(spec, "node-problem-detector")
	if ctr == nil {
		// Managed NPD (GKE) runs on the node: only its conditions show.
		return &ComponentConfig{DeterminedBy: DeterminedByPresence, Source: "node conditions", Conditions: nonCoreConditions(s)}
	}
	cfg := &ComponentConfig{DeterminedBy: DeterminedByPresence}
	monitors, conds, reasons := map[string]bool{}, map[string]bool{}, map[string]bool{}
	gpu, gpuKnown := false, true
	var sources []string
	read := func(name, text, source string) bool {
		m, ok := ParseNPDMonitor(name, text)
		if !ok {
			return false
		}
		monitors[m.Name] = true
		for _, x := range m.Conditions {
			conds[x] = true
		}
		for _, x := range m.Reasons {
			reasons[x] = true
		}
		gpu = gpu || m.GPURule
		sources = append(sources, source)
		cfg.DeterminedBy = DeterminedByConfig
		return true
	}
	refs := NPDMonitorArgs(containerArgs(ctr))
	for _, ref := range refs {
		if cmName, key, ok := resolveFile(spec, ctr, ref.Path); ok {
			if cm := configMap(s, c.Namespace, cmName); cm != nil {
				if text, ok := cm.Data[key]; ok && read(key, text, cmSource(c.Namespace, cmName, key)) {
					continue
				}
			}
			monitors[strings.TrimSuffix(path.Base(ref.Path), ".json")] = true
			gpuKnown = false
			continue
		}
		monitors[strings.TrimSuffix(path.Base(ref.Path), ".json")] = true
		if !strings.HasPrefix(ref.Path, npdBuiltinPrefix) {
			gpuKnown = false
		}
		if cfg.DeterminedBy == DeterminedByPresence {
			cfg.DeterminedBy = DeterminedByArgs
		}
	}
	if len(refs) == 0 && spec != nil {
		for _, name := range configMapVolumes(spec) {
			if cm := configMap(s, c.Namespace, name); cm != nil {
				for _, key := range cmKeys(cm) {
					read(key, cm.Data[key], cmSource(c.Namespace, name, key))
				}
			}
		}
		if cfg.DeterminedBy == DeterminedByPresence {
			gpuKnown = false
		}
	}
	cfg.Monitors = sortedKeys(monitors)
	cfg.Conditions = sortedKeys(conds)
	cfg.Reasons = sortedKeys(reasons)
	switch {
	case gpu:
		cfg.Settings = append(cfg.Settings, SettingGPURules+"=true")
	case gpuKnown && len(monitors) > 0:
		cfg.Settings = append(cfg.Settings, SettingGPURules+"=false")
	default:
		cfg.Settings = append(cfg.Settings, SettingGPURules+"=unknown")
	}
	if port, reachable := NPDPrometheusEndpoint(containerArgs(ctr)); port > 0 {
		cfg.Settings = append(cfg.Settings, fmt.Sprintf("prometheus-port=%d", port))
		if !reachable {
			cfg.Settings = append(cfg.Settings, "prometheus-address=loopback")
		}
	}
	if len(probesFor(s, c)) > 0 {
		cfg.Settings = append(cfg.Settings, "metrics=probed")
	}
	sort.Strings(cfg.Settings)
	if len(sources) > 0 {
		sort.Strings(sources)
		cfg.Source = strings.Join(sources, ", ")
	} else if len(refs) > 0 {
		cfg.Source = "args"
	}
	return cfg
}

// nonCoreConditions lists the node condition types node-problem-detector
// sets out of the box that any node reports, sorted.
func nonCoreConditions(s *Snapshot) []string {
	set := map[string]bool{}
	for _, n := range s.Nodes {
		for _, c := range n.Status.Conditions {
			if IsNPDDefaultCondition(string(c.Type)) {
				set[string(c.Type)] = true
			}
		}
	}
	return sortedKeys(set)
}

// --- collectors --------------------------------------------------------

// collectorCR returns the OpenTelemetryCollector resource that manages a
// component (the operator names its workload <name>-collector).
func collectorCR(s *Snapshot, c *Component) (map[string]any, string) {
	_, wname, _ := strings.Cut(c.Workload, "/")
	for _, obj := range s.CustomObjects[CRDOTelCollector] {
		meta, _ := obj["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		ns, _ := meta["namespace"].(string)
		if ns == c.Namespace && (wname == name+"-collector" || wname == name) {
			return obj, fmt.Sprintf("OpenTelemetryCollector/%s/%s spec.config", ns, name)
		}
	}
	return nil, ""
}

func fromCollector(cc CollectorConfig, by, source string) *ComponentConfig {
	return &ComponentConfig{DeterminedBy: by, Source: source, Receivers: cc.Receivers, Processors: cc.Processors,
		Exporters: cc.Exporters, Pipelines: cc.Pipelines, Settings: cc.Settings}
}

func collectorConfig(s *Snapshot, c *Component) *ComponentConfig {
	if obj, src := collectorCR(s, c); obj != nil {
		spec, _ := obj["spec"].(map[string]any)
		if cc, ok := CollectorConfigFromObject(spec["config"]); ok {
			return fromCollector(cc, DeterminedByConfig, src)
		}
	}
	spec, _ := podTemplate(s, c)
	ctr := mainContainer(spec, "otel", "collector")
	if ctr != nil {
		for _, a := range configArgs(containerArgs(ctr)) {
			if text, src, ok := readMounted(s, c, spec, ctr, a); ok {
				if cc, ok := ParseCollectorConfig(text); ok {
					return fromCollector(cc, DeterminedByConfig, src)
				}
			}
		}
	}
	if spec != nil {
		for _, name := range configMapVolumes(spec) {
			if cm := configMap(s, c.Namespace, name); cm != nil {
				for _, key := range cmKeys(cm) {
					if cc, ok := ParseCollectorConfig(cm.Data[key]); ok {
						return fromCollector(cc, DeterminedByConfig, cmSource(c.Namespace, name, key))
					}
				}
			}
		}
	}
	return &ComponentConfig{DeterminedBy: DeterminedByPresence}
}

// configArgs returns the file paths a collector's --config flags name
// (skipping env:, yaml: and http(s): providers).
func configArgs(args []string) []string {
	var out []string
	var fields []string
	for _, a := range args {
		fields = append(fields, strings.Fields(a)...)
	}
	for i, f := range fields {
		n, v, has := strings.Cut(f, "=")
		if n != "--config" && n != "-config" && n != "-c" {
			continue
		}
		if !has && i+1 < len(fields) {
			v = fields[i+1]
		}
		v = strings.TrimPrefix(v, "file:")
		if strings.HasPrefix(v, "/") {
			out = append(out, v)
		}
	}
	return out
}

func alloyConfig(s *Snapshot, c *Component) *ComponentConfig {
	spec, _ := podTemplate(s, c)
	ctr := mainContainer(spec, "alloy")
	fromAlloy := func(ac AlloyConfig, src string) *ComponentConfig {
		return &ComponentConfig{DeterminedBy: DeterminedByConfig, Source: src, Receivers: ac.Receivers, Processors: ac.Processors, Exporters: ac.Exporters}
	}
	if ctr != nil {
		args := containerArgs(ctr)
		for i, a := range args {
			if a != "run" || i+1 >= len(args) {
				continue
			}
			if text, src, ok := readMounted(s, c, spec, ctr, args[i+1]); ok {
				if ac, ok := ParseAlloyConfig(text); ok {
					return fromAlloy(ac, src)
				}
			}
		}
	}
	if spec != nil {
		for _, name := range configMapVolumes(spec) {
			if cm := configMap(s, c.Namespace, name); cm != nil {
				for _, key := range cmKeys(cm) {
					if ac, ok := ParseAlloyConfig(cm.Data[key]); ok {
						return fromAlloy(ac, cmSource(c.Namespace, name, key))
					}
				}
			}
		}
	}
	return &ComponentConfig{DeterminedBy: DeterminedByPresence}
}

// datadogConfig reads what the Datadog agent's env var names say (values
// are blanked by the collector).
func datadogConfig(s *Snapshot, c *Component) *ComponentConfig {
	spec, _ := podTemplate(s, c)
	ctr := mainContainer(spec, "agent")
	if ctr == nil {
		return &ComponentConfig{DeterminedBy: DeterminedByPresence}
	}
	names := envNames(ctr)
	cfg := &ComponentConfig{DeterminedBy: DeterminedByArgs, Source: "env names"}
	if names["DD_OTLP_CONFIG_RECEIVER_PROTOCOLS_GRPC_ENDPOINT"] || names["DD_OTLP_CONFIG_RECEIVER_PROTOCOLS_HTTP_ENDPOINT"] {
		cfg.Receivers = append(cfg.Receivers, "datadog-otlp")
	}
	if names["DD_COLLECT_KUBERNETES_EVENTS"] {
		cfg.Receivers = append(cfg.Receivers, "datadog-k8s-events")
	}
	if names["DD_LOGS_CONFIG_CONTAINER_COLLECT_ALL"] {
		cfg.Receivers = append(cfg.Receivers, "datadog-container-logs")
	}
	sort.Strings(cfg.Receivers)
	for _, n := range []string{"DD_LOGS_ENABLED", "DD_APM_ENABLED", "DD_PROCESS_AGENT_ENABLED"} {
		if names[n] {
			cfg.Settings = append(cfg.Settings, strings.ToLower(strings.TrimPrefix(n, "DD_"))+"=set")
		}
	}
	return cfg
}

// matchCollectorCRs adds an otel-collector component for each
// OpenTelemetryCollector resource whose workload the snapshot does not
// hold (the operator creates it as <name>-collector).
func (d *discoverer) matchCollectorCRs() {
	for _, obj := range d.s.CustomObjects[CRDOTelCollector] {
		meta, _ := obj["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		ns, _ := meta["namespace"].(string)
		if name == "" {
			continue
		}
		spec, _ := obj["spec"].(map[string]any)
		mode, _ := spec["mode"].(string)
		kind := WorkloadDeployment
		switch mode {
		case "daemonset":
			kind = WorkloadDaemonSet
		case "statefulset":
			kind = WorkloadStatefulSet
		case "sidecar":
			continue // injected into workloads; no collector of its own
		}
		wl := kind + "/" + name + "-collector"
		found := false
		for _, c := range d.ofKind(KindOTelCollector) {
			if c.Namespace == ns && c.Workload == wl {
				c.MatchedBy = append(c.MatchedBy, "cr:"+CRDOTelCollector)
				found = true
			}
		}
		if !found {
			labels, _ := meta["labels"].(map[string]any)
			owner := OwnerTheirs
			if _, ok := labels[LabelComponentOwner]; ok {
				owner = OwnerForge
			}
			d.add(Component{Kind: KindOTelCollector, Category: CategoryMonitoring, Status: StatusPresent, Owner: owner,
				Namespace: ns, Workload: wl, MatchedBy: []string{"cr:" + CRDOTelCollector}})
		}
	}
}
