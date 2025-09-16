package protomethod_test

import (
	"testing"

	"github.com/reliant-labs/forge/internal/linter/protomethod"
	"golang.org/x/tools/go/analysis/analysistest"
)

func TestAnalyzer(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, protomethod.Analyzer, "a")
}
