package cli

// Render-level tests for the frontend generated-code review fixes.
// These pin template OUTPUT (string/parse assertions on rendered TS), not
// the live npm toolchain — the fast loop the velocity rules require.

import (
	"bytes"
	"strings"
	"testing"
	"text/template"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/templates"
)

// renderHooksForTest renders hooks.ts.tmpl for a 3-RPC CRUD-ish service.
func renderHooksForTest(t *testing.T, workspaces bool) string {
	t.Helper()
	svc := codegen.ServiceDef{
		Name:      "TaskService",
		Package:   "demo.v1",
		ProtoFile: "proto/services/tasks/v1/tasks.proto",
		Methods: []codegen.Method{
			{Name: "ListTasks", InputType: "ListTasksRequest", OutputType: "ListTasksResponse"},
			{Name: "GetTask", InputType: "GetTaskRequest", OutputType: "GetTaskResponse"},
			{Name: "CreateTask", InputType: "CreateTaskRequest", OutputType: "CreateTaskResponse"},
			{Name: "SendReport", InputType: "SendReportRequest", OutputType: "SendReportResponse"},
		},
	}
	data := codegen.ServiceDefToHookData(svc)
	data.Workspaces = workspaces
	if workspaces {
		data.ApiPackage = "@demo/api"
	}

	content, err := templates.FrontendTemplates().Get("hooks.ts.tmpl")
	if err != nil {
		t.Fatalf("read hooks template: %v", err)
	}
	tmpl, err := template.New("hooks.ts.tmpl").Funcs(templates.FuncMap()).Parse(string(content))
	if err != nil {
		t.Fatalf("parse hooks template: %v", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		t.Fatalf("render hooks template: %v", err)
	}
	return buf.String()
}

// TestFrontendLintConfigConsistency is the cheap, string-level guard for
// F12: the emitted toolchain config must keep the strictness knobs the
// generated code is written against. The full `npm run lint` /
// `tsc --noEmit` pass over a scaffolded project is the (slow) end-to-end
// verification; this test catches config regressions in milliseconds.
func TestFrontendLintConfigConsistency(t *testing.T) {
	// tsconfig: noUncheckedIndexedAccess must be on — generated code is
	// written to satisfy it (typed Records, guarded index access).
	tsconfig, err := templates.FrontendTemplates().Render("nextjs/tsconfig.json.tmpl", templates.FrontendTemplateData{})
	if err != nil {
		t.Fatalf("render tsconfig: %v", err)
	}
	if !strings.Contains(string(tsconfig), `"noUncheckedIndexedAccess": true`) {
		t.Errorf("nextjs tsconfig must enable noUncheckedIndexedAccess:\n%s", tsconfig)
	}

	// eslint config: alias classification + default-export scoping that
	// the generated files rely on for a zero-warning pristine lint.
	eslintCfg, err := templates.FrontendTemplates().Get("nextjs/eslint.config.mjs")
	if err != nil {
		t.Fatalf("read eslint config: %v", err)
	}
	cfg := string(eslintCfg)
	for _, want := range []string{
		`"import/internal-regex": "^@/"`,
		`"src/app/**/*.{ts,tsx}"`,
		`"src/components/ui/**/*.{ts,tsx}"`,
		`"src/mocks/scenarios/**/*.{ts,tsx}"`,
		`"import/no-default-export": "off"`,
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("nextjs eslint config missing %q", want)
		}
	}

	// Generated hooks: type-only imports must come last (the import/order
	// "type" group) and value imports first — string-level proxy for the
	// import/order rule on the highest-traffic generated file.
	hooks := renderHooksForTest(t, false)
	lastValueImport := strings.LastIndex(hooks, "\nimport { ")
	firstTypeImport := strings.Index(hooks, "\nimport type { ")
	if firstTypeImport >= 0 && lastValueImport > firstTypeImport {
		t.Errorf("hooks file has a value import after a type import (violates import/order type-group-last):\n%s", hooks)
	}

	// format-utils: the badge variant map must be explicit with a neutral
	// fallback — no hash-to-color scheme.
	fu, err := templates.FrontendTemplates().Get("nextjs/src/lib/format-utils.ts")
	if err != nil {
		t.Fatalf("read format-utils: %v", err)
	}
	if strings.Contains(string(fu), "charCodeAt") {
		t.Errorf("format-utils still hashes enum values to badge colors")
	}
	for _, want := range []string{`?? "neutral"`, "export function userMessage", "rawMessage"} {
		if !strings.Contains(string(fu), want) {
			t.Errorf("format-utils missing %q", want)
		}
	}

	// query-client: the single error-toast chokepoint with typed opt-out.
	qc, err := templates.FrontendTemplates().Get("nextjs/src/lib/query-client.ts")
	if err != nil {
		t.Fatalf("read query-client: %v", err)
	}
	for _, want := range []string{"MutationCache", "silenceErrorToast", "emitToast({ message: userMessage(error)"} {
		if !strings.Contains(string(qc), want) {
			t.Errorf("query-client missing %q", want)
		}
	}
}

