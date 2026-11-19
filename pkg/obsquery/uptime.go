package obsquery

import (
	"fmt"

	"github.com/reliant-labs/forge/pkg/release"
)

// Uptime availability and incidents over the outside-in prober's gauges
// (release.UptimeMetric*): one sample per probe of each check, written into
// the env's own telemetry, so a check is (reliant.env.id, check name).
//
// Each function returns ONE read-only SELECT in the sql-panel form
// (dashspec.KindSQL): it reads otel_metrics_gauge and is bound by
// SQLPanel.Compile like any sql panel ({from}, {to}, {step}, {env_ids}), so a
// console runs it through QueryPanel, `forge obs query` runs it as written,
// and pkg/obsbuiltin draws it as the Uptime dashboard. The prober exists only
// on hosted environments; a local stack has no samples, and every statement
// answers no rows.
//
// The definitions, said once here and in each panel's description:
//
//   - a probe passed when uptime.check.up is 1;
//   - availability is the share of the range's probes that passed;
//   - an incident opens at the first of UptimeIncidentFailures consecutive
//     failed probes and closes at the next passing probe (Sentry Uptime's
//     defaults: one or two failures in a row are noise, not an outage). One
//     still open at the end of the range has no end;
//   - a check's status at the end of the range is down while an incident is
//     open, unknown when its newest probe is older than UptimeStaleAfter (the
//     prober could not say), and up otherwise.
//
// Probes from UptimeLookback before the range are read too, so an incident
// that began just before the range is found and starts where it began. Only
// the range's own probes count towards availability.
const (
	// UptimeIncidentFailures is how many failed probes in a row open an
	// incident.
	UptimeIncidentFailures = 3
	// UptimeStaleAfter is how old a check's newest probe may be, at the end of
	// the range, before its status is unknown: above the longest interval a
	// check may declare (release.MaxUptimeInterval).
	UptimeStaleAfter = "15 MINUTE"
	// UptimeLookback is how far before the range the incident statements
	// read, to find an incident already open when it starts:
	// UptimeIncidentFailures probes at the longest interval, with room.
	UptimeLookback = "1 HOUR"
)

// uptimeEnvScope is the environments in scope: the prober stamps each sample
// with its env's reliant.env.id, as every workload's telemetry is.
const uptimeEnvScope = "(empty({env_ids:Array(String)}) OR has({env_ids:Array(String)}, ResourceAttributes['reliant.env.id']))"

// uptimeCheckLabel names a check in a result: its name, followed by its
// environment's name when the scope is not one environment, where two
// environments can each have a check of that name.
const uptimeCheckLabel = "if(length({env_ids:Array(String)}) = 1, check, concat(check, ' (', env, ')'))"

// uptimeSamples is every sample of the three gauges for the checks in scope,
// from UptimeLookback before the range to its end: one row per sample,
// in_range set for the range's own.
func uptimeSamples() string {
	return fmt.Sprintf(`samples AS (
  SELECT ResourceAttributes['reliant.env.id'] AS env_id, ResourceAttributes['deployment.environment.name'] AS env,
         Attributes['%[1]s'] AS check, MetricName AS metric, TimeUnix AS ts, Value AS value,
         Attributes['%[2]s'] AS status, Attributes['%[3]s'] AS error,
         TimeUnix >= {from:DateTime64(9)} AS in_range
  FROM otel_metrics_gauge
  WHERE MetricName IN ('%[4]s', '%[5]s', '%[6]s')
    AND TimeUnix >= {from:DateTime64(9)} - INTERVAL %[7]s AND TimeUnix < {to:DateTime64(9)}
    AND %[8]s AND Attributes['%[1]s'] != '')`,
		release.UptimeAttrCheck, release.UptimeAttrStatus, release.UptimeAttrError,
		release.UptimeMetricUp, release.UptimeMetricDuration, release.UptimeMetricCertExpiry,
		UptimeLookback, uptimeEnvScope)
}

