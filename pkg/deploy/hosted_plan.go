package deploy

import (
	"fmt"
	"strings"

	"github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

// THE HOSTED PLANS — the ONE place a hosted workload's resources are judged.
//
// A hosted environment publishes under one of two plans, and each has its own
// resource rule:
//
//   - HostedPlanStandard, an organization's own (billed) deploys: the shape
//     band (CheckShapeBand). Its rule is EQUALITY to the 4 GiB-per-vCPU band,
//     because the bill is the max of the request and what the band implies.
//   - HostedPlanClaimable, an anonymous deploy (`forge env deploy <env>
//     --claimable`): the platform pays and never bills, so the band does not
//     apply. Its rule is a CEILING: at most ClaimableCPUMillicores and
//     ClaimableMemoryBytes, request AND limit, no burst.
//
// The two rules used to live in different places (the band in the hosted
// plan, the caps in the CLI's eligibility check), so `forge env shape` judged
// an anonymous env by the band and told an author that 125m needs exactly
// 512 MiB while the anonymous plan refused anything above 256 MiB. No shape
// satisfies both, and none needs to: an env is judged by the plan it deploys
// under, and CheckHostedResources is the only entry point.
//
// The numbers are control-plane's (internal/planlimits/claimable.go, design
// §2); the control plane enforces them at admission and these are checked
// first, in the same words, so a refusal costs no build.

// HostedPlan is the plan a hosted environment is admitted under.
type HostedPlan string

const (
	// HostedPlanStandard is an organization's own, billed plan. The zero
	// value: an env that names no plan is judged as standard.
	HostedPlanStandard HostedPlan = ""
	// HostedPlanClaimable is the anonymous, platform-funded plan of a
	// claimable deploy.
	HostedPlanClaimable HostedPlan = "claimable"
)

// String names the plan for messages.
func (p HostedPlan) String() string {
	if p == HostedPlanClaimable {
		return "claimable (anonymous)"
	}
	return "standard"
}

// The claimable plan's caps (control-plane internal/planlimits/claimable.go).
// Per workload, and with no burst: a limit is held to the cap as well as the
// request.
const (
	ClaimableMaxServices      = 1
	ClaimableMaxMigrationJobs = 1
	ClaimableMaxReplicas      = 1
	ClaimableCPUMillicores    = int64(250)
	ClaimableMemoryBytes      = int64(256) << 20
	ClaimableMaxDatabases     = 1
	ClaimableMaxStaticSites   = 1
)

// ClaimableResources is the largest shape the claimable plan admits, request
// equal to limit. It is what `forge project new --claimable` scaffolds and
// what a refusal suggests.
func ClaimableResources() v1alpha1.Resources {
	return v1alpha1.Resources{
		CPURequestMillicores: ClaimableCPUMillicores, CPULimitMillicores: ClaimableCPUMillicores,
		MemoryRequestBytes: ClaimableMemoryBytes, MemoryLimitBytes: ClaimableMemoryBytes,
	}
}

// ResourcesRefusal is a resources block a plan does not admit, with the
// conformant block to declare instead. Its message is self-contained; Fix is
// the KCL literal a caller that knows WHERE the block is declared prints
// beside it.
type ResourcesRefusal struct {
	Plan   HostedPlan
	Reason string
	// Want is the conformant block: the nearest shape the plan admits.
	Want v1alpha1.Resources
}

func (e *ResourcesRefusal) Error() string {
	return e.Reason + " (the " + e.Plan.String() + " plan)"
}

// Fix is Want as the KCL a binder or workload declares:
//
//	resources = fw.Resources {cpuRequestMillicores = 250, ...}
func (e *ResourcesRefusal) Fix() string { return KCLResources(e.Want) }

// KCLResources renders r as a `resources = fw.Resources {...}` KCL field,
// memory in forge.mib, omitting unset limits.
func KCLResources(r v1alpha1.Resources) string {
	parts := []string{fmt.Sprintf("cpuRequestMillicores = %d", r.CPURequestMillicores)}
	if r.CPULimitMillicores > 0 {
		parts = append(parts, fmt.Sprintf("cpuLimitMillicores = %d", r.CPULimitMillicores))
	}
	parts = append(parts, "memoryRequestBytes = "+kclBytes(r.MemoryRequestBytes))
	if r.MemoryLimitBytes > 0 {
		parts = append(parts, "memoryLimitBytes = "+kclBytes(r.MemoryLimitBytes))
	}
	return "resources = fw.Resources {" + strings.Join(parts, ", ") + "}"
}

func kclBytes(b int64) string {
	if b%(1<<20) == 0 {
		return fmt.Sprintf("forge.mib(%d)", b>>20)
	}
	return fmt.Sprintf("%d", b)
}

// CheckHostedResources judges one hosted workload's resources under plan:
// the shape band for the standard plan, the claimable caps for the claimable
// one. A refusal is a *ResourcesRefusal naming the conformant block.
func CheckHostedResources(plan HostedPlan, r v1alpha1.Resources) error {
	if plan == HostedPlanClaimable {
		return checkClaimableResources(r)
	}
	if err := CheckShapeBand(r); err != nil {
		return &ResourcesRefusal{Plan: plan, Reason: err.Error(), Want: bandFix(r)}
	}
	return nil
}

func checkClaimableResources(r v1alpha1.Resources) error {
	d := r.WithDefaults()
	var why []string
	if cpu := max(d.CPURequestMillicores, d.CPULimitMillicores); cpu > ClaimableCPUMillicores {
		why = append(why, fmt.Sprintf("%dm of CPU (at most %dm, no burst)", cpu, ClaimableCPUMillicores))
	}
	if mem := max(d.MemoryRequestBytes, d.MemoryLimitBytes); mem > ClaimableMemoryBytes {
		why = append(why, fmt.Sprintf("%d MiB of memory (at most %d MiB, no burst)", mem>>20, ClaimableMemoryBytes>>20))
	}
	if len(why) == 0 {
		return nil
	}
	want := v1alpha1.Resources{
		CPURequestMillicores: min(d.CPURequestMillicores, ClaimableCPUMillicores),
		MemoryRequestBytes:   min(d.MemoryRequestBytes, ClaimableMemoryBytes),
	}
	want.CPULimitMillicores, want.MemoryLimitBytes = want.CPURequestMillicores, want.MemoryRequestBytes
	return &ResourcesRefusal{Plan: HostedPlanClaimable, Reason: "asks for " + strings.Join(why, " and "), Want: want}
}

// bandFix is the on-band shape nearest r: its CPU request rounded UP to the
// band's granularity, with exactly the memory the band implies. A limit below
// the new request is raised to it (a limit may never be under its request).
func bandFix(r v1alpha1.Resources) v1alpha1.Resources {
	d := r.WithDefaults()
	cpu := d.CPURequestMillicores
	if cpu <= 0 {
		cpu = BandGranularityMillicores
	}
	if rem := cpu % BandGranularityMillicores; rem != 0 {
		cpu += BandGranularityMillicores - rem
	}
	want := v1alpha1.Resources{CPURequestMillicores: cpu, MemoryRequestBytes: BandMemoryBytes(cpu)}
	if r.CPULimitMillicores > 0 {
		want.CPULimitMillicores = max(r.CPULimitMillicores, cpu)
	}
	if r.MemoryLimitBytes > 0 {
		want.MemoryLimitBytes = max(r.MemoryLimitBytes, want.MemoryRequestBytes)
	}
	return want
}

// RunsBeforeRollout reports whether a job workload runs to completion BEFORE
// the workloads it belongs to roll out: a standalone job in the pre-rollout
// phase, or a GATING job (`before = [...]`, `before = [fw.BEFORE_ALL]`), which
// the renderer runs ahead of every workload it names. Either is the shape of
// a service's own migration, which is what the claimable plan admits beside
// its one service.
//
// The two spellings are one fact. EffectiveDeployPhase answers only for a
// standalone job (a gating job's phase is its dependents'), so judging a
// migration by it alone refused the scaffold's `before = [fw.BEFORE_ALL]`
// migrate job, while `before` and `deployPhase` cannot both be set.
func RunsBeforeRollout(s v1alpha1.WorkloadSpec) bool {
	if s.EffectiveKind() != v1alpha1.KindJob {
		return false
	}
	return len(s.Before) > 0 || s.EffectiveDeployPhase() == v1alpha1.DeployPhasePreRollout
}
