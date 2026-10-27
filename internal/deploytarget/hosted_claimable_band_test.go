package deploytarget

import (
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

// The claimable plan is 250m / 256 MiB, which is off the 4 GiB-per-vCPU band
// by construction. The band is a billing rule and that plan is never billed,
// so an anonymous --claimable deploy skips it; every other hosted deploy is
// still banded. Mutation: dropping the ClaimableActive guard in planHostedWith
// fails the first half; making it unconditional fails the second.
func TestHostedPlanSkipsTheShapeBandOnlyForAClaimableDeploy(t *testing.T) {
	claimableShape := v1alpha1.Resources{
		CPURequestMillicores: 250, CPULimitMillicores: 250,
		MemoryRequestBytes: 256 << 20, MemoryLimitBytes: 256 << 20,
	}
	group := func() ServiceGroup {
		return hostedGroup("v1", map[string]string{"api": digestA}, claimableShape)
	}

	if _, err := planHosted(group()); err == nil || !strings.Contains(err.Error(), "shape band") {
		t.Fatalf("a normal hosted deploy of 250m/256Mi: err = %v, want the shape-band refusal", err)
	}

	restore := cloud.UseClaimableCredential(cloud.Endpoint{URL: "https://cp.example"}, "rlat_anon", func() *time.Time { x := time.Now().Add(time.Hour); return &x }())
	defer restore()
	plan, err := planHosted(group())
	if err != nil {
		t.Fatalf("a claimable deploy of 250m/256Mi must pass the plan: %v", err)
	}
	if len(plan) == 0 {
		t.Fatal("the claimable plan published nothing")
	}

	restore()
	if _, err := planHosted(group()); err == nil || !strings.Contains(err.Error(), "shape band") {
		t.Fatalf("after the claimable deploy ends the band applies again: err = %v", err)
	}
}
