package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/pkg/credentials"
	"github.com/reliant-labs/forge/pkg/testkit"
)

// claimRequiredErr is the platform's claim-required refusal as forge reads it.
func claimRequiredErr(cause string) error {
	h := http.Header{}
	h.Set(cloud.ReasonHeader, cloud.ReasonClaimableContentRefused)
	if cause != "" {
		h.Set(cloud.ClaimRequiredHeader, cause)
	}
	return &cloud.Error{HTTPStatus: http.StatusBadRequest, Code: cloud.CodeFailedPrecondition, Reason: cloud.ReasonClaimableContentRefused,
		Message: "this content can't be published from an unclaimed deploy: claim the deploy with its claim link, then publish again",
		Header:  h}
}

// The refusal the platform makes after the build names the claim link: the
// way forward the old message pointed at but never printed (new.txt §1 #3b).
func TestClaimContentRefusal_PrintsTheClaimLink(t *testing.T) {
	t.Parallel()
	const link = "https://app.reliant.dev/claim#rcl_9f3KQe"
	for _, cause := range []string{cloud.ClaimRequiredContent, "a_newer_cause"} {
		deployErr := claimRequiredErr(cause)
		r := claimContentRefusal("prod", link, deployErr)
		msg := r.Error()
		for _, want := range []string{"Claim:  " + link, "give the claim link", "forge login && ", "Nothing was published"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: refusal lacks %q:\n%s", cause, want, msg)
			}
		}
		if !strings.Contains(msg, "Claim to unlock") {
			t.Errorf("%s: does not say what claiming unlocks:\n%s", cause, msg)
		}
		if r.reason != cloud.ReasonClaimableContentRefused || r.exit != exitCodeForError(deployErr) {
			t.Errorf("%s: reason=%q exit=%d: the exit code must stay the one the deploy's error had", cause, r.reason, r.exit)
		}
	}
}

func TestClaimRequiredOf_ReadsTheHeaderNotTheMessage(t *testing.T) {
	t.Parallel()
	if cause, ok := cloud.ClaimRequiredOf(claimRequiredErr(cloud.ClaimRequiredContent)); !ok || cause != cloud.ClaimRequiredContent {
		t.Errorf("ClaimRequiredOf = %q, %v", cause, ok)
	}
	// A control plane that predates the header: the reason alone is a
	// content refusal, not one forge knows claiming lifts.
	if _, ok := cloud.ClaimRequiredOf(claimRequiredErr("")); ok {
		t.Error("a refusal with no claim-required header read as claim-required")
	}
	if _, ok := cloud.ClaimRequiredOf(errors.New("claim the deploy")); ok {
		t.Error("message text read as claim-required")
	}
}

// env status of a deploy refused until claimed gives the claim link out, and
// never says to hold it back (new.txt §1 #3b): claiming is the way forward.
func TestClaimDocument_ClaimRequiredGivesTheLinkOut(t *testing.T) {
	now := time.Date(2026, 11, 17, 22, 0, 0, 0, time.UTC)
	prev := claimableNow
	claimableNow = func() time.Time { return now }
	defer func() { claimableNow = prev }()
	cred := credentials.Credential{Claimable: &credentials.Claimable{
		ClaimURL:  "https://app.reliant.dev/claim#rcl_9f3KQe",
		ExpiresAt: now.Add(71 * time.Hour), PurgeAfter: now.Add(239 * time.Hour),
		LastAttemptAt: now.Add(-time.Minute), LastAttemptError: "RecordBundle failed (HTTP 400 failed_precondition)",
		LastAttemptClaimRequired: cloud.ClaimRequiredContent,
	}}
	doc := claimDocument("prod", cred, now, false)
	if doc.Published || doc.ClaimRequired != cloud.ClaimRequiredContent {
		t.Fatalf("doc = %+v", doc)
	}
	for _, want := range []string{"Nothing is deployed yet", "Claim to unlock", "give claim_url to your user", "forge login && "} {
		if !strings.Contains(doc.AgentInstructions, want) {
			t.Errorf("agent_instructions lacks %q:\n%s", want, doc.AgentInstructions)
		}
	}
	if strings.Contains(doc.AgentInstructions, "Do NOT give") {
		t.Errorf("agent_instructions holds the link back:\n%s", doc.AgentInstructions)
	}
	b, _ := json.Marshal(doc)
	if !strings.Contains(string(b), `"claim_required":"content"`) {
		t.Errorf("claim_required is not serialised: %s", b)
	}

	var text strings.Builder
	printClaimBlock(&text, "prod", doc)
	out := text.String()
	for _, want := range []string{"Nothing is deployed", "Claim:  https://app.reliant.dev/claim#rcl_9f3KQe",
		"Claim to unlock", "give the claim link above to your user"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "do not give the claim link") || strings.Contains(out, "✓ Live") {
		t.Errorf("status contradicts the claim-required refusal:\n%s", out)
	}

	// The MCP relay says the same.
	res := &claimableDeployResult{Env: "prod"}
	fillClaim(res, doc)
	steps := claimNextSteps(res)
	if !strings.Contains(steps, "claim link "+cred.Claimable.ClaimURL) || strings.Contains(steps, "do not give them the claim link") {
		t.Errorf("next_steps:\n%s", steps)
	}
}

// The platform refuses the upload until the deploy is claimed (content the
// local check does not see): the deploy prints the claim link, and env
// status gives it out rather than holding it back.
func TestClaimableDeploy_PlatformClaimRequiredPrintsTheClaimLink(t *testing.T) {
	testkit.Slow(t, "renders KCL and runs the whole CLI")
	fake, _ := claimableFixture(t)
	fake.refuseBundle = cloud.ClaimRequiredContent

	out, err := runForge(t, "env", "deploy", "hosted", "--claimable", "--no-wait", "--json")
	if err == nil {
		t.Fatalf("a refused upload exited 0:\n%s", out)
	}
	doc := decodeClaimDoc(t, out)
	claim, _ := doc["claim"].(map[string]any)
	if doc["error_reason"] != cloud.ReasonClaimableContentRefused || claim == nil ||
		claim["claim_url"] != "https://app.reliant.dev/claim#rcl_9f3KQe" || claim["claim_required"] != cloud.ClaimRequiredContent {
		t.Fatalf("deploy doc = %v", doc)
	}
	if !strings.Contains(err.Error(), "Claim:  https://app.reliant.dev/claim#rcl_9f3KQe") {
		t.Errorf("the refusal does not print the claim link:\n%v", err)
	}

	statusOut, err := runForge(t, "env", "status", "hosted", "--claimable")
	if err != nil {
		t.Fatalf("status --claimable: %v\n%s", err, statusOut)
	}
	if !strings.Contains(statusOut, "Claim:  https://app.reliant.dev/claim#rcl_9f3KQe") || strings.Contains(statusOut, "do not give the claim link") {
		t.Errorf("status --claimable after a claim-required refusal:\n%s", statusOut)
	}
}
