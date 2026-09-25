package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/pkg/credentials"
)

// controlPlaneDeclaration reads one environment's hosted control-plane
// declaration out of its rendered KCL.
//
// The environment NAME is the only input, and that is the whole design:
// there is no `forge context use`, no current-endpoint file, and nothing
// a previous command could have left behind that changes the answer.
// `forge cloud releases staging` and `forge cloud releases prod` resolve
// two different endpoints in one process because each reads its own
// env's declaration.
//
// Returns nil (no error) when the env declares none — the default. The
// caller turns that into the actionable message, because what to say
// depends on what the caller was trying to do.
func controlPlaneDeclaration(ctx context.Context, envName string) (*cloud.Declaration, error) {
	entities, err := RenderKCL(ctx, projectDirForKCL(), envName)
	if err != nil {
		return nil, fmt.Errorf("render KCL for env %q: %w", envName, err)
	}
	return declarationFromEntities(entities), nil
}

// declarationFromEntities maps the rendered entity onto the cloud
// package's consumer-side type. Split out from the render so it can be
// tested without invoking KCL.
func declarationFromEntities(entities *KCLEntities) *cloud.Declaration {
	if entities == nil || entities.ControlPlane == nil {
		return nil
	}
	cp := entities.ControlPlane
	if strings.TrimSpace(cp.Endpoint) == "" {
		return nil
	}
	return &cloud.Declaration{
		Endpoint:     cp.Endpoint,
		TokenEnv:     cp.TokenEnv,
		Organization: cp.Organization,
	}
}

// resolveCloudTarget is the one path every hosted command takes: resolve
// the endpoint from the ENV's declaration, then the credential by
// precedence (flag > env var > credentials file entry for the endpoint).
//
// Order matters here. The endpoint resolves FIRST so that an env with no
// declaration gets "this env declares no hosted control plane" rather
// than "no credential" — the second would send a user to `forge login`
// for a problem logging in cannot fix, which is exactly the wrong hint.
func resolveCloudTarget(ctx context.Context, envName, flagToken string) (cloud.Endpoint, cloud.Credential, error) {
	decl, err := controlPlaneDeclaration(ctx, envName)
	if err != nil {
		return cloud.Endpoint{}, cloud.Credential{}, err
	}
	ep, err := cloud.ResolveEndpoint(envName, decl)
	if err != nil {
		return cloud.Endpoint{}, cloud.Credential{}, err
	}
	cred, err := cloud.ResolveCredential(flagToken, ep)
	if err != nil {
		return cloud.Endpoint{}, cloud.Credential{}, err
	}
	return ep, cred, nil
}

// loginCallbackTimeout bounds how long `forge login` waits for the
// browser to come back. Long enough for a real SSO round trip including
// an MFA prompt, short enough that a flow abandoned in a closed tab
// fails with the --token hint instead of hanging the terminal.
const loginCallbackTimeout = 5 * time.Minute

// openLoginBrowser launches the browser for `forge login`. A seam so the
// command's test drives the real flow against an httptest control plane
// without opening anything.
var openLoginBrowser = cloud.OpenBrowser

// loginTarget resolves which control plane `forge login` / `forge logout`
// act on: an env's KCL declaration, or an explicit --endpoint. Exactly one.
// There is no default and no "current" server — a login lands in the file
// under the endpoint it was made against, and a command finds it by the
// endpoint ITS env declares.
func loginTarget(ctx context.Context, args []string, endpoint string) (cloud.Endpoint, error) {
	endpoint = strings.TrimSpace(endpoint)
	switch {
	case len(args) == 1 && endpoint != "":
		return cloud.Endpoint{}, fmt.Errorf("pass an env OR --endpoint, not both")
	case len(args) == 1:
		decl, err := controlPlaneDeclaration(ctx, args[0])
		if err != nil {
			return cloud.Endpoint{}, err
		}
		return cloud.ResolveEndpoint(args[0], decl)
	case endpoint != "":
		if _, err := credentials.Normalize(endpoint); err != nil {
			return cloud.Endpoint{}, err
		}
		return cloud.ResolveEndpoint("", &cloud.Declaration{Endpoint: endpoint})
	default:
		return cloud.Endpoint{}, fmt.Errorf("name the control plane: `forge login <env>` (reads deploy/kcl/<env>/main.k's " +
			"forge.ControlPlane) or `forge login --endpoint https://…`")
	}
}

