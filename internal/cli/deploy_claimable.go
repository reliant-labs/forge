package cli

// `forge env deploy <env> --claimable`: a deploy with no account.
//
// An agent with no Reliant account runs the flag and gets two things back: a
// live URL, and a CLAIM LINK it hands to its human. Unless a human opens the
// link the deploy expires. The design is control-plane's
// docs/design/claimable-deploys.md (§6 is this file's spec).
//
// THE PATH RUNS ONLY WHEN ASKED FOR. There is no auto-detection: a missing
// credential stays a missing credential (its message now names the flag), and
// a signed-in user who passes --claimable is refused, so nobody creates an
// orphan preview by accident.
//
// HOW IT FITS THE ORDINARY DEPLOY. This is a separate branch in front of
// runDeployEverything, not a second deploy. It (1) checks no real credential
// resolves, (2) checks the render fits what an unclaimed deploy may be, (3)
// obtains the anonymous token (the stored one, or StartClaimableDeploy behind
// a proof of work), (4) installs the token for this process only, so the
// unchanged hosted publish runs as the anonymous user, and (5) prints the claim.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/pkg/credentials"
	v1alpha1 "github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

// The refusal reasons forge itself raises on this path, beside the control
// plane's (cloud.ReasonClaimable*).
const (
	reasonClaimableSignedIn   = "claimable_signed_in"
	reasonClaimableIneligible = "claimable_ineligible"
)

// The claimable plan's caps (control-plane internal/planlimits/claimable.go,
// design §2). The control plane enforces them at admission; they are checked
// here too, in the same words, so a refusal costs no build. Per workload, and
// with no burst: the limit is held to the cap as well as the request.
const (
	claimableMaxServices     = 1
	claimableMaxReplicas     = 1
	claimableMaxCPUMillicore = 250
	claimableMaxMemoryMiB    = 256
	claimableMaxDatabases    = 1
	claimableMaxStaticSites  = 1
)

// claimableAPI builds the unauthenticated ClaimableService client. A var so the
// CLI tests state the control plane's answers.
var claimableAPI = func(ep cloud.Endpoint) cloud.ClaimableAPI { return cloud.NewClaimableClient(ep) }

// claimableNow is the clock the throttle's "retry at" and the claim's "in 72h"
// are computed from. A var for the tests.
var claimableNow = time.Now

// claimableSession is one anonymous deploy as this invocation sees it: where it
// lives, its token, and its claim facts. It is what the output renders.
type claimableSession struct {
	ep      cloud.Endpoint
	project string
	env     string
	cred    credentials.Credential
	// reused is true when the stored deploy of an earlier run was used.
	reused bool
}

// deployClaimJSON is the `claim` object of the --json document (§6): present on
// a claimable deploy, absent on every ordinary one.
type deployClaimJSON struct {
	// State is live | suspended | claimed | expired.
	State string `json:"state"`
	// URL is the deploy's live address; empty when it has none yet.
	URL string `json:"url,omitempty"`
	// ClaimURL is the link a human opens to own the deploy. Sensitive: whoever
	// opens it first owns the deploy.
	ClaimURL string `json:"claim_url"`
	// ExpiresAt is when the deploy goes offline; PurgeAfter when it is deleted.
	ExpiresAt  time.Time `json:"expires_at"`
	PurgeAfter time.Time `json:"purge_after"`
	// AgentInstructions is what the agent must do with the above.
	AgentInstructions string `json:"agent_instructions"`
}

// claimableRefusal is a refusal this path raises itself (or a throttle it
// translated): an error that carries its exit code and the machine-readable
// facts the --json envelope reports.
type claimableRefusal struct {
	exit int
	// code is the Connect code name; reason the stable branch-on reason.
	code, reason string
	msg          string
	// throttle is set for claimable_throttled.
	throttle *claimableThrottleFacts
	// emitted is true when the deploy's own --json document already carries
	// this outcome, so it must not be written a second time.
	emitted bool
}

