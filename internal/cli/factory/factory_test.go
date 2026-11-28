package factory

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/projectstore"
)

// The package keeps process-global seams; each test restores them.
func restoreGlobals(t *testing.T) {
	t.Helper()
	prevFactories, prevGen, prevAudit, prevDebug, prevLoader := commandFactories, genAPI, auditAPI, debugAPI, projectStoreLoader
	t.Cleanup(func() {
		commandFactories, genAPI, auditAPI, debugAPI, projectStoreLoader = prevFactories, prevGen, prevAudit, prevDebug, prevLoader
	})
}

func TestNew_CarriesTheProcessStreamsAndRegisteredSeams(t *testing.T) {
	restoreGlobals(t)
	var loaded bool
	SetProjectStoreLoader(func() (*projectstore.Store, error) { loaded = true; return nil, nil })
	SetGenAPI(GenAPI{ServiceRegistryRelPath: "internal/app/services.go"})
	SetAuditAPI(AuditAPI{ListEnvs: func(string) ([]string, error) { return []string{"dev"}, nil }})
	SetDebugAPI(DebugAPI{})

	f := New()
	if f.Out != os.Stdout || f.Err != os.Stderr || f.In != os.Stdin {
		t.Errorf("New must default to the process's own streams: %v %v %v", f.Out, f.Err, f.In)
	}
	if f.Gen.ServiceRegistryRelPath != "internal/app/services.go" {
		t.Errorf("Gen = %+v, want the registered GenAPI", f.Gen)
	}
	if envs, _ := f.Audit.ListEnvs("."); !reflect.DeepEqual(envs, []string{"dev"}) {
		t.Errorf("Audit.ListEnvs = %v", envs)
	}
	if _, err := f.LoadProjectStore(); err != nil || !loaded {
		t.Errorf("the factory's loader must be the registered one (loaded=%v, err=%v)", loaded, err)
	}
}

func TestNew_BeforeAnyRegistration(t *testing.T) {
	restoreGlobals(t)
	projectStoreLoader, genAPI, auditAPI, debugAPI = nil, GenAPI{}, AuditAPI{}, DebugAPI{}
	f := New()
	if f.LoadProjectStore != nil || f.Gen.RunPipeline != nil || f.Audit.ListEnvs != nil || f.Debug.WorkloadEnv != nil {
		t.Errorf("an unregistered seam must be nil, not a stub: %+v", f)
	}
}

func TestNew_SnapshotsSeamsAtConstruction(t *testing.T) {
	restoreGlobals(t)
	SetGenAPI(GenAPI{ServiceRegistryRelPath: "first"})
	f := New()
	SetGenAPI(GenAPI{ServiceRegistryRelPath: "second"})
	if f.Gen.ServiceRegistryRelPath != "first" {
		t.Errorf("a registration after New changed an existing factory: %q", f.Gen.ServiceRegistryRelPath)
	}
	if New().Gen.ServiceRegistryRelPath != "second" {
		t.Error("a new factory must see the latest registration")
	}
}

func TestLoadProjectStore_UsesTheRegisteredLoader(t *testing.T) {
	restoreGlobals(t)
	boom := errors.New("no forge.yaml")
	SetProjectStoreLoader(func() (*projectstore.Store, error) { return nil, boom })
	if _, err := LoadProjectStore(); !errors.Is(err, boom) {
		t.Errorf("LoadProjectStore = %v, want the loader's error", err)
	}
}

func TestRegister_AccumulatesInOrder(t *testing.T) {
	restoreGlobals(t)
	commandFactories = nil
	if len(Registered()) != 0 {
		t.Fatal("expected an empty registry")
	}
	var order []string
	mk := func(name string) CmdFactory {
		return func(f *Factory) *cobra.Command {
			order = append(order, name)
			return &cobra.Command{Use: name}
		}
	}
	Register(mk("scaffold"))
	Register(mk("lint"))
	Register(mk("audit"))

	f := &Factory{}
	var used []string
	for _, build := range Registered() {
		used = append(used, build(f).Use)
	}
	want := []string{"scaffold", "lint", "audit"}
	if !reflect.DeepEqual(used, want) || !reflect.DeepEqual(order, want) {
		t.Errorf("registered commands = %v (built %v), want %v in registration order", used, order, want)
	}
}

func TestStepsFailedError(t *testing.T) {
	t.Run("names every failed step with its cause", func(t *testing.T) {
		err := &StepsFailedError{
			Steps:  []string{"proto", "frontend"},
			Causes: []string{"buf: no such plugin", "npm: ECONNRESET"},
		}
		msg := err.Error()
		for _, want := range []string{"2 generate step(s) failed", "the other steps ran and their output stands"} {
			if !strings.Contains(msg, want) {
				t.Errorf("message %q lacks %q", msg, want)
			}
		}
		if !strings.Contains(msg, "proto: buf: no such plugin; frontend: npm: ECONNRESET") {
			t.Errorf("steps must be joined with semicolons in failure order: %q", msg)
		}
	})

	t.Run("a step without a recorded cause is named alone", func(t *testing.T) {
		err := &StepsFailedError{Steps: []string{"a", "b", "c"}, Causes: []string{"first cause", ""}}
		msg := err.Error()
		if !strings.Contains(msg, "a: first cause; b; c") {
			t.Errorf("message %q: b has an empty cause and c has none, so both are named bare", msg)
		}
	})

	t.Run("a caller that names steps alone", func(t *testing.T) {
		msg := (&StepsFailedError{Steps: []string{"only"}}).Error()
		if !strings.HasPrefix(msg, "1 generate step(s) failed: only (") {
			t.Errorf("message = %q", msg)
		}
	})

	t.Run("it is an error a caller can single out with errors.As", func(t *testing.T) {
		var err error = &StepsFailedError{Steps: []string{"x"}}
		wrapped := errors.Join(errors.New("generate"), err)
		var sf *StepsFailedError
		if !errors.As(wrapped, &sf) || len(sf.Steps) != 1 {
			t.Error("errors.As must find the StepsFailedError")
		}
	})
}
