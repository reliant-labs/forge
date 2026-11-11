package obsdash

import (
	"fmt"
	"strings"

	"github.com/reliant-labs/forge/pkg/alerting"
	"github.com/reliant-labs/forge/pkg/release"
	"github.com/reliant-labs/forge/pkg/releasehealth"
)

// ── release probes ───────────────────────────────────────────────────────────
//
// The control plane's release prober requests each target of the env's
// release probes (the release shape's release_probes) a few times a minute and
// writes what happened into the env's telemetry: release.ProbeMetricRequests,
// a monotonic DELTA sum with one series per target (service.name), path,
// outcome, status class and error type, stamped reliant.source = probe by the
// gateway from its token. Both defaults read the env's own targets only
// (AlertInput.ReleaseProbeTargets), so a target the env does not probe can
// never alert.
//
// Only two outcomes are samples. `failed` is a status the probe does not
// expect, or no response after the prober's retry. `ok` is the rest that
// answered. `throttled` (a 429) is neither: the prober backs off and the
// target is not judged. `degraded` is a failure recorded while the prober's
// own canary was failing, so it is the platform's fault as likely as the
// target's, and never the tenant's alert; the platform watches its canary.
//
// Both carry release_health=exclude: the release-health verdict already
// judges probe failures (probe_failed), so these alerts must not count again
// as alert_firing, nor hold a later release as a standing critical. The two
// overlap on purpose: a target going down raises ReleaseProbeFailing at once
// and ReleaseProbeDown five minutes on, and the warning stays up meanwhile.

const pFailureRatio = "failure_ratio"

var releaseProbeLabels = map[string]string{releasehealth.LabelReleaseHealth: releasehealth.ReleaseHealthExclude}

func hasReleaseProbes(in AlertInput) bool { return len(in.ReleaseProbeTargets) > 0 }

func releaseProbeTargetList(in AlertInput) string {
	q := make([]string, len(in.ReleaseProbeTargets))
	for i, t := range in.ReleaseProbeTargets {
		q[i] = sqlQuote(t)
	}
	return strings.Join(q, ", ")
}

// releaseProbeJudged is the scan both defaults share: per target, the window's
// failed and ok probes, when the first failure in it was, and its failures as
// why-payload findings (one per path, status class and error type, most
// first, at most five). A finding is (code, ref, value, limit, message).
//
// Series are grouped by their full attribute set, so two prober replicas never
// subtract each other; a cumulative series (not what the prober writes, but
// what an SDK may) is differenced as the rpc defaults do.
func releaseProbeJudged(env string, in AlertInput, window float64) string {
	const ts = `'%Y-%m-%dT%H:%i:%SZ', 'UTC'`
	return fmt.Sprintf(`WITH series AS (
    SELECT ServiceName AS target,
           Attributes['%[1]s'] AS outcome,
           Attributes['%[2]s'] AS path,
           Attributes['%[3]s'] AS status_class,
           Attributes['%[4]s'] AS error_type,
           arraySort(x -> x.1, groupArray((TimeUnix, Value))) AS pts,
           arrayMap(x -> x.2, pts) AS vals,
           if(any(AggregationTemporality) = 1, vals, arrayMap((v, d) -> if(d < 0, v, d), vals, arrayDifference(vals))) AS incs
    FROM otel_metrics_sum
    WHERE MetricName = '%[5]s'
      AND %[6]s
      AND ResourceAttributes['deployment.environment.name'] = %[7]s
      AND ResourceAttributes['reliant.source'] = '%[8]s'
      AND ServiceName IN (%[9]s)
      AND Attributes['%[1]s'] IN ('%[10]s', '%[11]s')
    GROUP BY ServiceName, ResourceAttributes, Attributes
), counted AS (
    SELECT target, outcome, path, status_class, error_type,
           arraySum(incs) AS n,
           arrayFilter((p, i) -> i > 0, pts, incs) AS moved,
           moved[1].1 AS first_at,
           moved[-1].1 AS last_at
    FROM series
), merged AS (
    SELECT target, outcome, path, status_class, error_type,
           sum(n) AS total,
           minIf(first_at, n > 0) AS first_seen,
           maxIf(last_at, n > 0) AS last_seen
    FROM counted
    GROUP BY target, outcome, path, status_class, error_type
), judged AS (
    SELECT target,
           sumIf(total, outcome = '%[11]s') AS failed,
           sumIf(total, outcome = '%[10]s') + failed AS samples,
           minIf(first_seen, outcome = '%[11]s' AND total > 0) AS since,
           arraySlice(arrayReverseSort(f -> f.3, groupArrayIf(tuple(
               '%[12]s', path, total, toFloat64(0),
               concat(path, if(status_class != '', concat(' answered ', status_class), concat(' failed: ', if(error_type != '', error_type, 'no response'))),
                      if(total = 1, ' once', concat(' ', toString(toUInt64(total)), ' times')), ', ', formatDateTime(first_seen, %[13]s), ' to ', formatDateTime(last_seen, %[13]s))
           ), outcome = '%[11]s' AND total > 0)), 1, 5) AS findings
    FROM merged
    GROUP BY target
)`, release.ProbeAttrOutcome, release.ProbeAttrPath, release.ProbeAttrStatusClass, release.ProbeAttrError,
		release.ProbeMetricRequests, windowFilter(window), sqlQuote(env), release.ProbeSource, releaseProbeTargetList(in),
		release.ProbeOutcomeOK, release.ProbeOutcomeFailed, release.HealthProbeFailed, ts)
}

