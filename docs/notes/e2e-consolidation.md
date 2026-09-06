# e2e suite consolidation — analysis and proposal

> ## ⚠️ MEASURED CORRECTION — read before implementing any family below
>
> **Every saving estimate in this document is in TEST-SECONDS, and test-seconds
> are not wall-clock.** The e2e tests are `t.Parallel()`, so on a developer box
> they run concurrently and the estimates overstate the real prize by roughly 5x.
>
> Measured on this 16-core machine:
>
> | | test-seconds | wall-clock |
> |---|---|---|
> | whole e2e suite | 9,764 | **817s** (~12x concurrency) |
> | Family B's 7 members | 577 | **158s** |
>
> Wall-clock is bounded below by the SINGLE LONGEST MEMBER, not by the sum.
> Family B's longest member is 151.7s, so perfect consolidation of all seven
> would have saved ~6 seconds (~4%) locally.
>
> **Family B was attempted and abandoned** on this basis, plus a hard blocker:
> its premise that the seven entities are differently named is false. Three
> declare `message Order` and two declare `message Gadget`; table names are the
> pluralized snake_case of the entity with no service namespacing
> (`internal/codegen/schema_entities.go:63`), so co-locating them dies with
> `pq: relation "orders" already exists (42P07)`. There is no table-name
> override marker. Three of the seven also re-run `forge generate` over their
> own tree (their subject IS generate idempotency), so they cannot be read-only
> members of a shared tree either.
>
> **Before implementing Family A or C: measure that family's wall-clock
> baseline first**, and justify the work against that number rather than the
> test-second estimate below. Test-seconds still matter on CI (4 shards on
> 4-vCPU runners is ~4x concurrency, not 12x), so a family may be worth doing
> for CI cost alone — but say which currency you are buying in.
>
> ### Decision taken: no family was implemented
>
> Families A and C were declined on the measurement above. Family A's estimate
> was additionally invalid at the time it was written: all six of its members
> were RED, so its 2,168 test-seconds described failure paths (some dying fast
> at a precondition, some slow-failing) rather than the passing workload.
>
> The frontend fix that turned those six green also removed the pathology
> Family A would have papered over — six code paths looking in the wrong
> `node_modules` under the npm-workspace dev bridge. The npm cost that remains
> is real work, and sharing one install across six tests would couple six
> independent gates to a single fixture for a wall-clock gain bounded by the
> slowest member.
>
> **Revisit only if CI cost becomes the binding constraint**, where the ~4x
> shard concurrency makes test-seconds matter more than they do locally. Family
> D (derive in-process) needs no shared tree and stays the better move wherever
> it applies.


