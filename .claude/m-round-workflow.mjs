export const meta = {
  name: 'm-round-night',
  description: 'Demo-app journey + downstream re-reviews -> synthesis -> fix wave -> merge+gate',
  phases: [
    { title: 'Journey + Reviews', detail: 'tracker demo app + cp-forge/kalshi fresh-eyes critiques' },
    { title: 'Synthesize', detail: 'all findings -> prioritized fix missions' },
    { title: 'Fix wave', detail: 'one worktree agent per mission' },
    { title: 'Merge + Gate', detail: 'land on feat/gateways, e2e gate, rebuild binary, smoke' },
  ],
}

const FINDINGS_SCHEMA = {
  type: 'object',
  required: ['summary', 'frictions', 'reportPath'],
  properties: {
    summary: { type: 'string', description: 'One-paragraph verdict' },
    reportPath: { type: 'string', description: 'Absolute path of the full markdown report you wrote' },
    frictions: {
      type: 'array',
      items: {
        type: 'object',
        required: ['title', 'severity', 'area', 'evidence'],
        properties: {
          title: { type: 'string' },
          severity: { type: 'string', enum: ['P0', 'P1', 'P2'] },
          area: { type: 'string' },
          evidence: { type: 'string', description: 'Concrete: file:line, command output, exact behavior observed' },
          proposedFix: { type: 'string' },
        },
      },
    },
  },
}

const MISSIONS_SCHEMA = {
  type: 'object',
  required: ['missions', 'deferred'],
  properties: {
    missions: {
      type: 'array',
      items: {
        type: 'object',
        required: ['id', 'title', 'priority', 'evidence', 'scope', 'approach'],
        properties: {
          id: { type: 'string' }, title: { type: 'string' },
          priority: { type: 'string', enum: ['P0', 'P1', 'P2'] },
          evidence: { type: 'string' }, scope: { type: 'string', description: 'Files/areas; what is OUT of scope' },
          approach: { type: 'string' }, risk: { type: 'string' },
        },
      },
    },
    deferred: { type: 'array', items: { type: 'object', required: ['title', 'why'], properties: { title: { type: 'string' }, why: { type: 'string' } } } },
  },
}

const FIX_SCHEMA = {
  type: 'object',
  required: ['branch', 'headSha', 'commits', 'summary', 'status'],
  properties: {
    branch: { type: 'string' }, headSha: { type: 'string' },
    commits: { type: 'array', items: { type: 'string' } },
    summary: { type: 'string' },
    testsRun: { type: 'string' },
    status: { type: 'string', enum: ['complete', 'partial', 'abandoned'] },
    notes: { type: 'string' },
  },
}

const MERGE_SCHEMA = {
  type: 'object',
  required: ['merged', 'gate', 'binaryRebuilt', 'smoke'],
  properties: {
    merged: { type: 'array', items: { type: 'string' } },
    skipped: { type: 'array', items: { type: 'string' } },
    gate: { type: 'string' }, binaryRebuilt: { type: 'boolean' }, smoke: { type: 'string' },
    notes: { type: 'string' },
  },
}

const FORGE = '/Users/seanteeling/src/reliant-labs/forge'
const BIN = '/tmp/forge-bin/forge'

const SHARED_RULES = `
PROCESS RULES (hard-won, do not skip):
- Shell cwd resets between commands: prefix EVERY command with an explicit cd.
- grep -c returns exit 1 on zero matches — never use it bare inside && chains.
- Write your FULL markdown report to the path you return as reportPath, under /tmp/m-round/ (mkdir -p it).
- Use \`${BIN} friction add\` inside any forge project to file frictions as you hit them.
- Report only what you OBSERVED with evidence. Distinguish "forge bug" from "I misread docs" — both are findings (the second is a docs/skill finding).`

