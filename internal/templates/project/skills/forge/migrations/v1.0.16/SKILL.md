---
name: v1.0.16
description: Upgrade a project to forge v1.0.16. A `--kind cron` worker now runs its schedule on forge/pkg/cronkit, which claims each tick in the app's Postgres (table forge_cron_ticks, created on first use) so it fires on ONE replica instead of on every replica; `const PerReplica = true` opts a worker back into running everywhere. Workers scaffolded earlier keep firing on every replica until converted. Use when internal/workers/*/worker.go imports github.com/robfig/cron/v3. Also: forge/pkg/releasehealth.ServiceHealth gained an Endpoints slice, so it is no longer comparable with ==; use when Go code imports forge/pkg/releasehealth. Also: the scaffolded login broker no longer serves public sign-up (POST /auth/register) outside development unless the config sets auth_self_signup, because a self-registered address is unverified and an app that binds invitations by email is open to account takeover; a broker scaffolded earlier mounts it everywhere and must be edited by hand (forge lint warns: forgeconv-open-signup). Use when internal/app/login_broker.go mounts RegisterPath unconditionally.
version: v1.0.16
detection: grep -rqs --include='*.go' --exclude='*_test.go' 'github.com/robfig/cron/v3' internal/workers && exit 0; grep -rqs --include='*.go' --exclude-dir=gen --exclude-dir=vendor --exclude-dir=node_modules 'forge/pkg/releasehealth"' . && exit 0; grep -qs 'mux.Handle(RegisterPath' internal/app/login_broker.go && ! grep -qs 'if broker.selfSignup' internal/app/login_broker.go && exit 0; exit 1
---

# Upgrading to v1.0.16

## 1. Cron workers fire once per tick, not once per replica

**Use when** a worker under `internal/workers/` imports `github.com/robfig/cron/v3` (it was scaffolded with `--kind cron` before v1.0.16).

### What changed

A cron worker runs inside a long-running workload, so with two replicas every
replica ran every tick: an hourly poller polled twice an hour. A newly
scaffolded cron worker runs on `forge/pkg/cronkit` instead. Before each tick
it inserts (worker name, scheduled tick time) into `forge_cron_ticks` in the
app's Postgres with `ON CONFLICT DO NOTHING`; the replica whose insert lands
runs the tick and the others skip it (a DEBUG log and
`forge_cron_ticks_total{outcome="skipped"}`). The table is bookkeeping that
cronkit creates on first use (`CREATE TABLE IF NOT EXISTS`) as the app's own
role, the one that already runs the migrations at boot. No migration to write.

Nothing changes on upgrade by itself: `worker.go` is yours, and forge never
rewrites it. An unconverted worker keeps firing on every replica.

### Decide per worker

- The job must run once per tick (a poller, a digest, a sweep that calls a
  third party): convert it, below, with `PerReplica = false`.
- The job is meant to run on every pod (an in-process cache refresh, a local
  temp-dir sweep): convert it with `PerReplica = true`, or leave it as it is.

### Convert

1. In `Deps`, add the ORM handle the ticks are claimed on (skip this when the
   project has no `db/migrations`; the worker then fires on every replica and
   says so at boot):

   ```go
   // DB is where ticks are claimed, so a tick fires once across replicas.
   // forge:optional-dep
   DB orm.Context
   ```

2. Beside `const Schedule`, declare the replica policy:

   ```go
   const PerReplica = false
   ```

3. Replace the robfig/cron scheduler: drop the `cron`, `baseCtx` and
   `baseCancel` fields, and make `Start` block in cronkit:

   ```go
   func (w *Worker) Start(ctx context.Context) error {
       return cronkit.Run(ctx, cronkit.Options{
           Name:       w.Name(),
           Schedule:   Schedule,
           PerReplica: PerReplica,
           Claims:     cronkit.Postgres(w.deps.DB),
           Logger:     w.deps.Logger,
       }, w.Run)
   }

   func (w *Worker) Stop(ctx context.Context) error { return nil }
   ```

4. Make `Run` return an error, and drop its own span and `recover`: cronkit
   runs each tick under a root span (`cron.<name>.tick`), logs the returned
   error and recovers a panic. `cronkit.TickTime(ctx)` is the scheduled time,
   the same on every replica, for keying idempotent writes.

   ```go
   func (w *Worker) Run(ctx context.Context) error { … }
   ```

   Update `worker_test.go` where it calls `w.Run(ctx)` (`_ = w.Run(ctx)`).

