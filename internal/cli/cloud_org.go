package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/cloud"
)

const procListMyOrganizations = "controlplane.v1.OrganizationService/ListMyOrganizations"

type wireMyOrganization struct {
	ID                     string `json:"id"`
	Name                   string `json:"name"`
	Slug                   string `json:"slug"`
	Role                   string `json:"role"`
	IsDefault              bool   `json:"isDefault"`
	CanCreateHostedProject *bool  `json:"canCreateHostedProject,omitempty"`
}

func newCloudOrgCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "org", Aliases: []string{"organization", "organizations"}, Short: "Discover your organizations and their IDs"}
	var f cloudTokenFlags
	list := &cobra.Command{
		Use: "list", Short: "List your organizations, IDs and permission to create hosted projects", Args: cobra.NoArgs,
		Long: `List the signed-in person's organizations, including IDs usable with
forge env deploy <env> --org <id>. A first deploy automatically selects the only
organization where you may create projects; with several, select one explicitly.
The account's default organization never overrides an existing project's owner.

Works outside a project. --env selects a project's declared control plane.
Automation credentials are already bound to one organization; use forge cloud
status to inspect them.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ep, cred, err := f.target(cmd.Context(), projectDirForKCL())
			if err != nil {
				return err
			}
			return runCloudOrgList(cmd.Context(), organizationDiscoveryClient(ep, cred), f.jsonOut, cmd.OutOrStdout())
		},
	}
	list.Flags().StringVar(&f.env, "env", "", "Env whose declared control plane to use (default: Reliant cloud)")
	list.Flags().StringVar(&f.token, "token", "", "Credential to use, ahead of the env var and credentials file")
	list.Flags().BoolVar(&f.jsonOut, "json", false, "Emit machine-readable JSON")
	cmd.AddCommand(list)
	return cmdutil.StrictGroup(cmd)
}

// Account discovery must work before this checkout has any project, even
// when --env reads an endpoint from an existing or stale project binding.
func organizationDiscoveryClient(ep cloud.Endpoint, cred cloud.Credential) *cloud.Client {
	ep.ProjectID, ep.ProjectName = "", ""
	return cloud.NewClient(ep, cred)
}

func runCloudOrgList(ctx context.Context, caller cloudCaller, jsonOut bool, out io.Writer) error {
	var reply struct {
		Organizations []wireMyOrganization `json:"organizations"`
		DefaultOrgID  string               `json:"defaultOrgId"`
	}
	if err := caller.Call(ctx, procListMyOrganizations, struct{}{}, &reply); err != nil {
		return err
	}
	if reply.Organizations == nil {
		reply.Organizations = []wireMyOrganization{}
	}
	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			OK            bool                 `json:"ok"`
			Organizations []wireMyOrganization `json:"organizations"`
			DefaultOrgID  string               `json:"defaultOrgId"`
		}{true, reply.Organizations, reply.DefaultOrgID})
	}
	if len(reply.Organizations) == 0 {
		_, err := fmt.Fprintln(out, "No organizations found. Create or join an organization in the web app before deploying while signed in.")
		return err
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tID\tROLE\tCREATE PROJECT\tDEFAULT")
	for _, org := range reply.Organizations {
		permission := "unknown (older server)"
		if org.CanCreateHostedProject != nil {
			permission = "no"
			if *org.CanCreateHostedProject {
				permission = "yes"
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%t\n", org.Name, org.ID, org.Role, permission, org.IsDefault)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(out, "Select a first deployment's owner with: forge env deploy <env> --org <id>")
	return err
}
