package mldiscover

import (
	"bufio"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

// The DCGM exporter's built-in counters files, as the image ships them under
// /etc/dcgm-exporter/.
const (
	DCGMDefaultCounters = "default-counters.csv"
	DCGMDCPIncluded     = "dcp-metrics-included.csv"
)

// dcgmFieldRE is a DCGM field or exporter metric name.
var dcgmFieldRE = regexp.MustCompile(`^DCGM_[A-Z0-9_]+$`)

// ParseDCGMCounters reads a dcgm-exporter counters CSV (`field, type, help`,
// `#` comments) and returns the enabled field names, sorted and unique. A
// line that is not a DCGM field (a REDACTED value, a blank) is skipped.
func ParseDCGMCounters(csv string) []string {
	set := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(csv))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		field, _, _ := strings.Cut(line, ",")
		field = strings.TrimSpace(field)
		if dcgmFieldRE.MatchString(field) {
			set[field] = true
		}
	}
	return sortedKeys(set)
}

// DCGMExporterConfig is what a dcgm-exporter config file (the v2 YAML the
// 4.9 chart renders into DCGM_EXPORTER_CONFIG_FILE) says about metrics.
type DCGMExporterConfig struct {
	// Fields are the metrics[].fields names, sorted.
	Fields []string
	// Files are the metrics[].file paths (a counters CSV to read).
	Files []string
	// Kubernetes is metrics[].enrichment.kubernetes ("auto", "true",
	// "false"), the first one set.
	Kubernetes string
}

