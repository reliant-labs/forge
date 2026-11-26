package cli

// Sending a release's SBOMs to a control plane: the one code path behind the
// upload after a cut (hostedStore.recordReleaseSBOMs), the re-send of the
// releases an older control plane refused (drainPendingSBOMs), and
// `forge vuln sbom push` (vuln_sbom_push.go).
//
// THE SERVER IS IDEMPOTENT on (release, artifact, digest): sending an SBOM
// it already holds answers created=false, and sending a different one for an
// artifact that has one answers AlreadyExists (it keeps its copy). So a
// re-send is always safe, and every outcome is one of a few statuses.
//
// WHICH SBOM DESCRIBES AN ARTIFACT. The control plane's copy of a release
// names no SBOM digest for an artifact whose SBOM it never took (SBOM refs
// travel through RecordReleaseSBOM, never inside the release document), so
// the mapping is looked up, in order: an explicit --sbom override; the
// release forge holds in memory (a cut); the control plane's own record; the
// machine ledger's per-release index (ledgerfile.ReleaseSBOMIndex), which
// every cut writes; and, for a release cut before the index existed, the
// SBOM files themselves, matched by CycloneDX metadata.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/internal/ledgerfile"
	"github.com/reliant-labs/forge/internal/sbom"
	"github.com/reliant-labs/forge/pkg/release"
)

// sbomLedger is the machine ledger an upload reads SBOM bytes from and
// records the release's SBOM index in (*ledgerfile.Store).
type sbomLedger interface {
	GetSBOM(digest string) ([]byte, error)
	SBOMs() ([]ledgerfile.SBOMFile, error)
	SBOMHead(digest string, n int) ([]byte, error)
	ReleaseSBOMIndex(version string) (*ledgerfile.ReleaseSBOMIndex, error)
	UpdateReleaseSBOMIndex(version string, fn func(*ledgerfile.ReleaseSBOMIndex) error) error
	PendingSBOMUploads(endpoint string) ([]ledgerfile.ReleaseSBOMIndex, []string, error)
}

// The per-artifact outcomes of a push (sbomPushArtifact.Status).
const (
	sbomPushSent          = "sent"
	sbomPushAlready       = "already_recorded"
	sbomPushDiffers       = "differs"
	sbomPushMissing       = "missing"
	sbomPushFailed        = "failed"
	sbomPushNotApplicable = "not_applicable"
)

// Where an artifact's SBOM was found (sbomPushArtifact.From).
const (
	sbomFromOverride     = "override"
	sbomFromRelease      = "release"
	sbomFromControlPlane = "control_plane"
	sbomFromIndex        = "index"
	sbomFromMetadata     = "metadata"
)

// sbomPushArtifact is one artifact's outcome.
type sbomPushArtifact struct {
	// Artifact is the artifact's key in the release.
	Artifact string `json:"artifact"`
	// Status is sent, already_recorded, differs, missing, failed or
	// not_applicable.
	Status string `json:"status"`
	// SBOMDigest is the SBOM forge sent, or would have sent.
	SBOMDigest string `json:"sbom_digest,omitempty"`
	// RecordedDigest is the SBOM the control plane records for the
	// artifact, when it said (already_recorded, differs).
	RecordedDigest string `json:"recorded_digest,omitempty"`
	// From is where the artifact's SBOM was found: override, release,
	// control_plane, index or metadata.
	From string `json:"from,omitempty"`
	// Reason says why the artifact is missing, failed, differs or not
	// applicable.
	Reason string `json:"reason,omitempty"`
	// NeedsOverride: no SBOM can be proven to describe the artifact from
	// this ledger alone; name one with --sbom <artifact>=<digest>.
	NeedsOverride bool `json:"needs_override,omitempty"`
	// Candidates are the SBOMs in this ledger that name the artifact but
	// cannot be proven to describe this release's bytes (a static site's
	// SBOM records its lockfile's digest), newest first.
	Candidates []string `json:"candidates,omitempty"`

	ref *release.SBOMRef
}

