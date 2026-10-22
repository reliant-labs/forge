package cli

// `forge db query`'s port-forward transport: an env whose app database is a
// ManagedDatabase on a cluster forge created (lifecycle local or ephemeral: a
// developer's k3d, a preview env). forge reaches it the way a developer would,
// with their own kube context:
//
//	kube context     the database's declared cluster
//	service          <name>-ro when the Cluster runs standbys, else <name>-rw
//	                 (CloudNativePG's services; a read never loads the primary
//	                 when a replica exists)
//	role             dbgrants.RoleName (forge_ro), the read-only role forge's
//	                 render declares on every ManagedDatabase; its password is
//	                 in dbgrants.SecretName (<name>-forge-ro)
//	database         the ManagedDatabase's database (deploy.DatabaseIdentifiers)
//
// The tunnel lives exactly as long as the query: opened by the resolve,
// closed when the command returns.
//
// The env's data_access policy applies as on every transport. A REAL
// OnCluster env (lifecycle unset) is refused: reaching a production
// database through someone's laptop kube context is the control plane's job
// (its connected-cluster grant), not a tunnel's.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/reliant-labs/forge/pkg/dbgrants"
	"github.com/reliant-labs/forge/pkg/deploy"
)

// dbPostgresPort is the port a CNPG instance serves on.
const dbPostgresPort = 5432

// dbClusterSSLMode is the sslmode of a tunnelled connection. CNPG serves TLS
// and its pg_hba admits remote clients over hostssl only. A variable so a test
// can point the transport at a server without TLS.
var dbClusterSSLMode = "require"

// dbClusterService is the CNPG service a read goes to: a replica when the
// Cluster has one.
func dbClusterService(name string, instances int32) string {
	if instances > 1 {
		return name + "-ro"
	}
	return name + "-rw"
}

// dbTunnel is what the transport needs from a cluster. A seam: tests
// substitute a fake (k3d is not available everywhere this runs).
type dbTunnel interface {
	// Secret reads one Secret's data.
	Secret(ctx context.Context, namespace, name string) (map[string][]byte, error)
	// Forward port-forwards a local port to dbPostgresPort behind service,
	// and returns the local address and the function that closes the tunnel.
	Forward(ctx context.Context, namespace, service string) (addr string, stop func(), err error)
}

// newDBTunnel opens a tunnel factory for a kube context.
var newDBTunnel = func(kubeContext string) (dbTunnel, error) {
	if kubeContext == "" {
		return nil, errors.New("the database declares no cluster to reach it on")
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{CurrentContext: kubeContext}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kube context %q: %w", kubeContext, err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &kubeDBTunnel{kubeContext: kubeContext, cs: cs}, nil
}

// resolveDBQueryCluster resolves the port-forward target for an OnCluster
// workload's env. The returned target's close stops the tunnel.
func resolveDBQueryCluster(ctx context.Context, o dbQueryOptions, e *KCLEntities, w WorkloadEntity) (dbQueryTarget, error) {
	switch e.Lifecycle {
	case lifecycleLocal, lifecycleEphemeral:
	default:
		return dbQueryTarget{}, exitCodeError{code: exitUndetermined, msg: fmt.Sprintf("workload %q in %s runs on a cluster %s does not declare scratch (lifecycle local or ephemeral): its database is not reachable from forge yet", w.Name, o.env, o.env)}
	}
	db, err := selectQueryDatabase(e, o.database)
	if err != nil {
		return dbQueryTarget{}, err
	}
	tun, err := newDBTunnel(db.Cluster)
	if err != nil {
		return dbQueryTarget{}, exitCodeError{code: exitUndetermined, msg: fmt.Sprintf("database %q in %s: %v", db.Name, o.env, err)}
	}
	secretName := dbgrants.SecretName(db.Name)
	secret, err := tun.Secret(ctx, db.Namespace, secretName)
	if err != nil {
		return dbQueryTarget{}, exitCodeError{code: exitUndetermined, msg: fmt.Sprintf("database %q in %s: read-only credentials %s/%s: %v (does the database predate forge's read-only role? redeploy %s)", db.Name, o.env, db.Namespace, secretName, err, o.env)}
	}
	user, pass := string(secret["username"]), string(secret["password"])
	if user == "" {
		user = dbgrants.RoleName
	}
	if pass == "" {
		return dbQueryTarget{}, exitCodeError{code: exitUndetermined, msg: fmt.Sprintf("database %q in %s: %s/%s has no password", db.Name, o.env, db.Namespace, secretName)}
	}
	service := dbClusterService(db.Name, db.Spec.WithDefaults().Instances)
	addr, stop, err := tun.Forward(ctx, db.Namespace, service)
	if err != nil {
		return dbQueryTarget{}, exitCodeError{code: exitUndetermined, msg: fmt.Sprintf("database %q in %s: port-forward to %s/%s: %v", db.Name, o.env, db.Namespace, service, err)}
	}
	database, _ := deploy.DatabaseIdentifiers(db.Name)
	dsn := (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, pass),
		Host:     addr,
		Path:     "/" + database,
		RawQuery: "sslmode=" + dbClusterSSLMode,
	}).String()
	return dbQueryTarget{
		dsn:   dsn,
		close: stop,
		scope: dbQueryScope{Env: o.env, Transport: "port-forward", Workload: w.Name, Database: db.Name, Policy: dbQueryPolicy(e)},
	}, nil
}

