// Package cli — `forge audit` command.
//
// Audit produces a comprehensive snapshot of project state designed to
// orient an LLM (or human) without forcing them to grep ten different
// directories. It rolls up:
//
//   - Forge version pin (forge.yaml forge_version vs binary buildinfo).
//   - Project shape (kind, services + RPC counts, workers, operators,
//     frontends, installed packs).
//   - Convention compliance (rolled up forge lint counts per category).
//   - Codegen state (certified-file census via the embedded forge:hash markers,
//     orphan _gen files, uncommitted user edits to forge-space files).
//   - Pack health (each installed pack's version against the embedded
//     pack registry).
//   - Pack graph health (every installed pack's `depends_on` is also
//     installed; missing producers surface as errors).
//   - Proto-vs-migration alignment (entity tables vs db/migrations/).
//   - Migration safety summary (allowed_destructive count, latest
//     migration timestamp, destructive_change severity).
//   - Wire-coverage (unresolved Deps fields in pkg/app/wire_gen.go,
//     rolled up from `forge lint --wire-coverage`).
//   - FORGE_SCAFFOLD marker counts (P0 sharpening surface).
//   - Deps health (go.sum freshness vs go.mod, gen/ presence).
//
// JSON output groups checks by category with status: ok|warn|error so a
// sub-agent can branch on `.codegen.status == "warn"` directly.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/linter/forgeconv"
	"github.com/reliant-labs/forge/internal/packs"
)

// AuditStatus is the per-category roll-up. We keep the wire enum tiny so
// the JSON shape is easy to grep / jq against.
type AuditStatus string

// AuditStatus enum values.
const (
	AuditStatusOK    AuditStatus = "ok"
	AuditStatusWarn  AuditStatus = "warn"
	AuditStatusError AuditStatus = "error"
)

// AuditCategory is one section of the audit report. The shape mirrors the
// "category, status, summary, details" scheme called for in the spec —
// kept deliberately simple so a sub-agent can pluck `.summary` for a
// human-readable snippet or `.details` for structured fix-up data.
type AuditCategory struct {
	Status  AuditStatus    `json:"status"`
	Summary string         `json:"summary"`
	Details map[string]any `json:"details,omitempty"`
}

// AuditReport is the top-level JSON structure emitted by `forge audit --json`.
// Field order is stable so diffing two audits is human-readable.
type AuditReport struct {
	ProjectName   string                   `json:"project_name"`
	ProjectKind   string                   `json:"project_kind"`
	BinaryVersion string                   `json:"binary_version"`
	GeneratedAt   time.Time                `json:"generated_at"`
	Categories    map[string]AuditCategory `json:"categories"`
	OverallStatus AuditStatus              `json:"overall_status"`
}

// auditCategoryOrder pins the print-order so human output stays stable
// regardless of map iteration. Categories not in this list fall back to
// alphabetical at the end.
var auditCategoryOrder = []string{
	"version",
	"shape",
	"features",
	"ingress",
	"environments",
	"external_builds",
	"conventions",
	"codegen",
	"packs",
	"pack_graph",
	"migration_safety",
	"wire_coverage",
	"optional_deps_guard",
	"config_deps",
	"scaffold_markers",
	"crud_stubs",
	"diagnostics",
	"deps",
	"friction",
}

func newAuditCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Print a comprehensive project state snapshot",
		Long: `Print a comprehensive snapshot of forge project state.

Audit reports forge version pin, project shape, lint roll-ups, codegen
state, pack health, proto vs migration alignment, scaffold markers, and
dep health. Use --json for machine-readable output (sub-agents).

Examples:
  forge audit            # human-readable
  forge audit --json     # machine-readable`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAudit(jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

func runAudit(jsonOut bool) error {
	report, err := buildAuditReport(".")
	if err != nil {
		return err
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	printAuditReport(os.Stdout, report)
	return nil
}

// buildAuditReport collects every category's data and rolls up the
// overall status. Errors in individual category collectors are folded
// into a "warn" status for that category — we never bail the whole audit
// because a single grep failed; partial information beats nothing.
func buildAuditReport(projectDir string) (*AuditReport, error) {
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return nil, fmt.Errorf("resolve project dir: %w", err)
	}

	store, cfgErr := loadProjectStoreFrom(filepath.Join(abs, defaultProjectConfigFile))
	if cfgErr != nil && !errors.Is(cfgErr, ErrProjectConfigNotFound) {
		return nil, fmt.Errorf("load project config: %w", cfgErr)
	}
	var cfg *config.ProjectConfig
	if store != nil {
		cfg = store.Config()
	}

	report := &AuditReport{
		BinaryVersion: buildinfo.Version(),
		GeneratedAt:   time.Now().UTC(),
		Categories:    make(map[string]AuditCategory),
	}
	if cfg != nil {
		report.ProjectName = cfg.Name
		report.ProjectKind = cfg.EffectiveKind()
	} else {
		report.ProjectName = filepath.Base(abs)
		report.ProjectKind = "unknown"
	}

	report.Categories["version"] = auditVersion(cfg)
	report.Categories["shape"] = auditShape(cfg, abs)
	report.Categories["features"] = auditFeatures(cfg)
	if cfg != nil && cfg.Features.IngressEnabled() {
		report.Categories["ingress"] = auditIngress(cfg, abs)
	}
	report.Categories["environments"] = auditEnvironments(abs)
	report.Categories["external_builds"] = auditExternalBuilds(cfg, abs)
	report.Categories["conventions"] = auditConventions(cfg, abs)
	report.Categories["codegen"] = auditCodegen(cfg, abs)
	report.Categories["packs"] = auditPacks(cfg)
	report.Categories["pack_graph"] = auditPackGraph(cfg)
	report.Categories["migration_safety"] = auditMigrationSafety(cfg, abs)
	report.Categories["wire_coverage"] = auditWireCoverage(abs)
	report.Categories["optional_deps_guard"] = auditOptionalDepsGuard(abs)
	report.Categories["config_deps"] = auditConfigDeps(abs)
	report.Categories["scaffold_markers"] = auditScaffoldMarkers(abs)
	report.Categories["crud_stubs"] = auditCRUDStubs(abs)
	report.Categories["diagnostics"] = auditDiagnostics(cfg, abs)
	report.Categories["deps"] = auditDeps(abs)
	report.Categories["friction"] = auditFriction(abs)

	report.OverallStatus = rollupStatus(report.Categories)
	return report, nil
}

// rollupStatus collapses per-category statuses into one overall verdict.
// "error" beats "warn" beats "ok", same precedence forge doctor uses.
func rollupStatus(cats map[string]AuditCategory) AuditStatus {
	worst := AuditStatusOK
	for _, c := range cats {
		switch c.Status {
		case AuditStatusError:
			return AuditStatusError
		case AuditStatusWarn:
			worst = AuditStatusWarn
		}
	}
	return worst
}

// auditVersion compares forge.yaml's pinned forge_version against the
// running binary. Mismatches surface as warnings (not errors) because
// running newer is usually fine — `forge upgrade` fixes it.
func auditVersion(cfg *config.ProjectConfig) AuditCategory {
	binv := buildinfo.Version()
	if cfg == nil {
		return AuditCategory{
			Status:  AuditStatusError,
			Summary: "no forge.yaml found — not a forge project",
			Details: map[string]any{"binary_version": binv},
		}
	}
	pinned := cfg.EffectiveForgeVersion()
	details := map[string]any{
		"pinned_version": pinned,
		"binary_version": binv,
	}
	if warning := forgeVersionMismatchWarning(cfg.ForgeVersion, binv); warning != "" {
		details["hint"] = fmt.Sprintf("run `%s upgrade` to align", Name())
		return AuditCategory{
			Status:  AuditStatusWarn,
			Summary: warning,
			Details: details,
		}
	}
	return AuditCategory{
		Status:  AuditStatusOK,
		Summary: fmt.Sprintf("forge_version %s matches binary", pinned),
		Details: details,
	}
}

// auditShape inventories the project's structural elements: services
// (and their RPC counts), workers, operators, frontends, packs.
func auditShape(cfg *config.ProjectConfig, projectDir string) AuditCategory {
	if cfg == nil {
		return AuditCategory{Status: AuditStatusError, Summary: "no forge.yaml"}
	}
	// rpcInfo is the per-RPC entry under svcInfo.RPCs. Additive
	// (introduced after rpc_count) — consumers that only read
	// rpc_count keep working. mcp_callable tells an agent up front
	// whether the RPC is reachable through the forge-mcp bridge:
	// streaming RPCs are in the MCP manifest (marked) but excluded
	// from MCP tools/list because MCP tool calls are unary.
	type rpcInfo struct {
		Name string `json:"name"`
		// Streaming is "", "client", "server", or "bidi" — same
		// vocabulary as the MCP manifest; omitted for unary RPCs.
		Streaming   string `json:"streaming,omitempty"`
		MCPCallable bool   `json:"mcp_callable"`
		// Served is the additive types-only marker: present (and false)
		// ONLY when the owning service has no serviceRow in the
		// user-owned pkg/app/services.go — the RPC's types/client still
		// generate but this binary does not serve it (and it is excluded
		// from the MCP manifest, hence MCPCallable=false too). Absent
		// means served — the additive-extension contract keeps rpc-count
		// consumers and older readers untouched.
		Served *bool `json:"served,omitempty"`
	}
	type svcInfo struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		RPCCount int    `json:"rpc_count"`
		// Served reports whether THIS binary registers the service —
		// derived from the user-owned pkg/app/services.go row list (a
		// missing registration file means everything is served, the
		// pre-registration behavior). Always present so consumers can
		// filter the inventory without re-deriving the default. Workers
		// and operators are always served:true (the registration file
		// governs Connect services only).
		Served bool `json:"served"`
		// RPCs lists each RPC by name with streaming/MCP-callability
		// info. Empty when proto parsing was unavailable (see
		// proto_integrity) — additive field, may be absent.
		RPCs []rpcInfo `json:"rpcs,omitempty"`
	}
	var services, workers, crons, operators, binaries []svcInfo
	var frontends []map[string]string

	// Parse RPC counts when proto/services exists. We still emit the
	// structural shape from forge.yaml so the user gets the inventory
	// even when codegen is broken — but unlike the historic silent-drop
	// behavior, we surface the parse failure as proto_integrity on the
	// details map so JSON consumers can `jq '.details.proto_integrity'`
	// and detect the "RPC count = 0 because parse failed, not because
	// the service has no methods" case. See B5 in the audit quick-win
	// backlog.
	rpcByService := map[string][]rpcInfo{}
	var protoParseErr string
	if dirExists(filepath.Join(projectDir, "proto", "services")) {
		if defs, err := codegen.ParseServicesFromProtos(filepath.Join(projectDir, "proto", "services"), projectDir); err == nil {
			for _, d := range defs {
				rpcs := make([]rpcInfo, 0, len(d.Methods))
				for _, m := range d.Methods {
					// Same vocabulary as the MCP manifest's "streaming"
					// field; "" means unary. Unary RPCs are the only
					// ones the forge-mcp bridge can dispatch (MCP tool
					// calls are unary), hence mcp_callable.
					mode := ""
					switch {
					case m.ClientStreaming && m.ServerStreaming:
						mode = "bidi"
					case m.ServerStreaming:
						mode = "server"
					case m.ClientStreaming:
						mode = "client"
					}
					rpcs = append(rpcs, rpcInfo{
						Name:        m.Name,
						Streaming:   mode,
						MCPCallable: mode == "",
					})
				}
				rpcByService[d.Name] = rpcs
			}
		} else {
			protoParseErr = err.Error()
		}
	}

	// Registration view over the user-owned pkg/app/services.go. A parse
	// failure falls open (everything served) — audit must not die on a
	// broken tree; the generate pipeline is the fail-loud gate.
	reg, regErr := loadServiceRegistry(projectDir)
	if regErr != nil {
		reg = &serviceRegistry{Exists: false}
	}

	for _, s := range cfg.Components {
		served := !isConnectServiceConfig(s) || reg.registered(s.Name)
		info := svcInfo{Name: s.Name, Type: s.EffectiveKind(), Served: served}
		// match by ProtoService name suffix (Echo → EchoService)
		for protoName, rpcs := range rpcByService {
			short := strings.TrimSuffix(protoName, "Service")
			if strings.EqualFold(short, s.Name) || strings.EqualFold(protoName, s.Name) {
				info.RPCCount = len(rpcs)
				info.RPCs = rpcs
				break
			}
		}
		// Unregistered services keep their RPC inventory discoverable
		// but carry the additive served:false marker on every entry —
		// the surface stays visible without claiming this binary serves
		// it. MCPCallable flips false because the RPCs are deliberately
		// excluded from gen/mcp/manifest.json.
		if !info.Served && len(info.RPCs) > 0 {
			notServed := false
			// Copy before mutating — info.RPCs aliases the shared
			// rpcByService slice and another cfg entry could match the
			// same proto service.
			rpcs := make([]rpcInfo, len(info.RPCs))
			copy(rpcs, info.RPCs)
			for i := range rpcs {
				rpcs[i].Served = &notServed
				rpcs[i].MCPCallable = false
			}
			info.RPCs = rpcs
		}
		switch s.EffectiveKind() {
		case config.ComponentKindWorker:
			workers = append(workers, info)
		case config.ComponentKindCron:
			crons = append(crons, info)
		case config.ComponentKindOperator:
			operators = append(operators, info)
		case config.ComponentKindBinary:
			binaries = append(binaries, info)
		default:
			services = append(services, info)
		}
	}
	for _, fe := range cfg.Frontends {
		frontends = append(frontends, map[string]string{"name": fe.Name, "type": fe.Type})
	}

	details := map[string]any{
		"services":  services,
		"workers":   workers,
		"crons":     crons,
		"operators": operators,
		"binaries":  binaries,
		"frontends": frontends,
		"packs":     cfg.Packs,
		"packages":  packageNames(cfg.Packages),
	}
	// proto_integrity surfaces the RPC-count parse failure (if any) so
	// JSON consumers don't have to guess whether RPCCount: 0 means
	// "no methods" or "parse failed". Only emitted when there's an
	// error to report — under the additive-extension contract, the
	// field being absent IS the "all good" signal.
	status := AuditStatusOK
	summary := fmt.Sprintf("kind=%s, %d server(s), %d worker(s), %d cron(s), %d operator(s), %d binary(ies), %d frontend(s), %d pack(s)",
		cfg.EffectiveKind(), len(services), len(workers), len(crons), len(operators), len(binaries), len(frontends), len(cfg.Packs))
	if protoParseErr != "" {
		details["proto_integrity"] = map[string]any{
			"status": "warn",
			"reason": "ParseServicesFromProtos failed — RPC counts may be 0 even for services with methods",
			"error":  protoParseErr,
		}
		status = AuditStatusWarn
		summary += " (warn: proto parse failed — see proto_integrity)"
	}
	return AuditCategory{Status: status, Summary: summary, Details: details}
}

// auditFeatures surfaces the resolved `features:` block from forge.yaml
// at audit time. The category lists every feature gated by config —
// deploy/build/frontend/packs/ci/docs/observability/... — and
// whether it resolves to enabled (default for nil) or disabled
// (explicit false). The additive-extension contract holds: new
// features added to config.FeaturesConfig.EffectiveFeatures() show up
// here automatically, sub-agents can branch on
// `.features.details.<name>` directly.
//
// Status is always ok — this category is informational. Sub-agents
// that care about a specific gated subsystem check the boolean in
// details; humans reading the human-formatted audit see a one-line
// "N enabled, M disabled" summary plus the per-feature breakdown.
func auditFeatures(cfg *config.ProjectConfig) AuditCategory {
	if cfg == nil {
		return AuditCategory{
			Status:  AuditStatusError,
			Summary: "no forge.yaml — features unknown",
		}
	}
	effective := cfg.Features.EffectiveFeatures()
	// Pre-allocate to non-nil empty slices so the JSON encoder
	// emits `[]` rather than `null` when nothing falls into a
	// bucket — sub-agents that `jq '.disabled | length'` need a
	// numeric length regardless of state.
	//
	// Stable vs experimental are surfaced as separate buckets so
	// consumers don't have to know the menu to interpret
	// "disabled": a default-off experimental feature is structurally
	// different from a user-opted-out stable feature.
	enabled := []string{}
	disabled := []string{}
	experimentalEnabled := []string{}
	for name, on := range effective {
		if config.IsExperimentalFeature(name) {
			if on {
				experimentalEnabled = append(experimentalEnabled, name)
			}
			continue
		}
		if on {
			enabled = append(enabled, name)
		} else {
			disabled = append(disabled, name)
		}
	}
	sort.Strings(enabled)
	sort.Strings(disabled)
	sort.Strings(experimentalEnabled)

	details := map[string]any{
		"resolved":               effective,
		"enabled":                enabled,
		"disabled":               disabled,
		"experimental_enabled":   experimentalEnabled,
		"experimental_available": append([]string{}, config.ExperimentalFeatureNames...),
	}
	summary := fmt.Sprintf("%d stable feature(s) enabled, %d disabled; %d experimental on",
		len(enabled), len(disabled), len(experimentalEnabled))
	return AuditCategory{Status: AuditStatusOK, Summary: summary, Details: details}
}

