package lint

import (
	"strings"
	"testing"

	"golang.org/x/mod/semver"

	"github.com/reliant-labs/forge/internal/codegen"
)

func TestUnprovenSignup_FlagsSignUpWithNoMailAndLegacySelfSignupOutsideDev(t *testing.T) {
	root := t.TempDir()
	writeOpenSignupFile(t, root, "deploy/kcl/prod/main.k", `import forge

# delivery = "none" in a comment declares nothing
IDENTITY = forge.ManagedIdentity {
    signup = "open"
    email = forge.IdentityEmail {delivery = "none"}
}
`)
	writeOpenSignupFile(t, root, "deploy/kcl/staging/main.k", `API = forge.Service {
    env_vars = {"AUTH_SELF_SIGNUP" = "true"}
}
`)
	// Development turns legacy sign-up on anyway; mail-delivering envs prove.
	writeOpenSignupFile(t, root, "deploy/kcl/dev/main.k", `API = forge.Service {env_vars = {"AUTH_SELF_SIGNUP" = "true"}}
`)
	writeOpenSignupFile(t, root, "deploy/kcl/preview/main.k", `IDENTITY = forge.ManagedIdentity {signup = "open"}
`)
	found, err := findUnprovenSignups(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 || found[0].Where != "deploy/kcl/prod/main.k:5" || found[1].Where != "deploy/kcl/staging/main.k:2" {
		t.Fatalf("findings = %+v, want prod's no-mail sign-up and staging's legacy self-sign-up", found)
	}
	js, err := collectUnprovenSignupJSON(root)
	if err != nil || len(js) != 2 || js[0].Rule != ruleUnprovenSignup || js[0].Line != 5 || js[0].Severity != "error" {
		t.Fatalf("json = %+v, %v", js, err)
	}
}

// The config projection forge generates into every scaffold's
// deploy/kcl/config_gen.k maps auth_self_signup onto AUTH_SELF_SIGNUP with
// `"true" if c.auth_self_signup else "false"`. That turns nothing on: the
// lane flagged it in every fresh scaffold, and a scaffold's own lint went red
// (TestE2EFreshScaffoldLintExitsZero and six other e2e tests). Rendered by
// the real generator, so a change to its spelling is judged here too.
func TestUnprovenSignup_TheGeneratedConfigProjectionDeclaresNothing(t *testing.T) {
	field := func(def string) []codegen.ConfigField {
		return []codegen.ConfigField{{
			Name: "auth_self_signup", GoName: "AuthSelfSignup", GoType: "bool", ProtoType: "bool",
			EnvVar: "AUTH_SELF_SIGNUP", Flag: "auth-self-signup", DefaultValue: def,
		}}
	}
	gen, err := codegen.GenerateConfigKCL(field("false"), "proj")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gen, `"AUTH_SELF_SIGNUP" = "true" if c.auth_self_signup else "false"`) {
		t.Fatalf("the projection no longer maps the bool this way; re-check what the lane reads:\n%s", gen)
	}
	root := t.TempDir()
	writeOpenSignupFile(t, root, "deploy/kcl/config_gen.k", gen)
	if found, err := findUnprovenSignups(root); err != nil || len(found) != 0 {
		t.Fatalf("the generated projection (default false) = %+v, %v; want no finding", found, err)
	}

	// A proto default of true IS sign-up on in every env, prod included.
	gen, err = codegen.GenerateConfigKCL(field("true"), "proj")
	if err != nil {
		t.Fatal(err)
	}
	writeOpenSignupFile(t, root, "deploy/kcl/config_gen.k", gen)
	if found, err := findUnprovenSignups(root); err != nil || len(found) != 1 ||
		found[0].Rule != ruleUnprovenSignupDefault || !strings.HasPrefix(found[0].Where, "deploy/kcl/config_gen.k:") {
		t.Fatalf("a schema default of True = %+v, %v; want one %s finding in config_gen.k", found, err, ruleUnprovenSignupDefault)
	}
}

