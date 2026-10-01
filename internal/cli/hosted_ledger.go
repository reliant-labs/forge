package cli

// The HOSTED release ledger: the same bindingStore / releaseLedger seam as
// the file backend, served by a control plane's DeployService over Connect.
//
// forge does not import control-plane. Every request and response shape below
// is declared HERE, in proto3 JSON (lowerCamel field names, enums as their
// value names, Timestamps as RFC3339), and carried by cloud.Client.Call. A
// field forge does not read is a field forge does not break on.
//
// THE SERVER OWNS THE RULES THAT NEED A LOCK. Promote applies release.Decide
// inside a transaction that holds the environment row, so the idempotent-retry
// check is made against a history no concurrent promoter can change underneath
// it. This file therefore does
// NOT pre-check with a read — a read-then-write from a client would be the
// race the server's lock exists to close.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/pkg/release"
)

// hostedErrorHasCode reports whether err is a control-plane failure carrying
// the given Connect code.
//
// This is the ONE place a hosted caller classifies a wire failure, and it
// reads the code as DATA. The previous spelling was
// `strings.Contains(err.Error(), "already_exists")`, which is wrong in both
// directions: a server that improves its prose breaks forge's control flow
// silently, and forge's own message text contains the words it searches for,
// so a message that merely MENTIONS a code reads as that code.
//
// errors.As rather than a type assertion, so the classification survives the
// `fmt.Errorf("...: %w", err)` wrapping every caller adds for context.
func hostedErrorHasCode(err error, code string) bool {
	var cerr *cloud.Error
	return errors.As(err, &cerr) && cerr.HasCode(code)
}

// hostedErrorReason is the app-defined domain reason a refusal carries, or
// "" when the failure is not a control-plane error or carried none.
//
// The Connect code is the CATEGORY — every promote refusal is
// FailedPrecondition — and this is what says WHICH refusal, so it is what an
// exit code is mapped from (§3.A: every primitive maps reasons, not message
// text).
func hostedErrorReason(err error) string {
	var cerr *cloud.Error
	if errors.As(err, &cerr) {
		return cerr.Reason
	}
	return ""
}

const (
	procCutRelease     = "controlplane.v1.DeployService/CutRelease"
	procGetRelease     = "controlplane.v1.DeployService/GetRelease"
	procListReleases   = "controlplane.v1.DeployService/ListReleases"
	procPromote        = "controlplane.v1.DeployService/Promote"
	procListPromotions = "controlplane.v1.DeployService/ListPromotions"
)

// hostedReleaseListLimit bounds a release listing. The promote plan orders
// releases to decide a promote's DIRECTION; the server caps a page anyway.
const hostedReleaseListLimit = 500

// ─── Wire shapes (controlplane.v1) ───────────────────────────────────────────

