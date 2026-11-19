package cli

// Which forge does this project pin, and where is it?
//
// A tool that runs forge from OUTSIDE the project — a test resolver that
// execs forge from a temp directory, a script in another repo — gets the forge
// it ran, never the one the project pins: the hand-off (forge_pin_delegate.go)
// needs a project around the command to decide anything. Before this, the
// reliant and control-plane test resolvers ran `reliant forge lint --help` in
// the project to trigger the pinned build, then dug the binary out of
// forgedelegate's private cache layout and version-checked what they found.
//
// `forge version --pinned` answers it instead: the forge the project resolved
// from the working directory (or -C) pins, obtained through the hand-off's
// own resolution (delegateTargetFor + resolveTarget: the linked checkout, the
// project's .tools/bin/forge, the cache, or a build), and printed rather than
// exec'd. When the running forge already is the pin, the answer is this
// forge's own executable. cli.ResolvePinnedForgePath is the same answer for a
// host that embeds forge.
//
// The answer is about the PIN, so none of the hand-off's "run THIS forge"
// exceptions apply: not --allow-version-skew or FORGE_ALLOW_VERSION_SKEW (the
// command has no flag to override, and the environment says which forge to
// RUN, not which one is pinned), and not the development-forge-against-a-tag
// tolerance, which the build commands do not grant either.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/forgecompat"
)

// pinnedForgeSchema names the `forge version --pinned --json` document.
const pinnedForgeSchema = "forge.version.pinned/v1"

// PinnedSourceSelf is PinnedForge.Source when the running forge is the one
// the project pins.
const PinnedSourceSelf = "self"

// pinnedFlag is `forge version`'s --pinned.
const pinnedFlag = "pinned"

// pinnedForgeDocument is `forge version --pinned --json`.
type pinnedForgeDocument struct {
	Schema string `json:"schema"`
	// Path is the executable.
	Path string `json:"path"`
	// Command is the exec-ready command that runs it, before forge's own
	// arguments: [path], or [path, "forge"] when Path is a host that embeds
	// forge (`reliant forge`).
	Command []string `json:"command"`
	// Version is the version Path reports.
	Version string `json:"version"`
	// Source is where Path came from: self, tools, cache or built.
	Source string `json:"source"`
	// Pin is forge.yaml's forge_version.
	Pin string `json:"pin,omitempty"`
	// Checkout is the local forge checkout the project is linked to (go.work
	// use, or a directory replace), which then decides the forge over Pin.
	Checkout string `json:"checkout,omitempty"`
	// Project is the project root the answer is for.
	Project string `json:"project"`
}

// errNotInProject is ResolvePinnedForgePath's answer outside every project.
var errNotInProject = errors.New("not inside a forge project: no forge.yaml at or above the directory (-C names one)")

// ResolvePinnedForgePath is the forge the project at or above dir ("" for
// the working directory) pins, ready to run: this forge's executable when it
// already is that forge, else the forge the hand-off would exec there,
// obtained the way the hand-off obtains it (resolveTarget) and checked to
// report the pinned version. It errors when there is no project, no pin, or
// the pinned forge cannot be had. log receives the one-time build notice.
func ResolvePinnedForgePath(ctx context.Context, dir string, log io.Writer) (PinnedForge, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	projectRoot, err := projectRootFrom(dir)
	if err != nil {
		return PinnedForge{}, err
	}
	if projectRoot == "" {
		return PinnedForge{}, errNotInProject
	}
	cfgPath := filepath.Join(projectRoot, defaultProjectConfigFile)
	cfg, readable := readPinFields(projectRoot)
	pin := strings.TrimSpace(cfg.ForgeVersion)
	bridge := forgecompat.InspectBridge(projectRoot)
	checkout := ""
	if bridge.Bridged && bridge.CheckoutOK {
		checkout = bridge.Bridge.Dir
	}
	if checkout == "" {
		if !readable {
			return PinnedForge{}, fmt.Errorf("%s is not readable YAML, so it names no forge_version", cfgPath)
		}
		if pin == "" {
			return PinnedForge{}, fmt.Errorf("%s pins no forge_version: no forge is the project's (set one with `%s project upgrade --to <version>`)", cfgPath, Name())
		}
	}

	t, other := delegateTargetFor(projectRoot)
	if !other {
		return selfPinnedForge(pin, checkout)
	}
	res, err := resolveTarget(ctx, log, projectRoot, t)
	if err != nil {
		return PinnedForge{}, pinnedUnobtainable(cfgPath, t, err)
	}
	pf := PinnedForge{
		Path: res.Path, Key: t.resultKey(res.Path), Want: t.String(),
		Source: res.Kind, Pin: pin, Checkout: t.checkout,
	}
	r := pinDelegateResolver(log)
	if r.Version == nil {
		return pf, nil
	}
	got, err := r.Version(ctx, res.Path)
	if err != nil {
		return PinnedForge{}, pinnedUnobtainable(cfgPath, t, fmt.Errorf("%s does not run: %w", res.Path, err))
	}
	pf.Version = strings.TrimSpace(got)
	// A checkout has no version to hold it to: its build is the forge.
	if t.checkout == "" && r.Same != nil && !r.Same(t.pin, pf.Version) {
		return PinnedForge{}, pinnedUnobtainable(cfgPath, t, fmt.Errorf("%s reports forge %s, not the pinned %s", res.Path, pf.Version, t.pin))
	}
	return pf, nil
}

