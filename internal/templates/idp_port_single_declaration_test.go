package templates

import (
	"regexp"
	"strings"
	"testing"
)

// ONE port, three readers.
//
// The dev IdP's port has to be agreed on by three things that never talk to
// each other: the container that PUBLISHES it, the provisioning job that
// DIALS it to register the browser application, and the issuer origin that
// ends up in the `iss` of every token. Before this was a declaration they
// were three literals — `${IDP_PORT:-8080}` in docker-compose.yml, and a
// baked-in "http://localhost:8080" flag default in the generated
// `auth idp-provision` command — with nothing connecting them.
//
// The failure that produced was ugly out of all proportion to the cause.
// Moving the IdP (the ordinary way to dodge a port collision on a shared
// machine) moved the CONTAINER and left the job dialling 8080, where it
// waited out its full three-minute timeout and then failed. The error named
// the job, not the port.
//
// These guards derive both sides from the rendered templates and fail if
// either stops reading the single KCL declaration. They are deliberately
// about the WIRING, not the number: a project is free to change the port,
// and changing it must move all three together.

// renderDevMainK renders the scaffolded dev environment KCL for a project
// that ships a frontend — the only shape that scaffolds an IdP at all.
func renderDevMainK(t *testing.T, template string) string {
	t.Helper()
	out, err := DeployTemplates().Render(template, struct {
		ProjectName     string
		IngressEnabled  bool
		HasFrontend     bool
		PrimaryWorkload string
	}{ProjectName: "demo", IngressEnabled: true, HasFrontend: true, PrimaryWorkload: "api"})
	if err != nil {
		t.Fatalf("render %s: %v", template, err)
	}
	return string(out)
}

// TestIdPPort_DeclaredOnceInKCL is the regression test for the two
// independent hardcodings.
//
// It asserts the dev KCL binds the port to ONE variable and that both
// consumers reference that variable — the compose service through
// `Compose.env` (which is what feeds docker compose's own `${IDP_PORT}`
// interpolation) and the idp-provision job through its `env_vars`.
func TestIdPPort_DeclaredOnceInKCL(t *testing.T) {
	for _, tmpl := range []string{"kcl/dev/main.k.tmpl", "kcl/dev/main-shared.k.tmpl"} {
		t.Run(tmpl, func(t *testing.T) {
			src := renderDevMainK(t, tmpl)

			// 1. The port is BOUND to a variable, using the deterministic
			//    allocator. resolve_port would be wrong here and the choice
			//    is load-bearing: Zitadel bakes the origin it is reached on
			//    into the `iss` of every token it mints, so a port that
			//    steps off a busy neighbour between runs invalidates issued
			//    tokens and the registered redirect URI.
			bind := regexp.MustCompile(`_idp_port\s*=\s*plugin\.allocate_port\(`)
			if !bind.MatchString(src) {
				t.Fatalf("dev KCL does not bind _idp_port with plugin.allocate_port — " +
					"the IdP port must be ONE deterministic declaration (resolve_port would let it float, " +
					"and the issuer cannot float)")
			}

			// 2. The COMPOSE service reads it. Without this the container
			//    falls back to the compose file's own `${IDP_PORT:-8080}`
			//    default and the declaration moves nothing.
			if !regexp.MustCompile(`"IDP_PORT"\s*:\s*str\(_idp_port\)`).MatchString(src) {
				t.Errorf("the idp compose service does not receive IDP_PORT from _idp_port — " +
					"the container would keep publishing on the compose file's own default, " +
					"so moving the declaration would move nothing")
			}

			// 3. The JOB reads it, for both the address it dials and the
			//    origin it registers.
			for _, name := range []string{"IDP_BASE", "IDP_BROWSER_ORIGIN"} {
				pattern := regexp.MustCompile(`name\s*=\s*"` + name + `",\s*value\s*=\s*_idp_origin`)
				if !pattern.MatchString(src) {
					t.Errorf("the idp-provision job does not receive %s from the declared port — "+
						"it would fall back to the literal http://localhost:8080 baked into its flag "+
						"defaults at project-generation time, and dial the wrong port for its whole timeout", name)
				}
			}

			// 4. The origin is DERIVED from the port rather than spelled out
			//    a second time — otherwise the job and the container can
			//    still disagree, just one level up.
			if !strings.Contains(src, `_idp_origin = "http://localhost:${_idp_port}"`) {
				t.Errorf("_idp_origin is not composed from _idp_port — a second literal origin can drift " +
					"from the port the container actually publishes on")
			}
		})
	}
}

