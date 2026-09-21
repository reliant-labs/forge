// Copyright (c) 2025 Reliant Labs

package kclplugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHostPath_AnchorsUnderHome is the property the function's name promises.
func TestHostPath_AnchorsUnderHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory on this machine: %v", err)
	}

	got, err := hostPath(".kube/config")
	if err != nil {
		t.Fatalf("hostPath(.kube/config): %v", err)
	}
	want := filepath.Join(home, ".kube", "config")
	if got != want {
		t.Fatalf("hostPath = %q, want %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("hostPath returned a relative path %q; the rendered value must be absolute "+
			"because nothing downstream knows what it would be relative TO", got)
	}
}

// TestHostPath_RefusesAnAbsoluteArgument pins a refusal that looks pedantic
// and is not.
//
// Passing an absolute path through would SILENTLY IGNORE the home anchoring
// the caller asked for by calling this function at all. The result is a path
// that looks deliberate and is simply wrong, which is harder to notice than an
// error. A caller who genuinely wants an absolute path already has one.
func TestHostPath_RefusesAnAbsoluteArgument(t *testing.T) {
	if _, err := hostPath("/etc/kubeconfig"); err == nil {
		t.Fatal("hostPath accepted an ABSOLUTE argument. It would have ignored the home " +
			"anchoring silently — a helper that anchors only sometimes is worse than one " +
			"that always does")
	}
}

// TestHostPath_RefusesEscapingHome. `../..` is asking this function to produce
// something outside the home directory, which is the one thing its name says
// it will not do.
func TestHostPath_RefusesEscapingHome(t *testing.T) {
	for _, rel := range []string{"..", "../../etc/passwd", "../sibling"} {
		if _, err := hostPath(rel); err == nil {
			t.Fatalf("hostPath(%q) escaped the home directory", rel)
		}
	}
}

// TestHostPath_RefusesEmpty — an empty argument would resolve to the home
// directory itself, which is never what a caller naming a file meant.
func TestHostPath_RefusesEmpty(t *testing.T) {
	for _, rel := range []string{"", "   "} {
		if _, err := hostPath(rel); err == nil {
			t.Fatalf("hostPath(%q) was accepted; it would resolve to the home directory itself", rel)
		}
	}
}

// TestHostPath_ErrorsNameTheArgument. A render failure reports the KCL
// expression that caused it, so the message has to carry the value.
func TestHostPath_ErrorsNameTheArgument(t *testing.T) {
	_, err := hostPath("/etc/kubeconfig")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "/etc/kubeconfig") {
		t.Fatalf("error does not name the offending argument, so a reader cannot tell WHICH "+
			"call failed: %v", err)
	}
}
