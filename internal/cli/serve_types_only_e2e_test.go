//go:build e2e

package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/pgtest"
)

// TestE2EUnmountedServiceIsFailClosed is the modern replacement for the
// retired TestE2ERegistrationTypesOnlyService.
//
// WHY IT WAS REWRITTEN RATHER THAN DELETED. The old test drove the
// registration-in-code lifecycle: a user-owned pkg/app/services.go row list,
// serviceRow<X> constructors, and BootstrapOnly's string-keyed registration
// guard. That whole mechanism is gone — a fresh project's pkg/app/ holds only
// CONVENTIONS.md, and mount selection is now compile-time typed via
// (*app.Components).Mount<Svc> method expressions with no string→func table
// anywhere on the run path. Its skip comment pointed at the services.go PARSER
// unit tests (TestServiceRegistry_* in generate_serve_test.go) as residual
// coverage, and those do still exist. But they cover the parser for MIGRATED
// projects that still carry the file; they say nothing about the guarantee
// that replaced the registration guard. Deleting outright would have left that
// guarantee — the boot-time completeness gate — with no e2e coverage at all,
// so this file now pins the current model instead.
//
// THE QUESTION IS THE SAME ONE, ASKED OF THE NEW MODEL: what happens to a
// service whose proto generates and whose handler dir exists, but which a
// given binary does not mount? Four answers, in order:
//
//	A. The typed mount method IS generated for it — being unmounted is a
//	   composition choice, never a gap in what forge emitted.
//	B. Inventory lists it, so `forge project map` / `forge project audit`
//	   still see it. Introspection is data-only and independent of mounting.
//	C. FAIL-CLOSED: the all-services command sets RequireComplete: true, and
//	   a binary that declares a service but does not mount it REFUSES TO
//	   BOOT, naming the service. This is the guarantee that replaced the old
//	   registration guard, and it is what makes a silent 404 in production
//	   unreachable.
//	D. A deliberate subset mount, with the gate off, boots fine and serves
//	   ONLY the chosen service. The shipped per-service subcommand is exactly
//	   this shape.
//
// C and D are two halves of one design and are only meaningful together: the
// gate must refuse the accident and permit the intent.
func TestE2EUnmountedServiceIsFailClosed(t *testing.T) {
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	runCmd(t, dir, forgeBin,
		"project", "new", "tonly",
		"--mod", "github.com/test/tonly",
		"--service", "api",
	)
	projectDir := filepath.Join(dir, "tonly")
	assertPathExistsE2E(t, filepath.Join(projectDir, "forge.yaml"))

	// The scaffolded api service has no RPCs, and a zero-RPC service mounts
	// no routes at all — which would make "not mounted" and "mounted but
	// empty" indistinguishable over HTTP in phase D. Give it one RPC so the
	// served/not-served probe has something real to hit.
	apiProtoPath := filepath.Join(projectDir, "proto", "services", "api", "v1", "api.proto")
	apiProto := readFileE2E(t, apiProtoPath)
	const rpcTODO = "  // TODO: Add your RPC methods here.\n"
	if !strings.Contains(apiProto, rpcTODO) {
		t.Fatalf("scaffolded api.proto no longer carries the RPC TODO anchor:\n%s", apiProto)
	}
	apiProto = strings.Replace(apiProto, rpcTODO, "  rpc Ping(PingRequest) returns (PingResponse) {}\n", 1)
	apiProto += "\nmessage PingRequest {}\n\nmessage PingResponse {\n  string ok = 1;\n}\n"
	if err := os.WriteFile(apiProtoPath, []byte(apiProto), 0o644); err != nil {
		t.Fatalf("write api.proto: %v", err)
	}

	// Declare a SECOND service. Its canonical implementation lives in a
	// sibling binary (the control-plane shape); this project generates its
	// types and handler dir but a carved binary may well not serve it.
	protoDir := filepath.Join(projectDir, "proto", "services", "project", "v1")
	if err := os.MkdirAll(protoDir, 0o755); err != nil {
		t.Fatalf("mkdir proto dir: %v", err)
	}
	const projectProto = `syntax = "proto3";

package services.project.v1;

option go_package = "github.com/test/tonly/gen/services/project/v1;projectv1";

// ProjectService is canonically served by a sibling binary; this project
// generates its types and its typed mount, and composition decides whether a
// given binary actually mounts it.
service ProjectService {
  rpc GetProject(GetProjectRequest) returns (GetProjectResponse) {}
}

message GetProjectRequest {
  string id = 1;
}

message GetProjectResponse {
  string id = 1;
  string name = 2;
}
`
	if err := os.WriteFile(filepath.Join(protoDir, "project.proto"), []byte(projectProto), 0o644); err != nil {
		t.Fatalf("write project.proto: %v", err)
	}

	// Wire the unpublished forge/pkg to local sources, same as the
	// fixture-corpus harness (serverkit's completeness gate — the thing
	// under test — is newer than any published snapshot).
	addCorpusForgePkgReplace(t, projectDir)

	runCmd(t, projectDir, forgeBin, "generate")

	// ── A. The typed mount surface is generated for BOTH services ───────
	//
	// Not being mounted is a composition decision. Forge still emits
	// everything needed to mount it, so turning it on is a one-line change
	// and never a regeneration.
	assertPathExistsE2E(t, filepath.Join(projectDir, "gen", "services", "project", "v1"))
	assertPathExistsE2E(t, filepath.Join(projectDir, "internal", "handlers", "project"))

	mounts := readFileE2E(t, filepath.Join(projectDir, "internal", "app", "mounts_services_gen.go"))
	for _, method := range []string{
		"func (c *Components) MountProject(",
		"func (c *Components) MountAPI(",
		"func (c *Components) MountAll(",
	} {
		if !strings.Contains(mounts, method) {
			t.Errorf("mounts_services_gen.go must generate %q:\n%s", method, mounts)
		}
	}
	// The typed method expression is what a subset command names; it exists
	// per-service in its own generated file next to the user-owned command.
	projectMountExpr := readFileE2E(t, filepath.Join(projectDir,
		"cmd", "tonly", "cmd", "services", "project_mount_gen.go"))
	if !strings.Contains(projectMountExpr, "(*app.Components).MountProject") {
		t.Errorf("per-service mount file must name the typed method expression:\n%s", projectMountExpr)
	}

	// ── B. Inventory lists it — introspection is independent of mounting ─
	//
	// Inventory is DATA-ONLY (`forge project map` / `audit` / the services
	// listing read it; the run path never does), so a service that no binary
	// mounts is still fully discoverable.
	for _, row := range []string{
		`Name:        "project"`,
		"ConnectPath: projectv1connect.ProjectServiceName",
	} {
		if !strings.Contains(mounts, row) {
			t.Errorf("Inventory must carry the project row %q:\n%s", row, mounts)
		}
	}
	assertAuditSeesService(t, projectDir, forgeBin, "project", "GetProject")

	runCmd(t, projectDir, "go", "build", "./...")

	// One postgres for both boots below: the scaffolded config declares
	// database_url REQUIRED and OpenInfra pings it, so "no database" is not
	// a bootable configuration and neither phase could run without it.
	dsn, cleanup, err := pgtest.NewURL()
	if err != nil {
		t.Fatalf("provision boot postgres: %v", err)
	}
	defer cleanup()

	// ── C. FAIL-CLOSED: declared but not mounted refuses to boot ────────
	//
	// Carve the all-services command down to one service while leaving
	// RequireComplete: true — precisely the mistake the gate exists to
	// catch, and exactly what a half-finished refactor looks like. Boot must
	// FAIL and must NAME the unmounted service; a server that came up here
	// would answer production traffic for ProjectService with a 404.
	serverCmdPath := filepath.Join(projectDir, "cmd", "tonly", "cmd", "server.go")
	allServicesCmd := readFileE2E(t, serverCmdPath)
	const mountAllLine = "Mount:           (*app.Components).MountAll,"
	if !strings.Contains(allServicesCmd, mountAllLine) {
		t.Fatalf("all-services command no longer names MountAll as expected:\n%s", allServicesCmd)
	}
	if !strings.Contains(allServicesCmd, "RequireComplete: true,") {
		t.Fatalf("all-services command must set RequireComplete: true:\n%s", allServicesCmd)
	}
	incomplete := strings.Replace(allServicesCmd, mountAllLine,
		"Mount:           (*app.Components).MountAPI,", 1)
	if err := os.WriteFile(serverCmdPath, []byte(incomplete), 0o644); err != nil {
		t.Fatalf("write carved server.go: %v", err)
	}

	incompleteBin := filepath.Join(projectDir, "tonly-incomplete")
	runCmd(t, projectDir, "go", "build", "-o", incompleteBin, "./cmd/tonly")
	bootOut, bootErr := runServerExpectingExit(t, incompleteBin, projectDir, dsn)
	if bootErr == nil {
		t.Fatalf("a binary that declares ProjectService but does not mount it MUST refuse to boot "+
			"with RequireComplete: true; it started instead:\n%s", bootOut)
	}
	for _, want := range []string{
		"completeness check",
		"services.project.v1.ProjectService",
	} {
		if !strings.Contains(bootOut, want) {
			t.Errorf("fail-closed boot error must contain %q:\n%s", want, bootOut)
		}
	}

	// ── D. A deliberate subset mount boots, and serves only its service ──
	//
	// Same carved mount, gate turned off — the shape every per-service
	// subcommand ships with. It must boot cleanly and mount EXACTLY the
	// chosen service. The discriminator is the status code: a mounted
	// Connect route rejects the unauthenticated call with 401 (the auth
	// interceptor ran, so the route is registered), while an unmounted one
	// is a plain 404 from the mux.
	subset := strings.Replace(incomplete, "RequireComplete: true,", "RequireComplete: false,", 1)
	if err := os.WriteFile(serverCmdPath, []byte(subset), 0o644); err != nil {
		t.Fatalf("write subset server.go: %v", err)
	}
	subsetBin := filepath.Join(projectDir, "tonly-subset")
	runCmd(t, projectDir, "go", "build", "-o", subsetBin, "./cmd/tonly")

	assertSubsetMountServesOnly(t, subsetBin, projectDir, dsn,
		"/services.api.v1.APIService/Ping",
		"/services.project.v1.ProjectService/GetProject")
}

