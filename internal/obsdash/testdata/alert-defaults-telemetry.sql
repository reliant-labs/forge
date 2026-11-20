-- Clickhouse7e099f39b84ea79559b3e85ea046804e63725fd1f46b37f281276aae20f86dc3TargetDown (telemetry-target-down) severity=warning service=clickhouse for=5m interval=1m
SELECT ServiceName AS service,
       ResourceAttributes['service.instance.id'] AS instance,
       argMax(Value, TimeUnix) AS value
FROM otel_metrics_gauge
WHERE MetricName = 'up'
  AND ServiceName = 'clickhouse'
  AND TimeUnix >= now() - INTERVAL 10 MINUTE
GROUP BY service, instance
HAVING value = 0
ORDER BY instance

-- Temporale4540a7c8404b1becfed2f0ca242bec6cfd6096a8d944555145aafe5eab77c69CheckFailing (telemetry-check-failing) severity=warning service=temporal for=5m interval=1m
WITH probes AS (
    SELECT Attributes['tcpcheck.endpoint'] AS check, Value = 1 AS ok
    FROM otel_metrics_gauge
    WHERE MetricName = 'tcpcheck.status'
      AND ServiceName = 'temporal'
      AND TimeUnix >= now() - INTERVAL 5 MINUTE
      AND ResourceAttributes['deployment.environment.name'] = 'prod'
    UNION ALL
    SELECT Attributes['http.url'] AS check, Value = 1 AS ok
    FROM otel_metrics_sum
    WHERE MetricName = 'httpcheck.status'
      AND Attributes['http.status_class'] = '2xx'
      AND ServiceName = 'temporal'
      AND TimeUnix >= now() - INTERVAL 5 MINUTE
      AND ResourceAttributes['deployment.environment.name'] = 'prod'
    UNION ALL
    SELECT Attributes['http.url'] AS check, false AS ok
    FROM otel_metrics_sum
    WHERE MetricName = 'httpcheck.error'
      AND ServiceName = 'temporal'
      AND TimeUnix >= now() - INTERVAL 5 MINUTE
      AND ResourceAttributes['deployment.environment.name'] = 'prod'
)
SELECT 'temporal' AS service, check, toFloat64(count()) AS value
FROM probes
WHERE check != ''
GROUP BY check
HAVING countIf(ok) = 0
ORDER BY check

-- Temporale4540a7c8404b1becfed2f0ca242bec6cfd6096a8d944555145aafe5eab77c69TargetDown (telemetry-target-down) severity=warning service=temporal for=5m interval=1m
SELECT ServiceName AS service,
       ResourceAttributes['service.instance.id'] AS instance,
       argMax(Value, TimeUnix) AS value
FROM otel_metrics_gauge
WHERE MetricName = 'up'
  AND ServiceName = 'temporal'
  AND TimeUnix >= now() - INTERVAL 10 MINUTE
  AND ResourceAttributes['deployment.environment.name'] = 'prod'
GROUP BY service, instance
HAVING value = 0
ORDER BY instance