phase('Journey + Reviews')
const [journey, cpfReview, kalReview] = await parallel([
  () => agent(`You are a blind-ish new user building a real app with forge (the binary at ${BIN}, freshly built from the feat/gateways tip — AIP-134 update_mask, forge run dev defaults, dev-claims, features.deploy derivation all just landed). Your job is BOTH to build and to critique: find friction, and especially "places forge fights the LLM" — where the generated shape forced you into workarounds, hand-edits, or fighting the tool.

BUILD "tracker" in /tmp/tracker (delete the dir first if it exists): a moderately complex project/issue tracker that genuinely exercises forge:
- forge new, then entities via the schema-truth flow (forge add entity + SQL migrations are the ONLY schema truth now): project, issue (belongs to project, has status/priority), comment (belongs to issue), label + issue_label join. Use deleted_at on issue (soft-delete convention), created_at/updated_at everywhere, a text search column on issue.
- A hand-written migration mid-journey: add issue.assignee, then a second migration moving data (e.g. backfill from a temp column) — regenerate and verify the ORM follows.
- Auth roles: authz policy distinguishing admin vs member on at least one RPC.
- A background worker (e.g. stale-issue sweeper) via the worker mechanism.
- Frontend: list/detail/create/edit pages for issue + project, nav, the generated hooks; customize at least one page like a real user would.
- Rename one RPC mid-journey and follow forge's own guidance for the fallout (the stale scaffold-test warning just shipped — judge it).
- Testing patterns: run the scaffolded tests, write one real unit test against the generated shapes, run frontend tests.

EXECUTE EVERYTHING — this is the core demand: forge run dev loop (judge the new dev defaults: does a zero-config boot work? do dev claims work with NO token?), curl/Connect-POST real CRUD including a MASKED partial update (verify other fields survive — the data-loss fix just landed, try to break it), unknown mask path -> expect InvalidArgument, soft-delete then list, the worker actually firing, npm build + tests, go test. Boot failures, port conflicts, confusing errors: all evidence.

Time-box: be thorough but keep moving — if blocked >15 min on one thing, file the friction with full evidence and route around. Skim the project's generated .claude/skills/forge/* as a real LLM user would; judge where skills misled you. Preserve /tmp/tracker for the morning.
${SHARED_RULES}
Final: structured findings; severity P0 = would make a new user quit, P1 = forced a workaround, P2 = paper cut.`, { label: 'M1:tracker-journey', phase: 'Journey + Reviews', schema: FINDINGS_SCHEMA }),

  () => agent(`Fresh-eyes critical review (REPORT ONLY — change nothing) of /Users/seanteeling/src/reliant-labs/cp-forge (branch forge-improvements-round, just converged to schema-truth forge and verified green/bootable). Hunt FRICTION and "places the project fought the LLM/forge": hand-edits that revert or wrap generated shape, lint suppressions and their evidence comments, adapter/shim layers that exist only to bridge generated code to what the app actually needed, the one disowned file (admin-web/src/app/page.tsx — why?), cmd/workspace-proxy (a whole parallel main created instead of extending generated commands — the approved-but-unbuilt cmd-as-code redesign is relevant context), .forge/friction.jsonl entries (read ALL, assess which are systemic), git log fights (same file regenerated/hand-fixed repeatedly), the 9 scalar-Deps config advisories (G3 adoption gap — what would full adoption take?), the 3 contractlint-failing packages (svcbilling/daemonregistry/llmproxy — why did the shape not fit?), pack-hook orphans. ALSO verify-by-reading my boot finding: internal/auth.DevModeEnabledFromEnv reads os.Getenv("ENVIRONMENT") directly while the server takes --environment via typed config — two sources of env truth; is that forge template shape or local code? Where else does raw-env-vs-config split exist?
${SHARED_RULES}
Severity: P0 = systemic forge design flaw, P1 = forge shape forced local workaround, P2 = polish.`, { label: 'M2:cp-forge-review', phase: 'Journey + Reviews', schema: FINDINGS_SCHEMA }),

  () => agent(`Fresh-eyes critical review (REPORT ONLY — change nothing) of /Users/seanteeling/src/kalshi-trader (branch forge-improvements-round, just converged to schema-truth forge: entity protos deleted, ORM introspected from SQL). FENCE: there is active third-party WTI/cutover-v49 WIP in the tree — ANOTHER AGENT MAY BE EDITING WHILE YOU READ; change NOTHING, and judge from COMMITTED state (HEAD 1b7581b0, standalone-green) where the worktree is churning. Known WIP-owned breakage you must NOT report as forge friction: db.ListPosition/db.Position in pkg/app/wire_trader_hooks.go, app.WTIPersister in wire_gen.go, and their duplicate migration version 00012. Hunt friction and "places the project fought forge": the four proto-lies just reconciled (settlement_data naming, phantom positions/tradeables, stale ORM columns, UUID-vs-int64 PKs) — what let them drift so far? wire_gen scalar-config gap (WTIPersistMaxPerTick emits 0+TODO every regen — friction already filed; assess the real need), hand-implemented handlers vs generated CRUD (ListSettlements/GetTradeable — did the generated shape fit?), adapters/* layers bridging generated ORM to domain logic, lint suppressions + evidence comments, .forge/friction.jsonl (read ALL), git log fights, scaffold-test staleness, the WIP coupling (wire_gen/diagnostics_gen uncommittable because they reference WIP symbols — is forge's regen-whole-file model hostile to in-flight work?).
${SHARED_RULES}
Severity: P0 = systemic forge design flaw, P1 = forge shape forced local workaround, P2 = polish.`, { label: 'M3:kalshi-review', phase: 'Journey + Reviews', schema: FINDINGS_SCHEMA }),
])