// selectQueryDatabase picks the cluster ManagedDatabase to query: the one
// named, or the env's only one.
func selectQueryDatabase(e *KCLEntities, name string) (DatabaseEntity, error) {
	var all []DatabaseEntity
	for _, d := range e.Databases {
		if d.Runtime == RuntimeCluster {
			all = append(all, d)
		}
	}
	if name != "" {
		for _, d := range all {
			if d.Name == name {
				return d, nil
			}
		}
		return DatabaseEntity{}, exitCodeError{code: exitWrong, msg: fmt.Sprintf("no cluster database named %q (the env declares %s)", name, databaseNames(all))}
	}
	switch len(all) {
	case 1:
		return all[0], nil
	case 0:
		return DatabaseEntity{}, exitCodeError{code: exitUndetermined, msg: "the env declares no ManagedDatabase on a cluster to query"}
	default:
		return DatabaseEntity{}, exitCodeError{code: exitWrong, msg: fmt.Sprintf("the env declares %d cluster databases (%s): pick one with --database", len(all), databaseNames(all))}
	}
}

func databaseNames(ds []DatabaseEntity) string {
	if len(ds) == 0 {
		return "none"
	}
	out := ""
	for i, d := range ds {
		if i > 0 {
			out += ", "
		}
		out += d.Name
	}
	return out
}

// kubeDBTunnel is dbTunnel over client-go (the Secret) and kubectl (the
// port-forward).
type kubeDBTunnel struct {
	kubeContext string
	cs          kubernetes.Interface
}

func (k *kubeDBTunnel) Secret(ctx context.Context, namespace, name string) (map[string][]byte, error) {
	s, err := k.cs.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return s.Data, nil
}

// dbForwardReady bounds how long the tunnel may take to come up.
const dbForwardReady = 15 * time.Second

// forwardingRE is the line kubectl port-forward prints once the local port
// listens: "Forwarding from 127.0.0.1:54321 -> 5432".
var forwardingRE = regexp.MustCompile(`Forwarding from (127\.0\.0\.1:[0-9]+) ->`)

// Forward runs `kubectl port-forward svc/<service> :5432`, as forge's other
// cluster operations run kubectl: kubectl picks a ready pod behind the
// service, and the user's kubeconfig (exec plugins included) authenticates.
// The tunnel is the kubectl process; stop ends it.
func (k *kubeDBTunnel) Forward(ctx context.Context, namespace, service string) (string, func(), error) {
	fwdCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	cmd := exec.CommandContext(fwdCtx, "kubectl", "--context", k.kubeContext, "--namespace", namespace,
		"port-forward", "--address", "127.0.0.1", "svc/"+service, ":"+strconv.Itoa(dbPostgresPort))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", nil, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return "", nil, fmt.Errorf("start kubectl: %w", err)
	}
	stop := func() {
		cancel()
		_ = cmd.Wait()
	}
	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if m := forwardingRE.FindStringSubmatch(sc.Text()); m != nil {
				found <- m[1]
				break
			}
		}
		// Keep draining so kubectl never blocks on a full pipe.
		_, _ = io.Copy(io.Discard, stdout)
		close(found)
	}()
	select {
	case addr, ok := <-found:
		if ok {
			return addr, stop, nil
		}
		stop()
		return "", nil, fmt.Errorf("kubectl port-forward exited: %s", strings.TrimSpace(stderr.String()))
	case <-time.After(dbForwardReady):
		stop()
		return "", nil, fmt.Errorf("the tunnel did not come up in %s: %s", dbForwardReady, strings.TrimSpace(stderr.String()))
	case <-ctx.Done():
		stop()
		return "", nil, ctx.Err()
	}
}
