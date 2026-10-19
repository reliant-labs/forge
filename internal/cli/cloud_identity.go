package cli

import (
	"context"
	"fmt"
	"net/mail"
	"strings"
	"text/tabwriter"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/internal/deploytarget"
)

// Only the public, membership-checked IdentityService belongs here. The
// reconciler's IdentityInternalService and its credentials are never CLI inputs.
const (
	procIdentityStatus = "controlplane.v1.IdentityService/GetIdentityStatus"
	procIdentityUsers  = "controlplane.v1.IdentityService/ListUsers"
	procIdentityInvite = "controlplane.v1.IdentityService/InviteUser"
)

func newCloudIdentityCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "identity", Short: "Inspect managed identity and invite users to an environment",
		Long: `Manage the identity an environment declares in KCL, using its control plane.

Requires a human session (Reliant's session helper or forge login); machine
credentials are refused by the server. Members may read; invitations require
an org admin. The organization is resolved from the authenticated server, not
a token claim or a flag. The environment must already exist in this project.

Inviting sends an email link for the recipient to set their own credential.
There is no password option, seed account or implicit recipient. Roles managed
in deploy/kcl/identity.k (members_from = "declared") are read-only here.`,
	}
	cmd.AddCommand(newCloudIdentityStatusCmd(), newCloudIdentityUsersCmd(), newCloudIdentityInviteCmd())
	return cmdutil.StrictGroup(cmd)
}

// Keep KCL rendering outside the HTTP contract tests; production always uses
// the same per-environment endpoint and credential resolution as cloud releases.
var resolveIdentityCloudTarget = resolveCloudTarget

type cloudIdentityFlags struct {
	token string
	json  bool
}

func (f *cloudIdentityFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.token, "token", "", "Credential to use, ahead of the env var, stored login and session helper")
	cmd.Flags().BoolVar(&f.json, "json", false, "Emit machine-readable JSON (never credentials)")
}

func (f *cloudIdentityFlags) target(ctx context.Context, env string) (cloudCaller, map[string]any, error) {
	ep, cred, err := resolveIdentityCloudTarget(ctx, env, f.token)
	if err != nil {
		return nil, nil, err
	}
	client := cloud.NewClient(ep, cred)
	org, err := cloud.ResolveOrganization(ctx, client)
	if err != nil {
		return nil, nil, err
	}
	id, err := deploytarget.LookupHostedEnvironment(ctx, client, hostedProjectName(), env)
	if err != nil {
		return nil, nil, err
	}
	return client, map[string]any{"orgId": org, "environmentId": id}, nil
}

// Narrow proto3 JSON projections of controlplane.v1's identity messages.
// Unknown response fields are deliberately not forwarded to stdout.
type identityAppWire struct {
	Name            string `json:"name"`
	ClientID        string `json:"clientId"`
	BrokerExpiresAt string `json:"brokerExpiresAt,omitempty"`
}

type identityDeliveryWire struct {
	Kind           string `json:"kind"`
	Cluster        string `json:"cluster"`
	Namespace      string `json:"namespace"`
	Name           string `json:"name"`
	State          string `json:"state"`
	LastError      string `json:"lastError,omitempty"`
	LastVerifiedAt string `json:"lastVerifiedAt,omitempty"`
}

type identityStatusDocument struct {
	jsonEnvelope
	HasInstance          bool                   `json:"hasInstance"`
	State                string                 `json:"state"`
	Issuer               string                 `json:"issuer"`
	Domain               string                 `json:"domain"`
	Public               bool                   `json:"public"`
	Adopted              bool                   `json:"adopted"`
	Apps                 []identityAppWire      `json:"apps"`
	Deliveries           []identityDeliveryWire `json:"deliveries"`
	OwnerEnvironmentID   string                 `json:"ownerEnvironmentId"`
	SharerEnvironmentIDs []string               `json:"sharerEnvironmentIds"`
	LastReconciledAt     string                 `json:"lastReconciledAt,omitempty"`
	NextAttemptAt        string                 `json:"nextAttemptAt,omitempty"`
	LastError            string                 `json:"lastError,omitempty"`
	Failures             int32                  `json:"failures"`
	GraceUntil           string                 `json:"graceUntil,omitempty"`
	ExportURLAvailable   bool                   `json:"exportUrlAvailable"`
	Refused              string                 `json:"refused,omitempty"`
}

func newCloudIdentityStatusCmd() *cobra.Command {
	var f cloudIdentityFlags
	cmd := &cobra.Command{
		Use: "status <env>", Short: "Show managed-identity state, issuer and delivery metadata",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, req, err := f.target(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			var doc identityStatusDocument
			if err := client.Call(cmd.Context(), procIdentityStatus, req, &doc); err != nil {
				return fmt.Errorf("identity status: %w", err)
			}
			doc.stamp(nil)
			if f.json {
				return writeJSONDocument(cmd.OutOrStdout(), doc)
			}
			out := cmd.OutOrStdout()
			if !doc.HasInstance {
				fmt.Fprintln(out, "No managed identity instance.")
			} else {
				fmt.Fprintf(out, "State: %s\nIssuer: %s\n", doc.State, doc.Issuer)
				for _, app := range doc.Apps {
					fmt.Fprintf(out, "App: %s (client %s)\n", app.Name, app.ClientID)
				}
				for _, d := range doc.Deliveries {
					fmt.Fprintf(out, "Delivery: %s %s/%s/%s %s %s\n", d.Kind, d.Cluster, d.Namespace, d.Name, d.State, d.LastError)
				}
			}
			if doc.Refused != "" {
				fmt.Fprintf(out, "Refused: %s\n", doc.Refused)
			}
			if doc.LastError != "" {
				fmt.Fprintf(out, "Last error: %s\n", doc.LastError)
			}
			return nil
		},
	}
	f.register(cmd)
	return cmd
}

