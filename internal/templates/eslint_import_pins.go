package templates

// ESLintImportPackage is one package the Next.js scaffold's eslint.config.mjs
// imports for its import rules (import-x/no-cycle, import-x/order,
// import-x/no-default-export), at the exact version forge pins.
type ESLintImportPackage struct {
	Name    string
	Version string
}

// ESLintImportPackages are the packages the Next.js eslint.config.mjs
// imports for its import rules, in the order they are declared. It is the one
// place their versions live: the scaffold's package.json renders them
// (`eslintImportPin`), and `forge generate` declares them in an existing
// frontend whose eslint config imports them
// (generator.EnsureESLintImportDependencies), so a project whose config
// `forge project upgrade` rewrote installs what the new config imports.
//
// Exact pins, not ranges: no-cycle's cost and what it reports depend on the
// plugin and resolver together (forge measured 35s with eslint-plugin-import
// against 10s with these two on a fresh scaffold), so a bump is a decision,
// made here, rather than a drift.
var ESLintImportPackages = []ESLintImportPackage{
	{Name: "eslint-import-resolver-typescript", Version: "4.4.5"},
	{Name: "eslint-plugin-import-x", Version: "4.18.1"},
}

// eslintImportPin returns the version forge pins for one of
// ESLintImportPackages, and false for any other name.
func eslintImportPin(name string) (string, bool) {
	for _, p := range ESLintImportPackages {
		if p.Name == name {
			return p.Version, true
		}
	}
	return "", false
}
