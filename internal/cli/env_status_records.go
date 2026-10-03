package cli

// The RECORDS half of `forge env status <env>` (doc §6.3, §7.4, §13): where
// what is bound came FROM, whether the last apply of it finished, and which
// local stacks are running it.
//
// WHY THIS BELONGS IN env status AND NOT A NEW VERB. The command already
// answers "is this env running what it declares" by comparing a binding
// against a cluster. These three are the facts a reader needs the moment that
// answer is interesting: a DRIFT whose cause is an apply that never finished
// is a different problem from one whose cause is a bad release, and a reader
// who has to run a second command to tell them apart will not.
//
// IT NEVER CHANGES THE VERDICT, AND THAT IS LOAD-BEARING. Every exit code
// this command returns is computed by envStatusReleaseVerdict from the
// binding and the cluster, exactly as before. Records are DESCRIPTION: an
// abandoned apply is not an error here (the cluster comparison already has an
// opinion about whether the bytes landed), an unreachable records backend is
// not an error (it says so and the verdict stands), and an env with no
// records at all prints an empty state and exits 0. A records read that could
// fail the command would make `forge env status` depend on a control plane
// being reachable to answer a question about a local cluster.
//
// EMPTY IS NOT UNKNOWN, and the two are rendered differently throughout. "no
// apply recorded" is an answer; "could not read the records" is not. The
// §7.4 rule that a surface must distinguish "no sessions" from "cannot see
// sessions" is the same rule — a view that collapses them tells a user their
// stack is down when really their ledger could not be opened.

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/reliant-labs/forge/pkg/release"
)

// envStatusRecords is the additive section of the status document.
//
// Nil when the records were not read at all. Every field inside is
// independently absent-able, because the four facts come from four reads and
// one failing says nothing about the others.
type envStatusRecords struct {
	// Source is which backend answered: "machine ledger" or "control
	// plane". A reader comparing two machines' views needs it, and "the
	// records were empty" is a different diagnosis from "I read a
	// different store than I meant to".
	Source string `json:"source"`
	// Location is the store's address — a directory or an endpoint.
	Location string `json:"location,omitempty"`
	// Provenance is where the BOUND release's content came from, and how
	// the binding was made. Nil when the env is unbound or the release
	// record predates provenance.
	Provenance *envStatusProvenance `json:"provenance,omitempty"`
	// Apply is the LATEST apply of this env, with its derived state. Nil
	// means none was ever recorded — which is normal for an env whose
	// deploys predate apply records, and is rendered as "no apply
	// recorded", never as a failure.
	Apply *envStatusApply `json:"apply,omitempty"`
	// Sessions are the local stacks. Always non-nil when SessionsRead is
	// true, so a consumer sees `[]` rather than `null` for "none
	// running".
	Sessions []envStatusSession `json:"sessions"`
	// SessionsRead distinguishes "no sessions" from "sessions were not
	// read" — the §7.4 rule, as a field rather than as a convention a
	// consumer has to remember. False for a non-local env, which has no
	// local presence to report.
	SessionsRead bool `json:"sessions_read"`
	// SessionsDetail says WHY when SessionsRead is false.
	SessionsDetail string `json:"sessions_detail,omitempty"`
	// Detail carries a read failure. The verdict is unaffected: see the
	// file header.
	Detail string `json:"detail,omitempty"`
}

// envStatusProvenance is the bound release's source, plus the binding's own
// story.
//
// THE TWO HALVES ARE DIFFERENT KINDS OF CLAIM and the field names keep them
// apart. Commit and Tree are checkable against the repository (a clean tree's
// hash IS the commit's tree). Branch and Dirty are what forge REPORTED at
// build time — evidence, never authority — which is why the renderer labels
// them as reported rather than stating them flatly.
type envStatusProvenance struct {
	Release string `json:"release"`
	Repo    string `json:"repo,omitempty"`
	Commit  string `json:"commit,omitempty"`
	Branch  string `json:"branch,omitempty"`
	Tag     string `json:"tag,omitempty"`
	Tree    string `json:"tree,omitempty"`
	Dirty   bool   `json:"dirty"`
	// ForgeVersion is the binary that rendered it. A render is a function
	// of (forge, KCL, config), so the same commit rendered by two forges
	// is two different artifacts.
	ForgeVersion string `json:"forge_version,omitempty"`
	// BoundBy is HOW the current binding was made: the promotion kind, the
	// env it came from, who made it, and the plan that was approved. This
	// is the half a reader asking "why is this version here" wants, and it
	// is a property of the PROMOTION rather than of the release.
	BoundBy envStatusBinding `json:"bound_by"`
}

