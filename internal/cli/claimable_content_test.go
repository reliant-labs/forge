package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/pkg/credentials"
	"github.com/reliant-labs/forge/pkg/testkit"
)

// sharedCredentialFields are the shapes the local check refuses. The SAME
// list is control-plane's forgeLocalCredentialFields
// (internal/contentscan/claim_required_test.go), which asserts the platform
// refuses each as claim-required for credential fields: whatever forge says
// before the build, the platform says after it. Change both together.
var sharedCredentialFields = map[string]string{
	"html password input":        `<form><input type="password" name="pw"></form>`,
	"jsx password input":         `export function Login(){return <input type="password" autoComplete="current-password" />}`,
	"bundled jsx password":       `function L(){return jsx("input",{type:"password"})}`,
	"setAttribute password":      `el.setAttribute("type", "password")`,
	"new-password autocomplete":  `<input autocomplete="new-password">`,
	"one-time-code autocomplete": `<input autocomplete="one-time-code">`,
	"cc-number autocomplete":     `<input autocomplete="cc-number">`,
	"cc-csc autocomplete":        `<input autocomplete="cc-csc">`,
}

func TestCredentialFieldsIn_SharedShapesAreFound(t *testing.T) {
	t.Parallel()
	for name, src := range sharedCredentialFields {
		if got := credentialFieldsIn([]byte(src)); len(got) != 1 {
			t.Errorf("%s: credentialFieldsIn(%q) = %+v, want one field", name, src, got)
		}
	}
}

// An honest page's fields, and prose about passwords, are not credential
// fields. Each of these is admitted by the platform too.
func TestCredentialFieldsIn_HonestPagesPass(t *testing.T) {
	t.Parallel()
	for _, src := range []string{
		`<input type="email" name="email" autocomplete="email">`,
		`<input type="search" placeholder="Search">`,
		`<h1>Password managers compared</h1><p>Use a password manager.</p>`,
		`const policy = { minPasswordLength: 12 }`,
		`<input type="text" autoComplete="username" />`,
		`<textarea name="message"></textarea>`,
	} {
		if got := credentialFieldsIn([]byte(src)); len(got) != 0 {
			t.Errorf("credentialFieldsIn(%q) = %+v, want none", src, got)
		}
	}
}

// forge's own native sign-in scaffold has credential fields: the case the
// local check exists for (new.txt §1 #3). If this stops holding, the scaffold or
// the check changed and the other must be reconsidered.
func TestCredentialFieldsIn_ForgeNativeSignInScaffoldIsFound(t *testing.T) {
	t.Parallel()
	for _, rel := range []string{
		"../templates/frontend/shared-web/src/components/reset-password-form.tsx.tmpl",
		"../templates/frontend/shared-web/src/components/accept-invite-form.tsx.tmpl",
	} {
		data, err := os.ReadFile(rel)
		if errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s is gone: point this test at the native scaffold's sign-in form", rel)
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(credentialFieldsIn(data)) == 0 {
			t.Errorf("%s: no credential field found in forge's own sign-in form", rel)
		}
	}
}