// crudPageDataForTest builds a fully-enriched PageTemplateData (CRUD RPCs +
// entity field metadata) the way generateFrontendPages does.
func crudPageDataForTest(t *testing.T) codegen.PageTemplateData {
	t.Helper()
	svc := codegen.ServiceDef{
		Name:      "TaskService",
		Package:   "demo.v1",
		ProtoFile: "proto/services/tasks/v1/tasks.proto",
		Methods: []codegen.Method{
			{Name: "ListTasks", InputType: "ListTasksRequest", OutputType: "ListTasksResponse"},
			{Name: "GetTask", InputType: "GetTaskRequest", OutputType: "GetTaskResponse"},
			{Name: "CreateTask", InputType: "CreateTaskRequest", OutputType: "CreateTaskResponse"},
			{Name: "UpdateTask", InputType: "UpdateTaskRequest", OutputType: "UpdateTaskResponse"},
			{Name: "DeleteTask", InputType: "DeleteTaskRequest", OutputType: "DeleteTaskResponse"},
		},
		Messages: map[string][]codegen.MessageFieldDef{
			"CreateTaskRequest": {
				{Name: "title", ProtoType: "string"},
				{Name: "status", ProtoType: "string"},
				{Name: "done", ProtoType: "bool"},
			},
			"UpdateTaskRequest": {
				{Name: "id", ProtoType: "string"},
				{Name: "title", ProtoType: "string"},
				{Name: "status", ProtoType: "string"},
				{Name: "done", ProtoType: "bool"},
			},
		},
	}
	pages := codegen.ExtractCRUDEntities(svc)
	if len(pages) != 1 {
		t.Fatalf("expected 1 CRUD entity, got %d", len(pages))
	}
	page := pages[0]
	entity := codegen.EntityDef{
		Name:      "Task",
		PkField:   "id",
		ProtoFile: "proto/db/v1/tasks.proto",
		Fields: []codegen.EntityField{
			{Name: "id", ProtoType: "string", Kind: codegen.FieldKindScalar},
			{Name: "title", ProtoType: "string", Kind: codegen.FieldKindScalar},
			{Name: "status", ProtoType: "string", Kind: codegen.FieldKindScalar},
			{Name: "done", ProtoType: "bool", Kind: codegen.FieldKindScalar},
			{Name: "created_at", ProtoType: "google.protobuf.Timestamp", Kind: codegen.FieldKindTimestamp},
			{Name: "metadata", ProtoType: "message", Kind: codegen.FieldKindMessage}, // must be skipped
		},
	}
	codegen.AttachEntityMeta(&page, entity)
	return page
}

