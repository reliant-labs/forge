package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/reliant-labs/forge/pkg/dbgrants"
	"github.com/reliant-labs/forge/pkg/pgtest"
)

func TestDBClusterServicePrefersAReplica(t *testing.T) {
	if got := dbClusterService("app", 1); got != "app-rw" {
		t.Errorf("one instance: %s", got)
	}
	if got := dbClusterService("app", 3); got != "app-ro" {
		t.Errorf("with standbys: %s", got)
	}
}

func TestForwardingLine(t *testing.T) {
	for line, want := range map[string]string{
		"Forwarding from 127.0.0.1:54321 -> 5432": "127.0.0.1:54321",
		"Forwarding from [::1]:54321 -> 5432":     "",
		"Handling connection for 54321":           "",
	} {
		got := ""
		if m := forwardingRE.FindStringSubmatch(line); m != nil {
			got = m[1]
		}
		if got != want {
			t.Errorf("%q: %q, want %q", line, got, want)
		}
	}
}

func TestSelectQueryDatabase(t *testing.T) {
	e := &KCLEntities{Databases: []DatabaseEntity{
		{Name: "a", Runtime: RuntimeCluster}, {Name: "b", Runtime: RuntimeCluster}, {Name: "h", Runtime: RuntimeHosted},
	}}
	if _, err := selectQueryDatabase(e, ""); exitCodeForError(err) != exitWrong || !strings.Contains(err.Error(), "--database") {
		t.Errorf("two databases, none named: %v", err)
	}
	if d, err := selectQueryDatabase(e, "b"); err != nil || d.Name != "b" {
		t.Errorf("named: %v %+v", err, d)
	}
	if _, err := selectQueryDatabase(e, "h"); exitCodeForError(err) != exitWrong {
		t.Errorf("a hosted database is not a cluster database: %v", err)
	}
	if _, err := selectQueryDatabase(&KCLEntities{}, ""); exitCodeForError(err) != exitUndetermined {
		t.Errorf("none: %v", err)
	}
}

// fakeDBTunnel stands in for the cluster: it serves the read-only Secret and
// "forwards" to a real Postgres, recording what the transport asked for.
type fakeDBTunnel struct {
	t                      *testing.T
	db, namespace, service string
	secret                 map[string][]byte
	addr                   string
	stopped                bool
}

func (f *fakeDBTunnel) Secret(_ context.Context, namespace, name string) (map[string][]byte, error) {
	if namespace != f.namespace || name != dbgrants.SecretName(f.db) {
		f.t.Errorf("read secret %s/%s, want %s/%s", namespace, name, f.namespace, dbgrants.SecretName(f.db))
	}
	return f.secret, nil
}

func (f *fakeDBTunnel) Forward(_ context.Context, namespace, service string) (string, func(), error) {
	f.service = service
	if namespace != f.namespace {
		f.t.Errorf("forward in namespace %s, want %s", namespace, f.namespace)
	}
	return f.addr, func() { f.stopped = true }, nil
}

