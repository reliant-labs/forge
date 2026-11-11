-- ReleaseProbeDown (release-probe-down) severity=critical release_health=exclude for=5m interval=1m
WITH series AS (
    SELECT ServiceName AS target,
           Attributes['probe.outcome'] AS outcome,
           Attributes['probe.path'] AS path,
           Attributes['status_class'] AS status_class,
           Attributes['error.type'] AS error_type,
           arraySort(x -> x.1, groupArray((TimeUnix, Value))) AS pts,
           arrayMap(x -> x.2, pts) AS vals,
           if(any(AggregationTemporality) = 1, vals, arrayMap((v, d) -> if(d < 0, v, d), vals, arrayDifference(vals))) AS incs
    FROM otel_metrics_sum
    WHERE MetricName = 'reliant.probe.requests'
      AND ((AggregationTemporality = 1 AND TimeUnix >= now() - INTERVAL 5 MINUTE) OR (AggregationTemporality != 1 AND TimeUnix >= now() - INTERVAL 5 MINUTE - INTERVAL 2 MINUTE))
      AND ResourceAttributes['deployment.environment.name'] = 'prod'
      AND ResourceAttributes['reliant.source'] = 'probe'
      AND ServiceName IN ('api', 'web')
      AND Attributes['probe.outcome'] IN ('ok', 'failed')
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
           sumIf(total, outcome = 'failed') AS failed,
           sumIf(total, outcome = 'ok') + failed AS samples,
           minIf(first_seen, outcome = 'failed' AND total > 0) AS since,
           arraySlice(arrayReverseSort(f -> f.3, groupArrayIf(tuple(
               'probe_failed', path, total, toFloat64(0),
               concat(path, if(status_class != '', concat(' answered ', status_class), concat(' failed: ', if(error_type != '', error_type, 'no response'))),
                      if(total = 1, ' once', concat(' ', toString(toUInt64(total)), ' times')), ', ', formatDateTime(first_seen, '%Y-%m-%dT%H:%i:%SZ', 'UTC'), ' to ', formatDateTime(last_seen, '%Y-%m-%dT%H:%i:%SZ', 'UTC'))
           ), outcome = 'failed' AND total > 0)), 1, 5) AS findings
    FROM merged
    GROUP BY target
)
SELECT target,
       round(failed / samples, 3) AS value,
       toJSONString(CAST(tuple(
           'release_probe', 'probe_failure_ratio', concat('release_probe/', target, '/-'),
           toFloat64(round(failed / samples, 3)), 'fraction',
           'failed release probes over the ok and failed ones in the window; throttled and degraded probes are neither',
           tuple(toFloat64(0.8), toFloat64(10)),
           '5m',
           formatDateTime(since, '%Y-%m-%dT%H:%i:%SZ', 'UTC'),
           tuple(findings),
           ['forge obs metrics query prod reliant.probe.requests --group-by probe.outcome --since 3h', 'forge obs check prod']
       ), 'Tuple(detector String, signal String, dedupe_key String, observed Float64, unit String, method String, thresholds Tuple(failure_ratio Float64, min_samples Float64), window String, since String, context Tuple(findings Array(Tuple(code String, ref String, value Float64, `limit` Float64, message String))), next Array(String))')) AS why
FROM judged
WHERE samples >= 10 AND failed >= 0.8 * samples
ORDER BY target

-- ReleaseProbeFailing (release-probe-failing) severity=warning release_health=exclude for=0s interval=1m
WITH series AS (
    SELECT ServiceName AS target,
           Attributes['probe.outcome'] AS outcome,
           Attributes['probe.path'] AS path,
           Attributes['status_class'] AS status_class,
           Attributes['error.type'] AS error_type,
           arraySort(x -> x.1, groupArray((TimeUnix, Value))) AS pts,
           arrayMap(x -> x.2, pts) AS vals,
           if(any(AggregationTemporality) = 1, vals, arrayMap((v, d) -> if(d < 0, v, d), vals, arrayDifference(vals))) AS incs
    FROM otel_metrics_sum
    WHERE MetricName = 'reliant.probe.requests'
      AND ((AggregationTemporality = 1 AND TimeUnix >= now() - INTERVAL 5 MINUTE) OR (AggregationTemporality != 1 AND TimeUnix >= now() - INTERVAL 5 MINUTE - INTERVAL 2 MINUTE))
      AND ResourceAttributes['deployment.environment.name'] = 'prod'
      AND ResourceAttributes['reliant.source'] = 'probe'
      AND ServiceName IN ('api', 'web')
      AND Attributes['probe.outcome'] IN ('ok', 'failed')
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
           sumIf(total, outcome = 'failed') AS failed,
           sumIf(total, outcome = 'ok') + failed AS samples,
           minIf(first_seen, outcome = 'failed' AND total > 0) AS since,
           arraySlice(arrayReverseSort(f -> f.3, groupArrayIf(tuple(
               'probe_failed', path, total, toFloat64(0),
               concat(path, if(status_class != '', concat(' answered ', status_class), concat(' failed: ', if(error_type != '', error_type, 'no response'))),
                      if(total = 1, ' once', concat(' ', toString(toUInt64(total)), ' times')), ', ', formatDateTime(first_seen, '%Y-%m-%dT%H:%i:%SZ', 'UTC'), ' to ', formatDateTime(last_seen, '%Y-%m-%dT%H:%i:%SZ', 'UTC'))
           ), outcome = 'failed' AND total > 0)), 1, 5) AS findings
    FROM merged
    GROUP BY target
)
SELECT target,
       failed AS value,
       toJSONString(CAST(tuple(
           'release_probe', 'probe_failures', concat('release_probe/', target, '/-'),
           toFloat64(failed), 'probes',
           'failed release probes in the window, as the prober recorded them; throttled and degraded probes never count',
           tuple(toFloat64(2)),
           '5m',
           formatDateTime(since, '%Y-%m-%dT%H:%i:%SZ', 'UTC'),
           tuple(findings),
           ['forge obs metrics query prod reliant.probe.requests --group-by probe.outcome --since 3h', 'forge obs check prod']
       ), 'Tuple(detector String, signal String, dedupe_key String, observed Float64, unit String, method String, thresholds Tuple(failures Float64), window String, since String, context Tuple(findings Array(Tuple(code String, ref String, value Float64, `limit` Float64, message String))), next Array(String))')) AS why
FROM judged
WHERE failed >= 2
ORDER BY target

