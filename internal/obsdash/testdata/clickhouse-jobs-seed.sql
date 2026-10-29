DROP TABLE IF EXISTS otel_logs;
CREATE TABLE otel_logs (Timestamp DateTime64(9), ResourceAttributes Map(String,String), LogAttributes Map(String,String), Body String) ENGINE = MergeTree ORDER BY Timestamp;
-- db-migrate-up run "db-migrate-up-0123456789": SAME name in prod and staging. prod succeeds in 30s; staging fails.
INSERT INTO otel_logs SELECT now() - INTERVAL 20 MINUTE, map('reliant.env.id','prod','k8s.workload.name','db-migrate-up-0123456789','k8s.object.kind','Job'), map('k8s.event.reason','SuccessfulCreate'), '';
INSERT INTO otel_logs SELECT now() - INTERVAL 20 MINUTE + INTERVAL 30 SECOND, map('reliant.env.id','prod','k8s.workload.name','db-migrate-up-0123456789','k8s.object.kind','Job'), map('k8s.event.reason','Completed'), '';
INSERT INTO otel_logs SELECT now() - INTERVAL 19 MINUTE, map('reliant.env.id','stg','k8s.workload.name','db-migrate-up-0123456789','k8s.object.kind','Job'), map('k8s.event.reason','SuccessfulCreate'), '';
INSERT INTO otel_logs SELECT now() - INTERVAL 15 MINUTE, map('reliant.env.id','stg','k8s.workload.name','db-migrate-up-0123456789','k8s.object.kind','Job'), map('k8s.event.reason','BackoffLimitExceeded'), '';
-- a cron's second run, and a pod event about the same Job name that must NOT count
INSERT INTO otel_logs SELECT now() - INTERVAL 10 MINUTE, map('reliant.env.id','prod','k8s.workload.name','cloudsql-backup-prune-29012345','k8s.object.kind','Job'), map('k8s.event.reason','SuccessfulCreate'), '';
INSERT INTO otel_logs SELECT now() - INTERVAL 10 MINUTE + INTERVAL 90 SECOND, map('reliant.env.id','prod','k8s.workload.name','cloudsql-backup-prune-29012345','k8s.object.kind','Job'), map('k8s.event.reason','Completed'), '';
INSERT INTO otel_logs SELECT now() - INTERVAL 10 MINUTE, map('reliant.env.id','prod','k8s.workload.name','cloudsql-backup-prune-29012345','k8s.object.kind','Pod'), map('k8s.event.reason','SuccessfulCreate'), '';
-- something that is not one of our jobs
INSERT INTO otel_logs SELECT now() - INTERVAL 10 MINUTE, map('reliant.env.id','prod','k8s.workload.name','other-thing-0123456789','k8s.object.kind','Job'), map('k8s.event.reason','SuccessfulCreate'), '';