// auditIngress cross-checks forge.yaml backends against the dev-env
// KCL-declared Gateway API ingress (Gateways + HTTPRoutes + GRPCRoutes).
// Two failure modes:
//
//   - A route's `Service` doesn't match any known backend name —
//     services, frontends, or webhook handlers (error — the route is
//     dead at deploy time).
//   - A forge.yaml service declares `port:` but nothing routes to it
//     (info — internal-only services are valid; we just surface the
//     gap so the operator notices when they meant to ingress it).
//
// Frontends and webhook services are valid route backends too — at the
// k8s layer a route's `backendRefs[].name` resolves to any Service in
// the env namespace regardless of which forge.yaml block scaffolded it.
// We only emit the "port but no route" info for entries from
// cfg.Services: frontends own their own scaffold and may legitimately
// be cluster-internal-only (SSR-only) so flagging them would be noisy.
//
// We render the dev env because that's the only env every project is
// guaranteed to have. If `kcl` isn't on PATH or the dev dir is missing
// (CI environments without the toolchain), the category degrades to
// warn rather than failing the whole audit.
func auditIngress(cfg *config.ProjectConfig, projectDir string) AuditCategory {
	if cfg == nil {
		return AuditCategory{Status: AuditStatusError, Summary: "no forge.yaml"}
	}
	entities, err := RenderKCL(context.Background(), projectDir, "dev")
	if err != nil {
		return AuditCategory{
			Status:  AuditStatusWarn,
			Summary: fmt.Sprintf("could not evaluate dev KCL: %v", err),
		}
	}
	backends := ingressBackendNames(cfg)
	return crossCheckIngress(cfg.Components, backends, entities.Gateways, entities.HTTPRoutes, entities.GRPCRoutes)
}

// ingressBackendNames returns the union of every forge.yaml-declared
// name that can legitimately appear as a route backend: services,
// frontends, and per-service webhook handlers. K8s only sees a Service
// in the env namespace by that name — the forge.yaml block that
// scaffolded it is irrelevant at route-resolution time.
func ingressBackendNames(cfg *config.ProjectConfig) []string {
	names := make([]string, 0, len(cfg.Components)+len(cfg.Frontends))
	for _, s := range cfg.Components {
		names = append(names, s.Name)
		for _, w := range s.Webhooks {
			names = append(names, w.Name)
		}
	}
	for _, f := range cfg.Frontends {
		names = append(names, f.Name)
	}
	return names
}

// crossCheckIngress is the pure decision core of auditIngress: takes
// the resolved services / known-backend set / gateways / routes and
// returns the AuditCategory. Split out so unit tests can exercise the
// cross-check without shelling kcl. `backends` is the union of names
// (services + frontends + webhook handlers) any route may legally
// point at; `services` is kept separate because only it drives the
// "port declared but no route" info finding.
func crossCheckIngress(services []config.ComponentConfig, backends []string, gateways []GatewayEntity, httpRoutes []HTTPRouteEntity, grpcRoutes []GRPCRouteEntity) AuditCategory {
	knownBackend := make(map[string]struct{}, len(backends))
	for _, b := range backends {
		knownBackend[b] = struct{}{}
	}

	routedService := map[string]struct{}{}
	var findings []string
	hasError := false

	check := func(routeKind, name, svcRef string) {
		if svcRef == "" {
			return
		}
		routedService[svcRef] = struct{}{}
		if _, ok := knownBackend[svcRef]; !ok {
			findings = append(findings, fmt.Sprintf("error: %s %s references unknown service %s", routeKind, name, svcRef))
			hasError = true
		}
	}
	for _, r := range httpRoutes {
		check("route", r.Name, r.Service)
	}
	for _, r := range grpcRoutes {
		check("route", r.Name, r.Service)
	}

	servicesWithoutRoute := 0
	for _, s := range services {
		p := s.PrimaryPort()
		if p <= 0 {
			continue
		}
		if _, ok := routedService[s.Name]; ok {
			continue
		}
		findings = append(findings, fmt.Sprintf("info: service %s has port :%d declared but no ingress route — cluster-internal only", s.Name, p))
		servicesWithoutRoute++
	}

	sort.Strings(findings)

	status := AuditStatusOK
	if hasError {
		status = AuditStatusError
	}
	summary := fmt.Sprintf("%d gateway(s), %d route(s); %d service(s) without route",
		len(gateways), len(httpRoutes)+len(grpcRoutes), servicesWithoutRoute)
	details := map[string]any{
		"gateways":               len(gateways),
		"http_routes":            len(httpRoutes),
		"grpc_routes":            len(grpcRoutes),
		"services_without_route": servicesWithoutRoute,
	}
	if len(findings) > 0 {
		details["findings"] = findings
	}
	return AuditCategory{Status: status, Summary: summary, Details: details}
}

// auditEnvironments inventories every environment declared via a
// `deploy/kcl/<env>/main.k` file. Source of truth is the filesystem —
// forge.yaml no longer declares environments. Per-env deploy info
// (cluster / namespace / registry / domain) lives in the rendered
// KCL on `forge.K8sCluster` blocks; this audit just confirms the env
// directories exist and lists them.
func auditEnvironments(projectDir string) AuditCategory {
	envs, err := ListEnvs(projectDir)
	if err != nil {
		return AuditCategory{
			Status:  AuditStatusWarn,
			Summary: fmt.Sprintf("env discovery failed: %v", err),
		}
	}
	if len(envs) == 0 {
		return AuditCategory{
			Status:  AuditStatusOK,
			Summary: "no environments declared (no deploy/kcl/<env>/main.k)",
		}
	}
	type envEntry struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	entries := make([]envEntry, 0, len(envs))
	for _, env := range envs {
		entries = append(entries, envEntry{Name: env, Status: "ok"})
	}
	details := map[string]any{"environments": entries}
	return AuditCategory{
		Status:  AuditStatusOK,
		Summary: fmt.Sprintf("%d environment(s) declared (deploy/kcl/<env>/main.k)", len(envs)),
		Details: details,
	}
}

func packageNames(pkgs []config.PackageConfig) []string {
	out := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		out = append(out, p.Name)
	}
	return out
}

// auditConventions runs the lint linters whose results are amenable to
// programmatic roll-up: forgeconv. Anything that requires
// shelling to a Go subprocess (golangci, contractlint) is expensive and
// noisy in an audit context — we surface a hint to run `forge lint` for
// the full picture instead.
func auditConventions(cfg *config.ProjectConfig, projectDir string) AuditCategory {
	counts := map[string]int{}
	hasErrors := false
	hasWarnings := false

	protoDir := filepath.Join(projectDir, "proto")
	if dirExists(protoDir) {
		if res, err := forgeconv.LintProtoTree(protoDir); err == nil {
			for _, f := range res.Findings {
				key := "conventions/" + string(f.Severity)
				counts[key]++
				if f.Severity == forgeconv.SeverityError {
					hasErrors = true
				} else {
					hasWarnings = true
				}
			}
		}
	}

	status := AuditStatusOK
	switch {
	case hasErrors:
		status = AuditStatusError
	case hasWarnings:
		status = AuditStatusWarn
	}

	summary := "no convention violations"
	if hasErrors || hasWarnings {
		var bits []string
		keys := make([]string, 0, len(counts))
		for k := range counts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			bits = append(bits, fmt.Sprintf("%s=%d", k, counts[k]))
		}
		summary = strings.Join(bits, ", ")
	}

	_ = cfg // reserved for future per-feature gating
	return AuditCategory{
		Status:  status,
		Summary: summary,
		Details: map[string]any{
			"counts": counts,
			"hint":   fmt.Sprintf("run `%s lint` for full output (golangci, contractlint, etc.)", Name()),
		},
	}
}

