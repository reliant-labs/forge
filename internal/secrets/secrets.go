// Package secrets resolves declared secret REFERENCES (which live in
// git: EnvVar.secret_ref / secret_key) to secret VALUES (which never
// live in git) for one environment.
//
// A secret has two halves:
//
//   - a non-sensitive REFERENCE — the env-var NAME, the k8s Secret name,
//     and the key within it. KCL projects this into the manifest as a
//     `secretKeyRef`. It is reproducible and version-controlled.
//   - a sensitive VALUE — obtained at resolve time from a per-env
//     PROVIDER. Never in git, never in KCL render output.
//
// This package owns the VALUE side. KCL only emits the provider
// DECLARATION (type + path); all value resolution happens here in Go so
// secrets never enter the KCL renderer.
//
// Two provider kinds:
//
//   - "dotenv" (dev/local): forge reads a gitignored dotenv keyed by
//     env-var NAME, resolves declared refs from it, and — for k8s
//     targets — RENDERS Secret objects from it CLI-side. Local clusters
//     only.
//   - "external" (prod/staging): forge never sees values. k8s references
//     pre-existing Secrets (External Secrets Operator / sealed); host &
//     external runtimes obtain secrets via workload identity / ambient
//     env. forge only validates the secretKeyRef wiring (it can't, and
//     so does not, validate the values themselves).
//
// The package is intentionally decoupled from internal/cli to avoid an
// import cycle (cli depends on secrets, not the reverse). It reuses
// internal/hostlaunch's dotenv reader, which only imports the stdlib —
// no cycle risk.
package secrets

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/envutil"
)

// Provider resolves declared secret references to values for one env.
type Provider interface {
	Kind() string // "dotenv" | "external" | "none"
	// Resolve returns the value for an env var by NAME (dotenv-key
	// convention: the key in the dotenv == the EnvVar.name). ok=false
	// when this provider has no value for name.
	Resolve(name string) (value string, ok bool)
	// All returns every value the provider can supply, keyed by name.
	// dotenv: the whole file. external/none: nil.
	All() map[string]string
}

// ProviderConfig is the cli-decoupled view of the KCL secret_provider
// entity. (cli maps KCLEntities.SecretProvider -> this.)
type ProviderConfig struct {
	Type string // "dotenv" | "external"
	Path string // dotenv path (already resolved to an absolute/project path by caller)
}

// NewProvider builds a Provider. cfg==nil -> a noop provider (Kind
// "none", All nil, Resolve always !ok) so callers need no nil checks.
// dotenv: loads the file now; a MISSING dotenv file is a non-fatal
// empty provider with a returned error==nil but Kind "dotenv" and empty
// All (so validation, not load, reports missing declared keys) — BUT if
// the file exists and is unreadable/malformed, return the error.
func NewProvider(cfg *ProviderConfig) (Provider, error) {
	if cfg == nil {
		return noopProvider{}, nil
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Type)) {
	case "external":
		return externalProvider{}, nil
	case "dotenv":
		values, err := envutil.ParseDotEnv(cfg.Path)
		if err != nil {
			// A missing file is non-fatal: an empty dotenv provider so
			// ValidateDeclaredRefs (not load) reports the missing keys
			// with an actionable message. Any OTHER error (permissions,
			// a directory in place of a file, etc.) is fatal.
			if errors.Is(err, os.ErrNotExist) {
				return dotenvProvider{values: map[string]string{}, path: cfg.Path}, nil
			}
			return nil, fmt.Errorf("load dotenv %q: %w", cfg.Path, err)
		}
		if values == nil {
			values = map[string]string{}
		}
		return dotenvProvider{values: values, path: cfg.Path}, nil
	case "", "none":
		return noopProvider{}, nil
	default:
		return nil, fmt.Errorf("unknown secret provider type %q (expected \"dotenv\" or \"external\")", cfg.Type)
	}
}

// noopProvider is the absent-provider case: nothing to resolve.
type noopProvider struct{}

func (noopProvider) Kind() string                  { return "none" }
func (noopProvider) Resolve(string) (string, bool) { return "", false }
func (noopProvider) All() map[string]string        { return nil }

// externalProvider declares values come from outside forge's view. forge
// validates wiring only; it never resolves a value.
type externalProvider struct{}

