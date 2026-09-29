package shellbuildtokens

import "testing"

// The real control-plane shape: a raw string, which is the ONLY way a token
// survives KCL, carrying the silent one. This is the case the rule exists for.
func TestScanSourceFlagsRawStringToken(t *testing.T) {
	src := `import forge
import forge.workloads as fw

_reliant = fw.Workload {
    name = "reliant"
    build = forge.ShellBuild {
        cmd = r"""
        cd ../reliant && GOARCH=${TARGETARCH} go build -o bin/reliant ./cmd/reliant
        docker build -t ${REGISTRY}/${IMAGE}:${TAG} .
        """
        cwd = "."
    }
    runtime = forge.BuildOnly {}
}
`
	got := ScanSource("deploy/kcl/e2e/main.k", src)
	if len(got) != 4 {
		t.Fatalf("expected 4 findings (IMAGE REGISTRY TAG TARGETARCH), got %d: %+v", len(got), got)
	}
	for _, f := range got {
		if f.Workload != "reliant" {
			t.Errorf("workload = %q, want reliant (read from the enclosing literal)", f.Workload)
		}
		if f.File != "deploy/kcl/e2e/main.k" {
			t.Errorf("file = %q", f.File)
		}
		if f.Line != 6 {
			t.Errorf("line = %d, want 6 (the ShellBuild literal)", f.Line)
		}
	}
}

// A build that declares the token in its env map is correct and must pass —
// this is the migration path the remediation text offers, so the scanner has
// to honour it too, not just Check.
func TestScanSourceHonoursDeclaredEnv(t *testing.T) {
	src := `_w = fw.Workload {
    name = "reliant"
    build = forge.ShellBuild {
        cmd = r"GOARCH=${TARGETARCH} go build ./cmd/reliant"
        env = {"TARGETARCH" = forge.target_arch()}
    }
}
`
	if got := ScanSource("main.k", src); len(got) != 0 {
		t.Errorf("TARGETARCH is declared in env — want no findings, got %+v", got)
	}
}

// A ShellBuild written the NEW way — KCL interpolation of the accessors — must
// produce nothing. This is the shape the docs teach, so a false positive here
// would flag every correctly migrated project.
func TestScanSourcePassesPlainKCLShellBuild(t *testing.T) {
	src := `_w = fw.Workload {
    name = "reliant"
    build = forge.ShellBuild {
        cmd = "GOARCH=${_arch} go build -o bin/reliant ./cmd/reliant && docker build -t ${_ref} ."
        cwd = "../reliant"
    }
}
`
	if got := ScanSource("main.k", src); len(got) != 0 {
		t.Errorf("KCL interpolation of local values is the correct shape — want no findings, got %+v", got)
	}
}

// Several builds in one file are each judged on their own, and each names its
// own workload — a file-level scan that attributed a finding to the wrong
// workload would send the user to the wrong declaration.
func TestScanSourceAttributesPerWorkload(t *testing.T) {
	src := `_a = fw.Workload {
    name = "alpha"
    build = forge.ShellBuild {cmd = r"echo ${ENV}"}
}
_b = fw.Workload {
    name = "beta"
    build = forge.ShellBuild {cmd = "echo fine"}
}
_c = fw.Workload {
    name = "gamma"
    build = forge.ShellBuild {cmd = r"echo ${SERVICE}"}
}
`
	got := ScanSource("main.k", src)
	if len(got) != 2 {
		t.Fatalf("expected 2 findings, got %d: %+v", len(got), got)
	}
	if got[0].Workload != "alpha" || got[0].Token != "ENV" {
		t.Errorf("first finding = %+v, want alpha/ENV", got[0])
	}
	if got[1].Workload != "gamma" || got[1].Token != "SERVICE" {
		t.Errorf("second finding = %+v, want gamma/SERVICE", got[1])
	}
}