// auditDisownedFile is one entry of the codegen category's
// `disowned_files` detail list: a generated file the user has taken
// permanent ownership of via `forge disown` (or a legacy fork the
// pipeline migration converted). Package-level (rather than
// function-local) so tests can assert the JSON shape directly.
type auditDisownedFile struct {
	Path string `json:"path"`
	// Since is when the file was disowned (RFC3339 UTC). Empty for
	// legacy forks whose original timestamp predates recording.
	Since string `json:"since,omitempty"`
	// Reason is the recorded WHY behind the disown — the newest
	// .forge/friction.jsonl entry with area=disown (or the legacy
	// area=fork) whose context names this path. Additive field per the
	// audit-json contract; empty when no entry exists.
	Reason string `json:"reason,omitempty"`
}

// auditCodegen reports on the project's self-certification state:
// hand-edits to forge-certified files (embedded forge:hash markers that
// fail verification), disowned files, orphan _gen files, and a pending
// legacy-manifest migration if one exists. Ownership is read from the
// files themselves — there is no global manifest to consult.
func auditCodegen(cfg *config.ProjectConfig, projectDir string) AuditCategory {
	cs, err := generator.LoadChecksums(projectDir)
	if err != nil {
		return AuditCategory{
			Status:  AuditStatusWarn,
			Summary: fmt.Sprintf("could not load .forge ownership state: %v", err),
		}
	}

	markers := checksums.ScanMarkers(projectDir)
	certified := len(markers) + len(cs.Unstampable)
	details := map[string]any{
		// `tracked_files` survives for audit-json consumers; its meaning
		// is now "files carrying forge's certification" (embedded marker
		// or scoped .forge/hashes.json record).
		"tracked_files":   certified,
		"certified_files": certified,
	}

	// Rough "last generate" timestamp: the newest mtime among certified
	// files (the old proxy — checksums.json's mtime — is gone with the
	// manifest).
	lastGen := time.Time{}
	for rel := range markers {
		if stat, statErr := os.Stat(filepath.Join(projectDir, rel)); statErr == nil && stat.ModTime().After(lastGen) {
			lastGen = stat.ModTime()
		}
	}
	if lastGen.IsZero() {
		details["last_generate"] = "never"
	} else {
		details["last_generate"] = lastGen.UTC().Format(time.RFC3339)
	}

	// Pending one-time migration: a legacy .forge/checksums.json still
	// present means the next `forge generate` (or `forge upgrade`) will
	// convert it to embedded markers and delete it.
	if _, statErr := os.Stat(filepath.Join(projectDir, checksums.LegacyChecksumFile)); statErr == nil {
		details["legacy_manifest"] = "present — the next `forge generate` migrates it to embedded forge:hash markers and deletes it"
	}

	// User-edited gen files: certification fails (embedded hash doesn't
	// match the recomputed body hash). Disowned files carry no marker
	// and are excluded by construction; Tier-2-managed starters are
	// exempt (edits there are sanctioned).
	var modified []string
	for _, d := range scanProjectDrift(projectDir, cs) {
		modified = append(modified, d.Path)
	}
	sort.Strings(modified)
	if len(modified) > 0 {
		details["user_edited_gen_files"] = modified
	}

	// Disowned files: one-way ownership transfers recorded in
	// .forge/disowned.json. A legitimate end state — reported for
	// visibility, never as a warning.
	var disowned []auditDisownedFile
	for rel, entry := range cs.Disowned {
		disowned = append(disowned, auditDisownedFile{Path: rel, Since: entry.DisownedAt, Reason: entry.Reason})
	}
	sort.Slice(disowned, func(i, j int) bool { return disowned[i].Path < disowned[j].Path })

	// Additive-extension contract: the legacy `forked_files` key is never
	// repurposed. It now always emits an empty array, with a note field
	// pointing consumers at its replacement; both will be dropped in a
	// future release.
	details["forked_files"] = []auditDisownedFile{}
	details["forked_files_note"] = "deprecated: the fork state was removed; see disowned_files"

	if len(disowned) > 0 {
		// Backfill rationales from the friction log for entries whose
		// disowned.json record predates reason capture (legacy
		// migrations). The log is loaded exactly once and only when
		// disowned files exist at all.
		reasons := disownFrictionReasons(projectDir)
		for i := range disowned {
			if disowned[i].Reason == "" {
				disowned[i].Reason = reasons[disowned[i].Path]
			}
		}
		details["disowned_files"] = disowned
		details["disowned_hint"] = "disowned files are user-owned; forge never regenerates them. Re-adopt one by deleting it and running `forge generate`."
	}

	// Orphan _gen detection: walk the project for files ending in _gen.go
	// that carry NO certification at all. These are usually safe to
	// delete, but flagging them at audit time tells the user without
	// forcing a regenerate.
	orphans := findOrphanGenFiles(projectDir, markers, cs)
	if len(orphans) > 0 {
		details["orphan_gen_files"] = orphans
	}

	// Manifest-era `tracked_missing_files` is gone with the manifest:
	// there is no record of paths that don't exist — a deleted generated
	// file is simply re-emitted on the next generate (and deletion IS
	// the documented re-adoption signal for disowned files).
	var missing []string

	// Registration findings: services whose on-disk presence disagrees
	// with the user-owned pkg/app/services.go row list. Two states:
	//   - unlisted: the row constructor is generated but unreferenced
	//     (typically right after `forge add service`) — register or
	//     tombstone it.
	//   - tombstoned: deliberately retired (comment in services.go) but
	//     handlers/<svc>/ still exists — the gated Tier-1 files are
	//     stale-sweep candidates (deleted under `forge generate
	//     --force-cleanup`); user-written Tier-2 files are never touched.
	// Additive detail key under the codegen category.
	unregistered := unregisteredServiceFindings(cfg, projectDir)
	if len(unregistered) > 0 {
		details["unregistered_services"] = unregistered
	}

	status := AuditStatusOK
	summary := fmt.Sprintf("%d certified, %d modified, %d disowned, %d orphans, %d missing", certified, len(modified), len(disowned), len(orphans), len(missing))
	// Disowned files are a legitimate end state (unlike the old fork
	// limbo) — they appear in the summary for visibility but do NOT
	// degrade the category status.
	if len(modified) > 0 || len(orphans) > 0 || len(missing) > 0 {
		status = AuditStatusWarn
	}
	if len(unregistered) > 0 {
		status = AuditStatusWarn
		summary += fmt.Sprintf(", %d unregistered service(s)", len(unregistered))
	}
	return AuditCategory{Status: status, Summary: summary, Details: details}
}

// auditUnregisteredService is one registration finding: a Connect
// service with no serviceRow in pkg/app/services.go whose handlers
// directory still exists on disk.
type auditUnregisteredService struct {
	Service string `json:"service"` // forge.yaml services[].name
	Dir     string `json:"dir"`     // project-relative handlers dir
	// State is "unlisted" (name appears nowhere in services.go — newly
	// added, row constructor generated but unreferenced) or
	// "tombstoned" (mentioned only in a comment — deliberately retired).
	State   string `json:"state"`
	Message string `json:"message"`
}

