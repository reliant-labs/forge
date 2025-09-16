package main

import (
	"fmt"
	"os"

	"github.com/reliant-labs/forge/internal/cli"
)

var (
	Version   = "dev"
	BuildDate = "unknown"
	GitCommit = "unknown"
)

func main() {
	cli.SetVersion(Version, BuildDate, GitCommit)

	if err := cli.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}