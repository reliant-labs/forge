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
