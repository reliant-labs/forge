package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/deploytarget"
)

// `forge mcp app <env>` (docs/proposals/mcp-from-rpcs.md §7): the URL of an
// env's /mcp endpoint, the tools it serves and a ready snippet for each MCP
// client. The served set is codegen.MCPTools over gen/forge_descriptor, the
// same answer the generated tool table and the lint lane read, so this never
// lists a tool the app does not serve.
//
// Auth is a bearer token for now: the app's /mcp runs each tool call through
// the same interceptor chain as its API, so whatever token the API accepts
// works. OAuth (§8) is a later, additive slice.

// mcpAppSchemaVersion versions the --json document. A field is only ever
// added; a change a consumer would have to branch on bumps it.
const mcpAppSchemaVersion = 1

// mcpAppTokenEnv is the environment variable every snippet reads the token
// from, so no snippet carries a secret.
const mcpAppTokenEnv = "MCP_TOKEN"

// mcpAppAPI is an env's resolved API address.
type mcpAppAPI struct {
	Project string
	URL     string
	// Source says where URL came from: "flag", "host" (a host service
	// of a stack on this machine), "hosted" (Reliant hosting's status) or
	// "ingress" (the env's rendered gateway route).
	Source string
}

// mcpAppResolveAPI resolves an env's API address. A var so tests state the
// answer instead of rendering an env.
var mcpAppResolveAPI = resolveMCPAppAPI

// mcpAppDoc is the --json document.
type mcpAppDoc struct {
	jsonEnvelope
	SchemaVersion int    `json:"schema_version"`
	Env           string `json:"env"`
	// Name is the server name every snippet registers.
	Name string `json:"name"`
	// URL is the env's MCP endpoint, <api>/mcp; APIURL the API it is on.
	URL       string         `json:"url,omitempty"`
	APIURL    string         `json:"api_url,omitempty"`
	URLSource string         `json:"url_source,omitempty"`
	Auth      mcpAppAuth     `json:"auth"`
	Tools     []mcpAppTool   `json:"tools"`
	Clients   []mcpAppClient `json:"clients"`
	// Message is set when there is nothing to connect: no rpc opts in.
	Message string `json:"message,omitempty"`
}

// mcpAppAuth is how a client authenticates to /mcp.
type mcpAppAuth struct {
	// Kind is "bearer": a token in the Authorization header.
	Kind     string `json:"kind"`
	Header   string `json:"header"`
	TokenEnv string `json:"token_env"`
	Hint     string `json:"hint"`
}

// mcpAppTool is one served tool.
type mcpAppTool struct {
	Name        string `json:"name"`
	Procedure   string `json:"procedure"`
	Description string `json:"description,omitempty"`
	ReadOnly    bool   `json:"read_only"`
	Destructive bool   `json:"destructive"`
	Idempotent  bool   `json:"idempotent"`
	Public      bool   `json:"public"`
}

// mcpAppClient is one client's setup: a command to run, or a config file to
// merge the snippet into.
type mcpAppClient struct {
	Client string `json:"client"` // claude-code, codex, cursor, vscode
	Kind   string `json:"kind"`   // "command" | "file"
	File   string `json:"file,omitempty"`
	Format string `json:"format,omitempty"` // json | toml, for a file
	Text   string `json:"text"`
}

const mcpAppTokenHint = "a bearer token the app's API accepts (the same token a client of its RPCs sends; `forge skill load auth` covers how this app issues them); " +
	"tool calls run as that token's caller, through the API's own auth, validation and audit. Export it as " + mcpAppTokenEnv + " before starting the client"