// sbomPushRun is one release's push.
type sbomPushRun struct {
	Version string
	Project string
	CutAt   time.Time
	// Artifacts is the release's artifacts. Nil sends what the machine
	// ledger's index names (a drain).
	Artifacts map[string]release.Artifact
	// Overrides is --sbom: artifact → SBOM digest, vouched for by the
	// operator.
	Overrides map[string]string
	// Remote is the SBOM digest the control plane records per artifact;
	// nil when it was not read. An artifact it names is not re-sent.
	Remote map[string]string
	// Fallback matches SBOM files by CycloneDX metadata when nothing else
	// names an artifact's SBOM.
	Fallback bool
}

// sbomPushOutcome is a push's result.
type sbomPushOutcome struct {
	Artifacts []sbomPushArtifact
	// Unimplemented: the control plane answered Unimplemented, and nothing
	// after it was sent.
	Unimplemented bool
	// Refused is the control plane's refusal (no sign-in, no scope).
	Refused error
	// NotFound: the control plane does not know the release.
	NotFound bool
	Notes    []string
}

// pending reports whether the release has SBOMs the control plane has not
// taken that a later send could deliver.
func (o sbomPushOutcome) pending() bool {
	if o.Unimplemented || o.Refused != nil {
		return true
	}
	for _, a := range o.Artifacts {
		if a.Status == sbomPushFailed {
			return true
		}
	}
	return false
}

// sbomUploader sends SBOMs to one control plane.
type sbomUploader struct {
	client   cloudCaller
	endpoint string
	project  string
}

// sbomSendResult is one RecordReleaseSBOM.
type sbomSendResult struct {
	status   string
	recorded string
	err      error
	// unimplemented, refused and notFound classify err.
	unimplemented, refused, notFound bool
}

func (u sbomUploader) send(ctx context.Context, version, project, name string, ref release.SBOMRef, data []byte) sbomSendResult {
	gz, err := gzipBytes(data)
	if err != nil {
		return sbomSendResult{status: sbomPushFailed, err: fmt.Errorf("compress it: %w", err)}
	}
	req := map[string]any{
		"version":      version,
		"artifactName": name,
		"sbom":         gz, // []byte: proto3 JSON's base64
		"encoding":     sbomEncodingGzip,
		"format":       ref.Format,
	}
	// The org is the credential's (and Forge-Project-ID's), exactly as on
	// every DeployService call: org_id is left unset.
	if project == "" {
		project = u.project
	}
	if project != "" {
		req["project"] = project
	}
	for k, v := range map[string]string{"generator": ref.Generator, "source": ref.Source, "referrer": ref.Referrer} {
		if v != "" {
			req[k] = v
		}
	}
	var resp struct {
		Digest         string `json:"digest"`
		Created        bool   `json:"created"`
		ScanState      string `json:"scanState"`
		ComponentCount int32  `json:"componentCount"`
	}
	if err := u.client.Call(cloud.WithRequestTimeout(ctx, sbomUploadTimeout(len(gz))), procRecordReleaseSBOM, req, &resp); err != nil {
		switch {
		case hostedUnimplemented(err):
			return sbomSendResult{status: sbomPushFailed, err: err, unimplemented: true}
		case hostedErrorHasCode(err, cloud.CodeAlreadyExists):
			return sbomSendResult{status: sbomPushDiffers, err: err}
		case hostedErrorHasCode(err, cloud.CodePermissionDenied), hostedErrorHasCode(err, cloud.CodeUnauthenticated):
			return sbomSendResult{status: sbomPushFailed, err: err, refused: true}
		case hostedErrorHasCode(err, cloud.CodeNotFound):
			return sbomSendResult{status: sbomPushFailed, err: err, notFound: true}
		}
		return sbomSendResult{status: sbomPushFailed, err: err}
	}
	if resp.Digest != "" && resp.Digest != ref.Digest {
		return sbomSendResult{status: sbomPushDiffers, recorded: resp.Digest}
	}
	if !resp.Created {
		return sbomSendResult{status: sbomPushAlready, recorded: resp.Digest}
	}
	return sbomSendResult{status: sbomPushSent, recorded: resp.Digest}
}

