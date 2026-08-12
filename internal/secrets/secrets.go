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
// Provider kinds:
//
//   - "file" (dev/local): forge reads a gitignored YAML file — a flat map
//     of env-var name to value. Local clusters only. This is the
//     scaffolded default; see fileProvider for why it replaced the dotenv.
//   - "dotenv": REMOVED. It injected the whole file into every host
//     service, so values never had to be declared. NewProvider returns a
//     hard error naming `forge secret migrate`.
//   - "external" (prod/staging): forge never sees values. k8s references
//     pre-existing Secrets (External Secrets Operator / sealed); host &
//     external runtimes obtain secrets via workload identity / ambient
//     env. forge only validates the secretKeyRef wiring (it can't, and
//     so does not, validate the values themselves).
//
// The package is intentionally decoupled from internal/cli to avoid an
// import cycle (cli depends on secrets, not the reverse). It reuses
// only the stdlib for its own file reads — no cycle risk.
//
// forge:exclude-contract
// secrets is a secret-reference→value resolution utility (per-env dir /
// external providers), not a contract-shaped service. Opt out of the
// require-contract rule.
package secrets

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Provider resolves declared secret references to values for one env.
type Provider interface {
	Kind() string // "file" | "external" | "none"
	// Resolve returns the value for an env var by NAME (the key in the
	// store == the EnvVar.name). ok=false
	// when this provider has no value for name.
	Resolve(name string) (value string, ok bool)
	// All returns every value the provider can supply, keyed by name.
	// file: the whole store. external/none: nil.
	All() map[string]string
}

// ProviderConfig is the cli-decoupled view of the KCL secret_provider
// entity. (cli maps KCLEntities.SecretProvider -> this.)
type ProviderConfig struct {
	Type string // "file" | "external"
	Path string // secret-store file (already resolved to an absolute/project path by caller)
}