5. `forge generate`. The new `DB` field resolves to the ORM by type
   (`DB: g.ORM(),` in compose.go's `new<Worker>`); in a legacy `NewComponents`
   project, set it there yourself (`DB: infra.ORM` or what your Infra names
   it). Then `go mod tidy` if robfig/cron is now only indirect.

`Name()` is the claim key: keep it stable, since a renamed worker starts a
fresh claim history (harmless: the next tick is claimed under the new name).

### Behaviour to know

- At most once per tick: a replica that crashes mid-tick loses that tick, and
  a claim that fails (database down) skips the tick there, logged at ERROR
  (`outcome="claim_error"`).
- Ticks on one replica never overlap; a tick that outlives its period skips
  the ticks it covered. robfig/cron used to start an overlapping run instead.
- `@every <d>` is aligned to multiples of d, so it no longer counts from
  process start.

The `workers` skill (Cron-Scheduled Worker) is the full contract.

## 2. `releasehealth.ServiceHealth` is no longer comparable

**Use when** Go code imports `github.com/reliant-labs/forge/pkg/releasehealth`.

### What changed

`ServiceHealth` gained `Endpoints []EndpointHealth`: release health now judges
p95 latency per endpoint (`ServiceHealth.LatencyRatio`), so a window that
merely holds a different mix of endpoints does not read as the service getting
slower. A struct with a slice field cannot be compared with `==` or used as a
map key.

### What to do

Nothing, unless your code compares two `ServiceHealth` values with `==`/`!=`
or keys a map by one; the compiler names each such site. Compare the fields
you mean instead (`a.Service == b.Service`), or `reflect.DeepEqual` in a test.
Leaving `Endpoints` nil keeps the old service-level latency verdict.

## 3. Public sign-up is off outside development

**Use when** `internal/app/login_broker.go` mounts `RegisterPath` with no `if`
around it (`forge lint` reports `forgeconv-open-signup` at the line).

### What changed

The scaffolded broker used to mount `POST /auth/register` in every
environment, and outside development it signs the new account straight in
with an address nobody verified (`EmailVerified: false`). An app that binds
an invitation, a tenant membership or a pre-created record to a user by email
is then open to account takeover: register the victim's address, sign in, and
claim what was waiting for them.

A broker scaffolded since v1.0.16 serves sign-up in development only, unless
the deployed environment's config sets `auth_self_signup: true`, and serves
`GET /auth/options` so the sign-in screen offers "Create an account" only when
sign-up is served. `devidp.User` gained `EmailVerified`, read from the
issuer by `FindUserByID` / `FindUserByEmail`. `login_broker.go` and the
sign-in screen are yours, so forge does not rewrite them.

### Edit

1. Declare the opt-in on the config the broker reads (`proto/config/.../config.proto`,
   on the message `login_broker.go` takes; use its next free field number),
   then run `forge generate`:

   ```proto
   bool auth_self_signup = <next free tag> [(forge.v1.config) = {
     env_var: "AUTH_SELF_SIGNUP"
     flag: "auth-self-signup"
     default_value: "false"
     description: "Serve public sign-up (POST /auth/register) in a deployed environment; development always serves it. A self-registered address is UNVERIFIED: make every bind-by-email require devidp.User.EmailVerified before setting this."
   }];
   ```

2. In `internal/app/login_broker.go`, add the constant, the struct field, and
   its value in `RegisterLoginBroker`:

   ```go
   OptionsPath        = "GET /auth/options" // in the const block beside RegisterPath
   ```

   ```go
   type loginBroker struct {
       // ...
       // selfSignup is whether RegisterPath is served at all.
       selfSignup bool
   }
   ```

   ```go
   broker := &loginBroker{
       // ...
       devMode:    cfg.Environment == "development",
       selfSignup: cfg.Environment == "development" || cfg.AuthSelfSignup,
       // ...
   }
   ```

3. Replace the unconditional mount:

   ```go
   mux.Handle(RegisterPath, http.HandlerFunc(broker.handleRegister))
   ```

   with:

   ```go
   if broker.selfSignup {
       mux.Handle(RegisterPath, http.HandlerFunc(broker.handleRegister))
   }
   mux.Handle(OptionsPath, http.HandlerFunc(broker.handleOptions))
   ```

   and add the handler:

   ```go
   // authOptions is what the sign-in screen asks before it renders.
   type authOptions struct {
       SelfSignup bool `json:"selfSignup"`
   }

   // handleOptions reports whether sign-up is served. Public: anyone could
   // learn it by trying.
   func (b *loginBroker) handleOptions(w http.ResponseWriter, _ *http.Request) {
       writeAuthJSON(w, http.StatusOK, authOptions{SelfSignup: b.selfSignup})
   }
   ```

4. In the sign-in screen (`src/app/auth/sign-in/page.tsx` in Next.js, the
   sign-in route in `src/routes.tsx` in Vite), offer sign-up only when the
   server serves it. Add to `src/lib/auth/native-login.ts`:

   ```ts
   export interface AuthOptions {
     selfSignup: boolean;
   }

   // Fails closed: an older server, an outage or a malformed body hides sign-up.
   export async function fetchAuthOptions(): Promise<AuthOptions> {
     try {
       const response = await fetch(`${apiOrigin()}/auth/options`, {
         credentials: "include",
         headers: { Accept: "application/json" },
       });
       if (!response.ok) return { selfSignup: false };
       const body = (await response.json()) as Partial<AuthOptions> | null;
       return { selfSignup: body?.selfSignup === true };
     } catch {
       return { selfSignup: false };
     }
   }
   ```

   then, in the screen, load it once (a `useEffect` like `usePasswordPolicy`'s;
   new projects get `src/lib/auth/use-auth-options.ts`), change
   `const signingUp = mode === "signUp";` to
   `const signingUp = selfSignup && mode === "signUp";`, and wrap the
   "New here? / Create an account" `<p>` in `{selfSignup ? (…) : null}`.

5. **Before setting `auth_self_signup: true` in any deployed env,** make every
   place the app binds something to a user by email require the address
   verified: `idp.FindUserByID(ctx, claims.Subject)` and refuse unless
   `user.EmailVerified`. `forge skill load auth/authorization` ("Binding by
   email requires a verified address") has the code.

### Deciding

- Invite-only product: steps 2-4 are enough; leave `auth_self_signup` unset.
  Deleting the `RegisterPath` mount and the toggle outright also clears the
  lint.
- Anyone may join: do all five steps, then set `auth_self_signup = True` on
  the env's config in `deploy/kcl/<env>/config.k`.
