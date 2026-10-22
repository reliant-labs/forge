package secretaccess

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// DefaultWriterServiceAccount is the service account a person's `forge secret
// set` signs in as on a self-run store (OpenBaoSAWriter.service_account).
const DefaultWriterServiceAccount = "forge-secret-writer"

// WriterRequest asks for the one login role and policy through which a person
// writes an env's values to a self-run store.
//
// The writer is the other half of [Request]: readers (ESO, one role per
// namespace) can only read exact names; the writer can only write. `forge
// secret set` mints a ten-minute token for ServiceAccount with the operator's
// own kube context (TokenRequest) and logs in with it, so the credential that
// authorises a write is the kubeconfig the operator already holds and nothing
// is stored anywhere.
type WriterRequest struct {
	Scope Scope
	// Cluster is the cluster the store runs in; its mount carries the role.
	Cluster Cluster
	// Namespace and ServiceAccount identify the writer's service account. The
	// namespace is the store's own. ServiceAccount defaults to
	// [DefaultWriterServiceAccount].
	Namespace      string
	ServiceAccount string
}

// WriterAudience is the token audience of a cluster's writer role. It differs
// from the readers' audience on purpose: a token minted for ESO cannot log in
// as the writer, nor the reverse.
func WriterAudience(clusterID string) string { return "forge-secret-writer/" + clusterID }

// WriterServiceAccount is the account a [WriterRequest] binds to.
func (w WriterRequest) serviceAccount() string {
	if w.ServiceAccount == "" {
		return DefaultWriterServiceAccount
	}
	return w.ServiceAccount
}

// WriterPolicyName is the name of the writer policy, and of the role that
// carries it, for one env on one cluster:
//
//	env-<env>-<cluster>-w-<hash>
//
// The "-w-" keeps it from ever equalling a reader's name, which has no such
// segment.
func (s Scope) WriterPolicyName(clusterID string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{"writer", s.Org, s.Env, clusterID}, "\x00")))
	return "env-" + label(s.Env) + "-" + label(clusterID) + "-w-" + hex.EncodeToString(sum[:])[:8]
}

// DeriveWriter maps a [WriterRequest] to the mount, role and policy that let
// one service account write the env's values.
//
// The policy grants, under the env's directory only:
//
//   - create and update on the data path of any name (a set, a create-only
//     import with check-and-set 0, a rotation), never read: the credential that
//     can write a value cannot read one back;
//   - read and list on the metadata path, which carries version numbers and
//     timestamps and never a value, so `forge secret list` can say what exists.
//
// Nothing outside <mount>/{data,metadata}/<root>/<org>/<env>/ is reachable,
// and there is no delete, destroy or undelete.
func DeriveWriter(req WriterRequest) (Document, error) {
	var errs []error
	if err := req.Scope.Validate(); err != nil {
		errs = append(errs, err)
	}
	c := req.Cluster
	switch {
	case !clusterIDForm.MatchString(c.ID):
		errs = append(errs, fmt.Errorf("cluster id %q is not a plain identifier", c.ID))
	case strings.ContainsAny(c.Audience, " \t\r\n"):
		errs = append(errs, fmt.Errorf("cluster %q audience must not contain whitespace", c.ID))
	}
	if c.Trust.OIDCDiscoveryURL != "" && len(c.Trust.PublicKeys) > 0 {
		errs = append(errs, fmt.Errorf("cluster %q sets both an OIDC discovery URL and public keys; choose one", c.ID))
	}
	if !dnsLabelForm.MatchString(req.Namespace) || len(req.Namespace) > 63 {
		errs = append(errs, fmt.Errorf("writer namespace %q is not a DNS label", req.Namespace))
	}
	if !dnsLabelForm.MatchString(req.serviceAccount()) {
		errs = append(errs, fmt.Errorf("writer service account %q is not a DNS label", req.serviceAccount()))
	}
	if len(errs) > 0 {
		return Document{}, errors.Join(errs...)
	}

	audience := c.Audience
	if audience == "" {
		audience = WriterAudience(c.ID)
	}
	dir := req.Scope.kv() + "/%s/" + req.Scope.root() + "/" + req.Scope.Org + "/" + req.Scope.Env + "/*"
	name := req.Scope.WriterPolicyName(c.ID)
	policy := Policy{Name: name, Rules: []Rule{
		{Path: fmt.Sprintf(dir, "data"), Capabilities: []string{"create", "update"}},
		{Path: fmt.Sprintf(dir, "metadata"), Capabilities: []string{"read", "list"}},
	}}
	policy.sort()
	return Document{
		Mounts:   []Mount{{Cluster: c.ID, Trust: c.Trust}},
		Policies: []Policy{policy},
		Roles: []Role{{
			Mount:          MountName(c.ID),
			Name:           name,
			BoundAudiences: []string{audience},
			BoundSubject:   "system:serviceaccount:" + req.Namespace + ":" + req.serviceAccount(),
			Policies:       []string{name},
		}},
	}, nil
}

// WriterLogin is what a client needs to sign in as the writer: where to log in,
// the role, and the audience its token must carry. It is derived from the same
// request as the Document, so the two cannot disagree.
type WriterLogin struct {
	// LoginPath is the API path of the login call (PUT {"role","jwt"}).
	LoginPath string
	Role      string
	Audience  string
	// Namespace and ServiceAccount are the account whose token is minted.
	Namespace      string
	ServiceAccount string
}

// Login derives the [WriterLogin] of a request.
func (w WriterRequest) Login() (WriterLogin, error) {
	doc, err := DeriveWriter(w)
	if err != nil {
		return WriterLogin{}, err
	}
	r := doc.Roles[0]
	return WriterLogin{
		LoginPath:      "auth/" + r.Mount + "/login",
		Role:           r.Name,
		Audience:       r.BoundAudiences[0],
		Namespace:      w.Namespace,
		ServiceAccount: w.serviceAccount(),
	}, nil
}
