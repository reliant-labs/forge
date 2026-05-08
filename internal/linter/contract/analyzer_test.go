package contract_test

import (
	"testing"

	"github.com/reliant-labs/forge/internal/linter/contract"
	"golang.org/x/tools/go/analysis/analysistest"
)

func TestAnalyzer_Good(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, contract.Analyzer, "good")
}

func TestAnalyzer_Bad(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, contract.Analyzer, "bad")
}

func TestAnalyzer_NoMethods(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, contract.Analyzer, "nomethods")
}

func TestAnalyzer_MultipleInterfaces(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, contract.Analyzer, "multiface")
}

func TestAnalyzer_PointerReceivers(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, contract.Analyzer, "pointer")
}

func TestAnalyzer_NoContract(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, contract.Analyzer, "nocontract")
}

func TestAnalyzer_EmbeddedInterface(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, contract.Analyzer, "embedded")
}

// Bug #20 regression: one impl struct satisfying two disjoint interfaces
// (Reader, Writer with no method overlap) should be accepted. Methods on
// the impl are valid as long as they live in the union of interfaces.
func TestAnalyzer_DisjointInterfaces(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, contract.Analyzer, "disjoint")
}

// RequireContractAnalyzer tests

func TestRequireContract_WithContract(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, contract.RequireContractAnalyzer, "internal/requiregood")
}

func TestRequireContract_NoExportedMethods(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, contract.RequireContractAnalyzer, "internal/requirenoexported")
}

func TestRequireContract_MissingContract(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, contract.RequireContractAnalyzer, "internal/requirebad")
}

func TestRequireContract_NotInternal(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, contract.RequireContractAnalyzer, "notinternal")
}

// ExportedVarsAnalyzer tests

func TestExportedVars_Good(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, contract.ExportedVarsAnalyzer, "varsok")
}

func TestExportedVars_Bad(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, contract.ExportedVarsAnalyzer, "varsbad")
}

// Regression: //go:embed targets must be exported package vars (the embed
// package requires it). The analyzer must exempt them — see scaffold-1 in
// PACK_AUDIT.md for the pristine-scaffold breakage that motivated this.
func TestExportedVars_EmbedTargetsExempt(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, contract.ExportedVarsAnalyzer, "varsembed")
}

// Regression: kubebuilder/controller-runtime API packages MUST expose
// `GroupVersion`, `SchemeBuilder`, and `AddToScheme` as package-level vars
// because k8s API machinery discovers them by name. Wrapping them in a getter
// would silently break operator scheme registration. The analyzer must exempt
// them — see operator-scaffold dogfood (2026-04-30) for the breakage that
// motivated this.
func TestExportedVars_KubebuilderAPIExempt(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, contract.ExportedVarsAnalyzer, "varsoperator")
}

// Regression: newer kubebuilder scaffolds emit
// `SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}` from
// sigs.k8s.io/controller-runtime/pkg/scheme rather than the classic
// `runtime.NewSchemeBuilder(...)` form. Both shapes are kubebuilder-blessed
// and must be exempt — see control-plane operator port (2026-04-30).
func TestExportedVars_KubebuilderControllerRuntimeAPIExempt(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, contract.ExportedVarsAnalyzer, "varsoperatorcr")
}