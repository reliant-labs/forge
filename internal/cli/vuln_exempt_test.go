package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/release"
)

// Each testdata/vuln_exempt/<case>.k is an env's main.k; <case>.k.golden is
// what `forge vuln exempt` writes into it. FORGE_UPDATE_GOLDEN=1 rewrites the
// goldens.
func TestExemptInKCLGolden(t *testing.T) {
	const id, reason, expires = "CVE-2026-1234", `unreachable: we never call the "parser"`, "2026-12-31"
	cases := []string{
		"vuln_gate_multiline_list", // an exemptions list: one more line
		"vuln_gate_inline_list",    // a one-line list stays one line
		"vuln_gate_no_exemptions",  // the literal gains exemptions
		"vuln_gate_one_line",       // {mode = "enforce"} gains , exemptions = [...]
		"delivery_policy_only",     // a policy with no vuln_gate gains one
		"bundle_only",              // a Bundle with no policy gains one
		"same_id_new_expiry",       // the one entry is replaced, not duplicated
		"strings_and_comments_lie", // a vuln_gate in a comment or string is not code
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			in, err := os.ReadFile(filepath.Join("testdata", "vuln_exempt", name+".k"))
			if err != nil {
				t.Fatal(err)
			}
			got, changed, err := exemptInKCL(string(in), id, reason, expires)
			if err != nil {
				t.Fatal(err)
			}
			if !changed {
				t.Fatal("reported no change")
			}
			golden := filepath.Join("testdata", "vuln_exempt", name+".k.golden")
			if os.Getenv("FORGE_UPDATE_GOLDEN") == "1" {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read %s (FORGE_UPDATE_GOLDEN=1 writes it): %v", golden, err)
			}
			if got != string(want) {
				t.Errorf("exempt %s:\n--- got\n%s\n--- want\n%s", name, got, want)
			}
			// Idempotent: the same exemption again changes nothing.
			again, changed, err := exemptInKCL(got, id, reason, expires)
			if err != nil || changed || again != got {
				t.Errorf("a second run changed the file (changed=%v, err=%v)", changed, err)
			}
		})
	}
}

func TestExemptInKCLRefusesWhatItCannotPlace(t *testing.T) {
	for name, src := range map[string]string{
		"two policies": `a = forge.DeliveryPolicy {vuln_gate = forge.VulnPolicy {mode = "audit"}}
b = forge.DeliveryPolicy {vuln_gate = forge.VulnPolicy {mode = "enforce"}}
`,
		"built elsewhere": `import ..base.env as envbase

output = envbase.render(envbase.bundle("prod"))
`,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := exemptInKCL(src, "CVE-2026-1234", "why", "2026-12-31")
			var anchor *errVulnExemptNoAnchor
			if !errors.As(err, &anchor) {
				t.Fatalf("want a refusal with the lines to add, got %v", err)
			}
			if !strings.Contains(err.Error(), `forge.VulnExemption {id = "CVE-2026-1234", reason = "why", expires = "2026-12-31"}`) {
				t.Errorf("the refusal does not give the exemption to add:\n%s", err)
			}
		})
	}
}

func TestValidateVulnExemption(t *testing.T) {
	now := time.Date(2026, 11, 20, 23, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		id, reason, expires, want string
	}{
		{"CVE-2026-1", "why", "2026-11-20", ""}, // good through today
		{"GHSA-aaaa-bbbb-cccc", "why", "2027-01-01", ""},
		{"CVE-2026-1", "why", "2026-11-19", "in the past"},
		{"CVE-2026-1", "why", "2026/12/31", "not a YYYY-MM-DD"},
		{"CVE-2026-1", "why", "", "--expires"},
		{"CVE-2026-1", "  ", "2026-12-31", "--reason is required"},
		{"CVE-2026-1", "a\nb", "2026-12-31", "one line"},
		{"CVE-2026-1", "see ${x}", "2026-12-31", "interpolation"},
		{"not an id", "why", "2026-12-31", "not an advisory id"},
	} {
		err := validateVulnExemption(tc.id, tc.reason, tc.expires, now)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s %q %s: %v", tc.id, tc.reason, tc.expires, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s %q %s: got %v, want %q", tc.id, tc.reason, tc.expires, err, tc.want)
		}
	}
}

// exemptHarness is a project with deploy/kcl/prod/main.k and a stated render.
func exemptHarness(t *testing.T, mainK string, render func(path string) (release.ShapeVulnGate, error)) (vulnDeps, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "deploy", "kcl", "prod", "main.k")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(mainK), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := vulnExemptRender
	vulnExemptRender = func(context.Context, string, string) (release.ShapeVulnGate, error) { return render(path) }
	t.Cleanup(func() { vulnExemptRender = prev })
	return vulnDeps{projectDir: func() string { return dir }, now: func() time.Time { return vulnTestNow }}, path
}

