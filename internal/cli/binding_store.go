package cli

// The env→release promotion ledger, behind a seam.
//
// WHY A SEAM AT ALL. Promotion state answers "which release does prod run",
// and a file inside the repo that PRODUCED the artifact is circular by
// construction: the commit recording "prod runs v1.5.13" cannot be in
// v1.5.13, because it is written after v1.5.13 was cut. The way out is for
// the ledger to be able to live somewhere that is not the artifact's own
// source tree — the hosted control plane — which means the callers must not
// know that a ledger is a file.
//
// THE FILE BACKEND IS THE DEFAULT, FOREVER. Not a stepping stone. forge with
// no account, on a plane, must stay fully functional: `forge env promote` and
// `forge env deploy` are core verbs, and a core verb that degrades without a
// login is a product that lied about being local-first.
//
// ONE MODEL, TWO BACKENDS. Both read and write forge/pkg/release types and
// both apply release.Decide, so "is this a no-op retry", "is this rollback
// legal" and "what does the env run now" have one answer whichever backend
// holds the ledger. See pkg/release's package doc.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/internal/statefile"
	"github.com/reliant-labs/forge/pkg/release"
)

// bindingStore is the promotion ledger as its CONSUMERS need it: what one env
// runs now, append one promotion, and say where the answer came from.
//
// Declared here at the consumer rather than exported from an implementation
// package, per the package-boundary rule the repo follows for
// clusterImageLister and envTargetResolver in env_verify.go.
//
// THERE IS NO "SET". The ledger is append-only: an environment's current
// binding is its most recent entry, and there is no pointer beside the
// history that could disagree with it. A rollback is a NEW entry of kind
// rollback, never an edit of an old one.
//
// NO projectDir ANYWHERE IN THIS INTERFACE. A project directory is a FILE
// concept; the hosted backend has none. The backing is bound ONCE at
// construction and the methods speak only in domain terms.
type bindingStore interface {
	// Current returns env's most recent promotion, and whether it has one.
	// "Never promoted" is a normal state, so it is a bool rather than an
	// error — distinct from a ledger that could not be read at all.
	Current(ctx context.Context, env string) (release.Promotion, bool, error)

	// Append records p (which must name p.Env, p.Release and p.Kind) under
	// release.Decide's rules and returns the entry the ledger now holds:
	// the newly appended one, or — for a retry of the current state — the
	// EXISTING entry, unchanged. A rollback to a release the env never ran
	// is release.ErrNeverPromoted. The backend stamps ID and PromotedAt.
	Append(ctx context.Context, p release.Promotion) (release.Promotion, error)

	// Location names where promotions are recorded, for human-facing
	// output — a directory for the file backend, the endpoint URL for a
	// hosted one. Opaque: callers must only PRINT it.
	Location() string
}

// releaseLedger is the release half of the same backend: cut a release, read
// one back, list them. Separate from bindingStore rather than widening it,
// because the two have different consumers (build/cut writes releases;
// promote/deploy/verify read promotions) and a six-method interface would be
// a concrete type wearing an interface.
type releaseLedger interface {
	// Cut records r. created is false when an identical release already
	// held the version (a retry); a DIFFERENT artifact set under the same
	// version is release.ErrReleaseConflict.
	Cut(ctx context.Context, r release.Release) (created bool, err error)
	// Get returns the release, or (nil, nil) when the version was never cut.
	Get(ctx context.Context, version string) (*release.Release, error)
	// List returns releases NEWEST FIRST.
	List(ctx context.Context) ([]release.Release, error)
	Location() string
}

// envLedger is one environment's backend, both halves. A concrete struct —
// accept interfaces, return structs — so a caller that needs only one half
// takes only that field.
type envLedger struct {
	Bindings bindingStore
	Releases releaseLedger
	// Hosted reports whether this env's ledger is a control plane. Carried
	// for output that must say so (the deploy banner), never for branching:
	// every behavioural difference lives inside the two implementations.
	Hosted bool
}

// ─── Selection ───────────────────────────────────────────────────────────────