// unregisteredServiceFindings resolves each unregistered Connect
// service's handler directory disk-first and reports the ones still
// present. Resolution and registry-parse errors are skipped
// (best-effort — audit must not fail on a half-migrated tree); a
// missing dir on a tombstoned service is the retired steady state and
// produces no finding, while a missing services.go means everything is
// registered (pre-migration trees report nothing).
func unregisteredServiceFindings(cfg *config.ProjectConfig, projectDir string) []auditUnregisteredService {
	if cfg == nil {
		return nil
	}
	reg, err := loadServiceRegistry(projectDir)
	if err != nil || !reg.Exists {
		return nil
	}
	var out []auditUnregisteredService
	for _, s := range cfg.Components {
		if !isConnectServiceConfig(s) {
			continue
		}
		state := reg.state(s.Name)
		if state == registrationRegistered {
			continue
		}
		res, resErr := codegen.ResolveServiceComponent(projectDir, s.Name)
		if resErr != nil || !res.FromDisk {
			continue
		}
		dir := "handlers/" + res.ImportLeaf
		f := auditUnregisteredService{Service: s.Name, Dir: dir}
		switch state {
		case registrationTombstoned:
			f.State = "tombstoned"
			f.Message = fmt.Sprintf("%s exists but %s deliberately does not register %s (its row was deleted; the comment there says where it's served) — implement+register it by restoring `%s(app, cfg, logger, opts...),`, or delete the dir (run `%s generate --force-cleanup` to delete the generated files, then move or delete your hand-written ones)",
				dir, serviceRegistryRelPath, s.Name, codegen.ServiceRowFuncName(s.Name), Name())
		default:
			f.State = "unlisted"
			f.Message = fmt.Sprintf("row constructor %s is generated but unreferenced — to serve %s from this binary add `%s(app, cfg, logger, opts...),` to RegisteredServices in %s; to make it types-only delete %s and leave a comment in %s naming the binary that serves it",
				codegen.ServiceRowFuncName(s.Name), s.Name, codegen.ServiceRowFuncName(s.Name), serviceRegistryRelPath, dir, serviceRegistryRelPath)
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

// findOrphanGenFiles walks projectDir for *_gen.go files that carry no
// forge certification (no embedded marker, no scoped-fallback entry,
// not disowned) yet self-identify as forge output via the legacy
// banner. The walk skips noisy roots (vendor/, gen/, node_modules/,
// .git/) so audit stays cheap on large projects.
func findOrphanGenFiles(projectDir string, markers map[string]checksums.MarkerInfo, cs *generator.FileChecksums) []string {
	var orphans []string
	skip := map[string]struct{}{
		"vendor": {}, ".git": {}, "node_modules": {}, "gen": {}, ".forge": {},
	}
	_ = filepath.WalkDir(projectDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if _, ok := skip[d.Name()]; ok {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, "_gen.go") && !strings.HasSuffix(name, "_gen_test.go") {
			return nil
		}
		rel, err := filepath.Rel(projectDir, path)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if _, ok := markers[relSlash]; ok {
			return nil // certified — owned and accounted for
		}
		if cs != nil {
			if cs.IsDisowned(relSlash) {
				return nil
			}
			if _, ok := cs.Unstampable[relSlash]; ok {
				return nil
			}
		}
		// Banner check: forge-banner files generated by pre-marker forge
		// versions that never got certified.
		if isForgeGeneratedBanner(path) {
			orphans = append(orphans, relSlash)
		}
		return nil
	})
	sort.Strings(orphans)
	return orphans
}

// isForgeGeneratedBanner returns true when the file's first line
// declares it as "Code generated by forge" — the legacy authorship
// marker from before embedded forge:hash certification.
func isForgeGeneratedBanner(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 256)
	n, _ := f.Read(buf)
	head := string(buf[:n])
	if i := strings.IndexByte(head, '\n'); i >= 0 {
		head = head[:i]
	}
	return strings.Contains(head, "Code generated by forge")
}

// auditPacks compares each installed pack's version against the version
// embedded in the binary's pack registry. A mismatch (project pinned to
// "v0.1.0" but the binary ships "v0.2.0") surfaces as a warn.
func auditPacks(cfg *config.ProjectConfig) AuditCategory {
	if cfg == nil || len(cfg.Packs) == 0 {
		return AuditCategory{Status: AuditStatusOK, Summary: "no packs installed"}
	}
	type packEntry struct {
		Name             string `json:"name"`
		InstalledVersion string `json:"installed_version,omitempty"`
		LatestVersion    string `json:"latest_version,omitempty"`
		Status           string `json:"status"`
	}
	var entries []packEntry
	hasWarn := false
	for _, name := range cfg.Packs {
		// cfg.Packs is just a name list; we don't track per-project version
		// pins yet, so "installed" == whatever the binary ships.
		p, err := packs.GetPack(name)
		entry := packEntry{Name: name}
		if err != nil {
			entry.Status = "missing"
			entry.LatestVersion = "?"
			hasWarn = true
		} else {
			entry.LatestVersion = p.Version
			entry.InstalledVersion = p.Version
			entry.Status = "ok"
		}
		entries = append(entries, entry)
	}
	status := AuditStatusOK
	if hasWarn {
		status = AuditStatusWarn
	}
	return AuditCategory{
		Status:  status,
		Summary: fmt.Sprintf("%d pack(s) installed", len(entries)),
		Details: map[string]any{"packs": entries},
	}
}

// tablesFromMigrations naïvely greps "CREATE TABLE [IF NOT EXISTS]
// <name>" out of every .sql under migDir. It's a rough heuristic — the
// authoritative answer would parse SQL — but it captures the 95% case and
// the 5% it misses tend to be exotic forms (DDL inside CTE, etc.) that
// would never appear in a forge-generated migration.
var createTableRE = regexp.MustCompile(`(?i)CREATE\s+TABLE(?:\s+IF\s+NOT\s+EXISTS)?\s+["` + "`" + `]?([a-zA-Z_][a-zA-Z0-9_]*)["` + "`" + `]?`)

func tablesFromMigrations(dir string) map[string]struct{} {
	out := map[string]struct{}{}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".sql") {
			return nil
		}
		// Skip "down" migrations — they DROP TABLE, not CREATE.
		if strings.Contains(filepath.Base(path), ".down.") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, m := range createTableRE.FindAllStringSubmatch(string(data), -1) {
			if len(m) > 1 {
				out[strings.ToLower(m[1])] = struct{}{}
			}
		}
		return nil
	})
	return out
}

// auditScaffoldMarkers counts unfilled FORGE_SCAFFOLD placeholders that
// have survived a commit. The semantic check matches `forge lint
// --scaffolds`: only line-start `// FORGE_SCAFFOLD:` comments count as
// real markers; mere references to the literal string in source, docs,
// or template bodies do not. Directories that are intentional homes for
// markers (linter testdata, project templates) are skipped, matching
// the lint walker's skipDir + scaffold-template carve-outs.
//
// Without this filter, the audit would warn on every project that
// includes the scaffold linter source itself, the analyzer fixtures, or
// the generator templates that EMIT markers — i.e. it would be noisy on
// forge's own tree (which is a forge-managed project).
func auditScaffoldMarkers(projectDir string) AuditCategory {
	skip := map[string]struct{}{
		"vendor": {}, ".git": {}, "node_modules": {}, "gen": {}, ".forge": {},
		// testdata: linter fixtures intentionally hold markers so the
		// analyzer suite can assert it fires on them.
		"testdata": {},
		// templates/: forge's own scaffold templates contain the
		// markers as their literal output — they're not unfilled
		// placeholders in *this* tree.
		"templates": {},
	}
	var files []string
	total := 0
	_ = filepath.WalkDir(projectDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if _, ok := skip[d.Name()]; ok {
				return filepath.SkipDir
			}
			return nil
		}
		// Only scan files whose markers would be unfilled placeholders
		// (Go/proto/TS/YAML/SQL/templates). Markdown and JSON often
		// reference the marker syntax verbatim for documentation; we
		// don't want every README that mentions `FORGE_SCAFFOLD:` to
		// turn audit yellow.
		if !isMarkerScannable(d.Name()) {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		count := countLineStartScaffoldMarkers(data)
		if count > 0 {
			rel, _ := filepath.Rel(projectDir, path)
			files = append(files, filepath.ToSlash(rel))
			total += count
		}
		return nil
	})
	sort.Strings(files)
	status := AuditStatusOK
	if total > 0 {
		status = AuditStatusWarn
	}
	return AuditCategory{
		Status:  status,
		Summary: fmt.Sprintf("%d FORGE_SCAFFOLD marker(s) across %d file(s)", total, len(files)),
		Details: map[string]any{"files": files, "total_markers": total},
	}
}

