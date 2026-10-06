package templates

// GolangciLintVersion is the one golangci-lint release forge installs and
// ships: forge's own CI and scripts/bootstrap.sh, and — through the CI
// template and the scaffolded scripts/bootstrap.sh — every project forge
// creates. TestGolangciLintVersionIsPinnedEverywhere
// (internal/generator/golangci_pin_test.go) fails when any of those sites
// names a different version, `latest`, the v1 module path, or an action major
// that cannot run v2.
//
// It is a pin, not `latest`, because a linter release adds checks: v2.14.0
// turned seven months-old lines red with no commit in between. Bumping it is
// a deliberate one-line change here plus the four files the test names, made
// in a PR that also fixes whatever the new release reports.
const GolangciLintVersion = "v2.14.0"

// PrettierVersion is the one prettier release that formats a scaffolded
// frontend's sources, and therefore the one every formatter of those sources
// must run: the frontends' package.json (`npm run format`, and forge's
// format-at-birth of a scaffolded page, which uses the installed copy), the
// scaffolded .pre-commit-config.yaml hook, forge's OWN pre-commit hook (which
// formats pkg/components, the library copied verbatim into projects) and
// every test that runs prettier to prove a source is clean.
// TestPrettierVersionIsPinnedEverywhere
// (internal/generator/prettier_pin_test.go) fails when any of those names
// another version or a range.
//
// Exact, never a range, for the reason GolangciLintVersion is: prettier
// minors reformat code. Two pins disagreeing on the same file is a
// formatting round-trip neither side is wrong about — and was forge main's
// red pre-commit: the hook ran 3.1.0 over library files #502 had formatted
// with 3.5.3, the version projects ran. A `^3.5.0` range drifts the same way
// on its own, one `npm install` at a time.
const PrettierVersion = "3.5.3"