// uptimeIncidentCTEs are the samples and every incident they hold: probes
// (the up samples), runs (each probe with its spell: the number of passing
// probes up to it, so a spell is one passing probe and the failures after
// it), spells, and incidents (env_id, env, check, started, ended, failures,
// last_error), ended NULL while the incident is open.
func uptimeIncidentCTEs() string {
	return "WITH " + uptimeSamples() + `,
probes AS (SELECT env_id, env, check, ts, value >= 1 AS up, status, error, in_range FROM samples WHERE metric = '` + release.UptimeMetricUp + `'),
runs AS (
  SELECT *, sum(up) OVER (PARTITION BY env_id, check ORDER BY ts ROWS UNBOUNDED PRECEDING) AS spell
  FROM probes),
spells AS (
  SELECT env_id, any(env) AS env, check, spell, min(ts) AS spell_start,
         countIf(NOT up) AS failures, minIf(ts, NOT up) AS down_from,
         argMaxIf(if(error != '', error, concat('HTTP ', status)), ts, NOT up) AS last_error
  FROM runs GROUP BY env_id, check, spell),
closed AS (
  SELECT *, leadInFrame(toNullable(spell_start), 1, NULL) OVER (PARTITION BY env_id, check ORDER BY spell
                                                                 ROWS BETWEEN CURRENT ROW AND 1 FOLLOWING) AS ended
  FROM spells),
incidents AS (
  SELECT env_id, env, check, down_from AS started, ended, failures, last_error
  FROM closed
  WHERE failures >= ` + fmt.Sprint(UptimeIncidentFailures) + ` AND ifNull(ended, {to:DateTime64(9)}) > {from:DateTime64(9)})`
}

// uptimeDowntime is an incident's seconds inside the range.
const uptimeDowntime = "greatest(0, dateDiff('second', greatest(started, {from:DateTime64(9)}), least(ifNull(ended, {to:DateTime64(9)}), {to:DateTime64(9)})))"

// uptimeChecksCTEs add checks to uptimeIncidentCTEs: one row per check with
// probes in the range or an incident that reaches it: its label, status at
// the end of the range, probes, failed probes, availability (a fraction),
// incidents, downtime in seconds, the open incident's start, the p50 and p95
// probe time in milliseconds, the certificate's days left and the newest
// probe.
func uptimeChecksCTEs() string {
	return uptimeIncidentCTEs() + `,
per_check AS (
  SELECT env_id, any(env) AS env, check,
         countIf(in_range) AS probes, countIf(in_range AND NOT up) AS failed,
         max(ts) AS last_probe
  FROM probes GROUP BY env_id, check),
per_incident AS (
  SELECT env_id, check, count() AS n, sum(` + uptimeDowntime + `) AS downtime,
         minIf(started, ended IS NULL) AS open_since, countIf(ended IS NULL) > 0 AS open
  FROM incidents GROUP BY env_id, check),
per_timing AS (
  SELECT env_id, check,
         quantileExactIf(0.5)(value * 1000, metric = '` + release.UptimeMetricDuration + `' AND in_range) AS p50,
         quantileExactIf(0.95)(value * 1000, metric = '` + release.UptimeMetricDuration + `' AND in_range) AS p95,
         countIf(metric = '` + release.UptimeMetricDuration + `' AND in_range) > 0 AS has_timing,
         argMaxIf(value, ts, metric = '` + release.UptimeMetricCertExpiry + `') AS cert_seconds,
         countIf(metric = '` + release.UptimeMetricCertExpiry + `') > 0 AS has_cert
  FROM samples GROUP BY env_id, check),
checks AS (
  SELECT c.env_id AS env_id, c.env AS env, c.check AS check,
         multiIf(ifNull(i.open, 0), 'down', c.last_probe < {to:DateTime64(9)} - INTERVAL ` + UptimeStaleAfter + `, 'unknown', 'up') AS state,
         c.probes AS probes, c.failed AS failed,
         if(c.probes = 0, NULL, (c.probes - c.failed) / c.probes) AS availability,
         ifNull(i.n, 0) AS incidents, ifNull(i.downtime, 0) AS downtime,
         if(ifNull(i.open, 0), i.open_since, NULL) AS down_since,
         if(t.has_timing, t.p50, NULL) AS p50, if(t.has_timing, t.p95, NULL) AS p95,
         if(t.has_cert, round(t.cert_seconds / 86400, 1), NULL) AS cert_days,
         c.last_probe AS last_probe
  FROM per_check AS c
  LEFT JOIN per_incident AS i ON i.env_id = c.env_id AND i.check = c.check
  LEFT JOIN per_timing AS t ON t.env_id = c.env_id AND t.check = c.check
  WHERE c.probes > 0 OR ifNull(i.n, 0) > 0)`
}

