// Package cli — `forge api` command surface.
//
// `forge api curl <service.method>` prints a copy-pasteable curl invocation
// for a Connect RPC endpoint. Connect handlers already speak plain HTTP/1.1
// POST with application/json — no gRPC tooling needed — but the URL shape
// and Content-Type rules are undocumented in most projects. This command
// removes the discovery friction: read the service port from forge.yaml,
// look up the method's input message in forge_descriptor.json, and emit a
// curl command with a request-body skeleton populated from zero values for
// each field.
//
// Streaming RPCs are flagged but still printed — the body shape is the same;
// only the Content-Type changes to application/connect+json. We surface the
// difference rather than reject the command so users can hit streaming
// endpoints too (curl will get the first frame; this is enough to verify
// reachability and auth).
package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
)

// newAPICmd is the parent for `forge api ...` verbs. Today the only verb is
// `curl`; future verbs (e.g. `forge api list`, `forge api schema`) hang off
// the same parent so the namespace stays cohesive.
func newAPICmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "api",
		Short: "Inspect and exercise Connect RPC endpoints over plain HTTP+JSON",
		Long: `Connect handlers accept plain HTTP/1.1 POST requests with
Content-Type: application/json — no gRPC tooling required.

Sub-commands surface that capability for ad-hoc debugging from the shell.`,
	}
	cmd.AddCommand(newAPICurlCmd())
	return cmd
}

