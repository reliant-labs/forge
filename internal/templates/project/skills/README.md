# Skills

Skills are focused, self-contained playbooks that teach an AI agent (or a human) how to accomplish a specific task in a Forge project.

Skills are served via `forge skill list` and `forge skill load <name>` — they are embedded in the forge binary and not written to disk. Each skill has a path-based name (e.g. `db`, `frontend/state`, `debug/investigate`).

## Structure

Skills are organized by **action** — what you're trying to do — not by CLI command.

```
.reliant/skills/
└── forge/
    ├── SKILL.md                        # Project overview: proto-first philosophy, layout, dev loop
    ├── services/SKILL.md               # Scaffold and wire services, packages, frontends
    ├── workers/SKILL.md                # Background workers (including cron-scheduled)
    ├── api/SKILL.md                    # Write Connect RPC handlers
    ├── frontend/SKILL.md               # Write frontends (Next.js web + React Native mobile)
    ├── testing/                        # Testing methodology
    │   ├── SKILL.md                    # Philosophy: mock vs real, harness patterns, flakiness
    │   ├── unit/SKILL.md              # Hermetic unit tests with generated mocks
    │   ├── integration/SKILL.md       # Integration tests with real DB
    │   └── e2e/SKILL.md              # End-to-end tests against running stack
    ├── debug/                          # Debugging methodology
    │   ├── SKILL.md                    # Orchestrator: triage, parallel investigation, synthesis
    │   ├── reproduce/SKILL.md         # Runtime reproduction with diagnostic logging
    │   ├── isolate/SKILL.md           # TDD-driven bug isolation via top-down bisection
    │   └── investigate/SKILL.md       # Hypothesis-driven debugging via code review
    ├── db/SKILL.md                     # Database: migrations, queries, proto-db sync
    └── deploy/SKILL.md                 # Ship it: lint, build, deploy, verify
```

## How skills are used

Run `forge skill list` to see all available skills with descriptions. Run `forge skill load <name>` to print a skill's content to stdout. When an agent hits a task that matches a skill's description, it should load the skill and follow it.

Skills are **opinionated**. They encode project conventions and the non-obvious gotchas. Don't treat them as optional — the shortcuts around them cause pain.

## Adding your own skills

To add project-specific skills, create `.reliant/skills/<name>/SKILL.md` files. Use the existing forge skills as a template:

1. **YAML frontmatter** with `name` (must match directory name) and `description`
2. **Action-oriented structure** — organize by what the developer wants to do, not by CLI subcommand
3. **Rules** — invariants and things you must not do
4. **When this skill is not enough** — pointers to other skills or approaches

Keep skills short and actionable. Target < 60 lines. If a skill is getting long, split it into nested sub-skills.

**Important**: every command you cite in a skill must actually exist. There's a test at `internal/generator/skills_commands_test.go` that walks every SKILL.md and asserts each `forge ...` command resolves to a real cobra subcommand. That test will fail CI if you add a skill that references a non-existent command.