// End to end through the port-forward transport against a real Postgres: the
// read-only Secret is read, the replica service is chosen, the query runs as
// that role in the ManagedDatabase's database, and the tunnel is closed.
func TestDBQueryClusterThroughTheTunnel(t *testing.T) {
	if testing.Short() {
		t.Skip("boots a real postgres (pgtest); runs in task test")
	}
	admin, cleanup, err := pgtest.NewURL()
	if err != nil {
		t.Fatalf("pgtest.NewURL: %v", err)
	}
	t.Cleanup(cleanup)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })

	// A unique role per run: the server may be shared.
	role := fmt.Sprintf("fbl_ro_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS entries (id int PRIMARY KEY, amount int, secret text);
		INSERT INTO entries VALUES (1, 10, 's') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD 'pw'; GRANT SELECT (id, amount) ON entries TO %s`, role, role)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(ctx, fmt.Sprintf(`REVOKE ALL ON entries FROM %s; DROP ROLE IF EXISTS %s`, role, role))
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	// The ManagedDatabase is named after the pgtest database, so the database
	// the transport derives from its name (deploy.DatabaseIdentifiers) is that
	// one: a pgtest name has no hyphen for it to fold.
	dbName := strings.TrimPrefix(u.Path, "/")

	fake := &fakeDBTunnel{t: t, db: dbName, namespace: "demo", addr: u.Host,
		secret: map[string][]byte{"username": []byte(role), "password": []byte("pw")}}
	prevTunnel, prevSSL := newDBTunnel, dbClusterSSLMode
	newDBTunnel = func(kubeContext string) (dbTunnel, error) {
		if kubeContext != "k3d-demo" {
			t.Errorf("kube context %q, want the database's cluster", kubeContext)
		}
		return fake, nil
	}
	dbClusterSSLMode = "disable"
	t.Cleanup(func() { newDBTunnel, dbClusterSSLMode = prevTunnel, prevSSL })

	withKCLFixture(t, fmt.Sprintf(`{"output":{"lifecycle":"local","data_access":{"db_query":"open"},
		"workloads":[{"name":"api","kind":"service","runtime":{"type":"cluster"},"spec":{"kind":"service"}}],
		"databases":[{"name":%q,"runtime":"cluster","cluster":"k3d-demo","namespace":"demo","spec":{"instances":2}}]}}`, dbName))
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	out, err := runDBQueryCmd(t, "dev", "select id, amount from entries order by id", "--json")
	if err != nil {
		t.Fatalf("query: %v\n%s", err, out)
	}
	var doc struct {
		OK    bool         `json:"ok"`
		Scope dbQueryScope `json:"scope"`
		Rows  [][]any      `json:"rows"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !doc.OK || len(doc.Rows) != 1 || doc.Scope.Transport != "port-forward" || doc.Scope.Database != dbName {
		t.Errorf("doc %+v", doc)
	}
	if fake.service != dbName+"-ro" {
		t.Errorf("forwarded to %s, want the replica service", fake.service)
	}
	if !fake.stopped {
		t.Error("the tunnel was left open")
	}

	// The role's grants are the boundary: a column it was not granted is
	// refused by Postgres, exit 1.
	if _, err := runDBQueryCmd(t, "dev", "select secret from entries"); exitCodeForError(err) != exitWrong {
		t.Errorf("an ungranted column: err = %v, want exit 1", err)
	}
}

// fakeKubectl puts a kubectl on PATH that runs script.
func fakeKubectl(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The kubectl tunnel: the address kubectl reports, its arguments, and a stop
// that ends the process.
func TestKubeDBTunnelForward(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a shell-script kubectl")
	}
	args := filepath.Join(t.TempDir(), "args")
	fakeKubectl(t, `echo "$@" > `+args+`
echo "Forwarding from 127.0.0.1:54321 -> 5432"
echo "Forwarding from [::1]:54321 -> 5432"
exec sleep 30
`)
	k := &kubeDBTunnel{kubeContext: "k3d-demo"}
	start := time.Now()
	addr, stop, err := k.Forward(context.Background(), "demo", "ledger-ro")
	if err != nil {
		t.Fatal(err)
	}
	if addr != "127.0.0.1:54321" {
		t.Errorf("addr %q", addr)
	}
	stop()
	if time.Since(start) > 10*time.Second {
		t.Error("stop did not end kubectl")
	}
	got, _ := os.ReadFile(args)
	if want := "--context k3d-demo --namespace demo port-forward --address 127.0.0.1 svc/ledger-ro :5432"; strings.TrimSpace(string(got)) != want {
		t.Errorf("kubectl %s, want %s", got, want)
	}

	fakeKubectl(t, `echo 'error: services "ledger-ro" not found' >&2; exit 1`)
	if _, _, err := k.Forward(context.Background(), "demo", "ledger-ro"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("a failing kubectl: %v", err)
	}
}