// envStatusBinding is the promotion that bound the env.
type envStatusBinding struct {
	PromotionID string `json:"promotion_id,omitempty"`
	Kind        string `json:"kind,omitempty"`
	// FromEnv is the env this release was promoted FROM, empty for a
	// first deploy or a direct promote. It makes the path auditable.
	FromEnv string `json:"from_env,omitempty"`
	// PromotedBy is who, as the backend recorded it. A hosted backend
	// sets it from the credential; a file ledger records what it was told.
	PromotedBy string `json:"promoted_by,omitempty"`
	PromotedAt string `json:"promoted_at,omitempty"`
	// PlanDigest and ApprovedBy name the review this binding was written
	// under (O-13). Empty on a binding made before plans, or by a path
	// that needs no approval.
	PlanDigest string `json:"plan_digest,omitempty"`
	ApprovedBy string `json:"approved_by,omitempty"`
	// SupersededInFlight records that this promotion replaced one whose
	// rollout had not finished — a deliberate override, and the audit
	// trail for "who decided to interrupt a rollout".
	SupersededInFlight bool   `json:"superseded_in_flight,omitempty"`
	Note               string `json:"note,omitempty"`
}

// envStatusApply is the latest apply with its DERIVED state.
//
// The state is derived here through release.DeriveApplyState rather than left
// to the consumer, for the reason ledger_show.go gives: that is the one
// implementation of the rule, and a UI that re-derived "is this abandoned"
// from a deadline and a clock would eventually disagree with what forge
// prints in a terminal.
type envStatusApply struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	BundleID string `json:"bundle_id,omitempty"`
	// BundleDigest is the config artifact's identity — the bundle IS its
	// content, so this is the only field that says which config landed.
	BundleDigest string `json:"bundle_digest,omitempty"`
	PromotionID  string `json:"promotion_id,omitempty"`
	// AppliedBy and ReportedBy are the two identities, and they are NOT
	// the same claim. AppliedBy is what the applier said about itself;
	// ReportedBy is set by the backend from the credential on a hosted
	// env, which makes it the only one of the two a reader may trust.
	AppliedBy  string `json:"applied_by,omitempty"`
	ReportedBy string `json:"reported_by,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	// DeadlineAt is what "abandoned" is measured against, carried so a
	// reader can see how long ago the answer stopped being expected.
	DeadlineAt string `json:"deadline_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

// envStatusSession is one presence row with the verdicts a renderer needs.
type envStatusSession struct {
	Worktree string `json:"worktree"`
	Host     string `json:"host,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Commit   string `json:"commit,omitempty"`
	Dirty    bool   `json:"dirty"`
	// State is running | quiet | stopped. "quiet" is a LIVE session that
	// has not been seen for release.SessionStaleAfter — deliberately not
	// called stale or dead: forge cannot tell a crashed stack from a
	// laptop that closed its lid, and naming it after the observation
	// rather than a guessed cause is the honest rendering of both.
	State      string `json:"state"`
	LastSeenAt string `json:"last_seen_at,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
}

// Session states as this view names them.
const (
	sessionStateRunning = "running"
	sessionStateQuiet   = "quiet"
	sessionStateStopped = "stopped"
)