// renderFromText reads exemptions back from the written file the way a KCL
// render would see the ones it parsed: enough to tell a reached edit from one
// that missed.
func renderFromText(path string) (release.ShapeVulnGate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return release.ShapeVulnGate{}, err
	}
	var g release.ShapeVulnGate
	if strings.Contains(string(b), `id = "CVE-2026-1234"`) {
		g.Exemptions = append(g.Exemptions, release.ShapeVulnExempt{ID: "CVE-2026-1234", Reason: "not reachable", Expires: "2026-12-31"})
	}
	return g, nil
}

const exemptMainK = `import forge

bundle = forge.Bundle {
    project = "demo"
    delivery_policy = forge.DeliveryPolicy {
        vuln_gate = forge.VulnPolicy {mode = "enforce"}
    }
}
`

func TestRunVulnExemptWritesRendersAndIsIdempotent(t *testing.T) {
	deps, path := exemptHarness(t, exemptMainK, renderFromText)
	var out bytes.Buffer
	opts := vulnExemptOptions{reason: "not reachable", expires: "2026-12-31"}
	if err := runVulnExempt(context.Background(), &out, deps, "prod", "CVE-2026-1234", opts); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `vuln_gate = forge.VulnPolicy {mode = "enforce", exemptions = [forge.VulnExemption {id = "CVE-2026-1234", reason = "not reachable", expires = "2026-12-31"}]}`) {
		t.Fatalf("main.k:\n%s", b)
	}
	if !strings.Contains(out.String(), "exempted CVE-2026-1234 in env prod until 2026-12-31: deploy/kcl/prod/main.k") {
		t.Errorf("%s", out.String())
	}
	out.Reset()
	if err := runVulnExempt(context.Background(), &out, deps, "prod", "CVE-2026-1234", opts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "nothing changed") {
		t.Errorf("second run: %s", out.String())
	}
	if again, _ := os.ReadFile(path); !bytes.Equal(again, b) {
		t.Error("the second run rewrote main.k")
	}
}

// An edit the render does not see — the env reads its policy from somewhere
// else — is undone, never kept.
func TestRunVulnExemptRestoresAnEditTheRenderDoesNotSee(t *testing.T) {
	deps, path := exemptHarness(t, exemptMainK, func(string) (release.ShapeVulnGate, error) { return release.ShapeVulnGate{}, nil })
	err := runVulnExempt(context.Background(), &bytes.Buffer{}, deps, "prod", "CVE-2026-1234", vulnExemptOptions{reason: "not reachable", expires: "2026-12-31"})
	if err == nil || !strings.Contains(err.Error(), "does not reach env prod's rendered vuln_gate") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != exemptMainK {
		t.Errorf("main.k was not restored:\n%s", b)
	}
}

func TestRunVulnExemptRefusesAPastDate(t *testing.T) {
	deps, path := exemptHarness(t, exemptMainK, renderFromText)
	err := runVulnExempt(context.Background(), &bytes.Buffer{}, deps, "prod", "CVE-2026-1234", vulnExemptOptions{reason: "x", expires: "2026-01-01"})
	if err == nil || !strings.Contains(err.Error(), "in the past") {
		t.Fatalf("%v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != exemptMainK {
		t.Error("a refused exemption touched main.k")
	}
}

func TestExemptInEnvDocYAML(t *testing.T) {
	src := []byte(`apiVersion: forge.dev/env/v1
# the prod env
delivery_policy:
  vuln_gate:
    mode: enforce # opted in
`)
	got, changed, err := exemptInEnvDocYAML(src, "CVE-2026-1234", "not reachable", "2026-12-31")
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	for _, want := range []string{"# the prod env", "mode: enforce # opted in", "exemptions:", "- id: CVE-2026-1234", "reason: not reachable", `expires: "2026-12-31"`} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
	again, changed, err := exemptInEnvDocYAML([]byte(got), "CVE-2026-1234", "not reachable", "2026-12-31")
	if err != nil || changed || again != got {
		t.Errorf("not idempotent: changed=%v err=%v", changed, err)
	}
	renewed, _, err := exemptInEnvDocYAML([]byte(got), "CVE-2026-1234", "still not reachable", "2027-03-31")
	if err != nil || strings.Count(renewed, "CVE-2026-1234") != 1 || !strings.Contains(renewed, "2027-03-31") {
		t.Errorf("renewal duplicated or lost the entry (%v):\n%s", err, renewed)
	}
	if _, _, err := exemptInEnvDocYAML([]byte("apiVersion: forge.dev/env/v1\n"), "CVE-2026-1234", "x", "2026-12-31"); err == nil {
		t.Error("a document with no vuln_gate was edited")
	}
}