// auditCRUDStubs counts custom-read-shape CodeUnimplemented stubs in
// the project's CRUD shim files (user-owned handlers_crud.go, plus
// legacy handlers_crud_gen.go from pre-split projects). These stubs are
// scaffolded when a request/response message shape deliberately
// diverges from the AIP-158 CRUD conventions — a legitimate domain
// decision, not an error; the body is the user's to implement in the
// owned shim. The stub keeps the file compiling but returns
// CodeUnimplemented at runtime — production traffic to that RPC will
// 501 until the body lands, which is why the category warns.
//
// Markers recognized: `// forge:custom-read-shape: <reason>` (current)
// and `// FORGE_CRUD_SHAPE_MISMATCH: <reason>` (the pre-rename
// spelling, kept for one release so existing files and greps keep
// matching — see the legacy_marker detail).
//
// Without this audit, the stub is silent: nothing in `forge doctor`
// or CI tells the operator that an RPC is shipping as Unimplemented
// (forge generate prints one warning line per stub at scaffold time).
// This category surfaces the count + per-method list as a structured
// audit finding so CI can branch on `.crud_stubs.status == "warn"`.
//
// We scan files matching the CRUD shim names rather than every Go file —
// both to keep the walk cheap and because the marker is forge-emitted
// and lives only in those files. Skip set mirrors auditScaffoldMarkers
// (vendor/.git/etc) plus templates/ and testdata/ so forge's own tree
// doesn't false-positive on its template body (which contains the
// literal marker as emission text).
func auditCRUDStubs(projectDir string) AuditCategory {
	skip := map[string]struct{}{
		"vendor": {}, ".git": {}, "node_modules": {}, "gen": {}, ".forge": {},
		"testdata":  {},
		"templates": {},
	}
	var stubs []map[string]string
	files := map[string]int{}
	total := 0
	_ = filepath.WalkDir(projectDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if _, ok := skip[d.Name()]; ok {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "handlers_crud.go" && d.Name() != "handlers_crud_gen.go" {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		rel, _ := filepath.Rel(projectDir, path)
		rel = filepath.ToSlash(rel)
		// Walk lines so each marker carries the nearest preceding
		// `func (s *Service) <Method>(` name — callers want the RPC
		// identifier, not just a file path. The CRUD template emits
		// the forge:custom-read-shape comment between the doc
		// comment and the function declaration, so we record the
		// method name when we *next* see the func line. Simpler:
		// stash the previous func name and apply it to the next
		// marker we see (the marker sits above the matching func).
		lines := strings.Split(string(data), "\n")
		type pendingMarker struct {
			reason string
			idx    int
		}
		var pending []pendingMarker
		for i, line := range lines {
			trimmed := strings.TrimLeft(line, " \t")
			if marker, ok := strings.CutPrefix(trimmed, "// forge:custom-read-shape:"); ok {
				pending = append(pending, pendingMarker{reason: strings.TrimSpace(marker), idx: i})
				continue
			}
			// Pre-rename spelling — recognized for one release.
			if marker, ok := strings.CutPrefix(trimmed, "// FORGE_CRUD_SHAPE_MISMATCH:"); ok {
				pending = append(pending, pendingMarker{reason: strings.TrimSpace(marker), idx: i})
				continue
			}
			if !strings.HasPrefix(trimmed, "func (s *Service) ") {
				continue
			}
			if len(pending) == 0 {
				continue
			}
			rest := strings.TrimPrefix(trimmed, "func (s *Service) ")
			methodEnd := strings.IndexAny(rest, "(")
			if methodEnd <= 0 {
				pending = pending[:0]
				continue
			}
			methodName := rest[:methodEnd]
			// Attach every pending marker to this func (in practice
			// the template emits exactly one per func, but the loop
			// stays correct if that changes).
			for _, p := range pending {
				stubs = append(stubs, map[string]string{
					"file":   rel,
					"method": methodName,
					"reason": p.reason,
				})
				files[rel]++
				total++
			}
			pending = pending[:0]
		}
		return nil
	})
	// Stable file list for the summary.
	fileNames := make([]string, 0, len(files))
	for f := range files {
		fileNames = append(fileNames, f)
	}
	sort.Strings(fileNames)
	// Deterministic stubs order (by file, then method) so JSON diffs are clean.
	sort.SliceStable(stubs, func(i, j int) bool {
		if stubs[i]["file"] != stubs[j]["file"] {
			return stubs[i]["file"] < stubs[j]["file"]
		}
		return stubs[i]["method"] < stubs[j]["method"]
	})
	status := AuditStatusOK
	summary := "0 custom-read-shape CRUD stubs"
	if total > 0 {
		status = AuditStatusWarn
		summary = fmt.Sprintf("%d custom-read-shape CRUD stub(s) across %d file(s) — bodies are yours to implement; the RPCs return CodeUnimplemented until then", total, len(fileNames))
	}
	return AuditCategory{
		Status:  status,
		Summary: summary,
		Details: map[string]any{
			"files":       fileNames,
			"total_stubs": total,
			"stubs":       stubs,
			// Grep compatibility: the marker was renamed from
			// FORGE_CRUD_SHAPE_MISMATCH to forge:custom-read-shape;
			// both spellings are scanned this release. Consumers
			// grepping source for the old string should switch.
			"marker":        "forge:custom-read-shape",
			"legacy_marker": "FORGE_CRUD_SHAPE_MISMATCH",
		},
	}
}

// diagnosticsRegisterStubRE matches the codegen-emitted line shape
// `diagnostics.Default.RegisterStub("symbol", "file", 123)`. The
// quoted strings allow any non-quote char (codegen never embeds
// escaped quotes); the line number is the bare-int third arg. The
// regex deliberately tolerates extra whitespace so a future gofmt
// shift on the emitted file doesn't invalidate the parse.
var diagnosticsRegisterStubRE = regexp.MustCompile(`diagnostics\.Default\.RegisterStub\(\s*"([^"]*)"\s*,\s*"([^"]*)"\s*,\s*(\d+)\s*\)`)

// diagnosticsRegisterNilDepRE matches the codegen-emitted line shape
// `diagnostics.Default.RegisterNilDep("component", "dep", "file", 123)`.
// Four args; component and dep are the runtime registration shape's
// distinguishing fields.
var diagnosticsRegisterNilDepRE = regexp.MustCompile(`diagnostics\.Default\.RegisterNilDep\(\s*"([^"]*)"\s*,\s*"([^"]*)"\s*,\s*"([^"]*)"\s*,\s*(\d+)\s*\)`)

// auditDiagnostics surfaces the runtime diagnostics registry shape at
// audit time. Sources the data by parsing pkg/app/diagnostics_gen.go
// for the Register* calls the codegen emitted at last generate (parse
// approach — cheap; the alternative is running the binary briefly
// and snapshotting Registry.Default, which is heavy and brittle).
//
// Even when the project hasn't enabled `features.diagnostics`, the
// category still appears with status=ok and an empty list, so
// downstream consumers (CI, dashboards) can rely on the key being
// present and additive-extension contract holds — see the
// `audit-json` skill for the contract details.
func auditDiagnostics(cfg *config.ProjectConfig, projectDir string) AuditCategory {
	path := filepath.Join(projectDir, "pkg", "app", "diagnostics_gen.go")
	enabled := cfg != nil && cfg.Features.DiagnosticsEnabled()
	strict := cfg != nil && cfg.Features.StrictWiringEnabled()

	type diagEntry struct {
		Kind      string `json:"kind"`
		Symbol    string `json:"symbol"`
		File      string `json:"file"`
		Line      int    `json:"line"`
		Component string `json:"component,omitempty"`
		DepName   string `json:"dep_name,omitempty"`
	}

	// Default details payload — always present so the additive
	// contract holds. `enabled` is the runtime feature gate; the
	// presence of entries is the codegen-time signal. We surface both
	// independently so a consumer can tell `unwired scaffolds exist
	// but bootstrap isn't emitting them` apart from `clean project`.
	details := map[string]any{
		"diagnostics":           []diagEntry{},
		"runtime_enabled":       enabled,
		"strict_wiring_enabled": strict,
	}

	data, err := os.ReadFile(path)
	if err != nil {
		// File missing is the common case for projects that haven't
		// regenerated since hooks 1+2 landed (or library/cli projects
		// with no pkg/app/). Report ok with the empty list — the
		// additive contract requires the category to exist regardless.
		if os.IsNotExist(err) {
			return AuditCategory{
				Status:  AuditStatusOK,
				Summary: "no pkg/app/diagnostics_gen.go (n/a — pre-codegen or library project)",
				Details: details,
			}
		}
		return AuditCategory{
			Status:  AuditStatusWarn,
			Summary: fmt.Sprintf("could not read diagnostics_gen.go: %v", err),
			Details: details,
		}
	}

	var entries []diagEntry
	for _, m := range diagnosticsRegisterStubRE.FindAllStringSubmatch(string(data), -1) {
		line, _ := strconv.Atoi(m[3])
		entries = append(entries, diagEntry{
			Kind:   "stub-impl",
			Symbol: m[1],
			File:   m[2],
			Line:   line,
		})
	}
	for _, m := range diagnosticsRegisterNilDepRE.FindAllStringSubmatch(string(data), -1) {
		line, _ := strconv.Atoi(m[4])
		entries = append(entries, diagEntry{
			Kind:      "nil-dep",
			Symbol:    m[1] + "." + m[2],
			Component: m[1],
			DepName:   m[2],
			File:      m[3],
			Line:      line,
		})
	}
	// Stable sort (kind, symbol) for deterministic JSON.
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Kind != entries[j].Kind {
			return entries[i].Kind < entries[j].Kind
		}
		return entries[i].Symbol < entries[j].Symbol
	})
	details["diagnostics"] = entries

	if len(entries) == 0 {
		return AuditCategory{
			Status:  AuditStatusOK,
			Summary: "0 unwired scaffolds registered",
			Details: details,
		}
	}

	// Status semantics: warn when entries exist, error when strict_wiring
	// is on AND entries exist (the project will fail to boot in this
	// configuration). Matches the runtime emit policy — operators and
	// CI both get the same verdict from one audit run.
	status := AuditStatusWarn
	if strict {
		status = AuditStatusError
	}
	return AuditCategory{
		Status:  status,
		Summary: fmt.Sprintf("%d unwired scaffold(s) registered", len(entries)),
		Details: details,
	}
}

