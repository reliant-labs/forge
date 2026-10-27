package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/pkg/credentials"
	"github.com/spf13/cobra"
)

// Organization selection belongs to a project and endpoint, never the user's
// global credentials. .forge's local state is gitignored by scaffolded projects.
const orgSelectionFile = ".forge/organizations.json"

func readProjectOrganizations() (string, map[string]string, error) {
	cfg, err := findProjectConfigFile()
	if errors.Is(err, ErrProjectConfigNotFound) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	// The state file's own ignore rule also covers older/adopted projects whose
	// root .gitignore did not include .forge yet.
	path := filepath.Join(filepath.Dir(cfg), orgSelectionFile)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, map[string]string{}, nil
	}
	if err != nil {
		return "", nil, err
	}
	var choices map[string]string
	if err := json.Unmarshal(raw, &choices); err != nil {
		return "", nil, fmt.Errorf("read project organizations: %w", err)
	}
	if choices == nil {
		choices = map[string]string{}
	}
	return path, choices, nil
}

func projectCloudEndpoint(env string, decl *cloud.Declaration) (cloud.Endpoint, error) {
	ep, err := cloud.ResolveEndpoint(env, decl)
	if err != nil {
		return ep, err
	}
	_, choices, err := readProjectOrganizations()
	if err != nil {
		return cloud.Endpoint{}, err
	}
	key, err := credentials.Normalize(ep.URL)
	if err != nil {
		return cloud.Endpoint{}, err
	}
	ep.OrgID = choices[key]
	return ep, nil
}

func logoutOrganization(flag string, ep cloud.Endpoint) string {
	if strings.TrimSpace(flag) != "" {
		return strings.TrimSpace(flag)
	}
	return ep.OrgID
}

func newOrgCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "org", Short: "Select the organization this project acts in"}
	var endpoint string
	use := &cobra.Command{
		Use: "use <org_id|default>", Short: "Pin this project's org locally (default clears the project choice)",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			org := strings.TrimSpace(args[0])
			if org == "" {
				return errors.New("an organization id is required")
			}
			path, choices, err := readProjectOrganizations()
			if err != nil {
				return err
			}
			if path == "" {
				return errors.New("forge org use requires a forge project")
			}
			planes, err := loginTargets(cmd.Context(), endpoint, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if len(planes) != 1 {
				return errors.New("this project declares several endpoints; select one with --endpoint")
			}
			ep := planes[0].Endpoint
			key, err := credentials.Normalize(ep.URL)
			if err != nil {
				return err
			}
			if org == "default" {
				delete(choices, key)
			} else {
				ep.OrgID = org
				cred, err := cloud.ResolveCredential("", ep)
				if err != nil {
					return err
				}
				actual, err := cloud.ResolveOrganization(cmd.Context(), cloud.NewClient(ep, cred))
				if err != nil {
					return err
				}
				if actual != org {
					return fmt.Errorf("credential belongs to organization %q, not %q", actual, org)
				}
				choices[key] = org
			}
			raw, err := json.MarshalIndent(choices, "", "  ")
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			ignorePath := filepath.Join(filepath.Dir(path), ".gitignore")
			ignored, err := os.ReadFile(ignorePath)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if !strings.Contains("\n"+string(ignored), "\n/organizations.json\n") {
				ignore, err := os.OpenFile(ignorePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
				if err != nil {
					return err
				}
				if _, err := ignore.WriteString("\n/organizations.json\n/.organizations-*\n"); err != nil {
					ignore.Close()
					return err
				}
				if err := ignore.Close(); err != nil {
					return err
				}
			}
			tmp, err := os.CreateTemp(filepath.Dir(path), ".organizations-*")
			if err != nil {
				return err
			}
			defer os.Remove(tmp.Name())
			if _, err := tmp.Write(append(raw, '\n')); err != nil {
				tmp.Close()
				return err
			}
			if err := tmp.Close(); err != nil {
				return err
			}
			if err := os.Rename(tmp.Name(), path); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Project organization for %s: %s\n", key, org)
			return nil
		},
	}
	use.Flags().StringVar(&endpoint, "endpoint", "", "Select the control-plane endpoint when the project declares several")
	cmd.AddCommand(use)
	return cmdutil.StrictGroup(cmd)
}