Status: **proposal only**. Nothing here has been implemented. Written for the
implementing tasks (plan tasks "Implement the highest-value consolidation
family" and "Roll out consolidation to remaining families").

Source of truth for the survey: every `internal/cli/*_e2e_test.go` file as of
this writing. Timings are measured seconds from one real full-suite run on a
12-core dev box (the same run the plan description quotes).

---

## 1. The central finding, stated before the table

The naive framing — "30 tests scaffold the same project, so scaffold once and
share the tree" — **does not survive contact with the code.** Almost every
expensive test writes its own `proto/services/<svc>/v1/<svc>.proto` *before*
running `forge scaffold`, and several also write their own hand-rolled
migrations. Their scaffold inputs are not identical; they are identical only in
the `forge project new` flags, which is the *cheap* part of the pipeline. The
expensive parts are `forge generate` (buf compile + embedded-postgres shadow
apply), `go build ./...`, and `npm install`.

So the saving does not come from sharing a scaffold. It comes from three
different mechanisms, in descending value:

1. **Share one npm-installed frontend tree.** `npm install` is the single
   biggest cost in the suite and at least six tests each pay one.
2. **Merge entity-projection tests into ONE project carrying N entity
   messages.** Each currently pays its own `generate` + `go build` to assert a
   handful of read-only facts about generated Go. One project with eight
   entities pays one `generate` and one `go build`, and each former test
   becomes a read-only subtest reading a different generated file.
3. **Derive assertions instead of scaffolding at all**, following
   `scaffold_scalar_vocabulary_e2e_test.go`. Where an assertion is really
   "does the generator's own table/scanner agree with the fixture", it can run
   in 0.00s with no subprocess. This is strictly better than sharing a tree and
   should be checked for first, every time.

---

## 2. Per-test survey

Columns: **new flags** = the `forge project new` invocation; **mutates** = what
the test writes into the tree after scaffolding; **pg** = needs a real
postgres; **npm** = needs node/npm; **boot** = boots a server on a
`freePortE2E`; **fail** = asserts a failure path; **RO** = purely read-only
over the emitted tree after its own setup.

| Test | s | new flags | mutates after scaffold | pg | npm | boot | fail | RO |
|---|---|---|---|---|---|---|---|---|
| `Vocabulary_EntityCarriesEveryScalarKind` | 0.00 | none — no scaffold | writes fixture proto to TempDir only | – | – | – | – | ✔ |
| `Vocabulary_CreateRequestCarriesEveryScalarKind` | 0.00 | none | as above | – | – | – | – | ✔ |
| `Vocabulary_EveryScalarFieldHasAColumn` | 0.00 | none | as above | – | – | – | – | ✔ |
| `ScaffoldVersion` | ~0 | none (runs `forge --version`) | – | – | – | – | – | ✔ |
| `AllBinaryConfigNoPrimaryBinding_RejectsAtGenerate` | 20 | `--service item` | edits config/binary wiring, then `generate` must FAIL | – | – | – | **✔** | ✖ |
| `ScaffoldKCLRendersDevManifest` | 27 | `--service item` | none; `kcl` render + read | – | – | – | – | ✔ |
| `ScaffoldReadOnlyMarkerWithNoFieldFailsLoudly` | 55 | `--service orders` | writes bad proto, `scaffold` must FAIL | – | – | – | **✔** | ✖ |
| `ComponentsKCLDeclaredFromSources` | 59 | `--service order --service intake` | none | – | – | – | – | ✔ |
| `JSONBUnmappableFieldFailsGenerate` | 64 | `--service orders` | proto + hand migration, `generate` must FAIL | ✔ | – | – | **✔** | ✖ |
| `ZeroServiceScaffoldCompiles` | 69 | *(no `--service`)* | none | – | – | – | – | ✔ |
| `ScaffoldNoConflictingProtos` | 75 | `--service api` | writes an extra proto | – | – | – | – | ✖ |
| `GenerateRollbackOnValidateFailure` | 88 | `--service api` | corrupts a Tier-1 file, `generate` must revert | – | – | – | **✔** | ✖ |
| `SelfCertCloneReproduces` | 90 | `--service api` | edits a generated file, clones tree | – | – | – | – | ✖ |
| `ScaffoldSecretFieldPreservedOnFullReplace` | 92 | `--service vaults` | own proto; injects a Go test; runs it | ✔ | – | – | – | ✖ |
| `ScaffoldReadOnlySurvivesInlineOptions` | 92 | `--service orders` | own proto | – | – | – | – | ✖ |
| `ScaffoldConfigNaming` | 93 | `--service api` | none | – | – | – | – | ✔ |
| `AllBinaryConfigScaffoldCompiles` | 95 | `--service item` | binary/config wiring edits | – | – | – | – | ✖ |
| `ScaffoldAddService` | 95 | `--service api` | `scaffold service billing` | – | – | – | – | ✖ |
| `ScaffoldReadOnlyFieldOmittedFromCreate` | 100 | `--service orders` | own proto | – | – | – | – | ✖ |
| `ScaffoldKCLVendorFlow` | 100 | `--service item` | breaks then repairs `deploy/kcl/kcl.mod` | – | – | – | – | ✖ |
| `CRUDFixtureSurvivesAddedConstraints` | 103 | `--service widget` | own proto + hand constraint migration | ✔ | – | – | – | ✖ |
| `ScaffoldBasicProject` | 118 | `--service api` | none; `golangci-lint` + `buf lint` | – | – | – | – | ✔ |
| `SelfCertLegacyManifestMigration` | 119 | `--service api` | rewrites `.forge/checksums.json` | – | – | – | – | ✖ |
| `SelfCertParallelLaneSubsetCommit` | 125 | `--service api` | `scaffold service billing` | – | – | – | – | ✖ |
| `ScaffoldSchemaHardeningDefaults` | 140 | `--service shop` | own proto | – | – | – | – | ✖ |
| `OrderByWithoutDescending` | 145 | `--service widgets` | own proto | – | – | – | – | ✖ |
| `GeneratedStoreSeam` | 154 | `--service widgets` | own proto, `scaffold package pricing`, hand service | ✔ | – | **✔** | – | ✖ |
| `AddServiceThenEntityGenerates` | 164 | *(no `--service`)* | `scaffold service item` + own proto | – | – | – | – | ✖ |
| `SchemaDriftNotice` | 205 | `--service orders` | own proto, hand migration, tightened proto | ✔ | – | – | – | ✖ |
| `JSONBEntityConversion` | 215 | `--service orders` | own proto + injected Go test | ✔ | – | – | – | ✖ |
| `ScaffoldFrontendEnumEntityBuilds` | 389 | `--service brand --frontend dashboard` | own proto | – | **✔** | – | – | ✖ |
| `FreshScaffoldLintExitsZero` | 396 | `--service orders --frontend dashboard` | own proto; `forge lint --no-fix` | – | – | – | – | ✖ |
| `GeneratedHooksExposeTheTypedErrorContract` | 396 | `--frontend web` | `scaffold service item` + own proto; 3 tsc runs | – | **✔** | – | – | ✖ |
| `ScaffoldFrontendBuilds` | * | `--frontend web` | `scaffold service item`, corpus proto+migration | – | **✔** | – | – | ✖ |
| `ScaffoldFrontendListResource` | * | `--frontend dashboard` | `scaffold service order` | – | **✔** | – | – | ✖ |
| `ScaffoldFrontendRuntime` | * | `--frontend web` | `scaffold service item` | – | **✔** | – | – | ✖ |
| `AddFrontendKindsProduceABuildableTree` | * | `--service widget` | three `scaffold frontend --kind` adds, each `npm install` | – | **✔** | – | – | ✖ |
| `FreshScaffoldFrontendLintClean` | * | `--service catalog --frontend dashboard` | own proto; npm lint/styles/fix cycle | – | **✔** | – | – | ✖ |
| `ValidateConstraintsProjection` | * | `--service catalog --frontend dashboard` | own proto; npm install; wire test | ✔ | **✔** | – | – | ✖ |
| `FixtureCorpusFrontendBasePath` | * | `--service api` | own proto, `scaffold frontend console`, two npm builds | – | **✔** | – | **✔** (one build must fail) | ✖ |
| `ScaffoldFullSpecProject` | * | `--service api --frontend web` | none | – | – | – | – | ✔ |
| `ScaffoldMultiServiceProject` | * | `--service api,users,orders --frontend web` | none | – | – | – | – | ✔ |
| `ScaffoldServerStartup` | * | `--service api` | none; builds and boots | – | – | **✔** | – | ✖ |
| `ScaffoldIsReviveExportedClean` | * | `--service orders` | own proto + 4 `scaffold` noun adds | – | – | – | – | ✖ |
| `TestingPackageNotLinkedIntoBinary` | * | `--service order` | own proto | – | – | – | – | ✖ |
| `ScaffoldMarkedEntityDeclaresManagedFields` | * | `--service widget` | own proto | – | – | – | – | ✖ |
| `ScaffoldHandlersFilePlusMarkedEntityCompiles` | * | `--service widget` | own proto + hand handler file | – | – | – | – | ✖ |
| `ScaffoldReadOnlyColumnTakesItsSchemaDefault` | * | `--service widget` | own proto + tightening migration | – | – | – | – | ✖ |
| `OptionalScalarEntityBirthAndConversion` | * | `--service orders` | own proto | ✔ | – | – | – | ✖ |
| `EnumEntityConversionAndFilter` | * | `--service orders` | own proto + injected Go test | ✔ | – | – | – | ✖ |
| `PbThroughCrudPlusCustomRpc` | * | `--service widget` | own proto | ✔ | – | – | – | ✖ |
| `PbThroughStubToCrudTransition` | * | `--service catalog` | proto twice (stub → CRUD) | – | – | – | – | ✖ |
| `CRUDFixtureSatisfiesCheckConstraints` | * | `--service storefront` | own proto + check-constraint migration | ✔ | – | – | – | ✖ |
| `CRUDFixtureGuardFailsLoudlyOnUninvertibleCheck` | * | `--service account` | proto + migration, `generate` must FAIL | – | – | – | **✔** | ✖ |
| `ComponentObserveMarkerAndEnforceLint` | * | `--service api` | `scaffold package checkout`, 5 generates, lint | – | – | – | – | ✖ |
| `ObservedDecoratorGeneratedAndWired` | * | `--service api` | `scaffold package checkout`, contract edits | – | – | – | – | ✖ |
| `AddVerbsProduceABuildableTree` | * | `--service widget` | worker/cron/operator/crd/webhook adds | – | – | – | – | ✖ |
| `CmdAsCodeSubcommands` | * | `--service api` | writes custom commands file | ✔ | – | **✔** | – | ✖ |
| `FailedGenerateRevertConsistencyAndPreservation` | * | `--service widget` | conflicting file, `scaffold` must fail, then repair | – | – | – | **✔** | ✖ |
| `ScaffoldWorkloads`/`AllBinaryConfig` (2nd) | 20–95 | `--service item` | binary config edits | – | – | – | – | ✖ |
| `FixtureCorpusZeroService` | * | *(no `--service`)* | `scaffold service item` | – | – | – | – | ✖ |
| `FixtureCorpusCRUDLifecycle` | * | corpus shape | migrations + CRUD drive | ✔ | – | **✔** | – | ✖ |
| `FixtureCorpusCPForgeShaped` | skip | `--service api,billing,reporting` | webhook + 2 packages | – | – | ✔ | – | ✖ |
| `FixtureCorpusKalshiShaped` | skip | `--service engine` | 3 workers + adapter package | – | – | ✔ | – | ✖ |
| `RegistrationTypesOnlyService` | skip | `--service api --frontend web` | protos + registry edits | – | – | – | – | ✖ |

`*` = not in the quoted timing extract; treat as "expensive" (all are
scaffold-bearing, and every npm-bearing one is 150s+).

**The row that matters most:** only nine tests are genuinely read-only over an
unmodified scaffold. The "read-only over a plain `--service api` tree" family
the plan hypothesised is real but **small** — it is `ScaffoldConfigNaming`,
`ScaffoldBasicProject`, `ScaffoldKCLRendersDevManifest`,
`ComponentsKCLDeclaredFromSources`, `ScaffoldFullSpecProject`,
`ScaffoldMultiServiceProject`. `ScaffoldIsReviveExportedClean`,
`TestingPackageNotLinkedIntoBinary` and `ScaffoldNoConflictingProtos` all
mutate, so the plan's candidate list (a) needs correcting on three of its five
members.

---

## 3. Proposed families

### Family A — the npm-installed frontend tree (**biggest prize**)

**Members:** `ScaffoldFrontendBuilds`, `ScaffoldFrontendListResource`,
`ScaffoldFrontendRuntime`, `GeneratedHooksExposeTheTypedErrorContract`,
`ScaffoldFrontendEnumEntityBuilds`, `FreshScaffoldFrontendLintClean`.

**Why they group:** all six are `project new --frontend {web,dashboard}` plus
one service, and each pays its own `npm install` (the dominant term — the two
measured members are 389s and 396s). Their *assertions* differ (build green,
vitest green, tsc typed-error contract, enum form projection, runtime link
shape, style lint) but their *tree* need not.

**Mechanism:** a `sync.Once`-guarded `sharedFrontendTree(t)` helper, exactly
like `buildforgeBinary` and `sharedTestPostgres`. It scaffolds ONE
`--frontend dashboard` project whose single service carries an entity proto
that is the **union** of what the members need (CRUD entity + an enum field +
the scalar-vocabulary fields), runs `forge scaffold`, `forge generate`,
`go build ./...`, and one `npm install`. It returns the project dir. Members
read from it and run their own *non-mutating* npm scripts
(`npm run build`, `npm test`, `tsc --noEmit`, `npm run lint:styles`).

**Isolation risk and the answer:** `npm run build` writes `.next/` and
`FreshScaffoldFrontendLintClean` runs `lint:styles:fix`, which REWRITES
`globals.css`. Those are mutations, so:
- members that only *read* generated TS or run `tsc --noEmit` use the shared
  tree directly;
- members that run a build or a fixer get a **copy-on-write clone** of the
  shared tree (`cp -a`, or better, clone everything except `node_modules` and
  symlink/hardlink that one directory back). The clone is seconds; the
  `npm install` it avoids is minutes. `SelfCertCloneReproduces` already
  establishes tree-cloning as an idiom in this suite.

**Members that must NOT join:** `AddFrontendKindsProduceABuildableTree` (its
whole subject is three *different* `scaffold frontend --kind` adds each doing
its own install — sharing an install would delete the thing under test) and
`FixtureCorpusFrontendBasePath` (asserts a build must FAIL under an empty
`NEXT_PUBLIC_BASE_PATH`, and needs its own `--base-path /admin` add).

**Estimated saving:** six installs → one, plus five fewer
scaffold+generate+`go build` cycles. Conservatively **~1,200–1,500 test-seconds**,
and because npm is the least parallel-friendly step, more than that in wall
clock on a loaded runner.

### Family B — entity/column projection over one multi-entity tree

**Members (read-only after generate):**
`ScaffoldReadOnlyFieldOmittedFromCreate`,
`ScaffoldReadOnlySurvivesInlineOptions`,
`ScaffoldMarkedEntityDeclaresManagedFields`,
`OrderByWithoutDescending`,
`ScaffoldSchemaHardeningDefaults`,
`ScaffoldHandlersFilePlusMarkedEntityCompiles`,
`PbThroughCrudPlusCustomRpc`.

**Why they group:** each writes one entity proto into a one-service project and
then asserts read-only facts about the generated Go (which fields the create
request carries, which columns the ORM projects, what the order-by helper
emits, what the schema hardening produced). They do not conflict, because
**each one's proto declares a differently-named entity.** Nothing forces them
into separate *projects*; they were separate only because each was written
standalone.

**Mechanism:** one `sync.Once` `sharedEntityProjectionTree(t)` that scaffolds a
single project with one service whose proto file set contains **all seven
entities, each preserved verbatim from the test it came from** (rename the
service dir, not the messages). One `forge scaffold`, one `forge generate`, one
`go build ./...`. Each former test becomes a top-level test that calls the
helper and reads only *its* generated files.

**Non-negotiable:** every existing assertion moves across unchanged. The
implementing task must produce an explicit old-test → new-assertion map. If two
entities' hand-written migrations would collide, that member leaves the family
rather than having its migration weakened.

**Members that must NOT join:** `ScaffoldReadOnlyColumnTakesItsSchemaDefault`
and `CRUDFixtureSurvivesAddedConstraints` and
`CRUDFixtureSatisfiesCheckConstraints` write *additional* migrations after the
first generate and re-generate — that is a tree mutation. `SchemaDriftNotice`
deliberately mutates proto and migrations across three generates. All keep
their own tree.

**Estimated saving:** seven scaffold+generate+build cycles → one. Using the
measured members (100 + 92 + 145 + 140 ≈ 477s for four of them, and the other
three in the same 90–150s band), the family costs roughly **~800s today and
~180s consolidated: saving ~600 test-seconds.**

### Family C — read-only over an unmodified scaffold

**Members:** `ScaffoldConfigNaming` (93), `ScaffoldBasicProject` (118),
`ScaffoldKCLRendersDevManifest` (27), `ComponentsKCLDeclaredFromSources` (59),
`ScaffoldFullSpecProject`, `ScaffoldMultiServiceProject`, `ScaffoldServerStartup`.

These genuinely share input in the plan's original sense. But note two splits:
`ComponentsKCLDeclaredFromSources` and `ScaffoldMultiServiceProject` are
*multi-service*, `ScaffoldFullSpecProject`/`ScaffoldMultiServiceProject` carry a
frontend. So this is **two** shared trees, not one:
- **C1** — `sharedPlainAPITree`: `--service api`, no frontend. Serves
  `ScaffoldConfigNaming`, `ScaffoldBasicProject`, `ScaffoldServerStartup`
  (which only *builds and boots* — read-only w.r.t. the tree, and keeps its
  `freePortE2E`), plus `ScaffoldKCLRendersDevManifest` if its `--service item`
  is harmonised to `api` (check the KCL assertions do not name the service).
- **C2** — `sharedMultiServiceTree`: `--service a,b,c --frontend web`. Serves
  `ScaffoldMultiServiceProject`, `ScaffoldFullSpecProject`,
  `ComponentsKCLDeclaredFromSources`.

**Estimated saving:** C1 ≈ 240s saved, C2 ≈ 200s saved. **~440 test-seconds.**

### Family D — derive it, don't scaffold it (the best outcome where it applies)

`scaffold_scalar_vocabulary_e2e_test.go` is the model: three tests, 0.00s each,
that read forge's own `codegen.ProtoScalarKinds()` and `codegen.ScanRawProtoDir`
against the fixture consts, and fail **by name** when the generator's closed
table gains a kind the fixture does not cover.

Candidates to convert rather than consolidate:
- `ScaffoldConfigNaming` — the naming rule is a pure function of the project
  name; if the naming logic is reachable in-process from `internal/codegen`,
  this test does not need a 93-second scaffold at all.
- `ComponentsKCLDeclaredFromSources` — "the components KCL enumerates exactly
  the declared sources" is a set-equality between the project manifest and the
  rendered KCL; derivable if the renderer is callable in-process. The `kcl`
  *render* itself (`ScaffoldKCLRendersDevManifest`) is not derivable and stays.
- `TestingPackageNotLinkedIntoBinary` — currently shells `go list -deps`; keep
  the subprocess, but it can run against a Family C1 tree rather than its own.

The implementing task should spend one pass asking "is this a property of
forge's own tables?" before building any shared fixture. A derived test is
faster, more precise, and cannot rot the way a shared tree can.

---

## 4. Tests that must stay standalone, and why

**Failure paths — a reverted or failed generate must never touch a shared tree:**
`JSONBUnmappableFieldFailsGenerate`, `GenerateRollbackOnValidateFailure`,
`ScaffoldReadOnlyMarkerWithNoFieldFailsLoudly`,
`AllBinaryConfigNoPrimaryBinding_RejectsAtGenerate`,
`CRUDFixtureGuardFailsLoudlyOnUninvertibleCheck`,
`FailedGenerateRevertConsistencyAndPreservation`. Their subject *is* the
half-written state, and they are cheap anyway (20–88s).

**Multi-generate / migration-mutating:** `SchemaDriftNotice`,
`PbThroughStubToCrudTransition`, `ScaffoldReadOnlyColumnTakesItsSchemaDefault`,
`CRUDFixtureSurvivesAddedConstraints`, `CRUDFixtureSatisfiesCheckConstraints`,
`ComponentObserveMarkerAndEnforceLint`, `ObservedDecoratorGeneratedAndWired`,
`ScaffoldKCLVendorFlow`.

**Self-cert:** all three `SelfCert*` tests exist to observe the checksum
manifest evolve across edits; they mutate by definition.

**Add-verb / growth tests:** `ScaffoldAddService`, `AddServiceThenEntityGenerates`,
`AddVerbsProduceABuildableTree`, `ScaffoldIsReviveExportedClean`,
`ZeroServiceScaffoldCompiles`, `FixtureCorpusZeroService`. The *act of adding* is
the subject.

**Runtime/boot with injected tests:** `GeneratedStoreSeam`,
`ScaffoldSecretFieldPreservedOnFullReplace`, `JSONBEntityConversion`,
`EnumEntityConversionAndFilter`, `OptionalScalarEntityBirthAndConversion`,
`CmdAsCodeSubcommands`, `FixtureCorpusCRUDLifecycle`. Each writes a Go test into
the tree and runs it against postgres. In principle several could share a tree
(their injected tests are in different packages), but the risk of cross-test
migration interference is high and the payoff is smaller than A/B/C. **Defer.**

---

## 5. The CI-sharding constraint, and the shape chosen

`.github/workflows/e2e-suite.yml` enumerates with
`go test -tags e2e -list '.*'`, shards by `NR % job-total` across 4 jobs, and a
ledger job asserts the shard union equals the full list. There is also a skip
ratchet (`FORGE_E2E_MAX_SKIPS: 16`, may only go down).

Two consequences:

1. **Do not collapse a family into one top-level test.** Twelve tests becoming
   one makes an indivisible 300s+ unit that lands entirely in one shard, and
   pushes the top-level count toward the workflow's `>= 50` floor.
2. **Therefore: `sync.Once` shared fixtures, not `t.Run` trees.** Every member
   stays its own top-level `TestE2E*` function — the shard partition and the
   ledger are unchanged, the count stays put — and the expensive tree is built
   once per *package run* by whichever member reaches it first. This is exactly
   the idiom `buildforgeBinary` and `sharedTestPostgres` already use, so it
   composes with `t.Parallel()` without new machinery.

**The trade-off I am choosing, stated plainly:** `sync.Once` sharing means that
within a single shard the members serialise on the first one to arrive, and a
failure in fixture construction fails several tests at once rather than one.
That is worse for *diagnosis* than fully independent trees. It is accepted
because (a) the fixture failure message can name the fixture explicitly, and
(b) the alternative — one giant parent test — is worse on *both* diagnosis and
shard balance. Across shards the fixture is built up to 4 times (once per shard
process), which is fine: 4 installs beats 6, and the shards are the parallelism.

One caveat the implementer must handle: `sync.Once` fixtures and `t.TempDir()`
do not mix — the tree must live in a package-level temp dir cleaned by
`TestMain`, not one owned by whichever test happened to trigger construction.
`buildforgeBinary` already faces this; follow whatever it does.

Other invariants to preserve: keep `t.Parallel()` everywhere, keep
`freePortE2E` for `ScaffoldServerStartup` and `GeneratedStoreSeam`, and keep the
loud-precondition discipline — a member needing npm must still hard-fail under
`FORGE_E2E_REQUIRE_TOOLS=1` rather than skip.

---

## 6. Ranked recommendation

| Rank | Family | Mechanism | Est. saving (test-seconds) | Risk |
|---|---|---|---|---|
| 1 | **A — shared npm frontend tree** | `sync.Once` + clone-for-mutators | **~1,200–1,500** | medium (clone semantics, npm hoisting) |
| 2 | **B — multi-entity projection tree** | `sync.Once`, one project, N entities | **~600** | low (read-only members) |
| 3 | **C — read-only plain/multi-service trees** | two `sync.Once` trees | **~440** | low |
| 4 | D — derive instead of scaffold | in-process against `codegen` | 90–150 per convert | low, but only a few candidates |

**Do Family A first**, as the plan's "highest-value consolidation family" task
intends. It carries the most saving and it is the one whose risks (tree cloning
with a shared `node_modules`) most need proving before the pattern is rolled
out. If cloning turns out not to work cleanly, fall back to Family B as the
proving ground — it is lower-risk and its members are purely read-only — and
report that A needs a different mechanism.

Whatever is implemented, measure before/after on the same machine at the same
`-parallel` setting, and if the realised saving is materially below these
estimates, stop and report rather than rolling out further.
