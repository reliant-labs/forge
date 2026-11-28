package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// A hosted frontend publishes NO domains, whatever else it declares.
//
// On hosted a custom hostname is an org-scoped control-plane resource bound
// to one environment (`forge domain add` + `forge domain bind`), so the
// published StaticSite document must not carry one: a domain in spec is a
// field the control plane does not read as a binding, which deploys green
// and never resolves.
//
// MUTATION VERIFIED RED: re-adding a `spec.Domains = ...` assignment to
// hostedStaticSpec fails this.
func TestHostedStaticSpec_CarriesNoDomains(t *testing.T) {
	spec := hostedStaticSpec(FrontendEntity{Name: "web", BasePath: "/admin"})
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "domains") {
		t.Errorf("a hosted frontend published domains in its spec: %s", raw)
	}
	const want = `{"basePath":"/admin","keepReleases":10}`
	if string(raw) != want {
		t.Errorf("published StaticSite spec:\n got %s\nwant %s", raw, want)
	}
}

// That `Frontend.domains` is not a field at all — a compile-time refusal
// naming the member, not a value forge drops — is pinned by
// kcl/tests/closedschema_frontend_domains.k, which runs the closed schema
// through the same render this package uses.

// A hosted frontend publishes its content-hashed asset directory, so the
// platform can cache it forever and keep previous releases' copies serveable
// for tabs opened before a deploy (OnFirebase's keep_asset_releases, on the
// hosted runtime).
//
// MUTATION VERIFIED RED: dropping `AssetDir: f.AssetDir` from
// hostedStaticSpec fails this.
func TestHostedStaticSpec_CarriesAssetDir(t *testing.T) {
	spec := hostedStaticSpec(FrontendEntity{Name: "web", AssetDir: "assets"})
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"assetDir":"assets","keepReleases":10}`
	if string(raw) != want {
		t.Errorf("published StaticSite spec:\n got %s\nwant %s", raw, want)
	}
	if err := spec.Validate(); err != nil {
		t.Errorf("the published spec does not validate: %v", err)
	}
}

// A hosted frontend's named same-origin APIs are published as workload
// REFERENCES (the control plane resolves them in the same env), and a
// frontend that declares none publishes no apis key.
func TestHostedStaticSpec_CarriesNamedAPIsAsReferences(t *testing.T) {
	spec := hostedStaticSpec(FrontendEntity{Name: "web", APIs: map[string]string{"billing": "billing-api"}})
	if got := spec.APIs["billing"].Name; got != "billing-api" || len(spec.APIs) != 1 {
		t.Fatalf("spec.APIs = %v, want billing -> billing-api", spec.APIs)
	}
	if err := spec.Validate(); err != nil {
		t.Errorf("published spec refused: %v", err)
	}
	b, err := json.Marshal(hostedStaticSpec(FrontendEntity{Name: "web"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "apis") {
		t.Errorf("a frontend without apis publishes the key: %s", b)
	}
}

// Markdown negotiation and the edge-log opt-out reach the StaticSite spec
// only when declared: a frontend that declares neither publishes the spec
// it published before the fields (no key at all), and an explicit "on"
// (edge_logs true, never rendered by KCL) publishes nothing either.
func TestHostedStaticSpec_EdgeFieldsOnlyWhenDeclared(t *testing.T) {
	plain, err := json.Marshal(hostedStaticSpec(FrontendEntity{Name: "web"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"keepReleases":10}`; string(plain) != want {
		t.Errorf("a frontend declaring neither publishes %s, want %s", plain, want)
	}
	on := true
	if b, _ := json.Marshal(hostedStaticSpec(FrontendEntity{Name: "web", EdgeLogs: &on})); string(b) != string(plain) {
		t.Errorf("edge_logs on publishes %s, want %s", b, plain)
	}
	off := false
	b, _ := json.Marshal(hostedStaticSpec(FrontendEntity{Name: "web", MarkdownNegotiation: true, EdgeLogs: &off}))
	if want := `{"keepReleases":10,"markdownNegotiation":true,"edgeLogs":false}`; string(b) != want {
		t.Errorf("declared fields publish %s, want %s", b, want)
	}
}

// The implicit edge-log opt-out is observability disabled with nothing said
// about edge logs; an explicit edge_logs (rendered) is never also implicit.
func TestEdgeLogsOffIfHonoured(t *testing.T) {
	off := false
	disabled := &KCLEntities{Observability: &ObservabilityEntity{Enabled: false}}
	if !edgeLogsOffIfHonoured(disabled, FrontendEntity{Name: "web"}) {
		t.Error("observability disabled is not the implicit opt-out")
	}
	if edgeLogsOffIfHonoured(disabled, FrontendEntity{Name: "web", EdgeLogs: &off}) {
		t.Error("an explicit opt-out is also reported implicit")
	}
	if edgeLogsOffIfHonoured(&KCLEntities{Observability: &ObservabilityEntity{Enabled: true}}, FrontendEntity{Name: "web"}) ||
		edgeLogsOffIfHonoured(&KCLEntities{}, FrontendEntity{Name: "web"}) {
		t.Error("an env that keeps observability (or declares none) reports an opt-out")
	}
}