type claimableThrottleFacts struct {
	scope      string
	retryAfter time.Duration
	retryAt    time.Time
}

func (e *claimableRefusal) Error() string { return e.msg }
func (e *claimableRefusal) ExitCode() int { return e.exit }

// refuseClaimableFlags rejects the flag combinations that make no sense for a
// deploy that creates an anonymous account: anything that previews, scopes, or
// names a release.
func refuseClaimableFlags(envName string, f deployCmdFlags) error {
	bad := func(flag, why string) error {
		return fmt.Errorf("--claimable cannot be combined with %s: %s", flag, why)
	}
	switch {
	case f.offlineLedger:
		return bad("--offline-ledger", "an anonymous deploy always talks to the control plane")
	case f.explain:
		return bad("--explain", "it describes a cluster apply, and an anonymous deploy has none")
	case f.dryRun:
		return bad("--dry-run", "the deploy creates the anonymous account; there is nothing to preview without it")
	case f.promote.plan || f.promote.planOnly:
		return bad("--plan/--plan-only", "an anonymous deploy is created by running it")
	case f.promote.requestedRelease():
		return bad("a release version or --from", "--claimable ships this checkout; promote a release with your own account")
	case len(f.targets) > 0 || f.frontendsOnly || f.skipFrontend:
		return bad("--target/--frontends-only/--skip-frontend", "an anonymous deploy publishes the whole env")
	}
	return nil
}

// dispatchClaimableDeploy is `forge env deploy <env> --claimable`.
func dispatchClaimableDeploy(ctx context.Context, envName string, f deployCmdFlags) error {
	report := newDeployReport(envName, f.jsonOut)
	out := progressWriter(f.jsonOut)
	err := func() error {
		if err := refuseClaimableFlags(envName, f); err != nil {
			return err
		}
		projectDir := projectDirForKCL()
		entities, err := RenderKCL(ctx, projectDir, envName)
		if err != nil {
			return err
		}
		if entities == nil || !entities.HasHosted() || entities.ControlPlane == nil {
			return fmt.Errorf("env %q publishes nothing to a hosted control plane, so there is nothing to deploy with --claimable.\n"+
				"fix: bind the env's workloads to forge.OnHosted (deploy/hosting), or drop --claimable", envName)
		}
		if envAppliesLocally(entities) {
			return fmt.Errorf("env %q applies part of itself from this machine (a cluster, compose or host part), "+
				"which an anonymous deploy cannot do. --claimable publishes only hosted workloads", envName)
		}
		ep, err := projectCloudEndpoint(envName, declarationFromEntities(entities))
		if err != nil {
			return err
		}
		if err := refuseClaimableWhenSignedIn(ep); err != nil {
			return err
		}
		if reasons := claimableIneligibility(entities); len(reasons) > 0 {
			return claimableIneligibleError(envName, reasons)
		}
		project := hostedProjectName()
		if project == "" {
			return errors.New("--claimable needs the project's name: set `name` in forge.yaml")
		}
		session, err := obtainClaimable(ctx, out, ep, project, envName)
		if err != nil {
			return err
		}
		restore := cloud.UseClaimableCredential(ep, session.cred.Token, session.cred.ExpiresAt)
		defer restore()

		// Everything the ordinary no-version deploy does, as the anonymous user.
		// --yes is implied: there is no human at this keyboard to approve a
		// plan for a throwaway account, and the account is the only thing at stake.
		inner := f
		inner.promote.yes = true
		inner.claim = func() *deployClaimJSON {
			session.refreshURL(ctx)
			return session.document()
		}
		if err := runDeployEverything(ctx, envName, inner); err != nil {
			return claimableDeployError(envName, err)
		}
		if !f.jsonOut {
			printClaimBlock(os.Stdout, envName, session.document())
		}
		return nil
	}()
	if err == nil {
		return nil
	}
	// A refusal raised on this path owes a --json caller its envelope. An error
	// from inside the deploy keeps the deploy's own reporting.
	var r *claimableRefusal
	if f.jsonOut && errors.As(err, &r) && !r.emitted {
		return refuseDeployReported(report, err)
	}
	return err
}

