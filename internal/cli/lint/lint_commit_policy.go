package lint

import (
	"fmt"

	"github.com/reliant-labs/forge/internal/commitpolicy"
)

// The commit-policy step: generated code must be committed, and
// machine-local materializations (.forge-kcl/, a frontend's dev
// public/config.js) must not be. See internal/commitpolicy for why this is a
// gating check rather than a template default — a project keeps the
// .gitignore it was scaffolded with, and `forge ci verify-generated` cannot
// see an ignored file.

// runCommitPolicyLint is the text-mode arm.
func runCommitPolicyLint(root string) error {
	vs, err := commitpolicy.Check(root)
	if err != nil {
		return err
	}
	if len(vs) == 0 {
		return nil
	}
	return fmt.Errorf("%d path(s) break forge's commit policy "+
		"(generated code is committed; machine-local state is not):\n%s", len(vs), commitpolicy.Format(vs))
}

// collectCommitPolicyJSON is the --json arm. Every violation gates.
func collectCommitPolicyJSON(root string) ([]lintJSONFinding, bool, error) {
	vs, err := commitpolicy.Check(root)
	if err != nil {
		return nil, false, err
	}
	out := make([]lintJSONFinding, 0, len(vs))
	for _, v := range vs {
		out = append(out, lintJSONFinding{
			Rule:     v.Rule,
			Severity: "error",
			File:     v.Path,
			Message:  v.Why,
			FixHint:  v.Fix,
		})
	}
	return out, len(out) > 0, nil
}