// NewProvider builds a Provider. cfg==nil -> a noop provider (Kind
// "none", All nil, Resolve always !ok) so callers need no nil checks.
// file: loads the store now; a MISSING file is a non-fatal
// file: loads the store now; a MISSING file is a non-fatal empty
// provider (so validation, not load, reports the missing keys with the
// `forge secret set` fix) — BUT an unreadable/invalid store is an error.
func NewProvider(cfg *ProviderConfig) (Provider, error) {
	if cfg == nil {
		return noopProvider{}, nil
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Type)) {
	case "external":
		return externalProvider{}, nil
	case "file":
		values, err := ReadSecretFile(cfg.Path)
		if err != nil {
			// A missing file is non-fatal: ValidateDeclaredRefs reports
			// the unresolvable refs with a `forge secret set` fix line,
			// which is far more useful than a bare stat error.
			if errors.Is(err, os.ErrNotExist) {
				return fileProvider{values: map[string]string{}, path: cfg.Path}, nil
			}
			return nil, fmt.Errorf("load secret file %q: %w", cfg.Path, err)
		}
		return fileProvider{values: values, path: cfg.Path}, nil
	case "", "none":
		return noopProvider{}, nil
	case "dotenv":
		// REMOVED. The dotenv provider handed every host service the whole
		// file, so a value went live without ever being declared in KCL —
		// which is how non-secret config drifted out of version control.
		// This is a hard error rather than a silent fallback: a project
		// still declaring it would otherwise boot with NO secrets and fail
		// later, somewhere less obvious.
		return nil, fmt.Errorf(
			"secret_provider 'dotenv' has been removed\n"+
				"fix: forge secret migrate <env>   (converts %s to secrets/<env>.yaml)\n"+
				"     then declare `secret_provider = forge.FileSecrets {path = \"secrets/<env>.yaml\"}`",
			cfg.Path)
	default:
		return nil, fmt.Errorf("unknown secret provider type %q (expected \"file\" or \"external\")", cfg.Type)
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

// fileProvider resolves values from a single gitignored YAML file: a flat
// map of env-var NAME -> value.
//
// One file, not a file-per-secret and not a dotenv. The dotenv was
// replaced because forge handed the WHOLE file to every host service, so a
// value went live without ever being declared in KCL — that is a property
// of the injection, not of the format, and it is fixed by
// declaration-scoped injection (see scopeSecretsToService), not by the
// on-disk shape. YAML then buys what a dotenv cannot: real quoting, and
// multi-line values (a PEM key, a JSON service-account blob) without
// escaping games.
type fileProvider struct {
	values map[string]string
	path   string
}

func (f fileProvider) Kind() string { return "file" }

func (f fileProvider) Resolve(name string) (string, bool) {
	v, ok := f.values[name]
	return v, ok
}

func (f fileProvider) All() map[string]string { return f.values }

// ReadSecretFile loads the YAML secret file: a flat mapping of env-var
// name to value.
//
// Values are decoded as strings, so `PORT: 8080` and `DEBUG: true` yield
// "8080" and "true" rather than a type error — an env var is a string, and
// failing on an unquoted number would be a papercut with no upside.
// Nested maps and sequences ARE an error: they cannot become an env var,
// so accepting them silently would leave a value that looks set but never
// arrives.
func ReadSecretFile(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	values := make(map[string]string, len(doc))
	var bad []string
	for k, v := range doc {
		if !ValidSecretKey(k) {
			bad = append(bad, fmt.Sprintf("%s (not a valid env-var name)", k))
			continue
		}
		switch tv := v.(type) {
		case nil:
			values[k] = ""
		case string:
			values[k] = tv
		case bool, int, int64, float64:
			values[k] = fmt.Sprintf("%v", tv)
		default:
			bad = append(bad, fmt.Sprintf("%s (%T is not a scalar)", k, v))
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return nil, fmt.Errorf(
			"%s has %d unusable entr(ies): %s\n"+
				"fix: every key must be an UPPER_SNAKE_CASE env-var name mapped to a scalar",
			path, len(bad), strings.Join(bad, ", "))
	}
	return values, nil
}

// ValidSecretKey reports whether name is usable as an env-var name:
// [A-Za-z_][A-Za-z0-9_]*. Shared with the CLI so `forge secret set`
// rejects the same names the loader would.
func ValidSecretKey(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// WriteSecretFile writes the map back as YAML with sorted keys, 0600.
// Sorted so a re-write is a clean diff rather than map-iteration noise.
func WriteSecretFile(path string, values map[string]string) error {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("# forge secret store — GITIGNORED, never commit.\n")
	b.WriteString("# Managed by `forge secret set/unset`; hand-edits are fine too.\n")
	b.WriteString("# Keys must match an EnvVar.secret_ref declared in KCL, or the value\n")
	b.WriteString("# reaches nothing (`forge secret list <env>` reports unused keys).\n")
	for _, k := range keys {
		out, err := yaml.Marshal(map[string]string{k: values[k]})
		if err != nil {
			return fmt.Errorf("marshal %s: %w", k, err)
		}
		b.Write(out)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// SecretRef is a declared reference extracted from the entities: the
// env-var NAME (== the secret's filename in the store), the k8s Secret
// NAME, and the key within that Secret. SecretKey defaults to EnvName
// when empty (matches the KCL _env_source lambda).
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
// declared ref the provider cannot supply. For Kind "file": each
// EnvName must be present in All(). For "external"/"none": returns nil
// (forge cannot see those values).
func ValidateDeclaredRefs(p Provider, refs []SecretRef, storePath string) error {
	if p == nil {
		return nil
	}
	// Only value-resolving providers can be validated: external/none
	// deliberately cannot see values, so there is nothing to check.
	if p.Kind() != "file" {
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
	fmt.Fprintf(&b, "secret provider %q (path %s) is missing %d declared value(s):\n", p.Kind(), storePath, len(missing))
	for _, r := range missing {
		fmt.Fprintf(&b, "    %-*s   (Secret %s/%s)\n", width, r.EnvName, r.SecretName, r.key())
	}
	// The fix line names the command for a dir provider — the whole point
	// of the dir layout is that adding a secret is a forge command, not a
	// hand-edit of a file that gets injected everywhere.
	fmt.Fprintf(&b, "fix: forge secret set <env> <KEY>   (writes into %s)\n", storePath)
	fmt.Fprint(&b, "     …or remove the secret_ref from the EnvVar if it is no longer needed.")
	return errors.New(b.String())
}

// DeclaredSecret is one explicitly-declared k8s Secret a RenderedSecrets
// provider renders: a Secret NAME and a map of in-Secret key -> value
// SOURCE. Mirrors the cli RenderedSecretEntity (the secrets package stays
// decoupled from cli, so cli maps its entity -> this).
type DeclaredSecret struct {
	Name string
	Keys map[string]DeclaredSecretKey
}

// DeclaredSecretKey is the value source for one key in a DeclaredSecret.
// From is "file" (resolve Key from the secret store) or "literal"
// (inline Value, gated to dev/e2e by RenderDeclaredSecrets).
type DeclaredSecretKey struct {
	From  string
	Key   string
	Value string
}

// envAllowsLiteral reports whether `from='literal'` inline values are
// permitted for env. The Go-side guard mirroring the KCL check: only
// dev/e2e may inline a secret value; every other env resolves from the
// store. Defense in depth — a hand-built entity (no KCL check) can't
// smuggle a literal into prod.
func envAllowsLiteral(env string) bool {
	return env == "dev" || env == "e2e"
}

// RenderDeclaredSecrets builds k8s Secret manifests from a
// RenderedSecrets provider's declared Secrets. Each declared key resolves
// to its value:
//
//   - from="file": resolve Key from the env's secret store `dot`. A key
//     with no value is an error (listed per Secret/key) — the value can't
//     be rendered.
//   - from="literal": use the inline Value, but ONLY when env is dev/e2e.
//     A literal in any other env is a hard error (the trust-safe gate).
//
// Returns the Secret manifests (one per declared Secret) in deterministic
// order, or a single aggregated error listing every problem. namespace is
// stamped on each Secret's metadata.
func RenderDeclaredSecrets(declared []DeclaredSecret, dot Provider, env, namespace string) ([]map[string]any, error) {
	if len(declared) == 0 {
		return nil, nil
	}
	// Deterministic Secret order.
	byName := make(map[string]DeclaredSecret, len(declared))
	names := make([]string, 0, len(declared))
	for _, d := range declared {
		byName[d.Name] = d
		names = append(names, d.Name)
	}
	sort.Strings(names)

	var problems []string
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		d := byName[name]
		sd := map[string]any{}
		// Deterministic key order within a Secret.
		keys := make([]string, 0, len(d.Keys))
		for k := range d.Keys {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			src := d.Keys[k]
			switch strings.ToLower(strings.TrimSpace(src.From)) {
			case "literal":
				if !envAllowsLiteral(env) {
					problems = append(problems, fmt.Sprintf(
						"Secret %s/%s: from='literal' is only allowed in dev/e2e (env %q must use from='file')",
						name, k, env))
					continue
				}
				sd[k] = src.Value
			// Resolve Key from the env's secret store.
			case "file", "":
				srcKey := src.Key
				if srcKey == "" {
					srcKey = k
				}
				v, ok := dot.Resolve(srcKey)
				if !ok {
					problems = append(problems, fmt.Sprintf(
						"Secret %s/%s: secret store key %q not found", name, k, srcKey))
					continue
				}
				sd[k] = v
			default:
				problems = append(problems, fmt.Sprintf(
					"Secret %s/%s: unknown source %q (expected 'file' or 'literal')", name, k, src.From))
			}
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
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("rendered secrets: %d problem(s):\n    %s",
			len(problems), strings.Join(problems, "\n    "))
	}
	return out, nil
}

// RenderK8sSecrets builds k8s Secret manifests (as []map[string]any,
// ready to marshal to YAML/JSON) from the resolved values, grouping refs
// by SecretName; each Secret's stringData[SecretKey] = resolved(EnvName).
// Only Kind "file" produces output; "external"/"none" return nil
// (prod references pre-existing Secrets). Skips refs whose value doesn't
// resolve (ValidateDeclaredRefs is the gate for those). Deterministic
// ordering (sorted Secret names + keys) for stable diffs.
func RenderK8sSecrets(p Provider, refs []SecretRef, namespace string) []map[string]any {
	if p == nil || p.Kind() != "file" {
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