type wireSource struct {
	Repo   string `json:"repo,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Subdir string `json:"subdir,omitempty"`
	Commit string `json:"commit,omitempty"`
}

// wireArtifact is one DeployArtifact: exactly one per (name, variant).
type wireArtifact struct {
	Name      string      `json:"name"`
	Digest    string      `json:"digest,omitempty"`
	Platforms []string    `json:"platforms,omitempty"`
	Kind      string      `json:"kind"`
	Version   string      `json:"version,omitempty"`
	Integrity string      `json:"integrity,omitempty"`
	URI       string      `json:"uri,omitempty"`
	Mode      string      `json:"mode"`
	Variant   string      `json:"variant"`
	Source    *wireSource `json:"source,omitempty"`
}

type wireRelease struct {
	ID              string         `json:"id"`
	Version         string         `json:"version"`
	GitCommit       string         `json:"gitCommit,omitempty"`
	GitTag          string         `json:"gitTag,omitempty"`
	GitDirty        bool           `json:"gitDirty,omitempty"`
	Artifacts       []wireArtifact `json:"artifacts"`
	CreatedByUserID string         `json:"createdByUserId,omitempty"`
	CreatedAt       time.Time      `json:"createdAt"`
	// Run is the CI run that cut this release (DeployRelease.run, tag 9).
	Run *wireRun `json:"run,omitempty"`
}

// wireRun is controlplane.v1.DeployRun: the id that joins a release, its
// promotions and its gates into one readable story. Opaque to the control
// plane, and `provider` is display only.
type wireRun struct {
	ID       string `json:"id"`
	URL      string `json:"url,omitempty"`
	Provider string `json:"provider,omitempty"`
}

// wireGate is controlplane.v1.DeployGate, in full.
//
// Status is a plain STRING rather than a release.GateStatus even though the
// set is closed, because the read path must stay lenient: a control plane
// holding promotions recorded before the set closed returns whatever free
// text the old scaffold wrote, and a typed field would refuse to decode
// them — making historical promotions unrenderable. The mapping happens in
// gateFromWire via release.GateStatusFromStored; the WRITE path is strict
// (release.ParseGateStatus) in gateToWire.
type wireGate struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	URL     string `json:"url,omitempty"`
	Summary string `json:"summary,omitempty"`
	// The check's OWN window, not when it was recorded.
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	// RunID ties this check to one run. Optional: a manual sign-off
	// belongs to a promotion and to no run.
	RunID string `json:"runId,omitempty"`
	// Details is small and structured — counts and verdicts, capped
	// server-side at 8 KiB. The full report is behind URL.
	Details map[string]any `json:"details,omitempty"`
	// RecordedBy and RecordedAt are SET BY THE SERVER and ignored on a
	// request: evidence whose author the author chose is not
	// attributable. forge reads them and never sends them.
	RecordedBy string     `json:"recordedBy,omitempty"`
	RecordedAt *time.Time `json:"recordedAt,omitempty"`
}

type wirePromotion struct {
	ID                string                `json:"id"`
	EnvironmentID     string                `json:"environmentId"`
	ReleaseID         string                `json:"releaseId"`
	ReleaseVersion    string                `json:"releaseVersion"`
	Kind              string                `json:"kind"`
	FromEnvironmentID string                `json:"fromEnvironmentId,omitempty"`
	ResolvedArtifacts map[string]string     `json:"resolvedArtifacts,omitempty"`
	ResolvedSources   map[string]wireSource `json:"resolvedSources,omitempty"`
	PromotedByUserID  string                `json:"promotedByUserId,omitempty"`
	PromotedByActor   string                `json:"promotedByActor,omitempty"`
	Gates             []wireGate            `json:"gates,omitempty"`
	Note              string                `json:"note,omitempty"`
	CreatedAt         time.Time             `json:"createdAt"`

	// RecordedGates is evidence that arrived AFTER this row was written,
	// through RecordGate. Kept separate from Gates because the two answer
	// different questions: Gates is what the promoter claimed at the
	// moment they promoted, and these are results that came later.
	// Merging them would lose "was this known before the button was
	// pressed?", the first question asked about a bad release.
	RecordedGates []wireGate `json:"recordedGates,omitempty"`
	// SupersededInFlight: this promote overrode an unfinished rollout
	// (`--supersede`). Recorded because an override that leaves no trace
	// is indistinguishable from the refusal never having fired.
	SupersededInFlight bool `json:"supersededInFlight,omitempty"`
	// FromEnvironmentName is the NAME of FromEnvironmentID, so a ledger
	// reader renders "staging → prod" without a lookup per row. This is
	// what lets promotionFromWire report a name where it previously had
	// to fall back to the opaque id.
	FromEnvironmentName string `json:"fromEnvironmentName,omitempty"`
	// FromPromotionID is the specific promotion this release was taken
	// from (`promote --from staging`). FromEnvironmentID says WHERE it
	// came from; this says exactly WHAT was there, which is what keeps
	// "promote whatever staging is running" auditable after staging has
	// moved on.
	FromPromotionID string `json:"fromPromotionId,omitempty"`
	// Run is the CI run that performed this promotion.
	Run *wireRun `json:"run,omitempty"`
}

// ─── Rollout, refusal and run (P0's read shapes) ─────────────────────────────
//
// These are the documents the §3.2 wait, the §3.1 refusal and the §3.7 run
// view read. They are declared HERE, with F0, rather than by each verb that
// consumes them, so the wire contract has ONE spelling: F3's wait, F2's
// promote refusal and F8's run show all decode the same structs, and a field
// the server renamed breaks in one place instead of three. §4.3 makes this
// explicit — "a missing wire field is an F0 follow-up".

// The DeployRolloutPhase enum's value names, as protojson writes them.
//
// A rollout phase is DERIVED on read, never stored, and the distinctions
// matter to the exit codes: SUPERSEDED and UNKNOWN are their own outcomes
// rather than failures, because "overtaken" and "we cannot see it" are not
// "the release is bad".
const (
	wireRolloutPhasePrefix      = "DEPLOY_ROLLOUT_PHASE_"
	wireRolloutPhaseUnspecified = "DEPLOY_ROLLOUT_PHASE_UNSPECIFIED"
	wireRolloutPhasePending     = "DEPLOY_ROLLOUT_PHASE_PENDING"
	wireRolloutPhaseProgressing = "DEPLOY_ROLLOUT_PHASE_PROGRESSING"
	wireRolloutPhaseStabilizing = "DEPLOY_ROLLOUT_PHASE_STABILIZING"
	wireRolloutPhaseSucceeded   = "DEPLOY_ROLLOUT_PHASE_SUCCEEDED"
	wireRolloutPhaseDegraded    = "DEPLOY_ROLLOUT_PHASE_DEGRADED"
	wireRolloutPhaseSuperseded  = "DEPLOY_ROLLOUT_PHASE_SUPERSEDED"
	wireRolloutPhaseUnknown     = "DEPLOY_ROLLOUT_PHASE_UNKNOWN"
)

// rolloutPhaseName renders a phase enum value for a human and for --json:
// "DEPLOY_ROLLOUT_PHASE_DEGRADED" → "degraded". An unrecognised value is
// returned verbatim rather than mapped to a known phase, because a phase
// forge does not understand must not read as a success or a failure.
func rolloutPhaseName(wire string) string {
	if wire == "" || wire == wireRolloutPhaseUnspecified {
		return "unspecified"
	}
	if trimmed := strings.TrimPrefix(wire, wireRolloutPhasePrefix); trimmed != wire {
		return strings.ToLower(trimmed)
	}
	return wire
}

// exitCodeForRolloutPhase maps a terminal phase to §3.A's exit code.
//
// The three non-obvious rows are the point of the table. A rollout still
// PENDING / PROGRESSING / STABILIZING at the deadline is exitTimedOut (5),
// not exitWrong — it was progressing, and a pipeline should retry the WAIT
// rather than conclude the release is bad. SUPERSEDED is 6, its own outcome:
// the wait's subject is gone, so neither retry nor failure is right.
// UNKNOWN is exitUndetermined (2), never folded into either: "we cannot see
// it" is not permission.
func exitCodeForRolloutPhase(wire string) int {
	switch wire {
	case wireRolloutPhaseSucceeded:
		return exitOK
	case wireRolloutPhaseDegraded:
		return exitWrong
	case wireRolloutPhaseSuperseded:
		return exitSuperseded
	case wireRolloutPhaseUnknown:
		return exitUndetermined
	case wireRolloutPhasePending, wireRolloutPhaseProgressing, wireRolloutPhaseStabilizing:
		return exitTimedOut
	default:
		// A phase forge does not recognise is unobservable to forge,
		// which is exactly what 2 means. Reading it as a pass would be
		// the dangerous default.
		return exitUndetermined
	}
}

// wireWorkloadRollout is controlplane.v1.DeployWorkloadRollout: one
// workload's progress toward one promotion's pin.
type wireWorkloadRollout struct {
	DeploymentID string `json:"deploymentId"`
	Name         string `json:"name"`
	// Artifact is the release artifact key this workload runs
	// (Deployment.artifact). Empty on an unpinned row.
	Artifact string `json:"artifact,omitempty"`
	// PinnedDigest comes from THIS promotion's resolvedArtifacts — the
	// frozen pin, not whatever the deployment row declares now. That is
	// the whole point of scoping a rollout to a promotion: a wait must
	// not succeed on bytes it was never asked about because somebody
	// promoted again underneath it.
	PinnedDigest string `json:"pinnedDigest,omitempty"`
	// DesiredDigest is what the row currently declares, which differs
	// from PinnedDigest while a promotion is unapplied.
	DesiredDigest  string     `json:"desiredDigest,omitempty"`
	ObservedDigest string     `json:"observedDigest,omitempty"`
	ObservedState  string     `json:"observedState,omitempty"`
	Verdict        string     `json:"verdict,omitempty"`
	Phase          string     `json:"phase,omitempty"`
	StableSince    *time.Time `json:"stableSince,omitempty"`
	ConvergedAt    *time.Time `json:"convergedAt,omitempty"`
	LastError      string     `json:"lastError,omitempty"`
	// UpdatedReplicas and DesiredReplicas are what stop a rollout being
	// reported healthy before it is. Under RollingUpdate, old ready pods
	// keep readyReplicas up while the new ReplicaSet crash-loops, so
	// "ready ≥ wanted" is true of a release that never served a request.
	// Comparing UPDATED against DESIRED is the completion test
	// `kubectl rollout status` applies.
	UpdatedReplicas int32 `json:"updatedReplicas,omitempty"`
	DesiredReplicas int32 `json:"desiredReplicas,omitempty"`
}

// wireRollout is controlplane.v1.DeployRollout: the single server-computed
// answer to "is v6 done". Computed in one place on purpose, so wait, the
// in-flight refusal, the UI and a run timeline cannot hold four different
// opinions about whether a release landed.
type wireRollout struct {
	Promotion wirePromotion         `json:"promotion"`
	Phase     string                `json:"phase,omitempty"`
	Workloads []wireWorkloadRollout `json:"workloads,omitempty"`
	// Unpinned are workloads the promotion does not pin: databases,
	// third-party images. REPORTED, never gating — a database that cannot
	// be release-bound must not be able to fail a release.
	Unpinned          []wireWorkloadRollout `json:"unpinned,omitempty"`
	StartedAt         *time.Time            `json:"startedAt,omitempty"`
	FinishedAt        *time.Time            `json:"finishedAt,omitempty"`
	StabilityWindowMS int64                 `json:"stabilityWindowMs,omitempty"`
	// ConvergesPromotions false means nothing will move without a
	// client-side deploy, so a caller should refuse FAST rather than wait
	// out a timeout whose cause is "nobody was ever going to apply this".
	ConvergesPromotions bool   `json:"convergesPromotions,omitempty"`
	Reason              string `json:"reason,omitempty"`
}

// wirePromoteRefusal is controlplane.v1.DeployPromoteRefusal, carried as a
// Connect error detail on the FailedPrecondition a refused promote returns.
//
// It names WHAT IS THERE, not just that something was. A refusal reading
// "someone else promoted" sends a human to a dashboard; one carrying the
// actual current promotion lets the pipeline print "prod is on v1.9.1,
// promoted by alice 4 minutes ago" without a second round trip.
type wirePromoteRefusal struct {
	// Reason is one of the reason* constants (promotion_conflict |
	// rollout_in_flight | environment_pinned).
	Reason string `json:"reason"`
	// Echoed back from the request, so a log line is self-contained.
	ExpectedCurrentPromotionID string `json:"expectedCurrentPromotionId,omitempty"`
	ExpectedUnbound            bool   `json:"expectedUnbound,omitempty"`
	// ActualCurrent is the promotion that landed instead.
	ActualCurrent *wirePromotion `json:"actualCurrent,omitempty"`
	// ActualPhase is set for rollout_in_flight: the phase that made it
	// in flight.
	ActualPhase string `json:"actualPhase,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// promoteRefusalType is the protobuf full name under which a refusal rides
// as a Connect error detail.
const promoteRefusalType = "controlplane.v1.DeployPromoteRefusal"

// promoteRefusalOf decodes the refusal detail from a failed promote, if the
// server sent one.
//
// A refusal WITHOUT a decodable detail is not an error here: the reason
// header alone is enough to choose an exit code (exitCodeForRefusal), and
// the detail only enriches the message. Treating a missing detail as a
// failure would make forge refuse to report a refusal.
func promoteRefusalOf(err error) (wirePromoteRefusal, bool) {
	var cerr *cloud.Error
	if !errors.As(err, &cerr) {
		return wirePromoteRefusal{}, false
	}
	payload, ok := cerr.DetailJSON(promoteRefusalType)
	if !ok {
		return wirePromoteRefusal{}, false
	}
	var refusal wirePromoteRefusal
	if jsonErr := json.Unmarshal(payload, &refusal); jsonErr != nil {
		return wirePromoteRefusal{}, false
	}
	// The header is authoritative when both are present; a detail that
	// states no reason still carries the useful part (actual_current).
	if refusal.Reason == "" {
		refusal.Reason = cerr.Reason
	}
	return refusal, true
}

// wireRunStage is controlplane.v1.DeployRunStage: one step in a run's
// timeline, ASSEMBLED FROM THE LEDGER. Nothing here is a stored event —
// a stage is a release cut, a promotion, a recorded gate carrying the run
// id, or a promotion's derived rollout.
type wireRunStage struct {
	Kind          string     `json:"kind"`
	Name          string     `json:"name,omitempty"`
	EnvironmentID string     `json:"environmentId,omitempty"`
	PromotionID   string     `json:"promotionId,omitempty"`
	Status        string     `json:"status,omitempty"`
	StartedAt     *time.Time `json:"startedAt,omitempty"`
	FinishedAt    *time.Time `json:"finishedAt,omitempty"`
	URL           string     `json:"url,omitempty"`
	Summary       string     `json:"summary,omitempty"`
}

// ─── Gate conversion (through R1's vocabulary) ───────────────────────────────

// gateFromWire is the LENIENT read path. A control plane holding promotions
// recorded before the gate status set closed returns whatever free text the
// old scaffold wrote, and refusing to decode it would make those promotions
// unrenderable — the ledger is append-only, so no migration could clean
// them up. An unrecognised value maps to `error` and is kept verbatim on
// RawStatus, which is exactly release.GateStatusFromStored's contract.
func gateFromWire(w wireGate) release.Gate {
	status, raw := release.GateStatusFromStored(w.Status)
	return release.Gate{
		Name:       w.Name,
		Status:     status,
		RawStatus:  raw,
		URL:        w.URL,
		Summary:    w.Summary,
		StartedAt:  w.StartedAt,
		FinishedAt: w.FinishedAt,
		RunID:      w.RunID,
		Details:    w.Details,
		RecordedBy: w.RecordedBy,
		RecordedAt: w.RecordedAt,
	}
}

func gatesFromWire(in []wireGate) []release.Gate {
	if len(in) == 0 {
		return nil
	}
	out := make([]release.Gate, 0, len(in))
	for _, w := range in {
		out = append(out, gateFromWire(w))
	}
	return out
}

// gateToWire is the STRICT write path: release.Gate.Validate refuses a
// status outside the closed set, and a gate carrying a RawStatus (one that
// only survived a READ because it was mapped) cannot be written back.
//
// RecordedBy and RecordedAt are deliberately NOT sent. The server sets them
// from the authenticated principal, and a client-supplied attribution would
// be a client claiming who vouched for a check — the one part of a piece of
// evidence that must not come from the party being vouched for.
func gateToWire(g release.Gate) (wireGate, error) {
	if err := g.Validate(); err != nil {
		return wireGate{}, err
	}
	return wireGate{
		Name:       g.Name,
		Status:     string(g.Status),
		URL:        g.URL,
		Summary:    g.Summary,
		StartedAt:  g.StartedAt,
		FinishedAt: g.FinishedAt,
		RunID:      g.RunID,
		Details:    g.Details,
	}, nil
}

func gatesToWire(in []release.Gate) ([]wireGate, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]wireGate, 0, len(in))
	for _, g := range in {
		w, err := gateToWire(g)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, nil
}

