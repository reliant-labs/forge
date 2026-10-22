package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// The operator's kube context is the one credential the store-source commands
// lean on: `secret import` and `env secrets verify|adopt` read and patch
// Secrets with it, and `secret set` against a self-run OpenBao mints its
// ten-minute writer token with it. This file is the only place they touch the
// cluster, behind a seam so no unit test dials one.

// errKubeSecretNotFound is the one failure callers branch on.
var errKubeSecretNotFound = errors.New("secret not found")

// lastAppliedAnnotation is the copy of a manifest `kubectl apply` keeps on the
// object. For a Secret it holds every value in clear, which is why adoption
// strips it.
const lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// kubeSecret is what the commands read of a Secret: the data and the two
// facts adoption asserts. Held in memory only; no command prints Data.
type kubeSecret struct {
	Data        map[string][]byte
	Annotations map[string]string
	OwnerRefs   int
}

// kubeAccess is the narrow slice of the Kubernetes API the commands use.
type kubeAccess interface {
	GetSecret(ctx context.Context, namespace, name string) (kubeSecret, error)
	// RemoveAnnotations removes annotations with one JSON merge patch.
	// Removing an absent annotation is not an error. The patch never touches
	// data.
	RemoveAnnotations(ctx context.Context, namespace, name string, annotations []string) error
	// MintToken requests a short-lived token for a service account, bound to
	// one audience (the TokenRequest API).
	MintToken(ctx context.Context, namespace, serviceAccount, audience string, ttl time.Duration) (string, error)
	// ServiceProxy sends one request to a Service through the API server's
	// service proxy. The API server strips the operator's own credential
	// before forwarding, so only the headers passed here reach the backend.
	ServiceProxy(ctx context.Context, req serviceProxyRequest) (status int, body []byte, err error)
}

type serviceProxyRequest struct {
	Namespace string
	Scheme    string // "http" | "https"
	Service   string
	Port      int
	Method    string
	Path      string // below the service root, no leading slash
	Query     url.Values
	Headers   map[string]string
	Body      []byte
}

// newKubeAccess builds the access for a kube context. It is a seam: tests
// substitute a fake, and TestMain's tripwire never needs to know.
//
// An empty context is refused rather than read as "the current context": a
// secret aimed at whichever cluster kubectl last pointed at is the mistake the
// required --env flag exists to prevent.
var newKubeAccess = func(kubeContext string) (kubeAccess, error) {
	if kubeContext == "" {
		return nil, errors.New("no kube context: pass --kube-context, or declare a cluster_target in the env's KCL")
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{CurrentContext: kubeContext}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kube context %q: %w", kubeContext, err)
	}
	return newKubeClients(cfg)
}

type kubeClients struct{ cs kubernetes.Interface }

func newKubeClients(cfg *rest.Config) (*kubeClients, error) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &kubeClients{cs: cs}, nil
}

func (k *kubeClients) GetSecret(ctx context.Context, namespace, name string) (kubeSecret, error) {
	s, err := k.cs.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return kubeSecret{}, fmt.Errorf("%s/%s: %w", namespace, name, errKubeSecretNotFound)
	}
	if err != nil {
		return kubeSecret{}, fmt.Errorf("read secret %s/%s: %w", namespace, name, err)
	}
	return kubeSecret{Data: s.Data, Annotations: s.Annotations, OwnerRefs: len(s.OwnerReferences)}, nil
}

func (k *kubeClients) RemoveAnnotations(ctx context.Context, namespace, name string, annotations []string) error {
	ann := make(map[string]any, len(annotations))
	for _, a := range annotations {
		ann[a] = nil
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": ann}})
	if err != nil {
		return err
	}
	if _, err := k.cs.CoreV1().Secrets(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patch secret %s/%s: %w", namespace, name, err)
	}
	return nil
}

func (k *kubeClients) MintToken(ctx context.Context, namespace, serviceAccount, audience string, ttl time.Duration) (string, error) {
	secs := int64(ttl.Seconds())
	tr, err := k.cs.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, serviceAccount, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{Audiences: []string{audience}, ExpirationSeconds: &secs},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("request a token for serviceaccount %s/%s: %w", namespace, serviceAccount, err)
	}
	if tr.Status.Token == "" {
		return "", fmt.Errorf("the cluster issued an empty token for serviceaccount %s/%s", namespace, serviceAccount)
	}
	return tr.Status.Token, nil
}

func (k *kubeClients) ServiceProxy(ctx context.Context, p serviceProxyRequest) (int, []byte, error) {
	req := k.cs.CoreV1().RESTClient().Verb(p.Method).
		Namespace(p.Namespace).Resource("services").
		Name(p.Scheme + ":" + p.Service + ":" + strconv.Itoa(p.Port)).
		SubResource("proxy").Suffix(p.Path)
	for key, vals := range p.Query {
		for _, v := range vals {
			req = req.Param(key, v)
		}
	}
	for h, v := range p.Headers {
		req = req.SetHeader(h, v)
	}
	if p.Body != nil {
		req = req.Body(p.Body)
	}
	res := req.Do(ctx)
	var status int
	res.StatusCode(&status)
	body, err := res.Raw()
	if status == 0 {
		// No response at all: the API server was unreachable or refused the
		// proxy call itself (RBAC on services/proxy, a missing Service).
		return 0, nil, fmt.Errorf("service proxy %s/%s: %w", p.Namespace, p.Service, err)
	}
	// A non-2xx answer from the backend is data, not a transport error.
	return status, body, nil
}