// UptimeChecksSQL is one row per uptime check in scope: "Check", "Status"
// (up, down or unknown at the end of the range), "Availability %" (0 to 100,
// NULL for a check with no probe in the range), "Probes", "Failed probes",
// "Incidents", "Downtime" (seconds of incident inside the range), "Down since"
// (the open incident's start, else NULL), "p50 ms" and "p95 ms" (probe time),
// "TLS days left" (NULL for a check that saw no certificate) and "Last probe".
// Down first, then unknown, then by availability, lowest first.
func UptimeChecksSQL() string {
	return uptimeChecksCTEs() + `
SELECT ` + uptimeCheckLabel + ` AS "Check", c.state AS "Status",
       round(100 * c.availability, 3) AS "Availability %", c.probes AS "Probes", c.failed AS "Failed probes",
       c.incidents AS "Incidents", c.downtime AS "Downtime",
       if(c.down_since IS NULL, NULL, toDateTime(c.down_since, 'UTC')) AS "Down since",
       round(c.p50) AS "p50 ms", round(c.p95) AS "p95 ms", c.cert_days AS "TLS days left",
       toDateTime(c.last_probe, 'UTC') AS "Last probe"
FROM checks AS c
ORDER BY indexOf(['down', 'unknown', 'up'], c.state), ifNull(c.availability, 2), "Check"
LIMIT 500`
}

// UptimeSummarySQL is one row of figures over the checks in scope:
// "Availability" (the share of the range's probes that passed, a fraction;
// NULL when there were none), "Checks", "Checks down", "Checks unknown" and
// "Downtime" (seconds of incident inside the range, summed over the checks).
// A card reads it as "All 4 checks up" or "1 down".
func UptimeSummarySQL() string {
	return uptimeChecksCTEs() + `
SELECT if(sum(probes) = 0, NULL, round(sum(probes - failed) / sum(probes), 5)) AS "Availability",
       count() AS "Checks", countIf(state = 'down') AS "Checks down", countIf(state = 'unknown') AS "Checks unknown",
       sum(downtime) AS "Downtime"
FROM checks`
}

// UptimeIncidentsSQL is every incident that reaches the range, newest first:
// "Check", "Started" (which may precede the range), "Ended" (NULL while
// open), "Duration" (seconds to its end, or to the end of the range while
// open), "Failed probes" and "Last error" (the last failed probe's error
// type, or its HTTP status).
func UptimeIncidentsSQL() string {
	return uptimeIncidentCTEs() + `
SELECT ` + uptimeCheckLabel + ` AS "Check", toDateTime(started, 'UTC') AS "Started",
       if(ended IS NULL, NULL, toDateTime(ended, 'UTC')) AS "Ended",
       dateDiff('second', started, ifNull(ended, {to:DateTime64(9)})) AS "Duration",
       failures AS "Failed probes", last_error AS "Last error"
FROM incidents
ORDER BY started DESC, "Check"
LIMIT 500`
}

// UptimeTimelineSQL is each check's availability per {step} bucket of the
// range: t, "Check" and "Availability" (the bucket's passed probes over its
// probes, a fraction). A bucket with no row had no probe: unknown, not up. A
// renderer draws one status bar per check.
func UptimeTimelineSQL() string {
	return "WITH " + uptimeSamples() + `
SELECT toStartOfInterval(ts, toIntervalSecond({step:UInt32})) AS t, ` + uptimeCheckLabel + ` AS "Check",
       round(countIf(value >= 1) / count(), 4) AS "Availability"
FROM samples
WHERE metric = '` + release.UptimeMetricUp + `' AND in_range
GROUP BY t, env, check ORDER BY t, "Check"`
}