const found = [journey, cpfReview, kalReview].filter(Boolean)
log(`Phase 1 done: ${found.length}/3 reports, ${found.flatMap(r => r.frictions).length} frictions total`)

phase('Synthesize')
const synthesis = await agent(`You are the synthesis stage of an overnight forge-improvement round. Inputs:

1. JOURNEY (new-user tracker app): ${journey ? journey.reportPath : 'MISSING — agent died'}
2. CP-FORGE REVIEW: ${cpfReview ? cpfReview.reportPath : 'MISSING — agent died'}
3. KALSHI REVIEW: ${kalReview ? kalReview.reportPath : 'MISSING — agent died'}
Read all three reports IN FULL. Structured friction summaries:
${JSON.stringify(found.map(r => ({ summary: r.summary, frictions: r.frictions })), null, 1).slice(0, 30000)}

4. STANDING INPUTS (from the session lead — verify each before acting on it):
- Dev boot of cp-forge needed hand-discovered DATABASE_URL + ENVIRONMENT=development AS ENV VAR (the --environment flag does NOT satisfy internal/auth.DevModeEnabledFromEnv which reads os.Getenv directly — two sources of environment truth in the generated/templated shape) + AUTH_DEV_MODE=true; no error says which envs a dev boot needs.
- L1 frictions: forge-owned frontend-pages + auth skills still teach the RETIRED entity annotations (regenerated every run — must be fixed in forge's templates); schemadef shadow-apply chokes on pg schema-qualified DDL; pack-hook outputs audited as orphans.
- K2 noted: generated GeneratedAuthInterceptor has zero call sites in cmd/server.go (same dishonesty class as the jwt pack had).
- L2 (kalshi) frictions, all filed in kalshi's .forge/friction.jsonl with evidence: **fr-3fba9166ba P0** — managed-timestamps codegen not type-aware: schemadef.DetectConventions IS type-aware for deleted_at but writeManagedCreateTimestamps assumes time.Time, so legacy TEXT created_at/updated_at columns yield a generated ORM file that DOES NOT COMPILE and forge generate fails its own validate step on every run (kalshi carries a hand-patch); fr-4dfef712e9 P1 — ORM file headers advertise forge disown but those paths aren't in checksums.json so disown REFUSES (escape hatch unusable exactly where pitched); fr-fba0c4be8d P1 — scanExistingMethods skips handlers_crud.go so hand impls there get duplicate generated stubs (package doesn't compile).
- APPROVED-BUT-UNBUILT design (maintainer already said yes; include as a mission if tonight's evidence supports it): cmd-as-code — per-service cobra subcommands generated from registration rows, user-owned cmd/commands.go extension point, kill the devMode parameter threading, Set*-globals -> Deps. cp-forge's cmd/workspace-proxy is the standing evidence.
- Known open, do NOT spawn missions for (human-shaped): pkg version tag push, reliant pin bump, branch review/merge.

Produce AT MOST 6 fix missions for forge (repo: ${FORGE}, branch feat/gateways). Rules: each independently mergeable, scoped to forge itself (not downstream repos), evidence-backed (cite which report/observation), sized for one agent-night (a focused multi-hour change, not a rewrite); prefer fixing the GENERATOR/templates/libraries over downstream symptoms; collapse duplicates across reports (the same root often appears in all three). Priority by user pain. Anything real but too big or too risky for unattended overnight goes in deferred with why.`, { label: 'synthesize', phase: 'Synthesize', schema: MISSIONS_SCHEMA })