func newMCPAppCmd() *cobra.Command {
	var jsonOut bool
	var urlFlag, nameFlag string
	cmd := &cobra.Command{
		Use:   "app <env>",
		Short: "Print the env's /mcp URL, the tools it serves and a config snippet for each MCP client",
		Long: `Print how to connect an MCP client to this app's own tools in <env>.

The app serves the rpcs it opts in to ((forge.v1.method).mcp, or the
service's (forge.v1.service).mcp; a mutating rpc also needs mcp_mutating) as
MCP tools at <api>/mcp. This reads the served set from gen/forge_descriptor
(run 'forge generate' after changing an opt-in), resolves the env's API URL
the way 'forge env status' does, and prints:

  - the endpoint URL;
  - every tool, marked (read-only) or (destructive);
  - a snippet for Claude Code, Codex, Cursor and VS Code.

Auth is a bearer token the API accepts, read from $` + mcpAppTokenEnv + ` by every snippet.
--url names the API yourself when the env's cannot be resolved (or to point at
another host). --json prints the same as one document (schema_version 1).

An app that serves no tools prints why and exits 0.

'forge skill load api-mcp' covers opting in and writing tools a model can use.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMCPApp(cmd.Context(), cmd.OutOrStdout(), projectDirForKCL(), args[0], mcpAppOptions{JSON: jsonOut, URL: urlFlag, Name: nameFlag})
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the endpoint, tools and snippets as one JSON document")
	cmd.Flags().StringVar(&urlFlag, "url", "", "the API's base URL, instead of resolving the env's (e.g. https://api.example.com)")
	cmd.Flags().StringVar(&nameFlag, "name", "", "the server name the snippets register (default: <project>-<env>)")
	return cmd
}

type mcpAppOptions struct {
	JSON bool
	URL  string
	Name string
}

func runMCPApp(ctx context.Context, w io.Writer, projectDir, env string, o mcpAppOptions) (err error) {
	doc := mcpAppDoc{SchemaVersion: mcpAppSchemaVersion, Env: env, Tools: []mcpAppTool{}, Clients: []mcpAppClient{},
		Auth: mcpAppAuth{Kind: "bearer", Header: "Authorization", TokenEnv: mcpAppTokenEnv, Hint: mcpAppTokenHint}}
	if o.JSON {
		defer func() {
			doc.stamp(err)
			if werr := writeJSONDocument(w, doc); werr != nil && err == nil {
				err = werr
			}
		}()
	}

	services, err := codegen.ParseDeclaredServices(projectDir)
	if err != nil {
		return fmt.Errorf("read gen/forge_descriptor: %w (run `forge generate`)", err)
	}
	tools, err := codegen.MCPTools(services)
	if err != nil {
		return fmt.Errorf("%w\n(`forge generate` refuses these too; `forge lint --conventions` names each on its line)", err)
	}
	for _, t := range tools {
		doc.Tools = append(doc.Tools, mcpAppTool{
			Name: t.Name, Procedure: "/" + t.Service.Package + "." + t.Service.Name + "/" + t.Method.Name,
			Description: t.Description, ReadOnly: t.ReadOnly, Destructive: t.Destructive,
			Idempotent: t.Idempotent, Public: t.Public,
		})
	}
	if len(tools) == 0 {
		doc.Message = "this app serves no MCP tools: no rpc opts in. Add `option (forge.v1.method) = { mcp: true };` to an rpc " +
			"(or `mcp: true` on its service's (forge.v1.service) option), run `forge generate`, then run this again (forge skill load api-mcp)"
		if !o.JSON {
			_, _ = fmt.Fprintln(w, doc.Message)
		}
		return nil
	}

	api := mcpAppAPI{URL: o.URL, Source: "flag"}
	if api.URL == "" {
		api, err = mcpAppResolveAPI(ctx, env)
		if err != nil {
			return err
		}
	}
	base, err := url.Parse(strings.TrimRight(api.URL, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return fmt.Errorf("API URL %q is not an absolute http(s) URL; pass --url", api.URL)
	}
	doc.APIURL, doc.URLSource = base.String(), api.Source
	doc.URL = base.JoinPath("mcp").String()
	doc.Name = o.Name
	if doc.Name == "" {
		doc.Name = mcpServerName(api.Project, env)
	}
	doc.Clients = mcpAppClients(doc.Name, doc.URL)

	if !o.JSON {
		renderMCPApp(w, doc)
	}
	return nil
}

var reNotServerName = regexp.MustCompile(`[^a-z0-9_-]+`)

// mcpServerName is the default server name: <project>-<env>, in the
// characters every client accepts.
func mcpServerName(project, env string) string {
	n := strings.Trim(reNotServerName.ReplaceAllString(strings.ToLower(project+"-"+env), "-"), "-")
	if project == "" {
		n = strings.Trim(reNotServerName.ReplaceAllString(strings.ToLower("app-"+env), "-"), "-")
	}
	return n
}

// mcpAppClients is the snippet for each client.
func mcpAppClients(name, endpoint string) []mcpAppClient {
	mustJSON := func(v any) string {
		b, _ := json.MarshalIndent(v, "", "  ")
		return string(b)
	}
	return []mcpAppClient{
		{Client: "claude-code", Kind: "command",
			Text: fmt.Sprintf(`claude mcp add --transport http %s %s --header "Authorization: Bearer $%s"`, name, endpoint, mcpAppTokenEnv)},
		{Client: "codex", Kind: "file", File: "~/.codex/config.toml", Format: "toml",
			Text: fmt.Sprintf("[mcp_servers.%s]\nurl = %q\nbearer_token_env_var = %q", strings.ReplaceAll(name, "-", "_"), endpoint, mcpAppTokenEnv)},
		{Client: "cursor", Kind: "file", File: ".cursor/mcp.json", Format: "json",
			Text: mustJSON(map[string]any{"mcpServers": map[string]any{name: map[string]any{
				"url": endpoint, "headers": map[string]string{"Authorization": "Bearer ${env:" + mcpAppTokenEnv + "}"},
			}}})},
		{Client: "vscode", Kind: "file", File: ".vscode/mcp.json", Format: "json",
			Text: mustJSON(map[string]any{"servers": map[string]any{name: map[string]any{
				"type": "http", "url": endpoint, "headers": map[string]string{"Authorization": "Bearer ${env:" + mcpAppTokenEnv + "}"},
			}}})},
	}
}

func renderMCPApp(w io.Writer, doc mcpAppDoc) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("MCP endpoint (env %s): %s\n", doc.Env, doc.URL)
	p("  API %s (from %s)\n\n", doc.APIURL, doc.URLSource)
	p("Tools (%d):\n", len(doc.Tools))
	for _, t := range doc.Tools {
		var marks []string
		if t.ReadOnly {
			marks = append(marks, "read-only")
		}
		if t.Destructive {
			marks = append(marks, "destructive")
		}
		if t.Public {
			marks = append(marks, "public")
		}
		m := ""
		if len(marks) > 0 {
			m = " (" + strings.Join(marks, ", ") + ")"
		}
		p("  %s%s\n", t.Name, m)
	}
	p("\nAuth: %s.\n", doc.Auth.Hint)
	for _, c := range doc.Clients {
		switch c.Kind {
		case "command":
			p("\n%s:\n  %s\n", c.Client, c.Text)
		default:
			p("\n%s (merge into %s):\n%s\n", c.Client, c.File, indent(c.Text, "  "))
		}
	}
}

// resolveMCPAppAPI finds the env's API address the way `forge env status`
// does: render the env; for a stack on this machine, the first host
// service with a URL; otherwise Reliant hosting's status for a hosted
// workload, then the env's rendered gateway routes.
func resolveMCPAppAPI(ctx context.Context, env string) (mcpAppAPI, error) {
	read, err := readRuntimeStatus(ctx, env)
	if err != nil {
		return mcpAppAPI{}, err
	}
	api := mcpAppAPI{Project: read.projectName}
	if !read.remote {
		for _, r := range read.rows {
			if r.Kind == "host" && r.URL != "" {
				api.URL, api.Source = r.URL, "host"
				return api, nil
			}
		}
	} else if read.entities != nil {
		hosted := false
		for _, w := range read.entities.Workloads {
			hosted = hosted || w.OnRuntime(RuntimeHosted)
		}
		if hosted {
			if st, herr := readHostedStatusFromDeclaration(ctx, env, read.entities); herr == nil {
				for _, w := range st.Workloads {
					if deploytarget.HostedTier(w.Tier) == deploytarget.HostedTierWorkload && w.URL != "" {
						api.URL, api.Source = w.URL, "hosted"
						return api, nil
					}
				}
			} else if !errors.Is(herr, deploytarget.ErrHostedEnvironmentNotFound) {
				return api, fmt.Errorf("read env %s's status from Reliant hosting: %w (or pass --url)", env, herr)
			}
		}
		for _, u := range buildIngressURLs(read.entities) {
			if u.Kind == "HTTPRoute" && u.URL != "" {
				api.URL, api.Source = u.URL, "ingress"
				return api, nil
			}
		}
	}
	return api, fmt.Errorf("could not resolve env %s's API URL: no host service with a port, hosted workload with a URL or gateway route "+
		"(start it with `forge env up %s` or deploy it, or pass --url)", env, env)
}
