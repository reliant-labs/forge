package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Facts are what a forge command ALREADY knows about the local caches it just
// used: the project it ran in, the k3d contexts the env declares, the local
// registry container those contexts pull from, that registry's host aliases,
// the repositories this build pushes, and the project's release pins.
//
// Every field is optional. Converge records what it is given and leaves the
// rest of the policy alone — a cluster phase knows contexts and aliases but no
// repositories, a build knows all five. That is why activation does not need a
// command of its own: the facts arrive from whichever touch point happens to
// run, and accumulate.
type Facts struct {
	// Project is the project directory whose rotated logs forge may expire.
	// Stored absolute.
	Project string
	// Contexts are kubectl contexts whose workloads protect images in the
	// registry. Only local `k3d-` contexts are accepted: retention reads the
	// protected set from them, and a remote cluster forge cannot list would
	// make deletion unsafe.
	Contexts []string
	// Registry is the local registry's CONTAINER name (`k3d-…`), which is
	// also how `forge storage gc` addresses it.
	Registry string
	// Aliases are every host name under which that one registry is pushed to
	// or pulled from — the containerd mirror keys plus the in-network
	// `<container>:5000`. A repository is attributed to this registry only
	// when its host is one of these.
	Aliases []string
	// Repositories are full image references, registry host included. The
	// host is matched against Aliases and then stripped: retention works in
	// repository paths, as the registry API does.
	Repositories []string
	// Pins are image digests that must survive retention regardless of age.
	Pins []string
}

// Converge upserts Facts into the machine storage policy, under the same lock
// every other policy writer takes.
//
// Three properties make this safe to call from a hot path, on every run:
//
//   - IDEMPOTENT. Converging the same facts twice leaves the file untouched
//     (it is not even rewritten), so warm runs cost one read.
//   - ADDITIVE ONLY. Nothing is ever removed. Several projects on one machine
//     converge into the same policy, and a project that stops using a context
//     must not silently un-protect it for everybody else.
//   - COMPLETE-OR-NOTHING for a registry. A registry becomes a cleanup target
//     only once the container, its aliases, the protecting contexts and at
//     least one repository are all known. Recording a registry whose
//     protecting contexts were still unknown would authorize deletion against
//     an unknown protected set, which is the one failure mode retention must
//     never have. Partial facts still converge their project/context/pin half.
func Converge(policyPath string, f Facts) error {
	return WithLock(policyPath, func() error {
		p, err := Load(policyPath)
		if err != nil {
			return err
		}
		next, changed := upsertFacts(p, f)
		if !changed {
			return nil
		}
		if err := Save(policyPath, next); err != nil {
			return fmt.Errorf("converge local storage ownership: %w", err)
		}
		return nil
	})
}

// upsertFacts is the pure half of Converge: policy + facts -> policy, plus
// whether anything actually changed. Register (which DISCOVERS the same facts
// from live docker metadata instead of being told them) applies its findings
// through this function too, so there is one merge rule rather than two.
func upsertFacts(p Policy, f Facts) (Policy, bool) {
	before := p.fingerprint()

	if f.Project != "" {
		if absolute, err := filepath.Abs(f.Project); err == nil && !contains(p.Projects, absolute) {
			p.Projects = append(p.Projects, absolute)
		}
	}
	for _, c := range f.Contexts {
		if isLocalK3d(c) && !contains(p.Clusters, c) {
			p.Clusters = append(p.Clusters, c)
		}
	}
	for _, pin := range f.Pins {
		if pin != "" && !contains(p.Pins, pin) {
			p.Pins = append(p.Pins, pin)
		}
	}
	p.Registries = upsertRegistry(p.Registries, p.Clusters, f)

	return p, p.fingerprint() != before
}

// upsertRegistry merges one registry's facts into the registered set.
//
// The contexts recorded are the union with EVERY registered local context, not
// just the ones these facts named. Retention protects an image when some
// registered context still references it, so a context missing from the list is
// an image deleted out from under a running cluster, while a context listed
// that never pulls from this registry only ever widens the protected set. The
// asymmetry is deliberate: over-protection wastes disk, under-protection breaks
// a cluster.
func upsertRegistry(registries []Registry, allContexts []string, f Facts) []Registry {
	if !isLocalK3d(f.Registry) {
		return registries
	}
	index := -1
	reg := Registry{Container: f.Registry}
	for i, old := range registries {
		if old.Container == f.Registry {
			index, reg = i, old
			break
		}
	}
	for _, alias := range f.Aliases {
		if alias != "" && !contains(reg.Aliases, alias) {
			reg.Aliases = append(reg.Aliases, alias)
		}
	}
	for _, c := range allContexts {
		if isLocalK3d(c) && !contains(reg.Contexts, c) {
			reg.Contexts = append(reg.Contexts, c)
		}
	}
	for _, repository := range f.Repositories {
		host, path, ok := splitRepository(repository)
		if ok && contains(reg.Aliases, host) && !contains(reg.Repositories, path) {
			reg.Repositories = append(reg.Repositories, path)
		}
	}
	// The completeness gate. An entry that cannot yet be acted on safely is
	// not written at all, so the policy never describes a half-known target.
	if len(reg.Aliases) == 0 || len(reg.Contexts) == 0 || len(reg.Repositories) == 0 {
		return registries
	}
	if index >= 0 {
		registries[index] = reg
		return registries
	}
	return append(registries, reg)
}

