package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/cli"
)

var (
	Version   = "dev"
	BuildDate = "unknown"
	GitCommit = "unknown"
)

func main() {
	buildinfo.Set(Version, BuildDate, GitCommit)
	cli.SetVersion(Version, BuildDate, GitCommit)

	if err := cli.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(exitCodeFor(err))
	}
}

// exitCodeFor lets a command choose its own exit status by returning an error
// that reports one, defaulting to 1 for everything else.
//
// `forge release verify` is the motivating case: "an artifact is missing" and
// "the registry could not be reached" are different facts, and a CI gate that
// reports both as 1 cannot be configured to tolerate a network blip while
// still blocking a bad release. A command signals the difference by returning
// an error implementing ExitCode; anything else keeps the historical 1.
func exitCodeFor(err error) int {
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		if code := coded.ExitCode(); code != 0 {
			return code
		}
	}
	return 1
}