// runFromWire and runToWire convert controlplane.v1.DeployRun.
func runFromWire(w *wireRun) release.Run {
	if w == nil {
		return release.Run{}
	}
	return release.Run{ID: w.ID, URL: w.URL, Provider: w.Provider}
}

// The DeployPromotionKind enum's value names, as protojson writes them.
const (
	wireKindPromote = "DEPLOY_PROMOTION_KIND_PROMOTE"
	// wireKindLegacyRollback is what a control plane returns for a ledger
	// row written by the retired Rollback RPC. READ ONLY: it binds the env
	// to its release exactly as a promote does, and decodes as one.
	wireKindLegacyRollback = "DEPLOY_PROMOTION_KIND_ROLLBACK"
)

// ─── Conversion ──────────────────────────────────────────────────────────────

// releaseToWire flattens a release into one DeployArtifact per (name,
// variant), in a stable order.
func releaseToWire(r release.Release) []wireArtifact {
	var out []wireArtifact
	for _, name := range r.ArtifactNames() {
		a := r.Artifacts[name]
		base := wireArtifact{
			Name: name, Kind: string(a.Kind), Mode: string(a.Mode),
			Platforms: a.Platforms, Version: a.Version, Integrity: a.Integrity, URI: a.URI,
		}
		if a.Source != nil {
			s := wireSource(*a.Source)
			base.Source = &s
		}
		if len(a.Digests) == 0 {
			base.Variant = release.SharedVariant
			out = append(out, base)
			continue
		}
		variants := make([]string, 0, len(a.Digests))
		for v := range a.Digests {
			variants = append(variants, v)
		}
		sort.Strings(variants)
		for _, v := range variants {
			row := base
			row.Variant = v
			row.Digest = a.Digests[v]
			out = append(out, row)
		}
	}
	return out
}

