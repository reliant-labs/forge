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

## Dual-audience skills: `emit:` and `@forge-only` blocks

A skill can be authored once and emitted to two audiences: **general** (any project — methodology that doesn't depend on forge) and **forge** (forge projects, which see the full thing including framework-specific tooling). The compiler picks what to include based on per-skill frontmatter and inline block markers.

**Directory layout** decides the default audience:

```
skills/
├── forge/      # default emit: forge — framework skills (db, proto, api, ...)
└── general/    # default emit: general — methodology (code-review, refactor, ...)
```

A skill placed under `skills/forge/<name>/SKILL.md` defaults to `emit: forge`; under `skills/general/<name>/SKILL.md` defaults to `emit: general`. The frontmatter `emit:` field overrides this default — `debug` lives under `skills/forge/` but declares `emit: both` because its methodology applies anywhere while the forge-CLI tooling guidance is forge-only.

**Frontmatter `emit:` field** declares which audiences see the skill at all:

```yaml
emit: forge      # forge projects only (default for framework skills like proto, db, api)
emit: general    # any project (methodology that has no forge content)
emit: both       # both audiences (compiler strips @forge-only blocks for general emit)
```

**`@forge-only` block markers** mark content that only appears in the forge-audience emit. Use HTML comment markers so the raw source still renders cleanly in any markdown viewer:

```markdown
<!-- @forge-only:start -->
## Forge-Specific Debug Tools

forge debug start              # attach Delve debugger
forge test --race              # run tests with race detector
<!-- @forge-only:end -->
```

**Markers must sit on their own line** (whitespace around the line and inside the comment is fine). The renderer is line-based — inline markers in the middle of a sentence will not be stripped. If you need to gate a single sentence, lift it into its own paragraph between the markers.

Content outside `@forge-only` blocks is included in both emits. **The general prose has to be more than just CLI-free — it has to be architecture-free.** Anything that assumes a forge-shaped project belongs in `@forge-only`, including:

- Specific generated files / paths (`wire_gen.go`, `internal/<svc>/`, `pkg/tdd`).
- Forge architectural concepts (proto-as-canonical-input, generated mocks, Tier-1 vs Tier-2 ownership, DI wiring, `forge generate` pipelines, `forge audit`).
- Cross-references to forge sibling skills (`see the X sub-skill`) — those links are dead in a non-forge project. Fold the key idea inline in the general prose, then name the sub-skill inside the `@forge-only` block.
- Stack-specific tooling that only makes sense in a forge project (Connect RPC handler patterns, KCL deploy specifics, sqlc query files).

Generic principles (mock-vs-real, test pyramid, race detection, the verify-visually loop) stay in the general prose — they apply anywhere. The test: a reader on a Python or Rust project should still get value from the general emit; if they hit "see the wire_gen.go" or "swap the generated mock," you've leaked.

See `forge/debug/SKILL.md` and `forge/diagrams/SKILL.md` for worked examples.

## Adding your own skills

To add project-specific skills, create `.reliant/skills/<name>/SKILL.md` files. Use the existing forge skills as a template:

1. **YAML frontmatter** with `name` (must match directory name) and `description`
2. **Action-oriented structure** — organize by what the developer wants to do, not by CLI subcommand
3. **Rules** — invariants and things you must not do
4. **When this skill is not enough** — pointers to other skills or approaches

Keep skills short and actionable. Target < 60 lines. If a skill is getting long, split it into nested sub-skills.

**Important**: every command you cite in a skill must actually exist. There's a test at `internal/generator/skills_commands_test.go` that walks every SKILL.md and asserts each `forge ...` command resolves to a real cobra subcommand. That test will fail CI if you add a skill that references a non-existent command.