// ParseDCGMExporterConfig reads a dcgm-exporter config.yaml. ok is false
// when the text is not one (no `metrics` list).
func ParseDCGMExporterConfig(text string) (DCGMExporterConfig, bool) {
	var doc struct {
		Metrics []struct {
			File   string `json:"file"`
			Fields []struct {
				Name string `json:"name"`
			} `json:"fields"`
			Enrichment map[string]any `json:"enrichment"`
		} `json:"metrics"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil || len(doc.Metrics) == 0 {
		return DCGMExporterConfig{}, false
	}
	var out DCGMExporterConfig
	fields := map[string]bool{}
	for _, m := range doc.Metrics {
		if m.File != "" {
			out.Files = append(out.Files, m.File)
		}
		for _, f := range m.Fields {
			if dcgmFieldRE.MatchString(f.Name) {
				fields[f.Name] = true
			}
		}
		if v, ok := m.Enrichment["kubernetes"]; ok && out.Kubernetes == "" {
			out.Kubernetes = strings.ToLower(strings.TrimSpace(toString(v)))
		}
	}
	out.Fields = sortedKeys(fields)
	return out, true
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	}
	b, _ := yaml.Marshal(v)
	return strings.TrimSpace(string(b))
}

// dcgmDefaultSet is the enabled fields of one counters file at a range of
// verified dcgm-exporter releases.
type dcgmDefaultSet struct {
	// from is the first exporter version (the part after the DCGM version
	// in a tag like 3.3.9-3.6.1) the set applies to.
	from [3]int
	// tags are the upstream tags whose file was checked to hold exactly
	// these fields.
	tags   []string
	file   string
	fields []string
}

// dcgmDefaults are the counters files dcgm-exporter ships, by release.
// Verified 2026-11-26 against github.com/NVIDIA/dcgm-exporter
// etc/default-counters.csv and etc/dcp-metrics-included.csv at every tag
// listed (the fields of each file at those tags are identical). A release
// between two verified ones uses the nearest verified release at or below
// it; one below the oldest has no default.
var dcgmDefaults = []dcgmDefaultSet{
	{from: [3]int{3, 4, 0}, tags: []string{"3.3.5-3.4.0"}, file: DCGMDefaultCounters, fields: dcgmFields335},
	{from: [3]int{3, 6, 1}, tags: []string{"3.3.9-3.6.1"}, file: DCGMDefaultCounters, fields: dcgmFields339},
	{from: [3]int{4, 0, 1}, tags: []string{"4.0.0-4.0.1", "4.1.1-4.0.4", "4.2.3-4.1.3"}, file: DCGMDefaultCounters, fields: dcgmFieldsDCP40},
	{from: [3]int{4, 4, 0}, tags: []string{"4.3.1-4.4.0", "4.4.1-4.6.0", "4.4.2-4.7.1", "4.5.3-4.8.2", "4.6.0-4.8.3", "4.8.4"}, file: DCGMDefaultCounters, fields: dcgmFields44},
	{from: [3]int{4, 9, 0}, tags: []string{"4.9.0"}, file: DCGMDefaultCounters, fields: dcgmFields49},
	{from: [3]int{3, 4, 0}, tags: []string{"3.3.5-3.4.0", "3.3.9-3.6.1", "4.0.0-4.0.1", "4.1.1-4.0.4", "4.2.3-4.1.3"}, file: DCGMDCPIncluded, fields: dcgmFieldsDCP40},
	{from: [3]int{4, 4, 0}, tags: []string{"4.3.1-4.4.0", "4.4.1-4.6.0", "4.4.2-4.7.1", "4.5.3-4.8.2", "4.6.0-4.8.3", "4.8.4"}, file: DCGMDCPIncluded, fields: dcgmFields44},
	{from: [3]int{4, 9, 0}, tags: []string{"4.9.0"}, file: DCGMDCPIncluded, fields: dcgmFields49},
}

var dcgmFields335 = []string{"DCGM_FI_DEV_CORRECTABLE_REMAPPED_ROWS", "DCGM_FI_DEV_DEC_UTIL", "DCGM_FI_DEV_ENC_UTIL", "DCGM_FI_DEV_FB_FREE", "DCGM_FI_DEV_FB_USED", "DCGM_FI_DEV_GPU_TEMP", "DCGM_FI_DEV_GPU_UTIL", "DCGM_FI_DEV_MEMORY_TEMP", "DCGM_FI_DEV_MEM_CLOCK", "DCGM_FI_DEV_MEM_COPY_UTIL", "DCGM_FI_DEV_NVLINK_BANDWIDTH_TOTAL", "DCGM_FI_DEV_PCIE_REPLAY_COUNTER", "DCGM_FI_DEV_PCIE_RX_THROUGHPUT", "DCGM_FI_DEV_PCIE_TX_THROUGHPUT", "DCGM_FI_DEV_POWER_USAGE", "DCGM_FI_DEV_ROW_REMAP_FAILURE", "DCGM_FI_DEV_SM_CLOCK", "DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION", "DCGM_FI_DEV_UNCORRECTABLE_REMAPPED_ROWS", "DCGM_FI_DEV_VGPU_LICENSE_STATUS", "DCGM_FI_DEV_XID_ERRORS", "DCGM_FI_DRIVER_VERSION"}

var dcgmFields339 = []string{"DCGM_FI_DEV_CORRECTABLE_REMAPPED_ROWS", "DCGM_FI_DEV_DEC_UTIL", "DCGM_FI_DEV_ENC_UTIL", "DCGM_FI_DEV_FB_FREE", "DCGM_FI_DEV_FB_USED", "DCGM_FI_DEV_GPU_TEMP", "DCGM_FI_DEV_GPU_UTIL", "DCGM_FI_DEV_MEMORY_TEMP", "DCGM_FI_DEV_MEM_CLOCK", "DCGM_FI_DEV_MEM_COPY_UTIL", "DCGM_FI_DEV_NVLINK_BANDWIDTH_TOTAL", "DCGM_FI_DEV_PCIE_REPLAY_COUNTER", "DCGM_FI_DEV_POWER_USAGE", "DCGM_FI_DEV_ROW_REMAP_FAILURE", "DCGM_FI_DEV_SM_CLOCK", "DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION", "DCGM_FI_DEV_UNCORRECTABLE_REMAPPED_ROWS", "DCGM_FI_DEV_VGPU_LICENSE_STATUS", "DCGM_FI_DEV_XID_ERRORS", "DCGM_FI_DRIVER_VERSION", "DCGM_FI_PROF_PCIE_RX_BYTES", "DCGM_FI_PROF_PCIE_TX_BYTES"}

var dcgmFieldsDCP40 = []string{"DCGM_FI_DEV_CORRECTABLE_REMAPPED_ROWS", "DCGM_FI_DEV_DEC_UTIL", "DCGM_FI_DEV_ENC_UTIL", "DCGM_FI_DEV_FB_FREE", "DCGM_FI_DEV_FB_USED", "DCGM_FI_DEV_GPU_TEMP", "DCGM_FI_DEV_GPU_UTIL", "DCGM_FI_DEV_MEMORY_TEMP", "DCGM_FI_DEV_MEM_CLOCK", "DCGM_FI_DEV_MEM_COPY_UTIL", "DCGM_FI_DEV_NVLINK_BANDWIDTH_TOTAL", "DCGM_FI_DEV_PCIE_REPLAY_COUNTER", "DCGM_FI_DEV_POWER_USAGE", "DCGM_FI_DEV_ROW_REMAP_FAILURE", "DCGM_FI_DEV_SM_CLOCK", "DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION", "DCGM_FI_DEV_UNCORRECTABLE_REMAPPED_ROWS", "DCGM_FI_DEV_VGPU_LICENSE_STATUS", "DCGM_FI_DEV_XID_ERRORS", "DCGM_FI_DRIVER_VERSION", "DCGM_FI_PROF_DRAM_ACTIVE", "DCGM_FI_PROF_GR_ENGINE_ACTIVE", "DCGM_FI_PROF_PCIE_RX_BYTES", "DCGM_FI_PROF_PCIE_TX_BYTES", "DCGM_FI_PROF_PIPE_TENSOR_ACTIVE"}

var dcgmFields44 = []string{"DCGM_FI_DEV_CORRECTABLE_REMAPPED_ROWS", "DCGM_FI_DEV_DEC_UTIL", "DCGM_FI_DEV_ENC_UTIL", "DCGM_FI_DEV_FB_FREE", "DCGM_FI_DEV_FB_RESERVED", "DCGM_FI_DEV_FB_USED", "DCGM_FI_DEV_GPU_TEMP", "DCGM_FI_DEV_GPU_UTIL", "DCGM_FI_DEV_MEMORY_TEMP", "DCGM_FI_DEV_MEM_CLOCK", "DCGM_FI_DEV_MEM_COPY_UTIL", "DCGM_FI_DEV_NVLINK_BANDWIDTH_TOTAL", "DCGM_FI_DEV_PCIE_REPLAY_COUNTER", "DCGM_FI_DEV_POWER_USAGE", "DCGM_FI_DEV_ROW_REMAP_FAILURE", "DCGM_FI_DEV_SM_CLOCK", "DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION", "DCGM_FI_DEV_UNCORRECTABLE_REMAPPED_ROWS", "DCGM_FI_DEV_VGPU_LICENSE_STATUS", "DCGM_FI_DEV_XID_ERRORS", "DCGM_FI_DRIVER_VERSION", "DCGM_FI_PROF_DRAM_ACTIVE", "DCGM_FI_PROF_GR_ENGINE_ACTIVE", "DCGM_FI_PROF_PCIE_RX_BYTES", "DCGM_FI_PROF_PCIE_TX_BYTES", "DCGM_FI_PROF_PIPE_TENSOR_ACTIVE"}

// dcgmFields49 uses the metric names dcgm-exporter 4.9 renamed (units in the
// name, *_RATIO for utilisation).
var dcgmFields49 = []string{"DCGM_FI_DEV_BOARD_POWER_WATTS", "DCGM_FI_DEV_DEC_UTIL_RATIO", "DCGM_FI_DEV_ENC_UTIL_RATIO", "DCGM_FI_DEV_FB_CAPACITY_BYTES", "DCGM_FI_DEV_FB_FREE_BYTES", "DCGM_FI_DEV_FB_RESERVED_BYTES", "DCGM_FI_DEV_FB_USED_BYTES", "DCGM_FI_DEV_GPU_ENERGY_JOULES_TOTAL", "DCGM_FI_DEV_GPU_TEMP_CELSIUS", "DCGM_FI_DEV_GPU_UTIL_RATIO", "DCGM_FI_DEV_MEMORY_CLOCK_HERTZ", "DCGM_FI_DEV_MEMORY_TEMP_CELSIUS", "DCGM_FI_DEV_MEMORY_UTIL_RATIO", "DCGM_FI_DEV_NVLINK_THROUGHPUT_BYTES_PER_SECOND", "DCGM_FI_DEV_PCIE_REPLAY_TOTAL", "DCGM_FI_DEV_ROW_REMAP_CORRECTABLE_TOTAL", "DCGM_FI_DEV_ROW_REMAP_FAILED", "DCGM_FI_DEV_ROW_REMAP_UNCORRECTABLE_TOTAL", "DCGM_FI_DEV_SM_CLOCK_HERTZ", "DCGM_FI_DEV_VGPU_LICENSE_STATUS", "DCGM_FI_DEV_XID_ERROR", "DCGM_FI_PROF_DRAM_UTIL_RATIO", "DCGM_FI_PROF_GR_ENGINE_UTIL_RATIO", "DCGM_FI_PROF_PCIE_RX_BYTES", "DCGM_FI_PROF_PCIE_TX_BYTES", "DCGM_FI_PROF_TENSOR_UTIL_RATIO", "DCGM_FI_SYSTEM_DRIVER_VERSION"}

// GKEManagedDCGMFields are the DCGM metrics GKE's managed DCGM collection
// exports (its set is fixed; it cannot be extended). From
// cloud.google.com/kubernetes-engine/docs/how-to/dcgm-metrics, checked
// 2026-11-26.
var GKEManagedDCGMFields = []string{"DCGM_FI_DEV_FB_FREE", "DCGM_FI_DEV_FB_TOTAL", "DCGM_FI_DEV_FB_USED", "DCGM_FI_DEV_GPU_TEMP", "DCGM_FI_DEV_GPU_UTIL", "DCGM_FI_DEV_MEMORY_TEMP", "DCGM_FI_DEV_MEM_COPY_UTIL", "DCGM_FI_DEV_POWER_USAGE", "DCGM_FI_DEV_SM_CLOCK", "DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION", "DCGM_FI_PROF_DRAM_ACTIVE", "DCGM_FI_PROF_GR_ENGINE_ACTIVE", "DCGM_FI_PROF_NVLINK_RX_BYTES", "DCGM_FI_PROF_NVLINK_TX_BYTES", "DCGM_FI_PROF_PCIE_RX_BYTES", "DCGM_FI_PROF_PCIE_TX_BYTES", "DCGM_FI_PROF_PIPE_FP16_ACTIVE", "DCGM_FI_PROF_PIPE_FP32_ACTIVE", "DCGM_FI_PROF_PIPE_FP64_ACTIVE", "DCGM_FI_PROF_PIPE_TENSOR_ACTIVE", "DCGM_FI_PROF_SM_ACTIVE"}

// GKEManagedDCGMSource names where GKEManagedDCGMFields came from.
const GKEManagedDCGMSource = "GKE managed DCGM metric set (cloud.google.com/kubernetes-engine/docs/how-to/dcgm-metrics, 2026-11-26)"

var exporterVersionRE = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)`)

// dcgmExporterVersion extracts the exporter release from an image tag:
// "3.3.9-3.6.1-ubuntu22.04" → 3.6.1, "4.9.0" → 4.9.0,
// "4.2.3-4.1.3-distroless" → 4.1.3.
func dcgmExporterVersion(tag string) ([3]int, bool) {
	parts := strings.Split(tag, "-")
	var cands []string
	if len(parts) >= 2 && exporterVersionRE.MatchString(parts[0]) && exporterVersionRE.MatchString(parts[1]) {
		cands = []string{parts[1]}
	} else if len(parts) >= 1 {
		cands = []string{parts[0]}
	}
	for _, c := range cands {
		m := exporterVersionRE.FindStringSubmatch(c)
		if m == nil {
			continue
		}
		var v [3]int
		for i := range v {
			v[i], _ = strconv.Atoi(m[i+1])
		}
		return v, true
	}
	return [3]int{}, false
}

func versionLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// DCGMDefaultFields returns the fields a dcgm-exporter image tag enables
// with one of its built-in counters files (DCGMDefaultCounters or
// DCGMDCPIncluded), and the verified upstream tag the set was taken from.
// ok is false for a version below the oldest verified release or a tag it
// cannot parse.
func DCGMDefaultFields(imageTag, file string) (fields []string, tag string, ok bool) {
	v, ok := dcgmExporterVersion(imageTag)
	if !ok {
		return nil, "", false
	}
	var best *dcgmDefaultSet
	for i := range dcgmDefaults {
		d := &dcgmDefaults[i]
		if d.file != file || versionLess(v, d.from) {
			continue
		}
		if best == nil || versionLess(best.from, d.from) {
			best = d
		}
	}
	if best == nil {
		return nil, "", false
	}
	tag = best.tags[0]
	for _, t := range best.tags {
		if tv, ok := dcgmExporterVersion(t); ok && !versionLess(v, tv) {
			tag = t
		}
	}
	return append([]string(nil), best.fields...), tag, true
}

// DCGMPart is one expected part of an accelerator signal: a canonical name
// and every DCGM field or exporter metric name (across the 4.9 renames)
// that provides it.
type DCGMPart struct {
	Name  string
	Names []string
	// Prefixes match any name starting with one of them (NVLink error
	// counters change name by GPU generation).
	Prefixes []string
}

func (p DCGMPart) matches(field string) bool {
	for _, n := range p.Names {
		if n == field {
			return true
		}
	}
	for _, pre := range p.Prefixes {
		if strings.HasPrefix(field, pre) {
			return true
		}
	}
	return false
}

// DCGMSignalParts maps each DCGM-backed signal to its expected parts.
// SignalAccelPodAttribution is not a field: it is the exporter's
// Kubernetes enrichment (see ComponentConfig.Settings).
var DCGMSignalParts = map[string][]DCGMPart{
	SignalAccelUtilisation: {
		{Name: "GPU_UTIL", Names: []string{"DCGM_FI_DEV_GPU_UTIL", "DCGM_FI_DEV_GPU_UTIL_RATIO"}},
	},
	SignalAccelProfiling: {
		{Name: "PROF_GR_ENGINE_ACTIVE", Names: []string{"DCGM_FI_PROF_GR_ENGINE_ACTIVE", "DCGM_FI_PROF_GR_ENGINE_UTIL_RATIO"}},
		{Name: "PROF_SM_ACTIVE", Names: []string{"DCGM_FI_PROF_SM_ACTIVE", "DCGM_FI_PROF_SM_UTIL_RATIO"}},
		{Name: "PROF_SM_OCCUPANCY", Names: []string{"DCGM_FI_PROF_SM_OCCUPANCY", "DCGM_FI_PROF_SM_OCCUPANCY_RATIO"}},
		{Name: "PROF_PIPE_TENSOR_ACTIVE", Names: []string{"DCGM_FI_PROF_PIPE_TENSOR_ACTIVE", "DCGM_FI_PROF_TENSOR_UTIL_RATIO"}},
		{Name: "PROF_DRAM_ACTIVE", Names: []string{"DCGM_FI_PROF_DRAM_ACTIVE", "DCGM_FI_PROF_DRAM_UTIL_RATIO"}},
	},
	SignalAccelMemory: {
		{Name: "FB_USED", Names: []string{"DCGM_FI_DEV_FB_USED", "DCGM_FI_DEV_FB_USED_BYTES"}},
		{Name: "FB_FREE", Names: []string{"DCGM_FI_DEV_FB_FREE", "DCGM_FI_DEV_FB_FREE_BYTES"}},
	},
	SignalAccelXid: {
		{Name: "XID_ERRORS", Names: []string{"DCGM_FI_DEV_XID_ERRORS", "DCGM_FI_DEV_XID_ERROR", "DCGM_EXP_XID_ERRORS_COUNT", "DCGM_EXP_XID_ERRORS_TOTAL"}},
	},
	SignalAccelECC: {
		{Name: "ECC_SBE", Names: []string{"DCGM_FI_DEV_ECC_SBE_VOL_TOTAL", "DCGM_FI_DEV_ECC_SBE_AGG_TOTAL"}},
		{Name: "ECC_DBE", Names: []string{"DCGM_FI_DEV_ECC_DBE_VOL_TOTAL", "DCGM_FI_DEV_ECC_DBE_AGG_TOTAL"}},
	},
	SignalAccelRetiredPages: {
		{Name: "RETIRED_SBE", Names: []string{"DCGM_FI_DEV_RETIRED_SBE", "DCGM_FI_DEV_PAGE_RETIRED_SBE_TOTAL"}},
		{Name: "RETIRED_DBE", Names: []string{"DCGM_FI_DEV_RETIRED_DBE", "DCGM_FI_DEV_PAGE_RETIRED_DBE_TOTAL"}},
		{Name: "RETIRED_PENDING", Names: []string{"DCGM_FI_DEV_RETIRED_PENDING", "DCGM_FI_DEV_PAGE_RETIRED_PENDING"}},
	},
	SignalAccelRowRemap: {
		{Name: "UNCORRECTABLE_REMAPPED_ROWS", Names: []string{"DCGM_FI_DEV_UNCORRECTABLE_REMAPPED_ROWS", "DCGM_FI_DEV_ROW_REMAP_UNCORRECTABLE_TOTAL"}},
		{Name: "ROW_REMAP_FAILURE", Names: []string{"DCGM_FI_DEV_ROW_REMAP_FAILURE", "DCGM_FI_DEV_ROW_REMAP_FAILED"}},
	},
	SignalAccelNVLink: {
		{Name: "NVLINK_BANDWIDTH", Names: []string{"DCGM_FI_DEV_NVLINK_BANDWIDTH_TOTAL", "DCGM_FI_DEV_NVLINK_THROUGHPUT_BYTES_PER_SECOND", "DCGM_FI_PROF_NVLINK_TX_BYTES", "DCGM_FI_PROF_NVLINK_RX_BYTES"}},
		{Name: "NVLINK_ERRORS", Prefixes: []string{"DCGM_FI_DEV_NVLINK_CRC_", "DCGM_FI_DEV_NVLINK_REPLAY_", "DCGM_FI_DEV_NVLINK_RECOVERY_"}},
	},
	SignalAccelPCIe: {
		{Name: "PCIE_REPLAY", Names: []string{"DCGM_FI_DEV_PCIE_REPLAY_COUNTER", "DCGM_FI_DEV_PCIE_REPLAY_TOTAL"}},
		{Name: "PCIE_BYTES", Names: []string{"DCGM_FI_PROF_PCIE_TX_BYTES", "DCGM_FI_PROF_PCIE_RX_BYTES", "DCGM_FI_DEV_PCIE_TX_THROUGHPUT", "DCGM_FI_DEV_PCIE_RX_THROUGHPUT"}},
	},
	SignalAccelThrottle: {
		{Name: "CLOCK_EVENT_REASONS", Names: []string{"DCGM_FI_DEV_CLOCK_THROTTLE_REASONS", "DCGM_FI_DEV_CLOCKS_EVENT_REASONS", "DCGM_EXP_CLOCK_EVENTS_COUNT", "DCGM_EXP_CLOCK_EVENTS_TOTAL"},
			Prefixes: []string{"DCGM_FI_DEV_GPU_APP_THROTTLE_"}},
	},
	SignalAccelPowerThermal: {
		{Name: "POWER_USAGE", Names: []string{"DCGM_FI_DEV_POWER_USAGE", "DCGM_FI_DEV_BOARD_POWER_WATTS"}},
		{Name: "GPU_TEMP", Names: []string{"DCGM_FI_DEV_GPU_TEMP", "DCGM_FI_DEV_GPU_TEMP_CELSIUS"}},
		{Name: "POWER_VIOLATION", Names: []string{"DCGM_FI_DEV_POWER_VIOLATION", "DCGM_FI_DEV_GPU_POWER_THROTTLE_SECONDS_TOTAL"}},
		{Name: "THERMAL_VIOLATION", Names: []string{"DCGM_FI_DEV_THERMAL_VIOLATION", "DCGM_FI_DEV_GPU_THERMAL_THROTTLE_SECONDS_TOTAL"}},
	},
}

// DCGMPartsPresent returns the names of the signal's parts a field set
// provides, in the parts' order.
func DCGMPartsPresent(signal string, fields []string) []string {
	var out []string
	for _, p := range DCGMSignalParts[signal] {
		for _, f := range fields {
			if p.matches(f) {
				out = append(out, p.Name)
				break
			}
		}
	}
	return out
}

// DCGMPartNames returns a signal's part names in order.
func DCGMPartNames(signal string) []string {
	var out []string
	for _, p := range DCGMSignalParts[signal] {
		out = append(out, p.Name)
	}
	return out
}

// DCGMPartFields returns the field names that provide part of signal, in
// the given naming generation: the 4.9 names when modern is set.
func DCGMPartFields(signal, part string, modern bool) []string {
	for _, p := range DCGMSignalParts[signal] {
		if p.Name != part {
			continue
		}
		var out []string
		for _, n := range p.Names {
			if isModernDCGMName(n) == modern && strings.HasPrefix(n, "DCGM_FI_") {
				out = append(out, n)
			}
		}
		if len(out) == 0 && len(p.Names) > 0 {
			out = []string{p.Names[0]}
		}
		sort.Strings(out)
		return out
	}
	return nil
}

// isModernDCGMName reports whether a name is one dcgm-exporter 4.9
// introduced (units or _RATIO / _TOTAL suffixes on renamed fields).
func isModernDCGMName(n string) bool {
	for _, suf := range []string{"_RATIO", "_BYTES", "_WATTS", "_CELSIUS", "_HERTZ", "_SECONDS_TOTAL", "_JOULES_TOTAL", "_PER_SECOND"} {
		if strings.HasSuffix(n, suf) && !strings.HasPrefix(n, "DCGM_FI_PROF_PCIE_") && !strings.HasPrefix(n, "DCGM_FI_PROF_NVLINK_") {
			return true
		}
	}
	switch n {
	case "DCGM_FI_DEV_XID_ERROR", "DCGM_FI_DEV_ROW_REMAP_FAILED", "DCGM_FI_DEV_PCIE_REPLAY_TOTAL",
		"DCGM_FI_DEV_ROW_REMAP_UNCORRECTABLE_TOTAL", "DCGM_FI_DEV_ROW_REMAP_CORRECTABLE_TOTAL",
		"DCGM_FI_DEV_PAGE_RETIRED_SBE_TOTAL", "DCGM_FI_DEV_PAGE_RETIRED_DBE_TOTAL", "DCGM_FI_DEV_PAGE_RETIRED_PENDING":
		return true
	}
	return false
}

// UsesModernDCGMNames reports whether a field set uses dcgm-exporter 4.9's
// metric names.
func UsesModernDCGMNames(fields []string) bool {
	for _, f := range fields {
		if f == "DCGM_FI_DEV_GPU_UTIL_RATIO" || f == "DCGM_FI_DEV_FB_USED_BYTES" || f == "DCGM_FI_DEV_XID_ERROR" {
			return true
		}
	}
	return false
}

// DCGMField is one row of a counters CSV.
type DCGMField struct {
	Name, Type, Help string
}

// ForgeDCGMFields is the field list a forge.Install of dcgm-exporter (chart
// 4.9.0) configures: the 4.9.0 default-counters.csv plus the profiling,
// ECC, retired-page, NVLink-error, throttle and XID counters its
// recommendations read, all with 4.9.0's names and help text (copied from
// etc/default-counters.csv at tag 4.9.0, 2026-11-26).
var ForgeDCGMFields = []DCGMField{
	{"DCGM_FI_DEV_SM_CLOCK_HERTZ", "gauge", "SM clock frequency in hertz."},
	{"DCGM_FI_DEV_MEMORY_CLOCK_HERTZ", "gauge", "Memory clock frequency in hertz."},
	{"DCGM_FI_DEV_MEMORY_TEMP_CELSIUS", "gauge", "Memory temperature (in C)."},
	{"DCGM_FI_DEV_GPU_TEMP_CELSIUS", "gauge", "GPU temperature (in C)."},
	{"DCGM_FI_DEV_BOARD_POWER_WATTS", "gauge", "Power draw (in W)."},
	{"DCGM_FI_DEV_GPU_ENERGY_JOULES_TOTAL", "counter", "GPU energy consumed since the driver was reloaded in joules."},
	{"DCGM_FI_DEV_GPU_POWER_THROTTLE_SECONDS_TOTAL", "counter", "Time below application clocks due to power policy in seconds."},
	{"DCGM_FI_DEV_GPU_THERMAL_THROTTLE_SECONDS_TOTAL", "counter", "Time below application clocks due to thermal policy in seconds."},
	{"DCGM_FI_DEV_PCIE_REPLAY_TOTAL", "counter", "Total number of PCIe retries."},
	{"DCGM_FI_DEV_GPU_UTIL_RATIO", "gauge", "GPU utilization as a ratio."},
	{"DCGM_FI_DEV_MEMORY_UTIL_RATIO", "gauge", "Memory utilization as a ratio."},
	{"DCGM_FI_DEV_ENC_UTIL_RATIO", "gauge", "Encoder utilization as a ratio."},
	{"DCGM_FI_DEV_DEC_UTIL_RATIO", "gauge", "Decoder utilization as a ratio."},
	{"DCGM_FI_DEV_XID_ERROR", "gauge", "Value of the last XID error encountered."},
	{"DCGM_EXP_CLOCK_EVENTS_TOTAL", "counter", "Total clock events observed since the counter was last reset (edge-counted)."},
	{"DCGM_EXP_XID_ERRORS_TOTAL", "counter", "Total XID errors observed since the counter was last reset."},
	{"DCGM_FI_DEV_FB_CAPACITY_BYTES", "gauge", "Total framebuffer capacity in bytes."},
	{"DCGM_FI_DEV_FB_FREE_BYTES", "gauge", "Free framebuffer memory in bytes."},
	{"DCGM_FI_DEV_FB_USED_BYTES", "gauge", "Used framebuffer memory in bytes."},
	{"DCGM_FI_DEV_FB_RESERVED_BYTES", "gauge", "Reserved framebuffer memory in bytes."},
	{"DCGM_FI_DEV_ECC_SBE_VOL_TOTAL", "counter", "Total number of single-bit volatile ECC errors."},
	{"DCGM_FI_DEV_ECC_DBE_VOL_TOTAL", "counter", "Total number of double-bit volatile ECC errors."},
	{"DCGM_FI_DEV_PAGE_RETIRED_SBE_TOTAL", "counter", "Total number of retired pages due to single-bit errors."},
	{"DCGM_FI_DEV_PAGE_RETIRED_DBE_TOTAL", "counter", "Total number of retired pages due to double-bit errors."},
	{"DCGM_FI_DEV_PAGE_RETIRED_PENDING", "gauge", "Whether pages are pending retirement (1 means pending and 0 means not pending)."},
	{"DCGM_FI_DEV_NVLINK_CRC_ERROR_TOTAL", "counter", "Total number of NVLink data-link CRC errors."},
	{"DCGM_FI_DEV_NVLINK_RECOVERY_TOTAL", "counter", "Total number of NVLink data-link recovery errors."},
	{"DCGM_FI_DEV_NVLINK_REPLAY_TOTAL", "counter", "Total number of NVLink data-link replay errors."},
	{"DCGM_FI_DEV_NVLINK_THROUGHPUT_BYTES_PER_SECOND", "gauge", "Total NVLink throughput across all lanes in bytes per second."},
	{"DCGM_FI_DEV_VGPU_LICENSE_STATUS", "gauge", "vGPU License status"},
	{"DCGM_FI_DEV_ROW_REMAP_UNCORRECTABLE_TOTAL", "counter", "Number of remapped rows for uncorrectable errors"},
	{"DCGM_FI_DEV_ROW_REMAP_CORRECTABLE_TOTAL", "counter", "Number of remapped rows for correctable errors"},
	{"DCGM_FI_DEV_ROW_REMAP_FAILED", "gauge", "Whether remapping of rows has failed"},
	{"DCGM_FI_SYSTEM_DRIVER_VERSION", "label", "Driver Version"},
	{"DCGM_FI_PROF_GR_ENGINE_UTIL_RATIO", "gauge", "Ratio of time the graphics engine is active."},
	{"DCGM_FI_PROF_SM_UTIL_RATIO", "gauge", "The ratio of cycles an SM has at least 1 warp assigned."},
	{"DCGM_FI_PROF_SM_OCCUPANCY_RATIO", "gauge", "The ratio of number of warps resident on an SM."},
	{"DCGM_FI_PROF_TENSOR_UTIL_RATIO", "gauge", "Ratio of cycles the tensor (HMMA) pipe is active."},
	{"DCGM_FI_PROF_DRAM_UTIL_RATIO", "gauge", "Ratio of cycles the device memory interface is active sending or receiving data."},
	{"DCGM_FI_PROF_PCIE_TX_BYTES", "gauge", "The rate of data transmitted over the PCIe bus - including both protocol headers and data payloads - in bytes per second."},
	{"DCGM_FI_PROF_PCIE_RX_BYTES", "gauge", "The rate of data received over the PCIe bus - including both protocol headers and data payloads - in bytes per second."},
	{"DCGM_FI_PROF_NVLINK_TX_BYTES", "gauge", "The rate of data transmitted over NVLink in bytes per second."},
	{"DCGM_FI_PROF_NVLINK_RX_BYTES", "gauge", "The rate of data received over NVLink in bytes per second."},
}
