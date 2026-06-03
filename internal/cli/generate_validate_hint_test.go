// Regression guard for the kalshi-trader migration round friction
// reports that surfaced the same root cause from four separate task
// lanes:
//
//   - add-cycle-run-entity: `forge generate` final validate step fails
//     with `undefined: orm.TypeDoublePrecision` across every gen/db/v1
//     /*.pb.orm.go file. The pinned forge/pkg in go.mod predates the
//     symbol the in-PATH protoc-gen-forge emits.
//   - add-list-algo-predictions-rpc: same wedge, blocked the project-
//     wide go build sanity check.
//   - forge-doctor-observe-check: same wedge, cascaded into 5 downstream
//     packages.
//   - add-config-parity-test: same wedge, forced the user to scope
//     builds to a narrow subdirectory.
//
// The friction was not just the failure itself — the fix-hint
// `runGoBuildValidate` emitted ("ensure all referenced types are
// imported and re-run 'forge generate'") was wrong. Re-running generate
// produces the same broken output; the actual fix is to bump the
// forge/pkg pin in both root go.mod and gen/go.mod. This test pins the
// hint-selection so the next round's agent sees the actionable
// remediation immediately.
package cli

import (
	"strings"
	"testing"
)

func TestGoBuildValidateFixHint(t *testing.T) {
	cases := []struct {
		name      string
		errOutput string
		// wantContains is a list of substrings that MUST appear in the
		// returned hint. Multiple substrings let us assert both the
		// classification ("forge/pkg pin is older") AND the actionable
		// command (`go get github.com/reliant-labs/forge/pkg@latest`).
		wantContains []string
	}{
		{
			name: "orm-TypeDoublePrecision-skew",
			errOutput: `gen/db/v1/model_performance.pb.orm.go:42:18: undefined: orm.TypeDoublePrecision
gen/db/v1/cycle_run.pb.orm.go:55:18: undefined: orm.TypeDoublePrecision`,
			wantContains: []string{
				"forge/pkg pin is older",
				"go get github.com/reliant-labs/forge/pkg@latest",
				"go mod tidy",
				"gen/",
			},
		},
		{
			name: "orm-TypeReal-skew",
			errOutput: `gen/db/v1/model_performance.pb.orm.go:48:18: undefined: orm.TypeReal`,
			wantContains: []string{
				"forge/pkg pin is older",
				"go get github.com/reliant-labs/forge/pkg@latest",
			},
		},
		{
			name:      "pkg-config-undefined",
			errOutput: `bootstrap.go:42:18: undefined: pkg/config.Foo`,
			wantContains: []string{
				"proto/config/",
				"annotated config fields",
			},
		},
		{
			name:      "authorizer-gen-missing",
			errOutput: `bootstrap.go:42:18: undefined: GeneratedAuthorizer`,
			wantContains: []string{
				"authorizer_gen.go",
			},
		},
		{
			name:      "authorizer-gen-file-not-found",
			errOutput: `internal/auth/authorizer_gen: no such file`,
			wantContains: []string{
				"authorizer_gen.go",
			},
		},
		{
			name:      "empty-stderr",
			errOutput: "",
			wantContains: []string{
				"ensure all referenced types are imported",
			},
		},
		{
			name:      "unknown-error-default-fallthrough",
			errOutput: `internal/foo/bar.go:42:18: undefined: SomeUnrelatedSymbol`,
			wantContains: []string{
				"ensure all referenced types are imported",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := goBuildValidateFixHint(tc.errOutput)
			if got == "" {
				t.Fatalf("goBuildValidateFixHint returned empty hint for %q", tc.errOutput)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("goBuildValidateFixHint(%q) = %q, want substring %q",
						tc.errOutput, got, want)
				}
			}
		})
	}
}

// TestGoBuildValidateFixHint_OrmSkewBeatsOtherClassifiers pins the
// pattern-match ORDER: when multiple classifiers could match the same
// stderr blob, the orm.Type* skew classifier must win because the
// other hints would send the user down dead-end remediation paths
// ("re-run forge generate" produces the same broken output, since the
// generator is what emitted the bad reference).
func TestGoBuildValidateFixHint_OrmSkewBeatsOtherClassifiers(t *testing.T) {
	// Stderr that contains BOTH the orm.Type marker AND a `pkg/config`
	// reference. The orm marker wins.
	mixed := `gen/db/v1/foo.pb.orm.go:42:18: undefined: orm.TypeDoublePrecision
internal/bootstrap.go:90:5: imports pkg/config: foo.go:1:1: package config`
	got := goBuildValidateFixHint(mixed)
	if !strings.Contains(got, "forge/pkg pin is older") {
		t.Errorf("orm.Type* classifier did not win against pkg/config classifier:\n  got: %q", got)
	}
	if strings.Contains(got, "proto/config/") {
		t.Errorf("orm.Type* classifier should suppress the pkg/config hint; got mixed message: %q", got)
	}
}
