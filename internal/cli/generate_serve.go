// Types-only services (forge.yaml `services[].serve: false`).
//
// Field evidence (cp-forge): a service's canonical implementation lives
// in a sibling binary outside the repo. The project still needs the
// generated proto types, Connect client, and frontend hooks (its own
// handlers proxy to the sibling), but it must NOT serve the API itself.
// Pre-`serve:`, forge's model offered only implement-or-stub, so such
// projects carried permanent CodeUnimplemented stubs plus a doc.go
// apologizing for them — a fork by another name.
//
// The contract for `serve: false`:
//
//	STILL GENERATED (callers need them):
//	  - gen/ proto types + Connect client stubs (buf step is untouched)
//	  - frontend hooks / pages / TS mocks
//	  - forge_descriptor.json entries
//
//	GATED OFF (this binary doesn't serve them):
//	  - handlers/<svc>/ scaffold (handlers_gen, stubs, authorizer_gen,
//	    CRUD handlers, webhook routes, handlers/mocks/<svc>_mock.go)
//	  - the service's row in the bootstrap appkit table (no Construct /
//	    Register / wire deps / diagnostics)
//	  - middleware/authz registration derived from the service's RPCs
//	  - the service's tools in gen/mcp/manifest.json
//
// The single predicate is config.(*ProjectConfig).ServiceServed — this
// file only provides the pipeline-shaped views over it. Retirement of a
// pre-existing handlers/<svc>/ scaffold is handled by the existing
// manifest-driven stale sweep (generate_cleanup.go): the gated emitters
// stop writing the tracked Tier-1 files, so they fall out of
// WrittenThisRun and become report-only removal candidates (deleted
// under --force-cleanup); Tier-2 user-owned files in the same dir are
// never candidates. `forge audit` surfaces the dir via the codegen
// category's unserved_handler_dirs finding.
package cli

import (
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/naming"
)

// servedServiceDefs filters proto-parsed services down to the entries
// forge.yaml marks served. The second return value lists the filtered-
// out (types-only) defs so callers can print per-service skip lines.
//
// ServiceDef.Name carries the proto spelling ("AdminServerService");
// forge.yaml carries the CLI spelling ("admin-server") — both normalize
// through naming.ServicePackage inside cfg.ServiceServed, so the match
// is spelling-agnostic. A nil cfg (directory-scan fallback) serves
// everything, matching pre-`serve:` behavior.
func servedServiceDefs(cfg *config.ProjectConfig, services []codegen.ServiceDef) (served, unserved []codegen.ServiceDef) {
	if cfg == nil {
		return services, nil
	}
	for _, svc := range services {
		if cfg.ServiceServed(svc.Name) {
			served = append(served, svc)
		} else {
			unserved = append(unserved, svc)
		}
	}
	return served, unserved
}

// servedServices is the pipeline-context view over servedServiceDefs:
// ctx.Services filtered to forge.yaml-served entries. Steps whose output
// exists only on a serving binary (handlers scaffold, CRUD, authorizer,
// service mocks, auth middleware skip-lists, bootstrap/wire/diagnostics,
// MCP manifest) consume this instead of ctx.Services; steps that emit
// caller-side artifacts (frontend hooks/pages/mocks, descriptor) keep
// reading ctx.Services unfiltered.
func (ctx *pipelineContext) servedServices() []codegen.ServiceDef {
	served, _ := servedServiceDefs(ctx.Cfg, ctx.Services)
	return served
}

// unservedHandlerDirSkips returns the handlers/<dir> leaves (snake
// package form, e.g. "admin_server") of every `serve: false` service.
// GenerateAuthorizer's orphan-dir sweep consults this so a retired-but-
// still-present handler dir doesn't get its authorizer_gen.go re-emitted
// (which would re-add the path to WrittenThisRun and hide it from the
// stale sweep forever).
func unservedHandlerDirSkips(cfg *config.ProjectConfig) map[string]bool {
	if cfg == nil {
		return nil
	}
	var out map[string]bool
	for _, s := range cfg.UnservedServices() {
		if out == nil {
			out = map[string]bool{}
		}
		out[naming.ServicePackage(s.Name)] = true
	}
	return out
}

// unservedBootstrapGuards projects the `serve: false` services into the
// shape the bootstrap template's BootstrapOnly name-guard consumes:
// runtime kebab names (what `./<bin> server <name>` / cobra subcommands
// pass) plus the served_by documentation string. Passing one of these
// names to the name filter is a misconfiguration the generated code now
// rejects with a pointed error instead of appkit's generic unknown-name
// warning.
func unservedBootstrapGuards(cfg *config.ProjectConfig) []codegen.UnservedServiceData {
	if cfg == nil {
		return nil
	}
	var out []codegen.UnservedServiceData
	for _, s := range cfg.UnservedServices() {
		out = append(out, codegen.UnservedServiceData{
			Name:     naming.ToKebabCase(s.Name),
			ServedBy: s.ServedBy,
		})
	}
	return out
}