func renderPageForTest(t *testing.T, tmplName string, data codegen.PageTemplateData) string {
	t.Helper()
	tmpl, err := loadPageTemplate("pages", tmplName)
	if err != nil {
		t.Fatalf("load %s: %v", tmplName, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		t.Fatalf("render %s: %v", tmplName, err)
	}
	return buf.String()
}

// TestPageTemplates_TypedColumnsNoReflection pins F1: the generator knows
// the entity's fields, so pages declare typed columns/rows instead of
// casting proto messages to Record<string, unknown> and reflecting.
func TestPageTemplates_TypedColumnsNoReflection(t *testing.T) {
	page := crudPageDataForTest(t)

	list := renderPageForTest(t, "list-page.tsx.tmpl", page)
	for _, want := range []string{
		`import type { Task } from "@/gen/db/v1/tasks_pb";`,
		"key: keyof Task & string;",
		"] satisfies Column[];",
		`key: "title",`,
		// status is enum-like → Badge render, declared at generate time.
		`render: (item) => <Badge label={String(item.status)} variant={enumBadgeVariant(String(item.status))} />,`,
		// typed search over string fields only
		`const searchFields = ["id", "title", "status"] as const satisfies readonly (keyof Task & string)[];`,
		// typed row key + navigation
		"key={String(item.id)}",
		"router.push(`/tasks/${item.id}`)",
		// user-facing error copy
		"message={userMessage(error)}",
	} {
		if !strings.Contains(list, want) {
			t.Errorf("list page missing %q:\n%s", want, list)
		}
	}
	for _, banned := range []string{"as Record<string, unknown>", "Object.keys(", "Object.values(", "$typeName", "?? data;"} {
		if strings.Contains(list, banned) {
			t.Errorf("list page still reflects/hedges (%q):\n%s", banned, list)
		}
	}
	// The skipped message-kind field must not become a column.
	if strings.Contains(list, `key: "metadata"`) {
		t.Errorf("list page rendered a column for a message-kind field:\n%s", list)
	}

	detail := renderPageForTest(t, "detail-page.tsx.tmpl", page)
	for _, want := range []string{
		"const item = data?.task;",
		`label: "Created At",`,
		"value: formatValue(item.createdAt),",
		"message={userMessage(error)}",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail page missing %q:\n%s", want, detail)
		}
	}
	for _, banned := range []string{"as Record<string, unknown>", "Object.entries(", "?? data;", "useQueryClient", "invalidateQueries"} {
		if strings.Contains(detail, banned) {
			t.Errorf("detail page still contains %q (reflection hedge or redundant invalidation):\n%s", banned, detail)
		}
	}

	edit := renderPageForTest(t, "edit-page.tsx.tmpl", page)
	for _, want := range []string{
		"const item = data?.task;",
		"title: String(item.title ?? \"\"),",
		"done: Boolean(item.done),",
		"meta: { silenceErrorToast: true },",
		"message={userMessage(mutation.error)}",
	} {
		if !strings.Contains(edit, want) {
			t.Errorf("edit page missing %q:\n%s", want, edit)
		}
	}
	for _, banned := range []string{"as Record<string, unknown>", "as Parameters<typeof mutation.mutate>[0]"} {
		if strings.Contains(edit, banned) {
			t.Errorf("edit page still contains %q:\n%s", banned, edit)
		}
	}

	create := renderPageForTest(t, "create-page.tsx.tmpl", page)
	for _, want := range []string{
		"meta: { silenceErrorToast: true },",
		"message={userMessage(mutation.error)}",
	} {
		if !strings.Contains(create, want) {
			t.Errorf("create page missing %q:\n%s", want, create)
		}
	}
	if strings.Contains(create, "as Parameters<typeof mutation.mutate>[0]") {
		t.Errorf("create page still casts the mutate payload:\n%s", create)
	}
}