func TestScanCredentialFields_ReadsPagesNotTestsBuildsOrDependencies(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(project, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	login := "export default function SignIn() {\n  return <input\n    type=\"password\" />\n}\n"
	write("frontends/web/src/app/auth/sign-in/page.tsx", login)
	write("frontends/web/src/app/page.tsx", "export default function Home(){return <h1>Notes</h1>}\n")
	write("frontends/web/src/components/login.test.tsx", login)
	write("frontends/web/src/__tests__/fixtures.tsx", login)
	write("frontends/web/node_modules/ui/password.js", login)
	write("frontends/web/out/auth/sign-in.html", `<input type="password">`)
	write("frontends/web/.next/server/page.js", login)
	write("frontends/web/src/types.d.ts", `declare const x: { type: "password" }`)

	got, err := scanCredentialFields(project, filepath.Join(project, "frontends/web"), "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("fields = %+v, want only the sign-in page", got)
	}
	if f := got[0]; f.Path != "frontends/web/src/app/auth/sign-in/page.tsx" || f.Line != 3 || f.Frontend != "web" || f.What != "a password field" {
		t.Errorf("field = %+v", f)
	}
	if missing, err := scanCredentialFields(project, filepath.Join(project, "frontends/nope"), "nope"); err != nil || len(missing) != 0 {
		t.Errorf("a missing frontend directory: %+v, %v; want no finding and no error", missing, err)
	}
}

func TestClaimableCredentialFieldsRefusal_SaysWhatAndHowToProceed(t *testing.T) {
	t.Parallel()
	fields := []credentialField{{Frontend: "web", Path: "frontends/web/src/app/auth/sign-in/page.tsx", Line: 3, What: "a password field"}}
	for _, claimURL := range []string{"", "https://app.reliant.dev/claim#rcl_9f3KQe"} {
		err := claimableCredentialFieldsRefusal("prod", fields, claimURL)
		var r *claimableRefusal
		if !errors.As(err, &r) {
			t.Fatalf("refusal is %T", err)
		}
		if r.exit != exitRefused || r.reason != cloud.ReasonClaimableContentRefused {
			t.Errorf("exit=%d reason=%q", r.exit, r.reason)
		}
		msg := err.Error()
		for _, want := range []string{
			"credential input fields", "Nothing was built or created",
			"frontends/web/src/app/auth/sign-in/page.tsx:3  a password field",
			"Claim to unlock: pages with sign-in forms can't be served anonymously.",
			"forge login && ", "env deploy prod --claimable",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("refusal (claim %q) lacks %q:\n%s", claimURL, want, msg)
			}
		}
		if claimURL != "" && !strings.Contains(msg, "Claim:  "+claimURL) {
			t.Errorf("refusal does not offer this machine's claim link:\n%s", msg)
		}
		if fixOf(err) == "" {
			t.Errorf("refusal names no fix: line for the RESULT line:\n%s", msg)
		}
		if len(r.next) != 2 || !strings.HasPrefix(r.next[0], "forge login") {
			t.Errorf("next = %v", r.next)
		}
	}
}

// claimRequiredErr is the platform's claim-required refusal as forge reads it.
func claimRequiredErr(cause string) error {
	h := http.Header{}
	h.Set(cloud.ReasonHeader, cloud.ReasonClaimableContentRefused)
	if cause != "" {
		h.Set(cloud.ClaimRequiredHeader, cause)
	}
	return &cloud.Error{HTTPStatus: http.StatusBadRequest, Code: cloud.CodeFailedPrecondition, Reason: cloud.ReasonClaimableContentRefused,
		Message: "this site has pages with credential input fields (such as a sign-in form), which an unclaimed deploy can't publish: claim the deploy with its claim link, then publish again; or remove those pages",
		Header:  h}
}

// The refusal the platform makes after the build names the claim link: the
// way forward the old message pointed at but never printed (new.txt §1 #3b).
func TestClaimContentRefusal_PrintsTheClaimLink(t *testing.T) {
	t.Parallel()
	const link = "https://app.reliant.dev/claim#rcl_9f3KQe"
	for _, cause := range []string{cloud.ClaimRequiredCredentialFields, cloud.ClaimRequiredContent} {
		deployErr := claimRequiredErr(cause)
		r := claimContentRefusal("prod", cause, link, deployErr)
		msg := r.Error()
		for _, want := range []string{"Claim:  " + link, "give the claim link", "forge login && ", "Nothing was published"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: refusal lacks %q:\n%s", cause, want, msg)
			}
		}
		if got := strings.Contains(msg, "Claim to unlock: pages with sign-in forms"); got != (cause == cloud.ClaimRequiredCredentialFields) {
			t.Errorf("%s: names sign-in forms = %v:\n%s", cause, got, msg)
		}
		if r.reason != cloud.ReasonClaimableContentRefused || r.exit != exitCodeForError(deployErr) {
			t.Errorf("%s: reason=%q exit=%d: the exit code must stay the one the deploy's error had", cause, r.reason, r.exit)
		}
	}
}

