// Package mlrecs is the recommendation half of `forge ml discover`: the
// rules that read a cluster snapshot and its mldiscover inventory and say
// what to fix, and the writer of the env's ml.k proposal.
//
// [Evaluate] runs every rule in [Rules] over a snapshot. It is pure and
// deterministic. Each [Finding] carries its evidence (objects named
// kind/namespace/name) and an adopt-first [Fix]: a change to ml.k
// ([FixKCL], with a unified diff against the file [Propose] writes), an
// exact command for a setting outside forge's declarations ([FixCommand]),
// or a change only the component's owner can make ([FixManual]). A
// matching, unexpired forge.RecommendationDecision marks a finding
// dismissed and sorts it last; it is still reported.
//
// The policy the rules and the proposal hold: discover, adopt, install only
// what is missing, from a pinned upstream chart ([Charts]). Never a second
// DCGM exporter beside one that runs (two break each other's profiling
// counters); never an upgrade or reconfiguration of what forge did not
// install; never KubeRay.
//
// # Rules evaluated from a snapshot alone
//
// accel-telemetry-missing, accel-exporter-duplicate, bad-accelerator,
// mig-config-failed, driver-skew, no-node-health, gpu-jobs-bypass-queue,
// job-not-queued, spot-no-termination-handling, shm-too-small,
// llm-scales-on-cpu, model-downloaded-per-pod, weights-in-image,
// no-p2p-cache, image-streaming-off (GKE), accel-unallocated,
// image-not-warm, hf-transfer-off, image-volumes-unavailable and
// kubelet-tracing-off (unverified by construction: discovery never reads
// the kubelet's config).
//
// # Not here
//
// These need telemetry and arrive with the collector in a later slice:
// accel-idle (utilisation over time), dcgm-profiling-off and
// accel-error-counters-off (which series the exporter emits),
// bad-accelerator's Xid/ECC/row-remap half, pod-start-slow and the cold
// start rows it routes to (GPU node provisioning, slow loader),
// no-checkpointing and nccl-failures (logs), and registry-cross-cloud /
// registry region (the model's registry).
//
// These Appendix A rows are omitted because no snapshot signal is
// defensible: EKS SOCI (it needs the image's SOCI index referrer from the
// registry), AKS Artifact Streaming (the ACR SKU and pool setting are
// cloud API state), GKE secondary boot disks (the node label is unverified)
// and local NVMe unused (ephemeral storage placement is node config).
// image-streaming-off reads the cloud.google.com/gke-image-streaming=true
// node label GKE documents (checked 2026-11-25).
//
// # The proposal
//
// [Propose] writes one forge.MLStack for the cluster: `observed` lists the
// kinds discovery found running that forge did not install (so the KCL
// refuses installing a duplicate), every found component is adopted in its
// slot, forge's own installs (the forge.dev/component-owner label) are
// written as forge.Install again so a re-run keeps them, and each install
// a FixKCL finding suggests is written commented out under the finding's
// id. Vendor metric facts (the DCGM allow list) are written as data into
// the user's file, not taken from a preset in forge's KCL library. The
// output is deterministic: the same inventory, findings and options give
// the same bytes.
package mlrecs
