---
name: external-deploy-recipes
description: Copy-paste KCL recipes for the most common External deploy targets — Fly.io, Cloud Run, Cloudflare Workers, AWS ECS, Lambda, Vercel, Railway, systemd-on-VM. One block per provider plus the auth env-vars each one needs.
---

# External deploy recipes

`forge.External` is the generic shell-command deploy target. The
provider exec's `deploy_cmd` via `sh -c` after substituting:

| Token            | Meaning                                              |
|------------------|------------------------------------------------------|
| `${IMAGE}`       | service image (from `Service.image`)                 |
| `${TAG}`         | image tag forge resolved (build-state or `--tag`)    |
| `${LAST_TAG}`    | previous good tag (rollback only — empty on deploy)  |
| `${SERVICE}`     | `Service.name`                                       |
| `${ENV}`         | env name (e.g. `dev`, `staging`, `prod`)             |
| `${ENV_FILE}`    | the path declared in `env_file` (if any)             |
| `${PROJECT_DIR}` | absolute project root on the deploy machine          |
| `${YOURS}`       | any key you declare in the `env` map                 |

`rollback_cmd` is optional. When unset, `forge deploy --rollback` errors
loudly — forge can't synthesise a rollback for an arbitrary CLI. Set it
explicitly (or skip rollback for that service).

`health_cmd` runs after a successful deploy. A failing health check
short-circuits before the state-file write so the recorded last-good
tag stays clean.

## Fly.io (`flyctl`)

```kcl
forge.Service {
    name = "edge"
    image = "registry.fly.io/edge-prod"
    deploy = forge.External {
        deploy_cmd = r"flyctl deploy --image ${IMAGE}:${TAG} --app edge-prod"
        rollback_cmd = r"flyctl deploy --image ${IMAGE}:${LAST_TAG} --app edge-prod"
        health_cmd = "flyctl status --app edge-prod | grep -q 'running'"
    }
}
```

Auth: `FLY_API_TOKEN` in the deploy environment (`flyctl auth token`
locally; set as a CI secret for deploy bots).

## GCP Cloud Run (`gcloud run`)

```kcl
forge.Service {
    name = "api"
    image = "gcr.io/myproj/api"
    deploy = forge.External {
        deploy_cmd = r"gcloud run deploy ${SERVICE} --image ${IMAGE}:${TAG} --region us-central1 --platform managed"
        rollback_cmd = r"gcloud run deploy ${SERVICE} --image ${IMAGE}:${LAST_TAG} --region us-central1 --platform managed"
        health_cmd = r"gcloud run services describe ${SERVICE} --region us-central1 --format='value(status.latestReadyRevisionName)' | grep -q ."
    }
}
```

Auth: `gcloud auth activate-service-account --key-file=...` in CI, or
`gcloud auth login` locally. Set `CLOUDSDK_CORE_PROJECT` (or pass
`--project`) so the command isn't ambiguous.

## Cloudflare Workers (`wrangler`)

```kcl
forge.Service {
    name = "edge-worker"
    deploy = forge.External {
        deploy_cmd = r"wrangler deploy --name ${SERVICE} --env ${ENV}"
        rollback_cmd = r"wrangler rollback --name ${SERVICE} --message 'forge rollback to ${LAST_TAG}'"
        health_cmd = r"curl -fsS https://${SERVICE}.workers.dev/health"
    }
}
```

Auth: `CLOUDFLARE_API_TOKEN` (and `CLOUDFLARE_ACCOUNT_ID`). Workers
don't take a container image — `${IMAGE}` is unused; the worker code
lives in `wrangler.toml`. The tag here is just record-keeping.

## AWS ECS (Fargate, `ecs-deploy` style)

```kcl
forge.Service {
    name = "api"
    image = "123456789.dkr.ecr.us-east-1.amazonaws.com/api"
    deploy = forge.External {
        deploy_cmd = r"aws ecs update-service --cluster prod --service ${SERVICE} --force-new-deployment --task-definition $(aws ecs register-task-definition --family ${SERVICE} --container-definitions '[{\"name\":\"app\",\"image\":\"${IMAGE}:${TAG}\"}]' --query 'taskDefinition.taskDefinitionArn' --output text)"
        rollback_cmd = r"aws ecs update-service --cluster prod --service ${SERVICE} --task-definition ${SERVICE}:${LAST_TAG}"
        health_cmd = r"aws ecs wait services-stable --cluster prod --services ${SERVICE}"
    }
}
```