// releaseFromWire regroups DeployArtifact rows into the release map and
// validates the result — a hosted release is held to the same closed enums
// as one read from disk.
func releaseFromWire(w wireRelease) (release.Release, error) {
	r := release.Release{
		Version:   w.Version,
		Git:       release.Git{Commit: w.GitCommit, Tag: w.GitTag, Dirty: w.GitDirty},
		CreatedAt: w.CreatedAt,
		CreatedBy: w.CreatedByUserID,
		Artifacts: map[string]release.Artifact{},
		Run:       runFromWire(w.Run),
	}
	for _, row := range w.Artifacts {
		a, seen := r.Artifacts[row.Name]
		if !seen {
			a = release.Artifact{
				Kind: release.Kind(row.Kind), Mode: release.Mode(row.Mode),
				Platforms: row.Platforms, Version: row.Version, Integrity: row.Integrity, URI: row.URI,
			}
			if row.Source != nil {
				s := release.Source(*row.Source)
				a.Source = &s
			}
		}
		if row.Digest != "" {
			if a.Digests == nil {
				a.Digests = map[string]string{}
			}
			a.Digests[row.Variant] = row.Digest
		}
		r.Artifacts[row.Name] = a
	}
	if err := r.Validate(); err != nil {
		return release.Release{}, fmt.Errorf("control plane returned release %q: %w", w.Version, err)
	}
	return r, nil
}