// ledgerFor returns the ledger an environment uses. This is the SINGLE place
// the backend is chosen, and the choice is DECLARATIVE: an env whose KCL
// declares `forge.ControlPlane` uses that control plane's ledger; every other
// env uses the project's files. No flag, no context, no machine-local state —
// the same checkout resolves the same backend on every machine.
//
// A render FAILURE is an error, never a fallback to the file backend. For a
// hosted env that fallback would silently answer "never promoted", and a
// deploy would then ship mutable tags instead of the promoted digests — the
// exact failure the ledger exists to prevent.
func ledgerFor(ctx context.Context, projectDir, env string) (envLedger, error) {
	mainK := filepath.Join(projectDir, "deploy", "kcl", env, "main.k")
	if _, err := os.Stat(mainK); err != nil {
		// No KCL for this env in this checkout — nothing can declare a
		// control plane, so the answer is the project's own files. This
		// is how a test project, and an env named only on the command
		// line, keep working.
		return fileLedger(projectDir), nil
	}
	entities, err := RenderKCL(ctx, projectDir, env)
	if err != nil {
		return envLedger{}, fmt.Errorf("choose the release ledger for env %q: render deploy/kcl/%s: %w", env, env, err)
	}
	return ledgerForEntities(env, entities, projectDir)
}

// ledgerForEntities is the render-free half of ledgerFor, split out so the
// selection rule is testable from a literal entity.
func ledgerForEntities(env string, entities *KCLEntities, projectDir string) (envLedger, error) {
	decl := declarationFromEntities(entities)
	if decl == nil {
		return fileLedger(projectDir), nil
	}
	ep, err := cloud.ResolveEndpoint(env, decl)
	if err != nil {
		return envLedger{}, err
	}
	cred, err := cloud.ResolveCredential("", ep)
	if err != nil {
		return envLedger{}, fmt.Errorf("env %q keeps its release ledger on the control plane at %s: %w", env, ep.URL, err)
	}
	return hostedLedger(cloud.NewClient(ep, cred), ep.URL), nil
}

// bindingStoreFor is ledgerFor for the callers that need only the promotion
// half.
func bindingStoreFor(ctx context.Context, projectDir, env string) (bindingStore, error) {
	l, err := ledgerFor(ctx, projectDir, env)
	if err != nil {
		return nil, err
	}
	return l.Bindings, nil
}

func fileLedger(projectDir string) envLedger {
	return envLedger{
		Bindings: newFileBindingStore(projectDir),
		Releases: fileReleaseLedger{projectDir: projectDir},
	}
}

// ─── The file backend: promotions ────────────────────────────────────────────

// promotionsDirRel holds one append-only log per environment.
const promotionsDirRel = ".forge/promotions"

// legacyEnvReleasesRel is the retired single-file binding map. It is never
// READ as a ledger — there is no dual-read — but its presence is detected so
// a project that has not been converted fails loudly instead of reading as
// "never promoted" and deploying mutable tags.
const legacyEnvReleasesRel = ".forge/env-releases.json"

// fileBindingStore is the default backend: .forge/promotions/<env>.jsonl, one
// release.Promotion per line, appended with O_APPEND. The current binding is
// the last line.
//
// WHY A LOG AND NOT A MAP. The previous format was one JSON object holding
// every env's CURRENT binding, rewritten whole on every promote: history
// lived only in git, a rollback was indistinguishable from a promote, and two
// promotes of different envs raced a read-modify-write of one file. A log per
// env has no pointer that can disagree with its history, and an append never
// rewrites a neighbour's line.
//
// CONCURRENCY, STATED RATHER THAN PATCHED. One writer per env log is the
// supported case. A single write(2) of one line under O_APPEND is atomic on
// local filesystems for the line sizes a promotion produces, so two writers
// cannot interleave BYTES — but two writers can both decide from the same
// history and both append. That is documented, not locked around: a team with
// several concurrent promoters is exactly who the hosted backend's row lock
// is for.
type fileBindingStore struct {
	projectDir string
}

// newFileBindingStore binds a store to a project directory. Returns the
// concrete type: accept interfaces, return structs.
func newFileBindingStore(projectDir string) fileBindingStore {
	return fileBindingStore{projectDir: projectDir}
}

// promotionLogPath is the one env → file mapping. Uses the release stem rule
// so an env name can never escape the promotions directory.
func promotionLogPath(projectDir, env string) string {
	return filepath.Join(projectDir, promotionsDirRel, releaseFileStem(env)+".jsonl")
}

// errLegacyLedger reports a project still carrying env-releases.json.
func errLegacyLedger(projectDir string) error {
	return fmt.Errorf("%s is the retired binding format and is no longer read.\n"+
		"  Promotions now live in one append-only log per environment (%s/<env>.jsonl).\n"+
		"  Convert once with: forge release convert-ledger\n"+
		"  (reading it as \"never promoted\" instead would make the next deploy ship mutable tags "+
		"rather than the promoted digests)",
		filepath.Join(projectDir, legacyEnvReleasesRel), promotionsDirRel)
}