// TestBuildNavPages_FiltersOnEntitySet pins the F2 fix: nav/dashboard
// routes derive from the SAME entity set that gates page emission. A
// service with CRUD-shaped RPC names but no proto entity definition gets
// pages skipped — so it must get NO nav route either.
func TestBuildNavPages_FiltersOnEntitySet(t *testing.T) {
	services := []codegen.ServiceDef{
		{
			Name:      "TaskService",
			ProtoFile: "proto/services/tasks/v1/tasks.proto",
			Methods: []codegen.Method{
				{Name: "ListTasks", InputType: "ListTasksRequest", OutputType: "ListTasksResponse"},
				{Name: "CreateTask", InputType: "CreateTaskRequest", OutputType: "CreateTaskResponse"},
				// CRUD-shaped, but "Report" has no entity definition —
				// the page generator skips it, so nav must too.
				{Name: "ListReports", InputType: "ListReportsRequest", OutputType: "ListReportsResponse"},
			},
		},
	}
	entities := []codegen.EntityDef{{Name: "Task"}}

	pages := buildNavPages(services, entities)
	if len(pages) != 1 {
		t.Fatalf("expected exactly 1 nav page (Task), got %d: %+v", len(pages), pages)
	}
	p := pages[0]
	if p.Slug != "tasks" || !p.HasCreate || p.ListHook != "useListTasks" || p.ItemsField != "tasks" || p.LabelSingular != "Task" {
		t.Errorf("unexpected nav page data: %+v", p)
	}
	if p.HooksModule == "" {
		t.Errorf("expected HooksModule to be populated, got %+v", p)
	}
}

// TestBuildNavPages_ControlPlaneEntitySet is the regression test for the
// nav-empties bug: the applied-schema entity projection (BuildSchemaEntities)
// names entities by the singular CRUD-RPC form (EntityDef.Name = "LLMKey"),
// while ExtractCRUDEntities re-derives the plural + kebab slug from the same
// RPC. The nav gate matches the two halves; if it matches on the raw
// lowercase NAME instead of the deterministic kebab SLUG, an acronym entity
// whose two name projections disagree on casing (proto-Go "LlmKey" vs CRUD
// "LLMKey") falls through the gate, its route is dropped, and on a project
// where EVERY admin entity is an acronym/aggregated List the ENTIRE nav
// regenerates empty — ALL_ROUTES = [] and every dashboard tile vanishes,
// with no error.
//
// This reproduces the real control-plane shape that triggered it: five
// admin entities sourced from five different services (LLMKey, Daemon,
// Plan, UsageEvent, User), several List-only (admin read views), and the
// entity set keyed by the singular projection — including the casing-
// divergent "LlmKey" form to prove the slug match is casing-proof. The
// assertion is the full route set is populated, not empty.
func TestBuildNavPages_ControlPlaneEntitySet(t *testing.T) {
	services := []codegen.ServiceDef{
		{Name: "LLMGatewayService", ProtoFile: "proto/controlplane/v1/llm_gateway_service.proto",
			Methods: []codegen.Method{
				{Name: "ListLLMKeys", InputType: "ListLLMKeysRequest", OutputType: "ListLLMKeysResponse"},
				{Name: "CreateLLMKey", InputType: "CreateLLMKeyRequest", OutputType: "CreateLLMKeyResponse"},
				{Name: "GetLLMKey", InputType: "GetLLMKeyRequest", OutputType: "GetLLMKeyResponse"},
			}},
		{Name: "DaemonService", ProtoFile: "proto/controlplane/v1/daemon_service.proto",
			Methods: []codegen.Method{
				{Name: "ListDaemons", InputType: "ListDaemonsRequest", OutputType: "ListDaemonsResponse"},
				{Name: "CreateDaemon", InputType: "CreateDaemonRequest", OutputType: "CreateDaemonResponse"},
			}},
		{Name: "BillingService", ProtoFile: "proto/controlplane/v1/billing_service.proto",
			Methods: []codegen.Method{
				{Name: "ListPlans", InputType: "ListPlansRequest", OutputType: "ListPlansResponse"},
			}},
		{Name: "BillingAdminService", ProtoFile: "proto/controlplane/v1/billing_admin.proto",
			Methods: []codegen.Method{
				{Name: "ListUsageEvents", InputType: "ListUsageEventsRequest", OutputType: "ListUsageEventsResponse"},
			}},
		{Name: "UserAdminService", ProtoFile: "proto/controlplane/v1/user_admin.proto",
			Methods: []codegen.Method{
				{Name: "ListUsers", InputType: "ListUsersRequest", OutputType: "ListUsersResponse"},
			}},
	}
	// The entity set as BuildSchemaEntities projects it: singular names from
	// the applied-schema join. "LlmKey" is the proto-Go-cased projection of
	// the same entity ExtractCRUDEntities derives as "LLMKey" from
	// ListLLMKeys — the casing divergence the slug-keyed gate must absorb.
	// (BuildSchemaEntities sorts by Name; order is irrelevant to the gate.)
	entities := []codegen.EntityDef{
		{Name: "Daemon", TableName: "daemons"},
		{Name: "LlmKey", TableName: "llm_keys"},
		{Name: "Plan", TableName: "plans"},
		{Name: "UsageEvent", TableName: "usage_events"},
		{Name: "User", TableName: "users"},
	}

	pages := buildNavPages(services, entities)

	gotSlugs := make(map[string]bool, len(pages))
	for _, p := range pages {
		gotSlugs[p.Slug] = true
	}
	wantSlugs := []string{"llm-keys", "daemons", "plans", "usage-events", "users"}
	if len(pages) != len(wantSlugs) {
		t.Fatalf("nav regenerated %d route(s), want %d (empty/partial nav = the regression): %+v",
			len(pages), len(wantSlugs), pages)
	}
	for _, s := range wantSlugs {
		if !gotSlugs[s] {
			t.Errorf("missing nav route %q — gate dropped a valid entity: got %+v", s, pages)
		}
	}
}

