package obsdash

import (
	"fmt"
	"sort"
	"strings"
)

// JobsID is the id of the one dashboard every job and cron job shares, so its
// file is "<project>-jobs_gen.json".
const JobsID = "jobs"

// maxSeriesPerJobsPanel is the spec's series bound per panel; more jobs than
// this spread over several panels.
const maxSeriesPerJobsPanel = 10

// maxReplaces is dashspec.MaxReplaces.
const maxReplaces = 32

// The Job controller's event reasons (see obsquery.Workloads.Jobs).
const (
	jobEventCreated   = "SuccessfulCreate"
	jobEventCompleted = "Completed"
)

var jobFailureReasons = []string{"BackoffLimitExceeded", "DeadlineExceeded", "PodFailurePolicy"}

// jobRunSuffix is what follows a job's forge name in the name of one run: a
// standalone Job's spec hash (10 hex), or a CronJob's scheduled-time stamp.
const jobRunSuffix = `-[a-z0-9]{6,12}`

// fieldObjectKind is the kind of the object a Kubernetes event is about.
const fieldObjectKind = "resource.attr.k8s.object.kind"

// maxPredicateRegex is the longest regex the spec accepts.
const maxPredicateRegex = 256

// jobRunScope selects the events of the named jobs' runs: Job objects whose
// name is a forge job name plus a run suffix. When the joined names would be
// longer than a spec regex may be, it selects every Job object of the
// environment instead (a project's namespace holds only its own jobs).
func jobRunScope(names []string) []Predicate {
	scope := []Predicate{{Field: fieldObjectKind, Op: OpEq, Value: "Job"}}
	re := "^(" + strings.Join(quoted(names), "|") + ")" + jobRunSuffix + "$"
	if len(re) <= maxPredicateRegex {
		scope = append(scope, Predicate{Field: fieldWorkload, Op: OpMatch, Value: re})
	}
	return scope
}

// jobRunRegex matches the Kubernetes Job object names of one forge job's runs.
func jobRunRegex(name string) string {
	return "^" + regexpQuote(name) + jobRunSuffix + "$"
}