// selfPinnedForge is this forge as the project's forge.
func selfPinnedForge(pin, checkout string) (PinnedForge, error) {
	command, err := forgeExecCommand()
	if err != nil {
		return PinnedForge{}, err
	}
	want := "forge_version " + pin
	if checkout != "" {
		want = "the checkout " + checkout + " this project is linked to"
	}
	version := buildinfo.Version()
	return PinnedForge{
		Path: command[0], Args: command[1:], Key: selfKey(version, command[0]), Want: want,
		Source: PinnedSourceSelf, Version: version, Pin: pin, Checkout: checkout,
	}, nil
}

// selfKey is PinnedForge.Key for this forge: the version it reports and the
// identity of the file at path. Never "pin:": that names a module version a
// host may cache output for forever, and this binary may be a local build
// that reports one.
func selfKey(version, path string) string {
	stamp := "unknown"
	if st, err := os.Stat(path); err == nil {
		stamp = strconv.FormatInt(st.Size(), 10) + "@" + strconv.FormatInt(st.ModTime().UnixNano(), 10)
	}
	return "self:" + version + "@" + path + "@" + stamp
}

// pinnedUnobtainable is the error when the project's forge cannot be had.
func pinnedUnobtainable(cfgPath string, t delegateTarget, cause error) error {
	var b strings.Builder
	fmt.Fprintf(&b, "this forge is %s, but %s wants %s, and that forge could not be obtained:\n", t.binary, cfgPath, t)
	for _, line := range strings.Split(strings.TrimSpace(cause.Error()), "\n") {
		fmt.Fprintf(&b, "    %s\n", line)
	}
	b.WriteString("  Fix what stopped it (offline? a pin nobody published?), or move the pin deliberately with `" + Name() + " project upgrade`")
	return errors.New(b.String())
}

// runVersionPinned is `forge version --pinned [--json]`.
func runVersionPinned(cmd *cobra.Command, asJSON bool) error {
	// The project this invocation names: -C, installed by the root's
	// PersistentPreRun, else the working directory.
	dir, err := cmdutil.ResolutionRoot()
	if err != nil {
		return err
	}
	root, err := projectRootFrom(dir)
	if err != nil {
		return err
	}
	if root == "" {
		return errNotInProject
	}
	pf, err := ResolvePinnedForgePath(cmd.Context(), root, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	doc := pinnedForgeDocument{
		Schema: pinnedForgeSchema, Path: pf.Path, Command: append([]string{pf.Path}, pf.Args...),
		Version: pf.Version, Source: pf.Source, Pin: pf.Pin, Checkout: pf.Checkout, Project: root,
	}
	out := cmd.OutOrStdout()
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(doc)
	}
	fmt.Fprintf(out, "path:    %s\n", doc.Path)
	if len(pf.Args) > 0 {
		fmt.Fprintf(out, "command: %s\n", strings.Join(doc.Command, " "))
	}
	fmt.Fprintf(out, "version: %s\n", doc.Version)
	fmt.Fprintf(out, "source:  %s\n", doc.Source)
	fmt.Fprintf(out, "wants:   %s\n", pf.Want)
	return nil
}

// versionPinnedRun reports whether cmd is `forge version --pinned`: a
// question about the pin, which the root's skew warning would only restate.
func versionPinnedRun(cmd *cobra.Command, rel string) bool {
	if rel != "version" || cmd.Flags().Lookup(pinnedFlag) == nil {
		return false
	}
	on, _ := cmd.Flags().GetBool(pinnedFlag)
	return on
}