// pushReleaseSBOMs sends run's SBOMs through up, reading the bytes from
// store, and records what it sent in the release's index. It never returns
// an error: every outcome is in the result.
func pushReleaseSBOMs(ctx context.Context, up sbomUploader, store sbomLedger, run sbomPushRun) sbomPushOutcome {
	var out sbomPushOutcome
	idx, err := store.ReleaseSBOMIndex(run.Version)
	if err != nil {
		out.Notes = append(out.Notes, fmt.Sprintf("this machine's SBOM index of release %s could not be read: %v", run.Version, err))
		idx = nil
	}
	names := map[string]bool{}
	for name := range run.Artifacts {
		names[name] = true
	}
	if run.Artifacts == nil && idx != nil {
		for name := range idx.Artifacts {
			names[name] = true
		}
	}
	for name := range run.Overrides {
		names[name] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	var meta *sbomMetadataIndex
	for _, name := range sorted {
		a := planSBOMPush(name, run, idx, store, &meta)
		if a.Status == "" && a.ref != nil {
			if out.Unimplemented || out.Refused != nil || out.NotFound {
				a.Status, a.Reason = sbomPushFailed, "not sent: the control plane refused the release's earlier SBOMs"
			} else {
				sendPlanned(ctx, up, store, run, &a, &out)
			}
		}
		out.Artifacts = append(out.Artifacts, a)
	}
	recordSBOMPush(store, up.endpoint, run, out)
	return out
}

func sendPlanned(ctx context.Context, up sbomUploader, store sbomLedger, run sbomPushRun, a *sbomPushArtifact, out *sbomPushOutcome) {
	data, err := store.GetSBOM(a.ref.Digest)
	if err != nil {
		a.Status = sbomPushMissing
		if errors.Is(err, ledgerfile.ErrSBOMNotFound) {
			a.Reason = fmt.Sprintf("no SBOM %s in this machine's ledger", a.ref.Digest)
		} else {
			a.Reason = err.Error()
		}
		return
	}
	r := up.send(ctx, run.Version, run.Project, a.Artifact, *a.ref, data)
	a.Status, a.RecordedDigest = r.status, r.recorded
	switch {
	case r.unimplemented:
		out.Unimplemented = true
		a.Reason = "the control plane does not store SBOMs yet (RecordReleaseSBOM is Unimplemented)"
	case r.refused:
		out.Refused = r.err
		a.Reason = r.err.Error()
	case r.notFound:
		out.NotFound = true
		a.Reason = r.err.Error()
	case r.status == sbomPushDiffers && r.err != nil:
		a.Reason = "the control plane already records a different SBOM for it, and keeps it"
	case r.status == sbomPushDiffers:
		a.Reason = fmt.Sprintf("the control plane recorded it as %s: the two copies differ", r.recorded)
	case r.err != nil:
		a.Reason = r.err.Error()
	}
}

// planSBOMPush decides one artifact's SBOM and whether it is sent: a status
// already set means it is not.
func planSBOMPush(name string, run sbomPushRun, idx *ledgerfile.ReleaseSBOMIndex, store sbomLedger, meta **sbomMetadataIndex) sbomPushArtifact {
	a := sbomPushArtifact{Artifact: name}
	art, known := run.Artifacts[name]
	if run.Artifacts != nil && !known {
		a.Status, a.Reason = sbomPushFailed, "the release has no such artifact"
		return a
	}
	if known && !sbomDescribes(name, art) {
		a.Status, a.Reason = sbomPushNotApplicable, sbomNotApplicableReason(name, art)
		return a
	}
	remote := run.Remote[name]
	var unavailable string
	switch d, ok := run.Overrides[name]; {
	case ok:
		ref, err := overrideSBOMRef(store, name, d)
		if err != nil {
			a.Status, a.SBOMDigest, a.From, a.Reason = sbomPushMissing, d, sbomFromOverride, err.Error()
			return a
		}
		a.ref, a.From = ref, sbomFromOverride
	case art.SBOM.HasSBOM():
		ref := *art.SBOM
		a.ref, a.From = &ref, sbomFromRelease
	case remote != "":
		a.Status, a.SBOMDigest, a.RecordedDigest, a.From = sbomPushAlready, remote, remote, sbomFromControlPlane
		return a
	case idx != nil && idx.Artifacts[name].Digest != "":
		ref := idx.Artifacts[name]
		a.ref, a.From = &ref, sbomFromIndex
	default:
		if art.SBOM != nil && art.SBOM.Status == release.SBOMStatusUnavailable {
			unavailable = art.SBOM.Reason
		} else if idx != nil && idx.Artifacts[name].Status == release.SBOMStatusUnavailable {
			unavailable = idx.Artifacts[name].Reason
		}
	}
	if a.ref == nil && unavailable != "" {
		a.Status, a.Reason = sbomPushMissing, "the cut recorded no SBOM for it: "+unavailable
		return a
	}
	if a.ref == nil && run.Fallback && known {
		if *meta == nil {
			*meta = buildSBOMMetadataIndex(store)
		}
		ref, candidates := (*meta).match(name, art)
		if ref != nil {
			a.ref, a.From = ref, sbomFromMetadata
		} else {
			a.Candidates = candidates
		}
	}
	if a.ref == nil {
		a.Status, a.NeedsOverride = sbomPushMissing, true
		switch {
		case len(a.Candidates) > 0:
			a.Reason = fmt.Sprintf("%d SBOM(s) in this machine's ledger name it, but none records its digest (a static site's SBOM records its lockfile's): name the one to send with --sbom %s=<digest>", len(a.Candidates), name)
		default:
			a.Reason = "no SBOM in this machine's ledger describes it (cut on another machine, or with --no-sbom)"
		}
		return a
	}
	a.SBOMDigest = a.ref.Digest
	if remote != "" {
		a.RecordedDigest = remote
		if remote == a.ref.Digest {
			a.Status = sbomPushAlready
		} else {
			a.Status, a.Reason = sbomPushDiffers, "the control plane already records a different SBOM for it, and keeps it"
		}
	}
	return a
}

// sbomNotApplicableReason says why an artifact has no SBOM to send
// (sbomDescribes is false).
func sbomNotApplicableReason(name string, art release.Artifact) string {
	if _, maps := release.SourcemapsFrontend(name); maps {
		return "source maps: debugging data, not code that runs"
	}
	if art.Kind == release.KindGit {
		return "a git source pin: it names a commit, not bytes that ship"
	}
	return fmt.Sprintf("a %s artifact, which an SBOM does not describe", art.Kind)
}

// overrideSBOMRef is the ref of an SBOM the operator named with --sbom: the
// bytes are this machine's ledger's, verified against the digest, and a
// document that names a different component than the artifact is refused
// (a typo in a 64-character digest would otherwise file one artifact's
// SBOM under another for good).
func overrideSBOMRef(store sbomLedger, name, digest string) (*release.SBOMRef, error) {
	if !release.ValidDigest(digest) {
		return nil, fmt.Errorf("--sbom %s=%s: not a sha256:<hex> digest", name, digest)
	}
	data, err := store.GetSBOM(digest)
	if err != nil {
		return nil, fmt.Errorf("--sbom %s=%s: %v", name, digest, err)
	}
	doc, err := sbom.Identify(data)
	if err != nil {
		return nil, fmt.Errorf("--sbom %s=%s: %v", name, digest, err)
	}
	if c, ok := cycloneDXComponent(data); ok && c.Name != "" && c.Name != name {
		return nil, fmt.Errorf("--sbom %s=%s: that SBOM describes %q, not %s", name, digest, c.Name, name)
	}
	ref := &release.SBOMRef{Digest: digest, Status: release.SBOMStatusOK, Format: doc.Format, Generator: doc.Generator}
	return ref, nil
}

// ─── Matching SBOM files by their metadata ───────────────────────────────────

// sbomHeadBytes is how much of an SBOM file is read to find its metadata:
// syft writes metadata (tools, component) before the components list, in
// well under this.
const sbomHeadBytes = 256 << 10

// cdxComponent is a CycloneDX document's metadata.component, the subject it
// describes: forge's generator writes the artifact's release key as the
// name, and the digest it read (the image's, for an image) as the version.
type cdxComponent struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// cycloneDXComponent reads metadata.component from the leading bytes of a
// CycloneDX JSON document, decoding nothing past metadata. false when the
// document holds none in data (or is not CycloneDX JSON).
func cycloneDXComponent(data []byte) (cdxComponent, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return cdxComponent{}, false
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return cdxComponent{}, false
		}
		key, _ := t.(string)
		if key != "metadata" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return cdxComponent{}, false
			}
			continue
		}
		var meta struct {
			Component *cdxComponent `json:"component"`
		}
		if err := dec.Decode(&meta); err != nil || meta.Component == nil {
			return cdxComponent{}, false
		}
		return *meta.Component, true
	}
	return cdxComponent{}, false
}

