package secretaccess

// The docker smoke: the REAL, unmodified OpenBao image.
//
// The unit tests prove the derivation; this proves the thing it derives for.
// It applies a Document to a live OpenBao and logs in with signed tokens, which
// settles the claims the design rests on:
//
//   - a JWT role accepts token_type=batch (P5) and the login returns a batch
//     token carrying the derived policy (plus OpenBao's default) and the 10 minute TTL;
//   - the policy reads the declared names and nothing else: a sibling name, the
//     directory listing and the metadata path are all refused;
//   - the role pins the audience and the subject;
//   - the mount verifies against jwt_validation_pubkeys, the path a cluster whose
//     issuer OpenBao cannot reach takes.
//
// Gated by -short (it pulls and boots a container). Run it alone:
//
//	go test -count=1 -run TestSmoke -v ./pkg/secretaccess/

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// openbaoImage is the version control-plane runs in prod.
const openbaoImage = "quay.io/openbao/openbao:2.6.2"

const rootToken = "smoke-root"

type bao struct {
	t    *testing.T
	base string
}

func startOpenBao(t *testing.T) *bao {
	t.Helper()
	if testing.Short() {
		t.Skip("-short: boots an OpenBao container; runs in task test")
	}
	if exec.Command("docker", "version").Run() != nil {
		t.Skip("docker is not available")
	}
	if exec.Command("docker", "image", "inspect", openbaoImage).Run() != nil {
		if out, err := exec.Command("docker", "pull", openbaoImage).CombinedOutput(); err != nil {
			t.Skipf("cannot pull %s: %v\n%s", openbaoImage, err, out)
		}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	out, err := exec.Command("docker", "run", "-d", "--rm",
		"-p", fmt.Sprintf("127.0.0.1:%d:8200", port),
		"-e", "BAO_DEV_ROOT_TOKEN_ID="+rootToken,
		"-e", "BAO_DEV_LISTEN_ADDRESS=0.0.0.0:8200",
		openbaoImage, "server", "-dev").CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-fv", id).Run() })

	b := &bao{t: t, base: fmt.Sprintf("http://127.0.0.1:%d/v1/", port)}
	deadline := time.Now().Add(60 * time.Second)
	for {
		if code, _ := b.do(rootToken, http.MethodGet, "sys/health", nil); code == 200 {
			return b
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", id).CombinedOutput()
			t.Fatalf("openbao not ready:\n%s", logs)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// do sends one API request and returns the status and decoded JSON body.
func (b *bao) do(token, method, path string, payload any) (int, map[string]any) {
	b.t.Helper()
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			b.t.Fatal(err)
		}
		body = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, b.base+path, body)
	if err != nil {
		b.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (b *bao) mustRoot(method, path string, payload any) {
	b.t.Helper()
	if code, out := b.do(rootToken, method, path, payload); code/100 != 2 {
		b.t.Fatalf("%s %s = %d: %v", method, path, code, out)
	}
}

func (b *bao) apply(doc Document) {
	b.t.Helper()
	_, existing := b.do(rootToken, http.MethodGet, "sys/auth", nil)
	for _, m := range doc.Mounts {
		if _, ok := existing[m.Name()+"/"]; !ok {
			b.mustRoot(http.MethodPut, m.EnablePath(), m.EnablePayload())
		}
		if cfg := m.ConfigPayload(); cfg != nil {
			b.mustRoot(http.MethodPut, m.ConfigPath(), cfg)
		}
	}
	for _, p := range doc.Policies {
		b.mustRoot(http.MethodPut, p.Path(), p.Payload())
	}
	for _, r := range doc.Roles {
		b.mustRoot(http.MethodPut, r.Path(), r.Payload())
	}
}

type issuer struct {
	key *rsa.PrivateKey
	pem string
}

func newIssuer(t *testing.T) issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return issuer{key, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}
}

func (i issuer) token(t *testing.T, aud, sub string) string {
	t.Helper()
	now := time.Now()
	s, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": "https://kubernetes.default.svc", "aud": []string{aud}, "sub": sub,
		"iat": now.Unix(), "nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(10 * time.Minute).Unix(),
	}).SignedString(i.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSmoke_JWTRoleBatchTokenAndExactPaths(t *testing.T) {
	b := startOpenBao(t)
	iss := newIssuer(t)

	req := Request{
		Scope: Scope{Org: "acme", Env: "prod"},
		Clusters: []Cluster{{
			ID: "hub", Audience: "https://secrets.example.com/c/hub",
			Trust: Trust{PublicKeys: []string{iss.pem}, BoundIssuer: "https://kubernetes.default.svc"},
		}},
		Bindings: []Binding{
			{Cluster: "hub", Namespace: "payments", Names: []string{"STRIPE_SECRET_KEY", "DB_URL"}},
			{Cluster: "hub", Namespace: "search", Names: []string{"SEARCH_KEY"}},
		},
	}
	doc, err := Derive(req)
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range []string{"STRIPE_SECRET_KEY", "DB_URL", "SEARCH_KEY", "UNRELATED"} {
		b.mustRoot(http.MethodPut, "secret/data/tenants/acme/prod/"+n, map[string]any{"data": map[string]any{"value": "v-" + n}})
	}
	b.apply(doc)
	// Applying twice must converge, not fail: the converge Job reruns.
	b.apply(doc)

	const aud = "https://secrets.example.com/c/hub"
	sub := "system:serviceaccount:payments:reliant-secrets"

	login := func(jwtStr, role string) (int, map[string]any) {
		return b.do("", http.MethodPut, "auth/jwt-hub/login", map[string]any{"role": role, "jwt": jwtStr})
	}
	role := req.Scope.PolicyName("hub", "payments")

	// P5: the login succeeds and the token is a batch token.
	code, out := login(iss.token(t, aud, sub), role)
	if code != 200 {
		t.Fatalf("login = %d: %v", code, out)
	}
	auth := out["auth"].(map[string]any)
	token := auth["client_token"].(string)
	if !strings.HasPrefix(token, "b.") {
		t.Errorf("token %.4s... is not a batch token (OpenBao batch tokens start with b.)", token)
	}
	if got := auth["lease_duration"].(float64); got != 600 {
		t.Errorf("lease_duration = %v, want 600", got)
	}
	if auth["renewable"] != false {
		t.Errorf("renewable = %v, want false", auth["renewable"])
	}
	// OpenBao attaches its built-in default policy (token self-lookup and
	// renewal, which ESO's Vault provider calls); it grants no secret path, and
	// the denied reads below prove it. The derived policy is the only other one.
	if pol := fmt.Sprint(auth["token_policies"]); pol != "[default "+role+"]" {
		t.Errorf("token_policies = %v, want [default %s]", pol, role)
	}
	if code, out := b.do(token, http.MethodGet, "auth/token/lookup-self", nil); code != 200 || out["data"].(map[string]any)["type"] != "batch" {
		t.Errorf("lookup-self = %d %v, want type batch", code, out["data"])
	}

	read := func(path string) int {
		code, _ := b.do(token, http.MethodGet, path, nil)
		return code
	}
	for _, n := range []string{"STRIPE_SECRET_KEY", "DB_URL"} {
		if code, out := b.do(token, http.MethodGet, "secret/data/tenants/acme/prod/"+n, nil); code != 200 ||
			out["data"].(map[string]any)["data"].(map[string]any)["value"] != "v-"+n {
			t.Errorf("read %s = %d %v", n, code, out)
		}
	}
	for _, denied := range []string{
		"secret/data/tenants/acme/prod/SEARCH_KEY",                  // another namespace's name
		"secret/data/tenants/acme/prod/UNRELATED",                   // a name no namespace declared
		"secret/data/tenants/acme/other/DB_URL",                     // another env
		"secret/metadata/tenants/acme/prod/DB_URL",                  // metadata of a granted name
		"secret/metadata/tenants/acme/prod?list=true",               // the listing
		"secret/data/tenants/acme/prod/STRIPE_SECRET_KEY_OLD",       // a prefix match must not leak
		"secret/data/tenants/acme/prod/STRIPE_SECRET_KEYS",          // nor a suffix
		"secret/data/tenants/acme/prod/",                            // the directory
		"sys/policies/acl/" + req.Scope.PolicyName("hub", "search"), // other namespaces' policies
	} {
		if code := read(denied); code != 403 {
			t.Errorf("GET %s = %d, want 403", denied, code)
		}
	}
	if code, _ := b.do(token, http.MethodPut, "secret/data/tenants/acme/prod/DB_URL", map[string]any{"data": map[string]any{"value": "x"}}); code != 403 {
		t.Errorf("write with the read token = %d, want 403", code)
	}

	// The role pins subject and audience.
	if code, _ := login(iss.token(t, aud, "system:serviceaccount:search:reliant-secrets"), role); code == 200 {
		t.Error("another namespace's service account logged in to this role")
	}
	if code, _ := login(iss.token(t, aud, "system:serviceaccount:payments:default"), role); code == 200 {
		t.Error("a different service account of the same namespace logged in")
	}
	if code, _ := login(iss.token(t, "https://secrets.example.com/c/other", sub), role); code == 200 {
		t.Error("a token for another audience logged in")
	}
	// A token from an issuer the mount does not trust.
	if code, _ := login(newIssuer(t).token(t, aud, sub), role); code == 200 {
		t.Error("a token signed by an unknown key logged in")
	}

	// Narrowing: re-applying a smaller shape takes the dropped name away from a
	// token issued afterwards.
	req.Bindings[0].Names = []string{"DB_URL"}
	narrow, err := Derive(req)
	if err != nil {
		t.Fatal(err)
	}
	b.apply(narrow)
	_, out = login(iss.token(t, aud, sub), role)
	fresh := out["auth"].(map[string]any)["client_token"].(string)
	if code, _ := b.do(fresh, http.MethodGet, "secret/data/tenants/acme/prod/STRIPE_SECRET_KEY", nil); code != 403 {
		t.Errorf("dropped name still readable after narrowing: %d", code)
	}
	if code, _ := b.do(fresh, http.MethodGet, "secret/data/tenants/acme/prod/DB_URL", nil); code != 200 {
		t.Errorf("kept name unreadable after narrowing: %d", code)
	}
}

// The writer role (DeriveWriter): the credential `forge secret set` signs in
// with on a self-run store. It must write and list, never read a value, stay
// inside its env's directory, and refuse a reader's token.
func TestSmoke_WriterRoleIsWriteOnly(t *testing.T) {
	b := startOpenBao(t)
	iss := newIssuer(t)

	scope := Scope{Org: "acme", Env: "prod"}
	cluster := Cluster{
		ID: "hub", Audience: WriterAudience("hub"),
		Trust: Trust{PublicKeys: []string{iss.pem}, BoundIssuer: "https://kubernetes.default.svc"},
	}
	wr := WriterRequest{Scope: scope, Cluster: cluster, Namespace: "openbao"}
	doc, err := DeriveWriter(wr)
	if err != nil {
		t.Fatal(err)
	}
	b.apply(doc)
	b.apply(doc)

	login, err := wr.Login()
	if err != nil {
		t.Fatal(err)
	}
	sub := "system:serviceaccount:openbao:forge-secret-writer"
	code, out := b.do("", http.MethodPut, login.LoginPath, map[string]any{"role": login.Role, "jwt": iss.token(t, login.Audience, sub)})
	if code != 200 {
		t.Fatalf("writer login = %d: %v", code, out)
	}
	token := out["auth"].(map[string]any)["client_token"].(string)

	put := func(name, value string, cas *int) (int, map[string]any) {
		payload := map[string]any{"data": map[string]any{"value": value}}
		if cas != nil {
			payload["options"] = map[string]any{"cas": *cas}
		}
		return b.do(token, http.MethodPut, "secret/data/tenants/acme/prod/"+name, payload)
	}
	zero := 0
	if code, out := put("API_KEY", "v1", &zero); code != 200 || out["data"].(map[string]any)["version"] != float64(1) {
		t.Fatalf("create-only write = %d %v", code, out)
	}
	// A second create-only write must be refused, and the refusal must be the
	// check-and-set error `forge secret import` recognises.
	code, out = put("API_KEY", "v2", &zero)
	if code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(out["errors"]), "check-and-set") {
		t.Fatalf("second create-only write = %d %v, want 400 with a check-and-set error", code, out)
	}
	if code, out := put("API_KEY", "v2", nil); code != 200 || out["data"].(map[string]any)["version"] != float64(2) {
		t.Errorf("rotation = %d %v, want version 2", code, out)
	}

	// Metadata is readable (versions, never values).
	if code, out := b.do(token, http.MethodGet, "secret/metadata/tenants/acme/prod/API_KEY", nil); code != 200 ||
		out["data"].(map[string]any)["current_version"] != float64(2) {
		t.Errorf("metadata = %d %v", code, out)
	}

	for _, denied := range []struct{ method, path string }{
		{http.MethodGet, "secret/data/tenants/acme/prod/API_KEY"},           // a value
		{http.MethodGet, "secret/data/tenants/acme/prod/API_KEY?version=1"}, // an old value
		{http.MethodDelete, "secret/data/tenants/acme/prod/API_KEY"},        // delete
		{http.MethodPut, "secret/delete/tenants/acme/prod/API_KEY"},         // soft delete
		{http.MethodPut, "secret/destroy/tenants/acme/prod/API_KEY"},        // destroy
		{http.MethodDelete, "secret/metadata/tenants/acme/prod/API_KEY"},    // erase history
		{http.MethodGet, "secret/data/tenants/acme/other/API_KEY"},          // another env
		{http.MethodGet, "sys/policies/acl/" + doc.Policies[0].Name},        // its own policy
	} {
		code, _ := b.do(token, denied.method, denied.path, map[string]any{"versions": []int{1}})
		if code != 403 {
			t.Errorf("%s %s = %d, want 403", denied.method, denied.path, code)
		}
	}
	if code, _ := b.do(token, http.MethodPut, "secret/data/tenants/acme/other/API_KEY",
		map[string]any{"data": map[string]any{"value": "x"}}); code != 403 {
		t.Errorf("write outside the env's directory = %d, want 403", code)
	}

	// The audience pins the writer: a reader's token cannot sign in as it, and
	// the writer's token cannot sign in as a reader.
	if code, _ := b.do("", http.MethodPut, login.LoginPath, map[string]any{
		"role": login.Role, "jwt": iss.token(t, "https://secrets.example.com/c/hub", sub),
	}); code == 200 {
		t.Error("a token for the readers' audience signed in as the writer")
	}
	if code, _ := b.do("", http.MethodPut, login.LoginPath, map[string]any{
		"role": login.Role, "jwt": iss.token(t, login.Audience, "system:serviceaccount:openbao:default"),
	}); code == 200 {
		t.Error("another service account signed in as the writer")
	}
}