// newLoginCmd is `forge login`: obtain a credential for a hosted control
// plane and store it in the shared credentials file, keyed by endpoint.
//
// It STANDS ALONE. A user who wants to deploy to hosted infrastructure
// without the reliant harness runs this and nothing else — forge depends on
// no reliant code, so `reliant auth login` is never a prerequisite. The two
// CLIs share the FILE (forge/pkg/credentials), which forge owns.
func newLoginCmd() *cobra.Command {
	var (
		endpoint string
		token    string
		noVerify bool
	)

	cmd := &cobra.Command{
		Use:   "login [env]",
		Short: "Authenticate to a hosted control plane",
		Long: `Obtain a credential for a hosted control plane and store it in the shared
credentials file (` + credentialsPathForHelp() + `, 0600), keyed by the endpoint.

The control plane is named by an ENVIRONMENT, whose KCL declares it:

    control_plane = forge.ControlPlane {
        endpoint = "https://admin.example.com"
    }

or directly with --endpoint. There is no "current" server: every forge
command finds its credential by the endpoint its own env declares, so a
staging login is never presented to prod.

INTERACTIVE (a human): the OAuth authorization-code flow with PKCE. forge
opens your browser at <endpoint>/oauth/authorize, you sign in and approve,
and the browser returns a one-time code to a temporary listener on a
loopback port (chosen by the OS, so two logins can run at once). forge
redeems it at <endpoint>/oauth/token for an access token (rlat_…, 90 days).

NON-INTERACTIVE (CI): pass --token, or set the environment variable the
environment declares (FORGE_CONTROL_PLANE_TOKEN by default) and skip login
entirely. A pipeline has no browser, which is what org tokens are for.

CREDENTIAL PRECEDENCE, when any forge command talks to the control plane:

    1. --token           explicit, beats everything
    2. $<token_env>      the env var the environment declares — CI
    3. the credentials file entry for that env's endpoint — a human's default`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			ep, err := loginTarget(ctx, args, endpoint)
			if err != nil {
				return err
			}

			var stored credentials.Credential
			if t := strings.TrimSpace(token); t != "" {
				// Verify a pasted token before storing, so a bad one fails
				// HERE rather than at the first real command. A browser login
				// needs no check: the server minted it seconds ago.
				if !noVerify {
					verifyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
					defer cancel()
					cred := cloud.Credential{Token: t, Source: cloud.SourceFlag, From: "--token"}
					if err := cloud.VerifyToken(verifyCtx, ep, cred); err != nil {
						return fmt.Errorf("the token was not accepted by %s: %w\n"+
							"(pass --no-verify to store it anyway)", ep.URL, err)
					}
				}
				stored = credentials.Credential{Token: t, CreatedAt: time.Now().UTC()}
			} else {
				stored, err = cloud.BrowserLogin{
					Endpoint: ep,
					Out:      out,
					Timeout:  loginCallbackTimeout,
					OpenURL:  openLoginBrowser,
				}.Run(ctx)
				if err != nil {
					return err
				}
			}

			path, err := cloud.CredentialsPath()
			if err != nil {
				return err
			}
			if err := credentials.Store(path, ep.URL, cloud.ClientID, stored); err != nil {
				return fmt.Errorf("store credential: %w", err)
			}
			key, _ := credentials.Normalize(ep.URL)
			fmt.Fprintf(out, "Logged in to %s\n", key)
			if ep.Env != "" {
				fmt.Fprintf(out, "  Declared by: env %q\n", ep.Env)
			}
			if len(stored.Scopes) > 0 {
				fmt.Fprintf(out, "  Scopes:      %s\n", strings.Join(stored.Scopes, " "))
			}
			if stored.ExpiresAt != nil {
				fmt.Fprintf(out, "  Expires:     %s\n", stored.ExpiresAt.Format(time.RFC3339))
			}
			fmt.Fprintf(out, "  Credential:  %s\n", path)
			fmt.Fprintf(out, "\nIn CI, set %s instead — it takes precedence over this file.\n", ep.TokenEnv)
			return nil
		},
	}

	cmd.Flags().StringVar(&endpoint, "endpoint", "", "Control plane URL, instead of reading an env's declaration")
	cmd.Flags().StringVar(&token, "token", "", "Store this token instead of running the browser flow")
	cmd.Flags().BoolVar(&noVerify, "no-verify", false, "With --token: store it without checking it against the endpoint")
	return cmd
}