// assertAuditSeesService asserts that `forge project audit --json` reports the
// named service and one of its RPCs under the shape category. Mounting is a
// composition decision made in Go source; the audit reads the proto surface
// and the data-only Inventory, so a service no binary mounts must still be
// fully visible to introspection.
func assertAuditSeesService(t *testing.T, projectDir, forgeBin, serviceName, rpcName string) {
	t.Helper()
	out := runCmdOutput(t, projectDir, forgeBin, "project", "audit", "--json")
	var report struct {
		Categories map[string]struct {
			Details struct {
				Services []struct {
					Name string `json:"name"`
					RPCs []struct {
						Name string `json:"name"`
					} `json:"rpcs"`
				} `json:"services"`
			} `json:"details"`
		} `json:"categories"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse audit JSON: %v\n%s", err, out)
	}

	for _, svc := range report.Categories["shape"].Details.Services {
		if svc.Name != serviceName {
			continue
		}
		for _, rpc := range svc.RPCs {
			if rpc.Name == rpcName {
				return
			}
		}
		t.Fatalf("audit shape service %q is missing rpc %q: %+v", serviceName, rpcName, svc)
	}
	t.Fatalf("audit shape must keep the unmounted service %q discoverable:\n%s", serviceName, out)
}

// runServerExpectingExit boots serverBin's all-services command and waits for
// it to EXIT, returning its combined output. It is the fail-closed probe: the
// gate rejects before the listener opens, so a refusal is a fast exit rather
// than anything observable over HTTP. A server that stays up past the deadline
// is itself the failure — it is killed and reported as still running.
func runServerExpectingExit(t *testing.T, serverBin, projectDir, dsn string) (string, error) {
	t.Helper()
	port := freePortE2E(t)

	cmd := exec.Command(serverBin, "server")
	cmd.Dir = projectDir
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("PORT=%d", port),
		"DATABASE_URL="+dsn,
		"ENVIRONMENT=development",
	)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return out.String(), err
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		return out.String(), nil // nil error == "it did not refuse", which the caller fails on
	}
}

// assertSubsetMountServesOnly boots serverBin's carved all-services command
// (subset mount, completeness gate off), then asserts it is healthy, that
// mountedPath is served, and that unmountedPath is NOT.
//
// The status code is the discriminator, and the distinction is sharp: a
// registered Connect route runs the auth interceptor and rejects the
// unauthenticated probe with 401, while an unregistered one never reaches a
// handler and the mux answers 404. Asserting 401 (not 2xx) also keeps the
// test honest about auth — nothing here disables authentication.
func assertSubsetMountServesOnly(t *testing.T, serverBin, projectDir, dsn, mountedPath, unmountedPath string) {
	t.Helper()
	port := freePortE2E(t)

	cmd := exec.Command(serverBin, "server")
	cmd.Dir = projectDir
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("PORT=%d", port),
		"DATABASE_URL="+dsn,
		"ENVIRONMENT=development",
	)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start subset server: %v", err)
	}
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	if !waitForServer(t, base+"/healthz", 30*time.Second) {
		t.Fatalf("subset mount must boot with the completeness gate off; it never became ready\noutput:\n%s", out.String())
	}

	if code := postStatus(t, base+mountedPath); code != http.StatusUnauthorized {
		t.Errorf("mounted route %s = %d, want 401 (route registered, auth interceptor rejects)\noutput:\n%s",
			mountedPath, code, out.String())
	}
	if code := postStatus(t, base+unmountedPath); code != http.StatusNotFound {
		t.Errorf("unmounted route %s = %d, want 404 (never registered on the mux)\noutput:\n%s",
			unmountedPath, code, out.String())
	}

	// A carved process must still shut down cleanly.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		killed = true
		if err != nil {
			t.Fatalf("subset server did not shut down cleanly on SIGTERM: %v\noutput:\n%s", err, out.String())
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		killed = true
		t.Fatalf("subset server did not exit within 30s of SIGTERM\noutput:\n%s", out.String())
	}
}

// postStatus POSTs an empty Connect/JSON body and returns the status code.
func postStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
