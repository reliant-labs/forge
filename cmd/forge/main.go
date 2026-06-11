package main

import (
	"fmt"
	"os"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/cli"
)

var (
	Version   = "dev"
	BuildDate = "unknown"
	GitCommit = "unknown"
	// PkgVersion is the published github.com/reliant-labs/forge/pkg
	// module version this binary scaffolds against. Empty on dev builds
	// (projects then use the .forge-pkg dev vendoring flow); release
	// builds stamp it via
	//   -ldflags "-X main.PkgVersion=vX.Y.Z"
	// after tagging pkg/vX.Y.Z with scripts/release-pkg.sh.
	// See docs/pkg-versioning.md.
	PkgVersion = ""
)

func main() {
	buildinfo.Set(Version, BuildDate, GitCommit)
	buildinfo.SetPkgVersion(PkgVersion)
	cli.SetVersion(Version, BuildDate, GitCommit)

	if err := cli.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