// envRecordsReader reads one env's records.
//
// A seam, for the reason every other seam in this command exists: resolving
// it for real renders KCL, opens a ledger home and may call a control plane,
// and a test of the RENDERING should be able to state the records directly.
// The production command injects the real reader rather than leaving this nil
// and branching on it, so both paths run the same code.
type envRecordsReader func(ctx context.Context, projectDir, env string, now time.Time) (envStatusRecords, error)

// readEnvRecords is the production reader: the records of whichever store the
// env's declaration selects.
//
// It goes through sessionTargetFor, which goes through ledgerForEntities — so
// the store these records are READ from is the store a session is WRITTEN to
// and the store a promotion is recorded in. One declaration, one answer.
func readEnvRecords(ctx context.Context, projectDir, env string, now time.Time) (envStatusRecords, error) {
	entities, err := RenderKCL(ctx, projectDir, env)
	if err != nil {
		// Not fatal, and not silent. An env named on the command line
		// but absent from this checkout is a real case (a release cut
		// on a branch that declares it, read from one that does not),
		// and the records that exist for it are still worth naming as
		// unreadable rather than as empty.
		return envStatusRecords{}, fmt.Errorf("render deploy/kcl/%s to find this env's records: %w", env, err)
	}
	target, err := sessionTargetForEntities(env, entities, projectDir)
	if err != nil {
		return envStatusRecords{}, err
	}
	if target.Hosted {
		return readHostedEnvRecords(ctx, env, entities, now)
	}
	return readMachineEnvRecords(env, projectDir, target, now)
}

// readMachineEnvRecords reads the file ledger.
//
// It reuses F7's ledgerShow rather than walking the store again. That reader
// already joins each apply to its outcome, derives the state from ONE clock
// reading, and applies the live/quiet verdicts to sessions — so `forge ledger
// show` and `forge env status` cannot come to disagree about whether the same
// apply is abandoned.
func readMachineEnvRecords(env, projectDir string, target sessionTarget, now time.Time) (envStatusRecords, error) {
	store, err := openMachineLedger(projectDir)
	if err != nil {
		return envStatusRecords{}, err
	}
	out := envStatusRecords{
		Source:         "machine ledger",
		Location:       store.Dir(),
		Sessions:       []envStatusSession{},
		SessionsRead:   target.Report,
		SessionsDetail: target.Skip,
	}
	doc, err := ledgerShow(store, hostedProjectName(), env, now)
	if err != nil {
		return out, err
	}
	for _, shown := range doc.Environments {
		if shown.Name != env {
			continue
		}
		if shown.Current != nil {
			// The release record is read separately because the
			// promotion names a VERSION and the provenance lives on
			// the release. A missing release record is not an
			// error: a promotion can name a version imported from
			// elsewhere, and "bound to v1.2.0, provenance unknown"
			// is a better answer than no binding information at
			// all.
			rel, _ := store.Release(shown.Current.Release)
			out.Provenance = provenanceOf(*shown.Current, rel)
		}
		if len(shown.Applies) > 0 {
			latest := shown.Applies[0] // ledgerShow orders newest first
			out.Apply = applyOf(latest.Apply, latest.Outcome, latest.State)
		}
	}
	if target.Report {
		for _, s := range doc.Sessions {
			out.Sessions = append(out.Sessions, sessionOf(s.Session, now))
		}
	}
	return out, nil
}

