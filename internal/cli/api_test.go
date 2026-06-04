// Tests for `forge api curl`. The interesting logic is the pure
// buildCurlCommand function (descriptor + forge.yaml -> curl string),
// the resolver (target -> service + method), and the JSON-skeleton
// builder. We test each in isolation table-driven, and run one
// end-to-end happy-path through buildCurlCommand against a tempdir to
// pin the integration shape.
package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
)

// writeProjectWithDescriptor lays down a minimal forge project on disk: a
// forge.yaml carrying the named service+port plus a gen/forge_descriptor.json
// derived from descServices. Returns the project dir.
//
// Both pieces are required for buildCurlCommand to succeed end-to-end, and
// constructing them inline in every test would dominate the file.
func writeProjectWithDescriptor(t *testing.T, services []config.ServiceConfig, desc ForgeDescriptor) string {
	t.Helper()
	dir := t.TempDir()

	yaml := strings.Builder{}
	yaml.WriteString("name: test\n")
	yaml.WriteString("module_path: github.com/example/test\n")
	yaml.WriteString("services:\n")
	for _, s := range services {
		yaml.WriteString("  - name: " + s.Name + "\n")
		if s.Type != "" {
			yaml.WriteString("    type: " + s.Type + "\n")
		}
		if s.Port != 0 {
			yaml.WriteString("    port: ")
			yaml.WriteString(itoa(s.Port))
			yaml.WriteString("\n")
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte(yaml.String()), 0o644); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(dir, "gen"), 0o755); err != nil {
		t.Fatalf("mkdir gen: %v", err)
	}
	data, err := json.MarshalIndent(desc, "", "  ")
	if err != nil {
		t.Fatalf("marshal descriptor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gen", "forge_descriptor.json"), data, 0o644); err != nil {
		t.Fatalf("write descriptor: %v", err)
	}
	return dir
}

// itoa is strconv.Itoa inlined to avoid the import for one call site —
// keeps the test imports minimal.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestZeroValueFor pins the proto-zero mapping per scalar kind. nil for
// non-scalar branches is deliberate — the body builder renders it as
// JSON null, which ProtoJSON accepts for any nullable field.
func TestZeroValueFor(t *testing.T) {
	cases := []struct {
		proto string
		want  any
	}{
		{"string", ""},
		{"bool", false},
		{"int32", 0},
		{"int64", 0},
		{"uint32", 0},
		{"uint64", 0},
		{"sfixed64", 0},
		{"float", 0.0},
		{"double", 0.0},
		{"message", nil},
		{"enum", nil},
		{"bytes", nil},
		{"unknown-kind", nil},
	}
	for _, tc := range cases {
		got := zeroValueFor(tc.proto)
		if got != tc.want {
			t.Errorf("zeroValueFor(%q) = %v, want %v", tc.proto, got, tc.want)
		}
	}
}