func releaseProbeNext(env string) string {
	return fmt.Sprintf("['forge obs metrics query %[1]s %[2]s --group-by %[3]s --since 3h', 'forge obs check %[1]s']",
		env, release.ProbeMetricRequests, release.ProbeAttrOutcome)
}

// releaseProbeWhy is the why payload's tuple: signal, observed, unit, method,
// the thresholds (named in thresholdsType), the window, since and findings.
func releaseProbeWhy(env, signal, observed, unit, method, thresholds, thresholdsType string, window float64) string {
	whyType := fmt.Sprintf("Tuple(detector String, signal String, dedupe_key String, observed Float64, unit String, method String, thresholds Tuple(%s), window String, since String, context Tuple(findings %s), next Array(String))",
		thresholdsType, uptimeFindingType)
	return fmt.Sprintf(`toJSONString(CAST(tuple(
           '%s', '%s', concat('%s/', target, '/-'),
           toFloat64(%s), '%s',
           '%s',
           tuple(%s),
           '%sm',
           formatDateTime(since, '%%Y-%%m-%%dT%%H:%%i:%%SZ', 'UTC'),
           tuple(findings),
           %s
       ), '%s'))`, alerting.DetectorReleaseProbe, signal, alerting.DetectorReleaseProbe,
		observed, unit, method, thresholds, num(window), releaseProbeNext(env), whyType)
}

// releaseProbeFailingSQL fires per target with `failures` or more failed
// probes in the window (warning, ReleaseProbeFailing). `value` is the count.
func releaseProbeFailingSQL(env string, in AlertInput, p map[string]float64) string {
	return fmt.Sprintf(`%s
SELECT target,
       failed AS value,
       %s AS why
FROM judged
WHERE failed >= %s
ORDER BY target`,
		releaseProbeJudged(env, in, p[pWindow]),
		releaseProbeWhy(env, "probe_failures", "failed", "probes",
			"failed release probes in the window, as the prober recorded them; throttled and degraded probes never count",
			fmt.Sprintf("toFloat64(%s)", num(p[pFailures])), pFailures+" Float64", p[pWindow]),
		num(p[pFailures]))
}

// releaseProbeDownSQL fires per target where `failure_ratio` or more of the
// window's ok and failed probes failed, over at least `min_samples` of them
// (critical, ReleaseProbeDown). `value` is the failed share.
func releaseProbeDownSQL(env string, in AlertInput, p map[string]float64) string {
	return fmt.Sprintf(`%s
SELECT target,
       round(failed / samples, 3) AS value,
       %s AS why
FROM judged
WHERE samples >= %s AND failed >= %s * samples
ORDER BY target`,
		releaseProbeJudged(env, in, p[pWindow]),
		releaseProbeWhy(env, "probe_failure_ratio", "round(failed / samples, 3)", "fraction",
			"failed release probes over the ok and failed ones in the window; throttled and degraded probes are neither",
			fmt.Sprintf("toFloat64(%s), toFloat64(%s)", num(p[pFailureRatio]), num(p[pMinSamples])),
			pFailureRatio+" Float64, "+pMinSamples+" Float64", p[pWindow]),
		num(p[pMinSamples]), num(p[pFailureRatio]))
}
