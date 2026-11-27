package mldiscover

import (
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	resourcehelper "k8s.io/component-helpers/resource"
)

// Accelerator extended resources, as the device plugins publish them.
const (
	ResourceNVIDIAGPU       = "nvidia.com/gpu"
	ResourceNVIDIAGPUShared = "nvidia.com/gpu.shared"
	ResourceNVIDIAMIGPrefix = "nvidia.com/mig-"
	ResourceAMDGPU          = "amd.com/gpu"
	ResourceTPU             = "google.com/tpu"
	ResourceNeuron          = "aws.amazon.com/neuron"
	ResourceNeuronCore      = "aws.amazon.com/neuroncore"
	ResourceNeuronDevice    = "aws.amazon.com/neurondevice"
	ResourceGaudi           = "habana.ai/gaudi"
)

// DetectedBy values.
const (
	DetectedByResource = "extended-resource"
	DetectedByLabel    = "label"
)

// Sharing values. SharingMixed is a group whose nodes disagree.
const (
	SharingMIG         = "mig"
	SharingTimeSlicing = "time-slicing"
	SharingMPS         = "mps"
	SharingMixed       = "mixed"
)

// Node labels discovery reads.
const (
	LabelGFDProduct          = "nvidia.com/gpu.product"
	LabelGFDPresent          = "nvidia.com/gpu.present"
	LabelGFDReplicas         = "nvidia.com/gpu.replicas"
	LabelGFDSharingStrategy  = "nvidia.com/gpu.sharing-strategy"
	LabelMIGConfig           = "nvidia.com/mig.config"
	LabelMIGConfigState      = "nvidia.com/mig.config.state"
	LabelCUDADriverMajor     = "nvidia.com/cuda.driver.major"
	LabelCUDADriverMinor     = "nvidia.com/cuda.driver.minor"
	LabelCUDADriverRev       = "nvidia.com/cuda.driver.rev"
	LabelCUDADriverFull      = "nvidia.com/cuda.driver-version.full"
	LabelGKEAccelerator      = "cloud.google.com/gke-accelerator"
	LabelGKETPUAccelerator   = "cloud.google.com/gke-tpu-accelerator"
	LabelGKEGPUSharing       = "cloud.google.com/gke-gpu-sharing-strategy"
	LabelGKEGPUPartitionSize = "cloud.google.com/gke-gpu-partition-size"
	LabelGKENodePool         = "cloud.google.com/gke-nodepool"
	LabelGKEImageStreaming   = "cloud.google.com/gke-image-streaming"
	LabelEKSNodeGroup        = "eks.amazonaws.com/nodegroup"
	LabelEKSComputeType      = "eks.amazonaws.com/compute-type"
	LabelKarpenterNodePool   = "karpenter.sh/nodepool"
	LabelAKSAgentPool        = "kubernetes.azure.com/agentpool"
	LabelAgentPool           = "agentpool"
	LabelInstanceType        = "node.kubernetes.io/instance-type"
	LabelInstanceTypeBeta    = "beta.kubernetes.io/instance-type"
	LabelRegion              = "topology.kubernetes.io/region"
	LabelZone                = "topology.kubernetes.io/zone"
	LabelAMDProductName      = "amd.com/gpu.product-name"
	LabelAMDProductNameBeta  = "beta.amd.com/gpu.product-name"
	LabelComponentOwner      = "forge.dev/component-owner"
	LabelHelmChart           = "helm.sh/chart"
)

// SpotLabels are the node labels (and the value each must have) that mark
// spot or preemptible capacity, per cloud.
var SpotLabels = map[string]string{
	"cloud.google.com/gke-spot":             "true",
	"cloud.google.com/gke-preemptible":      "true",
	"karpenter.sh/capacity-type":            "spot",
	"eks.amazonaws.com/capacityType":        "SPOT",
	"kubernetes.azure.com/scalesetpriority": "spot",
}

// nonAcceleratorPrefixes are extended-resource prefixes that are devices but
// not accelerators: NICs (SR-IOV, RDMA, EFA, pod ENIs), virtualisation and
// FUSE devices. The generic fallback skips them.
var nonAcceleratorPrefixes = []string{
	"rdma/", "openshift.io/", "intel.com/sriov", "intel.com/intel_sriov",
	"devices.kubevirt.io/", "smarter-devices/", "squat.ai/", "github.com/fuse",
	"vpc.amazonaws.com/", "k8s.amazonaws.com/", "networking.gke.io/",
	"mellanox.com/", "nvidia.com/rdma", "nvidia.com/hostdev", "nvidia.com/sriov",
}