// readHostedEnvRecords reads the control plane's Live view.
//
// ONE RPC, not four. GetLiveView already returns the current promotion, the
// current release, the current bundle, the latest apply and the sessions for
// every env of a project — assembled server-side against one snapshot. Four
// per-env calls would be four round trips that could each see a different
// instant, which is how a view ends up showing an apply of a bundle it also
// says is not current.
func readHostedEnvRecords(ctx context.Context, env string, entities *KCLEntities, now time.Time) (envStatusRecords, error) {
	ledger, err := ledgerForEntities(env, entities, projectDirForKCL())
	if err != nil {
		return envStatusRecords{}, err
	}
	hosted, ok := ledger.Bindings.(*hostedStore)
	if !ok {
		return envStatusRecords{}, fmt.Errorf("env %q selected a hosted ledger whose store is %T", env, ledger.Bindings)
	}
	out := envStatusRecords{
		Source:   "control plane",
		Location: hosted.endpoint,
		Sessions: []envStatusSession{},
	}
	rows, err := hostedRecordStoreFor(hosted.client, hosted.project).Live().GetLiveView(ctx, hosted.project, false, now)
	if err != nil {
		return out, err
	}
	for _, row := range rows {
		if row.Env.Name != env {
			continue
		}
		if row.CurrentPromotion != nil {
			out.Provenance = provenanceOf(*row.CurrentPromotion, row.CurrentRelease)
		}
		if row.LatestApply != nil {
			out.Apply = applyOf(row.LatestApply.Apply, row.LatestApply.Outcome, row.LatestApply.State)
			if row.CurrentBundle != nil && row.CurrentBundle.ID == row.LatestApply.Apply.BundleID {
				// Only when the ids MATCH. The latest apply may
				// be of a bundle that is not the current one —
				// that is the whole point of carrying both —
				// and pasting the current bundle's digest onto
				// a different apply would state that the
				// running config is one forge never said had
				// landed.
				out.Apply.BundleDigest = row.CurrentBundle.Digest
			}
		}
		// Sessions come back only for a LOCAL env; the server's trigger
		// confines them. So the presence of the row IS the answer to
		// "were sessions read", and a non-local env reports none
		// without this client having to classify it a second time.
		out.SessionsRead = true
		for _, s := range row.Sessions {
			out.Sessions = append(out.Sessions, sessionOf(s, now))
		}
	}
	return out, nil
}

// provenanceOf projects a promotion and its release onto the view.
func provenanceOf(p release.Promotion, rel *release.Release) *envStatusProvenance {
	out := &envStatusProvenance{
		Release: p.Release,
		BoundBy: envStatusBinding{
			PromotionID:        p.ID,
			Kind:               string(p.Kind),
			FromEnv:            p.FromEnv,
			PromotedBy:         promoterName(p.PromotedBy),
			PlanDigest:         p.PlanDigest,
			ApprovedBy:         p.ApprovedBy,
			SupersededInFlight: p.SupersededInFlight,
			Note:               p.Note,
		},
	}
	if !p.PromotedAt.IsZero() {
		out.BoundBy.PromotedAt = p.PromotedAt.UTC().Format(time.RFC3339)
	}
	if rel != nil && rel.Provenance != nil {
		prov := rel.Provenance
		out.Repo, out.Commit, out.Branch = prov.Repo, prov.Commit, prov.Branch
		out.Tag, out.Tree, out.Dirty = prov.Tag, prov.Tree, prov.Dirty
		out.ForgeVersion = prov.ForgeVersion
	}
	return out
}

// promoterName is actorLabel for a JSON field.
//
// actorLabel (env_history.go) renders "-" for an unknown actor, which is
// right for a fixed-width table column and wrong here: a JSON consumer
// reading `promoted_by: "-"` has to know that one renderer's placeholder, and
// the field is already omitempty for exactly this case. So the placeholder is
// dropped rather than a second actor renderer written.
func promoterName(a release.Actor) string {
	if name := actorLabel(a); name != "-" {
		return name
	}
	return ""
}

// applyOf projects an apply, its outcome and its derived state onto the view.
func applyOf(a release.Apply, outcome *release.ApplyOutcome, state release.ApplyState) *envStatusApply {
	out := &envStatusApply{
		ID:          a.ID,
		State:       string(state),
		BundleID:    a.BundleID,
		PromotionID: a.PromotionID,
		AppliedBy:   a.AppliedBy,
	}
	if !a.CreatedAt.IsZero() {
		out.StartedAt = a.CreatedAt.UTC().Format(time.RFC3339)
	}
	if !a.DeadlineAt.IsZero() {
		out.DeadlineAt = a.DeadlineAt.UTC().Format(time.RFC3339)
	}
	if outcome != nil {
		out.ReportedBy, out.Summary = outcome.ReportedBy, outcome.Summary
		if !outcome.FinishedAt.IsZero() {
			out.FinishedAt = outcome.FinishedAt.UTC().Format(time.RFC3339)
		}
	}
	return out
}

