package doctor

// payload.go — Connect payload-cap check.
//
// connect-go treats a zero ReadMaxBytes/SendMaxBytes as "no limit"
// ("Setting WithReadMaxBytes to zero allows any message size"), so the
// difference between a 4 MiB cap and an unbounded one is a single
// unassigned struct field. That is a whole class of bug the compiler
// cannot see and no test that only asserts happy-path behaviour will
// catch: the server keeps working, it just also buffers whatever an
// anonymous caller sends it.
//
// The check reads the composition root the project actually compiles —
// cmd/<binary>/cmd/*.go — and asserts that whatever expression feeds
// connect.WithReadMaxBytes / WithSendMaxBytes is provably non-zero at
// that point in the file.
//
// "Provably non-zero" has three shapes, and all three must be accepted or
// the check cries wolf on correct code — which is worse than useless on a
// security gate, because the next FAIL gets ignored too:
//
//   - the field is set in the composite literal,
//   - the field is set by a later selector assignment,
//   - the config value was NORMALIZED in this file before the read. That
//     last one is how forge fixes the ordering hazard at the source:
//     projectServerkitConfig calls skCfg.Normalize() before returning, so
//     the caps are filled in BEFORE anything reads them rather than at
//     serverkit.Run entry, which is too late. A project whose config proto
//     predates the read_max_bytes / send_max_bytes fields has no literal to
//     match on and is still protected entirely by that call.

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// payloadCapOptions are the connect handler options whose argument must
// be non-zero for a cap to exist.
var payloadCapOptions = map[string]string{
	"WithReadMaxBytes": "inbound request",
	"WithSendMaxBytes": "outbound response",
}

// capUse is one observed call site.
type capUse struct {
	file   string // path relative to the project dir
	line   int
	option string // WithReadMaxBytes / WithSendMaxBytes
	arg    string // rendered argument expression
	// field is the selector's field name when arg is `x.Field`, else "".
	field string
	// recv is the selector's receiver name when arg is `x.Field`, else "".
	recv string
}

