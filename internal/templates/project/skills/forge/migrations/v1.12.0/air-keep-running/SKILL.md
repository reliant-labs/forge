---
name: migrations/v1.12.0/air-keep-running
description: A failed air rebuild leaves the running server alone (forge v1.12.0). air restarts its binary after every build, a failed one included, so with the scaffolded stop_on_error = false a compile error cost a full server restart onto the OLD binary, cutting every open stream while silently running stale code. A new project's .air.toml and .air-debug.toml build through `forge dev air-build`, which never hands a failed build back to air, so air never restarts the server for it; the next successful build restarts the server once, with the fresh code. An older project's air configs are its own (scaffold-once), so moving the pin changes nothing in them. `forge project upgrade` rewrites each [build] cmd still in forge's scaffolded form (`forge fix air-keep-running`) and names an edited one as the edit to make by hand. Use when an air config's [build] cmd ends in `&& mv ./tmp/<name>.tmp ./tmp/<name>`, or a failed air build restarts the server.
version: v1.12.0
automation: fix:air-keep-running
detection: forge-detect air-keep-running
---

# A failed air rebuild leaves the running server alone (v1.12.0)

air, the hot-reload runner `forge env up` and `task dev` use for a host API,
builds first and then stops the running binary and starts the one on disk,
whether or not the build succeeded. With `stop_on_error = false` (forge's
scaffold) a compile error therefore cost a full restart: SIGINT, the
`kill_delay` drain, a new process, onto the OLD binary. Every watch stream the
server held was cut, and nothing said it was still running stale code.
`stop_on_error = true` is worse: air stops the server and leaves it down until
the fix compiles. air has no setting that leaves the process alone.

## What changed in forge

`forge dev air-build --out <binary> -- <build command>` runs air's build. On
success it moves `<binary>.tmp` onto `<binary>` and exits 0. On failure it
prints one line,

```
forge: failed to build; the running <name> keeps serving the last good build. Fix the error and save to rebuild.
```

and does not return while air runs. air does nothing with the server until a
build command returns, so it never restarts it for the failed build:

- break a `.go` file and save: the compiler's errors and that line; the server
  keeps its PID and keeps serving;
- fix it and save: one restart, onto the fresh build.

The held build waits as a small `/bin/sh` loop (about 2 MB, polling once a
second) until air exits, one per failed build of the session. It has to stay
until then: air keeps each build's stop token in a one-slot channel, and a
superseded build that returned would take its successor's token, after which
air can cancel nothing and runs two restarts at once (two servers, one
orphaned on the port, observed with air v1.67.4). It exits with the build's
status when air exits or is killed, when air removes its `tmp_dir` on exit, or
on SIGINT/SIGTERM, so nothing outlives `forge env down`.

It exits with the build's status at once (air's own behaviour) when there is
nothing running to keep: the first build of an `env up` (so the readiness gate
still sees air's `failed to build` at once), a binary left behind by an air
that was killed, Windows (air stops the binary before it builds there), or a
platform where forge cannot tell air's process apart (it reads the process
table on Linux and macOS).

A project forge scaffolds from v1.12.0 has, in `.air.toml`:

```toml
  cmd = "forge storage check ./tmp; CGO_ENABLED=0 forge dev air-build --precheck-status $? --out ./tmp/app -- go build $FORGE_GO_BUILD_FLAGS -buildvcs=false -o ./tmp/app.tmp ./cmd/app"
```

and `.air-debug.toml` builds the same way with its `-gcflags='all=-N -l'
-trimpath=false`. `stop_on_error` stays `false`: a held build never returns, so
air reads it only when forge could not hold (Windows, the first build), where
`true` would stop the server until the fix compiles.

## What does not change by itself

`.air.toml`, `.air-debug.toml` and any `air_config` an OnHost runtime names are
the project's own (scaffold-once). Moving `forge_version` changes none of
them, and nothing breaks: an old config keeps restarting the old binary after a
failed build, as before.

## The edit: `forge fix air-keep-running`

`forge project upgrade --to v1.12.0` (or later) moves the pin and runs it. To
run it on its own:

```bash
forge fix air-keep-running --dry-run   # what it would change, and what it leaves to you
forge fix air-keep-running
git diff -- .air.toml .air-debug.toml
```

It reads `.air.toml`, `.air-debug.toml` and every `air_config` under
`deploy/kcl`, and rewrites a `[build]` cmd only when it is byte for byte a form
an older forge scaffolded for one binary name:

```
forge storage check ./tmp && CGO_ENABLED=0 go build <flags> -o ./tmp/<name>.tmp ./cmd/<name> && mv ./tmp/<name>.tmp ./tmp/<name>
```

where `<flags>` is `$FORGE_GO_BUILD_FLAGS -buildvcs=false`, `-buildvcs=false`,
or the debug config's `-gcflags='all=-N -l' -trimpath=false -buildvcs=false`
(with or without `-trimpath=false`). It becomes:

```
forge storage check ./tmp; CGO_ENABLED=0 forge dev air-build --precheck-status $? --out ./tmp/<name> -- go build <flags> -o ./tmp/<name>.tmp ./cmd/<name>
```

The same `go build` writes the same `./tmp/<name>.tmp`; forge installs it
instead of `mv`. `[build.windows]` is never touched, and nothing else in the
file changes: the result must decode as TOML to the old document with only
that cmd changed, or it is not written. forge.yaml must pin v1.12.0 or later,
because a bare `air` runs the forge on PATH, and an older one has no `forge dev
air-build`. Re-running it changes nothing.

## An edited command

A `[build]` cmd in any other form is left alone, and the run names it. Make the same edit by hand: run the `go build` under
`forge dev air-build --out <the binary air runs> --`, have it write
`<that binary>.tmp`, and drop the `&& mv` that installed it. Keep any prefix
your command has. If it starts with `forge storage check ./tmp`, separate the
check with `;` and pass `--precheck-status $?` to the wrapper, as in the example.
Keep build environment assignments such as `CGO_ENABLED=0`.

## Verify

```bash
forge env up dev
# break a .go file the server compiles, save: the log shows the compiler's
# errors and "keeps serving the last good build"; the server's PID
# (`pgrep -f tmp/<name>`) is unchanged, and the API still answers
# fix it, save: one restart
```
