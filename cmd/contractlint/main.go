package main

import (
	"github.com/reliant-labs/forge/internal/linter/contract"
	"golang.org/x/tools/go/analysis/singlechecker"
)

func main() {
	singlechecker.Main(contract.Analyzer)
}