func (s fileBindingStore) checkNotLegacy() error {
	if _, err := os.Stat(filepath.Join(s.projectDir, legacyEnvReleasesRel)); err == nil {
		return errLegacyLedger(s.projectDir)
	}
	return nil
}

// History returns env's promotions OLDEST FIRST. A missing log is an empty
// history. Every line must decode and validate: a ledger with a line nobody
// can read is not a ledger whose last line can be trusted as "current".
func (s fileBindingStore) History(env string) ([]release.Promotion, error) {
	if err := s.checkNotLegacy(); err != nil {
		return nil, err
	}
	path := promotionLogPath(s.projectDir, env)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read promotion log %s: %w", path, err)
	}
	var history []release.Promotion
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	line := 0
	for scanner.Scan() {
		line++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var p release.Promotion
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("promotion log %s line %d: %w", path, line, err)
		}
		if err := p.Validate(); err != nil {
			return nil, fmt.Errorf("promotion log %s line %d: %w", path, line, err)
		}
		if p.Env != env {
			return nil, fmt.Errorf("promotion log %s line %d records env %q, not %q", path, line, p.Env, env)
		}
		history = append(history, p)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read promotion log %s: %w", path, err)
	}
	return history, nil
}

// Current is the last line of env's log.
func (s fileBindingStore) Current(_ context.Context, env string) (release.Promotion, bool, error) {
	history, err := s.History(env)
	if err != nil || len(history) == 0 {
		return release.Promotion{}, false, err
	}
	return history[len(history)-1], true, nil
}

// Append applies release.Decide to the env's history and, when the entry is a
// real move, appends ONE line with a single O_APPEND write.
func (s fileBindingStore) Append(_ context.Context, p release.Promotion) (release.Promotion, error) {
	history, err := s.History(p.Env)
	if err != nil {
		return release.Promotion{}, err
	}
	if err := p.Validate(); err != nil {
		return release.Promotion{}, err
	}
	existing, err := release.Decide(history, p)
	if err != nil {
		return release.Promotion{}, err
	}
	if existing != nil {
		return *existing, nil
	}
	p.ID = newPromotionID()
	p.PromotedAt = time.Now().UTC().Truncate(time.Second)
	line, err := json.Marshal(p)
	if err != nil {
		return release.Promotion{}, fmt.Errorf("encode promotion: %w", err)
	}
	return p, appendLine(promotionLogPath(s.projectDir, p.Env), line)
}

// Location is the promotions directory.
func (s fileBindingStore) Location() string {
	return filepath.Join(s.projectDir, promotionsDirRel)
}

// appendLine writes line+"\n" in ONE write under O_APPEND, so a concurrent
// appender can never split it.
func appendLine(path string, line []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644) //nolint:gosec // path is built from the project dir and a sanitized env stem
	if err != nil {
		return fmt.Errorf("open promotion log %s: %w", path, err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("append to promotion log %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close promotion log %s: %w", path, err)
	}
	return nil
}

// newPromotionID is a random, sortable-enough identifier for a file-ledger
// entry. The hosted ledger assigns its own; this only has to be unique
// within one project's logs.
func newPromotionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

// ─── The file backend: releases ──────────────────────────────────────────────

// fileReleaseLedger is .forge/releases/<version>.json.
type fileReleaseLedger struct {
	projectDir string
}

func (l fileReleaseLedger) Cut(_ context.Context, r release.Release) (bool, error) {
	existing, err := ReadRelease(l.projectDir, r.Version)
	if err != nil {
		return false, err
	}
	if err := WriteRelease(l.projectDir, r); err != nil {
		return false, err
	}
	return existing == nil, nil
}

func (l fileReleaseLedger) Get(_ context.Context, version string) (*release.Release, error) {
	return ReadRelease(l.projectDir, version)
}

func (l fileReleaseLedger) List(_ context.Context) ([]release.Release, error) {
	return readReleaseLedgers(l.projectDir), nil
}

func (l fileReleaseLedger) Location() string {
	return filepath.Join(l.projectDir, releasesDirRel)
}

// ─── One-time conversion from the retired format ─────────────────────────────