// sbomMetadataIndex is every SBOM in the machine ledger by the component it
// names, newest file first.
type sbomMetadataIndex struct {
	store  sbomLedger
	byName map[string][]sbomMetadataEntry
}

type sbomMetadataEntry struct {
	digest  string
	version string
}

func buildSBOMMetadataIndex(store sbomLedger) *sbomMetadataIndex {
	m := &sbomMetadataIndex{store: store, byName: map[string][]sbomMetadataEntry{}}
	files, err := store.SBOMs()
	if err != nil {
		return m
	}
	for _, f := range files {
		head, err := store.SBOMHead(f.Digest, sbomHeadBytes)
		if err != nil {
			continue
		}
		c, ok := cycloneDXComponent(head)
		if !ok && int64(len(head)) < f.Size {
			// metadata past the head: read the whole document.
			if data, err := store.GetSBOM(f.Digest); err == nil {
				c, ok = cycloneDXComponent(data)
			}
		}
		if !ok || c.Name == "" {
			continue
		}
		m.byName[c.Name] = append(m.byName[c.Name], sbomMetadataEntry{digest: f.Digest, version: c.Version})
	}
	return m
}

// match is the SBOM that provably describes art: one whose component is the
// artifact's key, at the artifact's one digest (an image's SBOM records the
// image digest it read). Several such files are the same subject described
// more than once (another syft, another day); the newest is taken, as the
// next cut would take it. With none, candidates are the files that name the
// artifact at another version (a static site's lockfile digest).
func (m *sbomMetadataIndex) match(name string, art release.Artifact) (*release.SBOMRef, []string) {
	entries := m.byName[name]
	if digest, ok := art.SharedDigest(); ok {
		for _, e := range entries {
			if e.version != digest {
				continue
			}
			data, err := m.store.GetSBOM(e.digest)
			if err != nil {
				continue
			}
			ref := &release.SBOMRef{Digest: e.digest, Status: release.SBOMStatusOK}
			if doc, err := sbom.Identify(data); err == nil {
				ref.Format, ref.Generator = doc.Format, doc.Generator
			}
			return ref, nil
		}
	}
	var candidates []string
	for _, e := range entries {
		candidates = append(candidates, e.digest)
	}
	return nil, candidates
}