// refuseClaimableWhenSignedIn refuses --claimable when a real credential
// resolves: such a user deploys to their own organization.
func refuseClaimableWhenSignedIn(ep cloud.Endpoint) error {
	cred, err := cloud.ResolveCredential("", ep)
	if err == nil {
		return &claimableRefusal{
			exit: exitWrong, code: cloud.CodeFailedPrecondition, reason: reasonClaimableSignedIn,
			msg: fmt.Sprintf("you are signed in to %s (%s: %s); drop --claimable to deploy to your organization",
				ep.URL, cred.Source, cred.From),
		}
	}
	if errors.Is(err, cloud.ErrNoCredential) {
		return nil
	}
	return err
}

// claimableIneligibility lists what in the render an unclaimed deploy may not
// be (§2: "never"). The control plane enforces the same at admission; checking
// here saves a build and says it in the author's own vocabulary.
func claimableIneligibility(e *KCLEntities) []string {
	var out []string
	var services, jobs int
	for _, w := range e.Workloads {
		if !w.OnRuntime(RuntimeHosted) {
			continue
		}
		switch w.Spec.EffectiveKind() {
		case v1alpha1.KindService:
			services++
		case v1alpha1.KindJob:
			if w.Spec.EffectiveDeployPhase() == v1alpha1.DeployPhasePreRollout {
				jobs++
			} else {
				out = append(out, fmt.Sprintf("job %q is not a pre-rollout migration job", w.Name))
			}
		default:
			out = append(out, fmt.Sprintf("%s %q (only one service may run)", w.Spec.EffectiveKind(), w.Name))
		}
		if replicas := w.Spec.Replicas; replicas > claimableMaxReplicas && w.Spec.EffectiveKind() == v1alpha1.KindService {
			out = append(out, fmt.Sprintf("workload %q asks for %d replicas (%d is allowed)", w.Name, replicas, claimableMaxReplicas))
		}
		if w.Spec.EffectiveKind() == v1alpha1.KindService || w.Spec.EffectiveKind() == v1alpha1.KindJob {
			// The hosted shape band does not apply to this plan; these caps do.
			r := w.Spec.Resources.WithDefaults()
			if cpu := max(r.CPURequestMillicores, r.CPULimitMillicores); cpu > claimableMaxCPUMillicore {
				out = append(out, fmt.Sprintf("workload %q asks for %dm of CPU (at most %dm, no burst)", w.Name, cpu, claimableMaxCPUMillicore))
			}
			if mem := max(r.MemoryRequestBytes, r.MemoryLimitBytes); mem > claimableMaxMemoryMiB<<20 {
				out = append(out, fmt.Sprintf("workload %q asks for %d MiB of memory (at most %d MiB, no burst)", w.Name, mem>>20, claimableMaxMemoryMiB))
			}
		}
		if w.Spec.StorageGiB > 0 {
			out = append(out, fmt.Sprintf("workload %q asks for a %d GiB persistent volume (use the database)", w.Name, w.Spec.StorageGiB))
		}
		if w.Build.Type == "remote" {
			out = append(out, fmt.Sprintf("workload %q uses a remote build (build and push the image from this machine)", w.Name))
		}
		for _, p := range w.Spec.Ports {
			if len(p.Domains) > 0 {
				out = append(out, fmt.Sprintf("workload %q declares custom domains", w.Name))
				break
			}
		}
	}
	if services > claimableMaxServices {
		out = append(out, fmt.Sprintf("%d services (%d is allowed)", services, claimableMaxServices))
	}
	if jobs > 1 || (jobs == 1 && services == 0) {
		out = append(out, "a job that gates no service (only a service's own migration job is allowed)")
	}
	var dbs, sites int
	for _, d := range e.Databases {
		if d.Hosted() {
			dbs++
		}
	}
	for _, fe := range e.Frontends {
		if frontendIsHosted(fe) {
			sites++
		}
	}
	if dbs > claimableMaxDatabases {
		out = append(out, fmt.Sprintf("%d databases (%d is allowed)", dbs, claimableMaxDatabases))
	}
	if sites > claimableMaxStaticSites {
		out = append(out, fmt.Sprintf("%d static sites (%d is allowed)", sites, claimableMaxStaticSites))
	}
	sort.Strings(out)
	return out
}

