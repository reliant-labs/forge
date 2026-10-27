package openbao

// The docker smoke for retired auth mounts: the REAL OpenBao 2.6.2 with legacy
// `approle`, `kubernetes` and `userpass` mounts on it, and the embedded
// converge.sh run unchanged against it. It settles what the unit tests cannot:
//
//   - the converge policy's exact `sys/auth/<name>` delete (with sudo, which
//     OpenBao requires to disable an auth mount) and the `auth/<name>/role`
//     list the in-use check reads are enough, as the Job's own JWT login;
//   - a listed mount that still has roles is refused (exit 64) and NOTHING is
//     disabled, including the listed mounts that were clean;
//   - once the roles are gone the mounts are disabled, and a rerun is a no-op;
//   - `force` disables a mount that still has roles, and is the only way past
//     a mount whose type the Job cannot check;
//   - the policy reaches no mount that was not listed.
//
// Gated by -short (it boots containers). Run it alone:
//
//	go test -count=1 -run TestSmoke_RetiredAuthMounts -v ./pkg/deploy/openbao/

import (
	"encoding/base64"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSmoke_RetiredAuthMounts(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: boots OpenBao and the converge image; runs in task test")
	}
	if exec.Command("docker", "version").Run() != nil {
		t.Skip("docker is not available")
	}
	s := newSmoke(t)
	s.start()
	root := s.initWithOrphanRoot()
	s.converge(PhaseConverge, 0)

	for _, m := range []struct{ path, typ string }{{"approle", "approle"}, {"kubernetes", "kubernetes"}, {"userpass", "userpass"}} {
		s.must(root, http.MethodPost, "sys/auth/"+m.path, map[string]any{"type": m.typ})
	}
	s.must(root, http.MethodPost, "auth/approle/role/legacy-service", map[string]any{"token_policies": []string{"default"}})
	s.mustMountExist(root, "approle", "kubernetes", "userpass")

	retire := func(mounts ...RetiredAuthMount) {
		next := s.spec
		next.RetiredAuthMounts = mounts
		s.setSpec(next)
	}

	// ---- 1. approle still has a role: refused, and kubernetes (clean) is NOT
	// disabled either, so a refusal never leaves the store half-retired.
	retire(
		RetiredAuthMount{Name: "kubernetes", Reason: "no login since 2026-10-25 11:41Z"},
		RetiredAuthMount{Name: "approle", Reason: "no login since 2026-10-25 12:27Z"},
	)
	out := s.converge(PhaseConverge, exitRefused)
	mustContain(t, out, "auth mount approle is listed as retired but still has roles (legacy-service)")
	mustContain(t, out, "no login since 2026-10-25 12:27Z")
	s.mustMountExist(root, "approle", "kubernetes", "userpass")
	if strings.Contains(out, "disabling retired auth mount") {
		t.Errorf("nothing may be disabled when any listed mount is refused:\n%s", out)
	}
	// The refusal is before the applied document is recorded, so the run is
	// retried until the roles go: nothing was half-applied.
	if s.stateFile("openbao", "configmap-"+s.spec.Name+"-access-applied", "access.json") != "" &&
		strings.Contains(s.stateFile("openbao", "configmap-"+s.spec.Name+"-access-applied", "access.json"), `"retired"`) {
		t.Error("a refused retire must not be recorded as applied")
	}

	// ---- 2. the policy reaches exactly the listed mounts: as the Job's own
	// login it can list approle's roles and disable nothing it was not given.
	jobTok := s.login("jwt-local", RoleConverge, ConvergeAudience, "system:serviceaccount:openbao:openbao-converge", s.tokenKey)
	if code, _ := s.api(jobTok, "LIST", "auth/approle/role", nil); code != 200 {
		t.Errorf("the converge policy must list the roles of a retired mount, got %d (the in-use check depends on it)", code)
	}
	if code, _ := s.api(jobTok, "LIST", "auth/userpass/users", nil); code != 403 {
		t.Errorf("the converge policy reached a mount nobody retired: %d", code)
	}
	if code, _ := s.api(jobTok, http.MethodDelete, "sys/auth/userpass", nil); code != 403 {
		t.Errorf("the converge policy disabled a mount nobody retired: %d", code)
	}
	s.mustMountExist(root, "userpass")

	// ---- 3. the roles are dead: both mounts go, and the log says why.
	s.must(root, http.MethodDelete, "auth/approle/role/legacy-service", nil)
	out = s.converge(PhaseConverge, 0)
	mustContain(t, out, "disabling retired auth mount kubernetes (kubernetes): no login since 2026-10-25 11:41Z")
	mustContain(t, out, "disabling retired auth mount approle (approle): no login since 2026-10-25 12:27Z")
	s.mustMountAbsent(root, "approle", "kubernetes")
	s.mustMountExist(root, "userpass", "jwt-local")

	// ---- 4. idempotent: a rerun finds them gone and changes nothing.
	out = s.converge(PhaseConverge, 0)
	mustContain(t, out, "retired auth mount kubernetes is already gone")
	mustContain(t, out, "retired auth mount approle is already gone")
	if strings.Contains(out, "disabling") {
		t.Errorf("a rerun disabled something:\n%s", out)
	}

	// ---- 5. force disables a mount that still has roles, and is logged loudly.
	s.must(root, http.MethodPost, "sys/auth/approle", map[string]any{"type": "approle"})
	s.must(root, http.MethodPost, "auth/approle/role/still-here", map[string]any{"token_policies": []string{"default"}})
	retire(RetiredAuthMount{Name: "approle", Reason: "operator confirmed the role is dead", Force: true})
	out = s.converge(PhaseConverge, 0)
	mustContain(t, out, "FORCED: auth mount approle still has roles (still-here)")
	mustContain(t, out, "disabling retired auth mount approle (approle): operator confirmed the role is dead")
	s.mustMountAbsent(root, "approle")

	// ---- 6. a mount whose use the Job cannot check is refused without force.
	retire(RetiredAuthMount{Name: "userpass", Reason: "never used"})
	out = s.converge(PhaseConverge, exitRefused)
	mustContain(t, out, "auth mount userpass is a userpass mount")
	mustContain(t, out, "cannot tell whether it is in use")
	s.mustMountExist(root, "userpass")
	retire(RetiredAuthMount{Name: "userpass", Reason: "never used", Force: true})
	out = s.converge(PhaseConverge, 0)
	mustContain(t, out, "FORCED: auth mount userpass is a userpass mount")
	s.mustMountAbsent(root, "userpass")

	// ---- 7. the mount the component logs in on can never be retired, even
	// from a document that somehow lists it (the script refuses on its own).
	next := s.spec
	s.setSpec(next)
	doc := s.doc()
	doc.Retired = []RetiredAuthMount{{Name: "jwt-local", Reason: "wrong"}}
	s.installDoc(doc)
	out = s.converge(PhaseConverge, exitRefused)
	mustContain(t, out, "auth mount jwt-local is listed under retired, but it is a mount this component manages")
	s.mustMountExist(root, "jwt-local")
}

