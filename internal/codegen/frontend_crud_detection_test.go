package codegen

import (
	"slices"
	"testing"
)

// jobsServiceWithCustomRPCs is the roofers jobs service reduced to the rpc
// names an author reaches for when a state machine owns a column: a CRUD
// quintet for Job next to custom rpcs whose names START with a CRUD verb.
//
// The server-side projection (BuildSchemaEntities → MatchCRUDMethods) has
// always treated those as custom: "JobStatus", "InvoiceFromEstimate" and
// "JobsByCrew" name no table and no wire message. The frontend projections
// used to split the name on the verb alone, so the same proto produced
// hooks and pages that disagreed with the server about what is CRUD.
func jobsServiceWithCustomRPCs() ServiceDef {
	return ServiceDef{
		Name:      "JobsService",
		Package:   "services.jobs.v1",
		ProtoFile: "proto/services/jobs/v1/jobs.proto",
		Methods: []Method{
			{Name: "ListJobs", InputType: "ListJobsRequest", OutputType: "ListJobsResponse"},
			{Name: "GetJob", InputType: "GetJobRequest", OutputType: "GetJobResponse"},
			{Name: "CreateJob", InputType: "CreateJobRequest", OutputType: "CreateJobResponse"},
			{Name: "UpdateJob", InputType: "UpdateJobRequest", OutputType: "UpdateJobResponse"},
			{Name: "DeleteJob", InputType: "DeleteJobRequest", OutputType: "DeleteJobResponse"},
			{Name: "UpdateJobStatus", InputType: "UpdateJobStatusRequest", OutputType: "UpdateJobStatusResponse"},
			{Name: "CreateInvoiceFromEstimate", InputType: "CreateInvoiceFromEstimateRequest", OutputType: "CreateInvoiceFromEstimateResponse"},
			{Name: "ListJobsByCrew", InputType: "ListJobsByCrewRequest", OutputType: "ListJobsByCrewResponse"},
			{Name: "GetJobSummary", InputType: "GetJobSummaryRequest", OutputType: "GetJobSummaryResponse"},
		},
		Schemas: map[string][]SchemaFieldDef{
			"services.jobs.v1.Job": {
				{Name: "id", Kind: "string"},
				{Name: "status", Kind: "enum", TypeName: "services.jobs.v1.JobStatus"},
			},
			"services.jobs.v1.GetJobResponse":         {{Name: "job", Kind: "message", TypeName: "services.jobs.v1.Job"}},
			"services.jobs.v1.ListJobsResponse":       {{Name: "jobs", Kind: "message", TypeName: "services.jobs.v1.Job", Repeated: true}},
			"services.jobs.v1.UpdateJobStatusRequest": {{Name: "job_id", Kind: "string"}, {Name: "status", Kind: "enum", TypeName: "services.jobs.v1.JobStatus"}},
			"services.jobs.v1.ListJobsByCrewResponse": {{Name: "jobs", Kind: "message", TypeName: "services.jobs.v1.Job", Repeated: true}},
			"services.jobs.v1.GetJobSummaryResponse":  {{Name: "open_count", Kind: "int64"}},
		},
	}
}

// TestHookEntityScope_CustomRPCWithCRUDVerbInvalidatesWholeService pins the
// invalidation a custom mutation gets.
//
// The defect: `useUpdateJobStatus` invalidated `jobsServiceKeys.jobStatus` —
// a scope no query is keyed under, because no JobStatus entity exists. The
// Job list and detail the mutation just changed stayed cached, so the screen
// showed the old status until a hard refresh. A custom mutation may touch
// anything; the whole-service scope is the safe answer, and it is what
// `ChangeJobStatus` already got.
func TestHookEntityScope_CustomRPCWithCRUDVerbInvalidatesWholeService(t *testing.T) {
	data := ServiceDefToHookData(jobsServiceWithCustomRPCs())

	want := map[string]string{
		"ListJobs":                  "job",
		"GetJob":                    "job",
		"CreateJob":                 "job",
		"UpdateJob":                 "job",
		"DeleteJob":                 "job",
		"UpdateJobStatus":           "",
		"CreateInvoiceFromEstimate": "",
		"ListJobsByCrew":            "",
		"GetJobSummary":             "",
	}
	for _, m := range data.Methods {
		if got := m.EntityScope; got != want[m.Name] {
			t.Errorf("%s: EntityScope = %q, want %q", m.Name, got, want[m.Name])
		}
	}
	if !slices.Equal(data.EntityScopes, []string{"job"}) {
		t.Errorf("EntityScopes = %v, want [job] — a scope with no entity behind it is a key no mutation ever reaches", data.EntityScopes)
	}
}

// TestExtractCRUDEntities_CustomListRPCBirthsNoPage pins the page set: a
// custom `ListJobsByCrew` used to birth a `jobs-by-crews` list page, nav
// entry and mock fixture for an entity that does not exist.
func TestExtractCRUDEntities_CustomListRPCBirthsNoPage(t *testing.T) {
	var got []string
	for _, p := range ExtractCRUDEntities(jobsServiceWithCustomRPCs()) {
		got = append(got, p.EntityName)
	}
	if !slices.Equal(got, []string{"Job"}) {
		t.Errorf("page entities = %v, want [Job]", got)
	}
}

// TestCRUDEntity_FollowsTheServerRule spells the rule out case by case. It is
// the same rule BuildSchemaEntities applies (minus the table half, which the
// frontend projections cannot see): a CRUD verb, then a name the service
// declares as a wire message.
func TestCRUDEntity_FollowsTheServerRule(t *testing.T) {
	svc := jobsServiceWithCustomRPCs()
	cases := []struct {
		method, op, entity string
	}{
		{"ListJobs", "list", "Job"},
		{"GetJob", "get", "Job"},
		{"UpdateJob", "update", "Job"},
		{"UpdateJobStatus", "", ""},           // JobStatus is an enum, not a message
		{"CreateInvoiceFromEstimate", "", ""}, // no InvoiceFromEstimate message
		{"ListJobsByCrew", "", ""},            // no JobsByCrew message
		{"ChangeJobStatus", "", ""},           // no CRUD verb at all
	}
	for _, c := range cases {
		op, entity := crudEntity(svc, c.method)
		if op != c.op || entity != c.entity {
			t.Errorf("crudEntity(%s) = (%q, %q), want (%q, %q)", c.method, op, entity, c.op, c.entity)
		}
	}

	// A descriptor with no message inventory (written before Schemas
	// existed) keeps the name-only answer, as declaresWireMessage does: a
	// gate with no evidence must not drop every entity in the project.
	legacy := ServiceDef{Name: "JobsService", Methods: svc.Methods}
	if op, entity := crudEntity(legacy, "UpdateJob"); op != "update" || entity != "Job" {
		t.Errorf("legacy descriptor: crudEntity(UpdateJob) = (%q, %q), want (update, Job)", op, entity)
	}
}
