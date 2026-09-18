package codegen

import (
	"strings"
	"testing"
)

// blockCollisionMessages is the shape that broke: TWO composed config blocks
// that each declare a leaf named `base_domain`. Nothing about that is a proto
// error — a block is its own namespace, `StaticSiteConfig.base_domain` and
// `SimpleBackendConfig.base_domain` are distinct fields, and the Go loader
// binds each to its own env var. It is only the KCL projection that flattens
// every block's leaves into ONE `schema AppConfig`, and it is that flattening
// that turns two legal fields into one illegal declaration.
func blockCollisionMessages() []ConfigMessage {
	return []ConfigMessage{
		{Name: "AppConfig", Fields: []ConfigField{
			{Name: "port", GoName: "Port", GoType: "int32", ProtoType: "int32", EnvVar: "PORT", DefaultValue: "8080", ProtoFile: "config/v1/config.proto"},
			{Name: "static_site", GoName: "StaticSite", ProtoType: "message", MessageType: "StaticSiteConfig", ProtoFile: "config/v1/config.proto"},
			{Name: "simple_backend", GoName: "SimpleBackend", ProtoType: "message", MessageType: "SimpleBackendConfig", ProtoFile: "config/v1/config.proto"},
		}},
		{Name: "StaticSiteConfig", Fields: []ConfigField{
			{Name: "gcp_project", GoName: "GcpProject", GoType: "string", ProtoType: "string", EnvVar: "STATIC_SITE_GCP_PROJECT", ProtoFile: "config/v1/config.proto"},
			{Name: "base_domain", GoName: "BaseDomain", GoType: "string", ProtoType: "string", EnvVar: "STATIC_SITE_BASE_DOMAIN", ProtoFile: "config/v1/config.proto"},
		}},
		{Name: "SimpleBackendConfig", Fields: []ConfigField{
			{Name: "base_domain", GoName: "BaseDomain", GoType: "string", ProtoType: "string", EnvVar: "SIMPLE_BACKEND_BASE_DOMAIN", ProtoFile: "config/v1/config.proto"},
			{Name: "allowed_image_registries", GoName: "AllowedImageRegistries", GoType: "string", ProtoType: "string", EnvVar: "SIMPLE_BACKEND_ALLOWED_IMAGE_REGISTRIES", ProtoFile: "config/v1/config.proto"},
		}},
	}
}

// TestComposedBlocksWithCollidingLeafNamesStillProject is the regression.
//
// Before the fix, FlattenBlockLeaves did not exist and callers appended each
// block's leaves under their BARE names. Two blocks declaring `base_domain`
// therefore produced two `base_domain` entries in one schema,
// CheckDuplicateConfigFields refused the whole AppConfig, and — because
// `forge generate` downgrades a config-generation failure to a WARNING — the
// previous config_gen.k stayed on disk. The observable result was not an
// error an author would chase: it was EVERY leaf of BOTH blocks silently
// missing from deploy/kcl/config_gen.k, so a tier whose on-switch lived in
// one of them could not be turned on from KCL at all, and the generated file
// looked plausible because the fields had never been there to begin with.
//
// The assertion is per-leaf on purpose. Asserting only that generation
// SUCCEEDS would pass against an implementation that dropped both blocks
// quietly, which is the exact failure being fixed.
func TestComposedBlocksWithCollidingLeafNamesStillProject(t *testing.T) {
	fields := FlattenBlockLeaves(blockCollisionMessages(), "AppConfig")

	out, err := GenerateConfigKCL(fields, "proj")
	if err != nil {
		t.Fatalf("two blocks declaring the same leaf name must still project; got: %v", err)
	}

	for _, env := range []string{
		"STATIC_SITE_BASE_DOMAIN",
		"STATIC_SITE_GCP_PROJECT",
		"SIMPLE_BACKEND_BASE_DOMAIN",
		"SIMPLE_BACKEND_ALLOWED_IMAGE_REGISTRIES",
		"PORT",
	} {
		if !strings.Contains(out, env) {
			t.Errorf("%s missing from the projection — a block leaf was dropped", env)
		}
	}
}