// CheckPayloadLimits verifies the generated server caps Connect payload
// sizes.
//
// A missing cap is remotely exploitable without credentials: the server
// buffers the entire request body before any validation runs, so one
// request sized at the pod's memory limit is an OOMKill, and the
// generated deploy manifests ship a memory limit far below what a
// single large request costs.
//
// The check fails on three shapes:
//   - the option is passed a literal 0
//   - the option is passed `cfg.Field` where neither an assignment to that
//     field nor a `cfg.Normalize()` call appears in the same file (the
//     value reaching connect is the zero value, whatever a defaults helper
//     does later downstream)
//   - the project mounts Connect handlers and passes no cap at all
func CheckPayloadLimits(_ context.Context, env *Environment) CheckResult {
	cmdDir := filepath.Join(env.ProjectDir, "cmd")
	if _, err := os.Stat(cmdDir); err != nil {
		return CheckResult{Status: StatusSkip, Message: "no cmd/ tree — nothing to inspect"}
	}

	var (
		uses     []capUse
		problems []string
		files    int
	)

	walkErr := filepath.WalkDir(cmdDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable subtree is not this check's failure
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files++
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return nil
		}
		rel, _ := filepath.Rel(env.ProjectDir, path)
		assigned := assignedFields(file)
		normalized := normalizedReceivers(file)

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if _, tracked := payloadCapOptions[sel.Sel.Name]; !tracked {
				return true
			}
			use := capUse{
				file:   rel,
				line:   fset.Position(call.Pos()).Line,
				option: sel.Sel.Name,
				arg:    exprString(call.Args[0]),
			}
			if argSel, isSel := call.Args[0].(*ast.SelectorExpr); isSel {
				use.field = argSel.Sel.Name
				if recv, isIdent := argSel.X.(*ast.Ident); isIdent {
					use.recv = recv.Name
				}
			}
			uses = append(uses, use)

			switch {
			case use.arg == "0":
				problems = append(problems, fmt.Sprintf(
					"%s:%d: %s(0) — connect-go reads a zero cap as UNLIMITED %s payloads",
					use.file, use.line, use.option, payloadCapOptions[use.option]))
			case use.field != "" && !assigned[use.field] && !normalized[use.recv]:
				problems = append(problems, fmt.Sprintf(
					"%s:%d: %s(%s) but %s is never assigned in this file and %s is never normalized — "+
						"it reaches connect as 0, which means UNLIMITED %s payloads (a defaults helper "+
						"that runs later, e.g. inside serverkit.Run, cannot retroactively change the "+
						"value already captured here; call %s.Normalize() before this read)",
					use.file, use.line, use.option, use.arg, use.field, use.recv,
					payloadCapOptions[use.option], use.recv))
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		return CheckResult{Status: StatusWarn, Message: "could not walk cmd/", Evidence: walkErr.Error()}
	}
	if files == 0 {
		return CheckResult{Status: StatusSkip, Message: "no Go files under cmd/"}
	}

	if len(uses) == 0 {
		// No caps at all is only a finding when the project serves
		// Connect handlers; a CLI-shaped cmd tree legitimately has none.
		if !mountsConnectHandlers(env.ProjectDir) {
			return CheckResult{Status: StatusSkip, Message: "no Connect handlers mounted — payload caps not applicable"}
		}
		return CheckResult{
			Status: StatusFail,
			Message: "Connect handlers are mounted with no WithReadMaxBytes/WithSendMaxBytes — " +
				"request and response payloads are unbounded",
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return CheckResult{
			Status:   StatusFail,
			Message:  fmt.Sprintf("%d Connect payload cap(s) resolve to 0 (unlimited)", len(problems)),
			Evidence: strings.Join(problems, "\n"),
		}
	}

	set := make([]string, 0, len(uses))
	for _, u := range uses {
		set = append(set, fmt.Sprintf("%s:%d %s(%s)", u.file, u.line, u.option, u.arg))
	}
	sort.Strings(set)
	return CheckResult{
		Status:   StatusPass,
		Message:  fmt.Sprintf("%d Connect payload cap(s) set to a non-zero value", len(uses)),
		Evidence: strings.Join(set, "\n"),
	}
}

// assignedFields collects every struct-field name the file assigns —
// both as a composite-literal key (`Config{ReadMaxBytes: n}`) and as a
// selector assignment (`cfg.ReadMaxBytes = n`).
func assignedFields(file *ast.File) map[string]bool {
	assigned := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.KeyValueExpr:
			if key, ok := node.Key.(*ast.Ident); ok {
				assigned[key.Name] = true
			}
		case *ast.AssignStmt:
			for _, lhs := range node.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok {
					assigned[sel.Sel.Name] = true
				}
			}
		}
		return true
	})
	return assigned
}

// normalizedReceivers collects every identifier the file calls
// `.Normalize()` on. A config normalized in this file has had its unset
// caps filled in BEFORE any read here, which is exactly the guarantee this
// check is looking for — and the only guarantee available to a project
// whose config proto does not declare the cap fields.
func normalizedReceivers(file *ast.File) map[string]bool {
	normalized := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Normalize" {
			return true
		}
		if recv, isIdent := sel.X.(*ast.Ident); isIdent {
			normalized[recv.Name] = true
		}
		return true
	})
	return normalized
}

// mountsConnectHandlers reports whether the project has generated
// Connect handler wiring — the signal that payload caps are relevant.
func mountsConnectHandlers(projectDir string) bool {
	found := false
	handlersDir := filepath.Join(projectDir, "internal", "handlers")
	_ = filepath.WalkDir(handlersDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || found {
			return nil //nolint:nilerr // absence is the answer, not an error
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, rerr := os.ReadFile(path) //nolint:gosec // path comes from the walk of a project dir
		if rerr == nil && strings.Contains(string(b), "connect.HandlerOption") {
			found = true
		}
		return nil
	})
	return found
}

// exprString renders the small subset of expressions these options are
// called with, so the evidence quotes the source rather than an AST
// dump.
func exprString(e ast.Expr) string {
	switch node := e.(type) {
	case *ast.Ident:
		return node.Name
	case *ast.BasicLit:
		return node.Value
	case *ast.SelectorExpr:
		return exprString(node.X) + "." + node.Sel.Name
	case *ast.CallExpr:
		return exprString(node.Fun) + "(…)"
	case *ast.BinaryExpr:
		return exprString(node.X) + " " + node.Op.String() + " " + exprString(node.Y)
	case *ast.ParenExpr:
		return "(" + exprString(node.X) + ")"
	default:
		return strconv.Quote(fmt.Sprintf("%T", e))
	}
}
