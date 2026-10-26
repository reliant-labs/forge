package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/internal/deploytarget"
	v1alpha1 "github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

// fakeClaimableControlPlane is the hosted-deploy fake plus the ClaimableService
// as the design's §11 wire contract specifies it, and GetTenant (the org the
// anonymous token acts for). Every deploy rpc is checked to arrive carrying
// the anonymous token, never anything else.
type fakeClaimableControlPlane struct {
	*fakeDeployService

	mu         sync.Mutex
	starts     int
	challenges int
	// throttle, when set, makes StartClaimableDeploy answer ResourceExhausted.
	throttle *struct {
		scope   string
		seconds int64
	}
	// claimed makes every authenticated call answer access_token_revoked.
	claimed bool
	// token is what Start hands out.
	token string
	// authSeen is the Authorization header of every non-claimable rpc.
	authSeen []string
	// startedWithAuth is true if StartClaimableDeploy ever carried a bearer.
	startedWithAuth bool
}

func (f *fakeClaimableControlPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/" + cloud.ProcGetClaimableChallenge:
		f.mu.Lock()
		f.challenges++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"challenge":"signed-challenge","difficultyBits":8,"expiresAt":"2030-01-01T00:00:00Z"}`))
		return
	case "/" + cloud.ProcStartClaimableDeploy:
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != "" {
			f.startedWithAuth = true
		}
		var req cloud.ClaimableStartRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if f.throttle != nil {
			w.Header().Set(cloud.ReasonHeader, cloud.ReasonClaimableThrottled)
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": "resource_exhausted", "message": "throttled",
				"details": []map[string]any{{
					"type": "controlplane.v1.ClaimableThrottle", "value": "AA",
					"debug": map[string]any{"retryAfterSeconds": protoInt64(f.throttle.seconds), "scope": f.throttle.scope},
				}},
			})
			return
		}
		if req.Project != "acme" || req.Env != "hosted" || req.DeviceID == "" || req.PoWChallenge != "signed-challenge" || req.PoWNonce == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "invalid_argument", "message": "bad start request"})
			return
		}
		f.starts++
		_, _ = w.Write([]byte(`{"accessToken":"` + f.token + `","orgId":"org-anon","claimUrl":"https://app.reliant.dev/claim#rcl_9f3KQe",` +
			`"expiresAt":"2026-10-29T14:02:00Z","purgeAfter":"2026-11-05T14:02:00Z","reused":false}`))
		return
	}
	f.mu.Lock()
	f.authSeen = append(f.authSeen, r.Header.Get("Authorization"))
	claimed := f.claimed
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		connectErr(w, http.StatusUnauthorized, "unauthenticated", "wrong credential")
		return
	}
	if claimed {
		w.Header().Set(cloud.ReasonHeader, cloud.ReasonAccessTokenRevoked)
		connectErr(w, http.StatusUnauthorized, "unauthenticated", "revoked")
		return
	}
	if r.URL.Path == "/"+cloud.ProcGetOrganization {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cloud.OrganizationReply("4f3c2b1a-0000-4000-8000-0000000000aa"))
		return
	}
	f.fakeDeployService.ServeHTTP(w, r)
}

// protoInt64 spells an int64 as protojson does: a decimal string.
func protoInt64(n int64) string { return strconv.FormatInt(n, 10) }

// claimableFixture is a one-static-site hosted project, the fake control plane
// and every seam the hosted-static e2e test stubs.
func claimableFixture(t *testing.T) (*fakeClaimableControlPlane, string) {
	t.Helper()
	fake := &fakeClaimableControlPlane{fakeDeployService: newFakeDeployService(map[string]string{}), token: "rlat_anonymousToken0123"}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	dir := writeHostedStaticProject(t, srv.URL)
	t.Chdir(dir)
	t.Setenv("ACME_CP_TOKEN", "")
	t.Setenv("FORGE_CONTROL_PLANE_TOKEN", "")
	t.Setenv("FORGE_HOME", t.TempDir())
	t.Setenv("FORGE_CREDENTIAL_HELPER", "")
	prevPoll := hostedPollInterval
	hostedPollInterval = time.Millisecond
	t.Cleanup(func() { hostedPollInterval = prevPoll })
	stubHostedBundleRegistry(t, fake.fakeDeployService)
	prevPush := hostedStaticPusher
	hostedStaticPusher = func(context.Context, string, string, deploytarget.StaticSiteFrontend) (string, error) {
		return hostedStaticDigest, nil
	}
	t.Cleanup(func() { hostedStaticPusher = prevPush })
	prevNow := claimableNow
	claimableNow = func() time.Time { return time.Date(2026, 10, 26, 14, 2, 0, 0, time.UTC) }
	t.Cleanup(func() { claimableNow = prevNow })
	return fake, srv.URL
}

func decodeClaimDoc(t *testing.T, out string) map[string]any {
	t.Helper()
	// The cut may print before the document in a captured run; the JSON
	// document is the last top-level object on stdout.
	i := strings.LastIndex(out, "\n{\n")
	if i < 0 && strings.HasPrefix(out, "{") {
		i = -1
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out[i+1:]), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out)
	}
	return doc
}

// TestClaimableDeploy_PublishesAndPrintsTheClaim is the design's acceptance
// path: no credential plus --claimable publishes, exits 0, prints the claim
// block, and a redeploy reuses the stored entry.
func TestClaimableDeploy_PublishesAndPrintsTheClaim(t *testing.T) {
	if testing.Short() {
		t.Skip("renders KCL and runs the whole CLI; skipped in -short")
	}
	fake, _ := claimableFixture(t)

	out, err := runForge(t, "env", "deploy", "hosted", "--claimable", "--no-wait", "--json")
	if err != nil {
		t.Fatalf("a live unclaimed deploy must exit 0: %v\n%s", err, out)
	}
	doc := decodeClaimDoc(t, out)
	if doc["ok"] != true || doc["exit_code"] != float64(0) {
		t.Fatalf("envelope = ok:%v exit_code:%v", doc["ok"], doc["exit_code"])
	}
	claim, _ := doc["claim"].(map[string]any)
	if claim == nil {
		t.Fatalf("--json carries no `claim` object:\n%s", out)
	}
	for k, want := range map[string]string{
		"state": "live", "claim_url": "https://app.reliant.dev/claim#rcl_9f3KQe",
		"expires_at": "2026-10-29T14:02:00Z", "purge_after": "2026-11-05T14:02:00Z",
	} {
		if claim[k] != want {
			t.Errorf("claim.%s = %v, want %q", k, claim[k], want)
		}
	}
	if u, _ := claim["url"].(string); !strings.HasPrefix(u, "https://") {
		t.Errorf("claim.url = %q, want the deploy's live address", u)
	}
	instr, _ := claim["agent_instructions"].(string)
	for _, want := range []string{"Give claim_url to your user", "whoever claims it first owns the deploy", "env status hosted --claimable"} {
		if !strings.Contains(instr, want) {
			t.Errorf("agent_instructions lacks %q:\n%s", want, instr)
		}
	}
	if fake.starts != 1 || fake.challenges != 1 || fake.startedWithAuth {
		t.Fatalf("starts=%d challenges=%d startedWithAuth=%v, want one authenticated-free start", fake.starts, fake.challenges, fake.startedWithAuth)
	}
	// Every deploy rpc carried the anonymous token and nothing else.
	for _, a := range fake.authSeen {
		if a != "Bearer "+fake.token {
			t.Fatalf("a deploy rpc carried %q, want the anonymous token", a)
		}
	}
	if len(fake.authSeen) == 0 {
		t.Fatal("the deploy made no authenticated calls")
	}
	if fake.bundles[fake.envs["hosted"]]["web"] == nil {
		t.Fatal("the static site was not published")
	}

	// A redeploy reuses the stored entry: no new challenge, no new start.
	out, err = runForge(t, "env", "deploy", "hosted", "--claimable", "--no-wait", "--json")
	if err != nil {
		t.Fatalf("redeploy: %v\n%s", err, out)
	}
	if fake.starts != 1 || fake.challenges != 1 {
		t.Fatalf("a redeploy minted again: starts=%d challenges=%d", fake.starts, fake.challenges)
	}

	// The stored claim is printable again with no credential, and the stored
	// token never resolves as a login.
	statusOut, err := runForge(t, "env", "status", "hosted", "--claimable")
	if err != nil {
		t.Fatalf("status --claimable: %v\n%s", err, statusOut)
	}
	if !strings.Contains(statusOut, "Claim:  https://app.reliant.dev/claim#rcl_9f3KQe") {
		t.Errorf("status --claimable lacks the claim link:\n%s", statusOut)
	}
}

// The text output is the design's block, in order.
func TestPrintClaimBlock_TextMatchesTheDesign(t *testing.T) {
	prev := claimableNow
	claimableNow = func() time.Time { return time.Date(2026, 10, 26, 14, 2, 0, 0, time.UTC) }
	defer func() { claimableNow = prev }()
	var b strings.Builder
	printClaimBlock(&b, "prod", &deployClaimJSON{
		State: "live", URL: "https://amber-quokka.reliantpreview.dev", ClaimURL: "https://app.reliant.dev/claim#rcl_9f3KQe",
		ExpiresAt: time.Date(2026, 10, 29, 14, 2, 0, 0, time.UTC), PurgeAfter: time.Date(2026, 11, 5, 14, 2, 0, 0, time.UTC),
	})
	for _, want := range []string{
		"✓ Live:   https://amber-quokka.reliantpreview.dev",
		"This deploy belongs to no account. It goes offline 2026-10-29 14:02 UTC (in 72h)",
		"and is deleted 7 days after that, unless someone claims it.",
		"Claim:  https://app.reliant.dev/claim#rcl_9f3KQe",
		"FOR AN AGENT: give the claim link above to your user and tell them it expires in 72h.",
		"Opening it signs them up for free and makes this site theirs.",
		"Treat the link like a password: whoever claims it first owns the site. Do not",
		"env status prod --claimable",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("claim block lacks %q:\n%s", want, b.String())
		}
	}
}

func TestClaimableDeploy_ThrottleExits4WithTheRetryBlockAndNeverRetries(t *testing.T) {
	if testing.Short() {
		t.Skip("renders KCL and runs the whole CLI; skipped in -short")
	}
	fake, _ := claimableFixture(t)
	fake.throttle = &struct {
		scope   string
		seconds int64
	}{"platform", 840}

	out, err := runForge(t, "env", "deploy", "hosted", "--claimable", "--json")
	if err == nil {
		t.Fatalf("a throttle must fail:\n%s", out)
	}
	if code := exitCodeForError(err); code != exitRefused {
		t.Fatalf("exit code = %d, want 4", code)
	}
	for _, want := range []string{
		"Claimable deploys are throttled right now: too many new anonymous accounts",
		"(platform-wide limit). Nothing was created.",
		"retry after  14m  (at 2026-10-26T14:16:00Z)",
		"env deploy hosted --claimable",
		"forge login, then",
		"(exit 4: throttled, not a failure of your project)",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("throttle block lacks %q:\n%s", want, err)
		}
	}
	doc := decodeClaimDoc(t, out)
	if doc["ok"] != false || doc["exit_code"] != float64(4) || doc["error_code"] != "resource_exhausted" ||
		doc["error_reason"] != "claimable_throttled" || doc["retry_after_seconds"] != float64(840) ||
		doc["retry_at"] != "2026-10-26T14:16:00Z" || doc["throttle_scope"] != "platform" {
		t.Fatalf("json facts = %v", doc)
	}
	if _, has := doc["claim"]; has {
		t.Error("a throttled start must not carry a claim")
	}
	if fake.challenges != 1 {
		t.Errorf("forge asked for %d challenges; a throttle must never be retried", fake.challenges)
	}
	// Nothing was stored, so nothing can later pass for a deploy.
	if _, err := cloud.LoadClaimable(cloud.Endpoint{URL: fakeURLOf(t)}, "acme", "hosted", claimableNow()); err == nil {
		t.Error("a throttled start stored a credential")
	}
}

func fakeURLOf(t *testing.T) string {
	t.Helper()
	ep, err := RenderKCL(context.Background(), projectDirForKCL(), "hosted")
	if err != nil {
		t.Fatal(err)
	}
	return ep.ControlPlane.Endpoint
}

func TestNewClaimableThrottle_ScopesReadAsTheDesignSays(t *testing.T) {
	prev := claimableNow
	claimableNow = func() time.Time { return time.Date(2026, 10, 26, 14, 48, 0, 0, time.UTC) }
	defer func() { claimableNow = prev }()
	for scope, want := range map[string]string{
		"network":  "too many claimable deploys from your network",
		"device":   "this machine already has 3 live claimable deploys",
		"platform": "too many new anonymous accounts",
	} {
		err := newClaimableThrottle("prod", cloud.ClaimableThrottle{Scope: scope, RetryAfter: 14 * time.Minute})
		if !strings.Contains(err.Error(), want) {
			t.Errorf("scope %s: %q lacks %q", scope, err, want)
		}
		if got := exitCodeForError(err); got != exitRefused {
			t.Errorf("scope %s exit = %d", scope, got)
		}
		facts := errorFactsOf(err)
		if facts.ThrottleScope != scope || facts.RetryAfterSeconds != 840 || facts.ErrorReason != "claimable_throttled" ||
			facts.RetryAt != "2026-10-26T15:02:00Z" {
			t.Errorf("scope %s facts = %+v", scope, facts)
		}
	}
}

func TestFormatRetryAfter(t *testing.T) {
	for d, want := range map[time.Duration]string{
		45 * time.Second: "45s", 14 * time.Minute: "14m", 14*time.Minute + time.Second: "15m",
		time.Hour: "1h", 65 * time.Minute: "1h5m",
	} {
		if got := formatRetryAfter(d); got != want {
			t.Errorf("formatRetryAfter(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestClaimableDeploy_SignedInIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("renders KCL and runs the whole CLI; skipped in -short")
	}
	fake, _ := claimableFixture(t)
	t.Setenv("ACME_CP_TOKEN", "rlat_real_login")

	out, err := runForge(t, "env", "deploy", "hosted", "--claimable", "--json")
	if err == nil || exitCodeForError(err) != 1 {
		t.Fatalf("want exit 1; err=%v code=%d\n%s", err, exitCodeForError(err), out)
	}
	if !strings.Contains(err.Error(), "you are signed in to") || !strings.Contains(err.Error(), "drop --claimable to deploy to your organization") {
		t.Errorf("refusal text:\n%s", err)
	}
	if fake.challenges != 0 || fake.starts != 0 {
		t.Errorf("a signed-in --claimable still minted an anonymous account (challenges=%d starts=%d)", fake.challenges, fake.starts)
	}
	if strings.Contains(err.Error(), "rlat_real_login") {
		t.Error("the refusal printed the credential")
	}
}

func TestClaimableDeploy_ClaimedDeployExits4(t *testing.T) {
	if testing.Short() {
		t.Skip("renders KCL and runs the whole CLI; skipped in -short")
	}
	fake, _ := claimableFixture(t)
	if out, err := runForge(t, "env", "deploy", "hosted", "--claimable", "--no-wait", "--json"); err != nil {
		t.Fatalf("first deploy: %v\n%s", err, out)
	}
	fake.claimed = true
	out, err := runForge(t, "env", "deploy", "hosted", "--claimable", "--no-wait", "--json")
	if err == nil || exitCodeForError(err) != exitRefused {
		t.Fatalf("a claimed deploy must exit 4; err=%v\n%s", err, out)
	}
	if !strings.Contains(err.Error(), "this deploy was claimed") || !strings.Contains(err.Error(), "run forge login and deploy without --claimable") {
		t.Errorf("claimed text:\n%s", err)
	}
	if doc := decodeClaimDoc(t, out); doc["error_reason"] != "claimable_claimed" || doc["exit_code"] != float64(4) {
		t.Errorf("json facts = %v", doc)
	}
	if fake.starts != 1 {
		t.Errorf("a claimed deploy was re-minted (starts=%d)", fake.starts)
	}
}

func TestClaimableIneligibility(t *testing.T) {
	t.Parallel()
	svc := func(name, kind string) WorkloadEntity {
		w := WorkloadEntity{Name: name, Runtime: RuntimeEntity{Type: RuntimeHosted}}
		w.Spec.Kind = v1alpha1.WorkloadKind(kind)
		return w
	}
	cases := []struct {
		name string
		e    *KCLEntities
		want []string
	}{
		{"one service", &KCLEntities{Workloads: []WorkloadEntity{svc("api", "service")}}, nil},
		{"service and its pre-rollout migration", &KCLEntities{Workloads: []WorkloadEntity{svc("api", "service"), svc("migrate", "job")}}, nil},
		{"a worker", &KCLEntities{Workloads: []WorkloadEntity{svc("api", "service"), svc("w", "worker")}}, []string{"worker \"w\""}},
		{"two services", &KCLEntities{Workloads: []WorkloadEntity{svc("a", "service"), svc("b", "service")}}, []string{"2 services"}},
		{"a cron", &KCLEntities{Workloads: []WorkloadEntity{svc("c", "cron")}}, []string{"cron \"c\""}},
		{"two databases", &KCLEntities{Databases: []DatabaseEntity{{Runtime: RuntimeHosted}, {Runtime: RuntimeHosted}}}, []string{"2 databases"}},
	}
	for _, c := range cases {
		got := strings.Join(claimableIneligibility(c.e), "|")
		if len(c.want) == 0 && got != "" {
			t.Errorf("%s: unexpected reasons %q", c.name, got)
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: reasons %q lack %q", c.name, got, w)
			}
		}
	}
	err := claimableIneligibleError("prod", []string{"worker \"w\" (only one service may run)"})
	if exitCodeForError(err) != exitRefused || !strings.Contains(err.Error(), "sign up for the rest: forge login") {
		t.Errorf("ineligible error: code=%d %s", exitCodeForError(err), err)
	}
}

func TestRefuseClaimableFlags(t *testing.T) {
	t.Parallel()
	var f deployCmdFlags
	if err := refuseClaimableFlags("prod", f); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*deployCmdFlags){
		"--dry-run":  func(f *deployCmdFlags) { f.dryRun = true },
		"--explain":  func(f *deployCmdFlags) { f.explain = true },
		"--target":   func(f *deployCmdFlags) { f.targets = []string{"api"} },
		"a release":  func(f *deployCmdFlags) { f.promote.version = "v1" },
		"--plan":     func(f *deployCmdFlags) { f.promote.plan = true },
		"--offline-": func(f *deployCmdFlags) { f.offlineLedger = true },
	} {
		g := deployCmdFlags{}
		mod(&g)
		if err := refuseClaimableFlags("prod", g); err == nil || !strings.Contains(err.Error(), "--claimable cannot be combined") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// The error a claimed deploy yields is a refusal, not a queued deploy.
func TestClaimableDeployError_QueuedIsRefusedNotHeld(t *testing.T) {
	t.Parallel()
	err := claimableDeployError("prod", &deployQueuedError{Env: "prod", Holds: []deploytarget.HostedHold{{Kind: "billing"}}})
	var r *claimableRefusal
	if !errors.As(err, &r) || r.exit != exitRefused {
		t.Fatalf("err = %v", err)
	}
}
