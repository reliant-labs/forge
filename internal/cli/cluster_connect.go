package cli

// `forge cluster connect` / `forge cluster disconnect` — registering a
// Kubernetes cluster the control plane may deploy INTO.
//
// THE SIBLING COMMANDS IN THIS NAMESPACE ARE LOCAL k3d LIFECYCLE. These two
// are the opposite direction: nothing is created, a cluster the owner already
// operates becomes addressable by name, and an environment points at it with
// `ClusterTarget.connected_cluster`. The verb is `connect` rather than `add`
// because the cluster already exists and keeps existing after `disconnect`.
//
// DECLARATIVE AND IDEMPOTENT BY NAME, which is what makes it safe in a
// script: ConnectCluster UPDATES an existing cluster of the same name rather
// than colliding, and the in-cluster RBAC is a server-side apply. Re-running
// after changing a cluster's endpoint is the supported way to fix it.
//
// forge does not vendor the control plane's protos, so the wire shapes below
// are the proto3-JSON subset forge reads, declared locally — the same rule the
// hosted deploy target and `forge domain` follow.

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/internal/cluster"
)

const (
	procConnectCluster = "controlplane.v1.ClusterService/ConnectCluster"
	procListClusters   = "controlplane.v1.ClusterService/ListClusters"
	procRemoveCluster  = "controlplane.v1.ClusterService/RemoveCluster"
)

// The namespace and ServiceAccount forge mints on the token path.
//
// `forge-system` rather than a name carrying the hub's internals: this object
// group lives in a CUSTOMER's cluster, so it is named after the tool that
// created it and is deletable by `disconnect` without a second thought.
const (
	connectTokenNamespace      = "forge-system"
	connectTokenServiceAccount = "forge-connect"
)

// wireConnectedCluster is the subset of controlplane.v1.Cluster forge reads.
// A field forge does not use is a field forge cannot break on.
type wireConnectedCluster struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name,omitempty"`
	Address string `json:"address,omitempty"`
	// Connection is the ClusterConnection value name.
	Connection string `json:"connection,omitempty"`
	// EnvironmentIDs are the live environments targeting this cluster. A
	// non-empty list blocks removal server-side.
	EnvironmentIDs []string `json:"environmentIds,omitempty"`
}

type wireConnectClusterResponse struct {
	Cluster wireConnectedCluster `json:"cluster"`
}

