package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/cloud"
)

// hostedSecretWriter is the narrow surface `forge secret set/list/unset` use
// for an env whose secret_provider is forge.HostedSecrets.
//
// SEPARATE FROM secrets.Provider ON PURPOSE. A Provider RESOLVES values; the
// hosted one never does (its API is write-only and values are materialized
// in-cluster by the control plane). Widening Provider with Set/Delete would
// give every provider a write half it does not have, so the write half is
// declared here, at the one call site that needs it.
//
// List returns NAMES and presence only — the hosted API has no RPC that
// returns a value, and nothing in this file could carry one.
type hostedSecretWriter interface {
	Set(ctx context.Context, name, value string) (version uint32, err error)
	List(ctx context.Context) ([]hostedSecretSummary, error)
	Delete(ctx context.Context, name string) error
}

// hostedSecretSummary is the value-free list row.
type hostedSecretSummary struct {
	Name           string `json:"name"`
	CurrentVersion uint32 `json:"currentVersion"`
	// CurrentVersionDeleted / Destroyed report whether the CURRENT version
	// is usable. A deleted current version is absent for delivery.
	CurrentVersionDeleted   bool `json:"currentVersionDeleted"`
	CurrentVersionDestroyed bool `json:"currentVersionDestroyed"`
}

func (s hostedSecretSummary) present() bool {
	return s.CurrentVersion > 0 && !s.CurrentVersionDeleted && !s.CurrentVersionDestroyed
}

const (
	procSetSecret    = "controlplane.v1.SecretStoreService/SetSecret"
	procListSecrets  = "controlplane.v1.SecretStoreService/ListSecrets"
	procDeleteSecret = "controlplane.v1.SecretStoreService/DeleteSecret"
)

// cloudSecretWriter speaks SecretStoreService over Connect JSON. Field names
// are proto3 JSON (lowerCamel), declared here rather than generated: forge
// does not import control-plane (see cloud.Client).
type cloudSecretWriter struct {
	client        cloudCaller
	environmentID string
}

func (w cloudSecretWriter) Set(ctx context.Context, name, value string) (uint32, error) {
	var resp struct {
		Version uint32 `json:"version"`
	}
	req := map[string]any{"environmentId": w.environmentID, "name": name, "secretValue": value}
	if err := w.client.Call(ctx, procSetSecret, req, &resp); err != nil {
		// cloud.Client errors quote the server's message, which never
		// contains the request body — the value does not reach this error.
		return 0, err
	}
	return resp.Version, nil
}

func (w cloudSecretWriter) List(ctx context.Context) ([]hostedSecretSummary, error) {
	var resp struct {
		Secrets []hostedSecretSummary `json:"secrets"`
	}
	if err := w.client.Call(ctx, procListSecrets, map[string]any{"environmentId": w.environmentID}, &resp); err != nil {
		return nil, err
	}
	sort.Slice(resp.Secrets, func(i, j int) bool { return resp.Secrets[i].Name < resp.Secrets[j].Name })
	return resp.Secrets, nil
}

func (w cloudSecretWriter) Delete(ctx context.Context, name string) error {
	return w.client.Call(ctx, procDeleteSecret, map[string]any{"environmentId": w.environmentID, "name": name}, nil)
}

// hostedSecretTarget is everything a hosted secret command needs, resolved
// once from the env's KCL: the declaration, and a writer bound to the
// control-plane environment of this env's NAME.
//
// newHostedSecretWriter is a seam so tests can bind a writer to an httptest
// control plane without rendering KCL.
//
// ensure is true for the MUTATING commands (set, unset): they create the env
// by name first, so a secret can be set BEFORE the first deploy and a
// managedSecret pod never starts without its value. List is a read and passes
// false: an env the control plane has not seen has no secrets, and listing
// must not create it.
var newHostedSecretWriter = func(ctx context.Context, envName string, entities *KCLEntities, ensure bool) (hostedSecretWriter, cloud.Endpoint, error) {
	ep, err := cloud.ResolveEndpoint(envName, declarationFromEntities(entities))
	if err != nil {
		return nil, cloud.Endpoint{}, err
	}
	cred, err := cloud.ResolveCredential("", ep)
	if err != nil {
		return nil, cloud.Endpoint{}, err
	}
	client := cloud.NewClient(ep, cred)
	var envID string
	if ensure {
		envID, err = ensureHostedEnv(ctx, client, envName)
	} else {
		envID, err = cloudEnvResolver{client: client}.ResolveEnvironmentID(ctx, envName)
	}
	if err != nil {
		return nil, cloud.Endpoint{}, err
	}
	return cloudSecretWriter{client: client, environmentID: envID}, ep, nil
}

// isHostedSecretEnv reports whether the env's KCL declares forge.HostedSecrets.
func isHostedSecretEnv(entities *KCLEntities) bool {
	return entities != nil && entities.SecretProvider != nil &&
		strings.EqualFold(entities.SecretProvider.Type, "hosted")
}

func runHostedSecretSet(ctx context.Context, envName, key string, entities *KCLEntities, value string, out io.Writer) error {
	w, ep, err := newHostedSecretWriter(ctx, envName, entities, true)
	if err != nil {
		return err
	}
	version, err := w.Set(ctx, key, value)
	if err != nil {
		return err
	}
	// Never echo the value — only that it landed, where, and its version.
	fmt.Fprintf(out, "set %s (%d bytes) in hosted env %q at %s (version %d)\n", key, len(value), envName, ep.URL, version)
	return nil
}

func runHostedSecretUnset(ctx context.Context, envName, key string, entities *KCLEntities, out io.Writer) error {
	w, ep, err := newHostedSecretWriter(ctx, envName, entities, true)
	if err != nil {
		return err
	}
	if err := w.Delete(ctx, key); err != nil {
		return err
	}
	fmt.Fprintf(out, "unset %s in hosted env %q at %s (soft-deleted; the version history is kept)\n", key, envName, ep.URL)
	return nil
}

// collectHostedSecretListFacts builds the SAME secretListReport the file
// provider produces, from the hosted store's names — so `forge secret list
// --json` has one shape whatever the provider, and the console that reads it
// needs no second parser.
func collectHostedSecretListFacts(ctx context.Context, envName string, entities *KCLEntities) (secretListReport, error) {
	w, ep, err := newHostedSecretWriter(ctx, envName, entities, false)
	if err != nil {
		return secretListReport{}, err
	}
	stored, err := w.List(ctx)
	if err != nil {
		return secretListReport{}, err
	}
	present := map[string]bool{}
	for _, s := range stored {
		if s.present() {
			present[s.Name] = true
		}
	}
	report := secretListReport{
		Env:         envName,
		Provider:    "hosted",
		StorePath:   ep.URL,
		StoreExists: true,
		Secrets:     []secretListEntry{},
		Inert:       []string{},
		Missing:     []string{},
	}
	declared := declaredSecretNames(entities)
	attribution := secretDeclarationsByEnvName(entities)
	for _, name := range declared {
		if !present[name] {
			report.Missing = append(report.Missing, name)
		}
		report.Secrets = append(report.Secrets, secretListEntry{
			Name: name, Present: present[name], DeclaredBy: attribution[name],
		})
	}
	for name := range present {
		if !containsString(declared, name) {
			report.Inert = append(report.Inert, name)
		}
	}
	sort.Strings(report.Inert)
	report.MissingCount = len(report.Missing)
	report.OK = report.MissingCount == 0
	return report, nil
}