func promotionKindFromWire(k string) (release.PromotionKind, error) {
	switch k {
	case wireKindPromote, wireKindLegacyRollback:
		return release.KindPromote, nil
	default:
		// Never defaulted: an unknown kind read as "promote" would make a
		// future entry type indistinguishable from a binding.
		return "", fmt.Errorf("%w: control plane returned promotion kind %q", release.ErrInvalid, k)
	}
}

// ─── The store ───────────────────────────────────────────────────────────────

// hostedStore implements both bindingStore and releaseLedger against one
// control plane.
type hostedStore struct {
	client   cloudCaller
	resolver hostedEnvResolver
	endpoint string
	// project and kind address the environment a WRITE ensures: identity is
	// (org, project, name), and the kind (derived from the env's KCL) is
	// immutable server-side.
	project string
	kind    deploytarget.HostedEnvKind

	mu     sync.Mutex
	envIDs map[string]string // env name → control-plane id, per process
}

// hostedLedger binds both halves of an env's ledger to one client. project
// and kind are the ledger env's control-plane address (see hostedEnvRefFor).
func hostedLedger(client cloudCaller, endpoint, project string, kind deploytarget.HostedEnvKind) envLedger {
	s := &hostedStore{client: client, resolver: cloudEnvResolver{client: client, project: project}, endpoint: endpoint, project: project, kind: kind}
	return envLedger{Bindings: s, Releases: s, Hosted: true}
}