func claimableIneligibleError(env string, reasons []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "env %q does not fit an anonymous deploy. An unclaimed deploy is one static site, or one service with one database; it cannot have:", env)
	for _, r := range reasons {
		fmt.Fprintf(&b, "\n  - %s", r)
	}
	fmt.Fprintf(&b, "\nsign up for the rest: forge login, then forge env deploy %s", env)
	return &claimableRefusal{exit: exitRefused, code: cloud.CodeFailedPrecondition, reason: reasonClaimableIneligible, msg: b.String()}
}

// obtainClaimable returns this machine's anonymous deploy for (project, env):
// the stored one when the control plane still accepts its token, otherwise a
// fresh one from StartClaimableDeploy.
func obtainClaimable(ctx context.Context, out io.Writer, ep cloud.Endpoint, project, env string) (*claimableSession, error) {
	now := claimableNow()
	if stored, err := cloud.LoadClaimable(ep, project, env, now); err == nil {
		switch err := probeClaimableToken(ctx, ep, stored.Token); {
		case err == nil:
			fmt.Fprintf(out, "Reusing this machine's claimable deploy of %s/%s (goes offline %s)\n",
				project, env, stored.Claimable.ExpiresAt.UTC().Format(time.RFC3339))
			return &claimableSession{ep: ep, project: project, env: env, cred: stored, reused: true}, nil
		case isClaimedError(err):
			return nil, claimableClaimedError(env)
		case isDeadTokenError(err):
			// Expired or unknown to the control plane: the deploy is gone, so
			// mint a new one rather than fail on a token nobody accepts.
			_ = cloud.ForgetClaimable(ep, project, env)
		default:
			return nil, err
		}
	} else if !errors.Is(err, credentials.ErrNotFound) {
		return nil, err
	}

	api := claimableAPI(ep)
	deviceID, err := cloud.ClaimableDeviceID()
	if err != nil {
		return nil, err
	}
	var started cloud.ClaimableDeploy
	for attempt := 0; ; attempt++ {
		ch, err := api.Challenge(ctx)
		if err != nil {
			return nil, claimableStartError(env, err)
		}
		fmt.Fprintf(out, "Requesting an anonymous deploy for %s/%s (solving a %d-bit proof of work)…\n", project, env, ch.DifficultyBits)
		nonce, err := cloud.SolvePoW(ctx, ch)
		if err != nil {
			return nil, err
		}
		started, err = api.Start(ctx, cloud.ClaimableStartRequest{
			Project: project, Env: env, DeviceID: deviceID, PoWChallenge: ch.Challenge, PoWNonce: nonce,
		})
		if err == nil {
			break
		}
		var ce *cloud.Error
		// A challenge that expired while we solved it is refused as a bad
		// proof of work. A fresh one is the right response, once.
		if attempt == 0 && errors.As(err, &ce) && ce.HasReason(cloud.ReasonClaimableInvalidPoW) {
			continue
		}
		return nil, claimableStartError(env, err)
	}
	cred, err := cloud.StoreClaimable(ep, project, env, started, now)
	if err != nil {
		return nil, fmt.Errorf("the anonymous deploy was created but its token could not be saved (%w); "+
			"claim link: %s", err, started.ClaimURL)
	}
	return &claimableSession{ep: ep, project: project, env: env, cred: cred, reused: started.Reused}, nil
}

// probeClaimableToken asks the control plane one cheap question with the
// stored token: is it still accepted?
func probeClaimableToken(ctx context.Context, ep cloud.Endpoint, token string) error {
	client := hostedDeployClient(ep, cloud.Credential{Token: token, Source: cloud.SourceClaimable, From: "stored claimable deploy"})
	_, err := cloud.ResolveOrganization(ctx, client)
	return err
}

