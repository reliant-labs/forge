package cli

import (
	"testing"

	"github.com/reliant-labs/forge/internal/deploytarget"
)

// TestDeclaredSecretsFromEntities maps the entity provider shape to the
// secrets-package DeclaredSecret, preserving per-key sources.
func TestDeclaredSecretsFromEntities(t *testing.T) {
	e := &KCLEntities{
		SecretProvider: &SecretProviderEntity{
			Type: "rendered",
			Secrets: []RenderedSecretEntity{{
				Name: "db-credentials",
				Keys: map[string]RenderedSecretKeyEntity{
					"password": {From: "dotenv", Key: "DB_PASSWORD"},
					"issuer":   {From: "literal", Value: "https://test.local/"},
				},
			}},
		},
	}
	got := declaredSecretsFromEntities(e)
	if len(got) != 1 {
		t.Fatalf("got %d declared secrets want 1", len(got))
	}
	if got[0].Name != "db-credentials" {
		t.Errorf("name = %q", got[0].Name)
	}
	if got[0].Keys["password"].Key != "DB_PASSWORD" || got[0].Keys["password"].From != "dotenv" {
		t.Errorf("password key wrong: %+v", got[0].Keys["password"])
	}
	if got[0].Keys["issuer"].Value != "https://test.local/" || got[0].Keys["issuer"].From != "literal" {
		t.Errorf("issuer key wrong: %+v", got[0].Keys["issuer"])
	}

	// A non-rendered provider yields nothing.
	if declaredSecretsFromEntities(&KCLEntities{SecretProvider: &SecretProviderEntity{Type: "dotenv"}}) != nil {
		t.Error("dotenv provider should yield no declared secrets")
	}
}

// TestReferencedSecretNamesForGroup is the trust-boundary scoping: a
// declared Secret lands in a cluster ONLY when one of that cluster's
// services references it. A Secret referenced only by a service in
// ANOTHER group must NOT appear.
func TestReferencedSecretNamesForGroup(t *testing.T) {
	entities := &KCLEntities{
		Services: []ServiceEntity{
			{
				Name: "cp-api",
				EnvVars: []KCLEnvVar{
					{Name: "DB_PASSWORD", SecretRef: "cp-db", SecretKey: "password"},
				},
			},
			{
				Name: "workload-api",
				EnvVars: []KCLEnvVar{
					{Name: "TOKEN", SecretRef: "workload-token", SecretKey: "token"},
				},
			},
		},
	}

	cpGroup := deploytarget.ServiceGroup{
		ProviderID: "k8s-cluster",
		Cluster:    "k3d-cp",
		Services:   []deploytarget.ResolvedService{{Name: "cp-api"}},
	}
	workloadGroup := deploytarget.ServiceGroup{
		ProviderID: "k8s-cluster",
		Cluster:    "k3d-workload",
		Services:   []deploytarget.ResolvedService{{Name: "workload-api"}},
	}

	cpRefs := referencedSecretNamesForGroup(entities, cpGroup)
	if _, ok := cpRefs["cp-db"]; !ok {
		t.Error("cp group must reference cp-db")
	}
	if _, ok := cpRefs["workload-token"]; ok {
		t.Error("cp group must NOT reference workload-token (cross-boundary leak)")
	}

	wRefs := referencedSecretNamesForGroup(entities, workloadGroup)
	if _, ok := wRefs["workload-token"]; !ok {
		t.Error("workload group must reference workload-token")
	}
	if _, ok := wRefs["cp-db"]; ok {
		t.Error("workload group must NOT reference cp-db (cross-boundary leak)")
	}
}