func (s *hostedStore) Location() string { return s.endpoint }

// envID resolves (and remembers) the control plane's id for an env name.
func (s *hostedStore) envID(ctx context.Context, env string) (string, error) {
	s.mu.Lock()
	id, ok := s.envIDs[env]
	s.mu.Unlock()
	if ok {
		return id, nil
	}
	id, err := s.resolver.ResolveEnvironmentID(ctx, env)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	if s.envIDs == nil {
		s.envIDs = map[string]string{}
	}
	s.envIDs[env] = id
	s.mu.Unlock()
	return id, nil
}

// promotionFromWire converts one ledger entry. env is the NAME this store was
// asked about; the wire carries ids.
func (s *hostedStore) promotionFromWire(env string, w wirePromotion) (release.Promotion, error) {
	kind, err := promotionKindFromWire(w.Kind)
	if err != nil {
		return release.Promotion{}, err
	}
	p := release.Promotion{
		ID:         w.ID,
		Env:        env,
		Release:    w.ReleaseVersion,
		Kind:       kind,
		Resolved:   w.ResolvedArtifacts,
		PromotedBy: release.Actor{User: w.PromotedByUserID, Actor: w.PromotedByActor},
		Note:       w.Note,
		PromotedAt: w.CreatedAt,
	}
	switch {
	case w.FromEnvironmentName != "":
		// P0 added the NAME beside the id, so a ledger reader renders
		// "staging → prod" without a lookup per row. forge speaks
		// names, so prefer it.
		p.FromEnv = w.FromEnvironmentName
	case w.FromEnvironmentID != "":
		// A control plane that predates fromEnvironmentName sends only
		// the id. Kept verbatim rather than dropped, so the promotion
		// path stays auditable even when the name is unknown.
		p.FromEnv = w.FromEnvironmentID
	}
	p.FromPromotionID = w.FromPromotionID
	p.SupersededInFlight = w.SupersededInFlight
	p.Run = runFromWire(w.Run)
	if p.Resolved == nil {
		p.Resolved = map[string]string{}
	}
	if len(w.ResolvedSources) > 0 {
		p.Sources = map[string]release.Source{}
		for name, src := range w.ResolvedSources {
			p.Sources[name] = release.Source(src)
		}
	}
	// Both halves of the evidence trail: what the promoter claimed when
	// they promoted, and what arrived afterwards through RecordGate. Kept
	// in separate fields because merging them would lose "was this known
	// before the button was pressed?".
	p.Gates = gatesFromWire(w.Gates)
	p.RecordedGates = gatesFromWire(w.RecordedGates)
	if err := p.Validate(); err != nil {
		return release.Promotion{}, fmt.Errorf("control plane returned promotion %s: %w", w.ID, err)
	}
	return p, nil
}