func isClaimedError(err error) bool {
	var ce *cloud.Error
	return errors.As(err, &ce) && (ce.HasReason(cloud.ReasonClaimableClaimed) || ce.HasReason(cloud.ReasonAccessTokenRevoked))
}

func isDeadTokenError(err error) bool {
	var ce *cloud.Error
	return errors.As(err, &ce) && (ce.HasReason(cloud.ReasonAccessTokenExpired) || ce.HasReason(cloud.ReasonAccessTokenUnknown) ||
		ce.HasReason(cloud.ReasonClaimableExpired))
}

func claimableClaimedError(env string) error {
	return &claimableRefusal{
		exit: exitRefused, code: cloud.CodeFailedPrecondition, reason: cloud.ReasonClaimableClaimed,
		msg: fmt.Sprintf("this deploy was claimed: someone opened its claim link and it now belongs to their account.\n"+
			"  run forge login and deploy without --claimable: forge login, then forge env deploy %s", env),
	}
}

// claimableDeployError translates what the ordinary deploy raised into this
// path's vocabulary: a claimed deploy, and a queued one (which an unclaimed org
// must never be: nobody could resolve the hold).
func claimableDeployError(env string, err error) error {
	if isClaimedError(err) {
		return claimableClaimedError(env)
	}
	if q := queuedOf(err); q != nil {
		return &claimableRefusal{
			exit: exitRefused, code: cloud.CodeFailedPrecondition, reason: reasonClaimableIneligible, emitted: true,
			msg: fmt.Sprintf("the control plane held this anonymous deploy on %s, which an unclaimed deploy cannot resolve.\n"+
				"sign up for the rest: forge login, then forge env deploy %s", queuedLabel(q.Holds), env),
		}
	}
	return err
}

// claimableStartError renders a refusal of StartClaimableDeploy.
func claimableStartError(env string, err error) error {
	if th, ok := cloud.ClaimableThrottleOf(err); ok {
		return newClaimableThrottle(env, th)
	}
	var ce *cloud.Error
	if errors.As(err, &ce) {
		switch {
		case ce.HasReason(cloud.ReasonClaimableUnavailable):
			return &claimableRefusal{
				exit: exitRefused, code: ce.Code, reason: cloud.ReasonClaimableUnavailable,
				msg: fmt.Sprintf("Claimable deploys are not available right now. Nothing was created.\n"+
					"  instead  forge login, then forge env deploy %s (your own free account)", env),
			}
		case ce.HasReason(cloud.ReasonClaimableTakenDown), ce.HasReason(cloud.ReasonClaimableExpired), ce.HasReason(cloud.ReasonClaimableClaimed):
			return &claimableRefusal{exit: exitRefused, code: ce.Code, reason: ce.Reason, msg: fmt.Sprintf("%s\n  (reason: %s)", ce.Message, ce.Reason)}
		case ce.HasCode(cloud.CodeInvalidArgument), ce.HasCode(cloud.CodeFailedPrecondition):
			if ce.Reason != "" {
				return &claimableRefusal{exit: exitRefused, code: ce.Code, reason: ce.Reason, msg: fmt.Sprintf("%s\n  (reason: %s)", ce.Message, ce.Reason)}
			}
		}
	}
	return err
}