func TestClaimRequiredOf_ReadsTheHeaderNotTheMessage(t *testing.T) {
	t.Parallel()
	if cause, ok := cloud.ClaimRequiredOf(claimRequiredErr(cloud.ClaimRequiredCredentialFields)); !ok || cause != cloud.ClaimRequiredCredentialFields {
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
		LastAttemptClaimRequired: cloud.ClaimRequiredCredentialFields,
	}}
	doc := claimDocument("prod", cred, now, false)
	if doc.Published || doc.ClaimRequired != cloud.ClaimRequiredCredentialFields {
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
	if !strings.Contains(string(b), `"claim_required":"credential_fields"`) {
		t.Errorf("claim_required is not serialised: %s", b)
	}

	var text strings.Builder
	printClaimBlock(&text, "prod", doc)
	out := text.String()
	for _, want := range []string{"Nothing is deployed", "Claim:  https://app.reliant.dev/claim#rcl_9f3KQe",
		"Claim to unlock: pages with sign-in forms can't be served anonymously.", "give the claim link above to your user"} {
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

// The whole CLI: a frontend with a sign-in page fails the preview and the
// deploy in a second, before any proof of work, account or build.
func TestClaimableDeploy_CredentialFieldsRefusedBeforeAnythingIsCreated(t *testing.T) {
	testkit.Slow(t, "renders KCL and runs the whole CLI")
	fake, _ := claimableFixture(t)
	page := filepath.Join("frontends", "web", "src", "SignIn.tsx")
	if err := os.MkdirAll(filepath.Dir(page), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(page, []byte("export const SignIn = () => <input type=\"password\" autoComplete=\"current-password\" />\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runForge(t, "env", "deploy", "hosted", "--claimable", "--plan-only", "--json")
	if err == nil || exitCodeForError(err) != exitRefused {
		t.Fatalf("preview of a sign-in page: want exit 4; err=%v\n%s", err, out)
	}
	doc := decodeClaimDoc(t, out)
	if doc["eligible"] != true || doc["error_reason"] != cloud.ReasonClaimableContentRefused {
		t.Errorf("preview doc = %v", doc)
	}
	if fields, _ := doc["credential_fields"].([]any); len(fields) != 1 {
		t.Errorf("credential_fields = %v, want the sign-in page", doc["credential_fields"])
	}

	out, err = runForge(t, "env", "deploy", "hosted", "--claimable", "--json")
	if err == nil || exitCodeForError(err) != exitRefused {
		t.Fatalf("deploy of a sign-in page: want exit 4; err=%v\n%s", err, out)
	}
	if doc := decodeClaimDoc(t, out); doc["error_reason"] != cloud.ReasonClaimableContentRefused {
		t.Errorf("deploy doc = %v", doc)
	}
	if fake.challenges != 0 || fake.starts != 0 {
		t.Errorf("the refusal came after the account path: challenges=%d starts=%d", fake.challenges, fake.starts)
	}
}

// The platform refuses the upload until the deploy is claimed (content the
// local check does not see): the deploy prints the claim link, and env
// status gives it out rather than holding it back.
func TestClaimableDeploy_PlatformClaimRequiredPrintsTheClaimLink(t *testing.T) {
	testkit.Slow(t, "renders KCL and runs the whole CLI")
	fake, _ := claimableFixture(t)
	fake.refuseBundle = cloud.ClaimRequiredCredentialFields

	out, err := runForge(t, "env", "deploy", "hosted", "--claimable", "--no-wait", "--json")
	if err == nil {
		t.Fatalf("a refused upload exited 0:\n%s", out)
	}
	doc := decodeClaimDoc(t, out)
	claim, _ := doc["claim"].(map[string]any)
	if doc["error_reason"] != cloud.ReasonClaimableContentRefused || claim == nil ||
		claim["claim_url"] != "https://app.reliant.dev/claim#rcl_9f3KQe" || claim["claim_required"] != cloud.ClaimRequiredCredentialFields {
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