// convertLegacyLedger rewrites a project from the retired ledger format to the
// current one, once:
//
//   - every .forge/releases/*.json gains the now-required artifact `kind`
//     (oci for a shared image, git for a source-pinned frontend — the only
//     two things the old format could hold without a kind), and must then
//     satisfy release.Validate;
//   - .forge/env-releases.json becomes one promotion log per env, each
//     holding that env's binding as its first entry (kind promote, the
//     original promoted_at), and is then DELETED.
//
// History before the single binding each env held was never in the file —
// it lived in git — so it is not reconstructed. Returns a human summary.
func convertLegacyLedger(projectDir string) ([]string, error) {
	var done []string

	matches, err := filepath.Glob(filepath.Join(projectDir, releasesDirRel, "*.json"))
	if err != nil {
		return nil, err
	}
	for _, path := range matches {
		changed, err := convertLegacyReleaseFile(path)
		if err != nil {
			return done, err
		}
		if changed {
			done = append(done, "stamped artifact kinds in "+relOrAbs(projectDir, path))
		}
	}

	legacy := filepath.Join(projectDir, legacyEnvReleasesRel)
	data, err := os.ReadFile(legacy)
	if errors.Is(err, os.ErrNotExist) {
		return done, nil
	}
	if err != nil {
		return done, err
	}
	var old struct {
		Bindings map[string]struct {
			Release    string                    `json:"release"`
			Resolved   map[string]string         `json:"resolved"`
			Sources    map[string]release.Source `json:"sources,omitempty"`
			PromotedAt string                    `json:"promoted_at"`
		} `json:"bindings"`
	}
	if err := json.Unmarshal(data, &old); err != nil {
		return done, fmt.Errorf("parse %s: %w", legacy, err)
	}
	for env, b := range old.Bindings {
		logPath := promotionLogPath(projectDir, env)
		if _, err := os.Stat(logPath); err == nil {
			return done, fmt.Errorf("both %s and %s exist; refusing to guess which is current", legacy, logPath)
		}
		at, err := time.Parse(time.RFC3339, b.PromotedAt)
		if err != nil {
			return done, fmt.Errorf("%s: env %q promoted_at %q: %w", legacy, env, b.PromotedAt, err)
		}
		p := release.Promotion{
			ID:         at.UTC().Format("20060102T150405Z") + "-converted",
			Env:        env,
			Release:    b.Release,
			Kind:       release.KindPromote,
			Resolved:   b.Resolved,
			Sources:    b.Sources,
			PromotedBy: release.Actor{Actor: "forge-convert-ledger"},
			Note:       "converted from " + legacyEnvReleasesRel + "; earlier history is in git",
			PromotedAt: at.UTC(),
		}
		if p.Resolved == nil {
			p.Resolved = map[string]string{}
		}
		if err := p.Validate(); err != nil {
			return done, fmt.Errorf("%s: env %q: %w", legacy, env, err)
		}
		line, err := json.Marshal(p)
		if err != nil {
			return done, err
		}
		if err := appendLine(logPath, line); err != nil {
			return done, err
		}
		done = append(done, fmt.Sprintf("%s → %s (release %s)", env, relOrAbs(projectDir, logPath), b.Release))
	}
	if err := os.Remove(legacy); err != nil {
		return done, fmt.Errorf("remove %s: %w", legacy, err)
	}
	done = append(done, "removed "+legacyEnvReleasesRel)
	return done, nil
}

// convertLegacyReleaseFile stamps the kind every artifact now must carry.
// Reports whether the file changed. A file already in the current shape is
// left byte-for-byte alone.
func convertLegacyReleaseFile(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	var artifacts map[string]map[string]any
	if err := json.Unmarshal(doc["artifacts"], &artifacts); err != nil {
		return false, fmt.Errorf("parse %s artifacts: %w", path, err)
	}
	changed := false
	for name, a := range artifacts {
		if k, _ := a["kind"].(string); k != "" {
			continue
		}
		switch mode, _ := a["mode"].(string); mode {
		case "shared", "variant":
			a["kind"] = string(release.KindOCI)
		case "source":
			a["kind"] = string(release.KindGit)
		default:
			return false, fmt.Errorf("%s: artifact %q has no kind and mode %q — cannot infer one", path, name, mode)
		}
		changed = true
	}
	if !changed {
		return false, nil
	}
	raw, err := json.Marshal(artifacts)
	if err != nil {
		return false, err
	}
	doc["artifacts"] = raw
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return false, err
	}
	var rel release.Release
	if err := json.Unmarshal(out, &rel); err != nil {
		return false, fmt.Errorf("%s after conversion: %w", path, err)
	}
	if err := rel.Validate(); err != nil {
		return false, fmt.Errorf("%s after conversion: %w", path, err)
	}
	// Written through the type, so the file lands in the same canonical
	// field order a freshly cut release has — the diff a reviewer sees is
	// the added kinds, not a reshuffle.
	return true, statefile.Write(path, "release", rel)
}

func relOrAbs(base, path string) string {
	if rel, err := filepath.Rel(base, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}
