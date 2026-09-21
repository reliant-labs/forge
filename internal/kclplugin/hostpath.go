// Copyright (c) 2025 Reliant Labs

package kclplugin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// hostPath resolves a path relative to the developer's home directory.
//
// # Why this is a plugin function and not a literal in an env's main.k
//
// forge runs services on the HOST in dev (HostDeploy), and such a service
// sometimes needs a file that lives on the developer's disk rather than inside
// a container. The case that forced this was a kubeconfig: a host process
// reaching a second cluster needs a host-readable path, and the pod-side
// projection of that same declaration is a container mount path that does not
// exist off-cluster.
//
// KCL cannot read the environment, by design — a render must be a function of
// its inputs. So the alternative is a literal "/Users/someone/..." written
// into an env file: correct on exactly one machine, wrong the moment it is
// committed, and invisible until a teammate renders it and gets a path that is
// not theirs.
//
// Resolving it here moves the one machine-specific fact into the one component
// that legitimately knows it — the forge process doing the render — and leaves
// the rendered output a plain absolute path that nothing downstream has to
// interpret.
//
// # Why an absolute argument is refused rather than passed through
//
// host_path("/etc/x") would silently ignore the home anchoring the caller
// asked for. A helper that anchors sometimes is worse than one that always
// does: the failure is a path that looks deliberate and is simply wrong. A
// caller who genuinely wants an absolute path already has one and does not
// need this.
//
// Parent traversal is refused for the same reason — `host_path("../../etc")`
// is asking this function to produce something outside the home directory,
// which is the one thing its name promises it will not do.
func hostPath(rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", fmt.Errorf("forge.host_path: a relative path is required, e.g. host_path(\".kube/config\")")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf(
			"forge.host_path(%q): the argument must be RELATIVE to the home directory; "+
				"an absolute path would ignore the anchoring this function exists to provide, "+
				"so write it directly instead of calling host_path", rel)
	}
	if cleaned := filepath.Clean(rel); cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf(
			"forge.host_path(%q): escapes the home directory, which is the one thing this "+
				"function promises not to do", rel)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf(
			"forge.host_path(%q): cannot determine the home directory: %w. This renders a "+
				"HOST path, so it is only meaningful where a developer's filesystem exists",
			rel, err)
	}

	return filepath.Join(home, filepath.Clean(rel)), nil
}