Auth: standard AWS env vars (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`,
`AWS_REGION`) or an IAM role on the deploy host. The
register-task-definition command in `deploy_cmd` is a one-liner;
production codebases usually move it into a `./scripts/ecs-deploy.sh`
shim and reference that script from `deploy_cmd`.

## AWS Lambda (`aws lambda`)

```kcl
forge.Service {
    name = "ingest"
    image = "123456789.dkr.ecr.us-east-1.amazonaws.com/ingest"
    deploy = forge.External {
        deploy_cmd = r"aws lambda update-function-code --function-name ${SERVICE} --image-uri ${IMAGE}:${TAG} && aws lambda wait function-updated --function-name ${SERVICE}"
        rollback_cmd = r"aws lambda update-function-code --function-name ${SERVICE} --image-uri ${IMAGE}:${LAST_TAG}"
        health_cmd = r"aws lambda invoke --function-name ${SERVICE} --payload '{\"healthcheck\":true}' /tmp/${SERVICE}-health.json && grep -q '\"ok\":true' /tmp/${SERVICE}-health.json"
    }
}
```

Auth: same AWS env vars as ECS. The health check assumes your Lambda
honours a `healthcheck: true` payload and returns `{"ok":true}` — adjust
to match your shape.

## Vercel (`vercel`)

```kcl
forge.Service {
    name = "web"
    deploy = forge.External {
        deploy_cmd = r"vercel deploy --prod --yes --token ${VERCEL_TOKEN}"
        rollback_cmd = r"vercel rollback ${LAST_TAG} --token ${VERCEL_TOKEN}"
        health_cmd = r"curl -fsS https://${SERVICE}.vercel.app/api/health"
        env = {"VERCEL_TOKEN" = "ignored — overridden by CI secret"}
    }
}
```

Auth: `VERCEL_TOKEN` (set in CI; the value in `env` is a placeholder so
the substitution doesn't break — your CI overrides it as a real env var
before invoking forge). Vercel is git-driven so the image fields are
informational only.

## Railway (`railway`)

```kcl
forge.Service {
    name = "api"
    deploy = forge.External {
        deploy_cmd = r"railway up --service ${SERVICE} --environment ${ENV}"
        rollback_cmd = r"railway rollback --service ${SERVICE} --to ${LAST_TAG}"
        health_cmd = r"railway status --service ${SERVICE} | grep -q 'Running'"
    }
}
```

Auth: `RAILWAY_TOKEN` (project-scoped). Like Vercel, Railway is
git-driven for most projects, so the recorded tag is metadata for the
forge state file rather than a real image reference.

## systemd VM (rsync + systemctl)

For the legacy "non-docker VM" deploy — ship a binary to a remote
host and restart a systemd unit. This is what `VMDocker` used to
cover, written as an External.

```kcl
forge.Service {
    name = "edge"
    image = "edge"  # unused — informational
    deploy = forge.External {
        deploy_cmd = r"rsync -avz ./bin/${SERVICE} ${SSH_HOST}:/usr/local/bin/${SERVICE}.next && ssh ${SSH_HOST} 'mv /usr/local/bin/${SERVICE}.next /usr/local/bin/${SERVICE} && systemctl restart ${SERVICE}'"
        rollback_cmd = r"ssh ${SSH_HOST} 'systemctl rollback ${SERVICE}'"
        health_cmd = r"ssh ${SSH_HOST} 'systemctl is-active ${SERVICE}'"
        env = {"SSH_HOST" = "ubuntu@edge-prod.example.com"}
    }
}
```

Auth: SSH key in `~/.ssh/` (forge inherits the deploy user's ssh-agent).
The `env` map's `SSH_HOST` is checked-in config — secrets stay in the
SSH key itself, not in KCL.

## How rollback actually works

1. Deploy success writes `.forge/state/external-<env>-<service>.json`
   with `{image, tag, deployed_at}`.
2. Next deploy success overwrites that file with the new tag.
3. `forge deploy --rollback` reads the file, substitutes the recorded
   tag into `${LAST_TAG}`, and runs `rollback_cmd`.
4. If the state file is missing AND the dispatcher's fallback tag is
   empty, rollback errors loudly — forge won't guess.

Implication: the first `--rollback` after a project's first-ever
deploy is a no-op (no previous tag yet). Subsequent rollbacks work.

## Picking a recipe

| If your target is… | Use… |
|--------------------|------|
| In a Kubernetes cluster you control | `forge.K8sCluster`, not External |
| On your laptop for `forge up` | `forge.HostDeploy`, not External |
| A binary you ship without scheduling | `forge.BuildOnly`, not External |
| Anything driven by a CLI | `forge.External` (this skill) |
| Docker-compose on a remote host | `forge.Compose` (cleaner than scripting it via External) |

The general rule: if the deploy boils down to "run this one CLI
command and check it succeeded," External fits. If it fans out into a
multi-step CI pipeline with conditional logic, write that pipeline as
its own script and reference the script from External — keep
`deploy_cmd` as one line where you can.
