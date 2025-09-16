# Skills

This directory contains **skills** — focused, self-contained playbooks that teach an AI agent (or a human) how to accomplish a specific task in this Forge project.

Each skill lives in its own folder as a `SKILL.md` file. The folder name determines how the skill is referenced (for example, `forge/debug` lives at `forge/debug/SKILL.md`).

## Structure

Ordered roughly by workflow — create things, wire them, run them, test them, debug them, ship them.

```
.reliant/skills/
└── forge/
    ├── add/SKILL.md        # Scaffold new services or frontends
    ├── package/SKILL.md    # Create internal Go packages with interface contracts
    ├── generate/SKILL.md   # Regenerate code from .proto files
    ├── run/SKILL.md        # Run the full dev stack with hot reload
    ├── test/SKILL.md       # Unit + integration tests
    ├── e2e-test/SKILL.md   # End-to-end tests against a running stack
    ├── debug/SKILL.md      # Debug services with Delve / Chrome DevTools
    ├── build/SKILL.md      # Build binaries, frontends, and Docker images
    ├── lint/SKILL.md       # Run linters across Go, proto, and frontend
    ├── db/SKILL.md         # Database migrations, introspection, proto-db sync
    └── deploy/SKILL.md     # Deploy via KCL to k3d, staging, prod
```

## How skills are used

Skills are referenced by name from the project's `.reliant/reliant-forge.md` conventions file. When an agent hits a task that matches a skill's `when_to_use` criteria, it should open the corresponding `SKILL.md` and follow it.

Skills are **opinionated**. They encode project conventions and the non-obvious gotchas (e.g. "never hand-edit `gen/`", "integration tests need the `integration` build tag", "`forge package` is about internal Go packages, not Docker"). Don't treat them as optional — the shortcuts around them cause pain.

## Adding your own skills

Create a new folder under `.reliant/skills/` and drop a `SKILL.md` file in it. Use the existing forge skills as a template:

1. **YAML frontmatter** with `name`, `description`, and `when_to_use` list
2. **Core commands** up front — what to actually type, with real flags
3. **Workflow** as a numbered list
4. **Rules** — invariants and things you must not do
5. **When this skill is not enough** — pointers to other skills or approaches

Keep skills short and actionable. Target < 60 lines. If a skill is getting long, it probably wants to split into multiple skills.

**Important**: every command you cite in a skill must actually exist. There's a test at `internal/generator/skills_commands_test.go` that walks every SKILL.md and asserts each `forge ...` command resolves to a real cobra subcommand. That test will fail CI if you add a skill that references a non-existent command.