// splitRepository separates an image reference's registry host from its
// repository path. Only a first segment that looks like a host:port — which is
// what every local registry alias is — counts, so `library/app` is not read as
// host `library`.
func splitRepository(repository string) (host, path string, ok bool) {
	host, path, found := strings.Cut(repository, "/")
	if !found || host == "" || path == "" {
		return "", "", false
	}
	if strings.Index(host, ":") <= 0 {
		return "", "", false
	}
	return host, path, true
}

// isLocalK3d gates both contexts and container names on the `k3d-` prefix
// Policy.Validate also enforces, so Converge can never write an entry Save
// would reject.
func isLocalK3d(name string) bool { return strings.HasPrefix(name, "k3d-") && len(name) > len("k3d-") }

// fingerprint is a stable rendering of everything Converge can change, so
// "did this converge alter the policy" is one comparison rather than a
// field-by-field diff that a new field could silently fall out of.
func (p Policy) fingerprint() string {
	type registryPrint struct {
		Container                   string
		Repositories, Aliases, Ctxs []string
	}
	snapshot := struct {
		Projects, Clusters, Pins []string
		Registries               []registryPrint
	}{Projects: sorted(p.Projects), Clusters: sorted(p.Clusters), Pins: sorted(p.Pins)}
	for _, r := range p.Registries {
		snapshot.Registries = append(snapshot.Registries, registryPrint{
			Container: r.Container, Repositories: sorted(r.Repositories),
			Aliases: sorted(r.Aliases), Ctxs: sorted(r.Contexts),
		})
	}
	sort.Slice(snapshot.Registries, func(i, j int) bool {
		return snapshot.Registries[i].Container < snapshot.Registries[j].Container
	})
	b, _ := json.Marshal(snapshot)
	return string(b)
}

func sorted(xs []string) []string {
	out := append([]string(nil), xs...)
	sort.Strings(out)
	return out
}

// gcStamp records when a maintenance pass last ran to completion. It lives
// beside the policy rather than inside it so recording a run never rewrites
// (or risks invalidating) the policy document itself.
type gcStamp struct {
	CompletedAt time.Time `json:"completed_at"`
}

// GCStampPath is the completion marker's location, next to the policy file.
func GCStampPath(policyPath string) string {
	return filepath.Join(filepath.Dir(policyPath), "last-gc.json")
}

// LastGC reports when maintenance last completed. A missing or unreadable
// stamp is the zero time with no error: "we do not know" and "it was never
// run" lead to the same decision, and a corrupt marker must not be able to
// fail a user's command.
func LastGC(policyPath string) time.Time {
	b, err := os.ReadFile(GCStampPath(policyPath))
	if err != nil {
		return time.Time{}
	}
	var stamp gcStamp
	if json.Unmarshal(b, &stamp) != nil {
		return time.Time{}
	}
	return stamp.CompletedAt
}

// RecordGC writes the completion marker atomically.
func RecordGC(policyPath string, at time.Time) error {
	path := GCStampPath(policyPath)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(gcStamp{CompletedAt: at})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".last-gc-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// NonDisruptiveGC runs only the layers that cannot interrupt anything a
// developer is currently using: expiring rotated logs, evicting unused builder
// cache, and the temp sweep.
//
// Registry GC and node reconfiguration are deliberately NOT here. Registry
// cleanup takes the registry offline for the duration (pushes and pulls fail),
// and node reconfiguration restarts kubelet — both are fine for a scheduled
// 03:30 pass and unacceptable as a side effect of `forge env up`, which is
// typically the command that is about to push to that registry.
func (r Runner) NonDisruptiveGC(ctx context.Context, apply bool) error {
	if err := r.Policy.Validate(); err != nil {
		return err
	}
	if err := r.Logs(apply); err != nil {
		return err
	}
	if err := r.Local(ctx); err != nil {
		return err
	}
	var failures []error
	for _, builder := range r.Policy.Builders {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if err := r.builderGC(ctx, builder, apply); err != nil {
			failures = append(failures, fmt.Errorf("builder %s: %w", builder, err))
		}
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(append(failures, err)...)
	}
	// The temp sweep touches only $TMPDIR scratch under a fixed allowlist of
	// known-dead prefixes, so it is safe alongside a running stack.
	if err := r.TempSweep(apply); err != nil {
		failures = append(failures, fmt.Errorf("temp sweep: %w", err))
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(append(failures, err)...)
	}
	// Source eviction belongs here for the same reason the temp sweep does,
	// and it is the layer with the most to reclaim: 9.2 GB of one repository
	// on the machine this was written for. It cannot interrupt a running
	// stack. Resolve touches an entry's metadata on every cache HIT, so
	// anything a build is using now is minutes old and cannot be in a set
	// whose youngest member is SourceCacheUnused (14 days) stale; the
	// per-repository keep floor retains an idle project's current pin
	// regardless of age; and an entry a live process holds open is detected
	// and retained. The worst case for being wrong is one re-clone of a
	// re-fetchable pin, which is the same cost shape as a pruned build cache
	// — not the offline registry or the restarted kubelet this pass excludes.
	if err := r.Sources(apply); err != nil {
		failures = append(failures, fmt.Errorf("source cache: %w", err))
	}
	return errors.Join(failures...)
}