func newClusterConnectCmd() *cobra.Command {
	var (
		kubeContext string
		envName     string
		token       string
		dryRun      bool
	)
	cmd := &cobra.Command{
		Use:   "connect <name>",
		Short: "Register a Kubernetes cluster the control plane may deploy into",
		Long: `Register a cluster you operate, by address, so environments can target it.

Nothing is installed in your cluster beyond a ServiceAccount, its token and
the RBAC the platform's apply needs. forge reads the API server address and CA
from your kubectl context, applies that grant through it, and uploads the
token to the control plane write-only. It is never printed.

  forge cluster connect prod-us --context gke_acme_us-central1_prod --env prod
  forge cluster connect vke-prod --context vke-prod --env prod
  forge cluster disconnect prod-us --env prod

ONE MECHANISM ON EVERY CLUSTER — GKE, EKS, AKS, VKE, k3s, bare metal. The
address registered is the context's own server, so pick the context whose
server the platform can reach (for a GKE private endpoint: a context written by
"gcloud container clusters get-credentials --internal-ip").

Re-running connect UPDATES the cluster of that name, so fixing an endpoint is
the same command again. --env names WHICH control plane to talk to, from that
env's forge.ControlPlane declaration.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClusterConnect(cmd.Context(), clusterConnectOptions{
				Name:        args[0],
				KubeContext: kubeContext,
				Env:         envName,
				Token:       token,
				DryRun:      dryRun,
				Out:         cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&kubeContext, "context", "", "kubectl context addressing the cluster (required)")
	cmd.Flags().StringVar(&envName, "env", "", "Environment whose control plane to talk to (required)")
	cmd.Flags().StringVar(&token, "token", "", "Credential to use, ahead of the env var and the credentials file")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print what would be sent and applied, and contact nothing")
	return cmd
}

func newClusterDisconnectCmd() *cobra.Command {
	var (
		kubeContext string
		envName     string
		token       string
	)
	cmd := &cobra.Command{
		Use:   "disconnect <name>",
		Short: "Deregister a connected cluster and remove what forge created in it",
		Long: `Deregister the connected cluster of this exact name.

Two halves, and the order matters: the control plane revokes the cluster's
credential first, then forge deletes the ServiceAccount, token Secret and RBAC
it created in the cluster. A token revoked server-side is already powerless, so
a failure to reach the cluster afterwards leaves litter rather than a live
credential.

REFUSED while any live environment targets the cluster — that check is the
control plane's, and it is why this is not a local delete.

  forge cluster disconnect prod-us --env prod --context gke_acme_us-central1_prod

--context is optional and names where to clean up. Without it, forge
deregisters the cluster and tells you which objects to delete by hand.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClusterDisconnect(cmd.Context(), args[0], kubeContext, envName, token, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&kubeContext, "context", "", "kubectl context to delete forge's objects through")
	cmd.Flags().StringVar(&envName, "env", "", "Environment whose control plane to talk to (required)")
	cmd.Flags().StringVar(&token, "token", "", "Credential to use, ahead of the env var and the credentials file")
	return cmd
}

type clusterConnectOptions struct {
	Name        string
	KubeContext string
	Env         string
	Token       string
	DryRun      bool
	Out         io.Writer
}

// clusterConnectClient resolves the endpoint and credential for an env and
// builds the client. A var so tests point it at an httptest control plane.
var clusterConnectClient = func(ctx context.Context, envName, token string) (cloudCaller, string, error) {
	envName = strings.TrimSpace(envName)
	if envName == "" {
		return nil, "", fmt.Errorf("--env is required: it names which control plane to talk to, from that " +
			"env's forge.ControlPlane declaration\nfix: re-run with `--env <env>` (the environments under deploy/kcl/)")
	}
	ep, cred, err := resolveCloudTarget(ctx, envName, token)
	if err != nil {
		return nil, "", err
	}
	// The org composes the hub namespace in the impersonated username, so the
	// RBAC forge applies is wrong without it — and wrong in the quiet way,
	// authorizing a user that never appears. It is the org the credential acts
	// for, which only the control plane knows.
	client := cloud.NewClient(ep, cred)
	org, err := cloud.ResolveOrganization(ctx, client)
	if err != nil {
		return nil, "", err
	}
	return client, org, nil
}

func runClusterConnect(ctx context.Context, opts clusterConnectOptions) error {
	name := strings.TrimSpace(opts.Name)
	target, err := readConnectTarget(opts.KubeContext)
	if err != nil {
		return err
	}

	if opts.DryRun {
		// Pure: no control plane, no cluster. The one thing it cannot show
		// is the token, which does not exist until a real run asks for one.
		return writeConnectDryRun(opts.Out, name, target, connectRBAC{
			ClusterName:         name,
			KubeContext:         target.Context,
			Org:                 "<the env's organization>",
			TokenNamespace:      connectTokenNamespace,
			TokenServiceAccount: connectTokenServiceAccount,
		})
	}

	client, org, err := clusterConnectClient(ctx, opts.Env, opts.Token)
	if err != nil {
		return err
	}
	rbac := connectRBAC{
		ClusterName:         name,
		KubeContext:         target.Context,
		Org:                 org,
		TokenNamespace:      connectTokenNamespace,
		TokenServiceAccount: connectTokenServiceAccount,
	}

	// THE IN-CLUSTER WRITE COMES FIRST, because the token does not exist
	// until the ServiceAccount and its Secret do, so there is nothing to
	// send before it.
	fmt.Fprintf(opts.Out, "applying bootstrap RBAC to %s (server-side, field manager forge)\n", target.Context)
	if err := connectApply(ctx, target.Context, rbac.bootstrapManifests()); err != nil {
		return fmt.Errorf("apply bootstrap RBAC to context %q: %w\n"+
			"This needs cluster-admin on the target. It is a one-time bootstrap: it grants the "+
			"platform the permissions its deploys need, which no deploy can grant itself", target.Context, err)
	}

	// Read AFTER the apply, because the token controller populates the
	// Secret asynchronously — a read immediately after creation finds it
	// empty, which is the flake that gets diagnosed as "just run it again".
	token, err := connectReadToken(ctx, target.Context, rbac)
	if err != nil {
		return err
	}
	req := map[string]any{
		"name":    name,
		"auth":    authToken,
		"address": target.Address,
		"caPem":   target.CAPEM,
		// WRITE-ONLY, and never printed: not in a summary, not in a debug
		// line, not on failure. The control plane has no field to return it
		// in either.
		"token": token,
	}

	var resp wireConnectClusterResponse
	if err := client.Call(ctx, procConnectCluster, req, &resp); err != nil {
		return fmt.Errorf("connect cluster %q: %w", name, err)
	}
	if resp.Cluster.ID == "" {
		return fmt.Errorf("connect cluster %q: the control plane returned no cluster id", name)
	}

	writeConnectSummary(opts.Out, resp.Cluster, target)
	writeGrantSummary(opts.Out, rbac)
	fmt.Fprintf(opts.Out, "\nPoint an environment at it in deploy/kcl/<env>/main.k:\n\n"+
		"    forge.ClusterTarget {\n        cluster = %q\n        namespace = \"...\"\n        connected_cluster = %q\n    }\n",
		target.Context, name)
	return nil
}

// writeConnectSummary is what the author reads first: the cluster as the
// control plane now holds it.
func writeConnectSummary(out io.Writer, c wireConnectedCluster, target connectTarget) {
	fmt.Fprintf(out, "\nconnected %s\n", c.Name)
	fmt.Fprintf(out, "  id        %s\n", c.ID)
	fmt.Fprintf(out, "  auth      %s\n", connectAuthLabel)
	fmt.Fprintf(out, "  address   %s\n", c.Address)
	fmt.Fprintf(out, "  context   %s\n", target.Context)
}

// connectAuthLabel is how the one auth reads in forge's vocabulary.
const connectAuthLabel = "token (scoped ServiceAccount token, minted in your cluster)"

func writeConnectDryRun(out io.Writer, name string, target connectTarget, rbac connectRBAC) error {
	fmt.Fprintf(out, "[dry-run] would connect %q\n", name)
	fmt.Fprintf(out, "  auth         %s\n", connectAuthLabel)
	fmt.Fprintf(out, "  address      %s\n", target.Address)
	fmt.Fprintf(out, "  caPem        %d bytes, read from context %s\n", len(target.CAPEM), target.Context)
	fmt.Fprintf(out, "  token        minted on apply, sent write-only, never printed\n")
	fmt.Fprintf(out, "\n[dry-run] would apply to %s:\n\n", target.Context)
	for _, line := range strings.Split(strings.TrimRight(rbac.bootstrapManifests(), "\n"), "\n") {
		fmt.Fprintf(out, "    %s\n", line)
	}
	return nil
}

// The two cluster-contacting steps, as seams. Tests drive the real command
// against a fake control plane without a cluster; nothing else substitutes
// them.
var (
	connectApply     = cluster.KubectlApply
	connectReadToken = readConnectToken
)

// readConnectToken reads the minted ServiceAccount token, waiting for the
// token controller to populate the Secret it was just handed.
func readConnectToken(ctx context.Context, kctx string, rbac connectRBAC) (string, error) {
	const (
		timeout = 60 * time.Second
		every   = time.Second
	)
	deadline := time.Now().Add(timeout)
	for {
		if b64 := kubectlSecretField(ctx, kctx, rbac.TokenNamespace, rbac.tokenSecretName(), "token"); b64 != "" {
			tok, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				return "", fmt.Errorf("decode the minted ServiceAccount token: %w", err)
			}
			return string(tok), nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("the ServiceAccount token Secret %s/%s on %q was not populated within %s.\n"+
				"The token controller fills it asynchronously; an empty Secret this long usually means "+
				"the cluster's token controller is not running",
				rbac.TokenNamespace, rbac.tokenSecretName(), kctx, timeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(every):
		}
	}
}

func runClusterDisconnect(ctx context.Context, name, kubeContext, envName, token string, out io.Writer) error {
	name = strings.TrimSpace(name)
	client, _, err := clusterConnectClient(ctx, envName, token)
	if err != nil {
		return err
	}
	// Resolved by exact name, because that is what the author typed and the
	// RPC is keyed on an id. A prefix or fuzzy match here would be a
	// deletion that hit the wrong cluster.
	id, err := lookupConnectedCluster(ctx, client, name)
	if err != nil {
		return err
	}
	// THE REVOKE COMES FIRST. A credential the control plane has revoked is
	// already powerless, so a cleanup that then fails leaves litter rather
	// than a live credential. The other order leaves a window where the
	// token is deleted but still registered, which reads as an outage.
	if err := client.Call(ctx, procRemoveCluster, map[string]any{"id": id}, &struct{}{}); err != nil {
		return fmt.Errorf("disconnect cluster %q: %w", name, err)
	}
	fmt.Fprintf(out, "deregistered %s (%s)\n", name, id)

	if strings.TrimSpace(kubeContext) == "" {
		fmt.Fprintf(out, "\nNo --context given, so nothing was deleted in the cluster. To clean up:\n\n"+
			"    kubectl --context <ctx> delete clusterrole,clusterrolebinding %s\n"+
			"    kubectl --context <ctx> delete namespace %s\n",
			connectBootstrapName(name), connectTokenNamespace)
		return nil
	}
	rbac := connectRBAC{
		ClusterName:         name,
		KubeContext:         kubeContext,
		TokenNamespace:      connectTokenNamespace,
		TokenServiceAccount: connectTokenServiceAccount,
	}
	if err := deleteConnectObjects(ctx, kubeContext, rbac, out); err != nil {
		return err
	}
	return nil
}

// deleteConnectObjects removes what forge created in the target. Deleted by
// exact name, and a missing object is not an error — a disconnect run twice,
// or after a partial connect, must converge rather than refuse.
func deleteConnectObjects(ctx context.Context, kctx string, rbac connectRBAC, out io.Writer) error {
	name := connectBootstrapName(rbac.ClusterName)
	for _, target := range []struct{ args []string }{
		{[]string{"delete", "clusterrolebinding", name, "--ignore-not-found"}},
		{[]string{"delete", "clusterrole", name, "--ignore-not-found"}},
		{[]string{"delete", "secret", rbac.tokenSecretName(), "-n", rbac.TokenNamespace, "--ignore-not-found"}},
		{[]string{"delete", "serviceaccount", rbac.TokenServiceAccount, "-n", rbac.TokenNamespace, "--ignore-not-found"}},
	} {
		cmd := exec.CommandContext(ctx, "kubectl", cluster.KubectlArgs(kctx, target.args...)...)
		scrubSubprocessLogEnv(cmd)
		if outBytes, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("delete %s on context %q: %w\n%s",
				strings.Join(target.args[1:3], " "), kctx, err, strings.TrimSpace(string(outBytes)))
		}
	}
	// The NAMESPACE IS LEFT ALONE, deliberately. forge created it only if it
	// was absent, it is shared by construction (a second connected cluster in
	// the same checkout uses it), and deleting a namespace deletes everything
	// anyone else put in it. The two objects forge owns inside it are gone.
	fmt.Fprintf(out, "deleted forge's ServiceAccount, token and RBAC through context %s\n", kctx)
	fmt.Fprintf(out, "  namespace %s was left in place (shared; forge deletes only the objects it owns)\n",
		rbac.TokenNamespace)
	return nil
}

