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