// TestDashboardGenTemplate_RealCountsAndCreateGating pins the dashboard
// half of F2: tiles wire the real list hook count (no static &mdash; stat
// card) and QuickActions only renders "Create X" for entities whose
// service actually has a Create RPC.
func TestDashboardGenTemplate_RealCountsAndCreateGating(t *testing.T) {
	data := templates.FrontendTemplateData{
		FrontendName: "web",
		ProjectName:  "demo",
		Pages: []templates.NavPageData{{
			Label: "Tasks", LabelLower: "tasks", LabelSingular: "Task", Slug: "tasks",
			HasCreate: true, ListHook: "useListTasks", HooksModule: "@/hooks/task-service-hooks",
			ItemsField: "tasks", ComponentIdent: "Tasks",
		}},
		NavHookImports: []templates.NavHookImport{{
			Module: "@/hooks/task-service-hooks", Symbols: []string{"useListTasks"},
		}},
	}
	rendered, err := templates.FrontendTemplates().Render(
		"nextjs/src/app/dashboard_gen.tsx.tmpl", data)
	if err != nil {
		t.Fatalf("render dashboard_gen: %v", err)
	}
	out := string(rendered)

	for _, want := range []string{
		`import { useListTasks } from "@/hooks/task-service-hooks";`,
		"const { data } = useListTasks({});",
		"const count = data?.tasks?.length;",
		`{count ?? "—"}`,
		"ALL_ROUTES.filter((route) => route.hasCreate)",
		"Create {route.labelSingular}",
		`"use client";`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dashboard_gen missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "&mdash;") {
		t.Errorf("dashboard_gen still renders the static &mdash; fake stat card:\n%s", out)
	}

	// nav_gen carries hasCreate so the dashboard can gate.
	navRendered, err := templates.FrontendTemplates().Render(
		"nextjs/src/components/nav_gen.tsx.tmpl", data)
	if err != nil {
		t.Fatalf("render nav_gen: %v", err)
	}
	if !strings.Contains(string(navRendered), `slug: "tasks", labelSingular: "Task", hasCreate: true`) {
		t.Errorf("nav_gen missing hasCreate/labelSingular route fields:\n%s", navRendered)
	}
}

