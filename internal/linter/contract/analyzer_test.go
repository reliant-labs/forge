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
