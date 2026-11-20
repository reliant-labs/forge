package deploytarget

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestCompose_BestEffortServiceFailureDoesNotStopTheGroup pins NS FG-13: a
// local telemetry container that never becomes healthy is a warning, and
// the services after it in the group still deploy; a required one still
// fails the group.
func TestCompose_BestEffortServiceFailureDoesNotStopTheGroup(t *testing.T) {
	deploy := func(bestEffort bool) (*fakeRunner, error) {
		r := &fakeRunner{
			outputs: map[string]string{"docker compose -f docker-compose.yml ps": "db_1   Up\n"},
			// Its own compose file, so the failure matches clickstack's up
			// whatever flags the readiness wait spells.
			runErrs: map[string]error{"docker compose -f telemetry.yml up": errors.New("exit status 1")},
		}
		p := ComposeProvider{ProjectDir: t.TempDir(), Runner: r}
		err := p.Deploy(context.Background(), ServiceGroup{Env: "dev", ProviderID: "compose", Services: []ResolvedService{
			{Name: "clickstack", Compose: &ComposeSpec{ComposeFile: "telemetry.yml", BestEffort: bestEffort}},
			{Name: "db", Compose: &ComposeSpec{ComposeFile: "docker-compose.yml"}},
		}})
		return r, err
	}
	r, err := deploy(true)
	if err != nil {
		t.Fatalf("a best-effort telemetry service failed the group: %v", err)
	}
	if !slices.ContainsFunc(r.calls, func(c string) bool {
		return strings.HasPrefix(c, "docker compose -f docker-compose.yml up -d") && strings.HasSuffix(c, " db")
	}) {
		t.Fatalf("the service after the failed best-effort one never deployed: %v", r.calls)
	}
	if _, err := deploy(false); err == nil {
		t.Fatal("a required service's failure passed")
	}
}
