package cloud

import (
	"errors"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/credentials"
)

func TestClaimableProjectInstancesKeepIndependentCredentials(t *testing.T) {
	t.Setenv("FORGE_HOME", t.TempDir())
	ep := endpointFor(t, "prod", "https://cp.example.com", "")
	now := time.Now()
	for _, id := range []string{"", "instance-a", "instance-b"} {
		_, err := StoreClaimable(ep, "counter", "prod", ClaimableDeploy{
			AccessToken: "token-" + id, OrgID: "org-" + id,
			ExpiresAt: now.Add(time.Hour), PurgeAfter: now.Add(2 * time.Hour),
		}, now, id)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := RecordClaimableURL(ep, "counter", "prod", "https://a.example", "instance-a"); err != nil {
		t.Fatal(err)
	}
	if err := RecordClaimableAttempt(ep, "counter", "prod", now, nil, "instance-a"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "instance-a", "instance-b"} {
		got, err := LoadClaimable(ep, "counter", "prod", now, id)
		if err != nil || got.Token != "token-"+id {
			t.Fatalf("instance %q returned a different credential: %v", id, err)
		}
		if (got.Claimable.URL != "") != (id == "instance-a") ||
			(!got.Claimable.LastPublishedAt.IsZero()) != (id == "instance-a") {
			t.Fatalf("publication state leaked between instances: %q", id)
		}
	}
	if err := ForgetClaimable(ep, "counter", "prod", "instance-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadClaimable(ep, "counter", "prod", now, "instance-a"); !errors.Is(err, credentials.ErrNotFound) {
		t.Fatalf("forgotten instance still resolves: %v", err)
	}
	if _, err := LoadClaimable(ep, "counter", "prod", now, "instance-b"); err != nil {
		t.Fatalf("forget removed the other instance: %v", err)
	}
	if _, err := LoadClaimable(ep, "counter", "prod", now); err != nil {
		t.Fatalf("legacy credentials were lost: %v", err)
	}
}
