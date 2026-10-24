package kubedial

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"

	"github.com/jackc/pgx/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/reliant-labs/forge/pkg/dbgrants"
	"github.com/reliant-labs/forge/pkg/deploy"
)

// ErrNoReadOnlyCredentials means the read-only role's Secret is missing or
// holds no password: the database predates the forge_ro role, or CNPG has not
// reconciled it yet.
var ErrNoReadOnlyCredentials = errors.New("no read-only credentials")

// Credentials are what a connection as the read-only role needs, read through
// the same API server the tunnel runs over.
type Credentials struct {
	// Username is dbgrants.RoleName unless the Secret says otherwise.
	Username string
	Password string
	// CAPEM is the cluster CA certificate (ca.crt of <db>-ca).
	CAPEM []byte
}

// Credentials reads the read-only role's basic-auth Secret
// (dbgrants.SecretName) and the cluster CA (CASecretName, key ca.crt) of db.
// Only those two Secrets are read, and only those two keys are kept.
func (c *Client) Credentials(ctx context.Context, db Database) (*Credentials, error) {
	secretName := dbgrants.SecretName(db.Name)
	sec, err := c.cs.CoreV1().Secrets(db.Namespace).Get(ctx, secretName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil, fmt.Errorf("kubedial: secret %s/%s: %w (does the database predate forge's read-only role? redeploy)", db.Namespace, secretName, ErrNoReadOnlyCredentials)
	case err != nil:
		return nil, fmt.Errorf("kubedial: read secret %s/%s: %w%s", db.Namespace, secretName, err, forbiddenHint(err, db.Namespace))
	}
	creds := &Credentials{Username: string(sec.Data["username"]), Password: string(sec.Data["password"])}
	if creds.Username == "" {
		creds.Username = dbgrants.RoleName
	}
	if creds.Password == "" {
		return nil, fmt.Errorf("kubedial: secret %s/%s has no password: %w", db.Namespace, secretName, ErrNoReadOnlyCredentials)
	}

	caName := CASecretName(db.Name)
	ca, err := c.cs.CoreV1().Secrets(db.Namespace).Get(ctx, caName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil, fmt.Errorf("kubedial: secret %s/%s not found: CNPG has not issued the cluster CA yet, or the Cluster uses a CA Secret of its own", db.Namespace, caName)
	case err != nil:
		return nil, fmt.Errorf("kubedial: read secret %s/%s: %w%s", db.Namespace, caName, err, forbiddenHint(err, db.Namespace))
	}
	creds.CAPEM = ca.Data["ca.crt"]
	if len(creds.CAPEM) == 0 {
		return nil, fmt.Errorf("kubedial: secret %s/%s has no ca.crt", db.Namespace, caName)
	}
	return creds, nil
}

// ConnConfig is everything needed to run a query against db: it reads the
// credentials and returns a pgx config that dials through the tunnel and
// verifies the server certificate against the cluster CA
// (sslmode=verify-full semantics, ServerName <service>.<namespace>.svc).
//
// Pass it to pgx.ConnectConfig, or to a pgxpool.Config's ConnConfig: every
// connection takes its own stream and re-resolves the pod. The config holds
// the password; do not log it.
func (c *Client) ConnConfig(ctx context.Context, db Database) (*pgx.ConnConfig, error) {
	creds, err := c.Credentials(ctx, db)
	if err != nil {
		return nil, err
	}
	return c.ConnConfigFor(db, creds)
}

// ConnConfigFor is [Client.ConnConfig] with credentials the caller already
// holds, for a pool that caches them.
func (c *Client) ConnConfigFor(db Database, creds *Credentials) (*pgx.ConnConfig, error) {
	t := db.Target()
	cfg, err := readOnlyConfig(db, creds, t.Host(), PostgresPort)
	if err != nil {
		return nil, err
	}
	pool, err := caPool(db, creds)
	if err != nil {
		return nil, err
	}
	cfg.TLSConfig = &tls.Config{RootCAs: pool, ServerName: t.Host(), MinVersion: tls.VersionTLS12}
	// The host is a cluster-internal name with no DNS here; the dial ignores
	// it, and pgx must not try to resolve it first.
	cfg.LookupFunc = func(_ context.Context, host string) ([]string, error) { return []string{host}, nil }
	cfg.DialFunc = c.DialFunc(t)
	return cfg, nil
}

// SSL modes [ConnConfigForAddr] speaks. The empty string means
// [SSLModeVerifyFull].
const (
	SSLModeDisable    = "disable"
	SSLModeRequire    = "require"
	SSLModeVerifyFull = "verify-full"
)

// ServerNameForAddr is the name the server certificate is verified against
// when the caller reaches db at an address of its own: the CNPG read-write
// Service, <name>-rw.<namespace>.svc, which every CNPG server certificate
// carries. A port-forward to localhost therefore still verifies.
func ServerNameForAddr(db Database) string {
	return Target{Namespace: db.Namespace, Service: db.Name + "-rw"}.Host()
}

// ConnConfigForAddr is [Client.ConnConfigFor] for a caller that brings its own
// network path to the database (a VPN, a kubectl port-forward, a bastion): it
// dials addr (host:port) directly, with no tunnel of its own. sslmode is
// [SSLModeVerifyFull] by default, verified against the cluster CA with
// [ServerNameForAddr] as the server name; [SSLModeRequire] encrypts without
// verifying, [SSLModeDisable] does not encrypt.
//
// It makes no API call: the credentials are the caller's to read
// ([Client.Credentials]). The config holds the password; do not log it.
func ConnConfigForAddr(db Database, creds *Credentials, addr, sslmode string) (*pgx.ConnConfig, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, errors.New("kubedial: address: want host:port")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 || host == "" {
		return nil, errors.New("kubedial: address: want host:port")
	}
	cfg, err := readOnlyConfig(db, creds, host, port)
	if err != nil {
		return nil, err
	}
	switch sslmode {
	case "", SSLModeVerifyFull:
		pool, err := caPool(db, creds)
		if err != nil {
			return nil, err
		}
		cfg.TLSConfig = &tls.Config{RootCAs: pool, ServerName: ServerNameForAddr(db), MinVersion: tls.VersionTLS12}
	case SSLModeRequire:
		//nolint:gosec // sslmode=require is encryption without verification, the caller's explicit choice.
		cfg.TLSConfig = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
	case SSLModeDisable:
		cfg.TLSConfig = nil
	default:
		return nil, fmt.Errorf("kubedial: sslmode %q: use %s, %s or %s", sslmode, SSLModeVerifyFull, SSLModeRequire, SSLModeDisable)
	}
	return cfg, nil
}

// readOnlyConfig is the connection config of the read-only role at host:port,
// with TLS left for the caller to set.
func readOnlyConfig(db Database, creds *Credentials, host string, port int) (*pgx.ConnConfig, error) {
	database, _ := deploy.DatabaseIdentifiers(db.Name)
	dsn := (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(creds.Username, creds.Password),
		Host:     net.JoinHostPort(host, strconv.Itoa(port)),
		Path:     "/" + database,
		RawQuery: "sslmode=disable",
	}).String()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		// The error can quote the DSN, and the DSN carries the password.
		return nil, errors.New("kubedial: build connection config: invalid connection parameters")
	}
	return cfg, nil
}

func caPool(db Database, creds *Credentials) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(creds.CAPEM) {
		return nil, fmt.Errorf("kubedial: the cluster CA of %s/%s is not a PEM certificate", db.Namespace, db.Name)
	}
	return pool, nil
}
