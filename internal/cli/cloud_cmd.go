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
// precedence (flag > env var > login file).
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
	cred, err := cloud.ResolveCredential(flagToken, ep.TokenEnv)
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

// newLoginCmd is `forge login`: obtain a credential for a hosted control
// plane and store it locally.
//
// It STANDS ALONE. A user who wants to deploy to hosted infrastructure
// without the reliant harness runs this and nothing else — forge shares
// no code and no binary dependency with reliant's CLI, so `reliant auth
// login` is never a prerequisite.
func newLoginCmd() *cobra.Command {
	var (
		envName  string
		token    string
		noVerify bool
	)

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate to a hosted control plane",
		Long: `Obtain a credential for the hosted control plane an environment declares,
and store it in ~/.forge/login.json (0600).

INTERACTIVE (a human): opens your browser and starts a temporary HTTP
server on a loopback port to receive the callback. The port is chosen by
the OS, so two logins can run at once.

NON-INTERACTIVE (CI): pass --token, or set the environment variable the
environment declares (FORGE_CONTROL_PLANE_TOKEN by default). A pipeline
has no browser, which is what machine tokens are for.

The endpoint comes from the ENVIRONMENT's KCL, not from CLI state:

    control_plane = forge.ControlPlane {
        endpoint = "https://api.example.com"
    }

so --env selects which declaration to read. There is no "current
context" to get out of sync with the repository.

CREDENTIAL PRECEDENCE, when any forge command talks to the control plane:

    1. --token           explicit, beats everything
    2. $<token_env>      the env var the environment declares — CI
    3. ~/.forge/login.json   what this command stored — a human's default

Most explicit wins. The login file is last deliberately: it is the most
ambient of the three, so a stale one must never shadow the credential CI
just injected.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			decl, err := controlPlaneDeclaration(ctx, envName)
			if err != nil {
				return err
			}
			ep, err := cloud.ResolveEndpoint(envName, decl)
			if err != nil {
				return err
			}

			var stored cloud.StoredLogin
			if strings.TrimSpace(token) != "" {
				stored = cloud.StoredLogin{Token: strings.TrimSpace(token), Endpoint: ep.URL}
			} else {
				stored, err = cloud.BrowserLogin{
					Endpoint: ep,
					Out:      out,
					Timeout:  loginCallbackTimeout,
				}.Run(ctx)
				if err != nil {
					return err
				}
			}

			// Verify before storing, so a bad token fails HERE rather
			// than at the first real command — where it would look like
			// a problem with that command instead of with the login.
			if !noVerify {
				verifyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				cred := cloud.Credential{Token: stored.Token, Source: cloud.SourceFlag, From: "the credential just obtained"}
				if err := cloud.VerifyToken(verifyCtx, ep, cred); err != nil {
					return fmt.Errorf("the credential was not accepted by %s: %w\n"+
						"(pass --no-verify to store it anyway)", ep.URL, err)
				}
			}

			path, err := cloud.LoginFilePath()
			if err != nil {
				return err
			}
			if err := cloud.WriteLogin(path, stored); err != nil {
				return fmt.Errorf("store credential: %w", err)
			}

			fmt.Fprintf(out, "Logged in to %s\n", ep.URL)
			fmt.Fprintf(out, "  Declared by: env %q\n", ep.Env)
			if stored.Account != "" {
				fmt.Fprintf(out, "  Account:     %s\n", stored.Account)
			}
			fmt.Fprintf(out, "  Credential:  %s\n", path)
			fmt.Fprintf(out, "\nIn CI, set %s instead — it takes precedence over this file.\n", ep.TokenEnv)
			return nil
		},
	}

	cmd.Flags().StringVar(&envName, "env", "prod", "Environment whose control_plane declaration names the endpoint")
	cmd.Flags().StringVar(&token, "token", "", "Use this token instead of a browser flow (for CI)")
	cmd.Flags().BoolVar(&noVerify, "no-verify", false, "Store the credential without checking it against the endpoint")
	return cmd
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

			cred, err := cloud.ResolveCredential(token, ep.TokenEnv)
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
	cmd.Flags().StringVar(&token, "token", "", "Credential to use, ahead of the env var and the login file")
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
credential from --token, then the declared env var, then ~/.forge/login.json.

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
	cmd.Flags().StringVar(&token, "token", "", "Credential to use, ahead of the env var and the login file")
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
