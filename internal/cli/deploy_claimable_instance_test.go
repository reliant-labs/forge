package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/pkg/testkit"
)

func TestClaimableDeploy_ProjectInstancesDoNotReuseLegacyOrOtherProjects(t *testing.T) {
	testkit.Slow(t, "renders KCL and runs the deployment CLI")
	fake, _ := claimableFixture(t)
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	fake.availability = &cloud.ClaimableAvailability{Available: true, ComputeAvailable: true, ProjectInstanceIDs: true}
	ep := cloud.Endpoint{URL: fakeURLOf(t)}
	if _, err := cloud.StoreClaimable(ep, "acme", "hosted", cloud.ClaimableDeploy{
		AccessToken: "old-claimed-project", ExpiresAt: claimableNow().Add(time.Hour),
		PurgeAfter: claimableNow().Add(2 * time.Hour),
	}, claimableNow()); err != nil {
		t.Fatal(err)
	}
	ids := []string{"8eb52828-0e0a-420b-9cce-33624584c64e", "ee7d1ea9-c6a9-4bfe-8846-cf976cc6ae40"}
	for i, id := range ids {
		if err := generator.SetProjectConfigScalar(filepath.Join(dir, "forge.yaml"), "project_instance_id", id); err != nil {
			t.Fatal(err)
		}
		for attempt := 0; attempt < 2; attempt++ {
			out, err := runForge(t, "env", "deploy", "hosted", "--claimable", "--no-wait", "--json")
			if err != nil {
				t.Fatalf("instance %d attempt %d: %v\n%s", i, attempt, err, out)
			}
		}
		if fake.starts != i+1 {
			t.Fatalf("new instance must start once, retry must resume: starts=%d", fake.starts)
		}
	}
	if len(fake.startRequests) != 2 || fake.startRequests[0].ProjectInstanceID != ids[0] || fake.startRequests[1].ProjectInstanceID != ids[1] {
		t.Fatal("server did not receive distinct project identities")
	}
	if fake.startRequests[0].DeviceID != fake.startRequests[1].DeviceID {
		t.Fatal("project identity must not change the machine's rate-limit identity")
	}
}

func TestClaimableDeploy_ProjectInstanceRequiresServerCapability(t *testing.T) {
	testkit.Slow(t, "renders KCL and runs the deployment CLI")
	fake, _ := claimableFixture(t)
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := generator.SetProjectConfigScalar(filepath.Join(dir, "forge.yaml"), "project_instance_id", "8eb52828-0e0a-420b-9cce-33624584c64e"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"env", "deploy", "hosted", "--claimable", "--json"},
		{"env", "deploy", "hosted", "--claimable", "--plan-only", "--json"},
	} {
		out, err := runForge(t, args...)
		if err == nil || !strings.Contains(out, "does not yet support project instance identities") {
			t.Fatalf("older server must fail before allocation: %v\n%s", err, out)
		}
	}
	if fake.starts != 0 || fake.challenges != 0 {
		t.Fatal("unsupported server minted an identity")
	}
}