const missions = synthesis.missions.slice(0, 6)
log(`Missions: ${missions.map(m => `${m.id}(${m.priority})`).join(', ')}; deferred ${synthesis.deferred.length}`)

phase('Fix wave')
const fixes = await parallel(missions.map(m => () =>
  agent(`You are fixing forge (repo ${FORGE}) in YOUR OWN git worktree. MANDATORY FIRST STEP — the harness spawns worktrees at a stale commit: \`git reset --hard $(git rev-parse feat/gateways)\` then confirm \`git log --oneline -1\` shows the K2 merge (a3e3b88) or newer.

MISSION ${m.id} [${m.priority}]: ${m.title}
EVIDENCE: ${m.evidence}
SCOPE: ${m.scope}
APPROACH: ${m.approach}
RISK: ${m.risk || 'n/a'}

Rules: red-first where a bug is claimed (reproduce, pin with a failing test, fix); no backwards compat needed (unreleased); fix the generator/template/library root, not symptoms; skills updated when behavior changes. VELOCITY: go test -short ./... (~8s) inner loop; full mode only on touched packages; do NOT run the e2e gate (the merge stage owns it) unless you changed corpus fixtures, then run it ONCE at the end. COMMIT EARLY AND OFTEN — per coherent step, never leave work uncommitted (two predecessor agents died with everything uncommitted). Commits end: Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>. Stay strictly inside your scope — five sibling agents are doing other missions in parallel worktrees; out-of-scope edits cause merge conflicts. Return your branch name (git rev-parse --abbrev-ref HEAD), head sha, commit list, and honest status.`,
    { label: `fix:${m.id}`, phase: 'Fix wave', schema: FIX_SCHEMA, isolation: 'worktree' })
))

const landed = fixes.filter(Boolean).filter(f => f.status !== 'abandoned')
log(`Fix wave: ${landed.length}/${missions.length} branches ready`)

phase('Merge + Gate')
const merge = await agent(`You are the merge+gate stage in the MAIN forge repo ${FORGE} (branch feat/gateways, currently at the K2 merge a3e3b88 or newer). Prefix every command with cd ${FORGE}. Tree must be clean before you start (untracked .claude/* files are fine; never commit them).

Merge these mission branches SEQUENTIALLY (run \`go test -short ./...\` between merges; on conflict, resolve by understanding BOTH intents — port assertions, never drop either side's tests; if a branch is fundamentally broken, skip it and say why):
${JSON.stringify(landed.map(f => ({ branch: f.branch, headSha: f.headSha, summary: f.summary, status: f.status })), null, 1)}

Then: 1) full gate \`go test -tags e2e -count=1 -timeout 30m ./internal/cli/\` plus \`go test -race -count=1 ./...\` — fix what breaks (the fix is usually porting a fixture/assertion, not reverting a mission); 2) rebuild the binary: cd ${FORGE} && go build -o ${BIN} ./cmd/forge ; 3) SMOKE: with the fresh binary scaffold a brand-new project in /tmp/m-smoke (delete first), forge generate, go test -short inside it, boot it in dev mode (sqlite), one CRUD roundtrip via curl, kill it; 4) delete merged worktree branches (git branch -d) and \`git worktree prune\`. Merge commits: "Merge <mission-id>: <title>" + Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>. Report honestly — a skipped branch with reasons beats a forced broken merge.`,
  { label: 'merge+gate', phase: 'Merge + Gate', schema: MERGE_SCHEMA })

return {
  reports: found.map(r => ({ path: r.reportPath, summary: r.summary })),
  frictionCount: found.flatMap(r => r.frictions).length,
  missions: missions.map(m => ({ id: m.id, title: m.title, priority: m.priority })),
  deferred: synthesis.deferred,
  fixes: fixes.filter(Boolean).map(f => ({ branch: f.branch, status: f.status, summary: f.summary })),
  merge,
}