func newClaimableThrottle(env string, th cloud.ClaimableThrottle) error {
	at := claimableNow().Add(th.RetryAfter).UTC()
	facts := &claimableThrottleFacts{scope: th.Scope, retryAfter: th.RetryAfter, retryAt: at}
	var why string
	switch th.Scope {
	case "platform":
		why = "too many new anonymous accounts\n  (platform-wide limit)"
	case "network":
		why = "too many claimable deploys from your network"
	case "device":
		why = "this machine already has 3 live claimable deploys"
	default:
		why = "too many claimable deploys right now"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Claimable deploys are throttled right now: %s. Nothing was created.\n", why)
	if th.RetryAfter > 0 {
		fmt.Fprintf(&b, "  retry after  %s  (at %s)\n", formatRetryAfter(th.RetryAfter), at.Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "  then         %s env deploy %s --claimable\n", Name(), env)
	fmt.Fprintf(&b, "  or now       forge login, then %s env deploy %s (your own free account; this limit does not apply)\n", Name(), env)
	b.WriteString("  (exit 4: throttled, not a failure of your project)")
	return &claimableRefusal{
		exit: exitRefused, code: cloud.CodeResourceExhausted, reason: cloud.ReasonClaimableThrottled,
		msg: b.String(), throttle: facts,
	}
}

// formatRetryAfter renders a wait the way an agent can act on: "45s", "14m",
// "1h5m". Minutes round UP, so the agent never comes back too early.
func formatRetryAfter(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Round(time.Second)/time.Second))
	}
	mins := int((d + time.Minute - 1) / time.Minute)
	if mins < 60 {
		return fmt.Sprintf("%dm", mins)
	}
	if mins%60 == 0 {
		return fmt.Sprintf("%dh", mins/60)
	}
	return fmt.Sprintf("%dh%dm", mins/60, mins%60)
}

// ─── Output ──────────────────────────────────────────────────────────────────

// refreshURL reads the deploy's live address from the control plane and keeps
// it beside the token, so `env status --claimable` can print it later with no
// network.
func (s *claimableSession) refreshURL(ctx context.Context) {
	client := hostedDeployClient(s.ep, cloud.Credential{Token: s.cred.Token, Source: cloud.SourceClaimable, From: "claimable deploy"})
	st, err := deploytarget.ReadHostedStatus(ctx, client, s.project, s.env)
	if err != nil {
		return
	}
	if url := primaryURL(st.Workloads); url != "" {
		s.cred.Claimable.URL = url
		_ = cloud.RecordClaimableURL(s.ep, s.project, s.env, url)
	}
}

// primaryURL is the address to show first: the static site's when there is one,
// else the first workload that has one.
func primaryURL(ws []deploytarget.HostedWorkloadStatus) string {
	first := ""
	for _, w := range ws {
		if w.URL == "" {
			continue
		}
		if w.Tier == "static" {
			return w.URL
		}
		if first == "" {
			first = w.URL
		}
	}
	return first
}

// document is the session as the --json `claim` object.
func (s *claimableSession) document() *deployClaimJSON {
	return claimDocument(s.env, s.cred, claimableNow())
}

func claimDocument(env string, c credentials.Credential, now time.Time) *deployClaimJSON {
	cl := c.Claimable
	state := "live"
	switch {
	case !cl.PurgeAfter.IsZero() && !now.Before(cl.PurgeAfter):
		state = "expired"
	case !cl.ExpiresAt.IsZero() && !now.Before(cl.ExpiresAt):
		state = "suspended"
	}
	return &deployClaimJSON{
		State: state, URL: cl.URL, ClaimURL: cl.ClaimURL,
		ExpiresAt: cl.ExpiresAt, PurgeAfter: cl.PurgeAfter,
		AgentInstructions: fmt.Sprintf("Give claim_url to your user and tell them it expires at %s. "+
			"Opening it signs them up for free and makes this deploy theirs. "+
			"Treat the link like a password: whoever claims it first owns the deploy. Do not post it anywhere public. "+
			"To see it again: %s env status %s --claimable",
			cl.ExpiresAt.UTC().Format(time.RFC3339), Name(), env),
	}
}

