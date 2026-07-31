package lint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeLintProject materializes a set of files (relative paths → content)
// under a fresh temp dir and returns the dir.
func writeLintProject(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, src := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	return dir
}

func TestScanUndecidedObserveComponents(t *testing.T) {
	dir := writeLintProject(t, map[string]string{
		// Pure-compute, undecided → flag, suggest no-observe.
		"internal/pure/contract.go": "package pure\n\ntype Service interface{ Do() error }\n",
		"internal/pure/service.go":  "package pure\n\ntype Deps struct{\n\tLogger *slog.Logger\n\tConfig *config.Config\n}\ntype service struct{}\nfunc New(d Deps) (Service, error){ return &service{}, nil }\n",

		// DB dep, undecided → flag, suggest constructor.
		"internal/dbsvc/contract.go": "package dbsvc\n\ntype Service interface{ Do() error }\n",
		"internal/dbsvc/service.go":  "package dbsvc\n\ntype Deps struct{\n\tDB orm.Context\n}\ntype service struct{}\nfunc New(d Deps) (Service, error){ return &service{}, nil }\n",

		// Outbound-boundary package (I/O by marker) + a component depending
		// on it via closure → the dependent is I/O even though its own deps
		// are pure.
		"internal/notifier/contract.go": "// forge:outbound-io\npackage notifier\n\ntype Service interface{ Send() error }\n",
		"internal/notifier/service.go":  "package notifier\n\ntype Deps struct{}\ntype service struct{}\nfunc New(d Deps) (Service, error){ return &service{}, nil }\n",
		"internal/orders/contract.go":   "package orders\n\ntype Service interface{ Do() error }\n",
		"internal/orders/service.go":    "package orders\n\ntype Deps struct{\n\tNotifier notifier.Service\n}\ntype service struct{}\nfunc New(d Deps) (Service, error){ return &service{}, nil }\n",

		// Decided via marker → not flagged.
		"internal/marked/contract.go": "package marked\n\ntype Service interface{ Do() error }\n",
		"internal/marked/service.go":  "package marked\n\ntype Deps struct{}\ntype service struct{}\n\n// forge:constructor\nfunc New(d Deps) (Service, error){ return &service{}, nil }\n",

		// Decided via package opt-out → not flagged.
		"internal/optout/contract.go": "package optout\n\ntype Service interface{ Do() error }\n",
		"internal/optout/service.go":  "package optout\n\ntype Deps struct{}\ntype service struct{}\n\n// forge:no-observe\nfunc New(d Deps) (Service, error){ return &service{}, nil }\n",

		// Handler-shaped (New returns *Service) → excluded (edge-instrumented).
		"internal/handlers/api/contract.go": "package api\n\ntype Service struct{}\n",
		"internal/handlers/api/service.go":  "package api\n\ntype Deps struct{}\nfunc New(d Deps) (*Service, error){ return &Service{}, nil }\n",

		// Excluded from contract codegen → out of scope.
		"internal/skipme/contract.go": "// forge:exclude-contract\npackage skipme\n\ntype Service interface{ Do() error }\n",
		"internal/skipme/service.go":  "package skipme\n\ntype Deps struct{}\ntype service struct{}\nfunc New(d Deps) (Service, error){ return &service{}, nil }\n",
	})

	comps, err := scanUndecidedObserveComponents(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	got := map[string]bool{}     // name → present
	suggest := map[string]bool{} // name → SuggestConstructor
	for _, c := range comps {
		got[c.Name] = true
		suggest[c.Name] = c.SuggestConstructor
	}

	// notifier is a hand-written outbound boundary with a Service+New but no
	// observe decision → it too is undecided (the //forge:outbound-io marker
	// is not an observe decision) and, doing I/O by definition → suggest
	// constructor.
	wantFlagged := []string{"dbsvc", "notifier", "orders", "pure"}
	for _, n := range wantFlagged {
		if !got[n] {
			t.Errorf("expected %q to be flagged as undecided; got %v", n, comps)
		}
	}
	for _, n := range []string{"marked", "optout", "api", "skipme"} {
		if got[n] {
			t.Errorf("%q should NOT be flagged (decided/excluded/handler)", n)
		}
	}
	if len(comps) != len(wantFlagged) {
		t.Fatalf("expected exactly %d undecided components, got %d: %v", len(wantFlagged), len(comps), comps)
	}

	// I/O-aware suggestions.
	if suggest["pure"] {
		t.Error("pure-compute component should suggest // forge:no-observe (SuggestConstructor=false)")
	}
	if !suggest["dbsvc"] {
		t.Error("DB-dep component should suggest // forge:constructor (SuggestConstructor=true)")
	}
	if !suggest["orders"] {
		t.Error("outbound-io-closure component should suggest // forge:constructor (SuggestConstructor=true)")
	}
	if !suggest["notifier"] {
		t.Error("outbound-io component should suggest // forge:constructor (SuggestConstructor=true)")
	}
}

// TestComponentObserveError_NamesAndEscapes: the aggregated error is ONE
// message naming every undecided component AND all three escapes.
func TestComponentObserveError_NamesAndEscapes(t *testing.T) {
	comps := []observeComponent{
		{Name: "payments", Rel: "internal/payments", NewFileRel: "internal/payments/service.go", SuggestConstructor: true},
		{Name: "billing", Rel: "internal/billing", NewFileRel: "internal/billing/service.go", SuggestConstructor: false},
	}
	msg := componentObserveError(comps).Error()

	for _, want := range []string{
		"payments", "billing",
		"// forge:constructor",
		"// forge:no-observe",
		"config.enforce_component_observe: off",
		"2 component(s) have no observability decision",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated error missing %q:\n%s", want, msg)
		}
	}
	// The I/O-aware suggestion lines.
	if !strings.Contains(msg, "payments: deps touch I/O") {
		t.Errorf("payments should get the I/O suggestion:\n%s", msg)
	}
	if !strings.Contains(msg, "billing: pure compute") {
		t.Errorf("billing should get the pure-compute suggestion:\n%s", msg)
	}
}

// TestScanUndecided_CleanProject: a project whose components are all decided
// yields no findings.
func TestScanUndecided_CleanProject(t *testing.T) {
	dir := writeLintProject(t, map[string]string{
		"internal/a/contract.go": "package a\n\ntype Service interface{ Do() error }\n",
		"internal/a/service.go":  "package a\n\ntype Deps struct{}\ntype service struct{}\n\n// forge:constructor\nfunc New(d Deps) (Service, error){ return &service{}, nil }\n",
	})
	comps, err := scanUndecidedObserveComponents(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(comps) != 0 {
		t.Fatalf("expected clean project, got %v", comps)
	}
}
