package obsdash

import (
	"fmt"
	"sort"
	"strings"
)

// JobsID is the id of the one dashboard every job and cron job shares, so its
// file is "<project>-jobs_gen.json".
const JobsID = "jobs"

// maxReplaces is dashspec.MaxReplaces.
const maxReplaces = 32

// The Job controller's event reasons (see obsquery.Workloads.Jobs).
const (
	jobEventCreated   = "SuccessfulCreate"
	jobEventCompleted = "Completed"
)

var jobFailureReasons = []string{"BackoffLimitExceeded", "DeadlineExceeded", "PodFailurePolicy"}

// jobRunSuffix is what follows a job's forge name in the name of one run: a
// standalone Job's spec hash (forge renders 10 hex), or a CronJob's
// scheduled-time stamp (decimal minutes since the epoch). Hex only, so a job
// "app" does not claim the runs of a job named "app-worker".
const jobRunSuffix = `-[0-9a-f]{6,12}`

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

// jobIdentityRegexes are the regexes that select the telemetry of the named
// jobs, as many as keep each within a spec regex's length.
//
// A job's k8s.workload.name is not one string. The node agent reads the pod's
// app.kubernetes.io/name label first, and forge renders it as the bare forge
// name on every Job and CronJob pod, so pod metrics (restarts) and pod events
// carry the bare name. An event about a Job OBJECT has no pod to look up and is
// named for the object: the forge name plus the run suffix (a spec hash, or a
// cron's scheduled time). A label-less pod falls through the agent's owner
// chain the same way: a CronJob's pod to the bare CronJob name, a Job's pod to
// the suffixed Job name. So each regex takes the bare name with the run suffix
// optional.
func jobIdentityRegexes(names []string) []string {
	const head, tail = "^(", ")(" + jobRunSuffix + ")?$"
	var out, cur []string
	size := len(head) + len(tail)
	flush := func() {
		if len(cur) > 0 {
			out = append(out, head+strings.Join(cur, "|")+tail)
			cur, size = nil, len(head)+len(tail)
		}
	}
	for _, n := range quoted(names) {
		if len(cur) > 0 && size+len(n)+1 > maxPredicateRegex {
			flush()
		}
		cur = append(cur, n)
		size += len(n) + 1
	}
	flush()
	return out
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

	const perRun = " A run is one Kubernetes Job in one environment, however many pods it created."
	d.Panels = append(d.Panels,
		Panel{ID: "jobs-summary", Section: "runs", Title: "Runs per job",
			Description: "Per job over the selected range: runs seen, succeeded and failed, the latest run's start, and run durations (seconds, start to completion). One row per forge job." + perRun,
			Kind:        KindSQL, SQL: jobsSummarySQL(names), Width: 24, Height: 8},
		Panel{ID: "runs-totals", Section: "runs", Title: "Runs in range",
			Description: "Runs started in the selected range, and how many of the runs seen in it succeeded and failed, across every job." + perRun,
			Kind:        KindSQL, SQL: jobsTotalsSQL(names), Width: 24, Height: 4},
		Panel{ID: "runs-by-job", Section: "runs", Title: "Runs started by job",
			Description: "Runs started over time, one line per job." + perRun,
			Kind:        KindSQL, SQL: jobsByJobSQL(names, "started", "started > 0"), Width: 12, Height: 7},
		Panel{ID: "failures-by-job", Section: "runs", Title: "Failed runs by job",
			Description: "Runs that ended in failure over time, one line per job, at the time of the failure." + perRun,
			Kind:        KindSQL, SQL: jobsByJobSQL(names, "failed_at", "failed"), Width: 12, Height: 7},
		Panel{ID: "run-events", Section: "runs", Title: "Run events", Description: "Job create, completion and failure events, newest first.",
			Kind: KindSearch, Signal: Logs, Where: scoped(runScope, reasonIs(allReasons...)),
			Columns: []string{"timestamp", fieldWorkload, fieldEventWhy, "log.body"}, Width: 24, Height: 8},
	)

	// Restarts and warning events are read by the job's identity in every form
	// it takes (see jobIdentityRegexes); a project with more jobs than one regex
	// holds gets a panel per group.
	var restarts Panel
	for _, p := range BuildWorkloads(project).Panels {
		if p.ID == "restarts" {
			restarts = p
		}
	}
	identities := jobIdentityRegexes(names)
	for i, re := range identities {
		identity := Predicate{Field: fieldWorkload, Op: OpMatch, Value: re}
		r := restarts
		r.Section = "health"
		r.Where = scoped([]Predicate{identity}, restarts.Where...)
		w := Panel{ID: "warning-events", Section: "health", Title: "Warning events by job",
			Description: "Warning-type Kubernetes events of the jobs and their pods: failed image pulls, evictions, failed scheduling, back-offs, and a run that hit its backoff limit or deadline.",
			Kind:        KindLine, Signal: Logs,
			Where:   []Predicate{identity, {Field: fieldEventType, Op: OpEq, Value: "Warning"}},
			Series:  []Series{{Label: "warnings", Agg: AggCount}},
			GroupBy: []string{fieldWorkload}, Unit: UnitCount, Width: 24, Height: 7}
		if len(identities) > 1 {
			r.ID, r.Title = fmt.Sprintf("%s-%d", r.ID, i+1), fmt.Sprintf("%s (%d of %d)", r.Title, i+1, len(identities))
			w.ID, w.Title = fmt.Sprintf("%s-%d", w.ID, i+1), fmt.Sprintf("%s (%d of %d)", w.Title, i+1, len(identities))
		}
		d.Panels = append(d.Panels, r, w)
	}
	d.Panels = append(d.Panels,
		Panel{ID: "error-logs", Section: "logs", Title: "Error logs", Kind: KindSearch, Signal: Logs,
			Where: scoped(identityScope(FieldService, jobServiceNames(jobs)), errorLogs), Columns: logColumns, Width: 24, Height: 8},
	)
	return d
}

