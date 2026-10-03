package cli

// The records half of `forge env status`: what it renders, and — more
// importantly — what it must NEVER do to the verdict.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/release"
)

func recordsNow() time.Time { return time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC) }

// TestCollectEnvRecordsFoldsAReadFailureIntoTheDocument is the verdict
// firewall.
//
// MUTATION: have collectEnvRecords return the error, and every caller in
// env_status_release.go would have to propagate it — which is exactly the
// shape that makes `forge env status` of a local cluster fail because a
// control plane was unreachable.
func TestCollectEnvRecordsFoldsAReadFailureIntoTheDocument(t *testing.T) {
	read := func(context.Context, string, string, time.Time) (envStatusRecords, error) {
		return envStatusRecords{}, errors.New("control plane unreachable: dial tcp: connection refused")
	}
	records := collectEnvRecords(context.Background(), read, t.TempDir(), "dev", recordsNow())

	if records.Detail == "" {
		t.Fatal("a read failure left no Detail: the reader would render it as an empty result")
	}
	if records.Sessions == nil {
		t.Fatal("Sessions is nil after a failure; --json must carry [] rather than null")
	}
	// And the text form must say it could not LOOK, not that there is
	// nothing there.
	var out strings.Builder
	writeEnvStatusRecords(&out, records)
	if !strings.Contains(out.String(), "could not read the records") {
		t.Fatalf("the failure does not read as a failure:\n%s", out.String())
	}
	if strings.Contains(out.String(), "no apply recorded") {
		t.Fatalf("a read failure rendered as an empty result — the one thing it must never do:\n%s", out.String())
	}
}

