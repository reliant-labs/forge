package main

import (
	"github.com/reliant-labs/forge/internal/linter/contract"
	"golang.org/x/tools/go/analysis/multichecker"
)

func main() {
	multichecker.Main(
		contract.Analyzer,
		contract.RequireContractAnalyzer,
		contract.ExportedVarsAnalyzer,
	)
}
