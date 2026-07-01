package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestFrontendFirebaseDeployRoundTrip renders a Frontend that declares a
// forge.FirebaseHosting deploy (with a base_path, a bundle dir, rewrites,
// and build-time env_vars) against the in-tree forge KCL module, then
// asserts the FrontendEntity parses every field. Exercises the full #1+#2
// path: the FirebaseHosting schema, _render_firebase_hosting emitting it,
// and FrontendDeployEntity.UnmarshalJSON dispatching it. Needs CGO for
// the KCL plugin.
func TestFrontendFirebaseDeployRoundTrip(t *testing.T) {
	forgeKcl, err := filepath.Abs("../../kcl")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(forgeKcl, "schema.k")); err != nil {
		t.Skipf("forge kcl module not found at %s: %v", forgeKcl, err)
	}

	root := t.TempDir()
	kclParent := filepath.Join(root, "deploy", "kcl")
	stagingDir := filepath.Join(kclParent, "staging")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mod := "[package]\nname = \"t\"\nedition = \"v0.11.4\"\n\n[dependencies]\nforge = { path = " +
		strconv.Quote(forgeKcl) + " }\n"
	if err := os.WriteFile(filepath.Join(kclParent, "kcl.mod"), []byte(mod), 0o644); err != nil {
		t.Fatal(err)
	}
	main := `import forge
_bundle = forge.Bundle {
    frontends = [forge.Frontend {
        name = "admin-web"
        type = "nextjs"
        path = "admin-web"
        env_vars = [forge.EnvVar { name = "NEXT_PUBLIC_API_URL", value = "https://api.staging.example.com" }]
        deploy = forge.FirebaseHosting {
            project = "reliant-nonprod-490701"
            site = "reliant-staging"
            public_dir = "out"
            base_path = "/admin"
            bundle = [forge.FirebaseBundleDir { src = "../reliant-web/dist", dest = "" }]
            rewrites = [{ source = "**", destination = "/index.html" }]
        }
    }]
}
output = forge.render(_bundle)
`
	if err := os.WriteFile(filepath.Join(stagingDir, "main.k"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := renderKCLRaw(context.Background(), root, "staging")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	ents, err := parseKCLEntities(out)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, out)
	}
	if len(ents.Frontends) != 1 {
		t.Fatalf("want 1 frontend, got %d: %s", len(ents.Frontends), out)
	}
	fe := ents.Frontends[0]

	if fe.Deploy == nil {
		t.Fatalf("frontend deploy not parsed: %s", out)
	}
	if fe.Deploy.Type != "firebase" {
		t.Fatalf("deploy type = %q, want firebase", fe.Deploy.Type)
	}
	fb := fe.Deploy.Firebase
	if fb == nil {
		t.Fatalf("firebase deploy block nil: %s", out)
	}
	if fb.Project != "reliant-nonprod-490701" {
		t.Errorf("project = %q", fb.Project)
	}
	if fb.Site != "reliant-staging" {
		t.Errorf("site = %q", fb.Site)
	}
	if fb.PublicDir != "out" {
		t.Errorf("public_dir = %q", fb.PublicDir)
	}
	if fb.BasePath != "/admin" {
		t.Errorf("base_path = %q", fb.BasePath)
	}
	if len(fb.Bundle) != 1 || fb.Bundle[0].Src != "../reliant-web/dist" {
		t.Errorf("bundle = %+v", fb.Bundle)
	}
	if len(fb.Rewrites) != 1 || fb.Rewrites[0]["destination"] != "/index.html" {
		t.Errorf("rewrites = %+v", fb.Rewrites)
	}

	// env_vars round-trips for the build-time injection.
	if len(fe.EnvVars) != 1 || fe.EnvVars[0].Name != "NEXT_PUBLIC_API_URL" {
		t.Errorf("env_vars = %+v", fe.EnvVars)
	}
	if fe.EnvVars[0].Value != "https://api.staging.example.com" {
		t.Errorf("env_var value = %q", fe.EnvVars[0].Value)
	}

	// The CLI→provider mapping forwards every field intact.
	ff := frontendToFirebase(fe)
	if ff.Spec.Project != fb.Project || ff.Spec.BasePath != "/admin" {
		t.Errorf("frontendToFirebase spec = %+v", ff.Spec)
	}
	if ff.BuildEnv["NEXT_PUBLIC_API_URL"] != "https://api.staging.example.com" {
		t.Errorf("frontendToFirebase build env = %+v", ff.BuildEnv)
	}
	if len(ff.Spec.Bundle) != 1 || ff.Spec.Bundle[0].Src != "../reliant-web/dist" {
		t.Errorf("frontendToFirebase bundle = %+v", ff.Spec.Bundle)
	}
}

