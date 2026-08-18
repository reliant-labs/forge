package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// frontendRenderJSON builds the render shape a real project emits: the
// `manifests` root the deploy applies, plus the `output` JSON contract
// carrying the frontend declarations. frontends is spliced in verbatim so
// each test states only the declaration under test.
func frontendRenderJSON(frontends string) string {
	return `{
	  "manifests":[
	    {"apiVersion":"apps/v1","kind":"Deployment",
	     "metadata":{"name":"api","namespace":"ns"},
	     "spec":{"template":{"spec":{"containers":[{"name":"api","image":"api:1"}]}}}}
	  ],
	  "output":{"frontends":[` + frontends + `]}
	}`
}

// firebaseFrontend is the shape the issue was filed about: a Frontend
// with a FirebaseHosting deploy target, exactly as kcl/render.k's
// _render_firebase_hosting projects it.
const firebaseFrontend = `{"name":"reliant-web","type":"vite","path":"../reliant/web",
	"deploy":{"type":"firebase","project":"reliant-prod","site":"reliant-prod","public_dir":"dist"}}`

// buildOnlyFrontend is `deploy = None` — KCL projects an unset deploy
// block as a literal null, which is what distinguishes "compile-checked
// only" from "claims to ship".
const buildOnlyFrontend = `{"name":"internal-console","type":"nextjs","path":"frontends/internal-console",
	"deploy":null}`