// TestWriteEnvStatusRecordsEmptyStateIsPlain pins the empty state: an env
// with no records is an ANSWER, printed plainly, never an error.
func TestWriteEnvStatusRecordsEmptyStateIsPlain(t *testing.T) {
	var out strings.Builder
	writeEnvStatusRecords(&out, envStatusRecords{
		Source:       "machine ledger",
		Location:     "/tmp/ledger",
		Sessions:     []envStatusSession{},
		SessionsRead: true,
	})
	got := out.String()
	for _, want := range []string{
		"no apply recorded",
		"provenance    not recorded",
		"no local stacks running",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("empty state is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(strings.ToLower(got), "error") {
		t.Errorf("the empty state reads as an error:\n%s", got)
	}
}

// TestWriteEnvStatusRecordsDistinguishesNotReadFromNone is §7.4's rule: "no
// sessions" and "sessions do not apply here" are different facts, and a
// surface that collapses them tells a user their stack is down when really
// the env never had one.
func TestWriteEnvStatusRecordsDistinguishesNotReadFromNone(t *testing.T) {
	var none, notRead strings.Builder
	writeEnvStatusRecords(&none, envStatusRecords{Source: "machine ledger", Sessions: []envStatusSession{}, SessionsRead: true})
	writeEnvStatusRecords(&notRead, envStatusRecords{
		Source: "machine ledger", Sessions: []envStatusSession{},
		SessionsRead:   false,
		SessionsDetail: "the platform runs this environment's workloads, so a local stack is not its state",
	})
	if !strings.Contains(none.String(), "no local stacks running") {
		t.Errorf("the no-sessions state is not named:\n%s", none.String())
	}
	if !strings.Contains(notRead.String(), "not reported for this environment") {
		t.Errorf("the not-applicable state is not named:\n%s", notRead.String())
	}
	if !strings.Contains(notRead.String(), "the platform runs") {
		t.Errorf("declined without passing the reason through:\n%s", notRead.String())
	}
}

// TestWriteEnvStatusRecordsNamesAnAbandonedApply pins the state the design
// calls out: no outcome arrived by the deadline, which is NOT a failure
// report and NOT a success.
func TestWriteEnvStatusRecordsNamesAnAbandonedApply(t *testing.T) {
	var out strings.Builder
	writeEnvStatusRecords(&out, envStatusRecords{
		Source:       "control plane",
		Location:     "https://cp.example.com",
		SessionsRead: true,
		Sessions:     []envStatusSession{},
		Apply: &envStatusApply{
			ID:           "ap-1",
			State:        string(release.ApplyAbandoned),
			BundleID:     "bd-1",
			BundleDigest: "sha256:abc",
			ReportedBy:   "ci@example.com",
			StartedAt:    recordsNow().Format(time.RFC3339),
			DeadlineAt:   recordsNow().Add(10 * time.Minute).Format(time.RFC3339),
		},
	})
	got := out.String()
	if !strings.Contains(got, "ABANDONED") {
		t.Errorf("the abandoned state is not shown:\n%s", got)
	}
	if !strings.Contains(got, "whether it landed is unknown") {
		t.Errorf("abandoned rendered without saying what it means — the most misread state here:\n%s", got)
	}
	// The server-set identity must be labelled as such: applied_by and
	// reported_by look identical and only one is authority.
	if !strings.Contains(got, "server-set identity") {
		t.Errorf("reported_by is not labelled as the server-set identity:\n%s", got)
	}
}

// TestSessionOfDerivesTheThreeStates pins running / quiet / stopped, and the
// vocabulary: a live session unseen past release.SessionStaleAfter is QUIET,
// not failed — forge cannot tell a crash from a closed laptop.
func TestSessionOfDerivesTheThreeStates(t *testing.T) {
	now := recordsNow()
	base := release.LocalSession{
		Env:        "dev",
		Worktree:   release.Worktree{Key: "wt-1", Label: "feat-x", Host: "host-abc"},
		Provenance: release.Provenance{Branch: "feat-x", Commit: "0123456789abcdef0123456789abcdef01234567", Dirty: true},
		StartedAt:  now.Add(-time.Hour),
	}

	running := base
	running.LastSeenAt = now.Add(-30 * time.Second)
	if got := sessionOf(running, now); got.State != sessionStateRunning {
		t.Errorf("a session seen 30s ago is %q, want running", got.State)
	}

	quiet := base
	quiet.LastSeenAt = now.Add(-release.SessionStaleAfter - time.Minute)
	gotQuiet := sessionOf(quiet, now)
	if gotQuiet.State != sessionStateQuiet {
		t.Errorf("a session unseen past SessionStaleAfter is %q, want quiet", gotQuiet.State)
	}
	if !gotQuiet.Dirty || gotQuiet.Branch != "feat-x" {
		t.Errorf("the worktree claim was dropped: %+v", gotQuiet)
	}

	stopped := base
	stopped.LastSeenAt = now.Add(-time.Minute)
	at := now
	stopped.StoppedAt = &at
	if got := sessionOf(stopped, now); got.State != sessionStateStopped {
		t.Errorf("a stopped session is %q, want stopped", got.State)
	}

	// The primary checkout has an empty key and an empty label; it must
	// still render as something a human can read.
	primary := base
	primary.Worktree = release.Worktree{Host: "host-abc"}
	primary.LastSeenAt = now
	if got := sessionOf(primary, now); got.Worktree == "" {
		t.Error("the primary checkout rendered as an empty worktree label")
	}
}

// TestEnvStatusDocumentRecordsAreAdditive is the compatibility assertion
// R1/R2 depend on: reliant's daemon parses this document, and `forge gate
// record --from` recognises it by top-level `bound` and `images`. Adding
// records must not move or rename either.
func TestEnvStatusDocumentRecordsAreAdditive(t *testing.T) {
	doc := envStatusDocument{
		Env:    "dev",
		Bound:  true,
		Images: []imageVerification{},
		Records: &envStatusRecords{
			Source:       "machine ledger",
			SessionsRead: true,
			Sessions:     []envStatusSession{},
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The two keys gate_doc.go's "verify" recipe recognises, still at the
	// top level. If either moved under `records`, every recorded
	// deploy-verification gate would fall through to the generic reading
	// and silently lose the unbound-env-is-SKIPPED distinction.
	for _, key := range []string{"bound", "images"} {
		if _, ok := generic[key]; !ok {
			t.Fatalf("top-level %q is gone: `forge gate record --from` no longer recognises this document", key)
		}
	}
	records, ok := generic["records"].(map[string]any)
	if !ok {
		t.Fatalf("records is not an object: %T", generic["records"])
	}
	// sessions_read is the field that carries §7.4's distinction into
	// --json. Without it a consumer cannot tell [] from "not applicable".
	if _, ok := records["sessions_read"]; !ok {
		t.Fatal("records.sessions_read is absent: a consumer cannot tell \"no sessions\" from \"sessions not read\"")
	}
	if _, ok := records["sessions"]; !ok {
		t.Fatal("records.sessions is absent; it must be [] rather than missing")
	}
}

// TestReadMachineEnvRecordsReadsWhatTheLedgerHolds is the end-to-end machine
// path: a promotion, a release with provenance, an apply and a session all
// come back through the SAME reader `forge ledger show` uses.
func TestReadMachineEnvRecordsReadsWhatTheLedgerHolds(t *testing.T) {
	dir := newLedgerTestProject(t, "f6b-records")
	store := testStore(t, dir)
	now := recordsNow()

	prov := release.Provenance{
		Repo: "github.com/example/app", Branch: "main",
		Commit: "0123456789abcdef0123456789abcdef01234567",
		Tree:   "89abcdef0123456789abcdef0123456789abcdef",
		Dirty:  false, ForgeVersion: "v0.2.0",
	}
	rel := release.Release{Version: "v1.2.0", CreatedAt: now.Add(-time.Hour), Artifacts: map[string]release.Artifact{
		"ghcr.io/example/app": {
			Kind: release.KindOCI, Mode: release.ModeShared,
			Digests: map[string]string{release.SharedVariant: "sha256:" + strings.Repeat("a", 64)},
		},
	}}
	rel.SetProvenance(prov)
	if _, err := store.CutRelease(rel); err != nil {
		t.Fatalf("cut release: %v", err)
	}
	testPromote(t, dir, release.Promotion{
		Env: "dev", Release: "v1.2.0", Kind: release.KindPromote,
		Resolved:   map[string]string{"ghcr.io/example/app": "sha256:" + strings.Repeat("a", 64)},
		PromotedBy: release.Actor{User: "alice"}, PromotedAt: now.Add(-30 * time.Minute),
	})
	apply, err := store.BeginApply(release.Apply{
		Env: "dev", BundleID: "bd-1", AppliedBy: "forge",
		CreatedAt: now.Add(-10 * time.Minute), DeadlineAt: now.Add(-5 * time.Minute),
	}, false)
	if err != nil {
		t.Fatalf("begin apply: %v", err)
	}
	if err := store.ReportSession(release.LocalSession{
		ID: sessionIDFor("dev", "host-abc", "wt-1"), Env: "dev",
		Worktree:   release.Worktree{Key: "wt-1", Label: "feat-x", Host: "host-abc"},
		Provenance: release.Provenance{Branch: "feat-x", Dirty: true},
		StartedAt:  now.Add(-time.Hour), LastSeenAt: now.Add(-30 * time.Second),
	}); err != nil {
		t.Fatalf("report session: %v", err)
	}

	got, err := readMachineEnvRecords("dev", dir, sessionTarget{Report: true}, now)
	if err != nil {
		t.Fatalf("readMachineEnvRecords: %v", err)
	}
	if got.Source != "machine ledger" {
		t.Errorf("source = %q", got.Source)
	}
	if got.Provenance == nil {
		t.Fatal("no provenance for a bound env whose release carries it")
	}
	if got.Provenance.Commit != prov.Commit || got.Provenance.Branch != "main" {
		t.Errorf("provenance = %+v, want commit/branch from the release record", got.Provenance)
	}
	if got.Provenance.BoundBy.PromotedBy != "alice" {
		t.Errorf("bound_by.promoted_by = %q, want alice", got.Provenance.BoundBy.PromotedBy)
	}
	if got.Apply == nil {
		t.Fatal("no apply for an env with one recorded")
	}
	if got.Apply.ID != apply.ID {
		t.Errorf("apply id = %q, want %q", got.Apply.ID, apply.ID)
	}
	// The deadline passed with no outcome, so the DERIVED state is
	// abandoned — not stored, derived, through the one implementation.
	if got.Apply.State != string(release.ApplyAbandoned) {
		t.Errorf("apply state = %q, want abandoned (deadline passed with no outcome)", got.Apply.State)
	}
	if !got.SessionsRead || len(got.Sessions) != 1 {
		t.Fatalf("sessions_read = %v, sessions = %d, want true and 1", got.SessionsRead, len(got.Sessions))
	}
	if got.Sessions[0].State != sessionStateRunning {
		t.Errorf("session state = %q, want running", got.Sessions[0].State)
	}
}

// TestReadMachineEnvRecordsOnAnEmptyLedger proves the empty state again, at
// the reader rather than the renderer: no records is not an error.
func TestReadMachineEnvRecordsOnAnEmptyLedger(t *testing.T) {
	dir := newLedgerTestProject(t, "f6b-empty")
	testStore(t, dir) // create it

	got, err := readMachineEnvRecords("dev", dir, sessionTarget{Report: true}, recordsNow())
	if err != nil {
		t.Fatalf("an empty ledger is an answer, not an error: %v", err)
	}
	if got.Provenance != nil || got.Apply != nil {
		t.Fatalf("an empty ledger produced records: %+v", got)
	}
	if got.Sessions == nil {
		t.Fatal("Sessions is nil; --json must carry []")
	}
	if got.Detail != "" {
		t.Fatalf("an empty ledger set a failure Detail: %q", got.Detail)
	}
}

// TestReadMachineEnvRecordsCarriesTheSessionSkipReason proves a non-local env
// reaches the view with its reason attached rather than as an empty list.
func TestReadMachineEnvRecordsCarriesTheSessionSkipReason(t *testing.T) {
	dir := newLedgerTestProject(t, "f6b-skip")
	testStore(t, dir)

	got, err := readMachineEnvRecords("prod", dir, sessionTarget{Skip: "this environment runs on a cluster"}, recordsNow())
	if err != nil {
		t.Fatalf("readMachineEnvRecords: %v", err)
	}
	if got.SessionsRead {
		t.Fatal("sessions_read is true for an env that reports none")
	}
	if got.SessionsDetail == "" {
		t.Fatal("the skip reason was dropped between the target and the view")
	}
}