// printClaimBlock renders the claim for a person or an agent reading text. The
// wording is §6 of the design; the agent line is the point of the whole block.
func printClaimBlock(w io.Writer, env string, c *deployClaimJSON) {
	now := claimableNow()
	if c.State == "live" || c.State == "suspended" {
		if c.URL != "" {
			fmt.Fprintf(w, "\n✓ Live:   %s\n", c.URL)
		}
	}
	left := c.ExpiresAt.Sub(now)
	fmt.Fprintf(w, "\n  This deploy belongs to no account. It goes offline %s (in %s)\n",
		c.ExpiresAt.UTC().Format("2006-01-02 15:04 MST"), formatLifetime(left))
	fmt.Fprintf(w, "  and is deleted %s after that, unless someone claims it.\n", formatLifetime(c.PurgeAfter.Sub(c.ExpiresAt)))
	fmt.Fprintf(w, "\n  Claim:  %s\n", c.ClaimURL)
	fmt.Fprintf(w, "\n  FOR AN AGENT: give the claim link above to your user and tell them it expires in %s.\n", formatLifetime(left))
	fmt.Fprintf(w, "  Opening it signs them up for free and makes this site theirs.\n")
	fmt.Fprintf(w, "  Treat the link like a password: whoever claims it first owns the site. Do not\n")
	fmt.Fprintf(w, "  post it anywhere public. To see it again: %s env status %s --claimable\n", Name(), env)
}

// formatLifetime renders a span as hours (72h, 41h) up to three days, days
// beyond, minutes under an hour.
func formatLifetime(d time.Duration) string {
	if d <= 0 {
		return "0m"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int((d+time.Minute-1)/time.Minute))
	}
	hours := int((d + time.Hour - 1) / time.Hour)
	if hours > 72 && hours%24 == 0 {
		return fmt.Sprintf("%d days", hours/24)
	}
	return fmt.Sprintf("%dh", hours)
}

// runClaimableStatus is `forge env status <env> --claimable`: the claim link
// again, from the stored entry, with no credential.
func runClaimableStatus(ctx context.Context, env string, jsonOut bool) error {
	entities, err := RenderKCL(ctx, projectDirForKCL(), env)
	if err != nil {
		return err
	}
	if entities == nil || entities.ControlPlane == nil {
		return fmt.Errorf("env %q declares no hosted control plane, so it has no claimable deploy", env)
	}
	ep, err := projectCloudEndpoint(env, declarationFromEntities(entities))
	if err != nil {
		return err
	}
	project := hostedProjectName()
	now := claimableNow()
	stored, err := cloud.LoadClaimable(ep, project, env, now)
	if err != nil {
		if errors.Is(err, credentials.ErrNotFound) {
			return fmt.Errorf("this machine has no claimable deploy of %s/%s at %s (none made, or it was deleted).\n"+
				"fix: %s env deploy %s --claimable", project, env, ep.URL, Name(), env)
		}
		return err
	}
	doc := claimDocument(env, stored, now)
	if perr := probeClaimableToken(ctx, ep, stored.Token); isClaimedError(perr) {
		doc.State = "claimed"
	}
	if jsonOut {
		return emitJSONDocument(struct {
			jsonEnvelope
			Env   string           `json:"env"`
			Claim *deployClaimJSON `json:"claim"`
		}{Env: env, Claim: doc})
	}
	if doc.State == "claimed" {
		fmt.Printf("This deploy was claimed: it belongs to someone's account now.\n"+
			"  run forge login and deploy without --claimable: forge login, then %s env deploy %s\n", Name(), env)
		return nil
	}
	if doc.State == "expired" {
		fmt.Printf("This claimable deploy was deleted on %s.\n  deploy again: %s env deploy %s --claimable\n",
			doc.PurgeAfter.UTC().Format(time.RFC3339), Name(), env)
		return nil
	}
	if doc.State == "suspended" {
		fmt.Printf("\nThis deploy is OFFLINE since %s and is deleted on %s unless someone claims it.\n",
			doc.ExpiresAt.UTC().Format(time.RFC3339), doc.PurgeAfter.UTC().Format(time.RFC3339))
		fmt.Printf("\n  Claim:  %s\n\n  FOR AN AGENT: give the claim link above to your user.\n", doc.ClaimURL)
		return nil
	}
	printClaimBlock(os.Stdout, env, doc)
	return nil
}
