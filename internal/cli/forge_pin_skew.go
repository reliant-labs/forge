package cli

// Is the running forge the forge this project pins?
//
// WHY THIS IS LOUD. On 2026-10-07 ~/go/bin/forge was 3b4499b3 while every repo
// pinned d6b5d722, and agents repeatedly ran the wrong binary: fixes that were
// committed and pinned "did not work" because the forge running them predated
// them. Nothing said so. forgeVersionMismatchWarning stays silent for
// pseudo-versions on purpose (a dev build is not a release), and every pin in
// this workspace IS a pseudo-version — so the one comparison that mattered was
// the one never made.
//
// The comparison here is by IDENTITY, which a pseudo-version carries exactly:
// its 12-hex commit. Two pseudo-versions of one commit are the same forge;
// two commits are not, whatever their timestamps say.
//
// Every project command warns. The commands that BUILD, RELEASE or DEPLOY
// refuse, because what they produce is a function of the binary (a bundle
// records the forge that rendered it), and an artifact made by an unpinned
// forge is one the project cannot reproduce. --allow-version-skew (or
// FORGE_ALLOW_VERSION_SKEW=1) proceeds anyway.

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/forgecompat"
)

// pinGatedCommands are the commands, relative to forge's root, that refuse
// to run under a forge other than the pinned one.
var pinGatedCommands = map[string]bool{
	"env deploy": true,
	"env build":  true,
	"build":      true,
}

// allowVersionSkewEnv proceeds past the refusal, like --allow-version-skew.
const allowVersionSkewEnv = "FORGE_ALLOW_VERSION_SKEW"

// allowVersionSkewFlag is registered on exactly the pin-gated commands (it
// means nothing anywhere else) by registerVersionSkewFlags.
const allowVersionSkewFlag = "allow-version-skew"

// registerVersionSkewFlags adds --allow-version-skew to each pin-gated
// command under root.
func registerVersionSkewFlags(root *cobra.Command) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		rel := strings.TrimSpace(strings.TrimPrefix(c.CommandPath(), root.CommandPath()))
		if pinGatedCommands[rel] && c.Flags().Lookup(allowVersionSkewFlag) == nil {
			c.Flags().Bool(allowVersionSkewFlag, false,
				"Run even though this forge is not the forge_version forge.yaml pins (also "+allowVersionSkewEnv+"=1)")
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
}

// pseudoCommitRE captures a Go pseudo-version's 12-hex commit.
var pseudoCommitRE = regexp.MustCompile(`-(?:[\w.]+\.)?[0-9]{14}-([0-9a-f]{12})(?:\+.*)?$`)

// forgeIdentity is what a version string says about WHICH forge it is: a
// commit for a pseudo-version, the version itself (without build metadata)
// for a tag. Empty when the string names no forge in particular.
func forgeIdentity(v string) string {
	v = strings.TrimSpace(v)
	switch v {
	case "", "dev", "(devel)":
		return ""
	}
	if m := pseudoCommitRE.FindStringSubmatch(v); m != nil {
		return m[1]
	}
	if i := strings.Index(v, "+"); i >= 0 {
		v = v[:i]
	}
	return v
}

// pinSkew reports whether binary (and, when its version names no commit,
// binaryCommit) is a different forge from pin. ok is false when either side
// names no forge in particular — nothing can be compared, and nothing is said.
func pinSkew(pin, binary, binaryCommit string) (skewed, ok bool) {
	want := forgeIdentity(pin)
	have := forgeIdentity(binary)
	if pseudoCommitRE.MatchString(strings.TrimSpace(pin)) && !pseudoCommitRE.MatchString(strings.TrimSpace(binary)) && len(binaryCommit) >= 12 {
		have = binaryCommit[:12]
	}
	if want == "" || have == "" {
		return false, false
	}
	return want != have, true
}

// checkForgePinSkew is the root command's check, for cmd. It warns on w, and
// returns a refusal for a pin-gated command unless allowed.
func checkForgePinSkew(cmd, root *cobra.Command, w io.Writer, allow bool) error {
	projectRoot, err := cmdutil.FindProjectRoot()
	if err != nil || projectRoot == "" {
		return nil
	}
	cfg, err := config.LoadProjectDir(projectRoot)
	if err != nil || cfg == nil {
		return nil
	}
	// forge's own repository is built and run from source by the people
	// changing it; the pin there describes the scaffolds, not the binary.
	if cfg.ModulePath == forgeModulePathForSkew {
		return nil
	}
	// A project bridged (go.work) to the local forge checkout this very
	// binary was built from is someone running the forge they are working
	// on — the e2e lanes, a forge developer. Its pin follows go.mod (often
	// the published floor) and says nothing about that binary. A bridged
	// project run by a binary that is NOT its checkout's — a stale
	// ~/go/bin/forge, the 2026-10-07 case — is checked like any other.
	if rep := forgecompat.InspectBridge(projectRoot); rep.Bridged && rep.CheckoutOK && !rep.Skewed {
		return nil
	}
	pin, binary := cfg.ForgeVersion, buildinfo.Version()
	skewed, ok := pinSkew(pin, binary, buildinfo.GitCommit())
	if !ok || !skewed {
		return nil
	}
	rel := strings.TrimSpace(strings.TrimPrefix(cmd.CommandPath(), root.CommandPath()))
	gated := pinGatedCommands[rel]
	if gated && !allow && os.Getenv(allowVersionSkewEnv) == "" {
		return fmt.Errorf("this forge is %s, but %s/forge.yaml pins forge_version %s.\n"+
			"  `forge %s` produces artifacts that record the forge that made them, so it runs only under the pinned forge.\n"+
			"  Install the pinned one:  go install github.com/reliant-labs/forge/cmd/forge@%s\n"+
			"  (or proceed anyway, knowingly: --allow-version-skew, or %s=1)",
			binary, projectRoot, pin, rel, pin, allowVersionSkewEnv)
	}
	_, _ = fmt.Fprintf(w, "⚠️  this forge is %s, but forge.yaml pins forge_version %s — a different forge than the project's.\n"+
		"   Install the pinned one: go install github.com/reliant-labs/forge/cmd/forge@%s\n", binary, pin, pin)
	return nil
}

// forgeModulePathForSkew is forge's own module path.
const forgeModulePathForSkew = "github.com/reliant-labs/forge"