type identityUserWire struct {
	UserID      string   `json:"userId"`
	Email       string   `json:"email"`
	DisplayName string   `json:"displayName"`
	State       string   `json:"state"`
	Roles       []string `json:"roles"`
	CreatedAt   string   `json:"createdAt,omitempty"`
}

type identityRoleWire struct {
	Key         string `json:"key"`
	DisplayName string `json:"displayName"`
	Group       string `json:"group"`
	Declared    bool   `json:"declared"`
}

type identityUsersDocument struct {
	jsonEnvelope
	Users         []identityUserWire `json:"users"`
	NextPageToken string             `json:"nextPageToken"`
	Roles         []identityRoleWire `json:"roles"`
}

func newCloudIdentityUsersCmd() *cobra.Command {
	var f cloudIdentityFlags
	var query, pageToken string
	var pageSize int
	cmd := &cobra.Command{
		Use: "users <env>", Short: "List one page of users and declared roles (no credentials)",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if pageSize < 0 || pageSize > 100 || utf8.RuneCountInString(query) > 200 || utf8.RuneCountInString(pageToken) > 64 {
				return fmt.Errorf("--page-size must be 0..100, --query at most 200 characters and --page-token at most 64 characters")
			}
			client, req, err := f.target(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			req["pageSize"], req["pageToken"] = pageSize, pageToken
			if cmd.Flags().Changed("query") {
				req["query"] = query
			}
			doc := identityUsersDocument{Users: []identityUserWire{}, Roles: []identityRoleWire{}}
			if err := client.Call(cmd.Context(), procIdentityUsers, req, &doc); err != nil {
				return fmt.Errorf("identity users: %w", err)
			}
			doc.stamp(nil)
			if f.json {
				return writeJSONDocument(cmd.OutOrStdout(), doc)
			}
			out := cmd.OutOrStdout()
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "EMAIL\tUSER ID\tSTATE\tROLES")
			for _, u := range doc.Users {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", u.Email, u.UserID, u.State, strings.Join(u.Roles, ","))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			for _, r := range doc.Roles {
				fmt.Fprintf(out, "Role: %s (declared/read-only: %t)\n", r.Key, r.Declared)
			}
			if doc.NextPageToken != "" {
				fmt.Fprintf(out, "Next page: --page-token %s\n", doc.NextPageToken)
			}
			return nil
		},
	}
	f.register(cmd)
	cmd.Flags().StringVar(&query, "query", "", "Case-insensitive email substring (max 200 characters)")
	cmd.Flags().IntVar(&pageSize, "page-size", 20, "Maximum users in this page (0 uses server default, max 100)")
	cmd.Flags().StringVar(&pageToken, "page-token", "", "Continuation from the preceding response")
	return cmd
}

type identityInviteDocument struct {
	jsonEnvelope
	UserID  string `json:"userId"`
	Created bool   `json:"created"`
}

func newCloudIdentityInviteCmd() *cobra.Command {
	var f cloudIdentityFlags
	var email string
	var roles []string
	cmd := &cobra.Command{
		Use: "invite <env> --email <address> --role <role>", Short: "Email an invitation and grant console-managed roles (org admin)",
		Long: `Invite this explicit email address; repeat --role for multiple roles.
The recipient sets their own credential using the invitation email. An existing
account receives the requested roles (created=false). The server refuses unknown
or declared/read-only roles, machine credentials and an identity that is not
ready. Such a refusal is an error, never a fallback to a seeded password.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			address, err := mail.ParseAddress(email)
			if err != nil || address.Address != email || len(email) > 254 {
				return fmt.Errorf("--email must be an explicit email address, without a display name")
			}
			if len(roles) == 0 || len(roles) > 20 {
				return fmt.Errorf("provide 1..20 --role flags")
			}
			for _, role := range roles {
				if strings.TrimSpace(role) == "" || utf8.RuneCountInString(role) > 200 {
					return fmt.Errorf("each --role must be nonempty and at most 200 characters")
				}
			}
			client, req, err := f.target(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			req["email"], req["roles"] = email, roles
			var doc identityInviteDocument
			if err := client.Call(cmd.Context(), procIdentityInvite, req, &doc); err != nil {
				return fmt.Errorf("invite identity user: %w", err)
			}
			if strings.TrimSpace(doc.UserID) == "" {
				return fmt.Errorf("invite identity user: the control plane answered with no user id")
			}
			doc.stamp(nil)
			if f.json {
				return writeJSONDocument(cmd.OutOrStdout(), doc)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Invitation requested for %s (user %s, created=%t).\nThe recipient sets their credential through the invitation email.\n", email, doc.UserID, doc.Created)
			return nil
		},
	}
	f.register(cmd)
	cmd.Flags().StringVar(&email, "email", "", "Explicit recipient email address (required; never inferred)")
	cmd.Flags().StringArrayVar(&roles, "role", nil, "Console-managed role to grant (required; repeat for multiple roles)")
	_ = cmd.MarkFlagRequired("email")
	_ = cmd.MarkFlagRequired("role")
	return cmd
}
