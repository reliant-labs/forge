package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/pkg/authhooks"
)

// newAuthCmd is `forge auth`: helpers for an app's auth provider. Today it
// holds the inbound auth-event hooks (forge/pkg/authhooks); identity itself
// is declared in KCL and wired in the app's own SetupAuth.
func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Auth-provider helpers: the inbound auth-event hooks for Supabase, Clerk and Auth0",
	}
	hooks := &cobra.Command{
		Use:   "hooks",
		Short: "Inbound auth-event hooks: the provider reports sign-ups and sign-ins to /_forge/auth/hooks/<provider>",
		Long: `An app on Supabase, Clerk or Auth0 reports sign-ups and sign-ins from the
provider itself: the provider posts to a handler from forge/pkg/authhooks
that the app mounts on its own mux (conventionally at
/_forge/auth/hooks/<provider>), which records them as the standard auth
events (auth_signed_up, auth_signed_in).

Supabase needs SQL on its side (triggers through pg_net); print it with
` + "`forge auth hooks sql supabase`" + `. Clerk (a Svix webhook) and Auth0 (a
Log Stream) are configured in their dashboards. The observability/product-events/auth-events
skill has every provider's steps.`,
	}
	hooks.AddCommand(&cobra.Command{
		Use:   "sql <provider>",
		Short: "Print the provider-side SQL of a hook (supabase): triggers that post sign-ups and sign-ins through pg_net",
		Long: `Print the SQL that makes a Supabase project's auth database report sign-ups
and sign-ins to the app's /_forge/auth/hooks/supabase endpoint. It is
idempotent, asynchronous (pg_net, after commit) and never blocks or fails
auth; its header says how to set the Vault secrets, install, verify and
remove it. Commit it beside your other Supabase SQL:

  forge auth hooks sql supabase > deploy/supabase/forge_auth_events.sql`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch strings.ToLower(args[0]) {
			case "supabase":
				_, err := fmt.Fprint(cmd.OutOrStdout(), authhooks.SupabaseSQL())
				return err
			case "clerk", "auth0":
				return fmt.Errorf("%s needs no SQL: point its webhook at the handler the app mounts (authhooks.%s; forge skill load observability/product-events/auth-events)", args[0], map[string]string{"clerk": "Clerk", "auth0": "Auth0"}[strings.ToLower(args[0])])
			}
			return fmt.Errorf("no auth hook for %q: forge has hooks for supabase, clerk and auth0", args[0])
		},
	})
	cmd.AddCommand(cmdutil.StrictGroup(hooks))
	return cmdutil.StrictGroup(cmd)
}