// Each way an env turns legacy sign-up on is found, under the rule that
// owns it; each that only reads or maps the value is not.
func TestUnprovenSignup_LegacySelfSignupSpellings(t *testing.T) {
	for src, want := range map[string]string{
		`CONFIG = AppConfig {auth_self_signup = True}`:                          ruleUnprovenSignup,
		"CONFIG = AppConfig {\n    auth_self_signup = True  # opt in\n}":        ruleUnprovenSignup,
		`env_vars = {"AUTH_SELF_SIGNUP" = "true", "X" = "y"}`:                   ruleUnprovenSignup,
		`env_vars = {"AUTH_SELF_SIGNUP": "1"}`:                                  ruleUnprovenSignup,
		`auth_self_signup: bool = True`:                                         ruleUnprovenSignupDefault,
		`CONFIG = AppConfig {auth_self_signup = False}`:                         "",
		`auth_self_signup: bool = False`:                                        "",
		`"AUTH_SELF_SIGNUP" = "true" if c.auth_self_signup else "false"`:        "",
		`"AUTH_SELF_SIGNUP" = "true" if cfg.auth_self_signup else "false", "X"`: "",
	} {
		root := t.TempDir()
		writeOpenSignupFile(t, root, "deploy/kcl/prod/main.k", src+"\n")
		found, err := findUnprovenSignups(root)
		if err != nil {
			t.Fatal(err)
		}
		var got string
		if len(found) == 1 {
			got = found[0].Rule
		}
		if got != want || len(found) > 1 {
			t.Errorf("%q: findings = %+v, want rule %q", src, found, want)
		}
	}
}

// The schema-default finding joined the lane after forgeconv-unproven-signup
// shipped, so it carries its own rollout: a project pinned past the original
// rule's GatesFrom but before the new rule's sees the newly found default as
// a warning with the grace note, while a literal true outside development
// still gates. From the new rule's GatesFrom both gate.
func TestUnprovenSignup_TheSchemaDefaultWarnsBeforeItGates(t *testing.T) {
	root := t.TempDir()
	writeOpenSignupFile(t, root, "deploy/kcl/config_gen.k", "schema AppConfig:\n    auth_self_signup: bool = True\n")
	writeOpenSignupFile(t, root, "deploy/kcl/prod/main.k", `API = forge.Service {env_vars = {"AUTH_SELF_SIGNUP" = "true"}}`+"\n")
	off := map[string]bool{}
	for _, step := range lintPipeline() {
		if step.name != unprovenSignupLane {
			off[step.name] = true
		}
	}
	old, okOld := lintRollouts[ruleUnprovenSignup]
	ro, okNew := lintRollouts[ruleUnprovenSignupDefault]
	if !okOld || !okNew {
		t.Fatalf("rollout rows: %s=%v %s=%v", ruleUnprovenSignup, okOld, ruleUnprovenSignupDefault, okNew)
	}
	if semver.Compare(ro.IntroducedIn, old.GatesFrom) < 0 {
		t.Fatalf("%s is introduced in %s, before %s gates (%s): the grace-period pin below would not be past the old rule's",
			ruleUnprovenSignupDefault, ro.IntroducedIn, ruleUnprovenSignup, old.GatesFrom)
	}
	sweep := func(pin string) (byRule map[string]lintJSONFinding, gated bool) {
		t.Helper()
		outcomes := collectLintOutcomes(&lintRunCtx{ctx: t.Context(), cwd: root, cfg: pinnedCfg(pin, nil), lanesOff: off})
		fs, gated := flattenLintOutcomes(outcomes)
		byRule = map[string]lintJSONFinding{}
		for _, f := range fs {
			if f.Rule == ruleUnprovenSignup || f.Rule == ruleUnprovenSignupDefault {
				byRule[f.Rule] = f
			}
		}
		if len(byRule) != 2 {
			t.Fatalf("pin %s: findings = %+v, want one of each rule", pin, fs)
		}
		return byRule, gated
	}

	// In the grace period: past the old rule's GatesFrom, before the new one's.
	got, gated := sweep(ro.IntroducedIn)
	if f := got[ruleUnprovenSignup]; f.Severity != lintSevError || f.Grace != nil || f.File != "deploy/kcl/prod/main.k" {
		t.Errorf("pin %s: the literal true outside dev = %+v, want an ungraced error", ro.IntroducedIn, f)
	}
	f := got[ruleUnprovenSignupDefault]
	if f.Severity == lintSevError || f.Grace == nil || f.File != "deploy/kcl/config_gen.k" {
		t.Fatalf("pin %s: the schema default = %+v, want a warning carrying the grace note", ro.IntroducedIn, f)
	}
	wantNote := "is new in " + ro.IntroducedIn + " and gates from " + ro.GatesFrom
	if note := f.Grace.note(ruleUnprovenSignupDefault); !strings.Contains(note, wantNote) {
		t.Errorf("grace note = %q, want it to say %q", note, wantNote)
	}
	if !gated {
		t.Errorf("pin %s: the lane did not gate, but the literal true outside dev is an error", ro.IntroducedIn)
	}

	// From the new rule's GatesFrom, both gate.
	got, _ = sweep(ro.GatesFrom)
	for rule, f := range got {
		if f.Severity != lintSevError || f.Grace != nil {
			t.Errorf("pin %s: %s = %+v, want an ungraced error", ro.GatesFrom, rule, f)
		}
	}
}