func (externalProvider) Kind() string                  { return "external" }
func (externalProvider) Resolve(string) (string, bool) { return "", false }
func (externalProvider) All() map[string]string        { return nil }

// dotenvProvider resolves values from a gitignored dotenv keyed by
// env-var NAME.
type dotenvProvider struct {
	values map[string]string
	path   string
}

func (d dotenvProvider) Kind() string { return "dotenv" }

func (d dotenvProvider) Resolve(name string) (string, bool) {
	v, ok := d.values[name]
	return v, ok
}

func (d dotenvProvider) All() map[string]string { return d.values }

// SecretRef is a declared reference extracted from the entities: the
// env-var NAME (== dotenv key), the k8s Secret NAME, and the key within
// that Secret. SecretKey defaults to EnvName when empty (matches the KCL
// _env_source lambda).
type SecretRef struct {
	EnvName    string
	SecretName string
	SecretKey  string
}

// key returns the in-Secret key, defaulting to EnvName when unset.
func (r SecretRef) key() string {
	if r.SecretKey != "" {
		return r.SecretKey
	}
	return r.EnvName
}

// ValidateDeclaredRefs returns a single fail-fast error listing every
// declared ref the provider cannot supply. For Kind "dotenv": each
// EnvName must be present in All(). For "external"/"none": returns nil
// (forge cannot see those values).
func ValidateDeclaredRefs(p Provider, refs []SecretRef, dotenvPath string) error {
	if p == nil || p.Kind() != "dotenv" {
		return nil
	}
	values := p.All()
	var missing []SecretRef
	for _, r := range refs {
		if r.EnvName == "" {
			continue
		}
		if _, ok := values[r.EnvName]; !ok {
			missing = append(missing, r)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	// Deterministic ordering so the error message is stable across runs.
	sort.Slice(missing, func(i, j int) bool { return missing[i].EnvName < missing[j].EnvName })

	// Column-align the env names for a readable list.
	width := 0
	for _, r := range missing {
		if len(r.EnvName) > width {
			width = len(r.EnvName)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "secret provider \"dotenv\" (path %s) is missing %d declared value(s):\n", dotenvPath, len(missing))
	for _, r := range missing {
		fmt.Fprintf(&b, "    %-*s   (Secret %s/%s)\n", width, r.EnvName, r.SecretName, r.key())
	}
	fmt.Fprintf(&b, "fix: add them to %s, or remove the secret_ref from the EnvVar.", dotenvPath)
	return errors.New(b.String())
}

// RenderK8sSecrets builds k8s Secret manifests (as []map[string]any,
// ready to marshal to YAML/JSON) from the resolved values, grouping refs
// by SecretName; each Secret's stringData[SecretKey] = resolved(EnvName).
// Only Kind "dotenv" produces output; "external"/"none" return nil
// (prod references pre-existing Secrets). Skips refs whose value doesn't
// resolve (ValidateDeclaredRefs is the gate for those). Deterministic
// ordering (sorted Secret names + keys) for stable diffs.
func RenderK8sSecrets(p Provider, refs []SecretRef, namespace string) []map[string]any {
	if p == nil || p.Kind() != "dotenv" {
		return nil
	}
	// Group resolved (key -> value) pairs by Secret name.
	grouped := map[string]map[string]string{}
	for _, r := range refs {
		if r.SecretName == "" {
			continue
		}
		value, ok := p.Resolve(r.EnvName)
		if !ok {
			continue // ValidateDeclaredRefs is the gate; skip unresolved.
		}
		if grouped[r.SecretName] == nil {
			grouped[r.SecretName] = map[string]string{}
		}
		grouped[r.SecretName][r.key()] = value
	}
	if len(grouped) == 0 {
		return nil
	}
	// Sort Secret names for stable output.
	names := make([]string, 0, len(grouped))
	for name := range grouped {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		// stringData as map[string]any with sorted keys. Go marshals
		// maps with sorted keys for JSON, and the kcl/yaml emitters used
		// downstream sort too, so building a plain map is deterministic.
		sd := map[string]any{}
		for k, v := range grouped[name] {
			sd[k] = v
		}
		out = append(out, map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
			},
			"type":       "Opaque",
			"stringData": sd,
		})
	}
	return out
}
