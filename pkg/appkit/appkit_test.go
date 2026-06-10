package appkit

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// recordingDef builds a Def whose rows append event strings to *events
// so tests can assert the full orchestration order.
func recordingDef(events *[]string) Def {
	rec := func(s string) func() error {
		return func() error {
			*events = append(*events, s)
			return nil
		}
	}
	svc := func(name string) ServiceDef {
		return ServiceDef{Name: name, Construct: func() (Mounter, error) {
			*events = append(*events, "construct:"+name)
			return func(mux *http.ServeMux) {
				*events = append(*events, "mount:"+name)
			}, nil
		}}
	}
	return Def{
		Setup: rec("setup"),
		Packages: []PackageDef{
			{Name: "cache", Construct: rec("package:cache")},
			{Name: "audit", Construct: rec("package:audit")},
		},
		Services: []ServiceDef{svc("api"), svc("orders")},
		Workers: []WorkerDef{
			{Name: "emailer", Construct: rec("worker:emailer")},
		},
		Operators: []OperatorDef{
			{Name: "scaler", Construct: rec("operator:scaler")},
		},
	}
}

func TestRun_OrchestrationOrder(t *testing.T) {
	var events []string
	def := recordingDef(&events)
	hooks := Hooks{
		BeforeMount: func(mux *http.ServeMux) error {
			events = append(events, "beforeMount")
			return nil
		},
		AfterMount: func(mux *http.ServeMux) error {
			events = append(events, "afterMount")
			return nil
		},
		ExtraMounts: []MountDef{
			{Pattern: "/extra/", Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})},
		},
	}
	def.Hooks = func() *Hooks { return &hooks }

	mux := http.NewServeMux()
	if err := Run(def, mux, slog.New(slog.DiscardHandler), Options{}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	want := []string{
		"setup",
		"package:cache", "package:audit",
		"construct:api", "construct:orders",
		"beforeMount",
		"mount:api", "mount:orders",
		"afterMount",
		"worker:emailer",
		"operator:scaler",
	}
	if got := strings.Join(events, ","); got != strings.Join(want, ",") {
		t.Errorf("orchestration order mismatch:\n got: %s\nwant: %s", got, strings.Join(want, ","))
	}

	// The extra mount must actually be registered on the mux.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/extra/x", nil)
	if _, pattern := mux.Handler(req); pattern != "/extra/" {
		t.Errorf("ExtraMounts not registered: matched pattern %q, want %q", pattern, "/extra/")
	}
}

