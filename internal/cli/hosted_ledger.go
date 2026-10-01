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
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/pkg/release"
)

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
}

type wireGate struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	URL    string `json:"url,omitempty"`
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
	if w.FromEnvironmentID != "" {
		// The wire carries the SOURCE env's id; forge speaks names. The id
		// is kept verbatim rather than dropped when no name is known, so the
		// promotion path stays auditable.
		p.FromEnv = w.FromEnvironmentID
	}
	if p.Resolved == nil {
		p.Resolved = map[string]string{}
	}
	if len(w.ResolvedSources) > 0 {
		p.Sources = map[string]release.Source{}
		for name, src := range w.ResolvedSources {
			p.Sources[name] = release.Source(src)
		}
	}
	for _, g := range w.Gates {
		// The LENIENT read path. A control plane holding promotions
		// recorded before the gate status set closed returns whatever
		// free text the old scaffold wrote, and refusing to decode it
		// would make those promotions unrenderable. An unrecognised
		// value maps to error and is kept verbatim on RawStatus.
		status, raw := release.GateStatusFromStored(g.Status)
		p.Gates = append(p.Gates, release.Gate{Name: g.Name, Status: status, RawStatus: raw, URL: g.URL})
	}
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
func (s *hostedStore) Append(ctx context.Context, p release.Promotion) (release.Promotion, error) {
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
	req := map[string]any{"environmentId": id, "version": p.Release}
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
		gates := make([]wireGate, 0, len(p.Gates))
		for _, g := range p.Gates {
			// Only the three fields today's server stores. The rest of
			// release.Gate (summary, timing, run id, details) rides on
			// the wire once P0's DeployGate lands; sending them now
			// would be sending fields no deployed control plane reads.
			gates = append(gates, wireGate{Name: g.Name, Status: string(g.Status), URL: g.URL})
		}
		req["gates"] = gates
	}
	var resp struct {
		Promotion wirePromotion `json:"promotion"`
	}
	if err := s.client.Call(ctx, procPromote, req, &resp); err != nil {
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
	var resp struct {
		Created bool `json:"created"`
	}
	if err := s.client.Call(ctx, procCutRelease, req, &resp); err != nil {
		if strings.Contains(err.Error(), "already_exists") {
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
		// cloud.Client surfaces the Connect code in its message; not_found
		// is the one this read turns into an answer rather than a failure.
		if strings.Contains(err.Error(), "not_found") {
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