// TestIdPPort_ComposeFileInterpolatesTheDeclaration confirms the other end
// of the wire: docker-compose.yml must publish the IdP on ${IDP_PORT}, the
// name the KCL declaration sets. If the compose file hardcoded a port
// instead, `Compose.env` would set a variable nothing reads.
func TestIdPPort_ComposeFileInterpolatesTheDeclaration(t *testing.T) {
	out, err := ProjectTemplates().Render("docker-compose.yml.tmpl", struct {
		ProjectName string
		HasFrontend bool
	}{ProjectName: "demo", HasFrontend: true})
	if err != nil {
		t.Fatalf("render docker-compose.yml.tmpl: %v", err)
	}
	src := string(out)
	if !regexp.MustCompile(`\$\{IDP_PORT[:\-]`).MatchString(src) {
		t.Fatalf("docker-compose.yml does not publish the IdP on ${IDP_PORT} — " +
			"the KCL declaration feeds that variable, so a hardcoded port here would ignore it")
	}
}

// TestDevStack_PostgresRunsOnTheHostByDefault pins the main ask: a
// scaffolded dev environment must run its database as a host process, so a
// project whose own code needs no container needs no container runtime.
//
// The compose `postgres` service still EXISTS — this is a default, not a
// removal — but the dev env must not be the thing that names it.
func TestDevStack_PostgresRunsOnTheHostByDefault(t *testing.T) {
	for _, tmpl := range []string{"kcl/dev/main.k.tmpl", "kcl/dev/main-shared.k.tmpl"} {
		t.Run(tmpl, func(t *testing.T) {
			src := renderDevMainK(t, tmpl)
			if !regexp.MustCompile(`name\s*=\s*"postgres"\s*\n\s*deploy\s*=\s*forge\.HostInfra\s*\{`).MatchString(src) {
				t.Errorf("the dev env does not declare postgres as forge.HostInfra — " +
					"dev would require docker for a database the host can run natively")
			}
			if regexp.MustCompile(`name\s*=\s*"postgres"\s*\n\s*deploy\s*=\s*forge\.Compose`).MatchString(src) {
				t.Errorf("the dev env still declares postgres as a compose service")
			}
			// The DSN must be composed from the declared port. A literal
			// would be a second spelling of the port, which is the whole
			// class of bug this environment is arranged to prevent.
			if !strings.Contains(src, "${_postgres_port}") {
				t.Errorf("the dev DATABASE_URL is not composed from _postgres_port — " +
					"a literal port can drift from the port the database actually binds")
			}
		})
	}
}

// TestDevStack_ObservabilityIsOptIn pins the lgtm/alloy decision: an LGTM
// stack is ~1 GB resident, which is the wrong default on the small machines
// host-native infra exists to serve. It must stay defined in compose (so it
// is two lines to enable) but absent from what dev actually runs.
func TestDevStack_ObservabilityIsOptIn(t *testing.T) {
	for _, tmpl := range []string{"kcl/dev/main.k.tmpl", "kcl/dev/main-shared.k.tmpl"} {
		t.Run(tmpl, func(t *testing.T) {
			src := renderDevMainK(t, tmpl)
			for _, svc := range []string{"lgtm", "alloy"} {
				live := regexp.MustCompile(`(?m)^\s{8}forge\.RenderedWorkload\s*\{\s*\n\s*name\s*=\s*"` + svc + `"`)
				if live.MatchString(src) {
					t.Errorf("%s is in the dev env's live service list — an LGTM stack is ~1 GB "+
						"resident and should be opt-in on the machines this default is for", svc)
				}
				if !strings.Contains(src, `name = "`+svc+`"`) {
					t.Errorf("%s is not even mentioned in the dev env — it should remain a "+
						"commented, copy-pasteable opt-in rather than something a user has to invent", svc)
				}
			}
		})
	}
}