// newLogoutCmd is `forge logout`: drop ONE endpoint's entry from the shared
// credentials file. Every other endpoint's entry — including reliant's — is
// untouched. The token itself stays valid server-side until it expires or is
// revoked in the web UI; logout forgets it locally, which is all a CLI can
// honestly promise.
func newLogoutCmd() *cobra.Command {
	var endpoint string
	cmd := &cobra.Command{
		Use:   "logout [env]",
		Short: "Forget the stored credential for one control plane",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ep, err := loginTarget(cmd.Context(), args, endpoint)
			if err != nil {
				return err
			}
			path, err := cloud.CredentialsPath()
			if err != nil {
				return err
			}
			existed, err := credentials.Remove(path, ep.URL, cloud.ClientID)
			if err != nil {
				return err
			}
			key, _ := credentials.Normalize(ep.URL)
			if !existed {
				fmt.Fprintf(cmd.OutOrStdout(), "Not logged in to %s (nothing in %s)\n", key, path)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Logged out of %s (removed from %s)\n", key, path)
			return nil
		},
	}
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "Control plane URL, instead of reading an env's declaration")
	return cmd
}

// credentialsPathForHelp renders the file location for --help without
// failing help when HOME is unset.
func credentialsPathForHelp() string {
	if p, err := cloud.CredentialsPath(); err == nil {
		return p
	}
	return "~/.config/forge/credentials.json"
}

// newCloudCmd is `forge cloud`: commands that talk to the hosted control
// plane an environment declares.
//
// It is deliberately NARROW. Only operations backed by an implemented
// server-side RPC live here — see `forge cloud releases`, whose comment
// records which of the hosted API's RPCs are real and which are
// scaffold stubs. Adding a subcommand for a stub would produce a
// command that looks supported and returns Unimplemented, which is
// worse than the command not existing.
func newCloudCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cloud",
		Short: "Talk to the hosted control plane an environment declares",
		Long: `Commands that reach the hosted control plane declared by an environment's
forge.ControlPlane block.

The endpoint is per-environment and comes from KCL, so which server a
command hits follows from its env argument — not from any stored
"current context".

Authenticate with ` + "`forge login`" + `, or set the declared token env var.`,
	}
	cmd.AddCommand(newCloudStatusCmd())
	cmd.AddCommand(newCloudReleasesCmd())
	// StrictGroup, not a bare group: cobra's default would accept
	// `forge cloud <typo>` and exit 0, which for a command that talks to
	// a remote endpoint reads as "it worked".
	return cmdutil.StrictGroup(cmd)
}

// newCloudStatusCmd reports what WOULD be used, without calling anything.
//
// This exists because the most common hosted-auth question is "which
// endpoint and which credential is this about to use", and answering it
// by running a real command conflates configuration problems with
// server-side ones.
func newCloudStatusCmd() *cobra.Command {
	var token string

	cmd := &cobra.Command{
		Use:   "status <env>",
		Short: "Show the endpoint and credential source for an environment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			envName := args[0]
			out := cmd.OutOrStdout()

			decl, err := controlPlaneDeclaration(cmd.Context(), envName)
			if err != nil {
				return err
			}
			ep, err := cloud.ResolveEndpoint(envName, decl)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "env %q\n", ep.Env)
			fmt.Fprintf(out, "  endpoint:  %s   (declared in deploy/kcl/%s/main.k)\n", ep.URL, ep.Env)
			fmt.Fprintf(out, "  token env: %s\n", ep.TokenEnv)
			if ep.Organization != "" {
				fmt.Fprintf(out, "  org:       %s\n", ep.Organization)
			}

			cred, err := cloud.ResolveCredential(token, ep)
			if err != nil {
				fmt.Fprintf(out, "  credential: NONE\n")
				return err
			}
			// The SOURCE, never the token. Printing a bearer credential
			// to a terminal puts it in scrollback and in CI logs.
			fmt.Fprintf(out, "  credential: present, from %s (%s)\n", cred.From, cred.Source)
			return nil
		},
	}
	cmd.Flags().StringVar(&token, "token", "", "Credential to use, ahead of the env var and the credentials file")
	return cmd
}