// TestHooksTemplate_KeyFactory pins the F4/F5 fixes: a generated per-service
// query-key factory whose keys embed the protojson-normalized request
// (bigint-safe, type-normalized), entity-scoped invalidation for CRUD
// mutations, and whole-service fallback for non-CRUD mutations.
func TestHooksTemplate_KeyFactory(t *testing.T) {
	out := renderHooksForTest(t, false)

	for _, want := range []string{
		// Factory exists with service + entity scopes.
		"export const taskServiceKeys = {",
		`all: ["taskService"] as const,`,
		`task: ["taskService", "task"] as const,`,
		// Query keys: [service, entity, method, protojson(req)].
		`["taskService", "task", "getTask", requestKey(GetTaskRequestSchema, req)] as const,`,
		`["taskService", "task", "listTasks", requestKey(ListTasksRequestSchema, req)] as const,`,
		// Hooks consume the factory — no hand-built key literals.
		"queryKey: taskServiceKeys.getTask(req),",
		"queryKey: taskServiceKeys.listTasks(req),",
		// CRUD mutation invalidates the ENTITY scope, not the service.
		"queryClient.invalidateQueries({ queryKey: taskServiceKeys.task });",
		// Non-CRUD mutation (SendReport) falls back to the service scope.
		"queryClient.invalidateQueries({ queryKey: taskServiceKeys.all });",
		// protojson normalization helper present.
		"return toJson(schema, create(schema, req));",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered hooks missing %q:\n%s", want, out)
		}
	}

	// The hook-level error toast is gone — the MutationCache in
	// query-client.ts is the single chokepoint (F3).
	for _, banned := range []string{"getEventBus", "toast:show", "catch {"} {
		if strings.Contains(out, banned) {
			t.Errorf("rendered hooks still contain %q — toast policy must live only in query-client.ts", banned)
		}
	}
}