// sessionOf projects a presence row onto the view.
func sessionOf(s release.LocalSession, now time.Time) envStatusSession {
	out := envStatusSession{
		Worktree: s.Worktree.Label,
		Host:     s.Worktree.Host,
		Branch:   s.Provenance.Branch,
		Commit:   s.Provenance.Commit,
		Dirty:    s.Provenance.Dirty,
		State:    sessionStateStopped,
	}
	if out.Worktree == "" {
		out.Worktree = "(primary checkout)"
	}
	switch {
	case s.Stale(now):
		out.State = sessionStateQuiet
	case s.Live():
		out.State = sessionStateRunning
	}
	if !s.LastSeenAt.IsZero() {
		out.LastSeenAt = s.LastSeenAt.UTC().Format(time.RFC3339)
	}
	if !s.StartedAt.IsZero() {
		out.StartedAt = s.StartedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// collectEnvRecords runs the reader and folds a failure into the document
// rather than returning it.
//
// This is where "records never change the verdict" is implemented. The error
// becomes Detail and the caller gets a document either way, so there is no
// path by which a records read reaches runEnvStatusRelease's return value.
func collectEnvRecords(ctx context.Context, read envRecordsReader, projectDir, env string, now time.Time) envStatusRecords {
	if read == nil {
		read = readEnvRecords
	}
	records, err := read(ctx, projectDir, env, now)
	if err != nil {
		if records.Source == "" {
			records.Source = "unknown"
		}
		if records.Sessions == nil {
			records.Sessions = []envStatusSession{}
		}
		records.Detail = err.Error()
	}
	return records
}

// writeEnvStatusRecords renders the text form.
//
// Printed as its own block after the per-image report, because it answers a
// different question and a reader follows them in order: what is wrong, then
// where it came from and whether it finished landing.
func writeEnvStatusRecords(w io.Writer, r envStatusRecords) {
	fmt.Fprintf(w, "\nRecords (%s", r.Source)
	if r.Location != "" {
		fmt.Fprintf(w, " %s", r.Location)
	}
	fmt.Fprintf(w, ")\n")
	if r.Detail != "" {
		// Stated as a read failure, never as an empty result — the
		// empty-versus-unknown rule from the file header. The reader
		// must not conclude "nothing has been deployed" from "I could
		// not look".
		fmt.Fprintf(w, "  could not read the records: %s\n", r.Detail)
		return
	}

	switch p := r.Provenance; {
	case p == nil:
		fmt.Fprintf(w, "  provenance    not recorded for this binding\n")
	default:
		fmt.Fprintf(w, "  provenance    %s\n", provenanceLine(*p))
		fmt.Fprintf(w, "  bound by      %s\n", boundByLine(p.BoundBy))
	}

	if r.Apply == nil {
		fmt.Fprintf(w, "  apply         no apply recorded\n")
	} else {
		a := *r.Apply
		fmt.Fprintf(w, "  apply         %s %s\n", strings.ToUpper(a.State), applyWhen(a))
		if a.BundleDigest != "" {
			fmt.Fprintf(w, "                bundle %s (%s)\n", a.BundleID, a.BundleDigest)
		} else if a.BundleID != "" {
			fmt.Fprintf(w, "                bundle %s\n", a.BundleID)
		}
		if who := applyWho(a); who != "" {
			fmt.Fprintf(w, "                %s\n", who)
		}
		if a.State == string(release.ApplyAbandoned) {
			// Named rather than left to inference. An abandoned
			// apply is the single most misread state here: it is
			// not a failure report, it is the ABSENCE of one past
			// the deadline, and the bytes may well have landed.
			fmt.Fprintf(w, "                no outcome arrived by %s — whether it landed is unknown from the records alone\n", a.DeadlineAt)
		}
		if a.Summary != "" {
			fmt.Fprintf(w, "                %s\n", a.Summary)
		}
	}

	switch {
	case !r.SessionsRead:
		fmt.Fprintf(w, "  sessions      not reported for this environment")
		if r.SessionsDetail != "" {
			fmt.Fprintf(w, " — %s", r.SessionsDetail)
		}
		fmt.Fprintln(w)
	case len(r.Sessions) == 0:
		fmt.Fprintf(w, "  sessions      no local stacks running\n")
	default:
		fmt.Fprintf(w, "  sessions      %d local stack(s)\n", len(r.Sessions))
		for _, s := range r.Sessions {
			fmt.Fprintf(w, "                %-10s %s\n", s.State, sessionLine(s))
		}
	}
}

// provenanceLine is "v1.2.0 from main@abc1234 (dirty)".
func provenanceLine(p envStatusProvenance) string {
	parts := []string{p.Release}
	switch {
	case p.Branch != "" && p.Commit != "":
		parts = append(parts, "from "+p.Branch+"@"+shortID(p.Commit))
	case p.Commit != "":
		parts = append(parts, "from "+shortID(p.Commit))
	}
	if p.Tag != "" {
		parts = append(parts, "tag "+p.Tag)
	}
	if p.Dirty {
		// Flagged as REPORTED because it is a claim, not a checkable
		// fact: a dirty tree's hash cannot be verified against the
		// repository, so the renderer must not state it as established.
		parts = append(parts, "dirty tree (reported by forge at build time)")
	}
	if p.ForgeVersion != "" {
		parts = append(parts, "forge "+p.ForgeVersion)
	}
	return strings.Join(parts, ", ")
}

// boundByLine is how the binding was made.
func boundByLine(b envStatusBinding) string {
	parts := []string{}
	if b.Kind != "" {
		parts = append(parts, b.Kind)
	}
	if b.FromEnv != "" {
		parts = append(parts, "from "+b.FromEnv)
	}
	if b.PromotedBy != "" {
		parts = append(parts, "by "+b.PromotedBy)
	}
	if b.PromotedAt != "" {
		parts = append(parts, "at "+b.PromotedAt)
	}
	if b.ApprovedBy != "" {
		parts = append(parts, "approved by "+b.ApprovedBy)
	}
	if b.SupersededInFlight {
		parts = append(parts, "superseded a rollout in flight")
	}
	if b.Note != "" {
		parts = append(parts, "note: "+b.Note)
	}
	if len(parts) == 0 {
		return "(no promotion detail recorded)"
	}
	return strings.Join(parts, ", ")
}

func applyWhen(a envStatusApply) string {
	if a.FinishedAt != "" {
		return "at " + a.FinishedAt
	}
	if a.StartedAt != "" {
		return "started " + a.StartedAt
	}
	return ""
}

// applyWho renders the two identities, and says which one is authority.
func applyWho(a envStatusApply) string {
	switch {
	case a.ReportedBy != "":
		// The backend set this from the credential, so it is the one a
		// reader may trust. Said explicitly, because the other field
		// looks identical and is not.
		return "reported by " + a.ReportedBy + " (server-set identity)"
	case a.AppliedBy != "":
		return "applied by " + a.AppliedBy + " (as the applier reported itself)"
	}
	return ""
}

func sessionLine(s envStatusSession) string {
	parts := []string{s.Worktree}
	if s.Branch != "" {
		label := s.Branch
		if s.Commit != "" {
			label += "@" + shortID(s.Commit)
		}
		parts = append(parts, label)
	}
	if s.Dirty {
		parts = append(parts, "dirty")
	}
	if s.State != sessionStateStopped && s.LastSeenAt != "" {
		parts = append(parts, "last seen "+s.LastSeenAt)
	}
	return strings.Join(parts, ", ")
}

// shortID abbreviates a git object id for display. The full value is always
// in --json, so the text form may shorten; a reader copying a commit out of a
// terminal wants seven characters, and a machine never reads this path.
func shortID(id string) string {
	if len(id) > 7 {
		return id[:7]
	}
	return id
}
