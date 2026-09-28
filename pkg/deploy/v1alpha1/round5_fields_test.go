package v1alpha1

import (
	"strings"
	"testing"
)

var getPods = []PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}}}

// TestClusterRBACOnAnyScheduledKind: clusterRBAC is a Full-only tier any
// scheduled kind may take (workspace-proxy resolves resources cluster-wide).
// It ADDS to the namespaced tier rather than replacing it, so both on one
// workload is valid — including on an operator, whose namespace-local grants
// belong in its Role, not widened into its ClusterRole.
func TestClusterRBACOnAnyScheduledKind(t *testing.T) {
	for _, s := range []WorkloadSpec{
		{Kind: KindService, Image: pinnedImage, ClusterRBAC: getPods},
		{Kind: KindWorker, Image: pinnedImage, ClusterRBAC: getPods},
		{Kind: KindJob, Image: pinnedImage, Args: []string{"x"}, ClusterRBAC: getPods},
		{Kind: KindCron, Image: pinnedImage, Args: []string{"x"}, Schedule: "@daily", ClusterRBAC: getPods},
	} {
		mustPass(t, s, ProfileFull)
		mustFail(t, s.Validate(ProfileRestricted), "clusterRBAC: not allowed under the restricted profile")
	}
	mustPass(t, WorkloadSpec{Kind: KindWorker, Image: pinnedImage, ClusterRBAC: getPods, NamespacedRBAC: getPods}, ProfileFull)
	mustPass(t, WorkloadSpec{Kind: KindOperator, Image: pinnedImage, CRDs: []string{"W"}, ClusterRBAC: getPods, NamespacedRBAC: getPods}, ProfileFull)
	gating := WorkloadSpec{Kind: KindJob, Image: pinnedImage, Args: []string{"x"}, Before: []string{"api"}, ClusterRBAC: getPods}
	mustFail(t, gating.Validate(ProfileFull), "clusterRBAC is not allowed on a job with before")
	if !strings.Contains(FullOnlyReason("clusterRBAC"), "Kubernetes API access") {
		t.Errorf("clusterRBAC's Restricted refusal must say why: %q", FullOnlyReason("clusterRBAC"))
	}
}

// TestTerminationGracePeriodOverride (N1): Full-only, 0..3600, pod-bearing
// long-running and batch kinds only.
func TestTerminationGracePeriodOverride(t *testing.T) {
	ok := WorkloadSpec{Kind: KindWorker, Image: pinnedImage, TerminationGracePeriodSeconds: ptr(int32(60))}
	mustPass(t, ok, ProfileFull)
	mustFail(t, ok.Validate(ProfileRestricted), "terminationGracePeriodSeconds: not allowed under the restricted profile")
	mustPass(t, WorkloadSpec{Kind: KindWorker, Image: pinnedImage, TerminationGracePeriodSeconds: ptr(int32(0))}, ProfileFull)
	for _, v := range []int32{-1, 3601} {
		mustFail(t, WorkloadSpec{Kind: KindWorker, Image: pinnedImage, TerminationGracePeriodSeconds: ptr(v)}.Validate(ProfileFull), "terminationGracePeriodSeconds must be 0-3600")
	}
	mustFail(t, WorkloadSpec{Kind: KindTool, Image: pinnedImage, TerminationGracePeriodSeconds: ptr(int32(10))}.Validate(ProfileFull), `terminationGracePeriodSeconds is not supported for kind "tool"`)
	mustFail(t, WorkloadSpec{Kind: KindJob, Image: pinnedImage, Args: []string{"x"}, Before: []string{"api"}, TerminationGracePeriodSeconds: ptr(int32(10))}.Validate(ProfileFull),
		"terminationGracePeriodSeconds is not allowed on a job with before")
}

// TestActiveDeadlineSeconds (N2): Restricted-allowed, job/cron only, > 0.
func TestActiveDeadlineSeconds(t *testing.T) {
	job := WorkloadSpec{Kind: KindJob, Image: pinnedImage, Args: []string{"x"}, ActiveDeadlineSeconds: ptr(int64(60))}
	mustPass(t, job, ProfileRestricted)
	mustPass(t, WorkloadSpec{Kind: KindCron, Image: pinnedImage, Args: []string{"x"}, Schedule: "@daily", ActiveDeadlineSeconds: ptr(int64(600))}, ProfileFull)
	// A gating job's deadline bounds its initContainer: refused, since an
	// initContainer has no deadline of its own (the Job-level one would be
	// the gated pod's).
	mustFail(t, WorkloadSpec{Kind: KindJob, Image: pinnedImage, Args: []string{"x"}, Before: []string{"api"}, ActiveDeadlineSeconds: ptr(int64(60))}.Validate(ProfileFull),
		"activeDeadlineSeconds is not allowed on a job with before")
	mustFail(t, WorkloadSpec{Kind: KindService, Image: pinnedImage, ActiveDeadlineSeconds: ptr(int64(60))}.Validate(ProfileFull), `activeDeadlineSeconds is not supported for kind "service"`)
	for _, v := range []int64{0, -5} {
		mustFail(t, WorkloadSpec{Kind: KindJob, Image: pinnedImage, Args: []string{"x"}, ActiveDeadlineSeconds: ptr(v)}.Validate(ProfileFull), "activeDeadlineSeconds must be positive")
	}
	if FieldProfiles["activeDeadlineSeconds"] != ProfileRestricted {
		t.Errorf("activeDeadlineSeconds must be Restricted-allowed, got %s", FieldProfiles["activeDeadlineSeconds"])
	}
}

// TestPodSecurityOverride (N3): Full-only; only uid/gid/fsGroup/RO rootfs;
// runAsNonRoot stays forced, so uid/gid/fsGroup 0 is refused.
func TestPodSecurityOverride(t *testing.T) {
	ok := WorkloadSpec{Kind: KindService, Image: pinnedImage, SecurityContext: &PodSecurity{
		RunAsUser: ptr(int64(1000)), RunAsGroup: ptr(int64(1000)), FSGroup: ptr(int64(1000)), ReadOnlyRootFilesystem: ptr(false),
	}}
	mustPass(t, ok, ProfileFull)
	mustFail(t, ok.Validate(ProfileRestricted), "securityContext: not allowed under the restricted profile")
	for field, sc := range map[string]*PodSecurity{
		"runAsUser":  {RunAsUser: ptr(int64(0))},
		"runAsGroup": {RunAsGroup: ptr(int64(0))},
		"fsGroup":    {FSGroup: ptr(int64(-1))},
	} {
		mustFail(t, WorkloadSpec{Image: pinnedImage, SecurityContext: sc}.Validate(ProfileFull), "securityContext."+field)
	}
	mustFail(t, WorkloadSpec{Kind: KindTool, Image: pinnedImage, SecurityContext: &PodSecurity{RunAsUser: ptr(int64(1000))}}.Validate(ProfileFull), `securityContext is not supported for kind "tool"`)
	mustFail(t, WorkloadSpec{Kind: KindJob, Image: pinnedImage, Args: []string{"x"}, Before: []string{"api"}, SecurityContext: &PodSecurity{RunAsUser: ptr(int64(1000))}}.Validate(ProfileFull),
		"securityContext is not allowed on a job with before")
}