// TestFrontendNoDeployStillRenders guards backward compatibility: a
// Frontend with no deploy block renders with a nil Deploy and does not
// error the parse — existing projects are unaffected.
func TestFrontendNoDeployStillRenders(t *testing.T) {
	raw := []byte(`{"frontends":[{"name":"web","type":"nextjs","path":"web"}]}`)
	ents, err := parseKCLEntities(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(ents.Frontends) != 1 {
		t.Fatalf("want 1 frontend, got %d", len(ents.Frontends))
	}
	if ents.Frontends[0].Deploy != nil {
		t.Errorf("expected nil Deploy for a frontend with no deploy block, got %+v", ents.Frontends[0].Deploy)
	}
}

// TestFrontendDeployNoneRendersBuildOnly renders a Frontend that
// explicitly declares `deploy = None` (the build-only case) against the
// real forge KCL module, then asserts it renders + validates and yields a
// nil Deploy entity — i.e. dispatchFrontendDeploys will treat it as
// build-only, not Firebase. Exercises the schema-level `deploy?:
// FirebaseHosting` = None path end-to-end. Needs CGO for the KCL plugin.
func TestFrontendDeployNoneRendersBuildOnly(t *testing.T) {
	forgeKcl, err := filepath.Abs("../../kcl")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(forgeKcl, "schema.k")); err != nil {
		t.Skipf("forge kcl module not found at %s: %v", forgeKcl, err)
	}

	root := t.TempDir()
	kclParent := filepath.Join(root, "deploy", "kcl")
	stagingDir := filepath.Join(kclParent, "staging")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mod := "[package]\nname = \"t\"\nedition = \"v0.11.4\"\n\n[dependencies]\nforge = { path = " +
		strconv.Quote(forgeKcl) + " }\n"
	if err := os.WriteFile(filepath.Join(kclParent, "kcl.mod"), []byte(mod), 0o644); err != nil {
		t.Fatal(err)
	}
	main := `import forge
_bundle = forge.Bundle {
    frontends = [forge.Frontend {
        name = "admin-web"
        type = "nextjs"
        path = "admin-web"
        env_vars = [forge.EnvVar { name = "NEXT_PUBLIC_API_URL", value = "https://api.staging.example.com" }]
        deploy = None
    }]
}
output = forge.render(_bundle)
`
	if err := os.WriteFile(filepath.Join(stagingDir, "main.k"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := renderKCLRaw(context.Background(), root, "staging")
	if err != nil {
		t.Fatalf("render (deploy = None should validate): %v", err)
	}
	ents, err := parseKCLEntities(out)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, out)
	}
	if len(ents.Frontends) != 1 {
		t.Fatalf("want 1 frontend, got %d: %s", len(ents.Frontends), out)
	}
	fe := ents.Frontends[0]
	if fe.Deploy != nil {
		t.Fatalf("deploy = None should yield nil Deploy (build-only), got %+v", fe.Deploy)
	}

	// The CLI→build-only mapping forwards the inline env_var as build env
	// and infers the Next.js export dir for dry-run reporting.
	bo := frontendToBuildOnly(fe)
	if bo.BuildEnv["NEXT_PUBLIC_API_URL"] != "https://api.staging.example.com" {
		t.Errorf("frontendToBuildOnly build env = %+v", bo.BuildEnv)
	}
	if bo.PublicDir != "out" {
		t.Errorf("frontendToBuildOnly PublicDir = %q, want out", bo.PublicDir)
	}
}