// ResourceVendor returns the accelerator vendor of an extended resource, or
// "" when the resource is not an accelerator (cpu, memory, hugepages-*, a
// kubernetes.io/ resource, a NIC).
func ResourceVendor(name string) string {
	switch {
	case name == ResourceNVIDIAGPU || name == ResourceNVIDIAGPUShared || strings.HasPrefix(name, ResourceNVIDIAMIGPrefix):
		return VendorNVIDIA
	case name == ResourceAMDGPU:
		return VendorAMD
	case name == ResourceTPU:
		return VendorTPU
	case strings.HasPrefix(name, ResourceNeuron):
		return VendorNeuron
	case strings.HasPrefix(name, ResourceGaudi):
		return VendorGaudi
	}
	if !isExtendedResource(name) || strings.HasPrefix(name, "nvidia.com/") || strings.HasPrefix(name, "amd.com/") {
		return ""
	}
	for _, p := range nonAcceleratorPrefixes {
		if strings.HasPrefix(name, p) {
			return ""
		}
	}
	return VendorGeneric
}

// IsAcceleratorResource reports whether name is an accelerator resource,
// vendor or generic.
func IsAcceleratorResource(name string) bool { return ResourceVendor(name) != "" }

// isExtendedResource reports whether name is an extended resource: domain
// prefixed and outside the kubernetes.io domain.
func isExtendedResource(name string) bool {
	slash := strings.IndexByte(name, '/')
	if slash <= 0 || strings.HasPrefix(name, "requests.") || strings.HasPrefix(name, "attachable-volumes-") {
		return false
	}
	domain := name[:slash]
	return domain != "kubernetes.io" && !strings.HasSuffix(domain, ".kubernetes.io")
}

// DefaultResource is the resource forge.Accelerators.resource defaults to
// for a vendor ("" for generic, which must name it).
func DefaultResource(vendor string) string {
	switch vendor {
	case VendorNVIDIA:
		return ResourceNVIDIAGPU
	case VendorAMD:
		return ResourceAMDGPU
	case VendorTPU:
		return ResourceTPU
	case VendorNeuron:
		return ResourceNeuronCore
	case VendorGaudi:
		return ResourceGaudi
	}
	return ""
}

// vendorOrder ranks vendors when a node carries more than one.
var vendorOrder = map[string]int{VendorNVIDIA: 0, VendorAMD: 1, VendorTPU: 2, VendorNeuron: 3, VendorGaudi: 4, VendorGeneric: 5}

// VendorRank orders vendors: NVIDIA first, generic last.
func VendorRank(vendor string) int {
	if r, ok := vendorOrder[vendor]; ok {
		return r
	}
	return len(vendorOrder)
}

// NodeAccelerator is what a node carries, as discovery reads it.
type NodeAccelerator struct {
	Vendor string
	// Resource is the resource the node's devices are counted in.
	Resource string
	// Family is every resource counted into Capacity (all MIG profiles of
	// a mixed-strategy node; otherwise just Resource).
	Family      []string
	Capacity    int64
	Allocatable int64
	DetectedBy  string
}

// NodeAcceleratorOf reports the accelerator a node carries, and false when
// it carries none. A node with capacity for several vendors reports the
// first by VendorRank. NVIDIA counts nvidia.com/gpu, else
// nvidia.com/gpu.shared, else every MIG profile together; Neuron counts
// neuroncore, else neurondevice, else neuron.
func NodeAcceleratorOf(node *corev1.Node) (NodeAccelerator, bool) {
	byVendor := map[string][]string{}
	for name, q := range node.Status.Capacity {
		if q.Value() <= 0 {
			continue
		}
		if v := ResourceVendor(string(name)); v != "" {
			byVendor[v] = append(byVendor[v], string(name))
		}
	}
	if len(byVendor) > 0 {
		vendors := make([]string, 0, len(byVendor))
		for v := range byVendor {
			vendors = append(vendors, v)
		}
		sort.Slice(vendors, func(i, j int) bool { return VendorRank(vendors[i]) < VendorRank(vendors[j]) })
		v := vendors[0]
		res := byVendor[v]
		sort.Strings(res)
		family := pickFamily(v, res)
		acc := NodeAccelerator{Vendor: v, Resource: family[0], Family: family, DetectedBy: DetectedByResource}
		for _, r := range family {
			acc.Capacity += quantity(node.Status.Capacity, r)
			acc.Allocatable += quantity(node.Status.Allocatable, r)
		}
		return acc, true
	}
	if v := labelVendor(node.Labels); v != "" {
		r := DefaultResource(v)
		return NodeAccelerator{Vendor: v, Resource: r, Family: []string{r}, DetectedBy: DetectedByLabel}, true
	}
	return NodeAccelerator{}, false
}