// ─── The index ───────────────────────────────────────────────────────────────

// recordSBOMIndex writes r's SBOM refs to the machine ledger's index of r:
// what a later `forge vuln sbom push`, or a drain on the next cut, sends.
// Artifacts without a ref (no SBOM step, or one that describes no code) are
// left out.
func recordSBOMIndex(store sbomLedger, r release.Release) error {
	refs := map[string]release.SBOMRef{}
	for name, a := range r.Artifacts {
		if a.SBOM != nil {
			refs[name] = *a.SBOM
		}
	}
	if len(refs) == 0 {
		return nil
	}
	return store.UpdateReleaseSBOMIndex(r.Version, func(x *ledgerfile.ReleaseSBOMIndex) error {
		if x.Project == "" {
			x.Project = r.Project
		}
		if x.CutAt.IsZero() {
			x.CutAt = r.CreatedAt
		}
		if x.Artifacts == nil {
			x.Artifacts = map[string]release.SBOMRef{}
		}
		for name, ref := range refs {
			x.Artifacts[name] = ref
		}
		return nil
	})
}

// writeReleaseSBOMIndex is recordSBOMIndex into the checkout's machine
// ledger, for a cut whatever ledger the env records releases in. A failure
// warns: the index is how a refused upload is re-sent later, not part of
// the release.
func writeReleaseSBOMIndex(projectDir string, r release.Release, warn io.Writer) {
	has := false
	for _, a := range r.Artifacts {
		has = has || a.SBOM != nil
	}
	if !has || projectDir == "" {
		return
	}
	store, err := openMachineLedger(projectDir)
	if err == nil {
		err = recordSBOMIndex(store, r)
	}
	if err != nil && !cloud.ClaimableActive() {
		fmt.Fprintf(warn, "[build] warning: release %s: could not record which SBOM describes each artifact in this machine's ledger: %v\n", r.Version, err)
	}
}