// countLineStartScaffoldMarkers counts lines whose first non-whitespace
// content is the comment-form scaffold marker. Mirrors
// internal/linter/scaffolds.countScaffoldMarkers — kept separate to
// avoid pulling the linter package into the cli audit dependency
// graph.
func countLineStartScaffoldMarkers(data []byte) int {
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		// Match both Go-style `// FORGE_SCAFFOLD:` and YAML/shell-style
		// `# FORGE_SCAFFOLD:` (the lint analyzer is Go-only, but audit
		// is allowed to span more file types).
		if strings.HasPrefix(trimmed, "// FORGE_SCAFFOLD:") || strings.HasPrefix(trimmed, "# FORGE_SCAFFOLD:") {
			count++
		}
	}
	return count
}

// isMarkerScannable identifies file types whose
// FORGE_SCAFFOLD markers are real unfilled placeholders rather than
// documentation references. Markdown and JSON commonly cite the marker
// syntax in prose / fixtures, so they're excluded — those occurrences
// would otherwise generate noisy "scaffold present" warnings on every
// project that documents how scaffolds work.
func isMarkerScannable(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".go", ".proto", ".ts", ".tsx", ".js", ".jsx",
		".yaml", ".yml", ".sql", ".k", ".sh", ".tmpl", ".toml":
		return true
	}
	return false
}

// auditPackGraph checks that every installed pack's declared `depends_on`
// is also installed. Surfaces "missing producer" cases — e.g. someone
// hand-edited cfg.Packs to remove audit-log while leaving api-key in
// place, or installed an older project on a newer forge that introduced
// a new dep edge. Returns ok when no installed pack declares a dep, or
// when every dep is satisfied.
func auditPackGraph(cfg *config.ProjectConfig) AuditCategory {
	if cfg == nil || len(cfg.Packs) == 0 {
		return AuditCategory{Status: AuditStatusOK, Summary: "no packs installed (n/a)"}
	}
	missing := packs.MissingDependencies(cfg.Packs)
	// Also build the full edge list for the details payload — useful for
	// LLM consumers that want to render the graph without a second
	// round-trip to `forge pack list --deps`.
	edges := map[string][]string{}
	for _, name := range cfg.Packs {
		p, err := packs.GetPack(name)
		if err != nil || len(p.DependsOn) == 0 {
			continue
		}
		edges[name] = append([]string(nil), p.DependsOn...)
	}
	details := map[string]any{
		"installed_packs": cfg.Packs,
		"declared_edges":  edges,
	}
	if len(missing) > 0 {
		details["missing_dependencies"] = missing
		details["hint"] = "run `forge pack add <name>` for each missing dep, or remove the consuming pack to drop the requirement"
		return AuditCategory{
			Status:  AuditStatusError,
			Summary: fmt.Sprintf("%d missing pack dependency(ies): %s", len(missing), strings.Join(missing, ", ")),
			Details: details,
		}
	}
	return AuditCategory{
		Status:  AuditStatusOK,
		Summary: fmt.Sprintf("%d pack(s) installed; %d declared edge(s) all satisfied", len(cfg.Packs), len(edges)),
		Details: details,
	}
}

// auditMigrationSafety summarises the project's migration_safety
// configuration: number of allowlisted destructive globs, the
// destructive_change severity setting, and the timestamp of the most
// recent migration. Surfaces as warn when allowed_destructive is
// non-empty (informational — user has consciously opted out of the
// destructive-change guard for some files), error when the directory
// has migrations but none are parseable.
func auditMigrationSafety(cfg *config.ProjectConfig, projectDir string) AuditCategory {
	migDir := filepath.Join(projectDir, "db", "migrations")
	if cfg != nil && cfg.Database.MigrationsDir != "" {
		migDir = filepath.Join(projectDir, cfg.Database.MigrationsDir)
	}
	hasMigrations := dirExists(migDir)
	if !hasMigrations {
		return AuditCategory{Status: AuditStatusOK, Summary: "no migrations directory (n/a)"}
	}

	// Count migrations + find the latest mtime.
	var latestMtime time.Time
	latestName := ""
	migCount := 0
	entries, _ := os.ReadDir(migDir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		migCount++
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(latestMtime) {
			latestMtime = info.ModTime()
			latestName = e.Name()
		}
	}

	details := map[string]any{
		"migration_count": migCount,
		"migrations_dir":  migDir,
	}
	if latestName != "" {
		details["latest_migration"] = latestName
		details["latest_migration_mtime"] = latestMtime.UTC().Format(time.RFC3339)
	}

	allowedCount := 0
	severity := "error"
	if cfg != nil {
		allowedCount = len(cfg.Database.MigrationSafety.AllowedDestructive)
		severity = cfg.Database.MigrationSafety.EffectiveDestructiveChange()
		if allowedCount > 0 {
			details["allowed_destructive"] = cfg.Database.MigrationSafety.AllowedDestructive
		}
		details["destructive_change_severity"] = severity
	}

	status := AuditStatusOK
	summary := fmt.Sprintf("%d migration(s); %d allowed_destructive; destructive_change=%s",
		migCount, allowedCount, severity)
	if allowedCount > 0 {
		// Informational warn — surface that the project has explicit
		// destructive carve-outs the user should re-review periodically.
		status = AuditStatusWarn
		details["hint"] = "review allowed_destructive entries periodically; once a destructive migration ships, remove its allowlist entry to re-enable the guard"
	}
	return AuditCategory{Status: status, Summary: summary, Details: details}
}

// auditWireCoverage rolls up unresolved Deps fields in pkg/app/wire_gen.go
// — the same surface as `forge lint --wire-coverage`, but as a count and
// per-component breakdown rather than per-finding output. Useful for an
// audit-level "is wire complete?" yes/no without making the user shell
// to lint.
func auditWireCoverage(projectDir string) AuditCategory {
	path := filepath.Join(projectDir, "pkg", "app", "wire_gen.go")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return AuditCategory{
			Status:  AuditStatusOK,
			Summary: "no pkg/app/wire_gen.go (n/a — library project or pre-generate)",
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return AuditCategory{
			Status:  AuditStatusWarn,
			Summary: fmt.Sprintf("could not open wire_gen.go: %v", err),
		}
	}
	defer func() { _ = f.Close() }()

	findings, err := scanWireGen(f, path, projectDir)
	if err != nil {
		return AuditCategory{
			Status:  AuditStatusWarn,
			Summary: fmt.Sprintf("scan failed: %v", err),
		}
	}

	if len(findings) == 0 {
		return AuditCategory{
			Status:  AuditStatusOK,
			Summary: "wire coverage clean — no unresolved Deps fields",
		}
	}

	// Aggregate by component (wire*Deps function) for the breakdown.
	byComponent := map[string][]string{}
	for _, f := range findings {
		comp := f.Function
		if comp == "" {
			comp = "(unattributed)"
		}
		byComponent[comp] = append(byComponent[comp], f.Field)
	}
	components := make([]string, 0, len(byComponent))
	for k := range byComponent {
		components = append(components, k)
	}
	sort.Strings(components)

	details := map[string]any{
		"unresolved_count":    len(findings),
		"affected_components": components,
		"by_component":        byComponent,
		"hint":                fmt.Sprintf("run `%s lint --wire-coverage` for the full per-line report", Name()),
	}
	return AuditCategory{
		Status:  AuditStatusWarn,
		Summary: fmt.Sprintf("%d unresolved Deps field(s) across %d component(s)", len(findings), len(components)),
		Details: details,
	}
}