// TestBuildZeroBody covers the three paths in the body builder:
// Empty-input method -> {}, missing-field-data method -> {}, and the
// happy path with field declaration order preserved.
func TestBuildZeroBody(t *testing.T) {
	svc := codegen.ServiceDef{
		Name: "UserService",
		Messages: map[string][]codegen.MessageFieldDef{
			"GetUserRequest": {
				{Name: "id", ProtoType: "string"},
				{Name: "include_deleted", ProtoType: "bool"},
				{Name: "page_size", ProtoType: "int32"},
			},
		},
	}
	cases := []struct {
		name   string
		method codegen.Method
		want   string
	}{
		{
			name:   "empty input renders empty body",
			method: codegen.Method{Name: "Ping", InputType: "google.protobuf.Empty"},
			want:   "{}",
		},
		{
			name:   "unknown input message renders empty body",
			method: codegen.Method{Name: "Unknown", InputType: "UnknownRequest"},
			want:   "{}",
		},
		{
			name:   "happy path preserves proto field order",
			method: codegen.Method{Name: "GetUser", InputType: "GetUserRequest"},
			want:   `{"id":"","include_deleted":false,"page_size":0}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildZeroBody(svc, tc.method)
			if got != tc.want {
				t.Errorf("buildZeroBody = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveServiceMethod_TableDriven exercises the resolver against a
// catalog that mixes uniquely- and ambiguously-named services. The
// short form must work when unambiguous and surface a disambiguation
// message when not.
func TestResolveServiceMethod_TableDriven(t *testing.T) {
	services := []codegen.ServiceDef{
		{
			Name:    "UserService",
			Package: "users.v1",
			Methods: []codegen.Method{
				{Name: "GetUser", InputType: "GetUserRequest"},
				{Name: "ListUsers", InputType: "ListUsersRequest"},
			},
		},
		{
			Name:    "AccountService",
			Package: "accounts.v1",
			Methods: []codegen.Method{
				{Name: "GetAccount", InputType: "GetAccountRequest"},
			},
		},
		{
			// Same Name as accounts.v1.AccountService — short form is
			// ambiguous and must be rejected.
			Name:    "AccountService",
			Package: "billing.v1",
			Methods: []codegen.Method{
				{Name: "Charge", InputType: "ChargeRequest"},
			},
		},
	}
	cases := []struct {
		name        string
		target      string
		wantPkg     string
		wantSvc     string
		wantMethod  string
		wantErrSubs string
	}{
		{
			name:       "fully qualified resolves uniquely",
			target:     "users.v1.UserService.GetUser",
			wantPkg:    "users.v1",
			wantSvc:    "UserService",
			wantMethod: "GetUser",
		},
		{
			name:       "short form resolves when unique",
			target:     "UserService.ListUsers",
			wantPkg:    "users.v1",
			wantSvc:    "UserService",
			wantMethod: "ListUsers",
		},
		{
			name:        "short form ambiguous surfaces disambiguation",
			target:      "AccountService.GetAccount",
			wantErrSubs: "ambiguous",
		},
		{
			name:        "unknown service",
			target:      "BogusService.DoThing",
			wantErrSubs: "no service",
		},
		{
			name:        "unknown method on known service",
			target:      "users.v1.UserService.BogusMethod",
			wantErrSubs: "method \"BogusMethod\" not found",
		},
		{
			name:        "missing method segment",
			target:      "UserService",
			wantErrSubs: "invalid target",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, method, err := resolveServiceMethod(services, tc.target)
			if tc.wantErrSubs != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (svc=%+v method=%+v)", tc.wantErrSubs, svc, method)
				}
				if !strings.Contains(err.Error(), tc.wantErrSubs) {
					t.Errorf("error = %q, want substring %q", err.Error(), tc.wantErrSubs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if svc.Package != tc.wantPkg || svc.Name != tc.wantSvc {
				t.Errorf("service = %s.%s, want %s.%s", svc.Package, svc.Name, tc.wantPkg, tc.wantSvc)
			}
			if method.Name != tc.wantMethod {
				t.Errorf("method = %s, want %s", method.Name, tc.wantMethod)
			}
		})
	}
}

// TestShellQuoteSingle pins the single-quote escape strategy: a string
// containing a single quote must come back as `'…'\”…'` so the shell
// re-assembles it correctly.
func TestShellQuoteSingle(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`hello`, `'hello'`},
		{`{"id":""}`, `'{"id":""}'`},
		{`it's`, `'it'\''s'`},
		{`'`, `''\'''`},
	}
	for _, tc := range cases {
		got := shellQuoteSingle(tc.in)
		if got != tc.want {
			t.Errorf("shellQuoteSingle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestMatchServicePort drives the lookupServicePort heuristic against a
// table of mixed forge.yaml shapes. Worker / operator entries must be
// skipped; the name-candidate cascade must pick the proto-package leaf
// over the raw service-name match.
func TestMatchServicePort(t *testing.T) {
	svc := codegen.ServiceDef{
		Name:    "UserService",
		Package: "users.v1",
		PkgName: "usersv1",
	}
	cases := []struct {
		name string
		cfg  *config.ProjectConfig
		want int
	}{
		{
			name: "matches by package leaf",
			cfg: &config.ProjectConfig{Services: []config.ServiceConfig{
				{Name: "users", Type: "go_service", Port: 8123},
				{Name: "other", Type: "go_service", Port: 9000},
			}},
			want: 8123,
		},
		{
			name: "matches by short service name",
			cfg: &config.ProjectConfig{Services: []config.ServiceConfig{
				{Name: "user", Type: "go_service", Port: 8200},
			}},
			want: 8200,
		},
		{
			name: "matches by go pkg name",
			cfg: &config.ProjectConfig{Services: []config.ServiceConfig{
				{Name: "usersv1", Type: "go_service", Port: 8300},
			}},
			want: 8300,
		},
		{
			name: "falls back to first go_service entry",
			cfg: &config.ProjectConfig{Services: []config.ServiceConfig{
				{Name: "unrelated", Type: "go_service", Port: 8400},
			}},
			want: 8400,
		},
		{
			name: "ignores worker / operator entries",
			cfg: &config.ProjectConfig{Services: []config.ServiceConfig{
				{Name: "users", Type: "worker", Port: 9999},
				{Name: "users-op", Type: "operator", Port: 9998},
				{Name: "other", Type: "go_service", Port: 8500},
			}},
			want: 8500,
		},
		{
			name: "no matching service returns zero",
			cfg:  &config.ProjectConfig{Services: nil},
			want: 0,
		},
		{
			name: "nil config returns zero",
			cfg:  nil,
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := matchServicePort(tc.cfg, svc)
			if got != tc.want {
				t.Errorf("matchServicePort = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestBuildCurlCommand_HappyPath drives buildCurlCommand end-to-end
// against a tempdir-shaped project. Asserts the shell text contains the
// expected URL, port, content-type, and body.
func TestBuildCurlCommand_HappyPath(t *testing.T) {
	desc := ForgeDescriptor{Services: []codegen.ServiceDef{{
		Name:    "UserService",
		Package: "users.v1",
		Methods: []codegen.Method{
			{Name: "GetUser", InputType: "GetUserRequest"},
		},
		Messages: map[string][]codegen.MessageFieldDef{
			"GetUserRequest": {
				{Name: "id", ProtoType: "string"},
			},
		},
	}}}
	dir := writeProjectWithDescriptor(t, []config.ServiceConfig{
		{Name: "users", Type: "go_service", Port: 8123},
	}, desc)

	out, err := buildCurlCommand(dir, "users.v1.UserService.GetUser", curlOptions{})
	if err != nil {
		t.Fatalf("buildCurlCommand: %v", err)
	}
	wantSubs := []string{
		"curl -X POST",
		"-H 'Content-Type: application/json'",
		`-d '{"id":""}'`,
		"http://localhost:8123/users.v1.UserService/GetUser",
	}
	for _, s := range wantSubs {
		if !strings.Contains(out, s) {
			t.Errorf("output missing %q\nfull output:\n%s", s, out)
		}
	}
}

// TestBuildCurlCommand_PortOverride pins the --port flag: a non-zero
// opts.port wins over forge.yaml's port.
func TestBuildCurlCommand_PortOverride(t *testing.T) {
	desc := ForgeDescriptor{Services: []codegen.ServiceDef{{
		Name:    "UserService",
		Package: "users.v1",
		Methods: []codegen.Method{
			{Name: "Ping", InputType: "google.protobuf.Empty"},
		},
	}}}
	dir := writeProjectWithDescriptor(t, []config.ServiceConfig{
		{Name: "users", Type: "go_service", Port: 8123},
	}, desc)

	out, err := buildCurlCommand(dir, "users.v1.UserService.Ping", curlOptions{port: 9090})
	if err != nil {
		t.Fatalf("buildCurlCommand: %v", err)
	}
	if !strings.Contains(out, ":9090/") {
		t.Errorf("--port override not applied; output:\n%s", out)
	}
	if strings.Contains(out, ":8123/") {
		t.Errorf("--port override was supposed to replace 8123; output:\n%s", out)
	}
}

// TestBuildCurlCommand_BodyOverride pins the --body flag: a non-empty
// opts.body wins over the zero-skeleton render.
func TestBuildCurlCommand_BodyOverride(t *testing.T) {
	desc := ForgeDescriptor{Services: []codegen.ServiceDef{{
		Name:    "UserService",
		Package: "users.v1",
		Methods: []codegen.Method{
			{Name: "CreateUser", InputType: "CreateUserRequest"},
		},
		Messages: map[string][]codegen.MessageFieldDef{
			"CreateUserRequest": {
				{Name: "name", ProtoType: "string"},
				{Name: "age", ProtoType: "int32"},
			},
		},
	}}}
	dir := writeProjectWithDescriptor(t, []config.ServiceConfig{
		{Name: "users", Type: "go_service", Port: 8123},
	}, desc)

	custom := `{"name":"alice","age":30}`
	out, err := buildCurlCommand(dir, "users.v1.UserService.CreateUser", curlOptions{body: custom})
	if err != nil {
		t.Fatalf("buildCurlCommand: %v", err)
	}
	if !strings.Contains(out, "-d '"+custom+"'") {
		t.Errorf("--body override not applied; output:\n%s", out)
	}
	// Default skeleton must not appear when overridden.
	if strings.Contains(out, `"age":0`) {
		t.Errorf("expected user-supplied body, not zero skeleton; output:\n%s", out)
	}
}

// TestBuildCurlCommand_StreamingContentType pins that a server- or
// client-streaming method emits Content-Type: application/connect+json
// and a streaming note instead of the default application/json.
func TestBuildCurlCommand_StreamingContentType(t *testing.T) {
	desc := ForgeDescriptor{Services: []codegen.ServiceDef{{
		Name:    "EchoService",
		Package: "echo.v1",
		Methods: []codegen.Method{
			{Name: "Stream", InputType: "StreamRequest", ServerStreaming: true},
		},
		Messages: map[string][]codegen.MessageFieldDef{
			"StreamRequest": {{Name: "id", ProtoType: "string"}},
		},
	}}}
	dir := writeProjectWithDescriptor(t, []config.ServiceConfig{
		{Name: "echo", Type: "go_service", Port: 8123},
	}, desc)

	out, err := buildCurlCommand(dir, "echo.v1.EchoService.Stream", curlOptions{})
	if err != nil {
		t.Fatalf("buildCurlCommand: %v", err)
	}
	if !strings.Contains(out, "application/connect+json") {
		t.Errorf("expected streaming content-type; output:\n%s", out)
	}
	if !strings.Contains(out, "streaming") {
		t.Errorf("expected streaming note; output:\n%s", out)
	}
}

// TestBuildCurlCommand_DefaultPortFallback pins that when forge.yaml has
// no matching service entry, we fall back to 8080 rather than failing.
// This keeps the command useful on a half-bootstrapped project where the
// descriptor has run but forge.yaml hasn't been re-scaffolded.
func TestBuildCurlCommand_DefaultPortFallback(t *testing.T) {
	desc := ForgeDescriptor{Services: []codegen.ServiceDef{{
		Name:    "UserService",
		Package: "users.v1",
		Methods: []codegen.Method{
			{Name: "Ping", InputType: "google.protobuf.Empty"},
		},
	}}}
	// No services in forge.yaml at all.
	dir := writeProjectWithDescriptor(t, nil, desc)

	out, err := buildCurlCommand(dir, "users.v1.UserService.Ping", curlOptions{})
	if err != nil {
		t.Fatalf("buildCurlCommand: %v", err)
	}
	if !strings.Contains(out, ":8080/") {
		t.Errorf("expected fallback port 8080; output:\n%s", out)
	}
}

// TestBuildCurlCommand_NoDescriptor pins the user-facing error when the
// project has never been generated. The error must name the missing
// file and point at `forge generate`.
func TestBuildCurlCommand_NoDescriptor(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"),
		[]byte("name: t\nmodule_path: github.com/example/t\n"), 0o644); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
	}
	_, err := buildCurlCommand(dir, "users.v1.UserService.GetUser", curlOptions{})
	if err == nil {
		t.Fatal("expected error for missing descriptor")
	}
	if !strings.Contains(err.Error(), "no services found") && !strings.Contains(err.Error(), "forge_descriptor.json") {
		t.Errorf("error %q should mention the missing descriptor / no services", err.Error())
	}
	if !strings.Contains(err.Error(), "forge generate") {
		t.Errorf("error %q should point at the fix (`forge generate`)", err.Error())
	}
}

// TestServiceNameCandidates pins the candidate-derivation order so a
// regression in matchServicePort surfaces here rather than via the
// downstream port-resolution test alone.
func TestServiceNameCandidates(t *testing.T) {
	svc := codegen.ServiceDef{
		Name:    "UserService",
		Package: "users.v1",
		PkgName: "usersv1",
	}
	got := serviceNameCandidates(svc)
	want := []string{"users", "user", "usersv1"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("got[%d] = %q, want %q (full=%v)", i, got[i], w, got)
		}
	}
}