// recordSBOMPush records a push's outcome on endpoint in the release's
// index: the refs it used (so a release matched by metadata or --sbom need
// not be matched again), and whether anything is left to send.
func recordSBOMPush(store sbomLedger, endpoint string, run sbomPushRun, out sbomPushOutcome) {
	state, reason := ledgerfile.SBOMUploadDone, ""
	switch {
	case out.NotFound:
		state, reason = ledgerfile.SBOMUploadAbandoned, "the control plane does not know the release"
	case out.Unimplemented:
		state, reason = ledgerfile.SBOMUploadPending, "the control plane does not store SBOMs yet"
	case out.Refused != nil:
		state, reason = ledgerfile.SBOMUploadPending, "refused: "+out.Refused.Error()
	case out.pending():
		state, reason = ledgerfile.SBOMUploadPending, "some SBOMs failed to upload"
	}
	setSBOMUploadState(store, endpoint, run, out.Artifacts, state, reason)
}

// setSBOMUploadState writes one release's upload state on endpoint. Best
// effort: the ledger refusing the write loses only a re-send hint.
func setSBOMUploadState(store sbomLedger, endpoint string, run sbomPushRun, artifacts []sbomPushArtifact, state, reason string) {
	_ = store.UpdateReleaseSBOMIndex(run.Version, func(x *ledgerfile.ReleaseSBOMIndex) error {
		if x.Project == "" {
			x.Project = run.Project
		}
		if x.CutAt.IsZero() {
			x.CutAt = run.CutAt
		}
		if x.Artifacts == nil {
			x.Artifacts = map[string]release.SBOMRef{}
		}
		for name, a := range run.Artifacts {
			if _, ok := x.Artifacts[name]; !ok && a.SBOM != nil {
				x.Artifacts[name] = *a.SBOM
			}
		}
		statuses := map[string]string{}
		for _, a := range artifacts {
			if a.Status == sbomPushNotApplicable {
				continue
			}
			statuses[a.Artifact] = a.Status
			if a.ref != nil && (a.Status == sbomPushSent || a.Status == sbomPushAlready) {
				x.Artifacts[a.Artifact] = *a.ref
			}
		}
		if x.Uploads == nil {
			x.Uploads = map[string]ledgerfile.SBOMUpload{}
		}
		prev := x.Uploads[endpoint]
		next := ledgerfile.SBOMUpload{State: state, Reason: reason, UpdatedAt: time.Now().UTC().Truncate(time.Second), Artifacts: statuses}
		if len(statuses) == 0 {
			next.Artifacts = prev.Artifacts
		}
		if state == ledgerfile.SBOMUploadPending {
			next.Attempts = prev.Attempts + 1
		}
		x.Uploads[endpoint] = next
		return nil
	})
}

// sbomPushCounts tallies a push's outcomes.
type sbomPushCounts struct {
	Sent          int `json:"sent"`
	Already       int `json:"already_recorded"`
	Differs       int `json:"differs"`
	Missing       int `json:"missing"`
	Failed        int `json:"failed"`
	NotApplicable int `json:"not_applicable"`
}

func countSBOMPush(artifacts []sbomPushArtifact) sbomPushCounts {
	var c sbomPushCounts
	for _, a := range artifacts {
		switch a.Status {
		case sbomPushSent:
			c.Sent++
		case sbomPushAlready:
			c.Already++
		case sbomPushDiffers:
			c.Differs++
		case sbomPushMissing:
			c.Missing++
		case sbomPushFailed:
			c.Failed++
		case sbomPushNotApplicable:
			c.NotApplicable++
		}
	}
	return c
}

// sbomResendHint is the command that re-sends a release's SBOMs.
func sbomResendHint(version string) string {
	return "forge vuln sbom push --release " + version
}