// newCloudReleasesCmd lists the caller's releases from the hosted
// control plane.
//
// WHY ListReleases AND NOT ListDeployments. ListDeployments is a
// scaffold stub server-side: control-plane's
// internal/handlers/deploy/handlers_deployment.go returns
// CodeUnimplemented for it, by design, until the deploy domain service
// is wired. Building this command against it would have produced a
// forge command that authenticates correctly, reaches the server
// correctly, and then always fails — proving nothing about the seam.
//
// ListReleases (rpc_list_releases.go) is genuinely implemented: it
// checks the caller's org from their claims and reads a real store. It
// is therefore the honest choice for the ONE command that demonstrates
// the whole path end to end.
func newCloudReleasesCmd() *cobra.Command {
	var (
		token      string
		limit      int
		jsonOutput bool
	)

	cmd := &cobra.Command{
		Use:   "releases <env>",
		Short: "List releases from the hosted control plane",
		Long: `List the releases the hosted control plane holds for your organization.

The endpoint comes from <env>'s forge.ControlPlane declaration; the
credential from --token, then the declared env var, then the credentials file entry for
that endpoint (` + "`forge login <env>`" + `).

Scope is always the caller's own organization — the request carries no
organization field, so there is nothing to widen.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ep, cred, err := resolveCloudTarget(cmd.Context(), args[0], token)
			if err != nil {
				return err
			}
			return runCloudReleases(cmd.Context(), cloud.NewClient(ep, cred), limit, jsonOutput, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&token, "token", "", "Credential to use, ahead of the env var and the credentials file")
	cmd.Flags().IntVar(&limit, "limit", 20, "Maximum releases to return")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit the raw response as JSON")
	return cmd
}

// cloudRelease is the subset of controlplane.v1.DeployRelease forge
// reads.
//
// Declared locally and narrowly ON PURPOSE. forge does not vendor the
// control plane's protos — that coupling is the thing the whole design
// avoids — so the shape forge depends on is written down here, in one
// place, rather than inferred from a generated type. A field forge does
// not use is a field forge does not break on.
type cloudRelease struct {
	ID        string `json:"id"`
	Version   string `json:"version"`
	GitCommit string `json:"gitCommit"`
	GitTag    string `json:"gitTag"`
	GitDirty  bool   `json:"gitDirty"`
	CreatedAt string `json:"createdAt"`
}

type cloudReleasesResponse struct {
	Releases []cloudRelease `json:"releases"`
}

// cloudCaller is the one method this command needs from a client.
// Declared at the CONSUMER so the test can drive it without a real
// endpoint, and so Client stays a concrete type nothing has to satisfy.
type cloudCaller interface {
	Call(ctx context.Context, procedure string, req, out any) error
}

func runCloudReleases(ctx context.Context, client cloudCaller, limit int, jsonOutput bool, out io.Writer) error {
	req := map[string]any{}
	if limit > 0 {
		req["limit"] = limit
	}

	var resp cloudReleasesResponse
	if err := client.Call(ctx, "controlplane.v1.DeployService/ListReleases", req, &resp); err != nil {
		return err
	}

	if jsonOutput {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}

	if len(resp.Releases) == 0 {
		fmt.Fprintln(out, "No releases.")
		return nil
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "VERSION\tCOMMIT\tCREATED\tID")
	for _, r := range resp.Releases {
		commit := r.GitCommit
		if len(commit) > 8 {
			commit = commit[:8]
		}
		if r.GitDirty {
			commit += "+dirty"
		}
		if commit == "" {
			commit = "-"
		}
		created := r.CreatedAt
		if created == "" {
			created = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Version, commit, created, r.ID)
	}
	return tw.Flush()
}
