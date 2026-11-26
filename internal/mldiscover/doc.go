// Package mldiscover turns a read-only snapshot of a Kubernetes cluster into
// an inventory of its ML stack: the accelerators its nodes carry, one row per
// accelerator machine, and the ML components it already runs (GPU Operator,
// DCGM exporter, node-problem-detector, Kueue, JobSet, KServe, vLLM, …).
//
// It is the matching half of `forge ml discover`. The reading half (the
// CLI's live read through the user's kube context, a recorded snapshot file,
// later the control plane's collector inventory) fills a [Snapshot]; [Discover]
// is pure over it: no I/O, no clock, and a deterministic order, so the same
// snapshot always yields byte-identical JSON.
//
// # Signatures
//
// A component is present when its CRDs and a running controller match, or
// when one of its workload signatures (a container image pattern on a
// DaemonSet, Deployment, StatefulSet or, for exporters and model servers, a
// bare pod) matches. CRDs with no controller make a [StatusCRDOnly]
// component and a warning: a leftover from an uninstall is never reported as
// a running queue. Each component records the signatures that matched in
// [Component.MatchedBy]. The table is in signatures.go and follows Appendix B
// of forge's docs/proposals/ml-workloads.md.
//
// A component that carries the `forge.dev/component-owner` label was put
// there by a forge.Install and is [OwnerForge]; everything else is
// [OwnerTheirs], which forge adopts and never upgrades, reconfigures or
// removes. Workloads that carry other `forge.dev/` labels are forge's own
// platform (its node agent) and are not ML components.
//
// # Accelerators
//
// The accelerator model is vendor-neutral. A node's vendor comes from the
// extended resources in its capacity (nvidia.com/gpu, nvidia.com/mig-*,
// amd.com/gpu, google.com/tpu, aws.amazon.com/neuron*, habana.ai/gaudi),
// or, with no capacity yet, from vendor labels ([AcceleratorGroup.DetectedBy]
// "label": the device plugin is missing or not ready). Any other
// domain-prefixed extended resource that is not a known network or
// virtualisation device is reported as [VendorGeneric], at allocation level
// only. Allocated sums the limits (else requests) of the non-terminal pods
// bound to a node.
//
// # Coverage
//
// [Inventory.Coverage] has one row per signal in [Signals]: which parts of
// it (DCGM field groups, NPD rules, collector receivers, tracing flags) are
// collected, on how many nodes, by what, at which versions, and how that
// was established ([DeterminedByProbe] > config > args > version default >
// presence; [DeterminedByUnreadable] where the setting lives beyond the
// API, such as a managed control plane). A missing signal is a row, never
// an absence. The reading is two-phase: [ConfigRefs] names the ConfigMaps
// to get by name from a first snapshot, and [ProbeTargets] the metrics
// endpoints an opt-in probe may read. The parsers (DCGM counters CSV,
// dcgm-exporter config, NPD monitor JSON, OTel Collector and Alloy configs,
// KubeletConfiguration, kubeadm ClusterConfiguration) tolerate values the
// collector redacted. When nothing better is readable, a DCGM exporter's
// field set is the default counters file of its release, from a table
// verified against the upstream repository at the tags it records.
//
// The exported helpers ([PodAccelerators], [AcceleratorPods], [PodOwner],
// [NodeCoverage], [IsSpot], …) are the facts the recommendation rules in
// internal/mlrecs read, so the two packages never disagree about what an
// accelerator pod or a spot node is.
package mldiscover