// Current is the newest ListPromotions entry. An environment the control
// plane has never heard of has, by definition, never been promoted.
func (s *hostedStore) Current(ctx context.Context, env string) (release.Promotion, bool, error) {
	id, err := s.envID(ctx, env)
	if errors.Is(err, errHostedEnvNotFound) {
		return release.Promotion{}, false, nil
	}
	if err != nil {
		return release.Promotion{}, false, err
	}
	var resp struct {
		Promotions []wirePromotion `json:"promotions"`
	}
	if err := s.client.Call(ctx, procListPromotions, map[string]any{"environmentId": id, "limit": 1}, &resp); err != nil {
		return release.Promotion{}, false, err
	}
	if len(resp.Promotions) == 0 {
		return release.Promotion{}, false, nil
	}
	p, err := s.promotionFromWire(env, resp.Promotions[0])
	if err != nil {
		return release.Promotion{}, false, err
	}
	return p, true, nil
}

// Append calls Promote. The server freezes the pin set from the
// release it holds — the Resolved/Sources on p are the client's PREVIEW and
// are deliberately not sent, because a request that could state digests
// would be a request that could ship bytes nobody cut.
//
// The guard rides as PromoteReleaseRequest tags 7–9 and is checked by the
// SERVER, under the env row lock, after its own idempotent no-op. A refusal
// comes back as a *promoteRefusedError carrying what is actually current.
func (s *hostedStore) Append(ctx context.Context, p release.Promotion, guard appendGuard) (release.Promotion, error) {
	if err := p.Validate(); err != nil {
		return release.Promotion{}, err
	}
	// A promotion is a WRITE: ensure the env by name first (see
	// ensureHostedEnv).
	if s.kind == deploytarget.HostedEnvLocal {
		// A LOCAL env runs on a developer machine; it has no release to
		// bind. The control plane refuses this too — refusing here means
		// no write is attempted at all.
		return release.Promotion{}, fmt.Errorf("env %q is LOCAL (it declares control_plane but no hosted tier): it runs via `forge env up`, and a release cannot be promoted into it", p.Env)
	}
	id, err := ensureHostedEnv(ctx, s.client, deploytarget.HostedEnvRef{Project: s.project, Name: p.Env, Kind: s.kind})
	if err != nil {
		return release.Promotion{}, err
	}
	s.mu.Lock()
	if s.envIDs == nil {
		s.envIDs = map[string]string{}
	}
	s.envIDs[p.Env] = id
	s.mu.Unlock()
	req := map[string]any{"environmentId": id}
	if !guard.ResolveVersionFromSource {
		req["version"] = p.Release
	}
	// Otherwise NO version is sent (§3.4). p.Release holds the plan's
	// preview of what fromPromotionId names, and sending it would make
	// forge assert a release it read earlier, outside the target's lock —
	// exactly the stale read `--from` exists to eliminate. The server
	// resolves the release from the promotion id instead, and refuses
	// source_moved if the source has moved past it.
	if p.PromotedBy.Actor != "" {
		req["promotedByActor"] = p.PromotedBy.Actor
	}
	if p.Note != "" {
		req["note"] = p.Note
	}
	if p.FromEnv != "" {
		fromID, ferr := s.envID(ctx, p.FromEnv)
		if ferr != nil {
			return release.Promotion{}, ferr
		}
		req["fromEnvironmentId"] = fromID
	}
	if len(p.Gates) > 0 {
		// The STRICT write path: a status outside the closed set, or a
		// gate that only survived a READ because its status was mapped,
		// is refused here rather than recorded as evidence.
		gates, gerr := gatesToWire(p.Gates)
		if gerr != nil {
			return release.Promotion{}, fmt.Errorf("promote %s to %s: %w", p.Release, p.Env, gerr)
		}
		req["gates"] = gates
	}
	// RecordedGates are NOT sent: they are the post-promote half of the
	// trail and are appended through RecordGate (§3.3), against a
	// promotion that must already exist. Sending them on the promote
	// would claim evidence that arrived later was known beforehand.
	if run := runWireFields(p.Run); run != nil {
		req["run"] = run
	}
	if p.FromPromotionID != "" {
		req["fromPromotionId"] = p.FromPromotionID
	}
	guardWireFields(guard, req)
	var resp struct {
		Promotion wirePromotion `json:"promotion"`
	}
	if err := s.client.Call(ctx, procPromote, req, &resp); err != nil {
		if refused := s.refusalFromWire(p.Env, err); refused != nil {
			return release.Promotion{}, refused
		}
		return release.Promotion{}, err
	}
	return s.promotionFromWire(p.Env, resp.Promotion)
}