// initWithOrphanRoot initialises the empty store itself and returns a root
// token that outlives the converge run. OpenBao with a static seal cannot mint
// a new root token (generate-root is unsupported), and a legacy mount needs a
// privileged hand to create. The init root token goes into the recovery Secret
// the Job reads (so its first run converges as root, then revokes it), and an
// ORPHAN root-policy token, which survives that revocation, is returned.
func (s *smoke) initWithOrphanRoot() string {
	s.t.Helper()
	code, out := s.api("", http.MethodPut, "sys/init", map[string]any{"recovery_shares": 1, "recovery_threshold": 1})
	if code != 200 {
		s.t.Fatalf("init = %d: %v", code, out)
	}
	initRoot, _ := out["root_token"].(string)
	// Unsealed by the static seal; wait for the active node.
	for i := 0; i < 100; i++ {
		if c, _ := s.api(initRoot, http.MethodGet, "auth/token/lookup-self", nil); c == 200 {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	code, out = s.api(initRoot, http.MethodPost, "auth/token/create-orphan", map[string]any{"policies": []string{"root"}, "ttl": "2h"})
	if code != 200 {
		s.t.Fatalf("create-orphan = %d: %v", code, out)
	}
	orphan := out["auth"].(map[string]any)["client_token"].(string)

	dir := filepath.Join(s.stateDi, "openbao", "secret-openbao-recovery")
	mustMkdir(s.t, dir)
	_ = os.Chmod(filepath.Join(s.stateDi, "openbao"), 0o777)
	_ = os.Chmod(dir, 0o777)
	mustWrite(s.t, filepath.Join(dir, "root-token"), base64.StdEncoding.EncodeToString([]byte(initRoot)), 0o644)
	mustWrite(s.t, filepath.Join(dir, ".uid"), "uid-smoke-recovery", 0o644)
	return orphan
}

func (s *smoke) authMounts(root string) map[string]bool {
	s.t.Helper()
	code, out := s.api(root, http.MethodGet, "sys/auth", nil)
	if code != 200 {
		s.t.Fatalf("sys/auth = %d: %v", code, out)
	}
	data, _ := out["data"].(map[string]any)
	got := map[string]bool{}
	for k := range data {
		got[strings.TrimSuffix(k, "/")] = true
	}
	return got
}

func (s *smoke) mustMountExist(root string, names ...string) {
	s.t.Helper()
	have := s.authMounts(root)
	for _, n := range names {
		if !have[n] {
			s.t.Errorf("auth mount %s is gone; mounts: %v", n, have)
		}
	}
}

func (s *smoke) mustMountAbsent(root string, names ...string) {
	s.t.Helper()
	have := s.authMounts(root)
	for _, n := range names {
		if have[n] {
			s.t.Errorf("auth mount %s is still enabled; mounts: %v", n, have)
		}
	}
}

// installDoc replaces the access document the next converge Job reads, leaving
// everything else of the rendered Job as it was.
func (s *smoke) installDoc(doc AccessDoc) {
	s.t.Helper()
	b, err := doc.JSON()
	if err != nil {
		s.t.Fatal(err)
	}
	mustWrite(s.t, s.dir+"/converge/access.json", string(b), 0o644)
}
