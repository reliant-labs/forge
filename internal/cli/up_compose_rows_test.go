package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/deploytarget"
)

// The README: "turn on observability and the table includes Grafana too".
// The table listed host processes and frontends only, and Grafana's port is
// dynamic, so with the opt-in on nothing on screen said where Grafana was.
func TestUpSummaryListsWhatComposeWorkloadsPublish(t *testing.T) {
	e := &KCLEntities{Workloads: []WorkloadEntity{
		{Name: "api", Runtime: RuntimeEntity{Type: RuntimeHost}},
		{Name: "lgtm", Runtime: RuntimeEntity{Type: RuntimeCompose, Compose: &ComposeRuntime{Service: "lgtm"}}},
	}}
	asked := ""
	fake := func(_ context.Context, _, _, service string) ([]composePublisher, error) {
		asked = service
		return []composePublisher{
			{URL: "127.0.0.1", TargetPort: 4317, PublishedPort: 44317, Protocol: "tcp"},
			{URL: "0.0.0.0", TargetPort: 3000, PublishedPort: 56966, Protocol: "tcp"},
			{URL: "::", TargetPort: 3000, PublishedPort: 56966, Protocol: "tcp"}, // the IPv6 twin
		}, nil
	}

	rows := composeRows(context.Background(), e, t.TempDir(), nil, fake)
	if asked != "lgtm" {
		t.Fatalf("asked docker about %q, want the compose service lgtm", asked)
	}
	if len(rows) != 2 {
		t.Fatalf("want one row per published port (IPv4/IPv6 twins folded), got %+v", rows)
	}
	if rows[0].URL != "http://localhost:56966" || rows[0].Name != "lgtm :3000" {
		t.Errorf("Grafana row = %+v", rows[0])
	}

	var out bytes.Buffer
	renderUpSummary(&out, "dev", rows, "starting", false, nil, nil)
	for _, want := range []string{"Compose services", "lgtm :3000", "http://localhost:56966", "docker compose logs lgtm"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary is missing %q:\n%s", want, out.String())
		}
	}
}

// One `forge env up` converged an opted-in lgtm three times — the infra
// pre-warm, the deploy phase, then the host phase's own pre-warm — pulling
// the image each time. Once the pre-warm has converged the infra, the deploy
// phase leaves those groups alone; cluster groups still deploy.
func TestWithoutInfraGroups_KeepsOnlyWhatThePrewarmDidNotConverge(t *testing.T) {
	got := withoutInfraGroups([]deploytarget.ServiceGroup{
		{ProviderID: "host-infra"}, {ProviderID: "compose"}, {ProviderID: "k8s-cluster"}, {ProviderID: "hosted"},
	})
	if len(got) != 2 || got[0].ProviderID != "k8s-cluster" || got[1].ProviderID != "hosted" {
		t.Fatalf("withoutInfraGroups = %+v, want only the k8s-cluster and hosted groups", got)
	}
}