// Cut calls CutRelease.
func (s *hostedStore) Cut(ctx context.Context, r release.Release) (bool, error) {
	if err := r.Validate(); err != nil {
		return false, err
	}
	req := map[string]any{
		"version":   r.Version,
		"artifacts": releaseToWire(r),
		"gitCommit": r.Git.Commit,
		"gitTag":    r.Git.Tag,
		"gitDirty":  r.Git.Dirty,
	}
	if run := runWireFields(r.Run); run != nil {
		req["run"] = run
	}
	var resp struct {
		Created bool `json:"created"`
	}
	if err := s.client.Call(ctx, procCutRelease, req, &resp); err != nil {
		// THE CONNECT CODE, not the message text. A re-cut of a version
		// whose artifacts differ is the one failure this turns into a
		// domain error, and recognising it by searching the message
		// would break the moment the server reworded it — silently,
		// because the branch simply stops matching and the conflict
		// falls through as a generic failure.
		if hostedErrorHasCode(err, cloud.CodeAlreadyExists) {
			return false, fmt.Errorf("release %q: %w: %v", r.Version, release.ErrReleaseConflict, err)
		}
		return false, err
	}
	return resp.Created, nil
}

// Get calls GetRelease; a NotFound answer is (nil, nil), matching the file
// backend's "never cut".
func (s *hostedStore) Get(ctx context.Context, version string) (*release.Release, error) {
	var resp struct {
		Release wireRelease `json:"release"`
	}
	if err := s.client.Call(ctx, procGetRelease, map[string]any{"version": version}, &resp); err != nil {
		// not_found is the one code this read turns into an ANSWER
		// rather than a failure: a version nobody cut is the file
		// backend's "never cut", not an error.
		if hostedErrorHasCode(err, cloud.CodeNotFound) {
			return nil, nil
		}
		return nil, err
	}
	r, err := releaseFromWire(resp.Release)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// List calls ListReleases and orders the result the way the file backend
// does, so promote's direction logic sees one ordering whichever backend
// answered.
func (s *hostedStore) List(ctx context.Context) ([]release.Release, error) {
	var resp struct {
		Releases []wireRelease `json:"releases"`
	}
	if err := s.client.Call(ctx, procListReleases, map[string]any{"limit": hostedReleaseListLimit}, &resp); err != nil {
		return nil, err
	}
	out := make([]release.Release, 0, len(resp.Releases))
	for _, w := range resp.Releases {
		r, err := releaseFromWire(w)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	sortReleasesNewestFirst(out)
	return out, nil
}