// newAPICurlCmd implements `forge api curl <service.method>`. The command is
// read-only and side-effect free: it never executes the curl, only prints it.
func newAPICurlCmd() *cobra.Command {
	var (
		port int
		body string
		host string
	)
	cmd := &cobra.Command{
		Use:   "curl <service.method>",
		Short: "Print a copy-pasteable curl command for a Connect RPC method",
		Long: `Print a curl invocation that exercises a Connect RPC endpoint over
plain HTTP+JSON. The URL is derived from the proto package + service + method
name; the request body is a zero-value skeleton populated from the method's
input message fields.

Arguments:
  <service.method>   Fully-qualified service and method, e.g. "users.v1.UserService.GetUser".
                     Short form is also accepted: "UserService.GetUser" matches the unique
                     service of that name across all proto packages.

Examples:
  forge api curl users.v1.UserService.GetUser
  forge api curl UserService.GetUser --port 9090
  forge api curl users.v1.UserService.CreateUser --body '{"name":"alice"}'

The command never executes — it only prints. Pipe to ` + "`sh`" + ` if you want to run it,
or paste into a debugger / Postman / HTTPie session.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectDir, err := findProjectRoot()
			if err != nil || projectDir == "" {
				return cliutil.UserErr("forge api curl",
					"could not find forge.yaml in current directory or any parent",
					"",
					"run from inside a forge project, or `cd` to one first")
			}
			out, err := buildCurlCommand(projectDir, args[0], curlOptions{
				port: port,
				body: body,
				host: host,
			})
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), out)
			return nil
		},
	}
	cmd.Flags().IntVar(&port, "port", 0, "Service port (default: from forge.yaml services[].port)")
	cmd.Flags().StringVar(&body, "body", "", "Request body JSON (default: zero-value skeleton from proto fields)")
	cmd.Flags().StringVar(&host, "host", "localhost", "Host name to embed in the URL")
	return cmd
}

// curlOptions captures the flag-tunable inputs to buildCurlCommand.
// Bundled as a struct so the signature stays stable as we add knobs
// (e.g. --auth-token, --tenant-header) without churn at every call site.
type curlOptions struct {
	port int
	body string
	host string
}

// buildCurlCommand is the pure function under the cobra command. It loads
// forge.yaml + forge_descriptor.json, resolves the target service+method,
// and renders a curl invocation as a single string.
//
// Returned errors are user-facing (via cliutil.UserErr): they identify the
// command, what failed, and a one-line fix. Internal errors (missing
// descriptor file, unreadable forge.yaml) are also surfaced through the
// same wrapper so the CLI boundary stays uniform.
func buildCurlCommand(projectDir, target string, opts curlOptions) (string, error) {
	desc, err := loadForgeDescriptor(projectDir)
	if err != nil {
		return "", cliutil.UserErr("forge api curl",
			fmt.Sprintf("could not read forge_descriptor.json: %v", err),
			"gen/forge_descriptor.json",
			"run `forge generate` to produce the proto descriptor, then retry")
	}
	if desc == nil || len(desc.Services) == 0 {
		return "", cliutil.UserErr("forge api curl",
			"no services found in forge_descriptor.json",
			"gen/forge_descriptor.json",
			"run `forge generate` after declaring at least one service in proto/")
	}

	svc, method, err := resolveServiceMethod(desc.Services, target)
	if err != nil {
		return "", err
	}

	port := opts.port
	if port == 0 {
		port = lookupServicePort(projectDir, svc)
	}
	if port == 0 {
		// Last-resort fallback matches `forge add service` default.
		// Surface the assumption so the user can override via --port.
		port = 8080
	}

	bodyJSON := opts.body
	if bodyJSON == "" {
		bodyJSON = buildZeroBody(svc, method)
	}

	host := opts.host
	if host == "" {
		host = "localhost"
	}

	url := fmt.Sprintf("http://%s:%d/%s.%s/%s", host, port, svc.Package, svc.Name, method.Name)

	contentType := "application/json"
	streamingNote := ""
	if method.ClientStreaming || method.ServerStreaming {
		contentType = "application/connect+json"
		streamingNote = "\n# Note: this RPC is streaming — curl will only send/receive the first frame."
	}

	// Single-line-per-segment, copy-pasteable. Keep the body inline (the
	// skeleton is small) so users can edit it in place before sending.
	curl := fmt.Sprintf(`curl -X POST \
  -H 'Content-Type: %s' \
  -d %s \
  %s`,
		contentType,
		shellQuoteSingle(bodyJSON),
		url,
	)
	if streamingNote != "" {
		curl += streamingNote
	}
	return curl, nil
}

// resolveServiceMethod accepts either "<pkg>.<Service>.<Method>" (fully
// qualified) or "<Service>.<Method>" (short form). The short form is
// rejected when more than one service across proto packages shares the
// name — the user must disambiguate by qualifying.
func resolveServiceMethod(services []codegen.ServiceDef, target string) (codegen.ServiceDef, codegen.Method, error) {
	parts := strings.Split(target, ".")
	if len(parts) < 2 {
		return codegen.ServiceDef{}, codegen.Method{}, cliutil.UserErr("forge api curl",
			fmt.Sprintf("invalid target %q (need at least Service.Method)", target),
			"",
			"call as `forge api curl <pkg>.<Service>.<Method>` or `forge api curl <Service>.<Method>`")
	}

	methodName := parts[len(parts)-1]
	serviceName := parts[len(parts)-2]
	// Anything before the service name is the proto package (may be empty).
	pkgPrefix := strings.Join(parts[:len(parts)-2], ".")

	var candidates []codegen.ServiceDef
	for _, s := range services {
		if s.Name != serviceName {
			continue
		}
		if pkgPrefix != "" && s.Package != pkgPrefix {
			continue
		}
		candidates = append(candidates, s)
	}

	if len(candidates) == 0 {
		return codegen.ServiceDef{}, codegen.Method{}, cliutil.UserErr("forge api curl",
			fmt.Sprintf("no service %q found in proto descriptors", serviceName),
			"",
			fmt.Sprintf("available services: %s", availableServicesHint(services)))
	}
	if len(candidates) > 1 {
		// Multiple services share the (unqualified) name — the user passed
		// the short form against an ambiguous catalog. Surface every
		// fully-qualified option so the next attempt is unambiguous.
		var qualified []string
		for _, s := range candidates {
			qualified = append(qualified, s.Package+"."+s.Name)
		}
		sort.Strings(qualified)
		return codegen.ServiceDef{}, codegen.Method{}, cliutil.UserErr("forge api curl",
			fmt.Sprintf("service name %q is ambiguous — declared in %d packages", serviceName, len(candidates)),
			"",
			fmt.Sprintf("qualify the package, e.g. `forge api curl %s.%s`", qualified[0], methodName))
	}

	svc := candidates[0]
	for _, m := range svc.Methods {
		if m.Name == methodName {
			return svc, m, nil
		}
	}

	var available []string
	for _, m := range svc.Methods {
		available = append(available, m.Name)
	}
	sort.Strings(available)
	if len(available) == 0 {
		return codegen.ServiceDef{}, codegen.Method{}, cliutil.UserErr("forge api curl",
			fmt.Sprintf("method %q not found on %s.%s (service has no methods)", methodName, svc.Package, svc.Name),
			"",
			"declare an rpc in the .proto file and run `forge generate`")
	}
	return codegen.ServiceDef{}, codegen.Method{}, cliutil.UserErr("forge api curl",
		fmt.Sprintf("method %q not found on %s.%s", methodName, svc.Package, svc.Name),
		"",
		fmt.Sprintf("available methods: %s", strings.Join(available, ", ")))
}

// availableServicesHint returns a short comma-separated list of qualified
// service names, used in the "not found" error to guide the next attempt.
// Truncates beyond a small threshold so the error stays readable.
func availableServicesHint(services []codegen.ServiceDef) string {
	const limit = 5
	names := make([]string, 0, len(services))
	for _, s := range services {
		names = append(names, s.Package+"."+s.Name)
	}
	sort.Strings(names)
	if len(names) <= limit {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:limit], ", ") + fmt.Sprintf(", … (%d more)", len(names)-limit)
}

// lookupServicePort scans forge.yaml's services[] for a service whose
// handler directory matches the proto package's service. Today this is a
// best-effort match by service name (handlers/<pkg> conventional layout);
// returns 0 when no match is found so the caller can fall back to the
// default port or surface a useful error.
//
// We intentionally don't error on load failure here — a partly-bootstrapped
// project may not have forge.yaml yet, in which case the default port +
// --port override are sufficient.
func lookupServicePort(projectDir string, svc codegen.ServiceDef) int {
	store, err := loadProjectStoreFrom(filepath.Join(projectDir, defaultProjectConfigFile))
	if err != nil {
		return 0
	}
	return matchServicePort(store.Config(), svc)
}

// matchServicePort picks the most likely services[] entry for svc. We try
// two heuristics in order:
//
//  1. Service name match: forge.yaml usually scaffolds with
//     name: <pkg-leaf> for the handler dir. We try several conventional
//     mappings: the proto package leaf, the lowercased service name with
//     the trailing "Service" suffix stripped, and the Go package name.
//  2. First go_service entry: when nothing matches, we pick the first
//     non-worker, non-operator service. This is the common case in
//     single-service projects and is safe because the user can always
//     override via --port.
//
// Returns 0 when no service entry has a Port set (rare — `forge add service`
// always assigns one). Callers fall back to the default port.
func matchServicePort(cfg *config.ProjectConfig, svc codegen.ServiceDef) int {
	if cfg == nil {
		return 0
	}

	candidates := serviceNameCandidates(svc)
	for _, name := range candidates {
		for _, s := range cfg.Components {
			if !s.IsServer() {
				continue
			}
			if strings.EqualFold(s.Name, name) {
				return s.PrimaryPort()
			}
		}
	}

	// Fallback: first server component.
	for _, s := range cfg.Components {
		if s.IsServer() {
			return s.PrimaryPort()
		}
	}
	return 0
}

// serviceNameCandidates derives plausible forge.yaml service.name values
// from a ServiceDef. The list is deduped + ordered most-specific first.
func serviceNameCandidates(svc codegen.ServiceDef) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}

	// "users.v1" -> "users"
	if pkg := svc.Package; pkg != "" {
		if idx := strings.LastIndex(pkg, "."); idx > 0 {
			add(pkg[:idx])
		} else {
			add(pkg)
		}
	}

	// "UserService" -> "user"; strip the conventional Service suffix.
	name := svc.Name
	name = strings.TrimSuffix(name, "Service")
	add(name)

	// "usersv1" -> "usersv1" (last-ditch full match)
	add(svc.PkgName)
	return out
}

// buildZeroBody renders a JSON object containing each field of the
// method's input message at its proto zero value. The result is small and
// always valid JSON — just enough to make the request parse server-side so
// the user can iterate on the values.
//
// We use the ProtoJSON convention: field names stay snake_case (Connect's
// codec accepts both snake_case and camelCase by default, but snake_case
// matches the proto definition, which is what users tend to recognise).
//
// Input messages we don't have field data for (e.g. google.protobuf.Empty,
// or cross-file references the descriptor didn't capture) render as `{}`
// — empty but valid JSON, which is what those methods accept.
func buildZeroBody(svc codegen.ServiceDef, method codegen.Method) string {
	if method.IsInputEmpty() {
		return "{}"
	}
	fields, ok := svc.Messages[method.InputType]
	if !ok || len(fields) == 0 {
		// Method input is a message we don't have field defs for — the
		// most common cause is a cross-file message reference the
		// descriptor didn't capture. Returning {} keeps the command
		// useful: the request will parse, the user just edits in their
		// own values.
		return "{}"
	}

	obj := make(map[string]any, len(fields))
	for _, f := range fields {
		obj[f.Name] = zeroValueFor(f.ProtoType)
	}

	// json.Marshal sorts keys alphabetically by default; we want to
	// preserve proto declaration order so the skeleton matches the proto
	// file's reading order. Manual render keeps the dependency surface
	// zero (no yaml/json3rd-party deps).
	return renderJSONInOrder(obj, fieldOrder(fields))
}

// fieldOrder returns the proto-declared order of field names for a message.
func fieldOrder(fields []codegen.MessageFieldDef) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, f.Name)
	}
	return out
}

// renderJSONInOrder renders obj as a JSON object iterating keys in `order`.
// We marshal each value with encoding/json so escaping is correct; we hand-
// concatenate the surrounding `{...}` because the stdlib sorts map keys.
func renderJSONInOrder(obj map[string]any, order []string) string {
	if len(order) == 0 {
		return "{}"
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range order {
		if i > 0 {
			b.WriteByte(',')
		}
		// Key — always a string.
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		vb, err := json.Marshal(obj[k])
		if err != nil {
			vb = []byte("null")
		}
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.String()
}

// zeroValueFor returns the proto zero value for a field type. We use Go
// types that json.Marshal will render correctly: "" for string, 0 for
// numeric, false for bool, nil for message/bytes/enum (rendered as null,
// which ProtoJSON accepts for those).
//
// For message / bytes / enum we deliberately emit null rather than a nested
// stub: a recursive walk would balloon the skeleton for deep message graphs
// and risks infinite loops on self-referential types. null is valid
// ProtoJSON for any nullable field and a clear "fill me in" signal.
func zeroValueFor(protoType string) any {
	switch protoType {
	case "bool":
		return false
	case "string":
		return ""
	case "int32", "int64", "uint32", "uint64", "sint32", "sint64",
		"fixed32", "fixed64", "sfixed32", "sfixed64":
		return 0
	case "float", "double":
		return 0.0
	default:
		// message, enum, bytes, map, anything we don't recognise.
		return nil
	}
}

// shellQuoteSingle wraps a string in single quotes for shell embedding,
// escaping any embedded single quotes. We use single quotes for the curl
// -d argument so $-expansion doesn't fire on the JSON body.
//
// Escape sequence inside single quotes: close the quote (`'`), emit an
// escaped quote (`\'`), reopen the quote (`'`). The shell concatenates
// adjacent quoted strings.
func shellQuoteSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