// jobExecutionsSQL is the subquery every run count is built on: one row per
// EXECUTION of a Job. It groups events into executions instead of counting
// them, because
//
//   - the Job controller emits SuccessfulCreate once per POD (a parallel Job,
//     or a pod replaced after a failure, repeats it for one run);
//   - identical specs deployed to two environments share a hashed Job name, as
//     can a cron's scheduled-time stamp, so the environment is part of the key;
//   - forge names a standalone Job from its pod-spec hash alone, so deleting
//     and recreating an unchanged Job (or re-adding a pruned version) reuses
//     the name, and two executions share (environment, name).
//
// An execution is (environment, Job name, Job UID, generation):
//
//	uid         the involved object's UID (k8s.object.uid), which the k8s_events
//	            receiver emits as a resource attribute. The node agent drops it
//	            today (control-plane otel_agent.k, INFRA_EVENT_DROP_RES_ATTRS),
//	            so it is '' and this key is inert until the agent keeps it for
//	            Job events; when stored, it separates executions exactly, even
//	            one deleted while running that never emitted a terminal event.
//	generation  how many executions of this (environment, name, uid) began before
//	            this event: a SuccessfulCreate that directly follows a terminal
//	            event (Completed, or a failure) begins the next one. The Job
//	            controller never creates a pod after it has finished a Job, so a
//	            create after a terminal event is always a new Job. Events are
//	            ordered by time, creates before terminals on a tie (event times
//	            have one-second resolution).
//
// Only the events that bound a run are read (creates, Completed, the failure
// reasons), so a Job that only shows other events in the range is no run.
//
// Columns: env, run (the Job's name), job (the forge name it maps back to, ”
// for a Job that is not one of the project's), started, finished, failed_at
// (DateTime64; the epoch when the event was not seen) and failed (UInt8). The
// names are forge workload names (DNS labels), embedded as literals; the time
// range and environments are the dashboard's own query parameters.
func jobExecutionsSQL(names []string) string {
	list := "['" + strings.Join(names, "', '") + "']"
	fail := strings.Join(jobFailureReasons, "', '")
	terminal := "'" + jobEventCompleted + "', '" + fail + "'"
	return fmt.Sprintf(`SELECT env, run,
         arrayFirst(n -> match(run, concat('^', n, '%[1]s$')), %[2]s) AS job,
         minIf(Timestamp, reason = '%[3]s') AS started,
         maxIf(Timestamp, reason = '%[4]s') AS finished,
         maxIf(Timestamp, reason IN ('%[5]s')) AS failed_at,
         countIf(reason IN ('%[5]s')) > 0 AS failed
  FROM (
    SELECT Timestamp, env, run, uid, reason,
           sum(reason = '%[3]s' AND prev IN (%[6]s)) OVER (PARTITION BY env, run, uid ORDER BY Timestamp, ended ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS generation
    FROM (
      SELECT Timestamp, env, run, uid, reason, reason IN (%[6]s) AS ended,
             lagInFrame(reason, 1, '') OVER (PARTITION BY env, run, uid ORDER BY Timestamp, ended ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING) AS prev
      FROM (
        SELECT Timestamp, ResourceAttributes['reliant.env.id'] AS env,
               ResourceAttributes['k8s.workload.name'] AS run, ResourceAttributes['k8s.object.uid'] AS uid,
               LogAttributes['k8s.event.reason'] AS reason
        FROM otel_logs
        WHERE Timestamp >= {from:DateTime64(9)} AND Timestamp < {to:DateTime64(9)}
          AND LogAttributes['k8s.event.reason'] IN ('%[3]s', %[6]s)
          AND if(mapContains(LogAttributes, 'k8s.object.kind'), LogAttributes['k8s.object.kind'], ResourceAttributes['k8s.object.kind']) = 'Job'
          AND (empty({env_ids:Array(String)}) OR has({env_ids:Array(String)}, ResourceAttributes['reliant.env.id']))
      )
    )
  )
  GROUP BY env, run, uid, generation`, jobRunSuffix, list, jobEventCreated, jobEventCompleted, fail, terminal)
}

