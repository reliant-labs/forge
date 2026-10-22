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
//	role             forge_ro, the read-only role forge's render declares on
//	                 every ManagedDatabase, its password in <name>-forge-ro
//	database         the ManagedDatabase's database (deploy.DatabaseIdentifiers)
//
// The tunnel lives exactly as long as the query: opened by the resolve,
// closed when the command returns.
//
// A REAL OnCluster env (lifecycle unset) is refused: reaching a production
// database through someone's laptop kube context is the control plane's job
// (its connected-cluster grant), not a tunnel's.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	"github.com/reliant-labs/forge/pkg/deploy"
)

// Names the port-forward transport reads. The role and its Secret are what
// forge's ManagedDatabase render declares (CNPG spec.managed.roles); the
// services are CloudNativePG's.
const (
	dbReadOnlyRole       = "forge_ro"
	dbReadOnlySecretSufx = "-forge-ro"
	dbPostgresPort       = 5432
)

// dbReadOnlySecretName is the Secret holding forge_ro's password for the
// ManagedDatabase name.
func dbReadOnlySecretName(name string) string { return name + dbReadOnlySecretSufx }

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
	// Forward port-forwards a local port to dbPostgresPort on a ready pod
	// behind service, and returns the local address and the function that
	// closes the tunnel.
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
	return &kubeDBTunnel{cfg: cfg, cs: cs}, nil
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
	secretName := dbReadOnlySecretName(db.Name)
	secret, err := tun.Secret(ctx, db.Namespace, secretName)
	if err != nil {
		return dbQueryTarget{}, exitCodeError{code: exitUndetermined, msg: fmt.Sprintf("database %q in %s: read-only credentials %s/%s: %v (does the database predate forge's read-only role? redeploy %s)", db.Name, o.env, db.Namespace, secretName, err, o.env)}
	}
	user, pass := string(secret["username"]), string(secret["password"])
	if user == "" {
		user = dbReadOnlyRole
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
		RawQuery: "sslmode=require",
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

// kubeDBTunnel is dbTunnel over client-go.
type kubeDBTunnel struct {
	cfg *rest.Config
	cs  kubernetes.Interface
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

func (k *kubeDBTunnel) Forward(ctx context.Context, namespace, service string) (string, func(), error) {
	svc, err := k.cs.CoreV1().Services(namespace).Get(ctx, service, metav1.GetOptions{})
	if err != nil {
		return "", nil, err
	}
	if len(svc.Spec.Selector) == 0 {
		return "", nil, fmt.Errorf("service %s selects no pods", service)
	}
	pods, err := k.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(svc.Spec.Selector).String()})
	if err != nil {
		return "", nil, err
	}
	pod := readyPod(pods.Items)
	if pod == "" {
		return "", nil, fmt.Errorf("no ready pod behind service %s", service)
	}
	transport, upgrader, err := spdy.RoundTripperFor(k.cfg)
	if err != nil {
		return "", nil, err
	}
	target := k.cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(pod).SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, target)
	stopCh, readyCh := make(chan struct{}), make(chan struct{})
	pf, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{"0:" + strconv.Itoa(dbPostgresPort)}, stopCh, readyCh, io.Discard, io.Discard)
	if err != nil {
		return "", nil, err
	}
	errCh := make(chan error, 1)
	go func() { errCh <- pf.ForwardPorts() }()
	stop := func() { close(stopCh) }
	select {
	case <-readyCh:
	case err := <-errCh:
		return "", nil, fmt.Errorf("pod %s: %w", pod, err)
	case <-time.After(dbForwardReady):
		stop()
		return "", nil, fmt.Errorf("pod %s: the tunnel did not come up in %s", pod, dbForwardReady)
	case <-ctx.Done():
		stop()
		return "", nil, ctx.Err()
	}
	ports, err := pf.GetPorts()
	if err != nil || len(ports) != 1 {
		stop()
		return "", nil, fmt.Errorf("pod %s: no forwarded port: %v", pod, err)
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(int(ports[0].Local))), stop, nil
}

// readyPod is the first running pod whose Ready condition is true.
func readyPod(pods []corev1.Pod) string {
	for _, p := range pods {
		if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				return p.Name
			}
		}
	}
	return ""
}
