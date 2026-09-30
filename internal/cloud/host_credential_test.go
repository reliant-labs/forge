package cloud

import (
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/cloudcred"
)

// credPath is the credentials file this process would use — the same
// resolution forge itself performs, so a test writes where forge reads.
func credPath(t *testing.T) string {
	t.Helper()
	path, err := CredentialsPath()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// TestResolveCredential_HostDepositedIsTheFallback is the whole point of
// pkg/cloudcred: an application embedding forge deposits the user's token, and
// a later forge command finds it with no `forge login`.
func TestResolveCredential_HostDepositedIsTheFallback(t *testing.T) {
	t.Setenv("FORGE_HOME", t.TempDir())
	t.Setenv(DefaultTokenEnv, "")
	ep := endpointFor(t, "prod", "https://cp.example.com", "")

	if err := cloudcred.Save(credPath(t), ep.URL, cloudcred.Credential{
		Token:       "rlat_hostdeposited",
		TokenPrefix: "rlat_hostdep",
		Scopes:      []string{"deploy:write"},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveCredential("", ep)
	if err != nil {
		t.Fatalf("a host-deposited credential must resolve: %v", err)
	}
	if got.Token != "rlat_hostdeposited" {
		t.Errorf("token = %q, want the host-deposited one", got.Token)
	}
	if got.Source != SourceHost {
		t.Errorf("Source = %q, want %q so the user can see where it came from", got.Source, SourceHost)
	}
}

// TestResolveCredential_ForgeLoginBeatsHostDeposit: a human who ran
// `forge login` chose that credential deliberately for this endpoint. A host
// application signed in as somebody else must never displace it.
func TestResolveCredential_ForgeLoginBeatsHostDeposit(t *testing.T) {
	t.Setenv("FORGE_HOME", t.TempDir())
	t.Setenv(DefaultTokenEnv, "")
	ep := endpointFor(t, "prod", "https://cp.example.com", "")

	storeFor(t, ep.URL, "from-forge-login")
	if err := cloudcred.Save(credPath(t), ep.URL, cloudcred.Credential{Token: "from-host"}); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveCredential("", ep)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "from-forge-login" || got.Source != SourceLogin {
		t.Fatalf("`forge login` must win over a host deposit: %+v", got)
	}
}

// TestResolveCredential_EnvBeatsHostDeposit keeps CI overridable. A pipeline
// setting the variable must win over whatever is on the runner's disk.
func TestResolveCredential_EnvBeatsHostDeposit(t *testing.T) {
	t.Setenv("FORGE_HOME", t.TempDir())
	ep := endpointFor(t, "prod", "https://cp.example.com", "ACME_TOKEN")
	if err := cloudcred.Save(credPath(t), ep.URL, cloudcred.Credential{Token: "from-host"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ACME_TOKEN", "from-env")

	got, err := ResolveCredential("", ep)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "from-env" || got.Source != SourceEnv {
		t.Fatalf("the declared env var must beat a host deposit: %+v", got)
	}
}

// TestResolveCredential_HostDepositIsKeyedByEndpoint: depositing for staging
// must never authenticate a prod command.
func TestResolveCredential_HostDepositIsKeyedByEndpoint(t *testing.T) {
	t.Setenv("FORGE_HOME", t.TempDir())
	t.Setenv(DefaultTokenEnv, "")
	staging := endpointFor(t, "staging", "https://staging.example.com", "")
	prod := endpointFor(t, "prod", "https://prod.example.com", "")

	if err := cloudcred.Save(credPath(t), staging.URL, cloudcred.Credential{Token: "staging-only"}); err != nil {
		t.Fatal(err)
	}

	if _, err := ResolveCredential("", prod); err == nil {
		t.Fatal("a staging deposit must not authenticate prod")
	}
	got, err := ResolveCredential("", staging)
	if err != nil || got.Token != "staging-only" {
		t.Fatalf("staging must still resolve: %+v %v", got, err)
	}
}

// TestResolveCredential_ExpiredHostDepositPointsAtTheHost. An expired host
// credential has a different fix from an expired `forge login`: the host
// refreshes it, so telling the user to run `forge login` sends them to the
// wrong place.
func TestResolveCredential_ExpiredHostDepositPointsAtTheHost(t *testing.T) {
	t.Setenv("FORGE_HOME", t.TempDir())
	t.Setenv(DefaultTokenEnv, "")
	ep := endpointFor(t, "prod", "https://cp.example.com", "")

	past := time.Now().Add(-time.Hour)
	if err := cloudcred.Save(credPath(t), ep.URL, cloudcred.Credential{Token: "stale", ExpiresAt: &past}); err != nil {
		t.Fatal(err)
	}

	_, err := ResolveCredential("", ep)
	if err == nil {
		t.Fatal("an expired credential must be refused, not presented")
	}
	if !strings.Contains(err.Error(), "hosting forge") {
		t.Errorf("the message must point at the host application; got: %v", err)
	}
}

// TestCloudCredDelete_LeavesForgeLoginAlone. Logging out of the host must not
// log the user out of forge: the two are separate entries.
func TestCloudCredDelete_LeavesForgeLoginAlone(t *testing.T) {
	t.Setenv("FORGE_HOME", t.TempDir())
	t.Setenv(DefaultTokenEnv, "")
	ep := endpointFor(t, "prod", "https://cp.example.com", "")

	storeFor(t, ep.URL, "from-forge-login")
	if err := cloudcred.Save(credPath(t), ep.URL, cloudcred.Credential{Token: "from-host"}); err != nil {
		t.Fatal(err)
	}

	existed, err := cloudcred.Delete(credPath(t), ep.URL)
	if err != nil || !existed {
		t.Fatalf("Delete must report the entry it removed: %v %v", existed, err)
	}

	got, err := ResolveCredential("", ep)
	if err != nil {
		t.Fatalf("the `forge login` credential must survive a host logout: %v", err)
	}
	if got.Token != "from-forge-login" || got.Source != SourceLogin {
		t.Fatalf("wrong credential survived: %+v", got)
	}

	// Deleting again is not an error; it reports that nothing was there.
	existed, err = cloudcred.Delete(credPath(t), ep.URL)
	if err != nil || existed {
		t.Fatalf("a second Delete must be a no-op: %v %v", existed, err)
	}
}

// TestCloudCredSave_RefusesEmptyToken. Storing "" would make the endpoint look
// authenticated and then fail every call with a 401 that names no cause.
func TestCloudCredSave_RefusesEmptyToken(t *testing.T) {
	t.Setenv("FORGE_HOME", t.TempDir())
	if err := cloudcred.Save(credPath(t), "https://cp.example.com", cloudcred.Credential{}); err == nil {
		t.Fatal("an empty token must be refused at the write, not discovered at the call")
	}
}
