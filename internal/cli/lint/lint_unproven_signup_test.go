package lint

import (
	"testing"
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

func TestUnprovenSignup_NothingWithoutADeclaration(t *testing.T) {
	if found, err := findUnprovenSignups(t.TempDir()); err != nil || len(found) != 0 {
		t.Fatalf("= %v, %v", found, err)
	}
}
