package cli

// `--declare=best-effort` — the escape hatch for a control plane OLDER than
// this forge.
//
// THE GAP THIS CLOSES. `forge env build` records the env's declaration (its
// shape, on EnsureEnvironment) and every bundle it writes (RecordBundle) on the
// env's control plane, and both documents are typed by forge/pkg/release. The
// control plane decodes them STRICTLY, on purpose: a field it does not know is
// refused rather than dropped, because a stored shape or bundle it only half
// understood would be read as authoritative by every later surface. So a forge
// whose release.Shape or bundle document grew a field cannot record anything on
// a control plane built against the older forge/pkg — it answers
// InvalidArgument ("spec.shape is not a release.Shape").
//
// For most projects that is the platform's problem to sequence. For a project
// that IS a control plane and deploys itself with forge, it is a deadlock: the
// server that would understand the new field ships in the very release whose
// cut it refuses. control-plane hit exactly this moving from forge 6f9811cb to
// f3fd8c02 (ShapeObject.tree, Shape.delivery_policy).
//
// WHAT IT DOES. Under --declare=best-effort a control plane that READ a record
// and refused its CONTENT (InvalidArgument) no longer fails the command. forge
// prints a loud warning naming the record, the server's answer and every field
// the server named, and saying which command records it once the control plane
// is upgraded. Nothing else changes: the release, the promotion and the apply
// are recorded and run exactly as without the flag.
//
// WHAT IT DOES NOT DO.
//   - It is never the default, and nothing infers it. Strict is the only safe
//     default for a record every Live surface trusts.
//   - It downgrades only a content refusal. A kind change (FailedPrecondition),
//     a rejected credential (PermissionDenied, Unauthenticated) and every other
//     answer are facts about the env or the caller and still fail.
//   - It never downgrades a refused bundle for an env its control plane's hub
//     converges: there the recorded bundle IS what ships, so "not recorded"
//     means "nothing to apply" and a green command would be a lie.
//   - Undeliverable records (no credential, control plane unreachable) already
//     warn and carry on; this flag does not touch them.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"

	"github.com/spf13/pflag"

	"github.com/reliant-labs/forge/internal/cloud"
)

// declarePolicy is what a command does when the control plane refuses a
// declaration or a bundle record.
type declarePolicy string

const (
	// declareStrict fails the command on a refusal. The default.
	declareStrict declarePolicy = "strict"
	// declareBestEffort warns on a content refusal and carries on.
	declareBestEffort declarePolicy = "best-effort"
)

const declareFlagUsage = "What to do when the env's control plane REFUSES a record this command sends " +
	"(the env declaration, a bundle): `strict` (default) fails; `best-effort` warns, naming each field the " +
	"server refused, and carries on. For a control plane older than this forge, whose strict decoder refuses " +
	"fields it does not know yet — the one hop that upgrades a self-deploying control plane. Only a content " +
	"refusal (InvalidArgument) is downgraded, never for a hub-converged env's bundle; re-run without the flag " +
	"once the control plane is upgraded to record what was skipped."

// registerDeclareFlag declares --declare on a command's flag set.
func registerDeclareFlag(flags *pflag.FlagSet, into *string) {
	flags.StringVar(into, "declare", string(declareStrict), declareFlagUsage)
}

// parseDeclarePolicy validates a --declare value.
func parseDeclarePolicy(s string) (declarePolicy, error) {
	switch p := declarePolicy(s); p {
	case "", declareStrict:
		return declareStrict, nil
	case declareBestEffort:
		return p, nil
	default:
		return "", fmt.Errorf("--declare %q: want %q or %q", s, declareStrict, declareBestEffort)
	}
}

type declarePolicyCtxKey struct{}

// withDeclarePolicy marks ctx with the command's --declare policy. A context
// value, like --offline-ledger, because the records are written deep inside a
// build or deploy and no call site between the flag and them takes options.
func withDeclarePolicy(ctx context.Context, p declarePolicy) context.Context {
	if p == "" || p == declareStrict {
		return ctx
	}
	return context.WithValue(ctx, declarePolicyCtxKey{}, p)
}

func declareBestEffortFrom(ctx context.Context) bool {
	p, _ := ctx.Value(declarePolicyCtxKey{}).(declarePolicy)
	return p == declareBestEffort
}

// isContentRefusal reports whether err is the control plane having read a
// record and refused what it said: InvalidArgument, and nothing else.
func isContentRefusal(err error) bool {
	var cerr *cloud.Error
	return errors.As(err, &cerr) && cerr.HasCode(cloud.CodeInvalidArgument)
}

// declarationRefusalDowngraded reports whether a refused env declaration is
// a warning rather than a failure under ctx's policy.
func declarationRefusalDowngraded(ctx context.Context, err error) bool {
	return err != nil && declareBestEffortFrom(ctx) && isContentRefusal(err)
}

// bundleRefusalDowngraded is the same decision for a bundle record. A
// hub-converged env is excluded: its control plane applies the bundle it has
// recorded and nothing else.
func bundleRefusalDowngraded(ctx context.Context, err error, hubConverged bool) bool {
	return !hubConverged && declarationRefusalDowngraded(ctx, err)
}

var unknownFieldRe = regexp.MustCompile(`unknown field "([^"]+)"`)

// refusedFields lists every field the server's answer names as unknown, in
// order, once each. Empty when the server named none.
func refusedFields(err error) []string {
	if err == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, m := range unknownFieldRe.FindAllStringSubmatch(err.Error(), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// warnRefusedRecord is the loud half of best-effort: what was refused, what
// the server said, which fields, and the command that records it later.
func warnRefusedRecord(w io.Writer, prefix, what, retry string, err error) {
	fmt.Fprintf(w, "%s WARNING (--declare=best-effort): the control plane REFUSED %s, and it was NOT recorded.\n", prefix, what)
	fmt.Fprintf(w, "%s   server said: %v\n", prefix, err)
	if fields := refusedFields(err); len(fields) > 0 {
		fmt.Fprintf(w, "%s   refused field(s): %q\n", prefix, fields)
	} else {
		fmt.Fprintf(w, "%s   refused field(s): the server named none. A content refusal of a record this forge produced\n"+
			"%s     usually means the control plane runs an older forge/pkg than this forge and refuses fields it does not know.\n", prefix, prefix)
	}
	fmt.Fprintf(w, "%s   It is retried by %s run WITHOUT --declare=best-effort; run that once the control plane is upgraded.\n", prefix, retry)
}