func regexpQuote(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\.+*?()|[]{}^$`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// BuildJobs is the "Jobs" dashboard: every job and cron job of the project in
// one place, instead of one dashboard per job that is empty between runs.
//
// A run is read from the Job controller's Kubernetes events, so it needs the
// cluster collector and a deployed environment; a local forge env up draws it
// empty. Jobs are told apart by their FORGE workload name: a Job object is
// named "<workload>-<hash>" (or "<workload>-<scheduled time>" for a cron), and
// this dashboard maps every run back to the workload that declared it.
//
// jobs must be non-empty; Build only calls it when the project has some.
func BuildJobs(project string, jobs []Process) Dashboard {
	names := jobNames(jobs)
	d := Dashboard{
		ID:    JobsID,
		Title: project + ": jobs",
		Description: "Every job and cron job of the project: runs, failures, duration and last run per job, from the Kubernetes Job events, " +
			"plus container restarts, warning events and error logs. Present in deployed environments only.",
		Group:   GroupJobs,
		Tags:    []string{"jobs"},
		Filters: commonFilters(),
	}
	for i := range d.Filters {
		d.Filters[i].Signal = Logs
	}
	d.Sections = []Section{{ID: "runs", Title: "Runs"}, {ID: "health", Title: "Health"}, {ID: "logs", Title: "Logs"}}
	d.Replaces = jobReplaces(jobs)

	runScope := jobRunScope(names)
	reasonIs := func(reasons ...string) Predicate {
		return Predicate{Field: fieldEventWhy, Op: OpIn, Values: reasons}
	}
	allReasons := append([]string{jobEventCreated, jobEventCompleted}, jobFailureReasons...)

	d.Panels = append(d.Panels,
		Panel{ID: "jobs-summary", Section: "runs", Title: "Runs per job",
			Description: "Per job over the selected range: runs started, succeeded and failed, the latest run's start, and run durations (seconds, start to completion). One row per forge job.",
			Kind:        KindSQL, SQL: jobsSummarySQL(names), Width: 24, Height: 8},
		Panel{ID: "runs-started", Section: "runs", Title: "Runs started", Kind: KindNumber, Signal: Logs,
			Where: scoped(runScope, reasonIs(jobEventCreated)), Series: []Series{{Label: "started", Agg: AggCount}}, Unit: UnitCount, Width: 8, Height: 4},
		Panel{ID: "runs-succeeded", Section: "runs", Title: "Runs succeeded", Kind: KindNumber, Signal: Logs,
			Where: scoped(runScope, reasonIs(jobEventCompleted)), Series: []Series{{Label: "succeeded", Agg: AggCount}}, Unit: UnitCount, Width: 8, Height: 4},
		Panel{ID: "runs-failed", Section: "runs", Title: "Runs failed", Kind: KindNumber, Signal: Logs,
			Where: scoped(runScope, reasonIs(jobFailureReasons...)), Series: []Series{{Label: "failed", Agg: AggCount}}, Unit: UnitCount, Width: 8, Height: 4},
	)
	d.Panels = append(d.Panels, perJobLines("runs-by-job", "Runs started by job", names, scoped(runScope, reasonIs(jobEventCreated)))...)
	d.Panels = append(d.Panels, perJobLines("failures-by-job", "Failed runs by job", names, scoped(runScope, reasonIs(jobFailureReasons...)))...)
	d.Panels = append(d.Panels,
		Panel{ID: "run-events", Section: "runs", Title: "Run events", Description: "Job create, completion and failure events, newest first.",
			Kind: KindSearch, Signal: Logs, Where: scoped(runScope, reasonIs(allReasons...)),
			Columns: []string{"timestamp", fieldWorkload, fieldEventWhy, "log.body"}, Width: 24, Height: 8},
	)

	workloads := jobWorkloads(jobs)
	byWorkload := []string{fieldWorkload}
	for _, p := range BuildWorkloads(project).Panels {
		switch p.ID {
		case "restarts":
			p.Section = "health"
			p.Where = scoped(identityScope(fieldWorkload, workloads), p.Where...)
			d.Panels = append(d.Panels, p)
		}
	}
	d.Panels = append(d.Panels,
		Panel{ID: "warning-events", Section: "health", Title: "Warning events by job",
			Description: "Warning-type Kubernetes events of the jobs' pods: failed image pulls, evictions, failed scheduling, back-offs.",
			Kind:        KindLine, Signal: Logs,
			Where:   scoped(identityScope(fieldWorkload, workloads), Predicate{Field: fieldEventType, Op: OpEq, Value: "Warning"}),
			Series:  []Series{{Label: "warnings", Agg: AggCount}},
			GroupBy: byWorkload, Unit: UnitCount, Width: 24, Height: 7},
		Panel{ID: "error-logs", Section: "logs", Title: "Error logs", Kind: KindSearch, Signal: Logs,
			Where: scoped(identityScope(FieldService, jobServiceNames(jobs)), errorLogs), Columns: logColumns, Width: 24, Height: 8},
	)
	return d
}

// perJobLines is one line panel per ten jobs, one series per job, each series
// counting the events of that job's runs.
func perJobLines(id, title string, names []string, where []Predicate) []Panel {
	var out []Panel
	chunks := (len(names) + maxSeriesPerJobsPanel - 1) / maxSeriesPerJobsPanel
	for c := 0; c < chunks; c++ {
		part := names[c*maxSeriesPerJobsPanel : min(len(names), (c+1)*maxSeriesPerJobsPanel)]
		p := Panel{ID: id, Section: "runs", Title: title, Kind: KindLine, Signal: Logs, Where: where, Unit: UnitCount, Width: 12, Height: 7}
		if chunks > 1 {
			p.ID = fmt.Sprintf("%s-%d", id, c+1)
			p.Title = fmt.Sprintf("%s (%d of %d)", title, c+1, chunks)
		}
		for _, n := range part {
			p.Series = append(p.Series, Series{Label: n, Agg: AggCount, Where: []Predicate{{Field: fieldWorkload, Op: OpMatch, Value: jobRunRegex(n)}}})
		}
		out = append(out, p)
	}
	return out
}

// jobsSummarySQL is the per-job table: one row per forge job, from the run
// events. An EXECUTION is one Kubernetes Job in one environment: the inner
// query groups by (reliant.env.id, Job name), because identical specs deployed
// to two environments share a hashed Job name, and a cron's scheduled-time
// stamp can coincide. Collapsing them would report a run that both succeeded
// and failed, with a duration assembled across environments. The names are forge workload names (DNS labels), embedded as
// literals; the time range and environments are the dashboard's own query
// parameters.
func jobsSummarySQL(names []string) string {
	list := "['" + strings.Join(names, "', '") + "']"
	return fmt.Sprintf(`SELECT job,
       count() AS runs,
       countIf(finished > 0) AS succeeded,
       countIf(failed) AS failed,
       max(started) AS last_started,
       argMax(if(finished > 0 AND started > 0, dateDiff('second', started, finished), NULL), started) AS last_duration_s,
       if(countIf(finished > 0 AND started > 0) = 0, NULL, round(avgIf(dateDiff('second', started, finished), finished > 0 AND started > 0), 1)) AS avg_duration_s,
       if(countIf(finished > 0 AND started > 0) = 0, NULL, maxIf(dateDiff('second', started, finished), finished > 0 AND started > 0)) AS max_duration_s
FROM (
  SELECT env, run,
         arrayFirst(n -> match(run, concat('^', n, '%s$')), %s) AS job,
         minIf(Timestamp, reason = '%s') AS started,
         maxIf(Timestamp, reason = '%s') AS finished,
         countIf(reason IN ('%s')) > 0 AS failed
  FROM (
    SELECT Timestamp, ResourceAttributes['reliant.env.id'] AS env,
           ResourceAttributes['k8s.workload.name'] AS run, LogAttributes['k8s.event.reason'] AS reason
    FROM otel_logs
    WHERE Timestamp >= {from:DateTime64(9)} AND Timestamp < {to:DateTime64(9)}
      AND LogAttributes['k8s.event.reason'] != ''
      AND if(mapContains(LogAttributes, 'k8s.object.kind'), LogAttributes['k8s.object.kind'], ResourceAttributes['k8s.object.kind']) = 'Job'
      AND (empty({env_ids:Array(String)}) OR has({env_ids:Array(String)}, ResourceAttributes['reliant.env.id']))
  )
  GROUP BY env, run
)
WHERE job != ''
GROUP BY job
ORDER BY last_started DESC, job
`, jobRunSuffix, list, jobEventCreated, jobEventCompleted, strings.Join(jobFailureReasons, "', '"))
}

func jobNames(jobs []Process) []string {
	var out []string
	for _, p := range jobs {
		out = append(out, p.Workloads...)
	}
	return sortedIdentities(out)
}

func jobWorkloads(jobs []Process) []string { return jobNames(jobs) }

func jobServiceNames(jobs []Process) []string {
	var out []string
	for _, p := range jobs {
		out = append(out, p.ServiceNames...)
	}
	return sortedIdentities(out)
}

func quoted(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = regexpQuote(n)
	}
	sort.Strings(out)
	return out
}

// jobReplaces are the ids of the per-job dashboards this one replaces: each
// job's own "process-<name>" and the id it was generated under before
// dashboards were named for the workload. A reader moves stars and copies of
// any of them here. At most MaxReplaces (the spec's bound) are named; the
// newest names come first so the cap drops the oldest.
func jobReplaces(jobs []Process) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range jobs {
		for _, id := range append([]string{ProcessDashboardID(p.Name)}, prefixed("process-", p.Replaces)...) {
			if id != JobsID && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	sort.Strings(out)
	if len(out) > maxReplaces {
		out = out[:maxReplaces]
	}
	return out
}

func prefixed(prefix string, names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = prefix + n
	}
	return out
}