// auditOptionalDepsGuard rolls up unguarded derefs of
// `// forge:optional-dep` Deps fields (the optional-deps-guard lint).
// Optional fields skip validateDeps by design, so an unguarded
// `s.deps.X.Method(...)` is a latent nil-panic no startup gate catches.
// Findings are warn-level (the walker is conservative, not full
// dataflow — see lint_optional_deps_guard.go); the category is
// additive per the audit-json contract (consumers iterate
// `.categories | keys[]` and tolerate new entries).
func auditOptionalDepsGuard(projectDir string) AuditCategory {
	findings, err := collectOptionalDepsGuardFindings(projectDir)
	if err != nil {
		return AuditCategory{
			Status:  AuditStatusWarn,
			Summary: fmt.Sprintf("optional-deps-guard scan failed: %v", err),
		}
	}
	if len(findings) == 0 {
		return AuditCategory{
			Status:  AuditStatusOK,
			Summary: "optional-deps-guard clean — every optional-dep deref is nil-guarded (or suppressed)",
		}
	}

	// Aggregate by <role>/<pkg> so a sub-agent can jump straight to the
	// offending component; per-finding detail lives in
	// `forge lint --optional-deps-guard --json`.
	byPackage := map[string][]string{}
	for _, f := range findings {
		key := f.Role + "/" + f.Package
		byPackage[key] = append(byPackage[key],
			fmt.Sprintf("%s:%d %s in %s", f.File, f.Line, f.Expr, f.Method))
	}
	pkgs := make([]string, 0, len(byPackage))
	for k := range byPackage {
		pkgs = append(pkgs, k)
	}
	sort.Strings(pkgs)

	return AuditCategory{
		Status:  AuditStatusWarn,
		Summary: fmt.Sprintf("%d unguarded optional-dep deref(s) across %d package(s)", len(findings), len(pkgs)),
		Details: map[string]any{
			"finding_count":     len(findings),
			"affected_packages": pkgs,
			"by_package":        byPackage,
			"hint":              fmt.Sprintf("run `%s lint --optional-deps-guard` for the full per-line report; suppress confirmed-safe sites with `// forge:optional-checked` on the deref line", Name()),
		},
	}
}

// auditConfigDeps rolls up scalar Deps fields (the config-deps lint).
// Scalar Deps fields are configuration, not collaborators — wire_gen
// can never resolve them from App/AppExtras, so they regenerate as
// typed zeros + TODOs forever (kalshi-trader fr-ad24278452) unless the
// user hand-projects them via AppExtras + setup.go. The supported
// shape is a component config block in proto/config taken as one typed
// field. Findings are warn-level; the category is additive per the
// audit-json contract (consumers iterate `.categories | keys[]` and
// tolerate new entries).
func auditConfigDeps(projectDir string) AuditCategory {
	findings, err := collectConfigDepsFindings(projectDir)
	if err != nil {
		return AuditCategory{
			Status:  AuditStatusWarn,
			Summary: fmt.Sprintf("config-deps scan failed: %v", err),
		}
	}
	if len(findings) == 0 {
		return AuditCategory{
			Status:  AuditStatusOK,
			Summary: "config-deps clean — no scalar Deps fields (configuration flows through config blocks)",
		}
	}

	// Aggregate by <role>/<pkg> so a sub-agent can jump straight to the
	// offending component; per-finding detail lives in
	// `forge lint --config-deps --json`.
	byPackage := map[string][]string{}
	for _, f := range findings {
		key := f.Role + "/" + f.Package
		byPackage[key] = append(byPackage[key],
			fmt.Sprintf("%s:%d Deps.%s %s", f.File, f.Line, f.Field, f.Type))
	}
	pkgs := make([]string, 0, len(byPackage))
	for k := range byPackage {
		pkgs = append(pkgs, k)
	}
	sort.Strings(pkgs)

	return AuditCategory{
		Status:  AuditStatusWarn,
		Summary: fmt.Sprintf("%d scalar Deps field(s) across %d package(s) — scalars are configuration; declare component config blocks in proto/config", len(findings), len(pkgs)),
		Details: map[string]any{
			"finding_count":     len(findings),
			"affected_packages": pkgs,
			"by_package":        byPackage,
			"hint":              fmt.Sprintf("run `%s lint --config-deps` for per-field remediation snippets (declare a <Component>Config message in proto/config/v1/config.proto and take it as `Cfg config.<Component>Config`)", Name()),
		},
	}
}

// auditDeps surfaces dep-shaped risks: missing go.sum, gen/ unstable
// (would `go mod tidy` produce a diff?). We don't actually run `go mod
// tidy` because that's expensive and would mutate state — we just check
// for go.sum presence and report.
func auditDeps(projectDir string) AuditCategory {
	details := map[string]any{}
	hasWarn := false

	if _, err := os.Stat(filepath.Join(projectDir, "go.mod")); err == nil {
		details["go_mod"] = "present"
		if _, err := os.Stat(filepath.Join(projectDir, "go.sum")); os.IsNotExist(err) {
			details["go_sum"] = "missing — run `go mod tidy`"
			hasWarn = true
		} else {
			details["go_sum"] = "present"
		}
	} else {
		details["go_mod"] = "missing"
	}

	if _, err := os.Stat(filepath.Join(projectDir, "gen", "go.mod")); err == nil {
		details["gen_go_mod"] = "present"
	}

	status := AuditStatusOK
	summary := "deps look healthy"
	if hasWarn {
		status = AuditStatusWarn
		summary = "deps need attention"
	}
	return AuditCategory{Status: status, Summary: summary, Details: details}
}

// printAuditReport renders the human-readable audit. Layout: one line
// header, then one block per category in auditCategoryOrder, then a
// trailing overall verdict.
func printAuditReport(w *os.File, r *AuditReport) {
	_, _ = fmt.Fprintf(w, "Forge audit — %s (kind=%s, binary=%s)\n", r.ProjectName, r.ProjectKind, r.BinaryVersion)
	_, _ = fmt.Fprintf(w, "Generated at %s\n\n", r.GeneratedAt.Format(time.RFC3339))

	printed := map[string]struct{}{}
	for _, key := range auditCategoryOrder {
		if cat, ok := r.Categories[key]; ok {
			printAuditCategory(w, key, cat)
			printed[key] = struct{}{}
		}
	}
	// Fallthrough: any categories not in the canonical order, alphabetical.
	var extras []string
	for k := range r.Categories {
		if _, ok := printed[k]; !ok {
			extras = append(extras, k)
		}
	}
	sort.Strings(extras)
	for _, k := range extras {
		printAuditCategory(w, k, r.Categories[k])
	}

	_, _ = fmt.Fprintf(w, "Overall: %s\n", strings.ToUpper(string(r.OverallStatus)))
}

func printAuditCategory(w *os.File, key string, cat AuditCategory) {
	icon := "✓"
	switch cat.Status {
	case AuditStatusWarn:
		icon = "⚠"
	case AuditStatusError:
		icon = "✗"
	}
	_, _ = fmt.Fprintf(w, "%s %s — %s\n", icon, key, cat.Summary)
	if len(cat.Details) > 0 {
		// Print details indented; primitives inline, slices/maps collapsed.
		keys := make([]string, 0, len(cat.Details))
		for k := range cat.Details {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := cat.Details[k]
			_, _ = fmt.Fprintf(w, "    %s: %s\n", k, formatDetailValue(v))
		}
	}
	_, _ = fmt.Fprintln(w)
}

func formatDetailValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []string:
		if len(t) == 0 {
			return "[]"
		}
		if len(t) <= 5 {
			return "[" + strings.Join(t, ", ") + "]"
		}
		return fmt.Sprintf("[%s, ... (%d total)]", strings.Join(t[:5], ", "), len(t))
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		s := string(b)
		if len(s) > 200 {
			s = s[:197] + "..."
		}
		return s
	}
}