// lookupConnectedCluster resolves a cluster NAME to the id the RPCs are keyed
// on, so the author never has to know a uuid.
func lookupConnectedCluster(ctx context.Context, c cloudCaller, name string) (string, error) {
	var resp struct {
		Clusters []wireConnectedCluster `json:"clusters"`
	}
	if err := c.Call(ctx, procListClusters, map[string]any{}, &resp); err != nil {
		return "", fmt.Errorf("list clusters: %w", err)
	}
	for _, cl := range resp.Clusters {
		if cl.Name == name {
			return cl.ID, nil
		}
	}
	known := make([]string, 0, len(resp.Clusters))
	for _, cl := range resp.Clusters {
		known = append(known, cl.Name)
	}
	if len(known) == 0 {
		return "", fmt.Errorf("no cluster named %q: this organization has no connected clusters", name)
	}
	return "", fmt.Errorf("no cluster named %q (this organization has: %s)", name, strings.Join(known, ", "))
}

// clusterConnectCommands are the two commands this file adds to `forge
// cluster`. Named so dev.go's newClusterCmd registers them without this file
// needing to know the parent's shape.
func clusterConnectCommands() []*cobra.Command {
	return []*cobra.Command{newClusterConnectCmd(), newClusterDisconnectCmd()}
}