// jobsSummarySQL is the per-job table: one row per forge job.
//
// last_duration_s is the NEWEST run's duration, NULL while it is running or
// after it failed. argMax skips rows whose argument is NULL and would return the
// previous successful run's duration beside the newest run's start, so the
// nullable value travels in a tuple (a tuple is never NULL).
func jobsSummarySQL(names []string) string {
	return `SELECT job,
       count() AS runs,
       countIf(finished > 0) AS succeeded,
       countIf(failed) AS failed,
       max(started) AS last_started,
       tupleElement(argMax(tuple(if(finished > 0 AND started > 0, dateDiff('second', started, finished), NULL)), started), 1) AS last_duration_s,
       if(countIf(finished > 0 AND started > 0) = 0, NULL, round(avgIf(dateDiff('second', started, finished), finished > 0 AND started > 0), 1)) AS avg_duration_s,
       if(countIf(finished > 0 AND started > 0) = 0, NULL, maxIf(dateDiff('second', started, finished), finished > 0 AND started > 0)) AS max_duration_s
FROM (
  ` + jobExecutionsSQL(names) + `
)
WHERE job != ''
GROUP BY job
ORDER BY last_started DESC, job
`
}

// jobsTotalsSQL is one row for the whole project: executions created in the
// range, and executions seen in it that completed and that failed.
func jobsTotalsSQL(names []string) string {
	return `SELECT countIf(started > 0) AS runs_started,
       countIf(finished > 0) AS runs_succeeded,
       countIf(failed) AS runs_failed
FROM (
  ` + jobExecutionsSQL(names) + `
)
WHERE job != ''
`
}

// jobsByJobSQL is a time series of executions, one series per job: each
// execution counts once, in the bucket of at (its creation, or its failure),
// for the executions where cond holds.
func jobsByJobSQL(names []string, at, cond string) string {
	return `SELECT toStartOfInterval(` + at + `, toIntervalSecond({step:UInt32})) AS t, job, count() AS runs
FROM (
  ` + jobExecutionsSQL(names) + `
)
WHERE job != '' AND ` + cond + `
GROUP BY t, job
ORDER BY t, job
`
}

func jobNames(jobs []Process) []string {
	var out []string
	for _, p := range jobs {
		out = append(out, p.Workloads...)
	}
	return sortedIdentities(out)
}

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
