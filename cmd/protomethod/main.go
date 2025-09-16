package main

import (
	"github.com/reliant-labs/forge/internal/linter/protomethod"
	"golang.org/x/tools/go/analysis/singlechecker"
)

func main() {
	singlechecker.Main(protomethod.Analyzer)
}