// projectWithFrontends writes a forge.yaml declaring the named frontends
// and returns its directory. A project declaring none still gets a valid
// forge.yaml — "forge.yaml exists and lists nothing" and "there is no
// forge.yaml" are different states, and only the first is drift.
func projectWithFrontends(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	body := "name: proj\nmodule_path: example.com/proj\n"
	if len(names) > 0 {
		body += "frontends:\n"
		for _, n := range names {
			body += "    - name: " + n + "\n      type: nextjs\n      path: frontends/" + n + "\n"
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
	}
	return dir
}

// envForDrift wires a pre-seeded render to a project dir, the way the
// deploy checks see it in production after the first render memoises.
func envForDrift(t *testing.T, projectDir string, renders ...envRender) *Environment {
	t.Helper()
	e := envWithRender(renders)
	e.ProjectDir = projectDir
	return e
}

// THE BUG. A frontend with a FirebaseHosting target and no forge.yaml
// entry is deployable in principle and unbuildable in practice:
// `forge build --target reliant-web` answers "target not found in
// project config", which names the symptom and not the cause. Nothing
// reported it before this check — verified on the real project, where
// both `forge lint` and `forge doctor` matched zero lines for it.
func TestFrontendConfigDrift_DeployTargetIsAnError(t *testing.T) {
	dir := projectWithFrontends(t) // declares no frontends at all
	env := envForDrift(t, dir,
		renderFromJSON(t, "preprod", frontendRenderJSON(firebaseFrontend)),
		renderFromJSON(t, "prod", frontendRenderJSON(firebaseFrontend)))

	got := CheckFrontendConfigDrift(context.Background(), env)
	if got.Status != StatusFail {
		t.Fatalf("Status = %q, want %q — a frontend that declares a deploy target "+
			"claims to ship and cannot be built; that contradiction is an error.\n"+
			"message: %s\nevidence: %s", got.Status, StatusFail, got.Message, got.Evidence)
	}

	// The finding must name the frontend, both environments declaring it,
	// and the remedy — the batched, named-and-remediated style the
	// sibling forge.yaml→filesystem check uses.
	for _, want := range []string{
		"reliant-web",
		"deploy/kcl/{preprod,prod}/main.k",
		"forge build --target reliant-web",
		"forge.yaml",
	} {
		if !strings.Contains(got.Evidence, want) {
			t.Errorf("evidence does not mention %q — a finding the reader cannot act on "+
				"is the failure mode this check exists to fix.\nevidence: %s", want, got.Evidence)
		}
	}
}

// `deploy = None` makes no shipping claim. It is legitimately
// compile-checked, so an absent forge.yaml entry is a likely oversight
// rather than a contradiction — a warning, and specifically NOT a
// failure, because failing it would gate CI on a frontend nothing
// deploys.
func TestFrontendConfigDrift_BuildOnlyIsAWarningNotAnError(t *testing.T) {
	dir := projectWithFrontends(t)
	env := envForDrift(t, dir,
		renderFromJSON(t, "dev", frontendRenderJSON(buildOnlyFrontend)))

	got := CheckFrontendConfigDrift(context.Background(), env)
	if got.Status != StatusWarn {
		t.Fatalf("Status = %q, want %q — `deploy = None` makes no shipping claim, so "+
			"an absent forge.yaml entry must not fail the deployability gate.\n"+
			"message: %s\nevidence: %s", got.Status, StatusWarn, got.Message, got.Evidence)
	}
	if !strings.Contains(got.Evidence, "internal-console") {
		t.Errorf("evidence does not name the frontend: %s", got.Evidence)
	}
}

// Declared in both is the steady state and must be silent. A check that
// fires on a correct project is worse than no check — it trains the
// reader to ignore the whole report.
func TestFrontendConfigDrift_DeclaredInBothIsClean(t *testing.T) {
	dir := projectWithFrontends(t, "reliant-web")
	env := envForDrift(t, dir,
		renderFromJSON(t, "preprod", frontendRenderJSON(firebaseFrontend)),
		renderFromJSON(t, "prod", frontendRenderJSON(firebaseFrontend)))

	got := CheckFrontendConfigDrift(context.Background(), env)
	if got.Status != StatusPass {
		t.Fatalf("Status = %q, want %q — the frontend is declared in both places.\n"+
			"message: %s\nevidence: %s", got.Status, StatusPass, got.Message, got.Evidence)
	}
}

// The OPPOSITE direction is not this check's business. A frontend in
// forge.yaml that no environment's KCL declares is the ordinary shape of
// a dev-only frontend: `forge build --target` resolves it, and no
// environment claims to ship it. Reporting it here would fire on every
// project with a frontend it does not deploy.
func TestFrontendConfigDrift_ForgeYAMLOnlyIsNotReported(t *testing.T) {
	dir := projectWithFrontends(t, "docs-site")
	env := envForDrift(t, dir,
		renderFromJSON(t, "prod", frontendRenderJSON("")))

	got := CheckFrontendConfigDrift(context.Background(), env)
	if got.Status != StatusPass {
		t.Fatalf("Status = %q, want %q — a dev-only frontend declared in forge.yaml and "+
			"in no environment is legitimate, not drift.\nmessage: %s\nevidence: %s",
			got.Status, StatusPass, got.Message, got.Evidence)
	}
}

// One frontend, four environments, ONE finding. The fix is a single
// forge.yaml entry, so a per-environment finding would print the same
// instruction four times.
func TestFrontendConfigDrift_OneFindingPerFrontendAcrossEnvs(t *testing.T) {
	dir := projectWithFrontends(t)
	var renders []envRender
	for _, e := range []string{"dev", "staging", "preprod", "prod"} {
		renders = append(renders, renderFromJSON(t, e, frontendRenderJSON(firebaseFrontend)))
	}
	env := envForDrift(t, dir, renders...)

	got := CheckFrontendConfigDrift(context.Background(), env)
	if n := strings.Count(got.Evidence, "frontends[name=reliant-web]"); n != 1 {
		t.Fatalf("frontend reported %d times, want 1 — the remedy is one forge.yaml entry.\nevidence: %s",
			n, got.Evidence)
	}
	if !strings.Contains(got.Evidence, "deploy/kcl/{dev,preprod,prod,staging}/main.k") {
		t.Errorf("the finding does not name every environment declaring it: %s", got.Evidence)
	}
}

// A project mixing both cases rolls up to the worse one: a shipping
// frontend that cannot be built fails the gate even when a build-only
// one is also adrift.
func TestFrontendConfigDrift_ShippingDriftDominatesBuildOnly(t *testing.T) {
	dir := projectWithFrontends(t)
	env := envForDrift(t, dir,
		renderFromJSON(t, "prod", frontendRenderJSON(firebaseFrontend+","+buildOnlyFrontend)))

	got := CheckFrontendConfigDrift(context.Background(), env)
	if got.Status != StatusFail {
		t.Fatalf("Status = %q, want %q — one of the two claims to ship.\nevidence: %s",
			got.Status, StatusFail, got.Evidence)
	}
	for _, want := range []string{"reliant-web", "internal-console"} {
		if !strings.Contains(got.Evidence, want) {
			t.Errorf("evidence drops %q — findings are batched so one round-trip fixes all of them.\n%s",
				want, got.Evidence)
		}
	}
}

// A cluster-mode frontend ships too — it becomes a real Deployment. The
// discriminator is named in the finding so the reader knows what the
// project believes it is shipping.
func TestFrontendConfigDrift_ClusterDeployAlsoShips(t *testing.T) {
	dir := projectWithFrontends(t)
	const clusterFrontend = `{"name":"ops-console","type":"nextjs","path":"frontends/ops-console",
		"deploy":{"type":"cluster","cluster":"prod","namespace":"ns","registry":"r"}}`
	env := envForDrift(t, dir,
		renderFromJSON(t, "prod", frontendRenderJSON(clusterFrontend)))

	got := CheckFrontendConfigDrift(context.Background(), env)
	if got.Status != StatusFail {
		t.Fatalf("Status = %q, want %q — a cluster frontend renders a Deployment; it ships.\nevidence: %s",
			got.Status, StatusFail, got.Evidence)
	}
	if !strings.Contains(got.Evidence, "cluster deploy target") {
		t.Errorf("the finding does not name the deploy type: %s", got.Evidence)
	}
}

// A project with no environments is a --kind cli / library project. It
// SKIPs rather than passing: the check never asked its question.
func TestFrontendConfigDrift_NoEnvironmentsSkips(t *testing.T) {
	env := envForDrift(t, projectWithFrontends(t))
	if got := CheckFrontendConfigDrift(context.Background(), env); got.Status != StatusSkip {
		t.Fatalf("Status = %q, want %q for a project that declares no environments", got.Status, StatusSkip)
	}
}

// No forge.yaml means there is nothing for the deploy graph to disagree
// WITH. Reporting every KCL frontend as drift there would be noise, and
// a forge.yaml too broken to parse has already failed the command that
// loaded it first.
func TestFrontendConfigDrift_NoForgeYAMLSkips(t *testing.T) {
	env := envForDrift(t, t.TempDir(),
		renderFromJSON(t, "prod", frontendRenderJSON(firebaseFrontend)))

	if got := CheckFrontendConfigDrift(context.Background(), env); got.Status != StatusSkip {
		t.Fatalf("Status = %q, want %q when the project has no readable forge.yaml.\nmessage: %s",
			got.Status, StatusSkip, got.Message)
	}
}

// A render that carries no `output` contract at all (a project whose
// main.k exports only `manifests`) must not read as "declares no
// frontends, everything is fine" — but it also has nothing to report.
// The point of the test is that the parse degrades quietly instead of
// erroring the whole check.
func TestFrontendConfigDrift_ManifestsOnlyRenderIsQuiet(t *testing.T) {
	dir := projectWithFrontends(t, "web")
	body := `{"manifests":[{"apiVersion":"v1","kind":"Service","metadata":{"name":"s"}}]}`
	env := envForDrift(t, dir, renderFromJSON(t, "prod", body))

	if got := CheckFrontendConfigDrift(context.Background(), env); got.Status != StatusPass {
		t.Fatalf("Status = %q, want %q — a render exporting only `manifests` carries no "+
			"frontend contract to check.\nmessage: %s", got.Status, StatusPass, got.Message)
	}
}

// The check belongs to the deployability arm — the one `forge doctor
// --signal deploy` runs in CI. That is the whole point of the placement:
// the drift must be caught on a checkout, before a deploy is attempted.
func TestFrontendConfigDrift_IsInTheDeployabilityArm(t *testing.T) {
	for _, c := range deployabilityChecks() {
		if c.name == "Deploy Config Drift" {
			return
		}
	}
	t.Fatal("Deploy Config Drift is not in deployabilityChecks() — it would not run under " +
		"`forge doctor --signal deploy`, the arm CI calls")
}