func pickFamily(vendor string, res []string) []string {
	has := map[string]bool{}
	for _, r := range res {
		has[r] = true
	}
	switch vendor {
	case VendorNVIDIA:
		for _, r := range []string{ResourceNVIDIAGPU, ResourceNVIDIAGPUShared} {
			if has[r] {
				return []string{r}
			}
		}
		return res // every MIG profile
	case VendorNeuron:
		for _, r := range []string{ResourceNeuronCore, ResourceNeuronDevice, ResourceNeuron} {
			if has[r] {
				return []string{r}
			}
		}
	}
	return res[:1]
}

func quantity(list corev1.ResourceList, name string) int64 {
	q, ok := list[corev1.ResourceName(name)]
	if !ok {
		return 0
	}
	return q.Value()
}

// labelVendor recognises an accelerator node from its labels alone.
func labelVendor(l map[string]string) string {
	switch {
	case l[LabelGFDPresent] == "true" || l[LabelGFDProduct] != "" || strings.HasPrefix(l[LabelGKEAccelerator], "nvidia-"):
		return VendorNVIDIA
	case l[LabelAMDProductName] != "" || l[LabelAMDProductNameBeta] != "" || hasLabelPrefix(l, "amd.com/gpu."):
		return VendorAMD
	case l[LabelGKETPUAccelerator] != "":
		return VendorTPU
	}
	it := InstanceType(l)
	for _, p := range []string{"trn1.", "trn1n.", "trn2.", "trn2u.", "inf1.", "inf2."} {
		if strings.HasPrefix(it, p) {
			return VendorNeuron
		}
	}
	if strings.HasPrefix(it, "dl1.") {
		return VendorGaudi
	}
	return ""
}

func hasLabelPrefix(l map[string]string, prefix string) bool {
	for k := range l {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// InstanceType is the node's cloud instance type label, or "".
func InstanceType(l map[string]string) string {
	if v := l[LabelInstanceType]; v != "" {
		return v
	}
	return l[LabelInstanceTypeBeta]
}

// NodeProduct names the device model from the node's labels: GFD, the
// cloud's accelerator label, AMD's labeller, else the instance type.
func NodeProduct(vendor string, l map[string]string) string {
	var keys []string
	switch vendor {
	case VendorNVIDIA:
		keys = []string{LabelGFDProduct, LabelGKEAccelerator}
	case VendorAMD:
		keys = []string{LabelAMDProductName, LabelAMDProductNameBeta}
	case VendorTPU:
		keys = []string{LabelGKETPUAccelerator}
	}
	for _, k := range keys {
		if v := l[k]; v != "" {
			return v
		}
	}
	return InstanceType(l)
}

// NodePool names the node's pool from the GKE, EKS, Karpenter or AKS
// label, or "".
func NodePool(node *corev1.Node) string {
	for _, k := range []string{LabelGKENodePool, LabelEKSNodeGroup, LabelKarpenterNodePool, LabelAKSAgentPool, LabelAgentPool} {
		if v := node.Labels[k]; v != "" {
			return v
		}
	}
	return ""
}

// IsSpot reports whether one of SpotLabels marks the node.
func IsSpot(node *corev1.Node) bool {
	for k, v := range SpotLabels {
		if node.Labels[k] == v {
			return true
		}
	}
	return false
}

// NodeSharing reports how an NVIDIA node shares its GPUs: SharingMIG,
// SharingTimeSlicing, SharingMPS or "".
func NodeSharing(node *corev1.Node) string {
	l := node.Labels
	for name, q := range node.Status.Capacity {
		if strings.HasPrefix(string(name), ResourceNVIDIAMIGPrefix) && q.Value() > 0 {
			return SharingMIG
		}
	}
	// The MIG manager's desired config counts only once it applied
	// (state "success"): a failed or pending partitioning shares nothing.
	if strings.Contains(l[LabelGFDProduct], "-MIG-") || l[LabelGKEGPUPartitionSize] != "" ||
		(l[LabelMIGConfig] != "" && l[LabelMIGConfig] != "all-disabled" && l[LabelMIGConfigState] == "success") {
		return SharingMIG
	}
	switch strings.ToLower(l[LabelGFDSharingStrategy]) {
	case "time-slicing":
		return SharingTimeSlicing
	case "mps":
		return SharingMPS
	}
	switch strings.ToLower(l[LabelGKEGPUSharing]) {
	case "time-sharing":
		return SharingTimeSlicing
	case "mps":
		return SharingMPS
	}
	if n, err := strconv.Atoi(l[LabelGFDReplicas]); err == nil && n > 1 {
		return SharingTimeSlicing
	}
	return ""
}

// DriverVersion is the NVIDIA driver version GFD labelled the node with
// (550.54.15), or "".
func DriverVersion(l map[string]string) string {
	if v := l[LabelCUDADriverFull]; v != "" {
		return v
	}
	var parts []string
	for _, k := range []string{LabelCUDADriverMajor, LabelCUDADriverMinor, LabelCUDADriverRev} {
		v := l[k]
		if v == "" {
			break
		}
		parts = append(parts, v)
	}
	return strings.Join(parts, ".")
}

// PodActive reports whether a pod still holds its resources: not Succeeded
// or Failed.
func PodActive(pod *corev1.Pod) bool {
	return pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed
}

// PodAccelerators returns the accelerator resources a pod requests, with
// the effective count of each by Kubernetes' own rule (the scheduler's and
// kubelet's, from k8s.io/component-helpers/resource): the sum over its
// containers and restartable (sidecar) init containers, or the largest
// ordinary init container plus the sidecars started before it, whichever is
// larger. A container that sets only a limit requests that limit, as the API
// server defaults it.
func PodAccelerators(pod *corev1.Pod) map[string]int64 {
	return podSpecAccelerators(&pod.Spec)
}

// PodSpecAccelerators is PodAccelerators over a pod template's spec (a
// Job's, a Deployment's).
func PodSpecAccelerators(spec *corev1.PodSpec) map[string]int64 { return podSpecAccelerators(spec) }

func podSpecAccelerators(spec *corev1.PodSpec) map[string]int64 {
	// Only the accelerator requests matter, so the pod handed to the helper
	// carries nothing else: no copy of the rest of the spec, and no
	// pod-level resources or overhead (neither can name an extended
	// resource).
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		Containers:     acceleratorOnly(spec.Containers),
		InitContainers: acceleratorOnly(spec.InitContainers),
	}}
	sum := map[string]int64{}
	for name, q := range resourcehelper.PodRequests(pod, resourcehelper.PodResourcesOptions{}) {
		if n := q.Value(); n > 0 && IsAcceleratorResource(string(name)) {
			sum[string(name)] = n
		}
	}
	if len(sum) == 0 {
		return nil
	}
	return sum
}