func TestRun_OnlyFiltersMountingNotConstruction(t *testing.T) {
	var events []string
	def := recordingDef(&events)

	if err := Run(def, http.NewServeMux(), slog.New(slog.DiscardHandler), Options{Only: []string{"orders"}}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	joined := strings.Join(events, ",")
	// All services constructed (per-service binary mode: cheap structs,
	// prevents nil derefs on cross-service reads).
	for _, want := range []string{"construct:api", "construct:orders"} {
		if !strings.Contains(joined, want) {
			t.Errorf("events missing %q: %s", want, joined)
		}
	}
	// Only the named service is mounted.
	if strings.Contains(joined, "mount:api") {
		t.Errorf("api should NOT be mounted with Only=[orders]: %s", joined)
	}
	if !strings.Contains(joined, "mount:orders") {
		t.Errorf("orders should be mounted with Only=[orders]: %s", joined)
	}
	// Workers/operators are constructed regardless of the filter — the
	// caller gates which ones START.
	for _, want := range []string{"worker:emailer", "operator:scaler"} {
		if !strings.Contains(joined, want) {
			t.Errorf("events missing %q (filter must not skip construction): %s", want, joined)
		}
	}
}

func TestRun_LazyConstructSkipsFilteredServices(t *testing.T) {
	var events []string
	def := recordingDef(&events)

	if err := Run(def, http.NewServeMux(), slog.New(slog.DiscardHandler), Options{Only: []string{"orders"}, LazyConstruct: true}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	joined := strings.Join(events, ",")
	if strings.Contains(joined, "construct:api") {
		t.Errorf("LazyConstruct must skip construction of filtered-out services: %s", joined)
	}
	if !strings.Contains(joined, "construct:orders") || !strings.Contains(joined, "mount:orders") {
		t.Errorf("selected service must still construct + mount: %s", joined)
	}
}

func TestRun_EmptyOnlyMountsEverything(t *testing.T) {
	var events []string
	def := recordingDef(&events)

	if err := Run(def, http.NewServeMux(), slog.New(slog.DiscardHandler), Options{Only: nil}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	joined := strings.Join(events, ",")
	for _, want := range []string{"mount:api", "mount:orders"} {
		if !strings.Contains(joined, want) {
			t.Errorf("empty Only must mount everything; missing %q: %s", want, joined)
		}
	}
}

func TestRun_SetupErrorIsWrapped(t *testing.T) {
	sentinel := errors.New("boom")
	def := Def{Setup: func() error { return sentinel }}
	err := Run(def, http.NewServeMux(), slog.New(slog.DiscardHandler), Options{})
	if err == nil {
		t.Fatal("Run() should fail when Setup fails")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("setup error not wrapped: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "setup: ") {
		t.Errorf("setup error must keep the historical %q prefix, got %q", "setup: ", err.Error())
	}
}

func TestRun_ConstructErrorsAbort(t *testing.T) {
	sentinel := errors.New("init failed")
	var events []string
	def := recordingDef(&events)
	def.Services = append([]ServiceDef{{
		Name: "broken",
		Construct: func() (Mounter, error) {
			return nil, sentinel
		},
	}}, def.Services...)

	err := Run(def, http.NewServeMux(), slog.New(slog.DiscardHandler), Options{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run() = %v, want the construct error returned as-is", err)
	}
	// No later phase ran.
	if strings.Contains(strings.Join(events, ","), "worker:") {
		t.Error("workers must not be constructed after a service construct failure")
	}
}

func TestRun_ConstructWorkerHookIntercepts(t *testing.T) {
	var events []string
	def := recordingDef(&events)
	hooks := Hooks{
		ConstructWorker: func(name string, construct func() error) error {
			if name == "emailer" {
				events = append(events, "custom-worker:emailer")
				return nil // skip the table's constructor
			}
			return construct()
		},
	}
	def.Hooks = func() *Hooks { return &hooks }
	def.Workers = append(def.Workers, WorkerDef{Name: "other", Construct: func() error {
		events = append(events, "worker:other")
		return nil
	}})

	if err := Run(def, http.NewServeMux(), slog.New(slog.DiscardHandler), Options{}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	has := func(event string) bool {
		for _, e := range events {
			if e == event {
				return true
			}
		}
		return false
	}
	if has("worker:emailer") {
		t.Errorf("intercepted worker's default constructor must not run: %v", events)
	}
	if !has("custom-worker:emailer") {
		t.Errorf("hook replacement did not run: %v", events)
	}
	if !has("worker:other") {
		t.Errorf("hook delegating to construct() must keep default behavior: %v", events)
	}
}

func TestRun_HooksReadAfterSetup(t *testing.T) {
	// Hooks assigned DURING Setup (the documented setup.go pattern)
	// must be observed.
	var events []string
	var hooks Hooks
	def := recordingDef(&events)
	def.Setup = func() error {
		events = append(events, "setup")
		hooks.BeforeMount = func(mux *http.ServeMux) error {
			events = append(events, "beforeMount-from-setup")
			return nil
		}
		return nil
	}
	def.Hooks = func() *Hooks { return &hooks }

	if err := Run(def, http.NewServeMux(), slog.New(slog.DiscardHandler), Options{}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.Contains(strings.Join(events, ","), "beforeMount-from-setup") {
		t.Error("hooks assigned inside Setup were not observed — Hooks must be read after Setup returns")
	}
}

func TestRun_UnknownNameWarns(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	var events []string
	def := recordingDef(&events)

	if err := Run(def, http.NewServeMux(), logger, Options{Only: []string{"nope"}}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "unknown service/worker/operator name, ignoring") {
		t.Errorf("expected the historical unknown-name warning, got logs:\n%s", out)
	}
	if !strings.Contains(out, "nope") {
		t.Errorf("warning should carry the offending name, got logs:\n%s", out)
	}
}

func TestRun_RESTSkippedWithoutConnectNames(t *testing.T) {
	// REST on, but no service carries a ConnectName (defensive: rows
	// rendered without api.rest). Assign must not fire.
	var events []string
	def := recordingDef(&events)
	assigned := false
	def.REST = &RESTDef{Assign: func(http.Handler) { assigned = true }}

	if err := Run(def, http.NewServeMux(), slog.New(slog.DiscardHandler), Options{}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if assigned {
		t.Error("REST.Assign must not fire when no service has a ConnectName")
	}
}

func TestRun_RESTUnknownServiceErrorIsWrapped(t *testing.T) {
	// vanguard rejects Connect names with no registered proto service
	// descriptor; the error must surface with the historical prefix.
	var events []string
	def := recordingDef(&events)
	def.Services[0].ConnectName = "not.a.registered/Service"
	def.REST = &RESTDef{Assign: func(http.Handler) {}}

	err := Run(def, http.NewServeMux(), slog.New(slog.DiscardHandler), Options{})
	if err == nil {
		t.Fatal("Run() should fail when vanguard cannot resolve the service")
	}
	if !strings.HasPrefix(err.Error(), "init vanguard REST transcoder: ") {
		t.Errorf("REST error must keep the historical prefix, got %q", err.Error())
	}
}

func TestRun_NilHooksAndNilSetupAreFine(t *testing.T) {
	var events []string
	def := recordingDef(&events)
	def.Setup = nil
	def.Hooks = func() *Hooks { return nil }
	if err := Run(def, http.NewServeMux(), slog.New(slog.DiscardHandler), Options{}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}
