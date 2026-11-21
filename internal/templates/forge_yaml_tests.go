package templates

// ForgeYAMLTestsBlock is the `tests:` block every new project's forge.yaml
// carries (docs/proposals/tests-coverage-flakes.md §1.8, §5.1), and the one
// the tests upgrade codemod (`forge fix tests-capture`) appends to an
// existing project's. It is text, not a marshalled config.TestsConfig, so the
// knobs a project has not set stay discoverable as comments beside the one it
// has. It begins with a blank line: it is appended after the rest of the file.
//
// `coverage: true` is the opt-in: a project without the block measures no
// coverage and builds no instrumented binaries, so a forge bump alone never
// instruments anything (config.TestsConfig). With it, the scaffolded
// Taskfile lanes write coverage profiles and the dev env and every preview
// build their Go workloads with -cover; persistent envs never do.
const ForgeYAMLTestsBlock = `
# Test runs: every ` + "`task test*`" + ` lane records its results and coverage under
# .forge/tests/ (` + "`forge test report`" + ` reads them; ` + "`forge skill load testing`" + `).
# Recording never decides a verdict; FORGE_TESTS=off turns it off for one run.
tests:
    # upload: ci                           # ci (default) | always | never
    # flakes:
    #     quarantine: suggest              # suggest (default) | pr: open the quarantine PR
    #     issues: false                    # file flake:suspected / flake:removed issues
    #
    # coverage false: the lanes record results only, and no env builds
    # instrumented (-cover) binaries. Or a mapping:
    #   coverage:
    #       packages: package              # package (default) | module: -coverpkg=./...
    #       exclude: ["internal/legacy/**"]  # left out of every percentage
    #       include_generated: false       # count gen/, *_gen.go, *.pb.go in them
    coverage: true
`
