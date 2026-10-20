---
name: subtrees
description: Subtree configs — one typed config several commands of a binary load (every command at or below a path), without every process loading it; registration, the loader's path check, the collision rules and the KCL projection.
---

# Subtree configs

A setting many commands read but the server roles must not carry — the CLI's server URL and token, a guard policy `daemon start` and `open` both run — is a SUBTREE config: loaded by every command at or below each path, and by no other process.

```protobuf
message CLIConfig {
  option (forge.v1.binary_config) = {
    binary: "app"
    subtrees: [{path: ["auth"]}, {path: ["daemon"]}, {path: ["open"]}]
  };
  string server_url = 1 [(forge.v1.config) = {env_var: "APP_SERVER_URL", flag: "server"}];
  string token = 2 [(forge.v1.config) = {env_var: "APP_TOKEN", sensitive: true}];
}
```

```go
config.RegisterCLIConfigFlags(authCmd)     // persistent on EACH subtree's command
config.RegisterCLIConfigFlags(daemonCmd)
config.RegisterCLIConfigFlags(openCmd)
// in any RunE at or below them:
cli, err := config.LoadCLIConfig(cmd)      // refuses a command under none of them
```

- `subtrees` and `command` are exclusive; no path may lie below another of the same message.
- Any two configs one process can load together — root and anything, a subtree config and a command or subtree config at or below it — may not declare the same env var, flag or top-level field; `generate` refuses the pair, naming a process that loads both. Siblings no process loads together may compose the same block: one definition, two values.
- KCL: `<bin>ProcessEnvMap` adds every subtree config whose path prefixes the workload's argv path, so `app server api`'s Deployment carries none of `CLIConfig`. Add the instance to `<bin>_process` in each env's config.k.
- Composition graph: each subtree and command config of the primary binary is a graph input (`g.<Message>`). A root's process loads the ones over its command path, and its reach is the fields its closure reads of them (`architecture`, "Roots and commands"). `roots_gen.go` registers a subtree config's flags on its path's command.