// acceleratorOnly returns containers reduced to their accelerator requests
// (a limit standing in for a missing request) and restart policy, the two
// facts the effective-request rule reads.
func acceleratorOnly(cs []corev1.Container) []corev1.Container {
	out := make([]corev1.Container, len(cs))
	for i := range cs {
		req := corev1.ResourceList{}
		for r, n := range containerAccelerators(&cs[i]) {
			req[corev1.ResourceName(r)] = *resource.NewQuantity(n, resource.DecimalSI)
		}
		out[i] = corev1.Container{Name: cs[i].Name, RestartPolicy: cs[i].RestartPolicy, Resources: corev1.ResourceRequirements{Requests: req}}
	}
	return out
}

// containerAccelerators is a container's accelerator requests: its request,
// else its limit (the API server defaults an extended resource's request to
// its limit, and a snapshot of a pod template may predate defaulting).
func containerAccelerators(c *corev1.Container) map[string]int64 {
	out := map[string]int64{}
	for name, q := range c.Resources.Requests {
		if IsAcceleratorResource(string(name)) {
			out[string(name)] = q.Value()
		}
	}
	for name, q := range c.Resources.Limits {
		if _, ok := out[string(name)]; !ok && IsAcceleratorResource(string(name)) {
			out[string(name)] = q.Value()
		}
	}
	return out
}

// AcceleratorPods returns the snapshot's active pods that request any
// accelerator resource, sorted by namespace then name.
func AcceleratorPods(s *Snapshot) []*corev1.Pod {
	var out []*corev1.Pod
	for i := range s.Pods {
		p := &s.Pods[i]
		if PodActive(p) && len(PodAccelerators(p)) > 0 {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return podKey(out[i]) < podKey(out[j]) })
	return out
}

func podKey(p *corev1.Pod) string { return p.Namespace + "/" + p.Name }

// allocatedOn sums family's effective requests over the active pods bound
// to node.
func allocatedOn(pods []corev1.Pod, node string, family []string) int64 {
	var n int64
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName != node || !PodActive(p) {
			continue
		}
		req := PodAccelerators(p)
		for _, r := range family {
			n += req[r]
		}
	}
	return n
}