func TestUnprovenSignup_NothingWithoutADeclaration(t *testing.T) {
	if found, err := findUnprovenSignups(t.TempDir()); err != nil || len(found) != 0 {
		t.Fatalf("= %v, %v", found, err)
	}
}

// The lane is wired into the sweep a bare `forge lint` runs, and graded by
// the rollout table there: run through collectLintOutcomes (every other lane
// switched off, so no toolchain runs), a production sign-up with no mail
// warns while the pin is in the grace period and gates from GatesFrom.
// forge#2179 review: a merge once dropped the lane from lintPipeline while
// the collector's own tests stayed green.
func TestUnprovenSignup_TheBareSweepRunsAndGradesIt(t *testing.T) {
	root := t.TempDir()
	writeOpenSignupFile(t, root, "deploy/kcl/prod/main.k", `import forge

IDENTITY = forge.ManagedIdentity {
    signup = "open"
    email = forge.IdentityEmail {delivery = "none"}
}
`)
	off := map[string]bool{}
	for _, step := range lintPipeline() {
		if step.name != unprovenSignupLane {
			off[step.name] = true
		}
	}
	// Graded through a synthetic row: the rule's own grace window closed.
	ro := ruleRollout{IntroducedIn: "v1.0.0", GatesFrom: "v1.0.1"}
	withRollouts(t, map[string]ruleRollout{ruleUnprovenSignup: ro})
	sweep := func(pin string) ([]lintJSONFinding, bool) {
		t.Helper()
		outcomes := collectLintOutcomes(&lintRunCtx{ctx: t.Context(), cwd: root, cfg: pinnedCfg(pin, nil), lanesOff: off})
		fs, gated := flattenLintOutcomes(outcomes)
		var mine []lintJSONFinding
		for _, f := range fs {
			if f.Rule == ruleUnprovenSignup {
				mine = append(mine, f)
			}
		}
		if len(mine) != 1 || mine[0].File != "deploy/kcl/prod/main.k" {
			t.Fatalf("pin %s: the bare sweep's %s findings = %+v, want prod's no-mail sign-up (all findings %+v)", pin, ruleUnprovenSignup, mine, fs)
		}
		return mine, gated
	}
	if fs, gated := sweep(ro.IntroducedIn); gated || fs[0].Severity == lintSevError {
		t.Errorf("pin %s (before GatesFrom %s): want a warning that does not gate, got gated=%v %+v", ro.IntroducedIn, ro.GatesFrom, gated, fs)
	}
	if fs, gated := sweep(ro.GatesFrom); !gated || fs[0].Severity != lintSevError {
		t.Errorf("pin %s: want an error that gates, got gated=%v %+v", ro.GatesFrom, gated, fs)
	}
}
