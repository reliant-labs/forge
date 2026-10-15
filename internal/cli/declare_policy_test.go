package cli

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cloud"
)

func TestParseDeclarePolicy(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]declarePolicy{"": declareStrict, "strict": declareStrict, "best-effort": declareBestEffort} {
		got, err := parseDeclarePolicy(in)
		if err != nil || got != want {
			t.Errorf("parseDeclarePolicy(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"besteffort", "lenient", "STRICT"} {
		if _, err := parseDeclarePolicy(bad); err == nil {
			t.Errorf("parseDeclarePolicy(%q) accepted an unknown policy", bad)
		}
	}
}

// The flag exists on both commands that write records, and defaults to strict:
// best-effort is never the default.
func TestDeclareFlagDefaultsToStrict(t *testing.T) {
	t.Parallel()
	for name, cmd := range map[string]*cobra.Command{"env build": newEnvBuildCmd(), "env deploy": newDeployCmd()} {
		flag := cmd.Flags().Lookup("declare")
		if flag == nil {
			t.Errorf("`forge %s` has no --declare flag", name)
			continue
		}
		if flag.DefValue != string(declareStrict) {
			t.Errorf("`forge %s --declare` defaults to %q, want %q", name, flag.DefValue, declareStrict)
		}
	}
}

func TestRefusedFields(t *testing.T) {
	t.Parallel()
	err := &cloud.Error{HTTPStatus: 400, Code: cloud.CodeInvalidArgument,
		Message: `spec.shape is not a release.Shape: json: unknown field "tree"; also unknown field "delivery_policy" and unknown field "tree"`}
	if got, want := refusedFields(err), []string{"tree", "delivery_policy"}; !reflect.DeepEqual(got, want) {
		t.Errorf("refusedFields = %q, want %q", got, want)
	}
	if got := refusedFields(errors.New("spec.shape is not a release.Shape")); len(got) != 0 {
		t.Errorf("refusedFields named %q from an answer that names no field", got)
	}
}

// Only a CONTENT refusal is downgraded, only under best-effort, and never a
// hub-converged env's bundle.
func TestRefusalDowngradeDecision(t *testing.T) {
	t.Parallel()
	best := withDeclarePolicy(context.Background(), declareBestEffort)
	strict := withDeclarePolicy(context.Background(), declareStrict)
	invalid := &cloud.Error{HTTPStatus: 400, Code: cloud.CodeInvalidArgument, Message: "spec.shape is not a release.Shape"}
	kind := &cloud.Error{HTTPStatus: 400, Code: cloud.CodeFailedPrecondition, Message: "kind is immutable"}
	denied := &cloud.Error{HTTPStatus: 403, Code: cloud.CodePermissionDenied, Message: "no deploy:write"}

	cases := []struct {
		name string
		ctx  context.Context
		err  error
		hub  bool
		want bool
	}{
		{"strict refuses", strict, invalid, false, false},
		{"default context refuses", context.Background(), invalid, false, false},
		{"best-effort downgrades a content refusal", best, invalid, false, true},
		{"best-effort keeps a kind change fatal", best, kind, false, false},
		{"best-effort keeps a rejected credential fatal", best, denied, false, false},
		{"best-effort never downgrades a hub-converged bundle", best, invalid, true, false},
		{"no error is nothing to downgrade", best, nil, false, false},
	}
	for _, tc := range cases {
		if got := bundleRefusalDowngraded(tc.ctx, tc.err, tc.hub); got != tc.want {
			t.Errorf("%s: bundleRefusalDowngraded = %v, want %v", tc.name, got, tc.want)
		}
		if tc.hub {
			continue
		}
		if got := declarationRefusalDowngraded(tc.ctx, tc.err); got != tc.want {
			t.Errorf("%s: declarationRefusalDowngraded = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// End to end through `forge env build`'s declaration step: under best-effort a
// refused shape no longer fails the build; every other refusal still does.
func TestEnvBuildDeclaration_BestEffortDowngradesOnlyAContentRefusal(t *testing.T) {
	entities := &KCLEntities{ControlPlane: &ControlPlaneEntity{Type: "control_plane", Endpoint: "http://127.0.0.1:9"}}
	withDeclaredEntities(t, entities)

	cases := []struct {
		name      string
		policy    declarePolicy
		ensureErr error
		wantErr   bool
	}{
		{"strict: refused shape fails", declareStrict, &cloud.Error{HTTPStatus: 400, Code: cloud.CodeInvalidArgument, Message: "spec.shape is not a release.Shape"}, true},
		{"best-effort: refused shape warns", declareBestEffort, &cloud.Error{HTTPStatus: 400, Code: cloud.CodeInvalidArgument, Message: "spec.shape is not a release.Shape"}, false},
		{"best-effort: kind change still fails", declareBestEffort, &cloud.Error{HTTPStatus: 400, Code: cloud.CodeFailedPrecondition, Message: "kind is immutable"}, true},
		{"best-effort: credential rejected still fails", declareBestEffort, &cloud.Error{HTTPStatus: 403, Code: cloud.CodePermissionDenied, Message: "no deploy:write"}, true},
		{"best-effort: accepted is accepted", declareBestEffort, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := withDeclarationRecorder(t, declarationFixtureShape())
			fake.ensureErr = tc.ensureErr
			err := recordEnvBuildDeclaration(withDeclarePolicy(context.Background(), tc.policy), "prod")
			if (err != nil) != tc.wantErr {
				t.Fatalf("recordEnvBuildDeclaration err = %v, want error: %v", err, tc.wantErr)
			}
			if n := len(fake.callsTo(procEnsureEnv)); n != 1 {
				t.Errorf("EnsureEnvironment called %d time(s), want 1: best-effort still SENDS the declaration", n)
			}
		})
	}
}

// The warning is the whole contract of best-effort: it must say what was not
// recorded, every field the server named, and how to record it later.
func TestWarnRefusedRecord(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	err := &cloud.Error{HTTPStatus: 400, Code: cloud.CodeInvalidArgument,
		Message: `spec.shape is not a release.Shape: json: unknown field "tree"`}
	warnRefusedRecord(&buf, "[declare]", "env prod's declaration", "the next `forge env build prod`", err)
	out := buf.String()
	for _, want := range []string{"WARNING (--declare=best-effort)", "env prod's declaration", "NOT recorded",
		`refused field(s): ["tree"]`, "the next `forge env build prod` run WITHOUT --declare=best-effort"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning lacks %q:\n%s", want, out)
		}
	}

	buf.Reset()
	warnRefusedRecord(&buf, "[bundle]", "env prod's bundle sha256:abc", "the next `forge env deploy prod <version>`",
		&cloud.Error{HTTPStatus: 400, Code: cloud.CodeInvalidArgument, Message: "bundle document is not valid"})
	if !strings.Contains(buf.String(), "the server named none") {
		t.Errorf("a refusal naming no field must say so, not print an empty list:\n%s", buf.String())
	}
}