// TestCollidingLeavesAreDistinctSchemaFields pins that the two leaves become
// two SEPARATE KCL fields whose values can differ.
//
// This is the assertion with the teeth. "Both env vars appear" is satisfied
// by an implementation that emits one `base_domain` schema field and reads it
// twice — which would compile, render, and make the static-site and
// simple-backend domains permanently equal. For this tier that is not a
// cosmetic bug: the proto requires simple-backend's domain to be SEPARATE
// from the api host's, because customer-deployed apps run untrusted code and
// a shared parent domain puts the api's cookies in their reach. So the two
// must be independently settable, and this test fails if they are ever fused.
func TestCollidingLeavesAreDistinctSchemaFields(t *testing.T) {
	fields := FlattenBlockLeaves(blockCollisionMessages(), "AppConfig")

	var names []string
	for _, f := range fields {
		if f.EnvVar == "STATIC_SITE_BASE_DOMAIN" || f.EnvVar == "SIMPLE_BACKEND_BASE_DOMAIN" {
			names = append(names, f.Name)
		}
	}
	if len(names) != 2 {
		t.Fatalf("expected both base_domain leaves, got %v", names)
	}
	if names[0] == names[1] {
		t.Fatalf("the two base_domain leaves projected to ONE schema field %q; "+
			"static-site and simple-backend domains would be permanently equal, "+
			"and this tier requires them to differ", names[0])
	}

	out, err := GenerateConfigKCL(fields, "proj")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, n := range names {
		if !strings.Contains(out, "    "+n+":") {
			t.Errorf("schema field %q not declared in the emitted schema", n)
		}
	}
	// Each env var must read its OWN schema field, not share one.
	for _, pair := range [][2]string{
		{"STATIC_SITE_BASE_DOMAIN", names[0]},
		{"SIMPLE_BACKEND_BASE_DOMAIN", names[1]},
	} {
		want := `"` + pair[0] + `" = {value = c.` + pair[1] + `}`
		if !strings.Contains(out, want) {
			t.Errorf("projection line %q missing; env var is not bound to its own field", want)
		}
	}
}

// TestNonCollidingLeavesKeepTheirBareNames pins that the disambiguation is
// applied ONLY where it is needed.
//
// Every existing per-env config.k authors values by BARE leaf name
// (`github_client_id = "..."`, `log_level = "debug"`). Qualifying every block
// leaf unconditionally would be a cleaner rule and would silently invalidate
// every one of those files at once — a KCL error per line, in files forge
// does not own and cannot migrate. So the rule is deliberately narrow: a name
// is qualified only when two blocks actually claim it, which is the only case
// that was broken and the only case with no working spelling today.
func TestNonCollidingLeavesKeepTheirBareNames(t *testing.T) {
	fields := FlattenBlockLeaves(blockCollisionMessages(), "AppConfig")

	byEnv := map[string]string{}
	for _, f := range fields {
		byEnv[f.EnvVar] = f.Name
	}
	if got := byEnv["STATIC_SITE_GCP_PROJECT"]; got != "gcp_project" {
		t.Errorf("non-colliding leaf renamed to %q; existing config.k files author it as gcp_project", got)
	}
	if got := byEnv["SIMPLE_BACKEND_ALLOWED_IMAGE_REGISTRIES"]; got != "allowed_image_registries" {
		t.Errorf("non-colliding leaf renamed to %q; want allowed_image_registries", got)
	}
	if got := byEnv["PORT"]; got != "port" {
		t.Errorf("root scalar renamed to %q; want port", got)
	}
}

// TestDuplicateRefusalStillFiresForRealCollisions pins that this fix did not
// defang CheckDuplicateConfigFields.
//
// That check exists because KCL keeps the LAST declaration of a repeated
// schema field silently, and a shadowed default once shipped an empty APP_URL
// to three prod workloads. Disambiguating BLOCK-QUALIFIED names must not make
// a genuine same-namespace duplicate — one message declaring a name twice —
// projectable.
func TestDuplicateRefusalStillFiresForRealCollisions(t *testing.T) {
	fields := []ConfigField{
		{Name: "app_url", GoName: "AppUrl", GoType: "string", ProtoType: "string", EnvVar: "APP_URL", DefaultValue: "http://localhost:3000", ProtoFile: "a.proto"},
		{Name: "app_url", GoName: "AppUrl", GoType: "string", ProtoType: "string", EnvVar: "APP_URL", ProtoFile: "b.proto"},
	}
	if _, err := GenerateConfigKCL(fields, "proj"); err == nil {
		t.Fatal("a genuine duplicate field name must still be refused")
	}
}
