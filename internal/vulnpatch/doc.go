// Package vulnpatch is the deterministic half of `forge vuln patch`: it bumps
// a vulnerable dependency to the lowest version that fixes it, with the
// ecosystem's own tool, verifies the result, and says per finding what
// happened.
//
// # The four steps
//
//  1. Resolve each finding to the manifests in the tree it comes from
//     ([Plan]). A Go module goes to every go.mod (go.work aware) that
//     requires it; an npm package to every package-lock.json,
//     pnpm-lock.yaml or yarn.lock that pins it at the version the finding
//     names, and only one package manager's; a PyPI package likewise to
//     every requirements file, poetry.lock or uv.lock that pins it at that
//     version, and only one tool's. A finding the tree cannot be tied to
//     (no exact copy, manifests of several package managers, a package.json
//     or requirements file forge could not write back faithfully) is
//     skipped, with the reason, for the agent.
//  2. Choose the target ([ChooseTarget]): the lowest fixed version at or
//     above the installed one, in the installed major unless AllowMajor. A
//     major-only fix is skipped as "needs a major bump".
//  3. Apply it ([Apply]) with the ecosystem's own tool, so lockfiles stay
//     consistent: `go get mod@vX && go mod tidy`; `npm install pkg@X` or an
//     npm `overrides` entry; `pnpm update` or `pnpm.overrides`; `yarn up`
//     (`yarn upgrade` on yarn 1) or `resolutions`; a requirements pin
//     rewritten, or its pip-compile (or `uv pip compile`) command rerun with
//     --upgrade-package; `poetry update` or `poetry add pkg@^X`; `uv lock
//     --upgrade-package pkg==X`. Python versions are PEP 440, not semver.
//  4. Verify ([Verify]): run the project's gate, then rescan each changed
//     manifest's directory; a targeted finding that the rescan of one of its
//     own directories still reports, on the same package, is still_present.
//
// Every command runs through a [vulnscan.Runner], so the tests drive the
// whole flow against fixture trees with a fake go, npm, pnpm, yarn,
// pip-compile, poetry, uv, git and gh. The result is a [Document] (schema
// forge.dev/vuln-patch/v1), which the `vuln-patch` Reliant workflow reads to
// decide whether an agent takes over.
//
// OS packages and the Go standard library have no patcher here yet (task
// F9c of the SBOM design): their findings are skipped with "no patcher
// yet", which is exactly what an agent needs to pick them up.
//
//forge:lint-disable-next-line forge-exclude-contract-outbound-io: every outbound effect (go, npm, pnpm, yarn, pip-compile, poetry, uv, git, gh, the verify command) goes through the injectable vulnscan.Runner field the tests fake; a contract.go Service would be one implementation with no substitution point
//forge:exclude-contract: a CLI-driven dependency patcher, not an injected application component
package vulnpatch