// TestMockTransport_MutableStoreAndNotFound pins F7: per-entity mutable
// Map stores (Create/Delete round-trip within a session), Get-miss →
// ConnectError NotFound instead of silently serving the first fixture.
func TestMockTransport_MutableStoreAndNotFound(t *testing.T) {
	entities := []codegen.MockTransportEntity{{
		EntityName:         "Task",
		EntityNamePlural:   "Tasks",
		EntitySlug:         "tasks",
		ServiceTypeName:    "demo.v1.TaskService",
		ListRPC:            "ListTasks",
		GetRPC:             "GetTask",
		CreateRPC:          "CreateTask",
		UpdateRPC:          "UpdateTask",
		DeleteRPC:          "DeleteTask",
		HasList:            true,
		HasGet:             true,
		HasCreate:          true,
		HasUpdate:          true,
		HasDelete:          true,
		ItemsField:         "tasks",
		ImportPath:         "services/tasks/v1/tasks_pb",
		EntityImportPath:   "db/v1/tasks_pb",
		SchemaImport:       "TaskSchema",
		ListResponseType:   "ListTasksResponse",
		GetResponseType:    "GetTaskResponse",
		CreateResponseType: "CreateTaskResponse",
	}}

	got := renderMockTransport(t, entities)

	for _, want := range []string{
		// mutable session store
		"const tasksStore = new Map(",
		"tasksMocks.tasks.map((e) => [String(e.id), e]),",
		// list reads the store, not the static fixture array
		"tasks: Array.from(tasksStore.values()),",
		// get-miss is a real NotFound
		"Code.NotFound",
		// create inserts; delete removes
		"tasksStore.set(String(id), created);",
		"tasksStore.delete(String(req?.id));",
		// the entity schema import comes from the ENTITY's module
		`from "@/gen/db/v1/tasks_pb"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("mock transport missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "?? all[0]") {
		t.Errorf("mock transport still falls back to all[0] on get-miss:\n%s", got)
	}
}

// TestScenarioRpcsTemplate_TypedHandlerMap pins the typed scenario handler
// map: per-RPC keys with typed request params and MessageInitShape-typed
// returns (a snake_case payload fails tsc instead of rendering blank UI).
func TestScenarioRpcsTemplate_TypedHandlerMap(t *testing.T) {
	services := []codegen.ServiceDef{{
		Name:      "TaskService",
		Package:   "demo.v1",
		ProtoFile: "proto/services/tasks/v1/tasks.proto",
		Methods: []codegen.Method{
			{Name: "GetTask", InputType: "GetTaskRequest", OutputType: "GetTaskResponse"},
			{Name: "StreamTasks", InputType: "StreamTasksRequest", OutputType: "StreamTasksResponse", ServerStreaming: true},
		},
	}}
	data := codegen.BuildScenarioRpcData(services)

	content, err := templates.FrontendTemplates().Get("mocks/scenarios/scenario-rpcs.ts.tmpl")
	if err != nil {
		t.Fatalf("read scenario-rpcs template: %v", err)
	}
	tmpl, err := template.New("scenario-rpcs.ts.tmpl").Funcs(templates.FuncMap()).Parse(string(content))
	if err != nil {
		t.Fatalf("parse scenario-rpcs template: %v", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		t.Fatalf("render scenario-rpcs: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		`"demo.v1.TaskService/GetTask"?: (req: GetTaskRequest) => UnaryReturn<MessageInitShape<typeof GetTaskResponseSchema>>;`,
		`import type { GetTaskRequest, GetTaskResponseSchema } from "@/gen/services/tasks/v1/tasks_pb";`,
		"[key: string]: ((req: never) => unknown) | undefined;",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scenario-rpcs missing %q:\n%s", want, out)
		}
	}
	// Streaming RPCs are NOT in the typed map (no canonical return shape).
	if strings.Contains(out, "StreamTasks") {
		t.Errorf("scenario-rpcs should not type streaming RPCs:\n%s", out)
	}
}

// TestHooksTemplate_MutationComposeThenSpread pins the F9 fix: in BOTH the
// workspaces and non-workspaces branches, caller-supplied options must be
// destructured (onSuccess pulled out, the REST spread into useMutation) so a
// caller-supplied onSuccess can never REPLACE the composed
// invalidation+onSuccess handler. The shipped bug was `...options` spread
// AFTER the composed onSuccess in the workspaces branch — a caller's
// onSuccess silently disabled list invalidation (stale-list-after-save).
func TestHooksTemplate_MutationComposeThenSpread(t *testing.T) {
	for _, tc := range []struct {
		name       string
		workspaces bool
	}{
		{"workspaces", true},
		{"frontend-local", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := renderHooksForTest(t, tc.workspaces)

			// The raw options object must never be spread into useMutation —
			// only the destructured rest may be. `...options` after the
			// composed onSuccess re-introduces the override bug. (Query
			// hooks spread `...options` legitimately — nothing is composed
			// there — so scope the check to useMutation call bodies.)
			mutationBlocks := strings.Split(out, "useMutation({")[1:]
			if len(mutationBlocks) == 0 {
				t.Fatalf("expected at least one useMutation call in rendered hooks:\n%s", out)
			}
			for _, block := range mutationBlocks {
				if end := strings.Index(block, "});"); end >= 0 {
					block = block[:end]
				}
				if strings.Contains(block, "...options") {
					t.Errorf("useMutation body spreads raw `...options` — caller onSuccess would replace composed invalidation:\n%s", block)
				}
			}
			// The compose-then-spread pattern: destructure first...
			if !strings.Contains(out, "const { onSuccess, ...rest } = options ?? {};") {
				t.Errorf("expected destructuring `const { onSuccess, ...rest } = options ?? {};` in mutation hooks:\n%s", out)
			}
			// ...then spread the rest.
			if !strings.Contains(out, "...rest,") {
				t.Errorf("expected `...rest,` spread in mutation hooks:\n%s", out)
			}
			// And the composed callback still calls the caller's onSuccess
			// after invalidation.
			if !strings.Contains(out, "onSuccess?.(...args);") {
				t.Errorf("expected composed onSuccess to delegate to caller's onSuccess:\n%s", out)
			}
		})
	}
}
